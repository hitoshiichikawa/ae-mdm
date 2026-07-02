package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/httpserver"
	"github.com/hitoshiichikawa/ae-mdm/internal/tenant"
)

// 本ファイルは tasks.md task 7（`/api/admin` 認可ガード継承の結合テスト / Req 6.2 / 6.3 / 6.4）を担う。
//
// tenant.Handler.Mount で `/api/admin/tenants` 配下 5 endpoint を `Routers.Admin`（admin chain
// 適用済みサブルータ）へ載せたときに、#37 の `/api/admin` ガード（TenantContextMiddleware →
// RequireAdminConsoleAndSuperAdmin）が tenant エンドポイントにも一様に継承されることを、
// server.go の NewServer + Handler.Mount を実物で組み立てて検証する。
//
// 認可は HTTP middleware chain（auth スタブ default deny → admin-console aud + SuperAdmin の
// 2 条件 AND ガード）の責務であり、context value（AuthClaims）は HTTP 境界を越えられないため、
// `httptest.NewServer` + `http.Get` ではなく `httptest.NewRecorder()` + `srv.Handler.ServeHTTP`
// + `req.WithContext(httpserver.WithAuthClaims(...))` で claims を直接注入する
// （既存 http_subrouter_mount_test.go test (j) と同方式）。本テストは DB 不要で無条件に走る
// （NewServer の pool=nil + fake Service が domain を担う）。

// ---- fake Service（integration_test パッケージ用の tenant.Service テストダブル） ----

// fakeTenantService は tenant.Service interface（#39 の逆引き追加で 7 メソッド）を満たす最小の
// テストダブル。ガード通過後に Handler へ到達したか（List 呼出有無）を記録し、ガード拒否ケースでは
// Service が一切呼ばれないこと（Req 6.5 の存在露出防止 / 認可は guard 層で完結）を併せて検証できる。
type fakeTenantService struct {
	mu       sync.Mutex
	listCall int
}

func (f *fakeTenantService) Create(_ context.Context, _ uuid.UUID, _ tenant.CreateInput) (tenant.TenantView, tenant.SignupURL, error) {
	return tenant.TenantView{}, tenant.SignupURL{}, nil
}

func (f *fakeTenantService) Bind(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ tenant.BindInput) (tenant.TenantView, error) {
	return tenant.TenantView{}, nil
}

func (f *fakeTenantService) Disable(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ tenant.DisableInput) (tenant.TenantView, error) {
	return tenant.TenantView{}, nil
}

func (f *fakeTenantService) Get(_ context.Context, _ uuid.UUID) (tenant.TenantView, error) {
	return tenant.TenantView{}, nil
}

func (f *fakeTenantService) List(_ context.Context) ([]tenant.TenantView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCall++
	// Req 4.4: 0 件でも非 nil の空 slice を返す（Handler が 200 + `[]` を返すことの確認用）。
	return []tenant.TenantView{}, nil
}

func (f *fakeTenantService) EnterpriseNameForTenant(_ context.Context, _ uuid.UUID) (string, error) {
	return "", nil
}

// TenantIDByEnterpriseName は tenant.Service 契約（#39 で追加された逆引きメソッド）を満たすための
// テストダブル実装。本ガードテストは逆引きを駆動しないため未割当（found=false）を返す。
func (f *fakeTenantService) TenantIDByEnterpriseName(_ context.Context, _ string) (uuid.UUID, bool, error) {
	return uuid.Nil, false, nil
}

func (f *fakeTenantService) listCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.listCall
}

// 型 assertion: fakeTenantService が tenant.Service を満たすことを compile-time で確認する。
var _ tenant.Service = (*fakeTenantService)(nil)

// newTenantAdminGuardServer は NewServer（authMWAdmin=nil で admin chain を
// [TenantContextMiddleware, RequireAdminConsoleAndSuperAdmin] の 2 段だけにする）を構築し、
// tenant.Handler を `routers.Admin` へ Mount した HTTP handler と fake Service を返す。
//
// pool=nil / authMW*=nil / authMount=nil は既存 http_subrouter_mount_test.go の
// newIntegrationHTTPServer と同じ呼び方であり、本テストは DB を必要としない。
func newTenantAdminGuardServer(t *testing.T) (http.Handler, *fakeTenantService) {
	t.Helper()
	log, err := logger.NewLogger(config.Config{
		LogLevel: "error", LogFormat: "json", LogOutput: "stderr",
	})
	if err != nil {
		t.Fatalf("logger.NewLogger: %v", err)
	}
	srv, routers, err := httpserver.NewServer(
		config.Config{HTTPListenAddr: ":0"},
		log,
		nil, // pool: 本テストは DB 不要
		nil, // authMWTenant: 未配線（/api/admin は対象外）
		nil, // authMWAdmin: 未配線（admin chain を 2 段ガードのみにし、claims を直接注入する）
		nil, // authMount: /api/auth は Mount しない
	)
	if err != nil {
		t.Fatalf("httpserver.NewServer: %v", err)
	}
	fake := &fakeTenantService{}
	h := tenant.NewHandler(fake, log)
	h.Mount(routers.Admin) // /api/admin サブルータ配下に /tenants を登録 → 最終 path は /api/admin/tenants
	return srv.Handler, fake
}

// adminConsoleSuperAdminClaims は admin-console aud + SuperAdmin の正常 claims を返す。
func adminConsoleSuperAdminClaims() httpserver.AuthClaims {
	return httpserver.AuthClaims{
		TenantID:     uuid.Nil,
		AdminUserID:  uuid.New(),
		IsSuperAdmin: true,
		Console:      "admin-console",
	}
}

// ============================================================================
// Req 6.4: 認証済みセッション未確立（AuthClaims 不在）→ 401
// ============================================================================

// TestTenantAdminGuard_NoClaims_Returns401 は AuthClaims を注入しない（未認証）状態で
// `/api/admin/tenants` を叩いたとき、admin chain の前段 TenantContextMiddleware が default deny
// で 401 を返し、tenant.Handler（Service）に到達しないことを検証する（Req 6.4）。
func TestTenantAdminGuard_NoClaims_Returns401(t *testing.T) {
	// Arrange
	handler, fake := newTenantAdminGuardServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/tenants", nil)
	// claims は注入しない（未認証セッション）。

	// Act
	handler.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401（未認証は TenantContextMiddleware が default deny する）", rec.Code)
	}
	if got := fake.listCalls(); got != 0 {
		t.Errorf("ガード拒否時に Service.List が呼ばれた（calls=%d; want 0）", got)
	}
}

// ============================================================================
// Req 6.2: audience が admin-console 以外（tenant-console）→ 403
// ============================================================================

// TestTenantAdminGuard_TenantConsoleAudience_Returns403 は tenant-console aud で発行された
// SuperAdmin セッションを `/api/admin/tenants` に提示したとき、RequireAdminConsoleAndSuperAdmin が
// audience 不一致で 403 を返し、tenant.Handler に到達しないことを検証する（Req 6.2）。
func TestTenantAdminGuard_TenantConsoleAudience_Returns403(t *testing.T) {
	// Arrange
	handler, fake := newTenantAdminGuardServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/tenants", nil)
	// tenant-console aud + SuperAdmin（audience だけが不一致）。
	req = req.WithContext(httpserver.WithAuthClaims(req.Context(), httpserver.AuthClaims{
		AdminUserID:  uuid.New(),
		IsSuperAdmin: true,
		Console:      "tenant-console",
	}))

	// Act
	handler.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403（tenant-console aud は audience 不一致で拒否）", rec.Code)
	}
	if got := fake.listCalls(); got != 0 {
		t.Errorf("ガード拒否時に Service.List が呼ばれた（calls=%d; want 0）", got)
	}
}

// ============================================================================
// Req 6.3: role が SuperAdmin 以外（admin-console + 非 SuperAdmin）→ 403
// ============================================================================

// TestTenantAdminGuard_NonSuperAdmin_Returns403 は admin-console aud だが SuperAdmin でない
// セッションを提示したとき、RequireAdminConsoleAndSuperAdmin が super_admin_not_present で 403 を
// 返し、tenant.Handler に到達しないことを検証する（Req 6.3）。
func TestTenantAdminGuard_NonSuperAdmin_Returns403(t *testing.T) {
	// Arrange
	handler, fake := newTenantAdminGuardServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/tenants", nil)
	// admin-console aud + 非 SuperAdmin（role だけが不一致）。
	req = req.WithContext(httpserver.WithAuthClaims(req.Context(), httpserver.AuthClaims{
		AdminUserID:  uuid.New(),
		IsSuperAdmin: false,
		Console:      "admin-console",
	}))

	// Act
	handler.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403（非 SuperAdmin は super_admin_not_present で拒否）", rec.Code)
	}
	if got := fake.listCalls(); got != 0 {
		t.Errorf("ガード拒否時に Service.List が呼ばれた（calls=%d; want 0）", got)
	}
}

// ============================================================================
// 正常系（ガード通過）: admin-console aud + SuperAdmin → 200（fake Service 到達）
// ============================================================================

// TestTenantAdminGuard_AdminConsoleSuperAdmin_Returns200 は admin-console aud + SuperAdmin の
// 正常 claims で `/api/admin/tenants` を叩いたとき、2 段ガードを通過して tenant.Handler.list が
// fake Service.List を呼び 200 を返すことを検証する（Req 6.2 / 6.3 / 6.4 の正常側ガード通過）。
func TestTenantAdminGuard_AdminConsoleSuperAdmin_Returns200(t *testing.T) {
	// Arrange
	handler, fake := newTenantAdminGuardServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/tenants", nil)
	req = req.WithContext(httpserver.WithAuthClaims(req.Context(), adminConsoleSuperAdminClaims()))

	// Act
	handler.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200（admin-console SuperAdmin はガードを通過し Handler に到達）", rec.Code)
	}
	if got := fake.listCalls(); got != 1 {
		t.Errorf("ガード通過後に Service.List が到達していない（calls=%d; want 1）", got)
	}
}

// ============================================================================
// ガード継承の一様性: 別 endpoint（POST /tenants = create）でも 401/403/200 が継承されること
// ============================================================================

// TestTenantAdminGuard_AppliesUniformlyAcrossEndpoints は、ガードが GET /tenants（list）だけでなく
// POST /tenants（create）にも一様に継承されることを subtests で確認する。Mount された全 endpoint が
// 同一 admin chain 配下に入るため、authz 拒否（401/403）と通過（2xx）は endpoint 非依存で一様であるべき。
func TestTenantAdminGuard_AppliesUniformlyAcrossEndpoints(t *testing.T) {
	cases := []struct {
		name     string
		claims   *httpserver.AuthClaims
		wantCode int
	}{
		{
			name:     "未認証は 401",
			claims:   nil,
			wantCode: http.StatusUnauthorized,
		},
		{
			name:     "tenant-console aud は 403",
			claims:   &httpserver.AuthClaims{AdminUserID: uuid.New(), IsSuperAdmin: true, Console: "tenant-console"},
			wantCode: http.StatusForbidden,
		},
		{
			name:     "admin-console 非 SuperAdmin は 403",
			claims:   &httpserver.AuthClaims{AdminUserID: uuid.New(), IsSuperAdmin: false, Console: "admin-console"},
			wantCode: http.StatusForbidden,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			// Arrange: POST /tenants（create）へガードが継承されるかを確認する。
			handler, _ := newTenantAdminGuardServer(t)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/admin/tenants", nil)
			if tc.claims != nil {
				req = req.WithContext(httpserver.WithAuthClaims(req.Context(), *tc.claims))
			}

			// Act
			handler.ServeHTTP(rec, req)

			// Assert: 拒否系は body decode に到達する前にガードで止まるため、endpoint 非依存で一様。
			if rec.Code != tc.wantCode {
				t.Errorf("POST /api/admin/tenants status = %d; want %d", rec.Code, tc.wantCode)
			}
		})
	}
}
