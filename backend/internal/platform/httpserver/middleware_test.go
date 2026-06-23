package httpserver

import (
	"context"
	"encoding/json"
	stdErrors "errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	internalerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
)

// newTestLogger は logger.Logger を NewLogger 経由で生成する helper。
// json / stderr / info の組み合わせは本テストで観測しないため、I/O は副作用として
// stderr に出るが test 検証には用いない（観測すべき値は別途 fakeLogger 経由で取る）。
func newTestLogger(t *testing.T) logger.Logger {
	t.Helper()
	l, err := logger.NewLogger(config.Config{LogLevel: "error", LogFormat: "json", LogOutput: "stderr"})
	if err != nil {
		t.Fatalf("newTestLogger: %v", err)
	}
	return l
}

// fakeLogger は logger.Logger interface を満たすテスト用 spy。
// AccessLog の Info 呼び出しを観測するためにフィールドキャプチャを保持する。
type fakeLogger struct {
	infoCalls  []logCall
	warnCalls  []logCall
	errorCalls []logCall
}

type logCall struct {
	msg    string
	fields []any
}

func (f *fakeLogger) Debug(msg string, fields ...any) {}
func (f *fakeLogger) Info(msg string, fields ...any) {
	f.infoCalls = append(f.infoCalls, logCall{msg: msg, fields: fields})
}
func (f *fakeLogger) Warn(msg string, fields ...any) {
	f.warnCalls = append(f.warnCalls, logCall{msg: msg, fields: fields})
}
func (f *fakeLogger) Error(msg string, fields ...any) {
	f.errorCalls = append(f.errorCalls, logCall{msg: msg, fields: fields})
}
func (f *fakeLogger) With(_ ...any) logger.Logger { return f }
func (f *fakeLogger) Sync() error                 { return nil }

// fieldValue は fields []any（"key", value, "key", value ... の列）から
// 指定 key の値を取り出すテスト helper。
func fieldValue(fields []any, key string) (any, bool) {
	for i := 0; i+1 < len(fields); i += 2 {
		if k, ok := fields[i].(string); ok && k == key {
			return fields[i+1], true
		}
	}
	return nil, false
}

// TestRecoverer_PanicBecomes500_WithErrorLog は requirements.md Req 5.6（recover middleware
// 登録）と design.md「panic を recover した場合 errors.WriteHTTP 経由で 500 を返し
// ERROR ログを出す」契約に対応する。
//
// 一般的な panic（独自 Error 型でない）は CodeInternal で wrap され 500 になり、
// ERROR ログが 1 回出る。
func TestRecoverer_PanicBecomes500_WithErrorLog(t *testing.T) {
	// Arrange
	log := &fakeLogger{}
	handler := Recoverer(log)(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)

	// Act
	handler.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500", rec.Code)
	}
	if len(log.errorCalls) != 1 {
		t.Fatalf("ERROR log は 1 回出ること; got %d", len(log.errorCalls))
	}
	var body struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	if body.Code != string(internalerrors.CodeInternal) {
		t.Errorf("body.Code = %q; want %q", body.Code, internalerrors.CodeInternal)
	}
}

// TestRecoverer_PanicWithDomainErrorTenantCtxMissing_MapsTo500 は task 2 申し送りの
// contract verify。BeginTxFunc が panic する `*errors.Error{Code: CodeTenantCtxMissing}`
// payload を recover middleware が WriteHTTP 経由で 500 + 構造化 ERROR ログにマッピング
// することを確認する。
func TestRecoverer_PanicWithDomainErrorTenantCtxMissing_MapsTo500(t *testing.T) {
	// Arrange
	log := &fakeLogger{}
	handler := Recoverer(log)(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic(internalerrors.New(internalerrors.CodeTenantCtxMissing, "tenant context missing"))
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)

	// Act
	handler.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500", rec.Code)
	}
	if len(log.errorCalls) != 1 {
		t.Fatalf("ERROR log は 1 回出ること; got %d", len(log.errorCalls))
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	if body.Code != string(internalerrors.CodeTenantCtxMissing) {
		t.Errorf("body.Code = %q; want %q", body.Code, internalerrors.CodeTenantCtxMissing)
	}
}

// TestRecoverer_PanicWithDomainErrorForbidden_MapsTo403 は recover が *errors.Error
// payload をそのまま WriteHTTP に渡し、Code に応じた status code（403）を発行することを
// 確認する。
func TestRecoverer_PanicWithDomainErrorForbidden_MapsTo403(t *testing.T) {
	// Arrange
	log := &fakeLogger{}
	handler := Recoverer(log)(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic(internalerrors.New(internalerrors.CodeForbidden, "no"))
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)

	// Act
	handler.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403", rec.Code)
	}
	if len(log.warnCalls) != 1 {
		t.Fatalf("WARN log は 1 回出ること（4xx）; got %d", len(log.warnCalls))
	}
}

// TestRecoverer_NoPanic_PassThrough は通常経路。panic が無ければ handler の応答が
// そのまま返り、ログは出ない。
func TestRecoverer_NoPanic_PassThrough(t *testing.T) {
	// Arrange
	log := &fakeLogger{}
	handler := Recoverer(log)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)

	// Act
	handler.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusTeapot {
		t.Fatalf("status = %d; want 418", rec.Code)
	}
	if len(log.errorCalls)+len(log.warnCalls)+len(log.infoCalls) != 0 {
		t.Errorf("no panic 時はログ出力なし; calls = %+v", log)
	}
}

// TestRequestID_GeneratesUUIDWhenAbsent はリクエストヘッダ X-Request-ID が無い場合に
// 新規 uuid v4 が応答ヘッダと request context に設定されることを確認する。
func TestRequestID_GeneratesUUIDWhenAbsent(t *testing.T) {
	// Arrange
	var ctxID string
	handler := RequestID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctxID = RequestIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)

	// Act
	handler.ServeHTTP(rec, req)

	// Assert
	got := rec.Header().Get(requestIDHeader)
	if got == "" {
		t.Fatalf("X-Request-ID header が空")
	}
	if _, err := uuid.Parse(got); err != nil {
		t.Errorf("X-Request-ID は uuid v4 を期待; got %q (%v)", got, err)
	}
	if ctxID != got {
		t.Errorf("ctx の request_id = %q; header と一致を期待 (%q)", ctxID, got)
	}
}

// TestRequestID_ReusesIncomingHeader はクライアントが既に X-Request-ID を送っている場合
// にそれが応答ヘッダと ctx に貫通することを確認する（上流相関 ID の貫通）。
func TestRequestID_ReusesIncomingHeader(t *testing.T) {
	// Arrange
	const upstream = "upstream-correlation-id-12345"
	var ctxID string
	handler := RequestID()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctxID = RequestIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set(requestIDHeader, upstream)

	// Act
	handler.ServeHTTP(rec, req)

	// Assert
	if got := rec.Header().Get(requestIDHeader); got != upstream {
		t.Errorf("X-Request-ID header = %q; want %q", got, upstream)
	}
	if ctxID != upstream {
		t.Errorf("ctx の request_id = %q; want %q", ctxID, upstream)
	}
}

// TestAccessLog_EmitsInfoWithRequiredFields は requirements.md Req 5.6（構造化アクセスログ
// 登録）対応。method / path / status / duration_ms / request_id がフィールドに含まれる
// ことを確認する。
func TestAccessLog_EmitsInfoWithRequiredFields(t *testing.T) {
	// Arrange
	log := &fakeLogger{}
	// chain: RequestID → AccessLog → handler。request_id の貫通も同時に観測する。
	handler := RequestID()(AccessLog(log)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("created"))
	})))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/echo", nil)

	// Act
	handler.ServeHTTP(rec, req)

	// Assert
	if len(log.infoCalls) != 1 {
		t.Fatalf("Info ログは 1 回出ること; got %d", len(log.infoCalls))
	}
	call := log.infoCalls[0]
	if call.msg != "http_access" {
		t.Errorf("msg = %q; want %q", call.msg, "http_access")
	}
	if v, ok := fieldValue(call.fields, "method"); !ok || v != http.MethodPost {
		t.Errorf("method field 不正; got %v", v)
	}
	if v, ok := fieldValue(call.fields, "path"); !ok || v != "/echo" {
		t.Errorf("path field 不正; got %v", v)
	}
	if v, ok := fieldValue(call.fields, "status"); !ok || v != http.StatusCreated {
		t.Errorf("status field 不正; got %v", v)
	}
	if _, ok := fieldValue(call.fields, "duration_ms"); !ok {
		t.Errorf("duration_ms field が無い")
	}
	if v, ok := fieldValue(call.fields, "request_id"); !ok {
		t.Errorf("request_id field が無い")
	} else if s, _ := v.(string); s == "" {
		t.Errorf("request_id が空")
	}
}

// TestAccessLog_IncludesTenantIDWhenContextEstablished は TenantContext が ctx に
// 確立されていれば tenant_id field が access log に乗ることを確認する。
func TestAccessLog_IncludesTenantIDWhenContextEstablished(t *testing.T) {
	// Arrange
	log := &fakeLogger{}
	tenantID := uuid.New()
	tc := db.TenantContext{TenantID: tenantID, AdminUserID: uuid.New(), Roles: []string{"TenantAdmin"}}
	handler := AccessLog(log)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req = req.WithContext(db.WithTenantContext(context.Background(), tc))

	// Act
	handler.ServeHTTP(rec, req)

	// Assert
	if len(log.infoCalls) != 1 {
		t.Fatalf("Info ログは 1 回出ること; got %d", len(log.infoCalls))
	}
	v, ok := fieldValue(log.infoCalls[0].fields, "tenant_id")
	if !ok {
		t.Fatalf("tenant_id field が無い")
	}
	if s, _ := v.(string); s != tenantID.String() {
		t.Errorf("tenant_id = %v; want %s", v, tenantID.String())
	}
}

// TestAccessLog_NilLogger_NoPanic は log が nil でも handler を呼べることを確認する。
func TestAccessLog_NilLogger_NoPanic(t *testing.T) {
	// Arrange
	called := false
	handler := AccessLog(nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)

	// Act / Assert: panic しない
	handler.ServeHTTP(rec, req)
	if !called {
		t.Fatalf("handler が呼ばれていない")
	}
}

// TestTenantContextMiddleware_NoClaims_Returns401 は requirements.md Req 5.2 と
// design.md「tenant_id を持たないリクエストが `/api/...` に到達した場合 401 を返す」
// 契約に対応する。auth スタブの default deny 状態（claims 未注入）で
// TenantContextMiddleware が 401 を返して chain を終端することを確認する（テスト (g)）。
func TestTenantContextMiddleware_NoClaims_Returns401(t *testing.T) {
	// Arrange
	log := &fakeLogger{}
	nextCalled := false
	handler := TenantContextMiddleware(log)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/foo", nil)

	// Act
	handler.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401", rec.Code)
	}
	if nextCalled {
		t.Errorf("claims 不在で next が呼ばれてはならない")
	}
	if len(log.warnCalls) != 1 {
		t.Errorf("4xx は WARN ログを 1 回出すこと; got %d", len(log.warnCalls))
	}
	// body の code を確認
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	if body.Code != string(internalerrors.CodeUnauthenticated) {
		t.Errorf("body.Code = %q; want %q", body.Code, internalerrors.CodeUnauthenticated)
	}
}

// TestTenantContextMiddleware_WithClaims_PutsTenantContextOnCtx は claims が ctx に
// 注入された場合に TenantContext を組み立てて put し、next handler から
// db.FromContext で取り出せることを確認する。
func TestTenantContextMiddleware_WithClaims_PutsTenantContextOnCtx(t *testing.T) {
	// Arrange
	log := &fakeLogger{}
	tenantID := uuid.New()
	adminID := uuid.New()
	var observed db.TenantContext
	var observeErr error
	handler := TenantContextMiddleware(log)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed, observeErr = db.FromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/foo", nil)
	req = req.WithContext(withAuthClaims(req.Context(), authClaims{
		TenantID:     tenantID,
		AdminUserID:  adminID,
		Roles:        []string{"TenantAdmin"},
		IsSuperAdmin: false,
	}))

	// Act
	handler.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d; want 204", rec.Code)
	}
	if observeErr != nil {
		t.Fatalf("db.FromContext: %v", observeErr)
	}
	if observed.TenantID != tenantID {
		t.Errorf("TenantID = %v; want %v", observed.TenantID, tenantID)
	}
	if observed.AdminUserID != adminID {
		t.Errorf("AdminUserID = %v; want %v", observed.AdminUserID, adminID)
	}
	if observed.IsSuperAdmin {
		t.Errorf("IsSuperAdmin = true; want false")
	}
	if len(observed.Roles) != 1 || observed.Roles[0] != "TenantAdmin" {
		t.Errorf("Roles = %v; want [TenantAdmin]", observed.Roles)
	}
}

// TestTenantContextMiddleware_SuperAdminClaims_PassesIsSuperAdmin は SuperAdmin
// claims（TenantID=uuid.Nil, IsSuperAdmin=true）が TenantContext に正しく転記される
// ことを確認する。task 2 申し送り「SuperAdmin で TenantID==uuid.Nil を埋める middleware
// が cross-tenant 分岐に合流するよう、IsSuperAdmin=true を明示設定」と整合。
func TestTenantContextMiddleware_SuperAdminClaims_PassesIsSuperAdmin(t *testing.T) {
	// Arrange
	log := &fakeLogger{}
	var observed db.TenantContext
	handler := TenantContextMiddleware(log)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tc, err := db.FromContext(r.Context())
		if err != nil {
			t.Fatalf("FromContext: %v", err)
		}
		observed = tc
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/x", nil)
	req = req.WithContext(withAuthClaims(req.Context(), authClaims{
		TenantID:     uuid.Nil,
		AdminUserID:  uuid.New(),
		Roles:        []string{"SuperAdmin"},
		IsSuperAdmin: true,
	}))

	// Act
	handler.ServeHTTP(rec, req)

	// Assert
	if !observed.IsSuperAdmin {
		t.Errorf("IsSuperAdmin = false; want true")
	}
	if observed.TenantID != uuid.Nil {
		t.Errorf("TenantID = %v; want uuid.Nil", observed.TenantID)
	}
}

// TestRequestIDFromContext_Nil は nil ctx でも空文字を返し panic しないこと。
func TestRequestIDFromContext_Nil(t *testing.T) {
	if got := RequestIDFromContext(nil); got != "" {
		t.Errorf("got = %q; want empty", got)
	}
}

// TestNewTestLogger_Smoke は test helper 自体が壊れていないことの sanity check。
// 本テストは依存セットアップの検証のみで、AC との 1:1 対応は無い。
func TestNewTestLogger_Smoke(t *testing.T) {
	if l := newTestLogger(t); l == nil {
		t.Fatalf("newTestLogger returned nil")
	}
}

// Sanity: panicString helper が string / error 以外を「non-error panic value」と表示する。
// recover middleware の string 化フォーマットの安定性を確保する。
func TestPanicString_NonErrorValue(t *testing.T) {
	if got := panicString(123); got != "non-error panic value" {
		t.Errorf("got = %q; want non-error panic value", got)
	}
	if got := panicString("boom"); got != "boom" {
		t.Errorf("got = %q; want %q", got, "boom")
	}
	if got := panicString(stdErrors.New("wrapped")); !strings.Contains(got, "wrapped") {
		t.Errorf("got = %q; want contains 'wrapped'", got)
	}
}
