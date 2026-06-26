package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	internalerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/oidc"
)

// newServer は test helper。テスト用 logger と nil pool で *http.Server / Routers を返す。
// pool=nil の場合 /readyz は 503 を返す（テスト (c) の readyz 認証なし検証目的で 503 で OK）。
//
// auth middleware / Mount 引数は nil で渡し、A2 既存挙動（default deny 401）を維持する
// （task 6.2: 既存テストの後方互換のための nil 許容契約）。
func newServer(t *testing.T) (*http.Server, Routers) {
	t.Helper()
	srv, routers, err := NewServer(
		config.Config{HTTPListenAddr: ":0"},
		newTestLogger(t),
		nil,
		nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv, routers
}

// TestServer_Healthz_RespondsWithoutAuth はテスト (c) 対応。`/healthz` が認証なしで
// 200 を返すこと（middleware chain の外側に登録されている / design.md L706）。
func TestServer_Healthz_RespondsWithoutAuth(t *testing.T) {
	// Arrange
	srv, _ := newServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)

	// Act
	srv.Handler.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz status = %d; want 200", rec.Code)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "ok" {
		t.Errorf("/healthz body = %q; want %q", body, "ok")
	}
}

// TestServer_Readyz_NoPool_Returns503 は readyz の pool=nil 防御経路。
// （pool=nil は cmd/api bootstrap 失敗の防御経路だが、テストでは 503 が返ることだけ確認）。
func TestServer_Readyz_NoPool_Returns503(t *testing.T) {
	// Arrange
	srv, _ := newServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)

	// Act
	srv.Handler.ServeHTTP(rec, req)

	// Assert: pool=nil で 503
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz status = %d; want 503", rec.Code)
	}
}

// TestServer_RequestIDHeader_OnEveryResponse はテスト (b) 対応。RequestID middleware が
// chain の root に挟まっているため、ヘルスチェック応答にも X-Request-ID が乗ることを確認する。
func TestServer_RequestIDHeader_OnEveryResponse(t *testing.T) {
	// Arrange
	srv, _ := newServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)

	// Act
	srv.Handler.ServeHTTP(rec, req)

	// Assert
	got := rec.Header().Get(requestIDHeader)
	if got == "" {
		t.Fatalf("X-Request-ID header が空")
	}
	if _, err := uuid.Parse(got); err != nil {
		t.Errorf("X-Request-ID は uuid v4 を期待; got %q (%v)", got, err)
	}
}

// TestServer_APIWithoutAuth_Returns401 はテスト (g) 対応。
// `/api/*` 配下に auth スタブ default deny（claims 未注入）で到達すると 401 になる。
func TestServer_APIWithoutAuth_Returns401(t *testing.T) {
	// Arrange
	srv, routers := newServer(t)
	// 後続 Issue で mount されることを模擬: テストでは何も mount しないが、TenantContextMiddleware
	// が先に 401 を返すため 404 ではなく 401 が返る。
	_ = routers
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/devices", nil)

	// Act
	srv.Handler.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/api/devices status = %d; want 401", rec.Code)
	}
}

// TestServer_AdminWithoutAuth_Returns401 はテスト (d) 対応。
// `/api/admin/*` に TenantContext 未確立で到達すると TenantContextMiddleware が
// 先に 401 を返す（RequireSuperAdmin の 403 経路ではなく 401 が優先される）。
func TestServer_AdminWithoutAuth_Returns401(t *testing.T) {
	// Arrange
	srv, _ := newServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/tenants", nil)

	// Act
	srv.Handler.ServeHTTP(rec, req)

	// Assert: TenantContextMiddleware が 401 を発火
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/api/admin/tenants status = %d; want 401", rec.Code)
	}
}

// TestServer_AdminWithTenantContextNotSuperAdmin_Returns403 はテスト (e) 対応。
// auth スタブ経由で TenantContext を put した状態（IsSuperAdmin=false）で
// `/api/admin/*` に到達した場合、TenantContextMiddleware は通過し
// RequireAdminConsoleAndSuperAdmin が 403 を返すことを確認する（Issue #37 / Req 2.5）。
//
// 本テストでは [WithAuthClaims] で claims を ctx に注入し、
// TenantContextMiddleware を通過させる経路を成立させる。
func TestServer_AdminWithTenantContextNotSuperAdmin_Returns403(t *testing.T) {
	// Arrange
	srv, _ := newServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/tenants", nil)
	req = req.WithContext(WithAuthClaims(context.Background(), AuthClaims{
		TenantID:     uuid.New(),
		AdminUserID:  uuid.New(),
		Roles:        []string{"TenantAdmin"},
		IsSuperAdmin: false,
		Console:      "admin-console", // Issue #37: admin chain の本ガードが Console も見るため明示
	}))

	// Act
	srv.Handler.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusForbidden {
		t.Fatalf("/api/admin/tenants status = %d; want 403", rec.Code)
	}
}

// TestServer_AdminWithSuperAdminTenantContext_ReachesMountedHandler はテスト (f) 対応。
// IsSuperAdmin=true で /api/admin 配下に mount された Handler に到達することを確認する。
// 後続 Issue が Routers.Admin に Mount するパターンを模擬する。
func TestServer_AdminWithSuperAdminTenantContext_ReachesMountedHandler(t *testing.T) {
	// Arrange
	srv, routers := newServer(t)
	reached := false
	routers.Admin.Get("/ping", func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("pong"))
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/ping", nil)
	req = req.WithContext(WithAuthClaims(context.Background(), AuthClaims{
		TenantID:     uuid.Nil,
		AdminUserID:  uuid.New(),
		Roles:        []string{"SuperAdmin"},
		IsSuperAdmin: true,
		Console:      "admin-console", // Issue #37: admin chain は audience も判定する
	}))

	// Act
	srv.Handler.ServeHTTP(rec, req)

	// Assert
	if !reached {
		t.Fatalf("SuperAdmin で next handler に到達すること")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "pong" {
		t.Errorf("body = %q; want %q", body, "pong")
	}
}

// TestServer_APIWithClaims_ReachesMountedHandler は `/api/*` 配下に Routers.API で
// mount された Handler に、claims 注入後に到達することを確認する補助テスト。
// （g のポジティブ側: 401 でなく next 通過することの確認）
func TestServer_APIWithClaims_ReachesMountedHandler(t *testing.T) {
	// Arrange
	srv, routers := newServer(t)
	tenantID := uuid.New()
	reached := false
	routers.API.Get("/whoami", func(w http.ResponseWriter, r *http.Request) {
		tc, err := db.FromContext(r.Context())
		if err != nil {
			t.Errorf("FromContext: %v", err)
			return
		}
		if tc.TenantID != tenantID {
			t.Errorf("TenantID = %v; want %v", tc.TenantID, tenantID)
		}
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/whoami", nil)
	req = req.WithContext(WithAuthClaims(context.Background(), AuthClaims{
		TenantID:     tenantID,
		AdminUserID:  uuid.New(),
		Roles:        []string{"TenantAdmin"},
		IsSuperAdmin: false,
	}))

	// Act
	srv.Handler.ServeHTTP(rec, req)

	// Assert
	if !reached {
		t.Fatalf("claims 注入で /api/whoami の handler に到達すること")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
}

// TestServer_NewServerReturnsSetReadHeaderTimeout は slowloris 対策の
// ReadHeaderTimeout が必ず設定されていることを確認する（コード品質の sanity check）。
func TestServer_NewServerReturnsSetReadHeaderTimeout(t *testing.T) {
	srv, _ := newServer(t)
	if srv.ReadHeaderTimeout <= 0 {
		t.Fatalf("ReadHeaderTimeout = %v; want positive", srv.ReadHeaderTimeout)
	}
}

// TestServer_NewServerReturnsListenAddr は cfg.HTTPListenAddr が server.Addr に
// 正しく転記されることを確認する。
func TestServer_NewServerReturnsListenAddr(t *testing.T) {
	srv, _, err := NewServer(
		config.Config{HTTPListenAddr: ":12345"},
		newTestLogger(t),
		nil,
		nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if srv.Addr != ":12345" {
		t.Errorf("Addr = %q; want %q", srv.Addr, ":12345")
	}
}

// newServerWithAuth は task 6.2 用 helper。任意の auth middleware / Mount を
// `NewServer` に注入した状態の *http.Server / Routers を返す。
func newServerWithAuth(
	t *testing.T,
	authMWTenant func(http.Handler) http.Handler,
	authMWAdmin func(http.Handler) http.Handler,
	authMount func(r chi.Router, consolePrefix string, console oidc.Console),
) (*http.Server, Routers) {
	t.Helper()
	srv, routers, err := NewServer(
		config.Config{HTTPListenAddr: ":0"},
		newTestLogger(t),
		nil,
		authMWTenant,
		authMWAdmin,
		authMount,
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv, routers
}

// fakeAuthMWTenantInjectClaims は task 6.2 テスト (a) 用 fake 関数。
// テスト用に AuthClaims を ctx に注入してから next.ServeHTTP に進める tenant 用 auth
// middleware を模擬する（実 auth.NewMiddleware の代わり / Service 不要で境界網羅可能）。
func fakeAuthMWTenantInjectClaims(claims AuthClaims) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := WithAuthClaims(r.Context(), claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// TestServer_AuthMWTenant_Wired_APIReachesHandlerWithTenantContext は task 6.2 テスト (a)。
// `authMWTenant` を NewServer に注入した場合、`/api/probe` への到達経路で:
//   - authMWTenant が AuthClaims を ctx に注入し
//   - 後続 TenantContextMiddleware が TenantContext を確立し
//   - probe handler が db.FromContext 経由で TenantID を取得できる
// ことを assertion する（Req 5.3 / 5.4 / 6.2 の chain 順序確認）。
func TestServer_AuthMWTenant_Wired_APIReachesHandlerWithTenantContext(t *testing.T) {
	// Arrange
	tenantID := uuid.New()
	adminUserID := uuid.New()
	claims := AuthClaims{
		TenantID:     tenantID,
		AdminUserID:  adminUserID,
		Roles:        []string{"TenantAdmin"},
		IsSuperAdmin: false,
	}
	srv, routers := newServerWithAuth(t, fakeAuthMWTenantInjectClaims(claims), nil, nil)
	reached := false
	routers.API.Get("/probe", func(w http.ResponseWriter, r *http.Request) {
		tc, err := db.FromContext(r.Context())
		if err != nil {
			t.Errorf("db.FromContext: %v", err)
			return
		}
		if tc.TenantID != tenantID {
			t.Errorf("TenantID = %v; want %v", tc.TenantID, tenantID)
		}
		reached = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/probe", nil)

	// Act
	srv.Handler.ServeHTTP(rec, req)

	// Assert
	if !reached {
		t.Fatalf("authMWTenant 経由で probe handler に到達すること")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != "ok" {
		t.Errorf("body = %q; want %q", body, "ok")
	}
}

// fakeAuthMountLoginRedirect は task 6.2 テスト (b) 用 fake 関数。
// auth.Handler.Mount のシグネチャを模した関数で、`<prefix>/login` に GET すると 302 +
// Location header を返す stub を登録する（console 種別を Location header に乗せて
// authMount が 2 回 / ConsoleTenant + ConsoleAdmin で呼ばれた証跡を確認可能にする）。
func fakeAuthMountLoginRedirect(r chi.Router, prefix string, console oidc.Console) {
	r.Route(prefix, func(sub chi.Router) {
		sub.Get("/login", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "https://idp.example/auth?console="+string(console))
			w.WriteHeader(http.StatusFound)
		})
	})
}

// TestServer_AuthMount_Wired_AuthLoginReachable は task 6.2 テスト (b)。
// `authMount` を NewServer に注入した場合、`/api/auth/login` と `/api/admin/auth/login` の
// 双方が auth.Handler 相当の stub に到達し、それぞれ ConsoleTenant / ConsoleAdmin として
// Mount されていることを Location header 経由で確認する
// （Req 6.2 / 6.3 の cross-console mount 強制）。
func TestServer_AuthMount_Wired_AuthLoginReachable(t *testing.T) {
	// Arrange
	srv, _ := newServerWithAuth(t, nil, nil, fakeAuthMountLoginRedirect)

	t.Run("tenant /api/auth/login returns 302 with tenant console", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/auth/login", nil)
		// Act
		srv.Handler.ServeHTTP(rec, req)
		// Assert
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d; want 302", rec.Code)
		}
		got := rec.Header().Get("Location")
		if !strings.Contains(got, "console=tenant-console") {
			t.Errorf("Location = %q; want substring %q", got, "console=tenant-console")
		}
	})

	t.Run("admin /api/admin/auth/login returns 302 with admin console", func(t *testing.T) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/admin/auth/login", nil)
		// Act
		srv.Handler.ServeHTTP(rec, req)
		// Assert
		if rec.Code != http.StatusFound {
			t.Fatalf("status = %d; want 302", rec.Code)
		}
		got := rec.Header().Get("Location")
		if !strings.Contains(got, "console=admin-console") {
			t.Errorf("Location = %q; want substring %q", got, "console=admin-console")
		}
	})
}

// fakeAuthMWAdminRejectConsoleMismatch は task 6.2 テスト (c) 用 fake 関数。
// 漏洩した tenant 系 session が `/api/admin/...` に提示された場合に admin 用 auth
// middleware が `console_mismatch` で 401 + session cookie 削除を返す挙動を模擬する
// （実 NewMiddleware の `session_console_mismatch` 経路と等価な response shape を返す）。
func fakeAuthMWAdminRejectConsoleMismatch() func(http.Handler) http.Handler {
	return func(_ http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// session cookie 削除（Max-Age=0 で expire / RFC 6265 / NFR 4.1）
			http.SetCookie(w, &http.Cookie{
				Name:     "__Host-ae_mdm_session",
				Value:    "",
				Path:     "/",
				MaxAge:   -1,
				HttpOnly: true,
				Secure:   true,
				SameSite: http.SameSiteLaxMode,
			})
			internalerrors.WriteHTTP(w, r, internalerrors.New(
				internalerrors.CodeUnauthenticated,
				"console_mismatch",
			), nil)
		})
	}
}

// TestServer_AuthMWAdmin_Wired_CrossConsoleRejected は task 6.2 テスト (c)。
// `authMWAdmin` を NewServer に注入した場合、tenant 系 session cookie を `/api/admin/...` に
// 提示すると authMWAdmin 側で `console_mismatch` 検知 → 401 + session cookie 削除 (Max-Age=0)
// が返り、後段の TenantContextMiddleware / probe handler には到達しないことを assertion する
// （Req 6.2 / 6.3 の cross-console reject 物理分離強制）。
func TestServer_AuthMWAdmin_Wired_CrossConsoleRejected(t *testing.T) {
	// Arrange
	srv, routers := newServerWithAuth(t, nil, fakeAuthMWAdminRejectConsoleMismatch(), nil)
	reached := false
	routers.Admin.Get("/probe", func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/probe", nil)
	// tenant 系 session cookie を提示（authMWAdmin の console_mismatch を引き起こす入力）
	req.AddCookie(&http.Cookie{
		Name:  "__Host-ae_mdm_session",
		Value: "tenant-session-token-fixture", // 値自体は固定 fixture / 機密ではない
	})

	// Act
	srv.Handler.ServeHTTP(rec, req)

	// Assert: 401 + cookie 削除 + probe 未到達
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401", rec.Code)
	}
	if reached {
		t.Errorf("authMWAdmin 後段の probe handler に到達してはならない（cross-console reject）")
	}
	setCookie := rec.Header().Get("Set-Cookie")
	if !strings.Contains(setCookie, "__Host-ae_mdm_session=") {
		t.Errorf("Set-Cookie に session cookie 削除指示が無い; got %q", setCookie)
	}
	if !strings.Contains(setCookie, "Max-Age=0") {
		t.Errorf("Set-Cookie に Max-Age=0 が無い; got %q", setCookie)
	}
}
