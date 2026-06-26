package integration_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	platformdb "github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/httpserver"
)

// newIntegrationHTTPServer は httptest.NewServer 相当の流れで test 用 HTTP server を起動する。
// pool=nil で構築し、本テストは DB を必要としない（`/healthz` と middleware chain だけが
// 対象）。tasks.md 6.1 詳細項目の HTTP サブルータ mount テスト (g)/(h)/(i) を担う。
func newIntegrationHTTPServer(t *testing.T) (handler http.Handler) {
	t.Helper()
	log, err := logger.NewLogger(config.Config{
		LogLevel: "error", LogFormat: "json", LogOutput: "stderr",
	})
	if err != nil {
		t.Fatalf("logger.NewLogger: %v", err)
	}
	srv, _, err := httpserver.NewServer(
		config.Config{HTTPListenAddr: ":0"},
		log,
		nil, // pool は本テストでは未使用（/healthz は外側、/api/* は 401 で閉じる）
		nil, // authMWTenant: 本テストでは未配線（既存 default deny 401 を確認するため）
		nil, // authMWAdmin: 同上
		nil, // authMount: 本テストでは /api/auth を Mount しない（既存 404 経路を確認）
	)
	if err != nil {
		t.Fatalf("httpserver.NewServer: %v", err)
	}
	return srv.Handler
}

// TestHTTPSubrouterMount_HealthzReturns200 はテスト (g) 対応。
// `/healthz` は middleware chain の外側に登録されており、認証なしで 200 "ok" を返す
// （requirements.md Req 5.1 / 5.6 / design.md L706 と整合）。
func TestHTTPSubrouterMount_HealthzReturns200(t *testing.T) {
	// Arrange
	h := newIntegrationHTTPServer(t)
	ts := httptest.NewServer(h)
	defer ts.Close()

	// Act
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Assert
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz status = %d; want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Errorf("GET /healthz body = %q; want %q", string(body), "ok")
	}
}

// TestHTTPSubrouterMount_APIRequiresAuth_Returns401 はテスト (h) 対応。
// `/api/anything` には auth スタブ default deny が掛かっており、401 を返す
// （requirements.md Req 5.3 と TenantContextMiddleware の default deny 契約に整合）。
func TestHTTPSubrouterMount_APIRequiresAuth_Returns401(t *testing.T) {
	h := newIntegrationHTTPServer(t)
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/anything")
	if err != nil {
		t.Fatalf("GET /api/anything: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /api/anything status = %d; want 401", resp.StatusCode)
	}
}

// TestHTTPSubrouterMount_AdminRequiresAuth_Returns401Or403 はテスト (i) 対応。
// `/api/admin/anything` には SuperAdmin ガードが掛かっており、claims 未注入の default
// deny 状態では先に TenantContextMiddleware が 401 を返す（403 は IsSuperAdmin=false 時の経路）。
// 本テストでは 401 / 403 どちらでも合格とする（tasks.md 6.1 詳細項目 (i) に明記）。
func TestHTTPSubrouterMount_AdminRequiresAuth_Returns401Or403(t *testing.T) {
	h := newIntegrationHTTPServer(t)
	ts := httptest.NewServer(h)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/admin/anything")
	if err != nil {
		t.Fatalf("GET /api/admin/anything: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		t.Fatalf("GET /api/admin/anything status = %d; want 401 or 403", resp.StatusCode)
	}
}

// TestHTTPSubrouterMount_AdminReachesNextHandler_WhenSuperAdminContextInjected はテスト (j) 対応。
// 本テストは `RequireSuperAdmin` middleware の単体挙動を確認する目的で、auth スタブ
// （TenantContextMiddleware 経由の `httpserver.WithAuthClaims` injection）は bypass し、
// test 専用 router に SuperAdmin の TenantContext を直接埋め込んで next handler への
// 到達を検証する（RequireSuperAdmin の判定は TenantContext.IsSuperAdmin のみを参照する
// ため、auth chain 全体を組み立てなくても本 middleware 単体の境界条件を網羅できる）。
//
// （tasks.md 6.1 詳細項目「test 用 router で TenantContext を put して IsSuperAdmin=true の
// 場合に next handler まで到達することを確認」と整合）
func TestHTTPSubrouterMount_AdminReachesNextHandler_WhenSuperAdminContextInjected(t *testing.T) {
	// Arrange
	log, err := logger.NewLogger(config.Config{
		LogLevel: "error", LogFormat: "json", LogOutput: "stderr",
	})
	if err != nil {
		t.Fatalf("logger.NewLogger: %v", err)
	}
	r := chi.NewRouter()
	r.Use(httpserver.RequireSuperAdmin(log))
	reached := false
	r.Get("/probe", func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("reached"))
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	// SuperAdmin TenantContext を ctx に直接埋め込み（auth スタブを bypass）
	req = req.WithContext(platformdb.WithTenantContext(
		req.Context(),
		platformdb.TenantContext{TenantID: uuid.Nil, IsSuperAdmin: true},
	))

	// Act
	r.ServeHTTP(rec, req)

	// Assert
	if !reached {
		t.Fatalf("SuperAdmin context 付きで next handler に到達しなかった")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
}

// TestHTTPSubrouterMount_AdminReturns403_WhenNonSuperAdminContextInjected は (j) の負側補完。
// TenantContext を ctx に put したが IsSuperAdmin=false の場合は RequireSuperAdmin が 403 で
// 拒否することを確認する（Req 5.5: 対象リソースの存在を露出しない経路）。
func TestHTTPSubrouterMount_AdminReturns403_WhenNonSuperAdminContextInjected(t *testing.T) {
	// Arrange
	log, err := logger.NewLogger(config.Config{
		LogLevel: "error", LogFormat: "json", LogOutput: "stderr",
	})
	if err != nil {
		t.Fatalf("logger.NewLogger: %v", err)
	}
	r := chi.NewRouter()
	r.Use(httpserver.RequireSuperAdmin(log))
	reached := false
	r.Get("/probe", func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req = req.WithContext(platformdb.WithTenantContext(
		req.Context(),
		platformdb.TenantContext{TenantID: uuid.New(), IsSuperAdmin: false},
	))

	// Act
	r.ServeHTTP(rec, req)

	// Assert
	if reached {
		t.Fatalf("IsSuperAdmin=false で next handler に到達してしまった（403 で止まるべき）")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d; want 403", rec.Code)
	}
}
