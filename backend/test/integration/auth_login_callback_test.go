package integration_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hitoshiichikawa/ae-mdm/internal/auth"
)

// 本ファイルは Issue #33 task 6.4 の `/api/auth/login` / `/api/auth/callback` 結合テスト。
// DATABASE_URL 未設定環境では各テストが自身で t.Skip する。
//
// シナリオ（tasks.md task 6.4 L716〜L735）:
//   (a) GET /api/auth/login?return_to=/dashboard → 302 + state cookie + Location が IdP
//       認可エンドポイント
//   (b) GET /api/auth/callback で session 作成・sessions テーブル 1 行・cookie 生値 /
//       DB hash・state cookie 削除 + Location=/dashboard
//   (c) state cookie 改竄 → 401 + failure_kind=state_invalid ログ
//   (d) ID トークン aud 不一致 → 401 + failure_kind=invalid_aud
//   (e) 未 provisioning な OIDC (issuer, subject) → 403 + admin_user_not_provisioned
//   (f) 同一 (state cookie, query state) で 2 度提示 → 2 回目 401 + state_replay +
//       state cookie 削除 + sessions に追加行が無い

// setupCallbackFixture は test 用 DB + mock IdP + auth stack + HTTP server を構築する。
func setupCallbackFixture(t *testing.T) (*e2eAuthStack, *httptest.Server, func()) {
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

// TestAuthLogin_RedirectsToIdP_WithStateCookie はシナリオ (a) 対応。
func TestAuthLogin_RedirectsToIdP_WithStateCookie(t *testing.T) {
	_, ts, cleanup := setupCallbackFixture(t)
	defer cleanup()

	client := noRedirectClient()
	resp, err := client.Get(ts.URL + "/api/auth/login?return_to=/dashboard")
	if err != nil {
		t.Fatalf("GET /api/auth/login: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d; want 302", resp.StatusCode)
	}
	location := resp.Header.Get("Location")
	if location == "" {
		t.Fatalf("Location header 不在")
	}
	// IdP 認可エンドポイント（mock IdP の /auth）に向いている
	if !strings.Contains(location, "/realms/e2e/auth") {
		t.Errorf("Location が IdP 認可エンドポイントに向いていない: %q", location)
	}
	// state クエリ + nonce クエリが付与されている
	if extractQuery(t, location, "state") == "" {
		t.Errorf("Location に state クエリが無い")
	}
	if extractQuery(t, location, "nonce") == "" {
		t.Errorf("Location に nonce クエリが無い")
	}
	stateCookie := findCookie(resp.Cookies(), "__Host-ae_mdm_state")
	if stateCookie == nil {
		t.Fatalf("__Host-ae_mdm_state cookie が発行されていない")
	}
	if stateCookie.Value == "" {
		t.Errorf("state cookie value が空文字")
	}
	if stateCookie.MaxAge <= 0 {
		t.Errorf("state cookie MaxAge = %d; want > 0", stateCookie.MaxAge)
	}
}

// TestAuthCallback_HappyPath_CreatesSessionAndRedirects はシナリオ (b) 対応。
func TestAuthCallback_HappyPath_CreatesSessionAndRedirects(t *testing.T) {
	stack, ts, cleanup := setupCallbackFixture(t)
	defer cleanup()

	// admin_users を事前 provisioning
	tenantID := seedTenant(t, context.Background(), stack.pool, "Tenant-Callback")
	adminUserID := seedAdminUser(t, context.Background(), stack.pool, stack.idp.issuer, "sub-callback", "before@example.com", tenantID, []string{"TenantAdmin"})

	client := noRedirectClient()

	// (1) /api/auth/login で state cookie + redirect URL を取得
	loginResp, err := client.Get(ts.URL + "/api/auth/login?return_to=/dashboard")
	if err != nil {
		t.Fatalf("GET /api/auth/login: %v", err)
	}
	_ = loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusFound {
		t.Fatalf("login status = %d; want 302", loginResp.StatusCode)
	}
	stateCookie := findCookie(loginResp.Cookies(), "__Host-ae_mdm_state")
	if stateCookie == nil {
		t.Fatalf("state cookie 不在")
	}
	state := extractQuery(t, loginResp.Header.Get("Location"), "state")
	nonce := extractQuery(t, loginResp.Header.Get("Location"), "nonce")

	// (2) mock IdP に「次の token endpoint」が返すべき id_token を設定
	stack.idp.setNextToken(e2eTokenResponse{
		Subject: "sub-callback",
		Aud:     authE2ETenantClientID,
		Email:   "after@example.com",
		Nonce:   nonce,
	})

	// (3) /api/auth/callback?code=...&state=... を state cookie 付きで送信
	req := mustNewRequest(t, http.MethodGet, ts.URL+"/api/auth/callback?code=test-code&state="+url.QueryEscape(state))
	req.AddCookie(stateCookie)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /api/auth/callback: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("callback status = %d; want 302 (body=%s)", resp.StatusCode, string(body))
	}
	if loc := resp.Header.Get("Location"); loc != "/dashboard" {
		t.Errorf("Location = %q; want /dashboard", loc)
	}
	// session cookie が生 token として発行されている（hash ではない）
	sessionCookie := findCookie(resp.Cookies(), "__Host-ae_mdm_session")
	if sessionCookie == nil {
		t.Fatalf("session cookie 不在")
	}
	if sessionCookie.Value == "" {
		t.Errorf("session cookie value 空文字")
	}
	if sessionCookie.MaxAge <= 0 {
		t.Errorf("session cookie MaxAge = %d; want > 0", sessionCookie.MaxAge)
	}
	// state cookie が削除されている（MaxAge < 0）
	stateExpire := findCookie(resp.Cookies(), "__Host-ae_mdm_state")
	if stateExpire == nil {
		t.Fatalf("state cookie 削除属性が発行されていない")
	}
	if stateExpire.MaxAge >= 0 {
		t.Errorf("state cookie MaxAge = %d; want < 0 (削除)", stateExpire.MaxAge)
	}

	// DB 上に sessions 行が 1 件存在し、token_hash が cookie 生値の SHA-256 hex と一致する
	expectedHash := auth.HashToken(sessionCookie.Value)
	if rows := countSessionsByHash(t, context.Background(), stack.pool, expectedHash); rows != 1 {
		t.Errorf("sessions 行数 (token_hash=%s) = %d; want 1", expectedHash, rows)
	}
	// 永続ストアには **生 token を保持しない**: sessions テーブル token_hash 列に生値そのものが
	// 一致する行が無いことを確認する（NFR 1.2 の回帰耐性）
	if rows := countSessionsByHash(t, context.Background(), stack.pool, sessionCookie.Value); rows != 0 {
		t.Errorf("DB に session 生値そのものが含まれている: rows=%d (NFR 1.2 違反)", rows)
	}
	// admin_users.email が IdP 側値（after@example.com）で UPDATE されている
	if got := fetchAdminUserEmail(t, context.Background(), stack.pool, adminUserID); got != "after@example.com" {
		t.Errorf("admin_users.email = %q; want after@example.com", got)
	}
}

// TestAuthCallback_StateInvalid_Returns401AndLogsFailureKind はシナリオ (c) 対応。
func TestAuthCallback_StateInvalid_Returns401AndLogsFailureKind(t *testing.T) {
	stack, ts, cleanup := setupCallbackFixture(t)
	defer cleanup()

	tenantID := seedTenant(t, context.Background(), stack.pool, "Tenant-StateInvalid")
	_ = seedAdminUser(t, context.Background(), stack.pool, stack.idp.issuer, "sub-si", "x@example.com", tenantID, []string{"TenantAdmin"})

	client := noRedirectClient()
	loginResp, err := client.Get(ts.URL + "/api/auth/login")
	if err != nil {
		t.Fatalf("GET /api/auth/login: %v", err)
	}
	_ = loginResp.Body.Close()
	stateCookie := findCookie(loginResp.Cookies(), "__Host-ae_mdm_state")
	if stateCookie == nil {
		t.Fatalf("state cookie 不在")
	}
	state := extractQuery(t, loginResp.Header.Get("Location"), "state")

	// state cookie 値を tamper（先頭文字を別文字に書き換える）
	tampered := flipFirstChar(stateCookie.Value)
	tamperedCookie := &http.Cookie{Name: stateCookie.Name, Value: tampered}

	req := mustNewRequest(t, http.MethodGet, ts.URL+"/api/auth/callback?code=test-code&state="+url.QueryEscape(state))
	req.AddCookie(tamperedCookie)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401", resp.StatusCode)
	}
	if !stack.log.hasFailureKind("state_invalid") {
		t.Errorf("log に failure_kind=state_invalid が記録されていない: %+v", stack.log.entries)
	}
}

// TestAuthCallback_InvalidAud_Returns401AndLogsFailureKind はシナリオ (d) 対応。
//
// id_token の aud が tenant / admin のいずれにも一致しない場合、Verifier 経路で
// `invalid_aud` が立つ。
func TestAuthCallback_InvalidAud_Returns401AndLogsFailureKind(t *testing.T) {
	stack, ts, cleanup := setupCallbackFixture(t)
	defer cleanup()

	tenantID := seedTenant(t, context.Background(), stack.pool, "Tenant-Aud")
	_ = seedAdminUser(t, context.Background(), stack.pool, stack.idp.issuer, "sub-aud", "x@example.com", tenantID, []string{"TenantAdmin"})

	client := noRedirectClient()
	loginResp, err := client.Get(ts.URL + "/api/auth/login")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	_ = loginResp.Body.Close()
	stateCookie := findCookie(loginResp.Cookies(), "__Host-ae_mdm_state")
	state := extractQuery(t, loginResp.Header.Get("Location"), "state")
	nonce := extractQuery(t, loginResp.Header.Get("Location"), "nonce")

	// id_token の aud を不正な値に設定（tenant / admin client_id のいずれでもない）
	stack.idp.setNextToken(e2eTokenResponse{
		Subject: "sub-aud",
		Aud:     "wrong-aud-not-a-real-client",
		Email:   "x@example.com",
		Nonce:   nonce,
	})

	req := mustNewRequest(t, http.MethodGet, ts.URL+"/api/auth/callback?code=test-code&state="+url.QueryEscape(state))
	req.AddCookie(stateCookie)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401", resp.StatusCode)
	}
	if !stack.log.hasFailureKind("invalid_aud") {
		t.Errorf("log に failure_kind=invalid_aud が記録されていない: %+v", stack.log.entries)
	}
}

// TestAuthCallback_AdminUserNotProvisioned_Returns403 はシナリオ (e) 対応。
func TestAuthCallback_AdminUserNotProvisioned_Returns403(t *testing.T) {
	stack, ts, cleanup := setupCallbackFixture(t)
	defer cleanup()

	// admin_users は seed しない（事前 provisioning なし）

	client := noRedirectClient()
	loginResp, err := client.Get(ts.URL + "/api/auth/login")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	_ = loginResp.Body.Close()
	stateCookie := findCookie(loginResp.Cookies(), "__Host-ae_mdm_state")
	state := extractQuery(t, loginResp.Header.Get("Location"), "state")
	nonce := extractQuery(t, loginResp.Header.Get("Location"), "nonce")

	stack.idp.setNextToken(e2eTokenResponse{
		Subject: "sub-not-provisioned",
		Aud:     authE2ETenantClientID,
		Email:   "unknown@example.com",
		Nonce:   nonce,
	})

	req := mustNewRequest(t, http.MethodGet, ts.URL+"/api/auth/callback?code=test-code&state="+url.QueryEscape(state))
	req.AddCookie(stateCookie)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d; want 403", resp.StatusCode)
	}
	if !stack.log.hasFailureKind("admin_user_not_provisioned") {
		t.Errorf("log に failure_kind=admin_user_not_provisioned が記録されていない: %+v", stack.log.entries)
	}
	// sessions テーブルに行が増えていないこと
	if rows := countSessions(t, context.Background(), stack.pool); rows != 0 {
		t.Errorf("sessions 行数 = %d; want 0 (provisioning 失敗で session 作らない)", rows)
	}
}

// TestAuthCallback_StateReplay_SecondAttemptReturns401AndDeletesStateCookie はシナリオ (f) 対応。
//
// 同一 (state cookie, query state, code) を 2 度提示し、2 回目が state_replay で 401 になることを
// 確認する（state_nonces テーブルの PRIMARY KEY UNIQUE 制約に依拠 / Req 2.9 の物理的拒否）。
func TestAuthCallback_StateReplay_SecondAttemptReturns401AndDeletesStateCookie(t *testing.T) {
	stack, ts, cleanup := setupCallbackFixture(t)
	defer cleanup()

	tenantID := seedTenant(t, context.Background(), stack.pool, "Tenant-Replay")
	_ = seedAdminUser(t, context.Background(), stack.pool, stack.idp.issuer, "sub-replay", "replay@example.com", tenantID, []string{"TenantAdmin"})

	client := noRedirectClient()
	loginResp, err := client.Get(ts.URL + "/api/auth/login")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	_ = loginResp.Body.Close()
	stateCookie := findCookie(loginResp.Cookies(), "__Host-ae_mdm_state")
	state := extractQuery(t, loginResp.Header.Get("Location"), "state")
	nonce := extractQuery(t, loginResp.Header.Get("Location"), "nonce")

	// 1 回目: 正常 callback（session 作成）
	stack.idp.setNextToken(e2eTokenResponse{
		Subject: "sub-replay",
		Aud:     authE2ETenantClientID,
		Email:   "replay@example.com",
		Nonce:   nonce,
	})
	req1 := mustNewRequest(t, http.MethodGet, ts.URL+"/api/auth/callback?code=code-1&state="+url.QueryEscape(state))
	req1.AddCookie(stateCookie)
	resp1, err := client.Do(req1)
	if err != nil {
		t.Fatalf("callback #1: %v", err)
	}
	_ = resp1.Body.Close()
	if resp1.StatusCode != http.StatusFound {
		t.Fatalf("callback #1 status = %d; want 302", resp1.StatusCode)
	}
	beforeReplay := countSessions(t, context.Background(), stack.pool)

	// 2 回目: 同じ state cookie + query state を再提示 → state_replay (401) + state cookie 削除
	stack.idp.setNextToken(e2eTokenResponse{
		Subject: "sub-replay",
		Aud:     authE2ETenantClientID,
		Email:   "replay@example.com",
		Nonce:   nonce,
	})
	req2 := mustNewRequest(t, http.MethodGet, ts.URL+"/api/auth/callback?code=code-2&state="+url.QueryEscape(state))
	req2.AddCookie(stateCookie)
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatalf("callback #2: %v", err)
	}
	defer func() { _ = resp2.Body.Close() }()

	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("callback #2 status = %d; want 401", resp2.StatusCode)
	}
	if !stack.log.hasFailureKind("state_replay") {
		t.Errorf("log に failure_kind=state_replay が記録されていない: %+v", stack.log.entries)
	}
	// state cookie 削除属性が発行されている
	expireCookie := findCookie(resp2.Cookies(), "__Host-ae_mdm_state")
	if expireCookie == nil {
		t.Fatalf("state cookie 削除属性が発行されていない")
	}
	if expireCookie.MaxAge >= 0 {
		t.Errorf("state cookie MaxAge = %d; want < 0 (削除)", expireCookie.MaxAge)
	}
	// 2 回目で sessions に行が増えていない
	afterReplay := countSessions(t, context.Background(), stack.pool)
	if afterReplay != beforeReplay {
		t.Errorf("sessions 行数: replay 前後で増加 (%d → %d); want 不変 (1回目のみ作成)", beforeReplay, afterReplay)
	}
}

// flipFirstChar は文字列先頭 1 文字を別文字に書き換える（MAC tamper 用）。
func flipFirstChar(s string) string {
	if s == "" {
		return ""
	}
	c := s[0]
	var newC byte
	if c == 'A' {
		newC = 'B'
	} else {
		newC = 'A'
	}
	return string(newC) + s[1:]
}

// mustNewRequest は http.NewRequest の失敗時 t.Fatalf に倒す helper。
func mustNewRequest(t *testing.T, method, target string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, target, nil)
	if err != nil {
		t.Fatalf("http.NewRequest: %v", err)
	}
	return req
}

// countSessionsByHash は token_hash 一致 sessions 行数を SuperAdmin 文脈で取得する。
func countSessionsByHash(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tokenHash string) int {
	t.Helper()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	setSuperAdmin(t, ctx, tx)
	var n int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM sessions WHERE token_hash = $1`, tokenHash).Scan(&n); err != nil {
		t.Fatalf("count sessions by hash: %v", err)
	}
	return n
}

// countSessions は sessions 全行数を SuperAdmin 文脈で取得する。
func countSessions(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	setSuperAdmin(t, ctx, tx)
	var n int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	return n
}

// setSuperAdmin は tx で SuperAdmin GUC を設定する helper。
func setSuperAdmin(t *testing.T, ctx context.Context, tx pgx.Tx) {
	t.Helper()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', '00000000-0000-0000-0000-000000000000', true)"); err != nil {
		t.Fatalf("set_config app.tenant_id: %v", err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.is_superadmin', 'true', true)"); err != nil {
		t.Fatalf("set_config app.is_superadmin: %v", err)
	}
}
