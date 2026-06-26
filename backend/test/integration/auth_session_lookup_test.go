package integration_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hitoshiichikawa/ae-mdm/internal/auth"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/oidc"
)

// 本ファイルは Issue #33 task 6.4 の session lookup 結合テスト。
// 既存 session 行を test 側で seed し、auth.Middleware（tenant / admin）配下の probe handler
// に HTTP 経路で到達するかを検証する。fakeClock で「現在時刻」を操作することで idle /
// absolute timeout の境界経路を再現する。
//
// シナリオ（tasks.md task 6.4 L737〜L743）:
//   (a) cookie 提示で 200 + AuthClaims が ctx に到達
//   (b) idle 31 分後の再アクセスで 401 + revoked_at 更新
//   (c) absolute 8h+1s 後の再アクセスで 401
//   (d) tenant 用 session cookie を /api/admin/... に提示すると 401 + console_mismatch +
//       cookie 削除

// setupLookupFixture は test 用 DB + mock IdP + auth stack + HTTP server を構築する。
func setupLookupFixture(t *testing.T) (*e2eAuthStack, *httptest.Server, func()) {
	t.Helper()
	urls := requireDBURLs(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	applyMigrationsUp(t, urls.migrate)
	truncateAll(t, ctx, urls)
	pool := newAppPool(t, ctx, urls)

	stack := newE2EAuthStack(t, ctx, pool)
	ts := newAuthHTTPServer(t, stack)

	cleanup := func() {
		pool.Close()
		cancel()
	}
	return stack, ts, cleanup
}

// seedActiveSession は test 用 admin_users + tenant + sessions 1 行を seed する。
//
// 戻り値: rawToken（cookie value 用）, tokenHash, adminUserID
func seedActiveSession(t *testing.T, ctx context.Context, pool *pgxpool.Pool, issuer string, console oidc.Console, lastSeenAt, expiresAt time.Time) (rawToken, tokenHash string, adminUserID, tenantID uuid.UUID) {
	t.Helper()
	tenantID = seedTenant(t, ctx, pool, "Tenant-Lookup")
	adminUserID = seedAdminUser(t, ctx, pool, issuer, "sub-"+uuid.NewString(), "lookup@example.com", tenantID, []string{"TenantAdmin"})

	raw, err := auth.New()
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	hash := auth.HashToken(raw)
	now := lastSeenAt
	repo := auth.NewRepository(pool)
	if err := repo.Create(ctx, auth.Session{
		TokenHash:   hash,
		AdminUserID: adminUserID,
		Console:     console,
		IssuedAt:    now,
		LastSeenAt:  lastSeenAt,
		ExpiresAt:   expiresAt,
		RevokedAt:   nil,
	}); err != nil {
		t.Fatalf("Repository.Create: %v", err)
	}
	return raw, hash, adminUserID, tenantID
}

// TestAuthLookup_ActiveSession_Returns200WithAuthClaims はシナリオ (a) 対応。
func TestAuthLookup_ActiveSession_Returns200WithAuthClaims(t *testing.T) {
	stack, ts, cleanup := setupLookupFixture(t)
	defer cleanup()

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	stack.clock.Set(now)
	rawToken, _, adminUserID, _ := seedActiveSession(t, ctx, stack.pool, stack.idp.issuer, oidc.ConsoleTenant, now.Add(-1*time.Minute), now.Add(7*time.Hour))

	req := mustNewRequest(t, http.MethodGet, ts.URL+"/api/probe")
	req.AddCookie(&http.Cookie{Name: "__Host-ae_mdm_session", Value: rawToken})

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/probe: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d; want 200 (body=%s)", resp.StatusCode, string(body))
	}
	body, _ := io.ReadAll(resp.Body)
	var got struct {
		AdminUserID  string `json:"admin_user_id"`
		IsSuperAdmin bool   `json:"is_super_admin"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal probe response: %v (body=%s)", err, string(body))
	}
	if got.AdminUserID != adminUserID.String() {
		t.Errorf("AuthClaims.AdminUserID = %s; want %s", got.AdminUserID, adminUserID)
	}
}

// TestAuthLookup_IdleExceeded_Returns401AndRevokes はシナリオ (b) 対応。
//
// last_seen_at から 31 分経過した状態で再アクセス → 401 + revoked_at がセットされる。
func TestAuthLookup_IdleExceeded_Returns401AndRevokes(t *testing.T) {
	stack, ts, cleanup := setupLookupFixture(t)
	defer cleanup()

	ctx := context.Background()
	baseTime := time.Now().UTC().Truncate(time.Microsecond)
	lastSeenAt := baseTime.Add(-31 * time.Minute) // 31 分前にアクセス済み
	expiresAt := baseTime.Add(7 * time.Hour)      // absolute は OK
	stack.clock.Set(baseTime)
	rawToken, hash, _, _ := seedActiveSession(t, ctx, stack.pool, stack.idp.issuer, oidc.ConsoleTenant, lastSeenAt, expiresAt)

	req := mustNewRequest(t, http.MethodGet, ts.URL+"/api/probe")
	req.AddCookie(&http.Cookie{Name: "__Host-ae_mdm_session", Value: rawToken})

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/probe: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401", resp.StatusCode)
	}
	if !stack.log.hasFailureKind("session_idle") {
		t.Errorf("log に failure_kind=session_idle が記録されていない: %+v", stack.log.entries)
	}
	// revoked_at がセットされていること（idle 失効時に repo.Revoke が呼ばれる / Req 4.6）
	if !sessionIsRevoked(t, ctx, stack.pool, hash) {
		t.Errorf("session の revoked_at が NULL のまま（idle 失効時に repo.Revoke されるべき / Req 4.6）")
	}
}

// TestAuthLookup_AbsoluteExceeded_Returns401 はシナリオ (c) 対応。
func TestAuthLookup_AbsoluteExceeded_Returns401(t *testing.T) {
	stack, ts, cleanup := setupLookupFixture(t)
	defer cleanup()

	ctx := context.Background()
	baseTime := time.Now().UTC().Truncate(time.Microsecond)
	// expires_at は 1 秒前（absolute 経過）/ last_seen_at は 5 分前（idle 内）
	expiresAt := baseTime.Add(-1 * time.Second)
	lastSeenAt := baseTime.Add(-5 * time.Minute)
	stack.clock.Set(baseTime)
	rawToken, hash, _, _ := seedActiveSession(t, ctx, stack.pool, stack.idp.issuer, oidc.ConsoleTenant, lastSeenAt, expiresAt)

	req := mustNewRequest(t, http.MethodGet, ts.URL+"/api/probe")
	req.AddCookie(&http.Cookie{Name: "__Host-ae_mdm_session", Value: rawToken})

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/probe: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401", resp.StatusCode)
	}
	if !stack.log.hasFailureKind("session_expired") {
		t.Errorf("log に failure_kind=session_expired が記録されていない: %+v", stack.log.entries)
	}
	if !sessionIsRevoked(t, ctx, stack.pool, hash) {
		t.Errorf("session の revoked_at が NULL のまま（absolute 失効時も repo.Revoke されるべき / Req 4.6）")
	}
}

// TestAuthLookup_TenantSessionOnAdminRoute_Returns401WithConsoleMismatch はシナリオ (d) 対応。
//
// Console=tenant-console の session を /api/admin/probe に提示すると、admin middleware の
// expectedConsole=ConsoleAdmin に対し Session.Console が不一致 → console_mismatch (401) +
// cookie 削除。
func TestAuthLookup_TenantSessionOnAdminRoute_Returns401WithConsoleMismatch(t *testing.T) {
	stack, ts, cleanup := setupLookupFixture(t)
	defer cleanup()

	ctx := context.Background()
	baseTime := time.Now().UTC().Truncate(time.Microsecond)
	stack.clock.Set(baseTime)
	// tenant 系 session を作る
	rawToken, hash, _, _ := seedActiveSession(t, ctx, stack.pool, stack.idp.issuer, oidc.ConsoleTenant, baseTime.Add(-1*time.Minute), baseTime.Add(7*time.Hour))

	req := mustNewRequest(t, http.MethodGet, ts.URL+"/api/admin/probe")
	req.AddCookie(&http.Cookie{Name: "__Host-ae_mdm_session", Value: rawToken})

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/admin/probe: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401", resp.StatusCode)
	}
	if !stack.log.hasFailureKindWithConsole("console_mismatch", "admin-console") {
		t.Errorf("log に failure_kind=console_mismatch + console=admin-console が記録されていない: %+v", stack.log.entries)
	}
	// session cookie 削除属性が発行されている
	expireCookie := findCookie(resp.Cookies(), "__Host-ae_mdm_session")
	if expireCookie == nil {
		t.Fatalf("session cookie 削除属性が発行されていない")
	}
	if expireCookie.MaxAge >= 0 {
		t.Errorf("session cookie MaxAge = %d; want < 0 (削除)", expireCookie.MaxAge)
	}
	// console_mismatch 経路でも repo.Revoke が呼ばれる（Req 4.6）
	if !sessionIsRevoked(t, ctx, stack.pool, hash) {
		t.Errorf("session の revoked_at が NULL のまま（console_mismatch 時も repo.Revoke されるべき）")
	}
}

// sessionIsRevoked は token_hash の session の revoked_at が NULL でないかを返す。
func sessionIsRevoked(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tokenHash string) bool {
	t.Helper()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	setSuperAdmin(t, ctx, tx)
	var revokedAt *time.Time
	if err := tx.QueryRow(ctx, `SELECT revoked_at FROM sessions WHERE token_hash = $1`, tokenHash).Scan(&revokedAt); err != nil {
		t.Fatalf("sessionIsRevoked: %v", err)
	}
	return revokedAt != nil
}
