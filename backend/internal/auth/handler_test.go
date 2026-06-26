package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/oidc"
)

// ---- Test constants (Handler-scoped, fixed strings for leakage assertions) ----

const (
	testHandlerRedirectURL    = "https://idp.example.com/auth?state=abc&nonce=def"
	testHandlerReturnTo       = "/dashboard"
	testHandlerStateCookieVal = "test-state-cookie-value-DO-NOT-LEAK"
	testHandlerSessionRawVal  = "test-session-raw-token-DO-NOT-LEAK"
)

// ---- Fake Service for Handler tests ----

type fakeServiceCalls struct {
	beginLogin     int
	handleCallback int
	lookupRefresh  int
	logout         int
}

type fakeService struct {
	mu sync.Mutex

	// BeginLogin behaviors
	beginLoginRedirectURL string
	beginLoginCookie      http.Cookie
	beginLoginErr         error

	// HandleCallback behaviors
	handleCallbackRawToken string
	handleCallbackCookie   http.Cookie
	handleCallbackReturnTo string
	handleCallbackErr      error

	// Logout behaviors
	logoutErr error

	// Recorded inputs
	calls               fakeServiceCalls
	lastBeginConsole    oidc.Console
	lastBeginReturnTo   string
	lastCallbackConsole oidc.Console
	lastCallbackCode    string
	lastCallbackState   string
	lastCallbackCookie  string
	lastLogoutToken     string
}

func (f *fakeService) BeginLogin(_ context.Context, console oidc.Console, returnTo string) (string, http.Cookie, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls.beginLogin++
	f.lastBeginConsole = console
	f.lastBeginReturnTo = returnTo
	if f.beginLoginErr != nil {
		return "", http.Cookie{}, f.beginLoginErr
	}
	return f.beginLoginRedirectURL, f.beginLoginCookie, nil
}

func (f *fakeService) HandleCallback(_ context.Context, console oidc.Console, code, queryState, rawStateCookie string) (string, http.Cookie, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls.handleCallback++
	f.lastCallbackConsole = console
	f.lastCallbackCode = code
	f.lastCallbackState = queryState
	f.lastCallbackCookie = rawStateCookie
	if f.handleCallbackErr != nil {
		return "", http.Cookie{}, "", f.handleCallbackErr
	}
	return f.handleCallbackRawToken, f.handleCallbackCookie, f.handleCallbackReturnTo, nil
}

func (f *fakeService) LookupAndRefresh(_ context.Context, _ string, _ oidc.Console, _ time.Time) (Identity, Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls.lookupRefresh++
	return Identity{}, Session{}, nil
}

func (f *fakeService) Logout(_ context.Context, rawSessionToken string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls.logout++
	f.lastLogoutToken = rawSessionToken
	return f.logoutErr
}

// ---- Test helpers ----

// newTestHandlerRouter constructs a chi router with both tenant and admin Mount calls
// applied so that all 6 endpoints are testable via a single router instance.
func newTestHandlerRouter(t *testing.T, svc Service) chi.Router {
	t.Helper()
	r := chi.NewRouter()
	log := &fakeLogger{}
	h := NewHandler(svc, log)
	h.Mount(r, "/api/auth", oidc.ConsoleTenant)
	h.Mount(r, "/api/admin/auth", oidc.ConsoleAdmin)
	return r
}

// findCookie returns the first Set-Cookie entry with the matching Name, or nil.
func findCookie(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// makeValidStateCookie returns a usable state cookie with non-empty Value for tests that
// need to verify the cookie was passed to Service. Value is opaque to the Handler.
func makeValidStateCookie() *http.Cookie {
	return &http.Cookie{
		Name:  stateCookieName,
		Value: testHandlerStateCookieVal,
	}
}

// defaultFakeService returns a fakeService configured for the happy path.
func defaultFakeService() *fakeService {
	return &fakeService{
		beginLoginRedirectURL: testHandlerRedirectURL,
		beginLoginCookie: http.Cookie{
			Name:     stateCookieName,
			Value:    testHandlerStateCookieVal,
			Path:     "/",
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   600,
		},
		handleCallbackRawToken: testHandlerSessionRawVal,
		handleCallbackCookie: http.Cookie{
			Name:     sessionCookieName,
			Value:    testHandlerSessionRawVal,
			Path:     "/",
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   28800,
		},
		handleCallbackReturnTo: testHandlerReturnTo,
	}
}

// ============================================================================
// (a) /api/auth/login: 302 + state cookie / return_to=//evil.example で 400
// ============================================================================

func TestLogin_TenantSuccess_Returns302AndStateCookie(t *testing.T) {
	// Arrange
	fake := defaultFakeService()
	r := newTestHandlerRouter(t, fake)

	// Act
	req := httptest.NewRequest(http.MethodGet, "/api/auth/login?return_to=/dashboard", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusFound {
		t.Fatalf("status: want 302, got %d", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != testHandlerRedirectURL {
		t.Errorf("Location: want %q, got %q", testHandlerRedirectURL, got)
	}
	cookie := findCookie(t, rec, stateCookieName)
	if cookie == nil {
		t.Fatalf("expected Set-Cookie with name %s", stateCookieName)
	}
	if cookie.Value == "" {
		t.Errorf("state cookie Value is empty")
	}
	if fake.calls.beginLogin != 1 {
		t.Errorf("BeginLogin call count: want 1, got %d", fake.calls.beginLogin)
	}
	if fake.lastBeginConsole != oidc.ConsoleTenant {
		t.Errorf("BeginLogin console: want %s, got %s", oidc.ConsoleTenant, fake.lastBeginConsole)
	}
	if fake.lastBeginReturnTo != "/dashboard" {
		t.Errorf("BeginLogin returnTo: want /dashboard, got %s", fake.lastBeginReturnTo)
	}
}

func TestLogin_AdminSuccess_Returns302AndStateCookie(t *testing.T) {
	// Arrange
	fake := defaultFakeService()
	r := newTestHandlerRouter(t, fake)

	// Act
	req := httptest.NewRequest(http.MethodGet, "/api/admin/auth/login?return_to=/admin/dashboard", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusFound {
		t.Fatalf("status: want 302, got %d", rec.Code)
	}
	if fake.lastBeginConsole != oidc.ConsoleAdmin {
		t.Errorf("BeginLogin console: want %s, got %s", oidc.ConsoleAdmin, fake.lastBeginConsole)
	}
}

func TestLogin_OpenRedirectCandidate_Returns400(t *testing.T) {
	// Arrange: fake Service returns the same error that the real Service would for
	// `//evil.example` (the Handler delegates validation to Service).
	fake := defaultFakeService()
	fake.beginLoginErr = pkgerrors.Wrap(
		pkgerrors.CodeInvalidRequest,
		"return_to must be a same-origin relative path",
		FailureKindReturnToInvalid,
	)
	r := newTestHandlerRouter(t, fake)

	// Act
	req := httptest.NewRequest(http.MethodGet, "/api/auth/login?return_to=//evil.example", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d", rec.Code)
	}
	if findCookie(t, rec, stateCookieName) != nil {
		t.Errorf("expected no state cookie on 400")
	}
	if fake.calls.beginLogin != 1 {
		t.Errorf("BeginLogin call count: want 1, got %d", fake.calls.beginLogin)
	}
}

// ============================================================================
// (b) /api/auth/callback: 302 + session cookie + state cookie 削除 + Location が
//     fake Service の戻す returnTo 値と一致
// ============================================================================

func TestCallback_TenantSuccess_Returns302WithSessionAndStateExpired(t *testing.T) {
	// Arrange
	fake := defaultFakeService()
	r := newTestHandlerRouter(t, fake)

	// Act
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?code=auth-code-123&state=query-state-xyz", nil)
	req.AddCookie(makeValidStateCookie())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Assert: 302 + Location == fake.returnTo
	if rec.Code != http.StatusFound {
		t.Fatalf("status: want 302, got %d", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != testHandlerReturnTo {
		t.Errorf("Location: want %q, got %q", testHandlerReturnTo, got)
	}

	// session cookie present
	sessionCookie := findCookie(t, rec, sessionCookieName)
	if sessionCookie == nil {
		t.Fatalf("expected Set-Cookie with name %s", sessionCookieName)
	}
	if sessionCookie.Value != testHandlerSessionRawVal {
		t.Errorf("session cookie value: want %q, got %q", testHandlerSessionRawVal, sessionCookie.Value)
	}

	// state cookie expire marker present (MaxAge < 0)
	stateCookie := findCookie(t, rec, stateCookieName)
	if stateCookie == nil {
		t.Fatalf("expected state cookie expire marker (Set-Cookie %s)", stateCookieName)
	}
	if stateCookie.MaxAge >= 0 {
		t.Errorf("state cookie MaxAge: want <0 (expire marker), got %d", stateCookie.MaxAge)
	}
	if stateCookie.Value != "" {
		t.Errorf("state cookie expire Value: want \"\", got %q", stateCookie.Value)
	}

	// Service called with expected args
	if fake.calls.handleCallback != 1 {
		t.Errorf("HandleCallback call count: want 1, got %d", fake.calls.handleCallback)
	}
	if fake.lastCallbackConsole != oidc.ConsoleTenant {
		t.Errorf("HandleCallback console: want tenant, got %s", fake.lastCallbackConsole)
	}
	if fake.lastCallbackCode != "auth-code-123" {
		t.Errorf("HandleCallback code: want auth-code-123, got %s", fake.lastCallbackCode)
	}
	if fake.lastCallbackState != "query-state-xyz" {
		t.Errorf("HandleCallback state: want query-state-xyz, got %s", fake.lastCallbackState)
	}
	if fake.lastCallbackCookie != testHandlerStateCookieVal {
		t.Errorf("HandleCallback rawStateCookie: want %q, got %q", testHandlerStateCookieVal, fake.lastCallbackCookie)
	}
}

func TestCallback_AdminSuccess_RoutedToAdminConsole(t *testing.T) {
	// Arrange
	fake := defaultFakeService()
	r := newTestHandlerRouter(t, fake)

	// Act
	req := httptest.NewRequest(http.MethodGet, "/api/admin/auth/callback?code=admin-code&state=admin-state", nil)
	req.AddCookie(makeValidStateCookie())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusFound {
		t.Fatalf("status: want 302, got %d", rec.Code)
	}
	if fake.lastCallbackConsole != oidc.ConsoleAdmin {
		t.Errorf("HandleCallback console: want admin, got %s", fake.lastCallbackConsole)
	}
}

// ============================================================================
// (c) /api/auth/callback: state mismatch で 401 + cookie 削除
// ============================================================================

func TestCallback_StateMismatch_Returns401AndExpiresStateCookie(t *testing.T) {
	// Arrange
	fake := defaultFakeService()
	fake.handleCallbackErr = pkgerrors.Wrap(
		pkgerrors.CodeUnauthenticated,
		"query state does not match cookie nonce",
		FailureKindStateMismatch,
	)
	r := newTestHandlerRouter(t, fake)

	// Act
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?code=auth-code-123&state=wrong-state", nil)
	req.AddCookie(makeValidStateCookie())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401, got %d", rec.Code)
	}
	stateCookie := findCookie(t, rec, stateCookieName)
	if stateCookie == nil {
		t.Fatalf("expected state cookie expire marker on 401")
	}
	if stateCookie.MaxAge >= 0 {
		t.Errorf("state cookie MaxAge: want <0 (expire marker), got %d", stateCookie.MaxAge)
	}
	if findCookie(t, rec, sessionCookieName) != nil {
		t.Errorf("expected no session cookie on 401")
	}
	if fake.calls.handleCallback != 1 {
		t.Errorf("HandleCallback call count: want 1, got %d", fake.calls.handleCallback)
	}
}

func TestCallback_AdminStateMismatch_Returns401AndExpiresStateCookie(t *testing.T) {
	// Arrange
	fake := defaultFakeService()
	fake.handleCallbackErr = pkgerrors.Wrap(
		pkgerrors.CodeUnauthenticated,
		"query state does not match cookie nonce",
		FailureKindStateMismatch,
	)
	r := newTestHandlerRouter(t, fake)

	// Act
	req := httptest.NewRequest(http.MethodGet, "/api/admin/auth/callback?code=admin-code&state=wrong-state", nil)
	req.AddCookie(makeValidStateCookie())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401, got %d", rec.Code)
	}
}

// ============================================================================
// (c2) /api/auth/callback: code クエリ欠落で 400 invalid_request + state cookie 削除
//      （fake Service が呼ばれないことを assert）
// ============================================================================

func TestCallback_MissingCode_Returns400InvalidRequestAndDoesNotCallService(t *testing.T) {
	// Arrange
	fake := defaultFakeService()
	r := newTestHandlerRouter(t, fake)

	// Act: code 欠落
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?state=query-state-xyz", nil)
	req.AddCookie(makeValidStateCookie())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Assert: 400 + state cookie 削除 + Service 未呼出
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d", rec.Code)
	}
	stateCookie := findCookie(t, rec, stateCookieName)
	if stateCookie == nil {
		t.Fatalf("expected state cookie expire marker on 400")
	}
	if stateCookie.MaxAge >= 0 {
		t.Errorf("state cookie MaxAge: want <0 (expire marker), got %d", stateCookie.MaxAge)
	}
	if fake.calls.handleCallback != 0 {
		t.Errorf("HandleCallback should NOT be called on missing code, but called %d times", fake.calls.handleCallback)
	}
	// body には failure_kind / code が JSON で含まれるはず（errors.WriteHTTP 経由）
	body := rec.Body.String()
	if !strings.Contains(body, string(pkgerrors.CodeInvalidRequest)) {
		t.Errorf("body should contain code=%s, got: %s", pkgerrors.CodeInvalidRequest, body)
	}
}

// ============================================================================
// (c3) /api/auth/callback: state クエリ欠落で 400 invalid_request + state cookie 削除
//      （fake Service が呼ばれないことを assert）
// ============================================================================

func TestCallback_MissingState_Returns400InvalidRequestAndDoesNotCallService(t *testing.T) {
	// Arrange
	fake := defaultFakeService()
	r := newTestHandlerRouter(t, fake)

	// Act: state 欠落
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?code=auth-code-123", nil)
	req.AddCookie(makeValidStateCookie())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d", rec.Code)
	}
	stateCookie := findCookie(t, rec, stateCookieName)
	if stateCookie == nil {
		t.Fatalf("expected state cookie expire marker on 400")
	}
	if stateCookie.MaxAge >= 0 {
		t.Errorf("state cookie MaxAge: want <0 (expire marker), got %d", stateCookie.MaxAge)
	}
	if fake.calls.handleCallback != 0 {
		t.Errorf("HandleCallback should NOT be called on missing state, but called %d times", fake.calls.handleCallback)
	}
}

func TestCallback_MissingBoth_Returns400AndDoesNotCallService(t *testing.T) {
	// Arrange
	fake := defaultFakeService()
	r := newTestHandlerRouter(t, fake)

	// Act: code/state 双方欠落
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback", nil)
	req.AddCookie(makeValidStateCookie())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: want 400, got %d", rec.Code)
	}
	if fake.calls.handleCallback != 0 {
		t.Errorf("HandleCallback should NOT be called, but called %d times", fake.calls.handleCallback)
	}
}

// ============================================================================
// (d) /api/auth/logout: 204 + session cookie 削除、cookie 不在で 401
// ============================================================================

func TestLogout_TenantWithSessionCookie_Returns204AndExpiresSessionCookie(t *testing.T) {
	// Arrange
	fake := defaultFakeService()
	r := newTestHandlerRouter(t, fake)

	// Act
	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: testHandlerSessionRawVal})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: want 204, got %d", rec.Code)
	}
	sessionCookie := findCookie(t, rec, sessionCookieName)
	if sessionCookie == nil {
		t.Fatalf("expected Set-Cookie %s on logout", sessionCookieName)
	}
	if sessionCookie.MaxAge >= 0 {
		t.Errorf("session cookie expire MaxAge: want <0, got %d", sessionCookie.MaxAge)
	}
	if sessionCookie.Value != "" {
		t.Errorf("session cookie expire Value: want \"\", got %q", sessionCookie.Value)
	}
	if fake.calls.logout != 1 {
		t.Errorf("Logout call count: want 1, got %d", fake.calls.logout)
	}
	if fake.lastLogoutToken != testHandlerSessionRawVal {
		t.Errorf("Logout token: want %q, got %q", testHandlerSessionRawVal, fake.lastLogoutToken)
	}
}

func TestLogout_AdminWithSessionCookie_Returns204(t *testing.T) {
	// Arrange
	fake := defaultFakeService()
	r := newTestHandlerRouter(t, fake)

	// Act
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: testHandlerSessionRawVal})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status: want 204, got %d", rec.Code)
	}
	if fake.calls.logout != 1 {
		t.Errorf("Logout call count: want 1, got %d", fake.calls.logout)
	}
}

func TestLogout_NoSessionCookie_Returns401(t *testing.T) {
	// Arrange
	fake := defaultFakeService()
	r := newTestHandlerRouter(t, fake)

	// Act: cookie 不在
	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401, got %d", rec.Code)
	}
	if fake.calls.logout != 0 {
		t.Errorf("Logout should NOT be called when cookie is absent, but called %d times", fake.calls.logout)
	}
}

func TestLogout_AdminNoSessionCookie_Returns401(t *testing.T) {
	// Arrange
	fake := defaultFakeService()
	r := newTestHandlerRouter(t, fake)

	// Act
	req := httptest.NewRequest(http.MethodPost, "/api/admin/auth/logout", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401, got %d", rec.Code)
	}
}

func TestLogout_EmptyCookieValue_Returns401(t *testing.T) {
	// Arrange: cookie が存在するが Value が空（malformed / edge case）
	fake := defaultFakeService()
	r := newTestHandlerRouter(t, fake)

	// Act
	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: ""})
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401, got %d", rec.Code)
	}
	if fake.calls.logout != 0 {
		t.Errorf("Logout should NOT be called for empty cookie value, but called %d times", fake.calls.logout)
	}
}

// ============================================================================
// 補助: 6 endpoint mounted by tenant + admin Mount 2 回呼び出し
// ============================================================================

func TestMount_RegistersAllSixEndpoints(t *testing.T) {
	// Arrange
	fake := defaultFakeService()
	r := newTestHandlerRouter(t, fake)

	// Act/Assert: 6 endpoint が到達可能であることを最低限の応答コードで確認
	cases := []struct {
		method   string
		path     string
		wantCode int
	}{
		{http.MethodGet, "/api/auth/login", http.StatusFound},
		{http.MethodGet, "/api/auth/callback?code=c&state=s", http.StatusFound},
		{http.MethodPost, "/api/auth/logout", http.StatusUnauthorized}, // cookie 不在
		{http.MethodGet, "/api/admin/auth/login", http.StatusFound},
		{http.MethodGet, "/api/admin/auth/callback?code=c&state=s", http.StatusFound},
		{http.MethodPost, "/api/admin/auth/logout", http.StatusUnauthorized}, // cookie 不在
	}

	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			if strings.Contains(tc.path, "/callback") {
				req.AddCookie(makeValidStateCookie())
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			// Note: WriteHTTP は responseWriter ベースで sentinel を持つ。複数 sub-tests で
			// 同じ recorder ポインタが reuse されない構造のため問題は出ないが、念のため
			// pkgerrors.ClearWriter は不要（recorder ごとに新規）。
			if rec.Code != tc.wantCode {
				t.Errorf("status: want %d, got %d", tc.wantCode, rec.Code)
			}
		})
	}
}

// ============================================================================
// 機密値非埋込契約: response body / Header に session token / state cookie 生値が漏れない
// ============================================================================

func TestCallback_ErrorResponse_DoesNotLeakSensitiveValues(t *testing.T) {
	// Arrange: HandleCallback が error を返す経路（state mismatch）
	fake := defaultFakeService()
	fake.handleCallbackErr = pkgerrors.Wrap(
		pkgerrors.CodeUnauthenticated,
		"query state does not match cookie nonce",
		FailureKindStateMismatch,
	)
	r := newTestHandlerRouter(t, fake)

	// Act
	req := httptest.NewRequest(http.MethodGet, "/api/auth/callback?code=auth-code-123&state=wrong-state", nil)
	req.AddCookie(makeValidStateCookie())
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Assert: body / Header に state cookie 生値が含まれない
	body := rec.Body.String()
	if strings.Contains(body, testHandlerStateCookieVal) {
		t.Errorf("response body leaks state cookie raw value: %s", body)
	}
	for k, vs := range rec.Header() {
		for _, v := range vs {
			if strings.Contains(v, testHandlerStateCookieVal) {
				t.Errorf("response header %s leaks state cookie raw value: %s", k, v)
			}
		}
	}
}
