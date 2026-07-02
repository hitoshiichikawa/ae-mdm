package enrollment

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
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/authz"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/httpserver"
)

// ---- Test doubles ----

// fakeHandlerService は Service interface を満たす Handler テスト用 spy。各メソッドの呼び出し回数・
// 受信引数を記録し、任意の戻り値 / エラーを返せる（policy.fakeHandlerService を手本）。
type fakeHandlerService struct {
	issueCalls  int
	issueActor  uuid.UUID
	issueTenant uuid.UUID
	issueIn     IssueRequest
	issueView   TokenView
	issueErr    error

	listCalls  int
	listTenant uuid.UUID
	listOut    []TokenSummary
	listErr    error
}

func (s *fakeHandlerService) IssueToken(_ context.Context, actor, tenantID uuid.UUID, in IssueRequest) (TokenView, error) {
	s.issueCalls++
	s.issueActor = actor
	s.issueTenant = tenantID
	s.issueIn = in
	return s.issueView, s.issueErr
}

func (s *fakeHandlerService) ListTokens(_ context.Context, tenantID uuid.UUID) ([]TokenSummary, error) {
	s.listCalls++
	s.listTenant = tenantID
	return s.listOut, s.listErr
}

// newEnrollmentClaims は指定 role の tenant-console claims を作る helper（policy.newTenantClaims を手本）。
func newEnrollmentClaims(tenantID uuid.UUID, roles ...string) httpserver.AuthClaims {
	return httpserver.AuthClaims{
		TenantID:          tenantID,
		AdminUserID:       uuid.New(),
		Roles:             roles,
		Console:           "tenant-console",
		SessionHashPrefix: "abc12345",
	}
}

// doEnrollmentRequest は Handler を本番同様に親 chi router へ `Mount("/enrollment-tokens", h)` した上で
// リクエストを実行する helper。cmd/api の `routers.API.Mount("/enrollment-tokens", handler)` 配線
// （design.md Modified Files / policy.Handler と同方式）を忠実に再現することで、chi の prefix strip 後に
// 内包 router の root route が成立する経路を検証する。claimsOrNil が nil なら claims を ctx に注入しない
// （401 経路）。
func doEnrollmentRequest(t *testing.T, h *Handler, method, target, body string, claimsOrNil *httpserver.AuthClaims) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	router.Mount("/enrollment-tokens", h)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if claimsOrNil != nil {
		req = req.WithContext(httpserver.WithAuthClaims(req.Context(), *claimsOrNil))
	}
	router.ServeHTTP(rec, req)
	return rec
}

// decodeEnrollErrCode は 4xx/5xx 応答 body の code を取り出す helper。
func decodeEnrollErrCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v body=%q", err, rec.Body.String())
	}
	return body.Code
}

// hasDenyWarn は WARN 呼び出し列に msg + deny_reason=reason を含む呼び出しがあるかを返す
// （service_test.go の fakeLogger を流用 / NFR 4.1）。
func hasDenyWarn(log *fakeLogger, msg, reason string) bool {
	for _, e := range log.snapshot() {
		if e.Level == "warn" && e.Msg == msg {
			if v, ok := e.Fields["deny_reason"].(string); ok && v == reason {
				return true
			}
		}
	}
	return false
}

// ---- (1) Viewer の POST → 403（Req 2.2 / authz failure path / NFR 4.1） ----

func TestHandler_Issue_Viewer_Returns403(t *testing.T) {
	// Arrange
	tenantID := uuid.New()
	svc := &fakeHandlerService{}
	log := &fakeLogger{}
	h := NewHandler(svc, authz.New(), log)
	claims := newEnrollmentClaims(tenantID, "Viewer")

	// Act
	rec := doEnrollmentRequest(t, h, http.MethodPost, "/enrollment-tokens", `{"mode":"fully_managed"}`, &claims)

	// Assert
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403 body=%q", rec.Code, rec.Body.String())
	}
	if svc.issueCalls != 0 {
		t.Errorf("403 で svc.IssueToken が呼ばれてはならない（呼び出し回数=%d）", svc.issueCalls)
	}
	if code := decodeEnrollErrCode(t, rec); code != string(pkgerrors.CodeForbidden) {
		t.Errorf("body.Code = %q; want %q", code, pkgerrors.CodeForbidden)
	}
	// NFR 4.1: deny は構造化 WARN（deny_reason=authz denied）で記録される。
	if !hasDenyWarn(log, "enrollment operation denied", "authz denied") {
		t.Errorf("deny 経路で deny_reason=authz denied の WARN ログが無い; entries=%+v", log.snapshot())
	}
}

// ---- (2) TenantAdmin の POST → 200 + actor/tenant/body 写像（Req 2.1 / 正常系） ----

func TestHandler_Issue_TenantAdmin_Returns200(t *testing.T) {
	// Arrange
	tenantID := uuid.New()
	tokenID := uuid.New()
	svc := &fakeHandlerService{issueView: TokenView{
		ID:         tokenID,
		Mode:       ModeFullyManaged,
		Value:      "secret-token-value",
		QRCodeData: "secret-qr-data",
	}}
	h := NewHandler(svc, authz.New(), &fakeLogger{})
	claims := newEnrollmentClaims(tenantID, "TenantAdmin")

	// Act
	rec := doEnrollmentRequest(t, h, http.MethodPost, "/enrollment-tokens", `{"mode":"fully_managed"}`, &claims)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 body=%q", rec.Code, rec.Body.String())
	}
	if svc.issueCalls != 1 {
		t.Fatalf("svc.IssueToken 呼び出し回数 = %d; want 1", svc.issueCalls)
	}
	// actor / tenantID は claims から取得して Service へ引数で渡される（own-tenant 境界 / design Components）。
	if svc.issueTenant != tenantID {
		t.Errorf("svc.IssueToken tenantID = %v; want %v", svc.issueTenant, tenantID)
	}
	if svc.issueActor != claims.AdminUserID {
		t.Errorf("svc.IssueToken actor = %v; want %v", svc.issueActor, claims.AdminUserID)
	}
	if svc.issueIn.Mode != ModeFullyManaged {
		t.Errorf("svc.IssueToken in.Mode = %q; want %q", svc.issueIn.Mode, ModeFullyManaged)
	}
	var view TokenView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode TokenView: %v body=%q", err, rec.Body.String())
	}
	if view.ID != tokenID || view.Value != "secret-token-value" {
		t.Errorf("response TokenView = %+v; want ID=%v Value=secret-token-value", view, tokenID)
	}
}

// ---- (3) Operator の POST → 200（Req 2.1 / 正常系） ----

func TestHandler_Issue_Operator_Returns200(t *testing.T) {
	// Arrange
	tenantID := uuid.New()
	svc := &fakeHandlerService{issueView: TokenView{ID: uuid.New(), Mode: ModeDedicated}}
	h := NewHandler(svc, authz.New(), &fakeLogger{})
	claims := newEnrollmentClaims(tenantID, "Operator")

	// Act
	rec := doEnrollmentRequest(t, h, http.MethodPost, "/enrollment-tokens", `{"mode":"dedicated","policy_id":"`+uuid.New().String()+`"}`, &claims)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 body=%q", rec.Code, rec.Body.String())
	}
	if svc.issueCalls != 1 {
		t.Fatalf("svc.IssueToken 呼び出し回数 = %d; want 1", svc.issueCalls)
	}
}

// ---- (4) 越境（他テナント policy 指定）→ 非露出 404 + own-tenant 境界（Req 2.3） ----

func TestHandler_Issue_CrossTenantPolicy_Returns404NonExposing(t *testing.T) {
	// Arrange: Service（policyChecker 経由）が越境 policy を存在差非露出の NotFound（404）へ倒す。
	tenantID := uuid.New()
	crossTenantPolicyID := uuid.New()
	svc := &fakeHandlerService{issueErr: pkgerrors.New(pkgerrors.CodeNotFound, "policy not found")}
	h := NewHandler(svc, authz.New(), &fakeLogger{})
	claims := newEnrollmentClaims(tenantID, "TenantAdmin")

	// Act: 他テナントの policy_id を指定して DEDICATED 発行を試みる。
	rec := doEnrollmentRequest(t, h, http.MethodPost, "/enrollment-tokens",
		`{"mode":"dedicated","policy_id":"`+crossTenantPolicyID.String()+`"}`, &claims)

	// Assert
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404 body=%q", rec.Code, rec.Body.String())
	}
	if code := decodeEnrollErrCode(t, rec); code != string(pkgerrors.CodeNotFound) {
		t.Errorf("body.Code = %q; want %q", code, pkgerrors.CodeNotFound)
	}
	// 存在差非露出（Req 2.3）: body に対象 policy id を露出しない。
	if got := rec.Body.String(); strings.Contains(got, crossTenantPolicyID.String()) {
		t.Errorf("404 body に越境 policy id が露出している: %q", got)
	}
	// own-tenant 境界（Req 2.3）: Service へは claims 由来の自テナントのみが渡り、越境テナントは渡らない。
	if svc.issueTenant != tenantID {
		t.Errorf("svc.IssueToken tenantID = %v; want claims 由来の %v（越境テナントを渡してはならない）", svc.issueTenant, tenantID)
	}
}

// ---- (5) TenantAdmin の GET 一覧 → 200 + expires_at 由来 status + 自テナント scoped（Req 4.1） ----

func TestHandler_List_ReturnsExpiresAtDerivedStatus(t *testing.T) {
	// Arrange: Service が active / expired 派生済みの summary を返す（status 派生は Service.ListTokens の責務）。
	tenantID := uuid.New()
	svc := &fakeHandlerService{listOut: []TokenSummary{
		{ID: uuid.New(), Mode: ModeFullyManaged, Status: StatusActive},
		{ID: uuid.New(), Mode: ModeDedicated, Status: StatusExpired},
	}}
	h := NewHandler(svc, authz.New(), &fakeLogger{})
	claims := newEnrollmentClaims(tenantID, "TenantAdmin")

	// Act
	rec := doEnrollmentRequest(t, h, http.MethodGet, "/enrollment-tokens", "", &claims)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 body=%q", rec.Code, rec.Body.String())
	}
	if svc.listCalls != 1 {
		t.Fatalf("svc.ListTokens 呼び出し回数 = %d; want 1", svc.listCalls)
	}
	// 自テナント scoped: Service へ claims.TenantID が渡される（RLS に分離を委ねる前提 / Req 4.1）。
	if svc.listTenant != tenantID {
		t.Errorf("svc.ListTokens tenantID = %v; want %v", svc.listTenant, tenantID)
	}
	var summaries []TokenSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &summaries); err != nil {
		t.Fatalf("decode []TokenSummary: %v body=%q", err, rec.Body.String())
	}
	if len(summaries) != 2 {
		t.Fatalf("summary 件数 = %d; want 2", len(summaries))
	}
	// expires_at 由来 status（active / expired）が応答に写像される（Req 4.1）。
	if summaries[0].Status != StatusActive || summaries[1].Status != StatusExpired {
		t.Errorf("status = [%q, %q]; want [active, expired]", summaries[0].Status, summaries[1].Status)
	}
}

// ---- (6) malformed JSON body の POST → 400（入力検証契約 / svc.IssueToken 未呼出） ----

func TestHandler_Issue_MalformedJSON_Returns400(t *testing.T) {
	// Arrange
	tenantID := uuid.New()
	svc := &fakeHandlerService{}
	h := NewHandler(svc, authz.New(), &fakeLogger{})
	claims := newEnrollmentClaims(tenantID, "TenantAdmin")

	// Act: 壊れた JSON。
	rec := doEnrollmentRequest(t, h, http.MethodPost, "/enrollment-tokens", `{"mode":`, &claims)

	// Assert
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 body=%q", rec.Code, rec.Body.String())
	}
	if svc.issueCalls != 0 {
		t.Errorf("malformed body で svc.IssueToken が呼ばれてはならない（呼び出し回数=%d）", svc.issueCalls)
	}
	if code := decodeEnrollErrCode(t, rec); code != string(pkgerrors.CodeInvalidRequest) {
		t.Errorf("body.Code = %q; want %q", code, pkgerrors.CodeInvalidRequest)
	}
}

// ---- (7) claims 不在で 401（防御的 / Req 2.1 周辺の未認証ガード） ----

func TestHandler_Issue_NoClaims_Returns401(t *testing.T) {
	// Arrange
	svc := &fakeHandlerService{}
	log := &fakeLogger{}
	h := NewHandler(svc, authz.New(), log)

	// Act（claims を注入しない）
	rec := doEnrollmentRequest(t, h, http.MethodPost, "/enrollment-tokens", `{"mode":"fully_managed"}`, nil)

	// Assert
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401 body=%q", rec.Code, rec.Body.String())
	}
	if svc.issueCalls != 0 {
		t.Errorf("401 で svc.IssueToken が呼ばれてはならない（呼び出し回数=%d）", svc.issueCalls)
	}
	if code := decodeEnrollErrCode(t, rec); code != string(pkgerrors.CodeUnauthenticated) {
		t.Errorf("body.Code = %q; want %q", code, pkgerrors.CodeUnauthenticated)
	}
	if !hasDenyWarn(log, "enrollment operation denied", "missing auth claims") {
		t.Errorf("claims 不在経路で deny_reason=missing auth claims の WARN ログが無い; entries=%+v", log.snapshot())
	}
}
