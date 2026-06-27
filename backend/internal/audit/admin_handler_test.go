package audit

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	internalerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/authz"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/httpserver"
)

// ---- admin 専用テストヘルパ ----

// newAdminClaims は admin-console / SuperAdmin の AuthClaims を作る helper。
//
// tenant-console 用の newTenantClaims（handler_test.go）と異なり、admin は
// TenantID = uuid.Nil（SuperAdmin の cross-tenant context）/ Console = admin-console /
// IsSuperAdmin = true を持つ（admin_middleware_test.go / server_test.go の SuperAdmin claims と整合）。
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

// doAdminRequest は AdminHandler を本番同様に親 chi router へ `Mount("/audit-logs", h)` した上で
// GET /audit-logs?<rawQuery> を実行する helper。
//
// cmd/api の `routers.Admin.Mount("/audit-logs", adminHandler)` 配線（実 path
// `/api/admin/audit-logs`）を忠実に再現することで、chi の prefix strip 後に handler 内の
// root route（`/`）が成立する経路を検証する。claimsOrNil が nil の場合は claims を ctx に
// 注入しない（未認証経路の検証用）。
func doAdminRequest(t *testing.T, h *AdminHandler, rawQuery string, claimsOrNil *httpserver.AuthClaims) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	router.Mount("/audit-logs", h)

	rec := httptest.NewRecorder()
	target := "/audit-logs"
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

// ---- (a) SuperAdmin / tenant_id 無しで 200 + SuperAdmin TenantContext 確立（Req 3.1 / 3.5） ----

func TestAdminHandler_List_SuperAdmin_NoTenantID_Returns200AndEstablishesSuperAdminContext(t *testing.T) {
	// Arrange
	svc := &fakeService{listOut: []Event{
		{ID: uuid.New(), TenantID: uuid.New(), ActorID: uuid.New(), EventType: "policy_change", Result: ResultSuccess, OccurredAt: fixedNow},
		// NULL テナント（uuid.Nil）の行も横断ビューに含まれる（Req 3.1 / 3.5）。
		{ID: uuid.New(), TenantID: uuid.Nil, ActorID: uuid.New(), EventType: "system_event", Result: ResultSuccess, OccurredAt: fixedNow},
	}}
	log := &fakeLogger{}
	h := NewAdminHandler(svc, authz.New(), log)
	claims := newAdminClaims()

	// Act
	rec := doAdminRequest(t, h, "", &claims)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 body=%q", rec.Code, rec.Body.String())
	}
	if svc.listCalls != 1 {
		t.Fatalf("svc.List 呼び出し回数 = %d; want 1", svc.listCalls)
	}
	// SuperAdmin TenantContext が確立されていること（RLS の is_superadmin 句で全テナント + NULL 可視 / Req 3.1 / 3.5）。
	tc, err := db.FromContext(svc.listedCtx)
	if err != nil {
		t.Fatalf("svc.List に渡った ctx に TenantContext が確立されていない: %v", err)
	}
	if !tc.IsSuperAdmin {
		t.Errorf("TenantContext.IsSuperAdmin = false; want true（SuperAdmin 横断ビュー / Req 3.1）")
	}
	if tc.TenantID != uuid.Nil {
		t.Errorf("TenantContext.TenantID = %v; want uuid.Nil（SuperAdmin context / Req 3.5）", tc.TenantID)
	}
	// tenant_id 無し = 全テナント横断ビューなので Filter.TenantID は nil（Req 3.2 / 3.5）。
	if svc.listedFilter.TenantID != nil {
		t.Errorf("tenant_id 無しなのに Filter.TenantID が設定されている: %v", svc.listedFilter.TenantID)
	}
	dtos := decodeDTOs(t, rec)
	if len(dtos) != 2 {
		t.Fatalf("DTO 件数 = %d; want 2", len(dtos))
	}
}

// ---- (b) tenant_id query で Filter.TenantID 設定 / 無指定で nil（Req 3.2 / 3.5） ----

func TestAdminHandler_List_TenantIDQuery_SetsFilterTenantID(t *testing.T) {
	// Arrange
	wantTenant := uuid.New()
	svc := &fakeService{listOut: []Event{}}
	log := &fakeLogger{}
	h := NewAdminHandler(svc, authz.New(), log)
	claims := newAdminClaims()

	// Act
	rec := doAdminRequest(t, h, "tenant_id="+wantTenant.String(), &claims)

	// Assert
	// SuperAdmin × admin-console × cross-tenant は実 authz.New() で allow され 200 になる（Req 4.6）。
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 body=%q", rec.Code, rec.Body.String())
	}
	if svc.listedFilter.TenantID == nil {
		t.Fatalf("tenant_id 指定なのに Filter.TenantID が nil")
	}
	if *svc.listedFilter.TenantID != wantTenant {
		t.Errorf("Filter.TenantID = %v; want %v", *svc.listedFilter.TenantID, wantTenant)
	}
}

func TestAdminHandler_List_NoTenantIDQuery_FilterTenantIDNil(t *testing.T) {
	// Arrange
	svc := &fakeService{listOut: []Event{}}
	h := NewAdminHandler(svc, authz.New(), &fakeLogger{})
	claims := newAdminClaims()

	// Act
	rec := doAdminRequest(t, h, "", &claims)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 body=%q", rec.Code, rec.Body.String())
	}
	if svc.listedFilter.TenantID != nil {
		t.Errorf("tenant_id 無指定なのに Filter.TenantID が設定されている: %v", svc.listedFilter.TenantID)
	}
}

// ---- (c) event_type / actor_id / resource_id / from / to が Filter に写像（Req 3.3） ----

func TestAdminHandler_List_MapsAllFilterFields(t *testing.T) {
	// Arrange
	actorID := uuid.New()
	svc := &fakeService{listOut: []Event{}}
	h := NewAdminHandler(svc, authz.New(), &fakeLogger{})
	claims := newAdminClaims()
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	rawQuery := "event_type=role_change&actor_id=" + actorID.String() +
		"&resource_id=res-42&from=" + from.Format(time.RFC3339) + "&to=" + to.Format(time.RFC3339)

	// Act
	rec := doAdminRequest(t, h, rawQuery, &claims)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 body=%q", rec.Code, rec.Body.String())
	}
	got := svc.listedFilter
	if got.EventType != "role_change" {
		t.Errorf("Filter.EventType = %q; want role_change", got.EventType)
	}
	if got.ActorID != actorID.String() {
		t.Errorf("Filter.ActorID = %q; want %q", got.ActorID, actorID.String())
	}
	if got.ResourceID != "res-42" {
		t.Errorf("Filter.ResourceID = %q; want res-42", got.ResourceID)
	}
	if got.From == nil || !got.From.Equal(from) {
		t.Errorf("Filter.From = %v; want %v", got.From, from)
	}
	if got.To == nil || !got.To.Equal(to) {
		t.Errorf("Filter.To = %v; want %v", got.To, to)
	}
}

// ---- (d) tenant_id・actor_id 非 uuid / from・to 非 RFC3339 で 400 + failure_kind=parse_invalid（Req 3.2 / NFR 3.2） ----

func TestAdminHandler_List_InvalidQuery_Returns400(t *testing.T) {
	claims := newAdminClaims()

	cases := []struct {
		name     string
		rawQuery string
	}{
		{name: "tenant_id 非 uuid", rawQuery: "tenant_id=not-a-uuid"},
		{name: "actor_id 非 uuid", rawQuery: "actor_id=not-a-uuid"},
		{name: "from 非 RFC3339", rawQuery: "from=not-a-date"},
		{name: "to 非 RFC3339", rawQuery: "to=2026-13-99"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			svc := &fakeService{}
			log := &fakeLogger{}
			h := NewAdminHandler(svc, authz.New(), log)

			// Act
			rec := doAdminRequest(t, h, tc.rawQuery, &claims)

			// Assert
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400 body=%q", rec.Code, rec.Body.String())
			}
			if svc.listCalls != 0 {
				t.Errorf("400 で svc.List が呼ばれてはならない（呼び出し回数=%d）", svc.listCalls)
			}
			if code := decodeErrCode(t, rec); code != string(internalerrors.CodeInvalidRequest) {
				t.Errorf("body.Code = %q; want %q", code, internalerrors.CodeInvalidRequest)
			}
			assertWarnFailureKind(t, log, FailureKindParseInvalid)
		})
	}
}

// ---- (f) tenant_id 無しの全テナント横断ビューでも cross-tenant authz matrix が gate する（Req 4.6） ----

// TestAdminHandler_List_NoTenantID_NonSuperAdmin_Returns403 は、tenant_id 無し（全テナント横断
// ビュー）の経路でも許可マトリクスが cross-tenant `audit_log read` を判定し、SuperAdmin 以外を
// 403 で拒否することを回帰検証する（Req 4.4 / 4.6）。
//
// 修正前は tenant_id 無しのとき authz 判定を skip していたため、固定ガードを外れた非 SuperAdmin の
// admin-console claims が handler 本体まで到達すると 200 に倒れていた（許可マトリクスの bypass）。
// 本テストは all-tenant 経路でも authz が main path で発火することを担保する。
func TestAdminHandler_List_NoTenantID_NonSuperAdmin_Returns403(t *testing.T) {
	// Arrange: admin-console aud だが SuperAdmin ロールを持たない claims
	// （RequireAdminConsoleAndSuperAdmin 固定ガードは unit では不在のため handler 本体まで到達する）。
	svc := &fakeService{listOut: []Event{}}
	log := &fakeLogger{}
	h := NewAdminHandler(svc, authz.New(), log)
	claims := httpserver.AuthClaims{
		TenantID:          uuid.Nil,
		AdminUserID:       uuid.New(),
		Roles:             []string{"TenantAdmin"}, // SuperAdmin ではない
		IsSuperAdmin:      false,
		Console:           "admin-console",
		SessionHashPrefix: "abc12345",
	}

	// Act: tenant_id 無し（全テナント横断ビュー）。
	rec := doAdminRequest(t, h, "", &claims)

	// Assert: cross-tenant authz matrix の判定で 403（Req 4.6）。svc.List には到達しない。
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403（all-tenant view も authz matrix が gate する / Req 4.6） body=%q", rec.Code, rec.Body.String())
	}
	if svc.listCalls != 0 {
		t.Errorf("403 で svc.List が呼ばれてはならない（呼び出し回数=%d）", svc.listCalls)
	}
	if code := decodeErrCode(t, rec); code != string(internalerrors.CodeForbidden) {
		t.Errorf("body.Code = %q; want %q", code, internalerrors.CodeForbidden)
	}
	// NFR 3.2: admin 経路の認可拒否分岐でも failure_kind=authz_denied を構造化 WARN に出す。
	assertWarnFailureKind(t, log, FailureKindAuthzDenied)
}

// ---- (e) fake Service が空 slice を返したら 200 + [] （Req 3.4） ----

func TestAdminHandler_List_EmptyResult_Returns200EmptyArray(t *testing.T) {
	// Arrange
	svc := &fakeService{listOut: []Event{}}
	h := NewAdminHandler(svc, authz.New(), &fakeLogger{})
	claims := newAdminClaims()

	// Act
	rec := doAdminRequest(t, h, "", &claims)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	// JSON は null ではなく空配列 [] であること（Req 3.4）。
	if got := rec.Body.String(); got != "[]\n" {
		t.Errorf("body = %q; want %q", got, "[]\n")
	}
	dtos := decodeDTOs(t, rec)
	if len(dtos) != 0 {
		t.Errorf("DTO 件数 = %d; want 0", len(dtos))
	}
}

// ---- (g) svc.List が DB エラーを返したら 503 + failure_kind=query_error（NFR 3.2 / Req 3.x） ----

// TestAdminHandler_List_ServiceError_Returns503 は、Service.List が SELECT / scan の DB 失敗
// （CodeUnavailable）を返したとき、cross-tenant 経路が 503 を返し failure_kind=query_error を
// 構造化 WARN に出すことを検証する（design.md「503 / query_error」契約 / NFR 3.2）。
func TestAdminHandler_List_ServiceError_Returns503(t *testing.T) {
	// Arrange: SuperAdmin claims（認可通過）で fake Service が DB 失敗を返す。
	svc := &fakeService{listErr: internalerrors.New(internalerrors.CodeUnavailable, "db down")}
	log := &fakeLogger{}
	h := NewAdminHandler(svc, authz.New(), log)
	claims := newAdminClaims()

	// Act
	rec := doAdminRequest(t, h, "", &claims)

	// Assert
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503 body=%q", rec.Code, rec.Body.String())
	}
	if svc.listCalls != 1 {
		t.Errorf("svc.List 呼び出し回数 = %d; want 1（認可は通過し List で失敗する経路）", svc.listCalls)
	}
	if code := decodeErrCode(t, rec); code != string(internalerrors.CodeUnavailable) {
		t.Errorf("body.Code = %q; want %q", code, internalerrors.CodeUnavailable)
	}
	assertWarnFailureKind(t, log, FailureKindQueryError)
}
