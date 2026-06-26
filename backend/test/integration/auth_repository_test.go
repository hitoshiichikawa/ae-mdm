package integration_test

import (
	"context"
	stdErrors "errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hitoshiichikawa/ae-mdm/internal/auth"
	internalerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/oidc"
)

// 本ファイルは Issue #33 task 4.1 の Repository（sessions / admin_users / state_nonces）
// に対する integration test。DATABASE_URL 未設定環境では各テストが自身で t.Skip する。
//
// シナリオ（tasks.md task 4.1 L379〜L395）:
//   (a) ResolveAdminUser 未 provisioning 時 403 admin_user_not_provisioned
//   (b) ResolveAdminUser provisioned 既存行で Identity が tenant_id / Roles 込みで返る
//   (c) ResolveAdminUser で IdP 側 email 変更が反映される
//   (d) Create → Get で hash 一致時に Session+Identity が返る
//   (e) Get で hash 不一致時に 0 行 → session_tamper
//   (f) Touch 後の last_seen_at 更新と expires_at 不変
//   (g) Revoke 後の revoked_at セット
//   (h) Revoke 冪等性
//   (i) ConsumeStateNonce 初回成功（INSERT 1 行 + nil 返却）
//   (j) ConsumeStateNonce 同一 nonce 再呼出で state_replay
//   (k) ConsumeStateNonce の異なる nonce での並行 INSERT 干渉なし

const repoTestIssuer = "https://idp.test.example.com/realms/ae-mdm-test"

// authRepoFixture は Repository integration test の共通 setup を集約する。
type authRepoFixture struct {
	pool *pgxpool.Pool
	ctx  context.Context
}

// setupAuthRepoFixture は migrate-up → truncate → tenant 1 件と admin_user 1 件を seed する。
func setupAuthRepoFixture(t *testing.T) (*authRepoFixture, func()) {
	t.Helper()
	urls := requireDBURLs(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	applyMigrationsUp(t, urls.migrate)
	truncateAll(t, ctx, urls)
	pool := newAppPool(t, ctx, urls)
	cleanup := func() {
		pool.Close()
		cancel()
	}
	return &authRepoFixture{pool: pool, ctx: ctx}, cleanup
}

// seedTenant は SuperAdmin 文脈で tenant 1 件を作って tenant_id を返す。
func seedTenant(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) uuid.UUID {
	t.Helper()
	tenantID := uuid.New()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", uuid.Nil.String()); err != nil {
		t.Fatalf("set_config tenant_id: %v", err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.is_superadmin', 'true', true)"); err != nil {
		t.Fatalf("set_config is_superadmin: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO tenants (id, name, status) VALUES ($1, $2, 'bound')`,
		tenantID, name); err != nil {
		t.Fatalf("INSERT tenants: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return tenantID
}

// seedAdminUser は admin_users と admin_role_assignments を SuperAdmin 文脈で投入する。
//
// roles に "SuperAdmin" が含まれる場合 tenant_id は NULL で投入する（A2 既存規約に従う）。
func seedAdminUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, issuer, sub, email string, tenantID uuid.UUID, roles []string) uuid.UUID {
	t.Helper()
	adminUserID := uuid.New()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", uuid.Nil.String()); err != nil {
		t.Fatalf("set_config tenant_id: %v", err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.is_superadmin', 'true', true)"); err != nil {
		t.Fatalf("set_config is_superadmin: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO admin_users (id, oidc_subject, oidc_issuer, email, tenant_id) VALUES ($1, $2, $3, $4, $5)`,
		adminUserID, sub, issuer, email, tenantID); err != nil {
		t.Fatalf("INSERT admin_users: %v", err)
	}
	for _, role := range roles {
		var roleTenantID interface{}
		if role == "SuperAdmin" {
			roleTenantID = nil
		} else {
			roleTenantID = tenantID
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO admin_role_assignments (admin_user_id, role, tenant_id) VALUES ($1, $2, $3)`,
			adminUserID, role, roleTenantID); err != nil {
			t.Fatalf("INSERT admin_role_assignments: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return adminUserID
}

// fetchSession は token_hash で sessions の 1 行を SuperAdmin 文脈で取得する helper。
func fetchSession(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tokenHash string) (issuedAt, lastSeenAt, expiresAt time.Time, revokedAt *time.Time, console string, found bool) {
	t.Helper()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", uuid.Nil.String()); err != nil {
		t.Fatalf("set_config tenant_id: %v", err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.is_superadmin', 'true', true)"); err != nil {
		t.Fatalf("set_config is_superadmin: %v", err)
	}
	err = tx.QueryRow(ctx,
		`SELECT issued_at, last_seen_at, expires_at, revoked_at, console FROM sessions WHERE token_hash = $1`,
		tokenHash,
	).Scan(&issuedAt, &lastSeenAt, &expiresAt, &revokedAt, &console)
	if err != nil {
		if stdErrors.Is(err, pgx.ErrNoRows) {
			return
		}
		t.Fatalf("fetchSession: %v", err)
	}
	found = true
	return
}

// fetchAdminUserEmail は admin_users.email を返す helper。
func fetchAdminUserEmail(t *testing.T, ctx context.Context, pool *pgxpool.Pool, adminUserID uuid.UUID) string {
	t.Helper()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", uuid.Nil.String()); err != nil {
		t.Fatalf("set_config tenant_id: %v", err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.is_superadmin', 'true', true)"); err != nil {
		t.Fatalf("set_config is_superadmin: %v", err)
	}
	var email string
	if err := tx.QueryRow(ctx, `SELECT email FROM admin_users WHERE id = $1`, adminUserID).Scan(&email); err != nil {
		t.Fatalf("fetchAdminUserEmail: %v", err)
	}
	return email
}

// countStateNonces は state_nonces の行数を SuperAdmin 文脈で取得する helper。
func countStateNonces(t *testing.T, ctx context.Context, pool *pgxpool.Pool, nonce string) int {
	t.Helper()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", uuid.Nil.String()); err != nil {
		t.Fatalf("set_config tenant_id: %v", err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.is_superadmin', 'true', true)"); err != nil {
		t.Fatalf("set_config is_superadmin: %v", err)
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM state_nonces WHERE nonce = $1`, nonce).Scan(&n); err != nil {
		t.Fatalf("countStateNonces: %v", err)
	}
	return n
}

// errorContainsFailureKind は err.Error() に対応する failure_kind 文字列が含まれるかを
// best-effort で判定する（Cause チェーン上に auth.FailureKind* が含まれるかを文字列マッチで確認）。
func errorContainsFailureKind(err error, want string) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), want)
}

// TestAuthRepository_ResolveAdminUser_NotProvisioned はシナリオ (a) 対応。
func TestAuthRepository_ResolveAdminUser_NotProvisioned(t *testing.T) {
	fixture, cleanup := setupAuthRepoFixture(t)
	defer cleanup()

	repo := auth.NewRepository(fixture.pool)

	// admin_users に対応 sub が無い状態で resolve → 403 + admin_user_not_provisioned
	identity, err := repo.ResolveAdminUser(fixture.ctx, repoTestIssuer, "unknown-sub", "user@example.com", oidc.ConsoleTenant)
	if err == nil {
		t.Fatalf("ResolveAdminUser: 期待: error / actual: nil (identity=%+v)", identity)
	}
	var apiErr *internalerrors.Error
	if !stdErrors.As(err, &apiErr) {
		t.Fatalf("err は *internalerrors.Error でない: %T (%v)", err, err)
	}
	if apiErr.Code != internalerrors.CodeForbidden {
		t.Errorf("Code = %s; want %s", apiErr.Code, internalerrors.CodeForbidden)
	}
	if !errorContainsFailureKind(err, "admin_user_not_provisioned") {
		t.Errorf("err 文言に admin_user_not_provisioned が含まれない: %v", err)
	}
}

// TestAuthRepository_ResolveAdminUser_Provisioned はシナリオ (b) 対応。
func TestAuthRepository_ResolveAdminUser_Provisioned(t *testing.T) {
	fixture, cleanup := setupAuthRepoFixture(t)
	defer cleanup()

	tenantID := seedTenant(t, fixture.ctx, fixture.pool, "Tenant-Provisioned")
	adminUserID := seedAdminUser(t, fixture.ctx, fixture.pool, repoTestIssuer, "sub-provisioned", "old@example.com", tenantID, []string{"TenantAdmin", "Operator"})

	repo := auth.NewRepository(fixture.pool)
	identity, err := repo.ResolveAdminUser(fixture.ctx, repoTestIssuer, "sub-provisioned", "old@example.com", oidc.ConsoleTenant)
	if err != nil {
		t.Fatalf("ResolveAdminUser: %v", err)
	}
	if identity.AdminUserID != adminUserID {
		t.Errorf("AdminUserID = %s; want %s", identity.AdminUserID, adminUserID)
	}
	if identity.TenantID != tenantID {
		t.Errorf("TenantID = %s; want %s", identity.TenantID, tenantID)
	}
	if identity.OIDCSubject != "sub-provisioned" {
		t.Errorf("OIDCSubject = %s; want sub-provisioned", identity.OIDCSubject)
	}
	if identity.Email != "old@example.com" {
		t.Errorf("Email = %s; want old@example.com", identity.Email)
	}
	if identity.IsSuperAdmin {
		t.Errorf("IsSuperAdmin = true; want false (TenantAdmin / Operator のみ)")
	}
	// Roles に "TenantAdmin" と "Operator" が含まれていること（順序は不問）
	wantRoles := map[string]bool{"TenantAdmin": false, "Operator": false}
	for _, r := range identity.Roles {
		wantRoles[r] = true
	}
	for role, found := range wantRoles {
		if !found {
			t.Errorf("Roles に %q が含まれない: %v", role, identity.Roles)
		}
	}
}

// TestAuthRepository_ResolveAdminUser_EmailUpdate はシナリオ (c) 対応。
func TestAuthRepository_ResolveAdminUser_EmailUpdate(t *testing.T) {
	fixture, cleanup := setupAuthRepoFixture(t)
	defer cleanup()

	tenantID := seedTenant(t, fixture.ctx, fixture.pool, "Tenant-Email")
	adminUserID := seedAdminUser(t, fixture.ctx, fixture.pool, repoTestIssuer, "sub-email", "old@example.com", tenantID, []string{"Viewer"})

	repo := auth.NewRepository(fixture.pool)
	identity, err := repo.ResolveAdminUser(fixture.ctx, repoTestIssuer, "sub-email", "new@example.com", oidc.ConsoleTenant)
	if err != nil {
		t.Fatalf("ResolveAdminUser: %v", err)
	}
	if identity.AdminUserID != adminUserID {
		t.Errorf("AdminUserID = %s; want %s (id は不変)", identity.AdminUserID, adminUserID)
	}
	if identity.TenantID != tenantID {
		t.Errorf("TenantID = %s; want %s (tenant_id は不変)", identity.TenantID, tenantID)
	}
	if identity.Email != "new@example.com" {
		t.Errorf("Identity.Email = %s; want new@example.com (IdP 側値で UPDATE)", identity.Email)
	}
	// DB 側 admin_users.email が UPDATE されていること
	if got := fetchAdminUserEmail(t, fixture.ctx, fixture.pool, adminUserID); got != "new@example.com" {
		t.Errorf("DB admin_users.email = %s; want new@example.com", got)
	}
}

// TestAuthRepository_Create_Get_Touch_Revoke はシナリオ (d) / (f) / (g) / (h) を 1 テストで連続検証。
func TestAuthRepository_Create_Get_Touch_Revoke(t *testing.T) {
	fixture, cleanup := setupAuthRepoFixture(t)
	defer cleanup()

	tenantID := seedTenant(t, fixture.ctx, fixture.pool, "Tenant-Session")
	adminUserID := seedAdminUser(t, fixture.ctx, fixture.pool, repoTestIssuer, "sub-session", "session@example.com", tenantID, []string{"TenantAdmin"})

	repo := auth.NewRepository(fixture.pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	tokenHash := fmt.Sprintf("hash-%s", uuid.NewString())
	expiresAt := now.Add(8 * time.Hour)
	session := auth.Session{
		TokenHash:   tokenHash,
		AdminUserID: adminUserID,
		Console:     oidc.ConsoleTenant,
		IssuedAt:    now,
		LastSeenAt:  now,
		ExpiresAt:   expiresAt,
		RevokedAt:   nil,
	}

	// (d) Create
	if err := repo.Create(fixture.ctx, session); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// (d) Get → session / identity が返る
	got, identity, err := repo.Get(fixture.ctx, tokenHash)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AdminUserID != adminUserID {
		t.Errorf("got.AdminUserID = %s; want %s", got.AdminUserID, adminUserID)
	}
	if got.Console != oidc.ConsoleTenant {
		t.Errorf("got.Console = %s; want %s", got.Console, oidc.ConsoleTenant)
	}
	if identity.AdminUserID != adminUserID {
		t.Errorf("identity.AdminUserID = %s; want %s", identity.AdminUserID, adminUserID)
	}
	if identity.TenantID != tenantID {
		t.Errorf("identity.TenantID = %s; want %s", identity.TenantID, tenantID)
	}
	if identity.Email != "session@example.com" {
		t.Errorf("identity.Email = %s; want session@example.com", identity.Email)
	}

	// (f) Touch
	later := now.Add(5 * time.Minute)
	if err := repo.Touch(fixture.ctx, tokenHash, later); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	_, gotLastSeenAt, gotExpiresAt, _, _, found := fetchSession(t, fixture.ctx, fixture.pool, tokenHash)
	if !found {
		t.Fatalf("session row 不在")
	}
	if !gotLastSeenAt.Equal(later) {
		t.Errorf("last_seen_at = %s; want %s (Touch 後)", gotLastSeenAt, later)
	}
	if !gotExpiresAt.Equal(expiresAt) {
		t.Errorf("expires_at = %s; want %s (Touch は expires_at を変更しない / Req 4.8)", gotExpiresAt, expiresAt)
	}

	// (g) Revoke 1 回目
	revokeAt := now.Add(10 * time.Minute)
	if err := repo.Revoke(fixture.ctx, tokenHash, revokeAt); err != nil {
		t.Fatalf("Revoke #1: %v", err)
	}
	_, _, _, gotRevokedAt, _, found := fetchSession(t, fixture.ctx, fixture.pool, tokenHash)
	if !found {
		t.Fatalf("session row 不在 (after revoke)")
	}
	if gotRevokedAt == nil {
		t.Fatalf("revoked_at = nil; want 設定されている")
	}
	if !gotRevokedAt.Equal(revokeAt) {
		t.Errorf("revoked_at = %s; want %s", *gotRevokedAt, revokeAt)
	}

	// (h) Revoke 2 回目（冪等性 / revoked_at は変更されない）
	revokeAt2 := now.Add(30 * time.Minute)
	if err := repo.Revoke(fixture.ctx, tokenHash, revokeAt2); err != nil {
		t.Fatalf("Revoke #2: %v", err)
	}
	_, _, _, gotRevokedAt2, _, found := fetchSession(t, fixture.ctx, fixture.pool, tokenHash)
	if !found {
		t.Fatalf("session row 不在 (after revoke #2)")
	}
	if gotRevokedAt2 == nil {
		t.Fatalf("revoked_at = nil after revoke #2; want 設定されている")
	}
	if !gotRevokedAt2.Equal(revokeAt) {
		t.Errorf("revoked_at after #2 = %s; want %s (冪等性 / 1 回目の値が保持されるべき / Req 5.1)", *gotRevokedAt2, revokeAt)
	}
}

// TestAuthRepository_Get_HashMismatch_SessionTamper はシナリオ (e) 対応。
func TestAuthRepository_Get_HashMismatch_SessionTamper(t *testing.T) {
	fixture, cleanup := setupAuthRepoFixture(t)
	defer cleanup()

	repo := auth.NewRepository(fixture.pool)
	_, _, err := repo.Get(fixture.ctx, "nonexistent-hash")
	if err == nil {
		t.Fatalf("Get: 期待: error / actual: nil")
	}
	var apiErr *internalerrors.Error
	if !stdErrors.As(err, &apiErr) {
		t.Fatalf("err は *internalerrors.Error でない: %T (%v)", err, err)
	}
	if apiErr.Code != internalerrors.CodeUnauthenticated {
		t.Errorf("Code = %s; want %s", apiErr.Code, internalerrors.CodeUnauthenticated)
	}
	if !errorContainsFailureKind(err, "session_tamper") {
		t.Errorf("err 文言に session_tamper が含まれない: %v", err)
	}
}

// TestAuthRepository_ConsumeStateNonce_FirstCall はシナリオ (i) 対応。
func TestAuthRepository_ConsumeStateNonce_FirstCall(t *testing.T) {
	fixture, cleanup := setupAuthRepoFixture(t)
	defer cleanup()

	repo := auth.NewRepository(fixture.pool)
	nonce := fmt.Sprintf("nonce-first-%s", uuid.NewString())
	expiresAt := time.Now().Add(10 * time.Minute)

	if err := repo.ConsumeStateNonce(fixture.ctx, nonce, oidc.ConsoleTenant, expiresAt); err != nil {
		t.Fatalf("ConsumeStateNonce 初回: %v", err)
	}
	if got := countStateNonces(t, fixture.ctx, fixture.pool, nonce); got != 1 {
		t.Errorf("state_nonces 行数 = %d; want 1", got)
	}
}

// TestAuthRepository_ConsumeStateNonce_Replay はシナリオ (j) 対応。
func TestAuthRepository_ConsumeStateNonce_Replay(t *testing.T) {
	fixture, cleanup := setupAuthRepoFixture(t)
	defer cleanup()

	repo := auth.NewRepository(fixture.pool)
	nonce := fmt.Sprintf("nonce-replay-%s", uuid.NewString())
	expiresAt := time.Now().Add(10 * time.Minute)

	if err := repo.ConsumeStateNonce(fixture.ctx, nonce, oidc.ConsoleTenant, expiresAt); err != nil {
		t.Fatalf("ConsumeStateNonce #1: %v", err)
	}
	// 2 回目 → state_replay
	err := repo.ConsumeStateNonce(fixture.ctx, nonce, oidc.ConsoleTenant, expiresAt)
	if err == nil {
		t.Fatalf("ConsumeStateNonce #2: 期待: error / actual: nil")
	}
	var apiErr *internalerrors.Error
	if !stdErrors.As(err, &apiErr) {
		t.Fatalf("err は *internalerrors.Error でない: %T (%v)", err, err)
	}
	if apiErr.Code != internalerrors.CodeUnauthenticated {
		t.Errorf("Code = %s; want %s", apiErr.Code, internalerrors.CodeUnauthenticated)
	}
	if !errorContainsFailureKind(err, "state_replay") {
		t.Errorf("err 文言に state_replay が含まれない: %v", err)
	}
}

// TestAuthRepository_ConsumeStateNonce_DifferentNonces_NoInterference はシナリオ (k) 対応。
func TestAuthRepository_ConsumeStateNonce_DifferentNonces_NoInterference(t *testing.T) {
	fixture, cleanup := setupAuthRepoFixture(t)
	defer cleanup()

	repo := auth.NewRepository(fixture.pool)
	expiresAt := time.Now().Add(10 * time.Minute)

	// 異なる nonce / console を並行に消費 → 干渉なし
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	nonces := make([]string, n)
	for i := 0; i < n; i++ {
		nonces[i] = fmt.Sprintf("nonce-parallel-%d-%s", i, uuid.NewString())
	}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			console := oidc.ConsoleTenant
			if idx%2 == 1 {
				console = oidc.ConsoleAdmin
			}
			errs[idx] = repo.ConsumeStateNonce(fixture.ctx, nonces[idx], console, expiresAt)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("並行 ConsumeStateNonce[%d] (%s): %v", i, nonces[i], err)
		}
	}
	for _, nonce := range nonces {
		if got := countStateNonces(t, fixture.ctx, fixture.pool, nonce); got != 1 {
			t.Errorf("state_nonces[%s] 行数 = %d; want 1", nonce, got)
		}
	}
}
