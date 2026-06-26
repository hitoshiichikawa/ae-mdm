// Package integration_test は ae-mdm の結合テスト（実 PostgreSQL を用いる）を保持する。
//
// 本 package は backend/internal/* と物理的に分離した位置（backend/test/integration）に
// 置かれており、CI / ローカルで実 DB が利用可能な環境でのみ実行される。`DATABASE_URL`
// （正確には `INTEGRATION_TEST_DATABASE_URL` / `INTEGRATION_TEST_MIGRATE_URL`、無ければ
// `DATABASE_URL` / `MIGRATE_DATABASE_URL` にフォールバック）が未設定の環境では各テストが
// 自身で t.Skip して落ちないようにする（design.md「Testing Strategy」と整合）。
//
// requirements.md Req 4.5 / 5.5 / 6.3 / 6.4 / 7.1〜7.4 / NFR 1.1 / NFR 1.2 / NFR 2.1 / NFR 2.2
// に対応する Integration Tests（tasks.md 6.1 / 6.2 の詳細項目を参照）。
package integration_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	// `postgres://` スキームの URL は `database/postgres` driver が処理する。
	// `.env.example` の `DATABASE_URL` / `MIGRATE_DATABASE_URL` は `postgres://...` で
	// 書かれているため、pgx/v5 driver（scheme: `pgx5://`）だけでは migrate.New が
	// "unknown driver postgres" で失敗する（PR #31 round-3 review 由来）。両 driver を
	// blank import することで `postgres://` / `pgx5://` どちらの URL でも動く構成にする。
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationsRelPath は test ファイル基準で migration ディレクトリへの相対パス。
// `backend/test/integration` から見て `../../db/migrations` にあるため。
const migrationsRelPath = "../../db/migrations"

// connectTimeout は DB 接続 / migration 適用に被せる context timeout。
const connectTimeout = 30 * time.Second

// dbURLs は integration test で使う 2 系統の接続文字列を保持する struct。
type dbURLs struct {
	app     string // app_user（DML + RLS バインド）
	migrate string // migration_user（DDL）
}

// requireDBURLs は env から 2 系統の接続文字列を取得する。1 つでも未設定なら
// t.Skip で test を skip する（CI / DB 不在環境でも fail させないため）。
//
// 優先順:
//   - app:     INTEGRATION_TEST_DATABASE_URL → DATABASE_URL
//   - migrate: INTEGRATION_TEST_MIGRATE_URL → MIGRATE_DATABASE_URL
//
// .env.example の `DATABASE_URL` は docker compose 内 hostname（`postgres:5432`）を指し、
// ホストから直接 test を走らせる場合は解決不能になるため、ホスト実行用に
// INTEGRATION_TEST_* env を別途上書きできるよう 2 段階の fallback にしてある。
func requireDBURLs(t *testing.T) dbURLs {
	t.Helper()
	app := firstNonEmpty(os.Getenv("INTEGRATION_TEST_DATABASE_URL"), os.Getenv("DATABASE_URL"))
	mig := firstNonEmpty(os.Getenv("INTEGRATION_TEST_MIGRATE_URL"), os.Getenv("MIGRATE_DATABASE_URL"))
	if app == "" || mig == "" {
		t.Skip("INTEGRATION_TEST_DATABASE_URL/INTEGRATION_TEST_MIGRATE_URL (or DATABASE_URL/MIGRATE_DATABASE_URL) が未設定のため skip")
	}
	return dbURLs{app: app, migrate: mig}
}

// firstNonEmpty は引数のうち最初の非空文字列を返す。すべて空なら空文字列。
func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// migrationsURL は test ファイル基準の相対パスを golang-migrate 用 `file://...` URL に変換する。
func migrationsURL(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(migrationsRelPath)
	if err != nil {
		t.Fatalf("migration ディレクトリの絶対パス化に失敗: %v", err)
	}
	return "file://" + abs
}

// migrateInstance は migrate.New で migration インスタンスを構築する helper。
// 呼び出し側は defer m.Close() を必ず行うこと。
func migrateInstance(t *testing.T, migrateURL string) *migrate.Migrate {
	t.Helper()
	m, err := migrate.New(migrationsURL(t), migrateURL)
	if err != nil {
		t.Fatalf("migrate.New: %v", err)
	}
	return m
}

// applyMigrationsUp は migrate-up 相当を実行する。ErrNoChange は no-op として成功扱い。
func applyMigrationsUp(t *testing.T, migrateURL string) {
	t.Helper()
	m := migrateInstance(t, migrateURL)
	defer func() { _, _ = m.Close() }()
	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate up: %v", err)
	}
}

// applyMigrationsDown は migrate-down 相当を実行し、すべての migration を巻き戻す。
// ErrNoChange は no-op として成功扱い。
func applyMigrationsDown(t *testing.T, migrateURL string) {
	t.Helper()
	m := migrateInstance(t, migrateURL)
	defer func() { _, _ = m.Close() }()
	if err := m.Down(); err != nil && err != migrate.ErrNoChange {
		t.Fatalf("migrate down: %v", err)
	}
}

// newAppPool は app_user 接続 pool を構築する helper。失敗時は t.Fatal。
func newAppPool(t *testing.T, ctx context.Context, urls dbURLs) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(urls.app)
	if err != nil {
		t.Fatalf("pgxpool.ParseConfig(app): %v", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pgxpool.NewWithConfig(app): %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("app pool ping: %v", err)
	}
	return pool
}

// truncateAll は seed の冪等性のために主要テーブルを TRUNCATE する。
// migration_user 接続で実行する（app_user は audit_logs の TRUNCATE 不可）。
// CASCADE で FK を辿って依存テーブルもまとめてクリアする。
//
// audit_logs は `FORCE ROW LEVEL SECURITY` が効いており、TRUNCATE は RLS の影響を受けない
// （TRUNCATE は table 単位の権限で判定）。migration_user は table owner であり TRUNCATE 可能。
func truncateAll(t *testing.T, ctx context.Context, urls dbURLs) {
	t.Helper()
	conn, err := pgx.Connect(ctx, urls.migrate)
	if err != nil {
		t.Fatalf("migration_user 接続失敗: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	// 順序は FK 依存を CASCADE で吸収させるため、最上位の tenants から TRUNCATE すれば
	// 子テーブルもまとめてクリアされる。audit_logs と sessions / infra テーブルは
	// 個別に TRUNCATE する（tenants から CASCADE しない経路があるため）。
	stmts := []string{
		"TRUNCATE TABLE audit_logs RESTART IDENTITY CASCADE",
		"TRUNCATE TABLE notification_dedupe RESTART IDENTITY CASCADE",
		"TRUNCATE TABLE unassigned_notifications RESTART IDENTITY CASCADE",
		"TRUNCATE TABLE sessions RESTART IDENTITY CASCADE",
		"TRUNCATE TABLE tenants RESTART IDENTITY CASCADE",
	}
	for _, s := range stmts {
		if _, err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("TRUNCATE 失敗 (%q): %v", s, err)
		}
	}
}

// seededIDs はテスト全体で使う seed 済 ID 群（tenant A / B, admin_users A / B など）。
type seededIDs struct {
	tenantAID  uuid.UUID
	tenantBID  uuid.UUID
	adminAID   uuid.UUID
	adminBID   uuid.UUID
	deviceAID  uuid.UUID
	deviceBID  uuid.UUID
	policyAID  uuid.UUID
	policyBID  uuid.UUID
	auditAID   uuid.UUID // tenant A 配下の audit_logs 行
	auditBID   uuid.UUID // tenant B 配下の audit_logs 行
	sessionATH string    // tenant A 配下 session の token_hash
	sessionBTH string    // tenant B 配下 session の token_hash
}

// seedDummyData は 2 テナント分の最小ダミーデータを SuperAdmin 文脈で投入する。
// SuperAdmin GUC を set してから INSERT することで RLS で弾かれずに済む。
//
// migration_user 接続を直接使うのは「ownership 経由で RLS をバイパス」しないため不可
// （audit_logs の FORCE RLS により INSERT に WITH CHECK が必須）。pool 経由で BeginTx →
// set_config('app.is_superadmin', 'true', true) → INSERT 一連を実施する。
func seedDummyData(t *testing.T, ctx context.Context, pool *pgxpool.Pool) seededIDs {
	t.Helper()
	ids := seededIDs{
		tenantAID:  uuid.New(),
		tenantBID:  uuid.New(),
		adminAID:   uuid.New(),
		adminBID:   uuid.New(),
		deviceAID:  uuid.New(),
		deviceBID:  uuid.New(),
		policyAID:  uuid.New(),
		policyBID:  uuid.New(),
		auditAID:   uuid.New(),
		auditBID:   uuid.New(),
		sessionATH: fmt.Sprintf("token-hash-A-%s", uuid.NewString()),
		sessionBTH: fmt.Sprintf("token-hash-B-%s", uuid.NewString()),
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// SuperAdmin 文脈で全 RLS をバイパスして seed する。
	if _, err := tx.Exec(ctx, "SELECT set_config('app.is_superadmin', 'true', true)"); err != nil {
		t.Fatalf("set_config app.is_superadmin: %v", err)
	}

	// tenants
	for _, p := range []struct {
		id   uuid.UUID
		name string
	}{
		{ids.tenantAID, "Tenant A"},
		{ids.tenantBID, "Tenant B"},
	} {
		if _, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, name, status) VALUES ($1, $2, 'bound')`,
			p.id, p.name); err != nil {
			t.Fatalf("INSERT tenants: %v", err)
		}
	}

	// admin_users
	// 0014_admin_users_add_oidc_issuer で oidc_issuer NOT NULL 列が追加されたため、
	// 既存 INSERT に oidc_issuer カラム + テスト用 issuer URL を追加する
	// （fixture では tenant / admin で別 issuer を使わないため共通の test issuer URL を入れる）。
	const testOIDCIssuer = "https://idp.test.example.com/realms/ae-mdm-test"
	for _, p := range []struct {
		id       uuid.UUID
		tenantID uuid.UUID
		sub      string
		email    string
	}{
		{ids.adminAID, ids.tenantAID, "sub-A", "admin-a@example.com"},
		{ids.adminBID, ids.tenantBID, "sub-B", "admin-b@example.com"},
	} {
		if _, err := tx.Exec(ctx,
			`INSERT INTO admin_users (id, oidc_subject, oidc_issuer, email, tenant_id) VALUES ($1, $2, $3, $4, $5)`,
			p.id, p.sub, testOIDCIssuer, p.email, p.tenantID); err != nil {
			t.Fatalf("INSERT admin_users: %v", err)
		}
	}

	// policies
	for _, p := range []struct {
		id        uuid.UUID
		tenantID  uuid.UUID
		name      string
		amapiName string
	}{
		{ids.policyAID, ids.tenantAID, "policy-A", "enterprises/X/policies/A"},
		{ids.policyBID, ids.tenantBID, "policy-B", "enterprises/X/policies/B"},
	} {
		if _, err := tx.Exec(ctx,
			`INSERT INTO policies (id, tenant_id, name, amapi_policy_name) VALUES ($1, $2, $3, $4)`,
			p.id, p.tenantID, p.name, p.amapiName); err != nil {
			t.Fatalf("INSERT policies: %v", err)
		}
	}

	// devices
	for _, p := range []struct {
		id        uuid.UUID
		tenantID  uuid.UUID
		amapiName string
	}{
		{ids.deviceAID, ids.tenantAID, "enterprises/X/devices/A1"},
		{ids.deviceBID, ids.tenantBID, "enterprises/X/devices/B1"},
	} {
		if _, err := tx.Exec(ctx,
			`INSERT INTO devices (id, tenant_id, amapi_device_name, mode) VALUES ($1, $2, $3, 'fully_managed')`,
			p.id, p.tenantID, p.amapiName); err != nil {
			t.Fatalf("INSERT devices: %v", err)
		}
	}

	// sessions
	now := time.Now()
	for _, p := range []struct {
		tokenHash   string
		adminUserID uuid.UUID
	}{
		{ids.sessionATH, ids.adminAID},
		{ids.sessionBTH, ids.adminBID},
	} {
		// 0013_extend_sessions で idle_at → last_seen_at に rename され、console NOT NULL
		// （'tenant-console' / 'admin-console' のいずれか）が追加された。fixture は A2 同様の
		// セッション生存条件を維持するため last_seen_at = now（直近アクセス済み）/
		// console = 'tenant-console' を明示指定する（revoked_at は NULL のまま）。
		if _, err := tx.Exec(ctx,
			`INSERT INTO sessions (token_hash, admin_user_id, last_seen_at, expires_at, console) VALUES ($1, $2, $3, $4, $5)`,
			p.tokenHash, p.adminUserID, now, now.Add(8*time.Hour), "tenant-console"); err != nil {
			t.Fatalf("INSERT sessions: %v", err)
		}
	}

	// audit_logs
	for _, p := range []struct {
		id        uuid.UUID
		tenantID  uuid.UUID
		eventType string
		actorID   uuid.UUID
	}{
		{ids.auditAID, ids.tenantAID, "seed.event.A", ids.adminAID},
		{ids.auditBID, ids.tenantBID, "seed.event.B", ids.adminBID},
	} {
		if _, err := tx.Exec(ctx,
			`INSERT INTO audit_logs (id, tenant_id, actor_id, event_type, result) VALUES ($1, $2, $3, $4, 'success')`,
			p.id, p.tenantID, p.actorID, p.eventType); err != nil {
			t.Fatalf("INSERT audit_logs: %v", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	return ids
}
