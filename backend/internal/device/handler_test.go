package device

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/authz"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/httpserver"
)

// ---- Test doubles ----

// fakeHandlerService は device.Service を満たす Handler テスト用 spy。各メソッドの呼び出し回数・
// 受信引数を記録し、任意の戻り値 / エラーを返せる。read 系のため write メソッドは存在しない
// （Service interface が write を持たない = Req 7.3 の型担保）。
type fakeHandlerService struct {
	listCalls  int
	listTenant uuid.UUID
	listFilter ListFilter
	listOut    []DeviceSummary
	listErr    error

	getCalls  int
	getTenant uuid.UUID
	getID     uuid.UUID
	getOut    DeviceDetail
	getErr    error

	overviewCalls int
}

func (s *fakeHandlerService) List(_ context.Context, tenantID uuid.UUID, f ListFilter) ([]DeviceSummary, error) {
	s.listCalls++
	s.listTenant = tenantID
	s.listFilter = f
	return s.listOut, s.listErr
}

func (s *fakeHandlerService) Get(_ context.Context, tenantID, deviceID uuid.UUID) (DeviceDetail, error) {
	s.getCalls++
	s.getTenant = tenantID
	s.getID = deviceID
	return s.getOut, s.getErr
}

func (s *fakeHandlerService) Overview(_ context.Context, _ *uuid.UUID) ([]TenantOverview, error) {
	s.overviewCalls++
	return nil, nil
}

// handlerFakeLogger は logger.Logger を満たす WARN spy（policy handler_test の hFakeLogger と同型）。
type handlerFakeLogger struct {
	warnCalls []handlerLogCall
}

type handlerLogCall struct {
	msg    string
	fields []any
}

func (f *handlerFakeLogger) Debug(_ string, _ ...any) {}
func (f *handlerFakeLogger) Info(_ string, _ ...any)  {}
func (f *handlerFakeLogger) Warn(msg string, fields ...any) {
	f.warnCalls = append(f.warnCalls, handlerLogCall{msg: msg, fields: fields})
}
func (f *handlerFakeLogger) Error(_ string, _ ...any)    {}
func (f *handlerFakeLogger) With(_ ...any) logger.Logger { return f }
func (f *handlerFakeLogger) Sync() error                 { return nil }

// hasWarnReason は WARN 呼び出し列に deny_reason=want を含む呼び出しがあるかを返す。
func (f *handlerFakeLogger) hasWarnReason(want string) bool {
	for _, call := range f.warnCalls {
		for i := 0; i+1 < len(call.fields); i += 2 {
			if k, ok := call.fields[i].(string); ok && k == "deny_reason" {
				if v, ok := call.fields[i+1].(string); ok && v == want {
					return true
				}
			}
		}
	}
	return false
}

// newTenantClaims は指定 role の tenant-console claims を作る helper。
func newTenantClaims(tenantID uuid.UUID, roles ...string) httpserver.AuthClaims {
	return httpserver.AuthClaims{
		TenantID:          tenantID,
		AdminUserID:       uuid.New(),
		Roles:             roles,
		Console:           "tenant-console",
		SessionHashPrefix: "abc12345",
	}
}

// doRequest は Handler を本番同様に親 chi router へ `Mount("/devices", h)` した上でリクエストを
// 実行する helper。cmd/api の `routers.API.Mount("/devices", deviceHandler)` 配線（task 8 /
// policy.Handler と同方式）を忠実に再現し、chi の prefix strip 後に内包 router の root route が
// 成立する経路を検証する。claimsOrNil が nil なら claims を ctx に注入しない（401 経路）。
func doRequest(t *testing.T, h *Handler, method, target string, claimsOrNil *httpserver.AuthClaims) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	router.Mount("/devices", h)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, strings.NewReader(""))
	if claimsOrNil != nil {
		req = req.WithContext(httpserver.WithAuthClaims(req.Context(), *claimsOrNil))
	}
	router.ServeHTTP(rec, req)
	return rec
}

// decodeErrCode は 4xx/5xx 応答 body の code を取り出す helper。
func decodeErrCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v body=%q", err, rec.Body.String())
	}
	return body.Code
}

// ---- (a) claims 不在で 401（防御的 / Req 5.x 前段） ----

func TestHandler_List_NoClaims_Returns401(t *testing.T) {
	// Arrange
	svc := &fakeHandlerService{}
	log := &handlerFakeLogger{}
	h := NewHandler(svc, authz.New(), log)

	// Act（claims を注入しない）
	rec := doRequest(t, h, http.MethodGet, "/devices", nil)

	// Assert
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401 body=%q", rec.Code, rec.Body.String())
	}
	if svc.listCalls != 0 {
		t.Errorf("401 で svc.List が呼ばれてはならない（呼び出し回数=%d）", svc.listCalls)
	}
	if code := decodeErrCode(t, rec); code != string(pkgerrors.CodeUnauthenticated) {
		t.Errorf("body.Code = %q; want %q", code, pkgerrors.CodeUnauthenticated)
	}
	if !log.hasWarnReason("missing auth claims") {
		t.Errorf("claims 不在経路で deny_reason=missing auth claims の WARN ログが無い; warnCalls=%+v", log.warnCalls)
	}
}

// ---- (b) provisioned されない role で 403 + deny WARN（authz failure path / NFR 3.1） ----
//
// TenantAdmin / Operator / Viewer はいずれも device read を許可されているため、deny を作るには
// permissionMatrix に存在しない未知 role を用いる（fail-closed で deny / policy handler_test の
// deny ケースと同趣旨）。

func TestHandler_List_UnprivilegedRole_Returns403(t *testing.T) {
	// Arrange
	tenantID := uuid.New()
	svc := &fakeHandlerService{}
	log := &handlerFakeLogger{}
	h := NewHandler(svc, authz.New(), log)
	claims := newTenantClaims(tenantID, "NoAccessRole")

	// Act
	rec := doRequest(t, h, http.MethodGet, "/devices", &claims)

	// Assert
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403 body=%q", rec.Code, rec.Body.String())
	}
	if svc.listCalls != 0 {
		t.Errorf("403 で svc.List が呼ばれてはならない（呼び出し回数=%d）", svc.listCalls)
	}
	if code := decodeErrCode(t, rec); code != string(pkgerrors.CodeForbidden) {
		t.Errorf("body.Code = %q; want %q", code, pkgerrors.CodeForbidden)
	}
	// NFR 3.1: deny は構造化 WARN（deny_reason=authz denied）で記録される。
	if !log.hasWarnReason("authz denied") {
		t.Errorf("deny 経路で deny_reason=authz denied の WARN ログが無い; warnCalls=%+v", log.warnCalls)
	}
}

// ---- (c) TenantAdmin の GET 一覧 → 200 + 自テナント scoped + 既定ページング（Req 1.1） ----

func TestHandler_List_TenantAdmin_Returns200OwnTenant(t *testing.T) {
	// Arrange
	tenantID := uuid.New()
	svc := &fakeHandlerService{listOut: []DeviceSummary{{ID: uuid.New(), AMAPIDeviceName: "d1"}}}
	h := NewHandler(svc, authz.New(), &handlerFakeLogger{})
	claims := newTenantClaims(tenantID, "TenantAdmin")

	// Act（query 無し = 既定ページング）
	rec := doRequest(t, h, http.MethodGet, "/devices", &claims)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 body=%q", rec.Code, rec.Body.String())
	}
	if svc.listCalls != 1 {
		t.Fatalf("svc.List 呼び出し回数 = %d; want 1", svc.listCalls)
	}
	// 自テナント scoped: Service へ claims.TenantID が渡る（RLS に分離を委ねる前提 / Req 1.1）。
	if svc.listTenant != tenantID {
		t.Errorf("svc.List tenantID = %v; want %v", svc.listTenant, tenantID)
	}
	// 既定ページング補完（page=1 / page_size=50）とフィルタ無指定（nil）。
	if svc.listFilter.Page != defaultPage || svc.listFilter.PageSize != defaultPageSize {
		t.Errorf("既定ページング = (Page=%d, PageSize=%d); want (1, 50)", svc.listFilter.Page, svc.listFilter.PageSize)
	}
	if svc.listFilter.Compliance != nil || svc.listFilter.Mode != nil || svc.listFilter.SyncDelayed != nil {
		t.Errorf("フィルタ無指定は nil であるべき; got %+v", svc.listFilter)
	}
	var summaries []DeviceSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &summaries); err != nil {
		t.Fatalf("decode []DeviceSummary: %v body=%q", err, rec.Body.String())
	}
	if len(summaries) != 1 {
		t.Errorf("summary 件数 = %d; want 1", len(summaries))
	}
}

// ---- (d) フィルタ query を ListFilter へ parse する（Req 1.1 / parse 正常系） ----

func TestHandler_List_ParsesFilterQuery_Returns200(t *testing.T) {
	// Arrange
	tenantID := uuid.New()
	svc := &fakeHandlerService{}
	h := NewHandler(svc, authz.New(), &handlerFakeLogger{})
	claims := newTenantClaims(tenantID, "TenantAdmin")

	// Act
	rec := doRequest(t, h, http.MethodGet,
		"/devices?compliance=non_compliant&mode=dedicated&sync_state=delayed&page=3&page_size=20", &claims)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 body=%q", rec.Code, rec.Body.String())
	}
	f := svc.listFilter
	if f.Compliance == nil || *f.Compliance != ComplianceStatusNonCompliant {
		t.Errorf("filter.Compliance = %v; want non_compliant", f.Compliance)
	}
	if f.Mode == nil || *f.Mode != DeviceModeDedicated {
		t.Errorf("filter.Mode = %v; want dedicated", f.Mode)
	}
	if f.SyncDelayed == nil || *f.SyncDelayed != true {
		t.Errorf("filter.SyncDelayed = %v; want true（delayed）", f.SyncDelayed)
	}
	if f.Page != 3 || f.PageSize != 20 {
		t.Errorf("ページング = (Page=%d, PageSize=%d); want (3, 20)", f.Page, f.PageSize)
	}
}

// ---- (e) 未定義フィルタ値は 400（Req 1.6） ----

func TestHandler_List_InvalidFilterValue_Returns400(t *testing.T) {
	cases := []struct {
		name  string
		query string
	}{
		{"未定義の compliance 分類", "compliance=bogus"},
		{"未定義の mode", "mode=bogus"},
		{"未定義の sync_state", "sync_state=bogus"},
		{"非数値の page", "page=abc"},
		{"1 未満の page", "page=0"},
		{"非数値の page_size", "page_size=abc"},
		{"1 未満の page_size", "page_size=0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			tenantID := uuid.New()
			svc := &fakeHandlerService{}
			h := NewHandler(svc, authz.New(), &handlerFakeLogger{})
			claims := newTenantClaims(tenantID, "TenantAdmin")

			// Act
			rec := doRequest(t, h, http.MethodGet, "/devices?"+tc.query, &claims)

			// Assert
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400 body=%q", rec.Code, rec.Body.String())
			}
			if svc.listCalls != 0 {
				t.Errorf("不正フィルタで svc.List が呼ばれてはならない（呼び出し回数=%d）", svc.listCalls)
			}
			if code := decodeErrCode(t, rec); code != string(pkgerrors.CodeInvalidRequest) {
				t.Errorf("body.Code = %q; want %q", code, pkgerrors.CodeInvalidRequest)
			}
		})
	}
}

// ---- (f) page_size 上限超過は 400 ではなく 200 に clamp（Req 1.6 の境界） ----

func TestHandler_List_PageSizeOverMax_ClampsTo200(t *testing.T) {
	// Arrange
	tenantID := uuid.New()
	svc := &fakeHandlerService{}
	h := NewHandler(svc, authz.New(), &handlerFakeLogger{})
	claims := newTenantClaims(tenantID, "TenantAdmin")

	// Act: 上限 200 を超える page_size=500。
	rec := doRequest(t, h, http.MethodGet, "/devices?page_size=500", &claims)

	// Assert: 400 にはせず 200 成功、svc.List へ渡る PageSize は 200 に clamp される。
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200（clamp / 400 にしない）body=%q", rec.Code, rec.Body.String())
	}
	if svc.listCalls != 1 {
		t.Fatalf("svc.List 呼び出し回数 = %d; want 1", svc.listCalls)
	}
	if svc.listFilter.PageSize != maxPageSize {
		t.Errorf("clamp 後 PageSize = %d; want %d", svc.listFilter.PageSize, maxPageSize)
	}
}

// ---- (g) compliance=unsupported は 400 にならず parse 成功する第 4 分類（Req 3.4） ----

func TestHandler_List_ComplianceUnsupported_Returns200(t *testing.T) {
	// Arrange
	tenantID := uuid.New()
	svc := &fakeHandlerService{}
	h := NewHandler(svc, authz.New(), &handlerFakeLogger{})
	claims := newTenantClaims(tenantID, "TenantAdmin")

	// Act: unsupported は第 4 の有効な分類値（Req 3.4）。
	rec := doRequest(t, h, http.MethodGet, "/devices?compliance=unsupported", &claims)

	// Assert: 有効値として parse 成功し svc.List へ unsupported フィルタが渡る。
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200（unsupported は有効値）body=%q", rec.Code, rec.Body.String())
	}
	if svc.listFilter.Compliance == nil || *svc.listFilter.Compliance != ComplianceStatusUnsupported {
		t.Errorf("filter.Compliance = %v; want unsupported", svc.listFilter.Compliance)
	}
}

// ---- (h) TenantAdmin の GET 詳細 → 200 + DeviceDetail + 自テナント scoped（Req 2.1） ----

func TestHandler_Get_TenantAdmin_Returns200Detail(t *testing.T) {
	// Arrange
	tenantID := uuid.New()
	deviceID := uuid.New()
	svc := &fakeHandlerService{getOut: DeviceDetail{ID: deviceID, AMAPIDeviceName: "d1", Mode: DeviceModeFullyManaged}}
	h := NewHandler(svc, authz.New(), &handlerFakeLogger{})
	claims := newTenantClaims(tenantID, "TenantAdmin")

	// Act
	rec := doRequest(t, h, http.MethodGet, "/devices/"+deviceID.String(), &claims)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 body=%q", rec.Code, rec.Body.String())
	}
	if svc.getCalls != 1 {
		t.Fatalf("svc.Get 呼び出し回数 = %d; want 1", svc.getCalls)
	}
	// 自テナント scoped + path id が Service へ渡る。
	if svc.getTenant != tenantID {
		t.Errorf("svc.Get tenantID = %v; want %v", svc.getTenant, tenantID)
	}
	if svc.getID != deviceID {
		t.Errorf("svc.Get deviceID = %v; want %v", svc.getID, deviceID)
	}
	var detail DeviceDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decode DeviceDetail: %v body=%q", err, rec.Body.String())
	}
	if detail.ID != deviceID {
		t.Errorf("response DeviceDetail.ID = %v; want %v", detail.ID, deviceID)
	}
}

// ---- (i) 不在端末の GET 詳細 → 404 + 存在差非露出（Req 2.4 / 5.1） ----

func TestHandler_Get_NotFound_Returns404(t *testing.T) {
	// Arrange: Service が ErrDeviceNotFound（404 / 汎用 message）を返す。
	tenantID := uuid.New()
	svc := &fakeHandlerService{getErr: ErrDeviceNotFound}
	h := NewHandler(svc, authz.New(), &handlerFakeLogger{})
	claims := newTenantClaims(tenantID, "TenantAdmin")
	target := "/devices/" + uuid.New().String()

	// Act
	rec := doRequest(t, h, http.MethodGet, target, &claims)

	// Assert
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404 body=%q", rec.Code, rec.Body.String())
	}
	if svc.getCalls != 1 {
		t.Errorf("svc.Get 呼び出し回数 = %d; want 1", svc.getCalls)
	}
	if code := decodeErrCode(t, rec); code != string(pkgerrors.CodeNotFound) {
		t.Errorf("body.Code = %q; want %q", code, pkgerrors.CodeNotFound)
	}
	// 存在差非露出（Req 5.1）: body に対象 device id を露出しない。
	if got := rec.Body.String(); strings.Contains(got, target) {
		t.Errorf("404 body に対象 device id が露出している: %q", got)
	}
}

// ---- (j) 不在 ID と他テナント越境 ID で区別できない同一応答（Req 5.2） ----

func TestHandler_Get_NonExistentAndCrossTenant_IdenticalResponse(t *testing.T) {
	// Arrange: Service は不在・越境のいずれも同一 sentinel ErrDeviceNotFound へ写像済み（Req 5.1 / 5.2）。
	tenantID := uuid.New()
	svc := &fakeHandlerService{getErr: ErrDeviceNotFound}
	h := NewHandler(svc, authz.New(), &handlerFakeLogger{})
	claims := newTenantClaims(tenantID, "TenantAdmin")

	// Act: 存在しない端末 ID と他テナント端末 ID（テスト上は別 UUID）で 2 回要求する。
	recNonExistent := doRequest(t, h, http.MethodGet, "/devices/"+uuid.New().String(), &claims)
	recCrossTenant := doRequest(t, h, http.MethodGet, "/devices/"+uuid.New().String(), &claims)

	// Assert: status と body が完全一致し、存在有無・越境有無を区別させない（Req 5.2）。
	if recNonExistent.Code != http.StatusNotFound || recCrossTenant.Code != http.StatusNotFound {
		t.Fatalf("status = (%d, %d); want (404, 404)", recNonExistent.Code, recCrossTenant.Code)
	}
	if recNonExistent.Body.String() != recCrossTenant.Body.String() {
		t.Errorf("不在と越境で応答 body が異なる: %q vs %q",
			recNonExistent.Body.String(), recCrossTenant.Body.String())
	}
}

// ---- (k) 不正な path id の GET → 400（parseID） ----

func TestHandler_Get_InvalidID_Returns400(t *testing.T) {
	// Arrange
	tenantID := uuid.New()
	svc := &fakeHandlerService{}
	h := NewHandler(svc, authz.New(), &handlerFakeLogger{})
	claims := newTenantClaims(tenantID, "TenantAdmin")

	// Act
	rec := doRequest(t, h, http.MethodGet, "/devices/not-a-uuid", &claims)

	// Assert
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 body=%q", rec.Code, rec.Body.String())
	}
	if svc.getCalls != 0 {
		t.Errorf("不正 id で svc.Get が呼ばれてはならない（呼び出し回数=%d）", svc.getCalls)
	}
	if code := decodeErrCode(t, rec); code != string(pkgerrors.CodeInvalidRequest) {
		t.Errorf("body.Code = %q; want %q", code, pkgerrors.CodeInvalidRequest)
	}
}

// ---- (l) write endpoint を公開しない（Req 7.3） ----
//
// POST / PUT / DELETE は route 未登録のため chi が 405（or 404）を返す。TenantAdmin claims を
// 与えることで「route が存在すれば authorize を通過して 2xx に到達し得る」状態にした上で、なお
// 到達不能（405/404）であることを確認する（write 経路が HTTP に露出していないことの検証）。

func TestHandler_WriteMethods_NotRouted(t *testing.T) {
	tenantID := uuid.New()
	deviceID := uuid.New()
	cases := []struct {
		name   string
		method string
		target string
	}{
		{"POST /devices", http.MethodPost, "/devices"},
		{"PUT /devices/{id}", http.MethodPut, "/devices/" + deviceID.String()},
		{"DELETE /devices/{id}", http.MethodDelete, "/devices/" + deviceID.String()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			svc := &fakeHandlerService{}
			h := NewHandler(svc, authz.New(), &handlerFakeLogger{})
			claims := newTenantClaims(tenantID, "TenantAdmin")

			// Act
			rec := doRequest(t, h, tc.method, tc.target, &claims)

			// Assert: write route は非公開のため未登録 method 応答（405 or 404）になる（Req 7.3）。
			if rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
				t.Fatalf("status = %d; want 405 or 404（write endpoint 非公開）body=%q", rec.Code, rec.Body.String())
			}
			if svc.listCalls != 0 || svc.getCalls != 0 {
				t.Errorf("write method で read Service が呼ばれてはならない（list=%d get=%d）", svc.listCalls, svc.getCalls)
			}
		})
	}
}
