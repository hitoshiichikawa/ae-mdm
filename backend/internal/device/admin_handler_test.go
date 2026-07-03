package device

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/authz"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/httpserver"
)

// ---- Test doubles ----

// fakeAdminService は AdminHandler テスト用の device.Service spy。Overview の呼び出し回数・
// 受信 ctx（SuperAdmin TenantContext の確立検証用）・tenantFilter を記録し、任意の戻り値 /
// エラーを返せる。List / Get は AdminHandler では使わないため stub（handler_test.go の
// fakeHandlerService とは別型 / Overview の ctx・filter 捕捉が必要なため独立させる）。
type fakeAdminService struct {
	overviewCalls  int
	overviewCtx    context.Context
	overviewFilter *uuid.UUID
	overviewOut    []TenantOverview
	overviewErr    error
}

func (s *fakeAdminService) List(_ context.Context, _ uuid.UUID, _ ListFilter) ([]DeviceSummary, error) {
	return nil, nil
}

func (s *fakeAdminService) Get(_ context.Context, _, _ uuid.UUID) (DeviceDetail, error) {
	return DeviceDetail{}, nil
}

func (s *fakeAdminService) Overview(ctx context.Context, tenantFilter *uuid.UUID) ([]TenantOverview, error) {
	s.overviewCalls++
	s.overviewCtx = ctx
	s.overviewFilter = tenantFilter
	return s.overviewOut, s.overviewErr
}

// newAdminClaims は admin-console / SuperAdmin の AuthClaims を作る helper。
//
// tenant-console 用の newTenantClaims（handler_test.go）と異なり、admin は
// TenantID = uuid.Nil（SuperAdmin の cross-tenant context）/ Console = admin-console /
// IsSuperAdmin = true を持つ（audit.newAdminClaims と整合）。
func newAdminClaims() httpserver.AuthClaims {
	return httpserver.AuthClaims{
		TenantID:          uuid.Nil,
		AdminUserID:       uuid.New(),
		Roles:             []string{"SuperAdmin"},
		IsSuperAdmin:      true,
		Console:           "admin-console",
		SessionHashPrefix: "abc12345",
	}
}

// doAdminRequest は AdminHandler を本番同様に親 chi router へ `Mount("/devices/overview", h)`
// した上で GET /devices/overview?<rawQuery> を実行する helper。
//
// cmd/api の `routers.Admin.Mount("/devices/overview", adminHandler)` 配線（実 path
// `/api/admin/devices/overview`）を忠実に再現し、chi の prefix strip 後に内包 router の
// root route（`/`）が成立する経路を検証する。claimsOrNil が nil なら claims を ctx に
// 注入しない（未認証経路の検証用）。
func doAdminRequest(t *testing.T, h *AdminHandler, rawQuery string, claimsOrNil *httpserver.AuthClaims) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	router.Mount("/devices/overview", h)

	rec := httptest.NewRecorder()
	target := "/devices/overview"
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if claimsOrNil != nil {
		req = req.WithContext(httpserver.WithAuthClaims(req.Context(), *claimsOrNil))
	}
	router.ServeHTTP(rec, req)
	return rec
}

// ---- (a) SuperAdmin / tenant_id 無しで 200 + SuperAdmin TenantContext 確立（Req 6.1） ----

func TestAdminHandler_Overview_SuperAdmin_NoTenantID_Returns200AndEstablishesSuperAdminContext(t *testing.T) {
	// Arrange
	tenantA := uuid.New()
	svc := &fakeAdminService{overviewOut: []TenantOverview{
		{TenantID: tenantA, DeviceCount: 3, Breakdown: newBreakdown()},
	}}
	log := &handlerFakeLogger{}
	h := NewAdminHandler(svc, authz.New(), log)
	claims := newAdminClaims()

	// Act（tenant_id 無し = 全テナント横断ビュー）
	rec := doAdminRequest(t, h, "", &claims)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 body=%q", rec.Code, rec.Body.String())
	}
	if svc.overviewCalls != 1 {
		t.Fatalf("svc.Overview 呼び出し回数 = %d; want 1", svc.overviewCalls)
	}
	// SuperAdmin TenantContext が確立されていること（RLS の is_superadmin 句で全テナント可視 / Req 6.1 / NFR 3.1）。
	tc, err := db.FromContext(svc.overviewCtx)
	if err != nil {
		t.Fatalf("svc.Overview に渡った ctx に TenantContext が確立されていない: %v", err)
	}
	if !tc.IsSuperAdmin {
		t.Errorf("TenantContext.IsSuperAdmin = false; want true（SuperAdmin 横断ビュー / Req 6.1）")
	}
	if tc.TenantID != uuid.Nil {
		t.Errorf("TenantContext.TenantID = %v; want uuid.Nil（SuperAdmin context）", tc.TenantID)
	}
	// tenant_id 無し = 全テナント横断ビューなので tenantFilter は nil（Req 6.1）。
	if svc.overviewFilter != nil {
		t.Errorf("tenant_id 無しなのに tenantFilter が設定されている: %v", svc.overviewFilter)
	}
}

// ---- (b) 非 SuperAdmin / tenant-console aud → 403（Req 6.2） ----
//
// RequireAdminConsoleAndSuperAdmin 固定ガードは unit では不在のため handler 本体まで到達する。
// handler 内の cross-tenant authz 二重防御（ResourceDevice read）が非 SuperAdmin を 403 で拒否
// することを検証する（audit.AdminHandler の probe-tenant 手本 / Req 6.2）。

func TestAdminHandler_Overview_NonSuperAdmin_Returns403(t *testing.T) {
	cases := []struct {
		name   string
		claims httpserver.AuthClaims
	}{
		{
			name: "admin-console aud だが SuperAdmin ロールなし",
			claims: httpserver.AuthClaims{
				TenantID:          uuid.Nil,
				AdminUserID:       uuid.New(),
				Roles:             []string{"TenantAdmin"},
				IsSuperAdmin:      false,
				Console:           "admin-console",
				SessionHashPrefix: "abc12345",
			},
		},
		{
			name: "tenant-console ユーザー（自テナント Viewer）",
			claims: httpserver.AuthClaims{
				TenantID:          uuid.New(),
				AdminUserID:       uuid.New(),
				Roles:             []string{"Viewer"},
				IsSuperAdmin:      false,
				Console:           "tenant-console",
				SessionHashPrefix: "abc12345",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			svc := &fakeAdminService{}
			log := &handlerFakeLogger{}
			h := NewAdminHandler(svc, authz.New(), log)

			// Act（tenant_id 無し = 全テナント横断ビュー / probe テナントで cross-tenant authz が gate する）
			rec := doAdminRequest(t, h, "", &tc.claims)

			// Assert: cross-tenant authz matrix の判定で 403（Req 6.2）。svc.Overview には到達しない。
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d; want 403（非 SuperAdmin は authz matrix が gate する / Req 6.2） body=%q", rec.Code, rec.Body.String())
			}
			if svc.overviewCalls != 0 {
				t.Errorf("403 で svc.Overview が呼ばれてはならない（呼び出し回数=%d）", svc.overviewCalls)
			}
			if code := decodeErrCode(t, rec); code != string(pkgerrors.CodeForbidden) {
				t.Errorf("body.Code = %q; want %q", code, pkgerrors.CodeForbidden)
			}
			// NFR 3.1: deny は構造化 WARN（deny_reason=authz denied）で記録される。
			if !log.hasWarnReason("authz denied") {
				t.Errorf("deny 経路で deny_reason=authz denied の WARN ログが無い; warnCalls=%+v", log.warnCalls)
			}
		})
	}
}

// ---- (c) claims 不在で 401（防御的 / Req 6.2 前段） ----

func TestAdminHandler_Overview_NoClaims_Returns401(t *testing.T) {
	// Arrange
	svc := &fakeAdminService{}
	log := &handlerFakeLogger{}
	h := NewAdminHandler(svc, authz.New(), log)

	// Act（claims を注入しない）
	rec := doAdminRequest(t, h, "", nil)

	// Assert
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401 body=%q", rec.Code, rec.Body.String())
	}
	if svc.overviewCalls != 0 {
		t.Errorf("401 で svc.Overview が呼ばれてはならない（呼び出し回数=%d）", svc.overviewCalls)
	}
	if code := decodeErrCode(t, rec); code != string(pkgerrors.CodeUnauthenticated) {
		t.Errorf("body.Code = %q; want %q", code, pkgerrors.CodeUnauthenticated)
	}
	if !log.hasWarnReason("missing auth claims") {
		t.Errorf("claims 不在経路で deny_reason=missing auth claims の WARN ログが無い; warnCalls=%+v", log.warnCalls)
	}
}

// ---- (d) tenant_id query で単一テナントに絞り込み → Service.Overview へ伝播（Req 6.3） ----

func TestAdminHandler_Overview_TenantIDQuery_NarrowsToTenant_Returns200(t *testing.T) {
	// Arrange
	wantTenant := uuid.New()
	svc := &fakeAdminService{overviewOut: []TenantOverview{
		{TenantID: wantTenant, DeviceCount: 1, Breakdown: newBreakdown()},
	}}
	h := NewAdminHandler(svc, authz.New(), &handlerFakeLogger{})
	claims := newAdminClaims()

	// Act
	rec := doAdminRequest(t, h, "tenant_id="+wantTenant.String(), &claims)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 body=%q", rec.Code, rec.Body.String())
	}
	if svc.overviewCalls != 1 {
		t.Fatalf("svc.Overview 呼び出し回数 = %d; want 1", svc.overviewCalls)
	}
	// tenant_id 絞り込みが Service.Overview へ伝播する（Req 6.3）。
	if svc.overviewFilter == nil {
		t.Fatalf("tenant_id 指定なのに tenantFilter が nil")
	}
	if *svc.overviewFilter != wantTenant {
		t.Errorf("tenantFilter = %v; want %v", *svc.overviewFilter, wantTenant)
	}
}

// ---- (e) 不正書式の tenant_id は 400（Req 6.3 の境界） ----

func TestAdminHandler_Overview_InvalidTenantID_Returns400(t *testing.T) {
	// Arrange
	svc := &fakeAdminService{}
	h := NewAdminHandler(svc, authz.New(), &handlerFakeLogger{})
	claims := newAdminClaims()

	// Act
	rec := doAdminRequest(t, h, "tenant_id=not-a-uuid", &claims)

	// Assert
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 body=%q", rec.Code, rec.Body.String())
	}
	if svc.overviewCalls != 0 {
		t.Errorf("不正 tenant_id で svc.Overview が呼ばれてはならない（呼び出し回数=%d）", svc.overviewCalls)
	}
	if code := decodeErrCode(t, rec); code != string(pkgerrors.CodeInvalidRequest) {
		t.Errorf("body.Code = %q; want %q", code, pkgerrors.CodeInvalidRequest)
	}
}

// ---- (f) 空集計は 200 + [] （Req 6.4） ----

func TestAdminHandler_Overview_EmptyResult_Returns200EmptyArray(t *testing.T) {
	// Arrange: Service が非 nil 空 slice を返す（全体 0 件 / Req 6.4）。
	svc := &fakeAdminService{overviewOut: []TenantOverview{}}
	h := NewAdminHandler(svc, authz.New(), &handlerFakeLogger{})
	claims := newAdminClaims()

	// Act
	rec := doAdminRequest(t, h, "", &claims)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 body=%q", rec.Code, rec.Body.String())
	}
	// JSON は null ではなく空配列 [] であること（Req 6.4）。
	if got := rec.Body.String(); got != "[]\n" {
		t.Errorf("body = %q; want %q", got, "[]\n")
	}
}
