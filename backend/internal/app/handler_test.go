package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/authz"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/httpserver"
)

// ---- Test doubles ----

// fakeHandlerService は Service interface（4 メソッド）を満たす Handler テスト用 spy。各メソッドの
// 呼び出し回数・受信引数を記録し、任意の戻り値 / エラーを返せる。CheckAppsApproved は Handler が
// endpoint へ配線しない read seam であり、呼び出されないことを回数で検証するためだけに実装する。
type fakeHandlerService struct {
	playCalls  int
	playActor  uuid.UUID
	playTenant uuid.UUID
	playIn     PlayTokenRequest
	playView   PlayTokenView
	playErr    error

	listCalls  int
	listTenant uuid.UUID
	listOut    []TenantAppView
	listErr    error

	syncCalls  int
	syncActor  uuid.UUID
	syncTenant uuid.UUID
	syncIn     SyncRequest
	syncOut    SyncResult
	syncErr    error

	checkCalls int
}

func (s *fakeHandlerService) CreatePlayToken(_ context.Context, actor, tenantID uuid.UUID, in PlayTokenRequest) (PlayTokenView, error) {
	s.playCalls++
	s.playActor = actor
	s.playTenant = tenantID
	s.playIn = in
	return s.playView, s.playErr
}

func (s *fakeHandlerService) ListApps(_ context.Context, tenantID uuid.UUID) ([]TenantAppView, error) {
	s.listCalls++
	s.listTenant = tenantID
	return s.listOut, s.listErr
}

func (s *fakeHandlerService) SyncApps(_ context.Context, actor, tenantID uuid.UUID, in SyncRequest) (SyncResult, error) {
	s.syncCalls++
	s.syncActor = actor
	s.syncTenant = tenantID
	s.syncIn = in
	return s.syncOut, s.syncErr
}

func (s *fakeHandlerService) CheckAppsApproved(_ context.Context, _ uuid.UUID, _ []string) error {
	s.checkCalls++
	return nil
}

// hFakeLogger は logger.Logger を満たす WARN spy（policy/handler_test の hFakeLogger と同型）。
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

// leaks は全 WARN 呼び出しの msg と field 値（%v 展開）に substr が surface するかを返す。
// 秘匿値（webToken.Value）がログ経路へ載っていないことの検証に用いる（NFR 1.2）。
func (f *hFakeLogger) leaks(substr string) bool {
	for _, call := range f.warnCalls {
		if strings.Contains(call.msg, substr) {
			return true
		}
		for _, field := range call.fields {
			if strings.Contains(fmt.Sprintf("%v", field), substr) {
				return true
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

// doRequest は Handler を本番同様に `/api` chain へ `Mount` した上でリクエストを実行する helper。
// cmd/api の `appHandler.Mount(routers.API)` 配線（task 6 / design.md）を忠実に再現し、
// `/api/play-tokens` / `/api/apps` / `/api/apps/sync` への経路を検証する。claimsOrNil が nil なら
// claims を ctx に注入しない（401 経路）。
func doRequest(t *testing.T, h *Handler, method, target, body string, claimsOrNil *httpserver.AuthClaims) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	router.Route("/api", func(sub chi.Router) {
		h.Mount(sub)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
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

// ---- (a) POST /play-tokens: Viewer は app:read を持つため許可 → 200 + {value}（Req 1.1 / 4.1） ----

func TestHandler_CreatePlayToken_Viewer_Returns200(t *testing.T) {
	// Arrange: Viewer は app:read を持つため webToken 発行（ActionRead）が許可される。
	tenantID := uuid.New()
	svc := &fakeHandlerService{playView: PlayTokenView{Value: "secret-web-token"}}
	h := NewHandler(svc, authz.New(), &hFakeLogger{})
	claims := newTenantClaims(tenantID, "Viewer")

	// Act
	rec := doRequest(t, h, http.MethodPost, "/api/play-tokens", `{"parent_frame_url":"https://console.example.com"}`, &claims)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 body=%q", rec.Code, rec.Body.String())
	}
	if svc.playCalls != 1 {
		t.Fatalf("svc.CreatePlayToken 呼び出し回数 = %d; want 1", svc.playCalls)
	}
	// actor / tenantID は claims から取得して Service へ引数で渡される（design Components）。
	if svc.playActor != claims.AdminUserID {
		t.Errorf("svc.CreatePlayToken actor = %v; want %v", svc.playActor, claims.AdminUserID)
	}
	if svc.playTenant != tenantID {
		t.Errorf("svc.CreatePlayToken tenantID = %v; want %v", svc.playTenant, tenantID)
	}
	if svc.playIn.ParentFrameURL != "https://console.example.com" {
		t.Errorf("svc.CreatePlayToken in.ParentFrameURL = %q; want the request URL", svc.playIn.ParentFrameURL)
	}
	var view PlayTokenView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode PlayTokenView: %v body=%q", err, rec.Body.String())
	}
	if view.Value != "secret-web-token" {
		t.Errorf("response PlayTokenView.Value = %q; want secret-web-token", view.Value)
	}
}

// ---- (a-2) NFR 1.2: webToken.Value を Handler がログ経路へ載せない ----

func TestHandler_CreatePlayToken_DoesNotLogTokenValue(t *testing.T) {
	// Arrange: 秘匿値を返させ、拒否ログを誘発する状況が無い成功パスで検証する。
	tenantID := uuid.New()
	const secret = "super-secret-web-token-value"
	svc := &fakeHandlerService{playView: PlayTokenView{Value: secret}}
	log := &hFakeLogger{}
	h := NewHandler(svc, authz.New(), log)
	claims := newTenantClaims(tenantID, "TenantAdmin")

	// Act
	rec := doRequest(t, h, http.MethodPost, "/api/play-tokens", `{"parent_frame_url":"https://console.example.com"}`, &claims)

	// Assert: 成功しつつ、秘匿 Value がどのログエントリにも surface しない（NFR 1.2）。
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 body=%q", rec.Code, rec.Body.String())
	}
	if log.leaks(secret) {
		t.Errorf("webToken.Value がログ経路に露出している; warnCalls=%+v", log.warnCalls)
	}
}

// ---- (b) POST /play-tokens: malformed JSON → 400（body decode / Req 1.2） ----

func TestHandler_CreatePlayToken_MalformedJSON_Returns400(t *testing.T) {
	// Arrange
	tenantID := uuid.New()
	svc := &fakeHandlerService{}
	h := NewHandler(svc, authz.New(), &hFakeLogger{})
	claims := newTenantClaims(tenantID, "TenantAdmin")

	// Act: 壊れた JSON。
	rec := doRequest(t, h, http.MethodPost, "/api/play-tokens", `{"parent_frame_url":`, &claims)

	// Assert
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 body=%q", rec.Code, rec.Body.String())
	}
	if svc.playCalls != 0 {
		t.Errorf("malformed body で svc.CreatePlayToken が呼ばれてはならない（呼び出し回数=%d）", svc.playCalls)
	}
	if code := decodeErrCode(t, rec); code != string(pkgerrors.CodeInvalidRequest) {
		t.Errorf("body.Code = %q; want %q", code, pkgerrors.CodeInvalidRequest)
	}
}

// ---- (c) POST /play-tokens: 未バインドテナント → 422（service error mapping / Req 1.1） ----

func TestHandler_CreatePlayToken_NotBound_Returns422(t *testing.T) {
	// Arrange: Service が未バインド（CodeBusinessRule / 422）を返す。
	tenantID := uuid.New()
	svc := &fakeHandlerService{playErr: pkgerrors.New(pkgerrors.CodeBusinessRule, "tenant is not bound")}
	h := NewHandler(svc, authz.New(), &hFakeLogger{})
	claims := newTenantClaims(tenantID, "TenantAdmin")

	// Act
	rec := doRequest(t, h, http.MethodPost, "/api/play-tokens", `{"parent_frame_url":"https://console.example.com"}`, &claims)

	// Assert
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d; want 422 body=%q", rec.Code, rec.Body.String())
	}
	if code := decodeErrCode(t, rec); code != string(pkgerrors.CodeBusinessRule) {
		t.Errorf("body.Code = %q; want %q", code, pkgerrors.CodeBusinessRule)
	}
}

// ---- (d) POST /play-tokens: AMAPI 上流エラー → 502（service error mapping / Req 1.1） ----

func TestHandler_CreatePlayToken_UpstreamError_Returns502(t *testing.T) {
	// Arrange: Service が AMAPI 上流エラー（CodeUpstream / 502）を返す。
	tenantID := uuid.New()
	svc := &fakeHandlerService{playErr: pkgerrors.New(pkgerrors.CodeUpstream, "amapi web token failed")}
	h := NewHandler(svc, authz.New(), &hFakeLogger{})
	claims := newTenantClaims(tenantID, "TenantAdmin")

	// Act
	rec := doRequest(t, h, http.MethodPost, "/api/play-tokens", `{"parent_frame_url":"https://console.example.com"}`, &claims)

	// Assert
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d; want 502 body=%q", rec.Code, rec.Body.String())
	}
	if code := decodeErrCode(t, rec); code != string(pkgerrors.CodeUpstream) {
		t.Errorf("body.Code = %q; want %q", code, pkgerrors.CodeUpstream)
	}
}

// ---- (e) GET /apps: Viewer 許可 → 200 + 自テナント委譲 + 一覧返却（Req 2.1 / 2.3 / 4.1 / 4.2） ----

func TestHandler_ListApps_Viewer_Returns200OwnTenant(t *testing.T) {
	// Arrange: Viewer は app:read を持つため一覧参照が許可される。
	tenantID := uuid.New()
	icon := "https://cdn.example.com/icon.png"
	svc := &fakeHandlerService{listOut: []TenantAppView{
		{PackageName: "com.example.app", Title: "Example", IconURL: &icon},
	}}
	h := NewHandler(svc, authz.New(), &hFakeLogger{})
	claims := newTenantClaims(tenantID, "Viewer")

	// Act
	rec := doRequest(t, h, http.MethodGet, "/api/apps", "", &claims)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 body=%q", rec.Code, rec.Body.String())
	}
	if svc.listCalls != 1 {
		t.Fatalf("svc.ListApps 呼び出し回数 = %d; want 1", svc.listCalls)
	}
	// 自テナント scoped: Service へ claims.TenantID が渡される（RLS に分離を委ねる前提 / Req 2.3 / 4.2）。
	if svc.listTenant != tenantID {
		t.Errorf("svc.ListApps tenantID = %v; want %v", svc.listTenant, tenantID)
	}
	var views []TenantAppView
	if err := json.Unmarshal(rec.Body.Bytes(), &views); err != nil {
		t.Fatalf("decode []TenantAppView: %v body=%q", err, rec.Body.String())
	}
	if len(views) != 1 || views[0].PackageName != "com.example.app" {
		t.Errorf("response views = %+v; want 1 件 com.example.app", views)
	}
}

// ---- (f) POST /apps/sync: TenantAdmin 許可 → 200 + {synced_at,count} + 自テナント委譲（Req 3.3 / 4.1） ----

func TestHandler_SyncApps_TenantAdmin_Returns200(t *testing.T) {
	// Arrange: TenantAdmin は app:update を持つため同期が許可される。
	tenantID := uuid.New()
	syncedAt := time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)
	svc := &fakeHandlerService{syncOut: SyncResult{SyncedAt: syncedAt, Count: 2}}
	h := NewHandler(svc, authz.New(), &hFakeLogger{})
	claims := newTenantClaims(tenantID, "TenantAdmin")

	// Act
	rec := doRequest(t, h, http.MethodPost, "/api/apps/sync",
		`{"apps":[{"package_name":"com.a","title":"A"},{"package_name":"com.b","title":"B"}]}`, &claims)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 body=%q", rec.Code, rec.Body.String())
	}
	if svc.syncCalls != 1 {
		t.Fatalf("svc.SyncApps 呼び出し回数 = %d; want 1", svc.syncCalls)
	}
	// actor / tenantID は claims から取得して Service へ引数で渡される（design Components）。
	if svc.syncActor != claims.AdminUserID {
		t.Errorf("svc.SyncApps actor = %v; want %v", svc.syncActor, claims.AdminUserID)
	}
	if svc.syncTenant != tenantID {
		t.Errorf("svc.SyncApps tenantID = %v; want %v", svc.syncTenant, tenantID)
	}
	if len(svc.syncIn.Apps) != 2 {
		t.Errorf("svc.SyncApps in.Apps 件数 = %d; want 2", len(svc.syncIn.Apps))
	}
	var result SyncResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode SyncResult: %v body=%q", err, rec.Body.String())
	}
	if result.Count != 2 || !result.SyncedAt.Equal(syncedAt) {
		t.Errorf("response SyncResult = %+v; want Count=2 SyncedAt=%v", result, syncedAt)
	}
}

// ---- (g) POST /apps/sync: Operator は app:update を持たない → 403（RBAC deny / Req 4.1） ----

func TestHandler_SyncApps_Operator_Returns403(t *testing.T) {
	// Arrange: Operator は app:read のみで app:update を持たないため sync が拒否される。
	tenantID := uuid.New()
	svc := &fakeHandlerService{}
	log := &hFakeLogger{}
	h := NewHandler(svc, authz.New(), log)
	claims := newTenantClaims(tenantID, "Operator")

	// Act
	rec := doRequest(t, h, http.MethodPost, "/api/apps/sync",
		`{"apps":[{"package_name":"com.a","title":"A"}]}`, &claims)

	// Assert
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403 body=%q", rec.Code, rec.Body.String())
	}
	if svc.syncCalls != 0 {
		t.Errorf("403 で svc.SyncApps が呼ばれてはならない（呼び出し回数=%d）", svc.syncCalls)
	}
	if code := decodeErrCode(t, rec); code != string(pkgerrors.CodeForbidden) {
		t.Errorf("body.Code = %q; want %q", code, pkgerrors.CodeForbidden)
	}
	// deny は構造化 WARN（deny_reason=authz denied）で記録される。
	if !log.hasWarnReason("authz denied") {
		t.Errorf("deny 経路で deny_reason=authz denied の WARN ログが無い; warnCalls=%+v", log.warnCalls)
	}
}

// ---- (h) POST /apps/sync: Viewer は app:update を持たない → 403（read ロールの update 越権拒否 / Req 4.1） ----

func TestHandler_SyncApps_Viewer_Returns403(t *testing.T) {
	// Arrange: Viewer は app:read のみで app:update を持たない。
	tenantID := uuid.New()
	svc := &fakeHandlerService{}
	h := NewHandler(svc, authz.New(), &hFakeLogger{})
	claims := newTenantClaims(tenantID, "Viewer")

	// Act
	rec := doRequest(t, h, http.MethodPost, "/api/apps/sync",
		`{"apps":[{"package_name":"com.a","title":"A"}]}`, &claims)

	// Assert
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403 body=%q", rec.Code, rec.Body.String())
	}
	if svc.syncCalls != 0 {
		t.Errorf("403 で svc.SyncApps が呼ばれてはならない（呼び出し回数=%d）", svc.syncCalls)
	}
	if code := decodeErrCode(t, rec); code != string(pkgerrors.CodeForbidden) {
		t.Errorf("body.Code = %q; want %q", code, pkgerrors.CodeForbidden)
	}
}

// ---- (i) claims 不在で 401（防御的 / Req 4.1 周辺） ----

func TestHandler_ListApps_NoClaims_Returns401(t *testing.T) {
	// Arrange
	svc := &fakeHandlerService{}
	log := &hFakeLogger{}
	h := NewHandler(svc, authz.New(), log)

	// Act（claims を注入しない）
	rec := doRequest(t, h, http.MethodGet, "/api/apps", "", nil)

	// Assert
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401 body=%q", rec.Code, rec.Body.String())
	}
	if svc.listCalls != 0 {
		t.Errorf("401 で svc.ListApps が呼ばれてはならない（呼び出し回数=%d）", svc.listCalls)
	}
	if code := decodeErrCode(t, rec); code != string(pkgerrors.CodeUnauthenticated) {
		t.Errorf("body.Code = %q; want %q", code, pkgerrors.CodeUnauthenticated)
	}
	if !log.hasWarnReason("missing auth claims") {
		t.Errorf("claims 不在経路で deny_reason=missing auth claims の WARN ログが無い; warnCalls=%+v", log.warnCalls)
	}
}
