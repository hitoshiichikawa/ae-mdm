package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
)

// newServer は test helper。テスト用 logger と nil pool で *http.Server / Routers を返す。
// pool=nil の場合 /readyz は 503 を返す（テスト (c) の readyz 認証なし検証目的で 503 で OK）。
func newServer(t *testing.T) (*http.Server, Routers) {
	t.Helper()
	srv, routers, err := NewServer(
		config.Config{HTTPListenAddr: ":0"},
		newTestLogger(t),
		nil,
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
// `/api/admin/*` に到達した場合、TenantContextMiddleware は通過し RequireSuperAdmin が
// 403 を返すことを確認する。
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
	)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if srv.Addr != ":12345" {
		t.Errorf("Addr = %q; want %q", srv.Addr, ":12345")
	}
}
