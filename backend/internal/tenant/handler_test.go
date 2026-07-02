package tenant

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/httpserver"
)

// ---- Handler テスト用の固定文字列（存在露出 / 機密値 assertion 用） ----

const (
	testHandlerTenantName     = "Acme Corp"
	testHandlerSignupURLValue = "https://enterprise.google.com/signup?token=DO-NOT-LEAK"
	testHandlerSignupURLName  = "signupUrls/abc123"
)

// ---- Fake Service（Handler テスト用の Service interface テストダブル） ----

type fakeServiceCalls struct {
	create     int
	bind       int
	disable    int
	get        int
	list       int
	enterprise int
	recover    int
}

// fakeTenantService は tenant.Service を満たすテストダブル。
// 各メソッドの返り値を設定でき、呼出引数（特に actor）を記録する。
type fakeTenantService struct {
	mu sync.Mutex

	// 設定された挙動
	createView   TenantView
	createSignup SignupURL
	createErr    error

	bindView TenantView
	bindErr  error

	disableView TenantView
	disableErr  error

	getView TenantView
	getErr  error

	listViews []TenantView
	listErr   error

	recoverCount int
	recoverErr   error

	// 記録された入力
	calls           fakeServiceCalls
	lastActor       uuid.UUID
	lastCreateIn    CreateInput
	lastBindID      uuid.UUID
	lastBindIn      BindInput
	lastDisableID   uuid.UUID
	lastDisableIn   DisableInput
	lastGetID       uuid.UUID
	lastRecoverThan time.Duration
}

func (f *fakeTenantService) Create(_ context.Context, actor uuid.UUID, in CreateInput) (TenantView, SignupURL, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls.create++
	f.lastActor = actor
	f.lastCreateIn = in
	if f.createErr != nil {
		return TenantView{}, SignupURL{}, f.createErr
	}
	return f.createView, f.createSignup, nil
}

func (f *fakeTenantService) Bind(_ context.Context, actor uuid.UUID, id uuid.UUID, in BindInput) (TenantView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls.bind++
	f.lastActor = actor
	f.lastBindID = id
	f.lastBindIn = in
	if f.bindErr != nil {
		return TenantView{}, f.bindErr
	}
	return f.bindView, nil
}

func (f *fakeTenantService) Disable(_ context.Context, actor uuid.UUID, id uuid.UUID, in DisableInput) (TenantView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls.disable++
	f.lastActor = actor
	f.lastDisableID = id
	f.lastDisableIn = in
	if f.disableErr != nil {
		return TenantView{}, f.disableErr
	}
	return f.disableView, nil
}

func (f *fakeTenantService) Get(_ context.Context, id uuid.UUID) (TenantView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls.get++
	f.lastGetID = id
	if f.getErr != nil {
		return TenantView{}, f.getErr
	}
	return f.getView, nil
}

func (f *fakeTenantService) List(_ context.Context) ([]TenantView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls.list++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listViews, nil
}

func (f *fakeTenantService) EnterpriseNameForTenant(_ context.Context, _ uuid.UUID) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls.enterprise++
	return "", nil
}

func (f *fakeTenantService) RecoverStaleBindings(_ context.Context, actor uuid.UUID, olderThan time.Duration) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls.recover++
	f.lastActor = actor
	f.lastRecoverThan = olderThan
	if f.recoverErr != nil {
		return 0, f.recoverErr
	}
	return f.recoverCount, nil
}

// 型 assertion: fakeTenantService が Service interface を満たすことを compile-time で確認する。
var _ Service = (*fakeTenantService)(nil)

// ---- Test helpers ----

// newTestHandlerRouter は tenant.Handler を Mount した chi router を構築する。
// admin chain は Mount 呼び出し側（Routers.Admin）の責務のため、本テストでは Mount 後の
// route 解決と Handler の入出力契約のみを検証する（認可ガード継承は task 7 の結合テストで担保）。
func newTestHandlerRouter(t *testing.T, svc Service) chi.Router {
	t.Helper()
	r := chi.NewRouter()
	h := NewHandler(svc, &fakeLogger{})
	h.Mount(r)
	return r
}

// withActor は request に AuthClaims を注入し、admin chain 通過後の状態を模す。
// Handler は actor を AdminUserID から取得するため、actor 伝播の検証に用いる。
func withActor(req *http.Request, actor uuid.UUID) *http.Request {
	ctx := httpserver.WithAuthClaims(req.Context(), httpserver.AuthClaims{
		AdminUserID:  actor,
		IsSuperAdmin: true,
		Console:      "admin-console",
	})
	return req.WithContext(ctx)
}

// ============================================================================
// POST /tenants: name 空 JSON → 400（Req 1.3）
// ============================================================================

func TestCreate_EmptyName_Returns400(t *testing.T) {
	// Arrange: Service が name 空に対して CodeInvalidRequest を返す（Service 委譲を模す）。
	fake := &fakeTenantService{
		createErr: pkgerrors.New(pkgerrors.CodeInvalidRequest, "tenant name is required"),
	}
	r := newTestHandlerRouter(t, fake)

	// Act
	req := httptest.NewRequest(http.MethodPost, "/tenants", strings.NewReader(`{"name":""}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d", rec.Code)
	}
	if fake.calls.create != 1 {
		t.Errorf("Create call count: want 1, got %d", fake.calls.create)
	}
	body := rec.Body.String()
	if !strings.Contains(body, string(pkgerrors.CodeInvalidRequest)) {
		t.Errorf("body should contain code=%s, got: %s", pkgerrors.CodeInvalidRequest, body)
	}
}

// ============================================================================
// POST /tenants: JSON 不正 → 400（Service を呼ばずに Handler 入口で 400）
// ============================================================================

func TestCreate_MalformedJSON_Returns400AndDoesNotCallService(t *testing.T) {
	// Arrange
	fake := &fakeTenantService{}
	r := newTestHandlerRouter(t, fake)

	// Act: 壊れた JSON
	req := httptest.NewRequest(http.MethodPost, "/tenants", strings.NewReader(`{"name":`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d", rec.Code)
	}
	if fake.calls.create != 0 {
		t.Errorf("Create should NOT be called on malformed JSON, but called %d times", fake.calls.create)
	}
}

// ============================================================================
// POST /tenants: 先頭 JSON 値の後に後続トークンが続く body → 400（decodeJSON / #51 round4）
// ============================================================================

func TestCreate_TrailingTokensAfterJSON_Returns400AndDoesNotCallService(t *testing.T) {
	// Arrange
	fake := &fakeTenantService{}
	r := newTestHandlerRouter(t, fake)

	// Act: 先頭の単一 JSON object の後に余分なトークンが続く（単一 document ではない）。
	req := httptest.NewRequest(http.MethodPost, "/tenants", strings.NewReader(`{"name":"x"} trailing`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Assert: malformed として Handler 入口で 400、Service は呼ばれない。
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d", rec.Code)
	}
	if fake.calls.create != 0 {
		t.Errorf("Create should NOT be called on trailing tokens, but called %d times", fake.calls.create)
	}
}

// ============================================================================
// POST /tenants: 正常 → 201 + status=pending_bind + signup_url（Req 1.1 シリアライズ）
// ============================================================================

func TestCreate_Success_ReturnsPendingBindAndSignupURL(t *testing.T) {
	// Arrange
	id := uuid.New()
	actor := uuid.New()
	fake := &fakeTenantService{
		createView: TenantView{
			ID:     id,
			Name:   testHandlerTenantName,
			Status: StatusPendingBind,
		},
		createSignup: SignupURL{
			URL:  testHandlerSignupURLValue,
			Name: testHandlerSignupURLName,
		},
	}
	r := newTestHandlerRouter(t, fake)

	// Act
	req := httptest.NewRequest(http.MethodPost, "/tenants", strings.NewReader(`{"name":"Acme Corp"}`))
	req = withActor(req, actor)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Assert: 201 + JSON に status=pending_bind と signup_url が含まれる
	if rec.Code != http.StatusCreated {
		t.Fatalf("status: want 201, got %d", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v (body=%s)", err, rec.Body.String())
	}
	if resp["status"] != string(StatusPendingBind) {
		t.Errorf("status: want %q, got %v", StatusPendingBind, resp["status"])
	}
	if resp["signup_url"] != testHandlerSignupURLValue {
		t.Errorf("signup_url: want %q, got %v", testHandlerSignupURLValue, resp["signup_url"])
	}
	if resp["id"] != id.String() {
		t.Errorf("id: want %q, got %v", id.String(), resp["id"])
	}
	// #52: create 応答から signup_url_name を除去した（Req 3.2）。signup_url_name は create 時に
	// 発行元テナントへ永続化される正本となり、後続 bind は body から受け取らないため、応答へ
	// 載せる必要がなくなった（admin がコピペで別テナントの値を bind body へ渡す導線を排除）。
	// 応答に signup_url_name キーが含まれないことを検証する。
	if _, ok := resp["signup_url_name"]; ok {
		t.Errorf("create 応答に signup_url_name が含まれてはならない（Req 3.2）: got %v", resp["signup_url_name"])
	}
	// actor が AuthClaims.AdminUserID から Service へ伝播していること。
	if fake.lastActor != actor {
		t.Errorf("actor: want %s, got %s", actor, fake.lastActor)
	}
}

// ============================================================================
// GET /tenants/{id}: 不在 → 404 で body にテナント存在差を露出しない（Req 6.5）
// ============================================================================

func TestGet_NotFound_Returns404WithoutExistenceLeak(t *testing.T) {
	// Arrange: Service が固定 message の ErrTenantNotFound を返す。
	fake := &fakeTenantService{
		getErr: ErrTenantNotFound,
	}
	r := newTestHandlerRouter(t, fake)
	missingID := uuid.New()

	// Act
	req := httptest.NewRequest(http.MethodGet, "/tenants/"+missingID.String(), nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Assert: 404 + body は固定 message で対象 ID を露出しない
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: want 404, got %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, missingID.String()) {
		t.Errorf("404 body must not leak target tenant id, got: %s", body)
	}
	// body は ErrTenantNotFound の固定 message を含む（存在差非露出の汎用文言 / Req 6.5）。
	if !strings.Contains(body, "tenant not found") {
		t.Errorf("404 body should contain fixed message, got: %s", body)
	}
	if fake.calls.get != 1 {
		t.Errorf("Get call count: want 1, got %d", fake.calls.get)
	}
}

// ============================================================================
// GET /tenants/{id}: 不正な UUID → 400（Service を呼ばずに Handler 入口で 400）
// ============================================================================

func TestGet_InvalidUUID_Returns400AndDoesNotCallService(t *testing.T) {
	// Arrange
	fake := &fakeTenantService{}
	r := newTestHandlerRouter(t, fake)

	// Act: UUID として不正な path param
	req := httptest.NewRequest(http.MethodGet, "/tenants/not-a-uuid", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d", rec.Code)
	}
	if fake.calls.get != 0 {
		t.Errorf("Get should NOT be called on invalid UUID, but called %d times", fake.calls.get)
	}
}

// ============================================================================
// GET /tenants/{id}: 正常 → 200 + TenantView（Req 4.2）
// ============================================================================

func TestGet_Success_ReturnsTenantView(t *testing.T) {
	// Arrange
	id := uuid.New()
	fake := &fakeTenantService{
		getView: TenantView{
			ID:             id,
			Name:           testHandlerTenantName,
			Status:         StatusBound,
			EnterpriseName: "enterprises/LC0123456789",
		},
	}
	r := newTestHandlerRouter(t, fake)

	// Act
	req := httptest.NewRequest(http.MethodGet, "/tenants/"+id.String(), nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	var view TenantView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("response is not valid TenantView JSON: %v", err)
	}
	if view.ID != id || view.Status != StatusBound {
		t.Errorf("view: want id=%s status=bound, got id=%s status=%s", id, view.ID, view.Status)
	}
	if fake.lastGetID != id {
		t.Errorf("Get id: want %s, got %s", id, fake.lastGetID)
	}
}

// ============================================================================
// GET /tenants: 0 件 → 200 + 空配列（Req 4.1 / 4.4）
// ============================================================================

func TestList_Empty_Returns200WithEmptyArray(t *testing.T) {
	// Arrange: 0 件（非 nil 空 slice）
	fake := &fakeTenantService{listViews: []TenantView{}}
	r := newTestHandlerRouter(t, fake)

	// Act
	req := httptest.NewRequest(http.MethodGet, "/tenants", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	// 空配列 `[]` であり null ではないこと。
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("body: want [], got %s", got)
	}
	if fake.calls.list != 1 {
		t.Errorf("List call count: want 1, got %d", fake.calls.list)
	}
}

// ============================================================================
// DELETE /tenants/{id}: 確認テキスト欠落 → Service で 422 写像（Req 3.2）
// ============================================================================

func TestDisable_ConfirmationMissing_Returns422(t *testing.T) {
	// Arrange: Service が確認未完了で ErrConfirmationRequired（CodeBusinessRule=422）を返す。
	fake := &fakeTenantService{
		disableErr: ErrConfirmationRequired,
	}
	r := newTestHandlerRouter(t, fake)
	id := uuid.New()

	// Act: confirmation 欠落（空 body）。Handler は decode して Service へ委譲する。
	req := httptest.NewRequest(http.MethodDelete, "/tenants/"+id.String(), strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Assert: 422
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: want 422, got %d", rec.Code)
	}
	if fake.calls.disable != 1 {
		t.Errorf("Disable call count: want 1, got %d", fake.calls.disable)
	}
}

// ============================================================================
// DELETE /tenants/{id}: 空 body（確認テキスト未入力）→ Handler 入口で 400 にせず
// Service の確認未完了判定（422）へ委ねる（Req 3.2 / 境界値）
// ============================================================================

func TestDisable_EmptyBody_Returns422NotBadRequest(t *testing.T) {
	// Arrange: Service は確認未完了で ErrConfirmationRequired（422）を返す。
	fake := &fakeTenantService{
		disableErr: ErrConfirmationRequired,
	}
	r := newTestHandlerRouter(t, fake)
	id := uuid.New()

	// Act: body 無し（nil）。Handler は空 body を 400 にせず Service へ委譲する。
	req := httptest.NewRequest(http.MethodDelete, "/tenants/"+id.String(), nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Assert: 400 ではなく 422（Service の確認未完了判定）。
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: want 422, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if fake.calls.disable != 1 {
		t.Errorf("Disable must be called once on empty body, got %d", fake.calls.disable)
	}
}

// ============================================================================
// DELETE /tenants/{id}: malformed JSON → Handler 入口で 400（Service を呼ばない）
// ============================================================================

func TestDisable_MalformedJSON_Returns400AndDoesNotCallService(t *testing.T) {
	// Arrange
	fake := &fakeTenantService{}
	r := newTestHandlerRouter(t, fake)
	id := uuid.New()

	// Act: 壊れた JSON は空 body と区別され 400 となる。
	req := httptest.NewRequest(http.MethodDelete, "/tenants/"+id.String(), strings.NewReader(`{"confirmation":`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d", rec.Code)
	}
	if fake.calls.disable != 0 {
		t.Errorf("Disable should NOT be called on malformed JSON, but called %d times", fake.calls.disable)
	}
}

// ============================================================================
// DELETE /tenants/{id}: 先頭 JSON 値の後に後続トークンが続く body → 400
// （decodeJSONAllowEmpty / #51 round4）
// ============================================================================

func TestDisable_TrailingTokensAfterJSON_Returns400AndDoesNotCallService(t *testing.T) {
	// Arrange
	fake := &fakeTenantService{}
	r := newTestHandlerRouter(t, fake)
	id := uuid.New()

	// Act: 空 body は許容するが、後続トークンを伴う body は malformed として 400。
	req := httptest.NewRequest(http.MethodDelete, "/tenants/"+id.String(), strings.NewReader(`{"confirmation":"x"} trailing`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d", rec.Code)
	}
	if fake.calls.disable != 0 {
		t.Errorf("Disable should NOT be called on trailing tokens, but called %d times", fake.calls.disable)
	}
}

// ============================================================================
// DELETE /tenants/{id}: 正常 → 204 No Content（design.md API Contract）
// ============================================================================

func TestDisable_Success_Returns204NoContent(t *testing.T) {
	// Arrange
	id := uuid.New()
	actor := uuid.New()
	fake := &fakeTenantService{
		disableView: TenantView{ID: id, Name: testHandlerTenantName, Status: StatusDisabled},
	}
	r := newTestHandlerRouter(t, fake)

	// Act
	req := httptest.NewRequest(http.MethodDelete, "/tenants/"+id.String(), strings.NewReader(`{"confirmation":"Acme Corp"}`))
	req = withActor(req, actor)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Assert: 204 + body 無し
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: want 204, got %d", rec.Code)
	}
	if body := rec.Body.String(); body != "" {
		t.Errorf("204 response must have empty body, got: %s", body)
	}
	if fake.lastActor != actor {
		t.Errorf("actor: want %s, got %s", actor, fake.lastActor)
	}
	if fake.lastDisableID != id {
		t.Errorf("Disable id: want %s, got %s", id, fake.lastDisableID)
	}
}

// ============================================================================
// POST /tenants/{id}/bind: 正常 → 200 + bound TenantView（Req 2.x シリアライズ）
// #52: body の signup_url_name を無視し永続値経路で動く（Req 3.3）
// ============================================================================

func TestBind_Success_ReturnsBoundView(t *testing.T) {
	// Arrange
	id := uuid.New()
	fake := &fakeTenantService{
		bindView: TenantView{
			ID:             id,
			Name:           testHandlerTenantName,
			Status:         StatusBound,
			EnterpriseName: "enterprises/LC0123456789",
		},
	}
	r := newTestHandlerRouter(t, fake)

	// Act: body に余分な signup_url_name を渡しても Handler は無視する（Req 3.3）。
	req := httptest.NewRequest(http.MethodPost, "/tenants/"+id.String()+"/bind", strings.NewReader(`{"signup_url_name":"signupUrls/OTHER-TENANT"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Assert: 永続値経路で bind が成功し bound を返す。
	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	var view TenantView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("response is not valid TenantView JSON: %v", err)
	}
	if view.Status != StatusBound {
		t.Errorf("status: want bound, got %s", view.Status)
	}
	if fake.lastBindID != id {
		t.Errorf("Bind id: want %s, got %s", id, fake.lastBindID)
	}
	// BindInput は空 struct であり、body の signup_url_name は Service へ渡らない（Req 3.3）。
	if fake.calls.bind != 1 {
		t.Errorf("Bind call count: want 1, got %d", fake.calls.bind)
	}
}

// #52: bind は空 body でも動く（body から signup_url_name を読まない / Req 3.3）。
func TestBind_EmptyBody_UsesPersistedSignupURLName(t *testing.T) {
	// Arrange
	id := uuid.New()
	fake := &fakeTenantService{
		bindView: TenantView{
			ID:             id,
			Name:           testHandlerTenantName,
			Status:         StatusBound,
			EnterpriseName: "enterprises/LC0123456789",
		},
	}
	r := newTestHandlerRouter(t, fake)

	// Act: body 無し（signup_url_name を渡さない）で bind する。
	req := httptest.NewRequest(http.MethodPost, "/tenants/"+id.String()+"/bind", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Assert: 空 body でも 400 にならず bind が成立する（永続値経路 / Req 3.3）。
	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if fake.calls.bind != 1 {
		t.Errorf("Bind call count: want 1, got %d", fake.calls.bind)
	}
	if fake.lastBindID != id {
		t.Errorf("Bind id: want %s, got %s", id, fake.lastBindID)
	}
}

// ============================================================================
// POST /tenants/recover-bindings: 回収件数 JSON を返す（Req 2.1）
// ============================================================================

func TestRecoverBindings_ReturnsRecoveredCount(t *testing.T) {
	// Arrange: fake が recovered=3 を返す設定。
	actor := uuid.New()
	fake := &fakeTenantService{recoverCount: 3}
	r := newTestHandlerRouter(t, fake)

	// Act: body 不要の POST。
	req := httptest.NewRequest(http.MethodPost, "/tenants/recover-bindings", nil)
	req = withActor(req, actor)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Assert: 200 + {"recovered":3}。
	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response is not valid JSON: %v (body=%s)", err, rec.Body.String())
	}
	if resp["recovered"] != float64(3) {
		t.Errorf("recovered: want 3, got %v", resp["recovered"])
	}
	if fake.calls.recover != 1 {
		t.Errorf("RecoverStaleBindings call count: want 1, got %d", fake.calls.recover)
	}
	// actor が AuthClaims.AdminUserID から Service へ伝播していること。
	if fake.lastActor != actor {
		t.Errorf("actor: want %s, got %s", actor, fake.lastActor)
	}
	// olderThan は既定しきい値が渡ること（handler は body を読まず既定値を使う）。
	if fake.lastRecoverThan != defaultStaleBindingThreshold {
		t.Errorf("olderThan: want %s, got %s", defaultStaleBindingThreshold, fake.lastRecoverThan)
	}
}

// #52: recover-bindings が Service エラー時に Code 写像で応答すること（Req 2.1 / 503 経路）。
func TestRecoverBindings_ServiceError_MapsToHTTPStatus(t *testing.T) {
	// Arrange: Service が Unavailable（DB 失敗）を返す。
	fake := &fakeTenantService{recoverErr: pkgerrors.New(pkgerrors.CodeUnavailable, "database unavailable")}
	r := newTestHandlerRouter(t, fake)

	// Act
	req := httptest.NewRequest(http.MethodPost, "/tenants/recover-bindings", nil)
	req = withActor(req, uuid.New())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Assert: 200 を返さず errors.WriteHTTP が Code → HTTP status へ写像する（503）。
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: want 503, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if fake.calls.recover != 1 {
		t.Errorf("RecoverStaleBindings call count: want 1, got %d", fake.calls.recover)
	}
}

// ============================================================================
// Mount された 6 route が解決され Handler に到達すること（Req 6.1 の mount 確認 / #52 recover 追加）
// ============================================================================

func TestMount_RegistersAllEndpoints(t *testing.T) {
	// Arrange: 全メソッドが正常応答を返す fake Service。
	id := uuid.New()
	fake := &fakeTenantService{
		createView:   TenantView{ID: id, Name: testHandlerTenantName, Status: StatusPendingBind},
		createSignup: SignupURL{URL: testHandlerSignupURLValue, Name: testHandlerSignupURLName},
		bindView:     TenantView{ID: id, Name: testHandlerTenantName, Status: StatusBound},
		getView:      TenantView{ID: id, Name: testHandlerTenantName, Status: StatusPendingBind},
		listViews:    []TenantView{},
		disableView:  TenantView{ID: id, Name: testHandlerTenantName, Status: StatusDisabled},
		recoverCount: 0,
	}
	r := newTestHandlerRouter(t, fake)

	// Act/Assert: 6 endpoint が 404 でなく Handler に到達し、想定の成功 status を返すこと。
	// recover-bindings（静的 route）が /{id}/bind（path param）と衝突せず解決することも確認する。
	cases := []struct {
		name     string
		method   string
		path     string
		body     string
		wantCode int
	}{
		{"create", http.MethodPost, "/tenants", `{"name":"Acme Corp"}`, http.StatusCreated},
		{"list", http.MethodGet, "/tenants", "", http.StatusOK},
		{"recover", http.MethodPost, "/tenants/recover-bindings", "", http.StatusOK},
		{"get", http.MethodGet, "/tenants/" + id.String(), "", http.StatusOK},
		{"bind", http.MethodPost, "/tenants/" + id.String() + "/bind", "", http.StatusOK},
		{"disable", http.MethodDelete, "/tenants/" + id.String(), `{"confirmation":"Acme Corp"}`, http.StatusNoContent},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var req *http.Request
			if tc.body == "" {
				req = httptest.NewRequest(tc.method, tc.path, nil)
			} else {
				req = httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code == http.StatusNotFound {
				t.Fatalf("%s %s did not resolve to a handler (got 404)", tc.method, tc.path)
			}
			if rec.Code != tc.wantCode {
				t.Errorf("status: want %d, got %d", tc.wantCode, rec.Code)
			}
		})
	}
}
