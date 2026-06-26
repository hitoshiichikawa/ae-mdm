package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hitoshiichikawa/ae-mdm/internal/auth"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/oidc"
)

// 本ファイルは Issue #33 task 6.4 の logout / revoke 結合テスト。
//
// シナリオ（tasks.md task 6.4 L744〜L746）:
//   - Create Session → POST /api/auth/logout → 同 cookie 再提示で 401（Req 5.1 / 5.3）
//   - 改竄 cookie（hash 不一致）でも 401（Req 5.4）

// setupLogoutFixture は test 用 DB + mock IdP + auth stack + HTTP server を構築する。
func setupLogoutFixture(t *testing.T) (*e2eAuthStack, *httptest.Server, func()) {
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

// TestAuthLogout_RevokesSession_AndRePresentationReturns401 は logout 主シナリオ対応。
//
// (1) Create Session を seed
// (2) POST /api/auth/logout で 204 + session cookie 削除
// (3) 同じ cookie を /api/probe に提示すると 401 + failure_kind=session_revoked
func TestAuthLogout_RevokesSession_AndRePresentationReturns401(t *testing.T) {
	stack, ts, cleanup := setupLogoutFixture(t)
	defer cleanup()

	ctx := context.Background()
	baseTime := time.Now().UTC().Truncate(time.Microsecond)
	stack.clock.Set(baseTime)
	rawToken, hash, _, _ := seedActiveSession(t, ctx, stack.pool, stack.idp.issuer, oidc.ConsoleTenant, baseTime.Add(-1*time.Minute), baseTime.Add(7*time.Hour))

	// (1) POST /api/auth/logout: 204 + session cookie 削除
	client := noRedirectClient()
	logoutReq := mustNewRequest(t, http.MethodPost, ts.URL+"/api/auth/logout")
	logoutReq.AddCookie(&http.Cookie{Name: "__Host-ae_mdm_session", Value: rawToken})
	logoutResp, err := client.Do(logoutReq)
	if err != nil {
		t.Fatalf("POST /api/auth/logout: %v", err)
	}
	_ = logoutResp.Body.Close()
	if logoutResp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d; want 204", logoutResp.StatusCode)
	}
	expireCookie := findCookie(logoutResp.Cookies(), "__Host-ae_mdm_session")
	if expireCookie == nil {
		t.Fatalf("logout 後に session cookie 削除属性が発行されていない")
	}
	if expireCookie.MaxAge >= 0 {
		t.Errorf("session cookie MaxAge = %d; want < 0 (削除)", expireCookie.MaxAge)
	}
	// DB 上で revoked_at が立っている
	if !sessionIsRevoked(t, ctx, stack.pool, hash) {
		t.Errorf("logout 後に DB の revoked_at が NULL のまま")
	}

	// (2) 同じ cookie を probe に再提示 → 401 + failure_kind=session_revoked
	stack.clock.Set(baseTime.Add(1 * time.Second))
	reReq := mustNewRequest(t, http.MethodGet, ts.URL+"/api/probe")
	reReq.AddCookie(&http.Cookie{Name: "__Host-ae_mdm_session", Value: rawToken})
	reResp, err := http.DefaultClient.Do(reReq)
	if err != nil {
		t.Fatalf("GET /api/probe after logout: %v", err)
	}
	defer func() { _ = reResp.Body.Close() }()
	if reResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("re-presentation status = %d; want 401", reResp.StatusCode)
	}
	if !stack.log.hasFailureKind("session_revoked") {
		t.Errorf("log に failure_kind=session_revoked が記録されていない: %+v", stack.log.entries)
	}
}

// TestAuthLookup_TamperedCookie_Returns401SessionTamper は cookie 改竄シナリオ対応（Req 5.4）。
//
// 改竄 cookie（DB 上に該当 hash の無い値）を提示すると 401 + failure_kind=session_tamper。
// 改竄 cookie の提示は無関係の既存 session の状態に影響しない（lookup 失敗のため）。
func TestAuthLookup_TamperedCookie_Returns401SessionTamper(t *testing.T) {
	stack, ts, cleanup := setupLogoutFixture(t)
	defer cleanup()

	ctx := context.Background()
	baseTime := time.Now().UTC().Truncate(time.Microsecond)
	stack.clock.Set(baseTime)
	rawToken, _, _, _ := seedActiveSession(t, ctx, stack.pool, stack.idp.issuer, oidc.ConsoleTenant, baseTime.Add(-1*time.Minute), baseTime.Add(7*time.Hour))
	originalHash := auth.HashToken(rawToken)
	// 改竄 cookie: 先頭文字を変更（DB 上に該当する hash が無い）
	tampered := flipFirstChar(rawToken)

	req := mustNewRequest(t, http.MethodGet, ts.URL+"/api/probe")
	req.AddCookie(&http.Cookie{Name: "__Host-ae_mdm_session", Value: tampered})

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/probe: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401", resp.StatusCode)
	}
	if !stack.log.hasFailureKind("session_tamper") {
		t.Errorf("log に failure_kind=session_tamper が記録されていない: %+v", stack.log.entries)
	}
	// 改竄 cookie の提示で、無関係の既存 session が revoke されていない
	if sessionIsRevoked(t, ctx, stack.pool, originalHash) {
		t.Errorf("改竄 cookie の提示で、無関係の既存 session が revoke されてしまった")
	}
}
