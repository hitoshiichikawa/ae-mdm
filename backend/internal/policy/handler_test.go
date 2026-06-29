package policy

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

// fakeHandlerService は Service interface を満たす Handler テスト用 spy。各メソッドの
// 呼び出し回数・受信引数を記録し、任意の戻り値 / エラーを返せる。
type fakeHandlerService struct {
	createCalls  int
	createTenant uuid.UUID
	createActor  uuid.UUID
	createIn     PolicyRequest
	createView   PolicyView
	createErr    error
	updateCalls  int
	updateID     uuid.UUID
	updateView   PolicyView
	updateErr    error
	listCalls    int
	listTenant   uuid.UUID
	listOut      []PolicySummary
	listErr      error
	getCalls     int
	getTenant    uuid.UUID
	getID        uuid.UUID
	getView      PolicyView
	getErr       error
	deleteCalls  int
	deleteID     uuid.UUID
	deleteErr    error
	assignCalls  int
	assignDevice uuid.UUID
	assignPolicy uuid.UUID
	assignErr    error
}

func (s *fakeHandlerService) Create(_ context.Context, actor, tenantID uuid.UUID, in PolicyRequest) (PolicyView, error) {
	s.createCalls++
	s.createActor = actor
	s.createTenant = tenantID
	s.createIn = in
	return s.createView, s.createErr
}

func (s *fakeHandlerService) Update(_ context.Context, _, tenantID, policyID uuid.UUID, _ PolicyRequest) (PolicyView, error) {
	s.updateCalls++
	s.createTenant = tenantID
	s.updateID = policyID
	return s.updateView, s.updateErr
}

func (s *fakeHandlerService) List(_ context.Context, tenantID uuid.UUID) ([]PolicySummary, error) {
	s.listCalls++
	s.listTenant = tenantID
	return s.listOut, s.listErr
}

func (s *fakeHandlerService) Get(_ context.Context, tenantID, policyID uuid.UUID) (PolicyView, error) {
	s.getCalls++
	s.getTenant = tenantID
	s.getID = policyID
	return s.getView, s.getErr
}

func (s *fakeHandlerService) Delete(_ context.Context, _, _, policyID uuid.UUID) error {
	s.deleteCalls++
	s.deleteID = policyID
	return s.deleteErr
}

func (s *fakeHandlerService) Assign(_ context.Context, _, _, deviceID, policyID uuid.UUID) error {
	s.assignCalls++
	s.assignDevice = deviceID
	s.assignPolicy = policyID
	return s.assignErr
}

// hFakeLogger は logger.Logger を満たす WARN spy（audit/handler_test の fakeLogger と同型）。
type hFakeLogger struct {
	warnCalls []hLogCall
}

type hLogCall struct {
	msg    string
	fields []any
}

func (f *hFakeLogger) Debug(_ string, _ ...any) {}
func (f *hFakeLogger) Info(_ string, _ ...any)  {}
func (f *hFakeLogger) Warn(msg string, fields ...any) {
	f.warnCalls = append(f.warnCalls, hLogCall{msg: msg, fields: fields})
}
func (f *hFakeLogger) Error(_ string, _ ...any)    {}
func (f *hFakeLogger) With(_ ...any) logger.Logger { return f }
func (f *hFakeLogger) Sync() error                 { return nil }

// hasWarnReason は WARN 呼び出し列に deny_reason=want を含む呼び出しがあるかを返す。
func (f *hFakeLogger) hasWarnReason(want string) bool {
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

// doRequest は Handler を本番同様に親 chi router へ `Mount("/policies", h)` した上でリクエストを
// 実行する helper。cmd/api の `routers.API.Mount("/policies", policyHandler)` 配線（design.md
// Modified Files / tasks.md 6.1 / audit.Handler と同方式）を忠実に再現することで、chi の
// prefix strip 後に内包 router の root route が成立する経路を検証する。claimsOrNil が nil なら
// claims を ctx に注入しない（401 経路）。
func doRequest(t *testing.T, h *Handler, method, target, body string, claimsOrNil *httpserver.AuthClaims) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	router.Mount("/policies", h)

	rec := httptest.NewRecorder()
	var reader *strings.Reader
	if body != "" {
		reader = strings.NewReader(body)
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, target, reader)
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

// ---- (a) Viewer の POST → 403（authz failure path / Req 4.1 / NFR 3.1） ----

func TestHandler_Create_Viewer_Returns403(t *testing.T) {
	// Arrange
	tenantID := uuid.New()
	svc := &fakeHandlerService{}
	log := &hFakeLogger{}
	h := NewHandler(svc, authz.New(), log)
	claims := newTenantClaims(tenantID, "Viewer")

	// Act
	rec := doRequest(t, h, http.MethodPost, "/policies", `{"name":"p","body":{}}`, &claims)

	// Assert
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403 body=%q", rec.Code, rec.Body.String())
	}
	if svc.createCalls != 0 {
		t.Errorf("403 で svc.Create が呼ばれてはならない（呼び出し回数=%d）", svc.createCalls)
	}
	if code := decodeErrCode(t, rec); code != string(pkgerrors.CodeForbidden) {
		t.Errorf("body.Code = %q; want %q", code, pkgerrors.CodeForbidden)
	}
	// NFR 3.1: deny は構造化 WARN（deny_reason=authz denied）で記録される。
	if !log.hasWarnReason("authz denied") {
		t.Errorf("deny 経路で deny_reason=authz denied の WARN ログが無い; warnCalls=%+v", log.warnCalls)
	}
}

// ---- (b) TenantAdmin の POST → 200（Req 4.1 / 正常系） ----

func TestHandler_Create_TenantAdmin_Returns200(t *testing.T) {
	// Arrange
	tenantID := uuid.New()
	policyID := uuid.New()
	svc := &fakeHandlerService{createView: PolicyView{ID: policyID, Name: "p", Version: 0}}
	h := NewHandler(svc, authz.New(), &hFakeLogger{})
	claims := newTenantClaims(tenantID, "TenantAdmin")

	// Act
	rec := doRequest(t, h, http.MethodPost, "/policies", `{"name":"p","body":{}}`, &claims)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 body=%q", rec.Code, rec.Body.String())
	}
	if svc.createCalls != 1 {
		t.Fatalf("svc.Create 呼び出し回数 = %d; want 1", svc.createCalls)
	}
	// actor / tenantID は claims から取得して Service へ引数で渡される（design Components）。
	if svc.createTenant != tenantID {
		t.Errorf("svc.Create tenantID = %v; want %v", svc.createTenant, tenantID)
	}
	if svc.createActor != claims.AdminUserID {
		t.Errorf("svc.Create actor = %v; want %v", svc.createActor, claims.AdminUserID)
	}
	if svc.createIn.Name != "p" {
		t.Errorf("svc.Create in.Name = %q; want p", svc.createIn.Name)
	}
	var view PolicyView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode PolicyView: %v body=%q", err, rec.Body.String())
	}
	if view.ID != policyID {
		t.Errorf("response PolicyView.ID = %v; want %v", view.ID, policyID)
	}
}

// ---- (c) 不在 GET → 404 + 存在差非露出（Req 4.5 / NFR） ----

func TestHandler_Get_NotFound_Returns404(t *testing.T) {
	// Arrange: Service が ErrPolicyNotFound（404 / 固定 message）を返す。
	tenantID := uuid.New()
	svc := &fakeHandlerService{getErr: ErrPolicyNotFound}
	h := NewHandler(svc, authz.New(), &hFakeLogger{})
	claims := newTenantClaims(tenantID, "TenantAdmin")
	target := "/policies/" + uuid.New().String()

	// Act
	rec := doRequest(t, h, http.MethodGet, target, "", &claims)

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
	// 存在差非露出（Req 4.5）: body の message は固定文言で policy id 等を露出しない。
	if got := rec.Body.String(); strings.Contains(got, target) {
		t.Errorf("404 body に対象 policy id が露出している: %q", got)
	}
}

// ---- (d) claims 不在で 401（防御的 / Req 4.1 周辺） ----

func TestHandler_List_NoClaims_Returns401(t *testing.T) {
	// Arrange
	svc := &fakeHandlerService{}
	log := &hFakeLogger{}
	h := NewHandler(svc, authz.New(), log)

	// Act（claims を注入しない）
	rec := doRequest(t, h, http.MethodGet, "/policies", "", nil)

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

// ---- (e) TenantAdmin の GET 一覧 → 200 + 自テナント scoped（Req 4.4） ----

func TestHandler_List_TenantAdmin_Returns200OwnTenant(t *testing.T) {
	// Arrange
	tenantID := uuid.New()
	svc := &fakeHandlerService{listOut: []PolicySummary{{ID: uuid.New(), Name: "p1"}}}
	h := NewHandler(svc, authz.New(), &hFakeLogger{})
	claims := newTenantClaims(tenantID, "TenantAdmin")

	// Act
	rec := doRequest(t, h, http.MethodGet, "/policies", "", &claims)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 body=%q", rec.Code, rec.Body.String())
	}
	// 自テナント scoped: Service へ claims.TenantID が渡される（RLS に分離を委ねる前提 / Req 4.4）。
	if svc.listTenant != tenantID {
		t.Errorf("svc.List tenantID = %v; want %v", svc.listTenant, tenantID)
	}
	var summaries []PolicySummary
	if err := json.Unmarshal(rec.Body.Bytes(), &summaries); err != nil {
		t.Fatalf("decode []PolicySummary: %v body=%q", err, rec.Body.String())
	}
	if len(summaries) != 1 {
		t.Errorf("summary 件数 = %d; want 1", len(summaries))
	}
}

// ---- (f) 検証エラー（business rule = 3000 件超）→ 422 + 全件 details（Req 2.2 / 2.4） ----

func TestHandler_Create_BusinessRuleViolation_Returns422(t *testing.T) {
	// Arrange: Service が business rule 違反のみの ValidationFailedError（422）を返す。
	tenantID := uuid.New()
	verr := newValidationFailedError([]ValidationError{
		{Domain: DomainApp, Field: "AppCount", Kind: KindBusinessRule, Message: "アプリ件数が上限(3000)を超えています"},
	})
	svc := &fakeHandlerService{createErr: verr}
	h := NewHandler(svc, authz.New(), &hFakeLogger{})
	claims := newTenantClaims(tenantID, "TenantAdmin")

	// Act
	rec := doRequest(t, h, http.MethodPost, "/policies", `{"name":"p","body":{}}`, &claims)

	// Assert
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d; want 422 body=%q", rec.Code, rec.Body.String())
	}
	var body validationErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode validationErrorBody: %v body=%q", err, rec.Body.String())
	}
	if body.Code != string(pkgerrors.CodeBusinessRule) {
		t.Errorf("body.Code = %q; want %q", body.Code, pkgerrors.CodeBusinessRule)
	}
	if len(body.Details) != 1 {
		t.Fatalf("details 件数 = %d; want 1（全件提示 / Req 2.4）", len(body.Details))
	}
	if body.Details[0].Kind != string(KindBusinessRule) {
		t.Errorf("details[0].Kind = %q; want %q", body.Details[0].Kind, KindBusinessRule)
	}
}

// ---- (g) 検証エラー（invalid field 混在）→ 400 + 全件 details（Req 2.3 / 2.4） ----

func TestHandler_Create_InvalidField_Returns400WithAllDetails(t *testing.T) {
	// Arrange: invalid field と business rule が混在 → top-level は 400（invalid field 優先）。
	tenantID := uuid.New()
	verr := newValidationFailedError([]ValidationError{
		{Domain: DomainPassword, Field: "MinimumLength", Kind: KindInvalidField, Message: "passwordMinimumLength は整数である必要があります"},
		{Domain: DomainApp, Field: "AppCount", Kind: KindBusinessRule, Message: "アプリ件数が上限を超えています"},
	})
	svc := &fakeHandlerService{createErr: verr}
	h := NewHandler(svc, authz.New(), &hFakeLogger{})
	claims := newTenantClaims(tenantID, "TenantAdmin")

	// Act
	rec := doRequest(t, h, http.MethodPost, "/policies", `{"name":"p","body":{}}`, &claims)

	// Assert
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 body=%q", rec.Code, rec.Body.String())
	}
	var body validationErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode validationErrorBody: %v body=%q", err, rec.Body.String())
	}
	if body.Code != string(pkgerrors.CodeInvalidRequest) {
		t.Errorf("body.Code = %q; want %q", body.Code, pkgerrors.CodeInvalidRequest)
	}
	// 全件提示（Req 2.4）: invalid field と business rule の両方が details に載る。
	if len(body.Details) != 2 {
		t.Fatalf("details 件数 = %d; want 2（全件提示 / Req 2.4）", len(body.Details))
	}
}

// ---- (h) malformed JSON body の POST → 400（入力検証契約） ----

func TestHandler_Create_MalformedJSON_Returns400(t *testing.T) {
	// Arrange
	tenantID := uuid.New()
	svc := &fakeHandlerService{}
	h := NewHandler(svc, authz.New(), &hFakeLogger{})
	claims := newTenantClaims(tenantID, "TenantAdmin")

	// Act: 壊れた JSON。
	rec := doRequest(t, h, http.MethodPost, "/policies", `{"name":`, &claims)

	// Assert
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 body=%q", rec.Code, rec.Body.String())
	}
	if svc.createCalls != 0 {
		t.Errorf("malformed body で svc.Create が呼ばれてはならない（呼び出し回数=%d）", svc.createCalls)
	}
	if code := decodeErrCode(t, rec); code != string(pkgerrors.CodeInvalidRequest) {
		t.Errorf("body.Code = %q; want %q", code, pkgerrors.CodeInvalidRequest)
	}
}

// ---- (i) 不正な path id の GET → 400（parseID） ----

func TestHandler_Get_InvalidID_Returns400(t *testing.T) {
	// Arrange
	tenantID := uuid.New()
	svc := &fakeHandlerService{}
	h := NewHandler(svc, authz.New(), &hFakeLogger{})
	claims := newTenantClaims(tenantID, "TenantAdmin")

	// Act
	rec := doRequest(t, h, http.MethodGet, "/policies/not-a-uuid", "", &claims)

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

// ---- (j) DELETE 成功 → 204（Req 5.3） ----

func TestHandler_Delete_TenantAdmin_Returns204(t *testing.T) {
	// Arrange
	tenantID := uuid.New()
	policyID := uuid.New()
	svc := &fakeHandlerService{}
	h := NewHandler(svc, authz.New(), &hFakeLogger{})
	claims := newTenantClaims(tenantID, "TenantAdmin")

	// Act
	rec := doRequest(t, h, http.MethodDelete, "/policies/"+policyID.String(), "", &claims)

	// Assert
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d; want 204 body=%q", rec.Code, rec.Body.String())
	}
	if svc.deleteCalls != 1 {
		t.Fatalf("svc.Delete 呼び出し回数 = %d; want 1", svc.deleteCalls)
	}
	if svc.deleteID != policyID {
		t.Errorf("svc.Delete policyID = %v; want %v", svc.deleteID, policyID)
	}
}

// ---- (k) DELETE 競合（割当済み端末あり）→ 409（Req 5.3 / design 確認事項 3） ----

func TestHandler_Delete_Conflict_Returns409(t *testing.T) {
	// Arrange: Service が ErrDeleteConflict（409）を返す。
	tenantID := uuid.New()
	svc := &fakeHandlerService{deleteErr: ErrDeleteConflict}
	h := NewHandler(svc, authz.New(), &hFakeLogger{})
	claims := newTenantClaims(tenantID, "TenantAdmin")

	// Act
	rec := doRequest(t, h, http.MethodDelete, "/policies/"+uuid.New().String(), "", &claims)

	// Assert
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d; want 409 body=%q", rec.Code, rec.Body.String())
	}
	if code := decodeErrCode(t, rec); code != string(pkgerrors.CodeConflict) {
		t.Errorf("body.Code = %q; want %q", code, pkgerrors.CodeConflict)
	}
}

// ---- (l) PUT assign 成功 → 204 + device_id 写像（Req 3.1） ----

func TestHandler_Assign_TenantAdmin_Returns204(t *testing.T) {
	// Arrange
	tenantID := uuid.New()
	policyID := uuid.New()
	deviceID := uuid.New()
	svc := &fakeHandlerService{}
	h := NewHandler(svc, authz.New(), &hFakeLogger{})
	claims := newTenantClaims(tenantID, "TenantAdmin")

	// Act
	rec := doRequest(t, h, http.MethodPut, "/policies/"+policyID.String()+"/assign",
		`{"device_id":"`+deviceID.String()+`"}`, &claims)

	// Assert
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d; want 204 body=%q", rec.Code, rec.Body.String())
	}
	if svc.assignCalls != 1 {
		t.Fatalf("svc.Assign 呼び出し回数 = %d; want 1", svc.assignCalls)
	}
	if svc.assignDevice != deviceID {
		t.Errorf("svc.Assign deviceID = %v; want %v", svc.assignDevice, deviceID)
	}
	if svc.assignPolicy != policyID {
		t.Errorf("svc.Assign policyID = %v; want %v", svc.assignPolicy, policyID)
	}
}

// ---- (m) PUT assign で自テナント不在 device → 404（Req 3.3 / 存在差非露出） ----

func TestHandler_Assign_NotFound_Returns404(t *testing.T) {
	// Arrange: Service が ErrPolicyNotFound（404）を返す（他テナント policy/device 越境を NotFound に倒す）。
	tenantID := uuid.New()
	svc := &fakeHandlerService{assignErr: ErrPolicyNotFound}
	h := NewHandler(svc, authz.New(), &hFakeLogger{})
	claims := newTenantClaims(tenantID, "TenantAdmin")

	// Act
	rec := doRequest(t, h, http.MethodPut, "/policies/"+uuid.New().String()+"/assign",
		`{"device_id":"`+uuid.New().String()+`"}`, &claims)

	// Assert
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404 body=%q", rec.Code, rec.Body.String())
	}
	if code := decodeErrCode(t, rec); code != string(pkgerrors.CodeNotFound) {
		t.Errorf("body.Code = %q; want %q", code, pkgerrors.CodeNotFound)
	}
}

// ---- (n) AMAPI 上流エラー（502）→ そのまま伝達（Req 1.4 周辺の HTTP 写像） ----

func TestHandler_Create_UpstreamError_Returns502(t *testing.T) {
	// Arrange: Service が AMAPI 上流エラー（CodeUpstream / 502）を返す。
	tenantID := uuid.New()
	svc := &fakeHandlerService{createErr: pkgerrors.New(pkgerrors.CodeUpstream, "amapi upstream failed")}
	h := NewHandler(svc, authz.New(), &hFakeLogger{})
	claims := newTenantClaims(tenantID, "TenantAdmin")

	// Act
	rec := doRequest(t, h, http.MethodPost, "/policies", `{"name":"p","body":{}}`, &claims)

	// Assert
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d; want 502 body=%q", rec.Code, rec.Body.String())
	}
	if code := decodeErrCode(t, rec); code != string(pkgerrors.CodeUpstream) {
		t.Errorf("body.Code = %q; want %q", code, pkgerrors.CodeUpstream)
	}
}
