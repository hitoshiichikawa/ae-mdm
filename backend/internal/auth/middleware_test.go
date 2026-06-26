package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/httpserver"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/oidc"
)

// ---- Middleware-scoped test constants ----

const (
	testMWRawSessionToken = "test-mw-raw-session-token-DO-NOT-LEAK"
	testMWStateMACSecret  = "test-mw-state-mac-secret-DO-NOT-LEAK-PLEASE"
)

// ---- Fake Service for Middleware tests ----
//
// Implements auth.Service. Only LookupAndRefresh is exercised in middleware tests;
// other methods exist to satisfy the interface and panic if accidentally invoked.

type fakeMWService struct {
	mu sync.Mutex

	// Configured behavior for LookupAndRefresh
	lookupIdentity Identity
	lookupSession  Session
	lookupErr      error

	// Recorded inputs
	lookupCalls           int
	lastLookupRawToken    string
	lastLookupExpConsole  oidc.Console
	lastLookupNow         time.Time
}

func (f *fakeMWService) BeginLogin(_ context.Context, _ oidc.Console, _ string) (string, http.Cookie, error) {
	panic("BeginLogin must not be invoked by middleware")
}

func (f *fakeMWService) HandleCallback(_ context.Context, _ oidc.Console, _, _, _ string) (string, http.Cookie, string, error) {
	panic("HandleCallback must not be invoked by middleware")
}

func (f *fakeMWService) LookupAndRefresh(_ context.Context, raw string, expected oidc.Console, now time.Time) (Identity, Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookupCalls++
	f.lastLookupRawToken = raw
	f.lastLookupExpConsole = expected
	f.lastLookupNow = now
	if f.lookupErr != nil {
		return Identity{}, Session{}, f.lookupErr
	}
	return f.lookupIdentity, f.lookupSession, nil
}

func (f *fakeMWService) Logout(_ context.Context, _ string) error {
	panic("Logout must not be invoked by middleware")
}

// ---- Test helpers ----

// nextProbeHandler returns an http.Handler that records the request context and
// writes HTTP 200. The captured context can be inspected after the middleware runs.
type nextProbe struct {
	called    bool
	gotCtx    context.Context
	gotClaims httpserver.AuthClaims
	hasClaims bool
}

func newNextProbeHandler(p *nextProbe) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.called = true
		p.gotCtx = r.Context()
		p.gotClaims, p.hasClaims = httpserver.AuthClaimsFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
}

// findCookieMW returns the first Set-Cookie entry from the recorder that matches name.
func findCookieMW(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// newMWFixture builds the middleware under test together with the fake Service and
// a fake logger / clock, returning all dependencies for inspection.
type mwFixture struct {
	svc     *fakeMWService
	log     *fakeLogger
	clock   *fakeClock
	handler func(http.Handler) http.Handler
}

func newMWFixture(t *testing.T, expected oidc.Console) *mwFixture {
	t.Helper()
	svc := &fakeMWService{}
	log := &fakeLogger{}
	clk := &fakeClock{now: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	mw := NewMiddleware(svc, expected, log, clk)
	return &mwFixture{svc: svc, log: log, clock: clk, handler: mw}
}

// ============================================================================
// (a) cookie 不在 → 401 + cookie 削除 + failure_kind=session_tamper
// ============================================================================

func TestMiddleware_NoCookie_Returns401AndExpiresCookieAndLogsTamper(t *testing.T) {
	// Arrange
	fx := newMWFixture(t, oidc.ConsoleTenant)
	probe := &nextProbe{}

	// Act
	req := httptest.NewRequest(http.MethodGet, "/api/foo", nil)
	rec := httptest.NewRecorder()
	fx.handler(newNextProbeHandler(probe)).ServeHTTP(rec, req)

	// Assert: 401 + 削除 cookie + Service 未呼出 + next 未到達 + failure_kind ログ
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401, got %d", rec.Code)
	}
	if probe.called {
		t.Errorf("next handler should not be invoked when cookie is absent")
	}
	if fx.svc.lookupCalls != 0 {
		t.Errorf("LookupAndRefresh should not be called when cookie is absent, calls=%d", fx.svc.lookupCalls)
	}
	c := findCookieMW(t, rec, sessionCookieName)
	if c == nil {
		t.Fatalf("expected Set-Cookie %s (expire marker) on 401", sessionCookieName)
	}
	if c.MaxAge >= 0 {
		t.Errorf("expire cookie MaxAge: want <0, got %d", c.MaxAge)
	}
	if c.Value != "" {
		t.Errorf("expire cookie Value: want empty, got %q", c.Value)
	}
	if !fx.log.hasWarnWithFailureKind("session_tamper") {
		t.Errorf("expected log.Warn with failure_kind=session_tamper, got: %+v", fx.log.entries)
	}
}

// (a2) cookie 名が __Host-ae_mdm_session 以外（不正な name）も「不在」扱いで 401
func TestMiddleware_DifferentCookieName_TreatedAsAbsent(t *testing.T) {
	fx := newMWFixture(t, oidc.ConsoleTenant)
	probe := &nextProbe{}

	req := httptest.NewRequest(http.MethodGet, "/api/foo", nil)
	req.AddCookie(&http.Cookie{Name: "some_other_cookie", Value: "x"})
	rec := httptest.NewRecorder()
	fx.handler(newNextProbeHandler(probe)).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401, got %d", rec.Code)
	}
	if fx.svc.lookupCalls != 0 {
		t.Errorf("LookupAndRefresh should not be called when only foreign cookie present")
	}
	if !fx.log.hasWarnWithFailureKind("session_tamper") {
		t.Errorf("expected log.Warn with failure_kind=session_tamper")
	}
}

// ============================================================================
// (b) LookupAndRefresh が session_idle → 401 + cookie 削除 + failure_kind=session_idle
// ============================================================================

func TestMiddleware_SessionIdle_Returns401AndExpiresCookie(t *testing.T) {
	fx := newMWFixture(t, oidc.ConsoleTenant)
	fx.svc.lookupErr = pkgerrors.Wrap(
		pkgerrors.CodeUnauthenticated,
		"session idle timeout exceeded",
		FailureKindSessionIdle,
	)
	probe := &nextProbe{}

	req := httptest.NewRequest(http.MethodGet, "/api/foo", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: testMWRawSessionToken})
	rec := httptest.NewRecorder()
	fx.handler(newNextProbeHandler(probe)).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401, got %d", rec.Code)
	}
	if probe.called {
		t.Errorf("next handler should not be invoked on session_idle")
	}
	c := findCookieMW(t, rec, sessionCookieName)
	if c == nil {
		t.Fatalf("expected Set-Cookie %s expire marker", sessionCookieName)
	}
	if c.MaxAge >= 0 {
		t.Errorf("expire cookie MaxAge: want <0, got %d", c.MaxAge)
	}
	if !fx.log.hasWarnWithFailureKind("session_idle") {
		t.Errorf("expected log.Warn with failure_kind=session_idle, got: %+v", fx.log.entries)
	}
	// Service is invoked once with the raw token and expected console and clock.Now()
	if fx.svc.lookupCalls != 1 {
		t.Errorf("LookupAndRefresh: want 1 call, got %d", fx.svc.lookupCalls)
	}
	if fx.svc.lastLookupRawToken != testMWRawSessionToken {
		t.Errorf("LookupAndRefresh raw token: got %q", fx.svc.lastLookupRawToken)
	}
	if fx.svc.lastLookupExpConsole != oidc.ConsoleTenant {
		t.Errorf("LookupAndRefresh expectedConsole: got %s", fx.svc.lastLookupExpConsole)
	}
	if !fx.svc.lastLookupNow.Equal(fx.clock.now) {
		t.Errorf("LookupAndRefresh now: want %v, got %v", fx.clock.now, fx.svc.lastLookupNow)
	}
}

// ============================================================================
// (c) session_expired / session_revoked / session_tamper も同様の挙動
// ============================================================================

func TestMiddleware_ExpiredRevokedTamper_Returns401AndExpiresCookie(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		wantKey string
	}{
		{
			name:    "session_expired",
			err:     pkgerrors.Wrap(pkgerrors.CodeUnauthenticated, "session absolute expiry exceeded", FailureKindSessionExpired),
			wantKey: "session_expired",
		},
		{
			name:    "session_revoked",
			err:     pkgerrors.Wrap(pkgerrors.CodeUnauthenticated, "session revoked", FailureKindSessionRevoked),
			wantKey: "session_revoked",
		},
		{
			name:    "session_tamper",
			err:     pkgerrors.Wrap(pkgerrors.CodeUnauthenticated, "session not found", FailureKindSessionTamper),
			wantKey: "session_tamper",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newMWFixture(t, oidc.ConsoleTenant)
			fx.svc.lookupErr = tc.err
			probe := &nextProbe{}

			req := httptest.NewRequest(http.MethodGet, "/api/foo", nil)
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: testMWRawSessionToken})
			rec := httptest.NewRecorder()
			fx.handler(newNextProbeHandler(probe)).ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status: want 401, got %d", rec.Code)
			}
			if probe.called {
				t.Errorf("next handler should not be invoked")
			}
			c := findCookieMW(t, rec, sessionCookieName)
			if c == nil {
				t.Fatalf("expected Set-Cookie %s expire marker", sessionCookieName)
			}
			if c.MaxAge >= 0 {
				t.Errorf("expire cookie MaxAge: want <0, got %d", c.MaxAge)
			}
			if !fx.log.hasWarnWithFailureKind(tc.wantKey) {
				t.Errorf("expected log.Warn with failure_kind=%s, got: %+v", tc.wantKey, fx.log.entries)
			}
		})
	}
}

// ============================================================================
// (d) expectedConsole=ConsoleAdmin で tenant session 提示 → console_mismatch 401
//     （fake Service が console_mismatch を返すケース / Req 6.2 / 6.3 の直接対応）
// ============================================================================

func TestMiddleware_AdminExpectedTenantPresented_ConsoleMismatch(t *testing.T) {
	fx := newMWFixture(t, oidc.ConsoleAdmin)
	fx.svc.lookupErr = pkgerrors.Wrap(
		pkgerrors.CodeUnauthenticated,
		"session console does not match expected console",
		FailureKindConsoleMismatch,
	)
	probe := &nextProbe{}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/foo", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: testMWRawSessionToken})
	rec := httptest.NewRecorder()
	fx.handler(newNextProbeHandler(probe)).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401, got %d", rec.Code)
	}
	if probe.called {
		t.Errorf("next handler should not be invoked on console_mismatch")
	}
	c := findCookieMW(t, rec, sessionCookieName)
	if c == nil {
		t.Fatalf("expected Set-Cookie %s expire marker", sessionCookieName)
	}
	if c.MaxAge >= 0 {
		t.Errorf("expire cookie MaxAge: want <0, got %d", c.MaxAge)
	}
	if !fx.log.hasWarnWithFailureKind("console_mismatch") {
		t.Errorf("expected log.Warn with failure_kind=console_mismatch, got: %+v", fx.log.entries)
	}
	// expectedConsole=Admin が Service へ伝搬していること
	if fx.svc.lastLookupExpConsole != oidc.ConsoleAdmin {
		t.Errorf("Service expectedConsole: want %s, got %s", oidc.ConsoleAdmin, fx.svc.lastLookupExpConsole)
	}
}

// ============================================================================
// (e) 成功時に AuthClaims が ctx に注入され next 到達（status 200）
// ============================================================================

func TestMiddleware_HappyPath_InjectsAuthClaimsAndReachesNext(t *testing.T) {
	fx := newMWFixture(t, oidc.ConsoleTenant)
	tenantID := uuid.New()
	adminUserID := uuid.New()
	roles := []string{"TenantAdmin", "Operator"}
	fx.svc.lookupIdentity = Identity{
		AdminUserID:  adminUserID,
		OIDCSubject:  "sub-xyz",
		Email:        "admin@example.com",
		TenantID:     tenantID,
		Roles:        roles,
		IsSuperAdmin: false,
	}
	fx.svc.lookupSession = Session{
		TokenHash:   HashToken(testMWRawSessionToken),
		AdminUserID: adminUserID,
		Console:     oidc.ConsoleTenant,
		IssuedAt:    fx.clock.now.Add(-1 * time.Hour),
		LastSeenAt:  fx.clock.now.Add(-1 * time.Minute),
		ExpiresAt:   fx.clock.now.Add(7 * time.Hour),
	}
	probe := &nextProbe{}

	req := httptest.NewRequest(http.MethodGet, "/api/foo", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: testMWRawSessionToken})
	rec := httptest.NewRecorder()
	fx.handler(newNextProbeHandler(probe)).ServeHTTP(rec, req)

	// next reached → 200
	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	if !probe.called {
		t.Fatalf("next handler not called on happy path")
	}
	if !probe.hasClaims {
		t.Fatalf("AuthClaims not found in context after middleware success")
	}
	if probe.gotClaims.TenantID != tenantID {
		t.Errorf("claims.TenantID: want %s, got %s", tenantID, probe.gotClaims.TenantID)
	}
	if probe.gotClaims.AdminUserID != adminUserID {
		t.Errorf("claims.AdminUserID: want %s, got %s", adminUserID, probe.gotClaims.AdminUserID)
	}
	if len(probe.gotClaims.Roles) != len(roles) {
		t.Fatalf("claims.Roles length: want %d, got %d", len(roles), len(probe.gotClaims.Roles))
	}
	for i, want := range roles {
		if probe.gotClaims.Roles[i] != want {
			t.Errorf("claims.Roles[%d]: want %q, got %q", i, want, probe.gotClaims.Roles[i])
		}
	}
	if probe.gotClaims.IsSuperAdmin {
		t.Errorf("claims.IsSuperAdmin: want false, got true")
	}
	// Console は Session.Console の文字列表現が転記される（Issue #37 / 6.2）
	if probe.gotClaims.Console != string(oidc.ConsoleTenant) {
		t.Errorf("claims.Console: want %q, got %q", string(oidc.ConsoleTenant), probe.gotClaims.Console)
	}
	// On success no Set-Cookie expire marker is emitted
	if findCookieMW(t, rec, sessionCookieName) != nil {
		t.Errorf("did not expect Set-Cookie for %s on success", sessionCookieName)
	}
}

// (e1) 成功時 Console と SessionHashPrefix が AuthClaims に転記される
//
// Issue #37 (#44 PR iteration round 1): Session.Console → AuthClaims.Console の転記、
// および raw token から派生する SessionHashPrefix（短縮 hash prefix）の転記を verify する。
// Console が空文字に退行すると `/api/admin/*` ガード [RequireAdminConsoleAndSuperAdmin] が
// audience_mismatch で実 session を拒否する回帰になるため、ここで明示的に固定する
// （review-notes 7.3 / 6.2 / NFR 1.1）。
func TestMiddleware_HappyPath_CopiesConsoleAndSessionHashPrefixToClaims(t *testing.T) {
	cases := []struct {
		name            string
		expected        oidc.Console
		sessionConsole  oidc.Console
		wantConsoleSent string
	}{
		{"tenant_console", oidc.ConsoleTenant, oidc.ConsoleTenant, "tenant-console"},
		{"admin_console", oidc.ConsoleAdmin, oidc.ConsoleAdmin, "admin-console"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newMWFixture(t, tc.expected)
			tenantID := uuid.New()
			adminUserID := uuid.New()
			fx.svc.lookupIdentity = Identity{
				AdminUserID:  adminUserID,
				TenantID:     tenantID,
				Roles:        []string{"TenantAdmin"},
				IsSuperAdmin: false,
			}
			fx.svc.lookupSession = Session{
				TokenHash: HashToken(testMWRawSessionToken),
				Console:   tc.sessionConsole,
			}
			probe := &nextProbe{}

			req := httptest.NewRequest(http.MethodGet, "/api/foo", nil)
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: testMWRawSessionToken})
			rec := httptest.NewRecorder()
			fx.handler(newNextProbeHandler(probe)).ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status: want 200, got %d", rec.Code)
			}
			if !probe.hasClaims {
				t.Fatalf("AuthClaims missing on happy path")
			}
			// Session.Console が AuthClaims.Console に転記されていること
			if probe.gotClaims.Console != tc.wantConsoleSent {
				t.Errorf("claims.Console: want %q, got %q (Session.Console → AuthClaims.Console 転記失敗)",
					tc.wantConsoleSent, probe.gotClaims.Console)
			}
			// SessionHashPrefix は HashPrefix(HashToken(raw)) と一致し、
			// raw token そのものは含まない（NFR 1.1 / NFR 4.2）
			wantPrefix := HashPrefix(HashToken(testMWRawSessionToken))
			if probe.gotClaims.SessionHashPrefix != wantPrefix {
				t.Errorf("claims.SessionHashPrefix: want %q, got %q", wantPrefix, probe.gotClaims.SessionHashPrefix)
			}
			if strings.Contains(probe.gotClaims.SessionHashPrefix, testMWRawSessionToken) {
				t.Errorf("claims.SessionHashPrefix leaks raw session token: %q", probe.gotClaims.SessionHashPrefix)
			}
		})
	}
}

// (e2) 成功時 SuperAdmin / 空 Roles の転記も正しい
func TestMiddleware_HappyPath_SuperAdminAndEmptyRoles(t *testing.T) {
	fx := newMWFixture(t, oidc.ConsoleAdmin)
	adminUserID := uuid.New()
	fx.svc.lookupIdentity = Identity{
		AdminUserID:  adminUserID,
		TenantID:     uuid.Nil, // SuperAdmin
		Roles:        nil,
		IsSuperAdmin: true,
	}
	fx.svc.lookupSession = Session{
		TokenHash: HashToken(testMWRawSessionToken),
		Console:   oidc.ConsoleAdmin,
	}
	probe := &nextProbe{}

	req := httptest.NewRequest(http.MethodGet, "/api/admin/foo", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: testMWRawSessionToken})
	rec := httptest.NewRecorder()
	fx.handler(newNextProbeHandler(probe)).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	if !probe.hasClaims {
		t.Fatalf("AuthClaims missing")
	}
	if probe.gotClaims.TenantID != uuid.Nil {
		t.Errorf("claims.TenantID: want Nil, got %s", probe.gotClaims.TenantID)
	}
	if !probe.gotClaims.IsSuperAdmin {
		t.Errorf("claims.IsSuperAdmin: want true, got false")
	}
}

// ============================================================================
// (f) 想定外 panic で fail-closed: middleware は panic を握りつぶさず外側 Recoverer に委ねる
//     （Service が panic した場合に next 未到達であることだけを確認）
// ============================================================================

func TestMiddleware_ServicePanic_NextNotInvoked(t *testing.T) {
	fx := newMWFixture(t, oidc.ConsoleTenant)
	panickingSvc := &panickingService{}
	mw := NewMiddleware(panickingSvc, oidc.ConsoleTenant, fx.log, fx.clock)
	probe := &nextProbe{}

	req := httptest.NewRequest(http.MethodGet, "/api/foo", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: testMWRawSessionToken})
	rec := httptest.NewRecorder()

	defer func() {
		_ = recover() // 外側 Recoverer の役割を test harness が担う
		if probe.called {
			t.Errorf("next handler should not be invoked when service panics")
		}
	}()
	mw(newNextProbeHandler(probe)).ServeHTTP(rec, req)
}

// panickingService は LookupAndRefresh で必ず panic する fake.
type panickingService struct{}

func (panickingService) BeginLogin(_ context.Context, _ oidc.Console, _ string) (string, http.Cookie, error) {
	panic("unused")
}
func (panickingService) HandleCallback(_ context.Context, _ oidc.Console, _, _, _ string) (string, http.Cookie, string, error) {
	panic("unused")
}
func (panickingService) LookupAndRefresh(_ context.Context, _ string, _ oidc.Console, _ time.Time) (Identity, Session, error) {
	panic("simulated unexpected failure")
}
func (panickingService) Logout(_ context.Context, _ string) error { panic("unused") }

// ============================================================================
// 機密値の非埋込: log field 値 / response body に session token / state MAC 鍵が漏れない
// ============================================================================

func TestMiddleware_FailurePaths_DoNotLeakSensitiveValues(t *testing.T) {
	fx := newMWFixture(t, oidc.ConsoleTenant)
	fx.svc.lookupErr = pkgerrors.Wrap(
		pkgerrors.CodeUnauthenticated,
		"session idle timeout exceeded",
		FailureKindSessionIdle,
	)
	probe := &nextProbe{}

	req := httptest.NewRequest(http.MethodGet, "/api/foo", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: testMWRawSessionToken})
	rec := httptest.NewRecorder()
	fx.handler(newNextProbeHandler(probe)).ServeHTTP(rec, req)

	// response body に raw session token / state MAC 鍵が含まれない
	body := rec.Body.String()
	for _, sensitive := range []string{testMWRawSessionToken, testMWStateMACSecret} {
		if strings.Contains(body, sensitive) {
			t.Errorf("response body leaks sensitive value %q: %s", sensitive, body)
		}
	}
	// log field 値（map に flatten 済み）にも生 token / 鍵が含まれない
	fx.log.mu.Lock()
	defer fx.log.mu.Unlock()
	for _, e := range fx.log.entries {
		for k, v := range e.Fields {
			s, ok := v.(string)
			if !ok {
				continue
			}
			if strings.Contains(s, testMWRawSessionToken) {
				t.Errorf("log field %s leaks raw session token: %q", k, s)
			}
			if strings.Contains(s, testMWStateMACSecret) {
				t.Errorf("log field %s leaks state mac secret: %q", k, s)
			}
		}
	}
}
