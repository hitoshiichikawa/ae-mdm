package auth

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/oidc"
)

// テスト共通定数 - 機密値の漏洩 assertion のための fixed strings
const (
	testServiceStateSecret    = "test-service-state-secret-0123456789abcdef"
	testTenantClientSecret    = "tenant-client-secret-DO-NOT-LEAK"
	testAdminClientSecret     = "admin-client-secret-DO-NOT-LEAK"
	testRawSessionTokenValue  = "fake-raw-session-token-DO-NOT-LEAK"
	testRawIDTokenValue       = "fake-raw-id-token.DO.NOT.LEAK.JWT"
	testTenantClientIDService = "tenant-console-svc"
	testAdminClientIDService  = "admin-console-svc"
	testTenantIssuerURL       = "https://idp.example.com/tenant"
	testAdminIssuerURL        = "https://idp.example.com/admin"
)

// ---- Fake Logger ----

// fakeLogger records all field key/value pairs for assertion of failure_kind etc.
type fakeLogger struct {
	mu      sync.Mutex
	entries []fakeLogEntry
}

type fakeLogEntry struct {
	Level   string
	Msg     string
	Fields  map[string]any
}

func (l *fakeLogger) record(level, msg string, fields ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := map[string]any{}
	for i := 0; i+1 < len(fields); i += 2 {
		k, ok := fields[i].(string)
		if !ok {
			continue
		}
		f[k] = fields[i+1]
	}
	l.entries = append(l.entries, fakeLogEntry{Level: level, Msg: msg, Fields: f})
}

func (l *fakeLogger) Debug(msg string, fields ...any) { l.record("debug", msg, fields...) }
func (l *fakeLogger) Info(msg string, fields ...any)  { l.record("info", msg, fields...) }
func (l *fakeLogger) Warn(msg string, fields ...any)  { l.record("warn", msg, fields...) }
func (l *fakeLogger) Error(msg string, fields ...any) { l.record("error", msg, fields...) }
func (l *fakeLogger) With(_ ...any) logger.Logger     { return l }
func (l *fakeLogger) Sync() error                     { return nil }

func (l *fakeLogger) hasWarnWithFailureKind(kind string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.entries {
		if e.Level != "warn" {
			continue
		}
		if v, ok := e.Fields["failure_kind"]; ok && v == kind {
			return true
		}
	}
	return false
}

// ---- Fake Clock ----

type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time { return c.now }

// ---- Fake Repository ----

type fakeRepoCalls struct {
	consumeStateNonce  int
	resolveAdminUser   int
	create             int
	get                int
	touch              int
	revoke             int
}

type fakeRepository struct {
	mu sync.Mutex

	// Configured behaviors
	consumeStateNonceErr error
	resolveAdminUserOut  Identity
	resolveAdminUserErr  error
	createErr            error

	// Get behavior
	getSession  Session
	getIdentity Identity
	getErr      error

	touchErr  error
	revokeErr error

	// Recorded inputs
	calls            fakeRepoCalls
	lastConsumeNonce string
	lastCreateSess   Session
	lastTouchHash    string
	lastTouchNow     time.Time
	lastRevokeHash   string
	lastRevokeNow    time.Time
}

func (r *fakeRepository) ConsumeStateNonce(_ context.Context, nonce string, _ oidc.Console, _ time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls.consumeStateNonce++
	r.lastConsumeNonce = nonce
	return r.consumeStateNonceErr
}

func (r *fakeRepository) ResolveAdminUser(_ context.Context, _, _, _ string, _ oidc.Console) (Identity, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls.resolveAdminUser++
	if r.resolveAdminUserErr != nil {
		return Identity{}, r.resolveAdminUserErr
	}
	return r.resolveAdminUserOut, nil
}

func (r *fakeRepository) Create(_ context.Context, s Session) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls.create++
	r.lastCreateSess = s
	return r.createErr
}

func (r *fakeRepository) Get(_ context.Context, _ string) (Session, Identity, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls.get++
	if r.getErr != nil {
		return Session{}, Identity{}, r.getErr
	}
	return r.getSession, r.getIdentity, nil
}

func (r *fakeRepository) Touch(_ context.Context, hash string, now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls.touch++
	r.lastTouchHash = hash
	r.lastTouchNow = now
	return r.touchErr
}

func (r *fakeRepository) Revoke(_ context.Context, hash string, now time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls.revoke++
	r.lastRevokeHash = hash
	r.lastRevokeNow = now
	return r.revokeErr
}

// ---- Fake Verifier ----

type fakeVerifier struct {
	mu sync.Mutex

	claims oidc.Claims
	err    error
	calls  int
}

func (v *fakeVerifier) VerifyIDToken(_ context.Context, _ string) (oidc.Claims, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.calls++
	if v.err != nil {
		return oidc.Claims{}, v.err
	}
	return v.claims, nil
}

func (v *fakeVerifier) TenantEndpoint() oauth2.Endpoint { return oauth2.Endpoint{} }
func (v *fakeVerifier) AdminEndpoint() oauth2.Endpoint  { return oauth2.Endpoint{} }

// ---- Fake oauth2 token endpoint ----

// newMockTokenServer constructs an httptest server that always returns the configured
// id_token in the JSON token response.
func newMockTokenServer(t *testing.T, idToken string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := map[string]any{
			"access_token": "fake-access-token",
			"token_type":   "Bearer",
			"id_token":     idToken,
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newMockTokenServerNoIDToken returns a token response that omits id_token (upstream_oidc_token path).
func newMockTokenServerNoIDToken(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := map[string]any{
			"access_token": "fake-access-token",
			"token_type":   "Bearer",
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newMockTokenServer500 returns 500 on every token endpoint hit.
func newMockTokenServer500(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream failure", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ---- Test helpers ----

func makeTestConfig() config.Config {
	return config.Config{
		StateMACSecret:         testServiceStateSecret,
		StateCookieTTL:         10 * time.Minute,
		SessionAbsoluteTimeout: 8 * time.Hour,
		SessionIdleTimeout:     30 * time.Minute,
		OIDCTenantClientID:     testTenantClientIDService,
		OIDCAdminClientID:      testAdminClientIDService,
		OIDCTenantClientSecret: testTenantClientSecret,
		OIDCAdminClientSecret:  testAdminClientSecret,
		OIDCTenantIssuerURL:    testTenantIssuerURL,
		OIDCAdminIssuerURL:     testAdminIssuerURL,
	}
}

// makeOAuth2Configs builds tenant + admin oauth2.Config maps using the mock token server URL.
func makeOAuth2Configs(tokenURL string) map[oidc.Console]*oauth2.Config {
	return map[oidc.Console]*oauth2.Config{
		oidc.ConsoleTenant: {
			ClientID:     testTenantClientIDService,
			ClientSecret: testTenantClientSecret,
			RedirectURL:  "https://app.example.com/api/auth/callback",
			Scopes:       []string{"openid", "email", "profile"},
			Endpoint: oauth2.Endpoint{
				AuthURL:   "https://idp.example.com/tenant/auth",
				TokenURL:  tokenURL,
				AuthStyle: oauth2.AuthStyleInHeader,
			},
		},
		oidc.ConsoleAdmin: {
			ClientID:     testAdminClientIDService,
			ClientSecret: testAdminClientSecret,
			RedirectURL:  "https://app.example.com/api/admin/auth/callback",
			Scopes:       []string{"openid", "email", "profile"},
			Endpoint: oauth2.Endpoint{
				AuthURL:   "https://idp.example.com/admin/auth",
				TokenURL:  tokenURL,
				AuthStyle: oauth2.AuthStyleInHeader,
			},
		},
	}
}

// makeServiceWith builds a service with all dependencies and returns it together with
// the underlying fakes for inspection.
type serviceFixture struct {
	svc      Service
	cfg      config.Config
	repo     *fakeRepository
	verifier *fakeVerifier
	clock    *fakeClock
	log      *fakeLogger
	oauth2   map[oidc.Console]*oauth2.Config
	tokenGen TokenGenerator
}

func newServiceFixture(t *testing.T, tokenURL string) *serviceFixture {
	t.Helper()
	cfg := makeTestConfig()
	repo := &fakeRepository{}
	verifier := &fakeVerifier{}
	clk := &fakeClock{now: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	log := &fakeLogger{}
	tokenGen := func() (string, error) { return testRawSessionTokenValue, nil }
	configs := makeOAuth2Configs(tokenURL)
	svc := NewService(cfg, verifier, repo, configs, clk, tokenGen, log)
	return &serviceFixture{
		svc:      svc,
		cfg:      cfg,
		repo:     repo,
		verifier: verifier,
		clock:    clk,
		log:      log,
		oauth2:   configs,
		tokenGen: tokenGen,
	}
}

// signValidState builds a state cookie value for the given console / oidc nonce.
// Returns the cookie value (for setting on the request) and the state Nonce string (for query).
func signValidState(t *testing.T, cfg config.Config, console oidc.Console, oidcNonce, returnTo string, issuedAt time.Time) (cookieValue, queryNonce string) {
	t.Helper()
	payload := StatePayload{
		Nonce:     "test-state-nonce-1234567890ab",
		OIDCNonce: oidcNonce,
		Console:   console,
		ReturnTo:  returnTo,
		IssuedAt:  issuedAt,
	}
	v, err := Sign(payload, []byte(cfg.StateMACSecret))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return v, payload.Nonce
}

func assertFailureKind(t *testing.T, err error, want failureKind) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with failure_kind=%s, got nil", want)
	}
	if !stderrors.Is(err, want) {
		t.Fatalf("expected failure_kind=%s in Cause chain, got: %v", want, err)
	}
}

func assertErrorCode(t *testing.T, err error, want pkgerrors.Code) {
	t.Helper()
	var pe *pkgerrors.Error
	if !stderrors.As(err, &pe) {
		t.Fatalf("expected *errors.Error, got %T: %v", err, err)
	}
	if pe.Code != want {
		t.Fatalf("expected Code=%s, got Code=%s (err=%v)", want, pe.Code, err)
	}
}

// ---- Tests: BeginLogin ----

// (a) BeginLogin: returnTo "/" 受理（正常系）
func TestBeginLogin_ValidReturnTo_ReturnsRedirectAndCookie(t *testing.T) {
	// Arrange
	srv := newMockTokenServer(t, testRawIDTokenValue)
	fx := newServiceFixture(t, srv.URL)

	// Act
	redirectURL, cookie, err := fx.svc.BeginLogin(context.Background(), oidc.ConsoleTenant, "/dashboard")

	// Assert
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	if redirectURL == "" {
		t.Fatalf("redirectURL empty")
	}
	u, parseErr := url.Parse(redirectURL)
	if parseErr != nil {
		t.Fatalf("parse redirectURL: %v", parseErr)
	}
	// state クエリ + nonce クエリの双方が含まれる
	q := u.Query()
	if q.Get("state") == "" {
		t.Errorf("redirectURL missing state query")
	}
	if q.Get("nonce") == "" {
		t.Errorf("redirectURL missing nonce query")
	}
	if q.Get("state") == q.Get("nonce") {
		t.Errorf("state and nonce must be different values (independent generation)")
	}
	if cookie.Name != stateCookieName {
		t.Errorf("cookie.Name = %q, want %q", cookie.Name, stateCookieName)
	}
	if cookie.Value == "" {
		t.Errorf("cookie.Value is empty")
	}
	if cookie.MaxAge != int(fx.cfg.StateCookieTTL/time.Second) {
		t.Errorf("cookie.MaxAge = %d, want %d", cookie.MaxAge, int(fx.cfg.StateCookieTTL/time.Second))
	}
}

// (a2) BeginLogin: 空文字 returnTo は default "/"
func TestBeginLogin_EmptyReturnTo_DefaultsToRoot(t *testing.T) {
	srv := newMockTokenServer(t, testRawIDTokenValue)
	fx := newServiceFixture(t, srv.URL)

	redirectURL, cookie, err := fx.svc.BeginLogin(context.Background(), oidc.ConsoleTenant, "")
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	// redirectURL の state クエリパラメータ = Sign 時の payload.Nonce
	u, parseErr := url.Parse(redirectURL)
	if parseErr != nil {
		t.Fatalf("parse redirectURL: %v", parseErr)
	}
	stateNonce := u.Query().Get("state")
	if stateNonce == "" {
		t.Fatalf("state query missing")
	}
	// 同じ Nonce を queryState として渡せば Verify が成功して payload を取り出せる
	payload, err := Verify(cookie.Value, stateNonce, []byte(fx.cfg.StateMACSecret), fx.cfg.StateCookieTTL, fx.clock.now)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if payload.ReturnTo != "/" {
		t.Errorf("ReturnTo = %q, want \"/\" (default)", payload.ReturnTo)
	}
}

// (a3) BeginLogin: open redirect 候補 `//evil.example` で 400
func TestBeginLogin_OpenRedirectCandidate_Rejected(t *testing.T) {
	srv := newMockTokenServer(t, testRawIDTokenValue)
	fx := newServiceFixture(t, srv.URL)

	_, _, err := fx.svc.BeginLogin(context.Background(), oidc.ConsoleTenant, "//evil.example/path")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	assertErrorCode(t, err, pkgerrors.CodeInvalidRequest)
	assertFailureKind(t, err, FailureKindReturnToInvalid)
}

// (a4) BeginLogin: `http://evil.example` で 400
func TestBeginLogin_AbsoluteURL_Rejected(t *testing.T) {
	srv := newMockTokenServer(t, testRawIDTokenValue)
	fx := newServiceFixture(t, srv.URL)

	_, _, err := fx.svc.BeginLogin(context.Background(), oidc.ConsoleTenant, "http://evil.example/path")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	assertErrorCode(t, err, pkgerrors.CodeInvalidRequest)
}

// (a5) BeginLogin: 相対パス "dashboard"（"/" で始まらない）で 400
func TestBeginLogin_RelativePathWithoutLeadingSlash_Rejected(t *testing.T) {
	srv := newMockTokenServer(t, testRawIDTokenValue)
	fx := newServiceFixture(t, srv.URL)

	_, _, err := fx.svc.BeginLogin(context.Background(), oidc.ConsoleTenant, "dashboard")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	assertErrorCode(t, err, pkgerrors.CodeInvalidRequest)
}

// ---- Tests: HandleCallback ----

// (b) HandleCallback 正常系: session 作成 + cookie + returnTo
func TestHandleCallback_HappyPath_CreatesSessionAndReturnsCookie(t *testing.T) {
	// Arrange
	srv := newMockTokenServer(t, testRawIDTokenValue)
	fx := newServiceFixture(t, srv.URL)

	oidcNonce := "test-oidc-nonce-ABC123"
	cookieVal, queryState := signValidState(t, fx.cfg, oidc.ConsoleTenant, oidcNonce, "/dashboard", fx.clock.now)

	fx.verifier.claims = oidc.Claims{
		Subject:        "sub-123",
		Email:          "admin@example.com",
		Issuer:         testTenantIssuerURL,
		MatchedConsole: oidc.ConsoleTenant,
		Nonce:          oidcNonce,
	}
	adminUserID := uuid.New()
	tenantID := uuid.New()
	fx.repo.resolveAdminUserOut = Identity{
		AdminUserID:  adminUserID,
		OIDCSubject:  "sub-123",
		Email:        "admin@example.com",
		TenantID:     tenantID,
		Roles:        []string{"TenantAdmin"},
		IsSuperAdmin: false,
	}

	// Act
	rawToken, cookie, returnTo, err := fx.svc.HandleCallback(
		context.Background(), oidc.ConsoleTenant, "fake-auth-code", queryState, cookieVal,
	)

	// Assert
	if err != nil {
		t.Fatalf("HandleCallback: %v", err)
	}
	if rawToken != testRawSessionTokenValue {
		t.Errorf("rawToken = %q, want %q", rawToken, testRawSessionTokenValue)
	}
	if cookie.Name != sessionCookieName {
		t.Errorf("cookie.Name = %q, want %q", cookie.Name, sessionCookieName)
	}
	if cookie.Value != testRawSessionTokenValue {
		t.Errorf("cookie.Value = %q, want %q", cookie.Value, testRawSessionTokenValue)
	}
	if returnTo != "/dashboard" {
		t.Errorf("returnTo = %q, want %q", returnTo, "/dashboard")
	}
	if fx.repo.calls.create != 1 {
		t.Errorf("repo.Create called %d times, want 1", fx.repo.calls.create)
	}
	if fx.repo.calls.consumeStateNonce != 1 {
		t.Errorf("repo.ConsumeStateNonce called %d times, want 1", fx.repo.calls.consumeStateNonce)
	}
	if fx.repo.lastCreateSess.AdminUserID != adminUserID {
		t.Errorf("Session.AdminUserID mismatch")
	}
	if fx.repo.lastCreateSess.Console != oidc.ConsoleTenant {
		t.Errorf("Session.Console = %s, want %s", fx.repo.lastCreateSess.Console, oidc.ConsoleTenant)
	}
	if !fx.repo.lastCreateSess.ExpiresAt.Equal(fx.clock.now.Add(fx.cfg.SessionAbsoluteTimeout)) {
		t.Errorf("Session.ExpiresAt mismatch: %v", fx.repo.lastCreateSess.ExpiresAt)
	}
}

// (c) state mismatch（query state ≠ cookie Nonce）
func TestHandleCallback_StateMismatch_Returns401AndDoesNotExchange(t *testing.T) {
	srv := newMockTokenServer(t, testRawIDTokenValue)
	fx := newServiceFixture(t, srv.URL)
	cookieVal, _ := signValidState(t, fx.cfg, oidc.ConsoleTenant, "oidc-nonce", "/dashboard", fx.clock.now)

	_, _, _, err := fx.svc.HandleCallback(
		context.Background(), oidc.ConsoleTenant, "code", "wrong-query-state", cookieVal,
	)
	assertErrorCode(t, err, pkgerrors.CodeUnauthenticated)
	assertFailureKind(t, err, FailureKindStateMismatch)
	if fx.repo.calls.consumeStateNonce != 0 {
		t.Errorf("ConsumeStateNonce should not be called on state mismatch")
	}
	if fx.repo.calls.create != 0 {
		t.Errorf("Create should not be called on state mismatch")
	}
}

// (c1) state expired
func TestHandleCallback_StateExpired_Returns401(t *testing.T) {
	srv := newMockTokenServer(t, testRawIDTokenValue)
	fx := newServiceFixture(t, srv.URL)
	issued := fx.clock.now.Add(-fx.cfg.StateCookieTTL).Add(-time.Second)
	cookieVal, queryState := signValidState(t, fx.cfg, oidc.ConsoleTenant, "oidc-nonce", "/dashboard", issued)

	_, _, _, err := fx.svc.HandleCallback(
		context.Background(), oidc.ConsoleTenant, "code", queryState, cookieVal,
	)
	assertFailureKind(t, err, FailureKindStateExpired)
}

// (c2) state cookie invalid (空文字)
func TestHandleCallback_EmptyStateCookie_Returns401(t *testing.T) {
	srv := newMockTokenServer(t, testRawIDTokenValue)
	fx := newServiceFixture(t, srv.URL)

	_, _, _, err := fx.svc.HandleCallback(
		context.Background(), oidc.ConsoleTenant, "code", "any-query-state", "",
	)
	assertFailureKind(t, err, FailureKindStateInvalid)
}

// (c3) StatePayload.Console != handler の console: state_console_mismatch
func TestHandleCallback_ConsoleMismatchOnState_Returns401(t *testing.T) {
	srv := newMockTokenServer(t, testRawIDTokenValue)
	fx := newServiceFixture(t, srv.URL)
	// tenant 用 state を admin callback に提示
	cookieVal, queryState := signValidState(t, fx.cfg, oidc.ConsoleTenant, "oidc-nonce", "/dashboard", fx.clock.now)

	_, _, _, err := fx.svc.HandleCallback(
		context.Background(), oidc.ConsoleAdmin, "code", queryState, cookieVal,
	)
	assertErrorCode(t, err, pkgerrors.CodeUnauthenticated)
	assertFailureKind(t, err, FailureKindStateConsoleMismatch)
	if fx.repo.calls.consumeStateNonce != 0 {
		t.Errorf("ConsumeStateNonce should not run on state_console_mismatch")
	}
}

// (c4) repo.ConsumeStateNonce が state_replay を返したら 401 で伝播、後段未呼出
func TestHandleCallback_StateReplay_Returns401AndDoesNotExchange(t *testing.T) {
	srv := newMockTokenServer(t, testRawIDTokenValue)
	fx := newServiceFixture(t, srv.URL)
	cookieVal, queryState := signValidState(t, fx.cfg, oidc.ConsoleTenant, "oidc-nonce", "/dashboard", fx.clock.now)
	fx.repo.consumeStateNonceErr = pkgerrors.Wrap(
		pkgerrors.CodeUnauthenticated,
		"state nonce replay detected",
		FailureKindStateReplay,
	)

	_, _, _, err := fx.svc.HandleCallback(
		context.Background(), oidc.ConsoleTenant, "code", queryState, cookieVal,
	)
	assertFailureKind(t, err, FailureKindStateReplay)
	if fx.verifier.calls != 0 {
		t.Errorf("verifier.VerifyIDToken should not be called on state_replay")
	}
	if fx.repo.calls.resolveAdminUser != 0 {
		t.Errorf("ResolveAdminUser should not be called on state_replay")
	}
	if fx.repo.calls.create != 0 {
		t.Errorf("Create should not be called on state_replay")
	}
}

// (c5) tokenGen が err を返したら csprng_failure で 500
func TestHandleCallback_TokenGenError_ReturnsCSPRNGFailure(t *testing.T) {
	srv := newMockTokenServer(t, testRawIDTokenValue)
	cfg := makeTestConfig()
	repo := &fakeRepository{}
	verifier := &fakeVerifier{
		claims: oidc.Claims{
			Subject:        "sub-123",
			Email:          "admin@example.com",
			Issuer:         testTenantIssuerURL,
			MatchedConsole: oidc.ConsoleTenant,
			Nonce:          "oidc-nonce",
		},
	}
	repo.resolveAdminUserOut = Identity{AdminUserID: uuid.New(), OIDCSubject: "sub-123"}
	clk := &fakeClock{now: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	log := &fakeLogger{}
	tokenGen := func() (string, error) { return "", fmt.Errorf("simulated csprng failure") }
	configs := makeOAuth2Configs(srv.URL)
	svc := NewService(cfg, verifier, repo, configs, clk, tokenGen, log)

	cookieVal, queryState := signValidState(t, cfg, oidc.ConsoleTenant, "oidc-nonce", "/dashboard", clk.now)

	_, _, _, err := svc.HandleCallback(
		context.Background(), oidc.ConsoleTenant, "code", queryState, cookieVal,
	)
	assertErrorCode(t, err, pkgerrors.CodeInternal)
	assertFailureKind(t, err, FailureKindCSPRNGFailure)
	if repo.calls.create != 0 {
		t.Errorf("repo.Create should not run when tokenGen fails")
	}
}

// (d) Claims.MatchedConsole != console → invalid_aud
func TestHandleCallback_AudConsoleMismatch_Returns401(t *testing.T) {
	srv := newMockTokenServer(t, testRawIDTokenValue)
	fx := newServiceFixture(t, srv.URL)
	cookieVal, queryState := signValidState(t, fx.cfg, oidc.ConsoleTenant, "oidc-nonce", "/dashboard", fx.clock.now)

	fx.verifier.claims = oidc.Claims{
		Subject:        "sub-123",
		MatchedConsole: oidc.ConsoleAdmin, // mismatched
		Nonce:          "oidc-nonce",
	}

	_, _, _, err := fx.svc.HandleCallback(
		context.Background(), oidc.ConsoleTenant, "code", queryState, cookieVal,
	)
	assertFailureKind(t, err, FailureKindInvalidAud)
	if fx.repo.calls.create != 0 {
		t.Errorf("Create should not run on aud mismatch")
	}
}

// (d2) Claims.Nonce != payload.OIDCNonce → nonce_mismatch
func TestHandleCallback_NonceMismatch_Returns401(t *testing.T) {
	srv := newMockTokenServer(t, testRawIDTokenValue)
	fx := newServiceFixture(t, srv.URL)
	cookieVal, queryState := signValidState(t, fx.cfg, oidc.ConsoleTenant, "expected-oidc-nonce", "/dashboard", fx.clock.now)

	fx.verifier.claims = oidc.Claims{
		Subject:        "sub-123",
		MatchedConsole: oidc.ConsoleTenant,
		Nonce:          "attacker-different-nonce", // mismatched
	}

	_, _, _, err := fx.svc.HandleCallback(
		context.Background(), oidc.ConsoleTenant, "code", queryState, cookieVal,
	)
	assertFailureKind(t, err, FailureKindNonceMismatch)
	if fx.repo.calls.create != 0 {
		t.Errorf("Create should not run on nonce mismatch")
	}
}

// (d3) Claims.Nonce == "" → nonce_mismatch (IdP nonce 不返却の fail-closed)
func TestHandleCallback_EmptyIDTokenNonce_Returns401(t *testing.T) {
	srv := newMockTokenServer(t, testRawIDTokenValue)
	fx := newServiceFixture(t, srv.URL)
	cookieVal, queryState := signValidState(t, fx.cfg, oidc.ConsoleTenant, "expected-oidc-nonce", "/dashboard", fx.clock.now)

	fx.verifier.claims = oidc.Claims{
		Subject:        "sub-123",
		MatchedConsole: oidc.ConsoleTenant,
		Nonce:          "", // IdP did not return nonce
	}

	_, _, _, err := fx.svc.HandleCallback(
		context.Background(), oidc.ConsoleTenant, "code", queryState, cookieVal,
	)
	assertFailureKind(t, err, FailureKindNonceMismatch)
}

// (e) ResolveAdminUser が admin_user_not_provisioned を返したら 403 で伝播、session 作成未到達
func TestHandleCallback_AdminUserNotProvisioned_Returns403(t *testing.T) {
	srv := newMockTokenServer(t, testRawIDTokenValue)
	fx := newServiceFixture(t, srv.URL)
	cookieVal, queryState := signValidState(t, fx.cfg, oidc.ConsoleTenant, "oidc-nonce", "/dashboard", fx.clock.now)
	fx.verifier.claims = oidc.Claims{
		Subject:        "sub-123",
		MatchedConsole: oidc.ConsoleTenant,
		Nonce:          "oidc-nonce",
	}
	fx.repo.resolveAdminUserErr = pkgerrors.Wrap(
		pkgerrors.CodeForbidden,
		"admin user not provisioned",
		FailureKindAdminUserNotProvisioned,
	)

	_, _, _, err := fx.svc.HandleCallback(
		context.Background(), oidc.ConsoleTenant, "code", queryState, cookieVal,
	)
	assertErrorCode(t, err, pkgerrors.CodeForbidden)
	assertFailureKind(t, err, FailureKindAdminUserNotProvisioned)
	if fx.repo.calls.create != 0 {
		t.Errorf("Create should not run when admin user not provisioned")
	}
}

// (e2) token endpoint が id_token を返さない → upstream_oidc_token
func TestHandleCallback_NoIDToken_ReturnsUpstreamFailure(t *testing.T) {
	srv := newMockTokenServerNoIDToken(t)
	fx := newServiceFixture(t, srv.URL)
	cookieVal, queryState := signValidState(t, fx.cfg, oidc.ConsoleTenant, "oidc-nonce", "/dashboard", fx.clock.now)

	_, _, _, err := fx.svc.HandleCallback(
		context.Background(), oidc.ConsoleTenant, "code", queryState, cookieVal,
	)
	assertErrorCode(t, err, pkgerrors.CodeUpstream)
	assertFailureKind(t, err, FailureKindUpstreamOIDCToken)
}

// (e3) token endpoint 5xx → upstream_oidc_token
func TestHandleCallback_TokenEndpoint5xx_ReturnsUpstreamFailure(t *testing.T) {
	srv := newMockTokenServer500(t)
	fx := newServiceFixture(t, srv.URL)
	cookieVal, queryState := signValidState(t, fx.cfg, oidc.ConsoleTenant, "oidc-nonce", "/dashboard", fx.clock.now)

	_, _, _, err := fx.svc.HandleCallback(
		context.Background(), oidc.ConsoleTenant, "code", queryState, cookieVal,
	)
	assertErrorCode(t, err, pkgerrors.CodeUpstream)
	assertFailureKind(t, err, FailureKindUpstreamOIDCToken)
}

// ---- Tests: LookupAndRefresh ----

func makeValidSession(now time.Time) Session {
	return Session{
		TokenHash:   HashToken(testRawSessionTokenValue),
		AdminUserID: uuid.New(),
		Console:     oidc.ConsoleTenant,
		IssuedAt:    now.Add(-1 * time.Hour),
		LastSeenAt:  now.Add(-1 * time.Minute),
		ExpiresAt:   now.Add(7 * time.Hour),
		RevokedAt:   nil,
	}
}

// (f) LookupAndRefresh idle 境界: 29:59 → OK, 30:00 → OK, 30:01 → session_idle
func TestLookupAndRefresh_IdleBoundaries(t *testing.T) {
	cases := []struct {
		name         string
		idleDelta    time.Duration
		wantErr      bool
		wantFailKind failureKind
	}{
		{"idle_29m59s_accepted", 29*time.Minute + 59*time.Second, false, ""},
		{"idle_30m_accepted", 30 * time.Minute, false, ""},
		{"idle_30m1s_rejected", 30*time.Minute + 1*time.Second, true, FailureKindSessionIdle},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newMockTokenServer(t, testRawIDTokenValue)
			fx := newServiceFixture(t, srv.URL)
			now := fx.clock.now
			sess := makeValidSession(now)
			sess.LastSeenAt = now.Add(-tc.idleDelta)
			fx.repo.getSession = sess
			fx.repo.getIdentity = Identity{AdminUserID: sess.AdminUserID}

			_, _, err := fx.svc.LookupAndRefresh(context.Background(), testRawSessionTokenValue, oidc.ConsoleTenant, now)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				assertFailureKind(t, err, tc.wantFailKind)
				if fx.repo.calls.revoke == 0 {
					t.Errorf("expected Revoke to be called on idle expiry (Req 4.6)")
				}
			} else {
				if err != nil {
					t.Fatalf("expected nil, got: %v", err)
				}
				if fx.repo.calls.touch != 1 {
					t.Errorf("expected Touch on success, got calls=%d", fx.repo.calls.touch)
				}
			}
		})
	}
}

// (f2) absolute boundary: 7:59:59 OK / 8:00:00 OK / 8:00:01 rejected
func TestLookupAndRefresh_AbsoluteBoundaries(t *testing.T) {
	cases := []struct {
		name          string
		expiresOffset time.Duration
		wantErr       bool
		wantFailKind  failureKind
	}{
		{"absolute_remaining_1s_accepted", 1 * time.Second, false, ""},
		{"absolute_remaining_0_accepted", 0, false, ""},
		{"absolute_passed_1s_rejected", -1 * time.Second, true, FailureKindSessionExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newMockTokenServer(t, testRawIDTokenValue)
			fx := newServiceFixture(t, srv.URL)
			now := fx.clock.now
			sess := makeValidSession(now)
			sess.ExpiresAt = now.Add(tc.expiresOffset)
			fx.repo.getSession = sess
			fx.repo.getIdentity = Identity{AdminUserID: sess.AdminUserID}

			_, _, err := fx.svc.LookupAndRefresh(context.Background(), testRawSessionTokenValue, oidc.ConsoleTenant, now)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				assertFailureKind(t, err, tc.wantFailKind)
				if fx.repo.calls.revoke == 0 {
					t.Errorf("expected Revoke to be called on absolute expiry (Req 4.6)")
				}
			} else {
				if err != nil {
					t.Fatalf("expected nil, got: %v", err)
				}
			}
		})
	}
}

// (f3) revoked_at != nil → session_revoked
func TestLookupAndRefresh_Revoked_Returns401(t *testing.T) {
	srv := newMockTokenServer(t, testRawIDTokenValue)
	fx := newServiceFixture(t, srv.URL)
	now := fx.clock.now
	sess := makeValidSession(now)
	revokedAt := now.Add(-1 * time.Minute)
	sess.RevokedAt = &revokedAt
	fx.repo.getSession = sess
	fx.repo.getIdentity = Identity{AdminUserID: sess.AdminUserID}

	_, _, err := fx.svc.LookupAndRefresh(context.Background(), testRawSessionTokenValue, oidc.ConsoleTenant, now)
	assertErrorCode(t, err, pkgerrors.CodeUnauthenticated)
	assertFailureKind(t, err, FailureKindSessionRevoked)
}

// (f4) console mismatch → console_mismatch
func TestLookupAndRefresh_ConsoleMismatch_Returns401(t *testing.T) {
	srv := newMockTokenServer(t, testRawIDTokenValue)
	fx := newServiceFixture(t, srv.URL)
	now := fx.clock.now
	sess := makeValidSession(now)
	sess.Console = oidc.ConsoleTenant // session is tenant
	fx.repo.getSession = sess
	fx.repo.getIdentity = Identity{AdminUserID: sess.AdminUserID}

	// expected = admin → mismatch
	_, _, err := fx.svc.LookupAndRefresh(context.Background(), testRawSessionTokenValue, oidc.ConsoleAdmin, now)
	assertErrorCode(t, err, pkgerrors.CodeUnauthenticated)
	assertFailureKind(t, err, FailureKindConsoleMismatch)
	if fx.repo.calls.revoke == 0 {
		t.Errorf("expected Revoke to be called on console_mismatch (Req 4.6)")
	}
}

// (f4b) expectedConsole == ConsoleAny の場合は console 照合を skip し、tenant-console /
// admin-console いずれの session でも success する（Issue #37 / Req 2.4: 後段 guard が 403 を返す）。
// session 自体の他チェック（expiry / revoked / idle）は通常通り通過必要。
func TestLookupAndRefresh_ConsoleAny_SkipsConsoleMatching(t *testing.T) {
	cases := []struct {
		name           string
		sessionConsole oidc.Console
	}{
		{"tenant_console_session", oidc.ConsoleTenant},
		{"admin_console_session", oidc.ConsoleAdmin},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newMockTokenServer(t, testRawIDTokenValue)
			fx := newServiceFixture(t, srv.URL)
			now := fx.clock.now
			sess := makeValidSession(now)
			sess.Console = tc.sessionConsole
			fx.repo.getSession = sess
			fx.repo.getIdentity = Identity{AdminUserID: sess.AdminUserID}

			_, gotSess, err := fx.svc.LookupAndRefresh(context.Background(), testRawSessionTokenValue, oidc.ConsoleAny, now)
			if err != nil {
				t.Fatalf("ConsoleAny で console 照合が走った: %v", err)
			}
			// session.Console は revoke / refresh で改変されず、元の値が呼び出し側まで届く
			if gotSess.Console != tc.sessionConsole {
				t.Errorf("session.Console = %s; want %s", gotSess.Console, tc.sessionConsole)
			}
			if fx.repo.calls.revoke != 0 {
				t.Errorf("ConsoleAny の success 経路で revoke が呼ばれてはならない: revoke=%d", fx.repo.calls.revoke)
			}
		})
	}
}

// (f5) repo.Get が session_tamper を返したら 401 で伝播
func TestLookupAndRefresh_SessionTamper_Returns401(t *testing.T) {
	srv := newMockTokenServer(t, testRawIDTokenValue)
	fx := newServiceFixture(t, srv.URL)
	now := fx.clock.now
	fx.repo.getErr = pkgerrors.Wrap(
		pkgerrors.CodeUnauthenticated,
		"session not found",
		FailureKindSessionTamper,
	)

	_, _, err := fx.svc.LookupAndRefresh(context.Background(), testRawSessionTokenValue, oidc.ConsoleTenant, now)
	assertFailureKind(t, err, FailureKindSessionTamper)
}

// ---- Tests: Logout ----

// (g) Logout で Revoke 1 回呼ばれる
func TestLogout_CallsRevokeOnce(t *testing.T) {
	srv := newMockTokenServer(t, testRawIDTokenValue)
	fx := newServiceFixture(t, srv.URL)

	if err := fx.svc.Logout(context.Background(), testRawSessionTokenValue); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if fx.repo.calls.revoke != 1 {
		t.Errorf("Revoke called %d times, want 1", fx.repo.calls.revoke)
	}
	expected := HashToken(testRawSessionTokenValue)
	if fx.repo.lastRevokeHash != expected {
		t.Errorf("Revoke called with hash=%q, want %q", fx.repo.lastRevokeHash, expected)
	}
}

// (g2) Logout: 2 度目は repository.Revoke の冪等性に委ねる（呼ぶこと自体は OK）
func TestLogout_SecondCallStillCallsRevoke(t *testing.T) {
	srv := newMockTokenServer(t, testRawIDTokenValue)
	fx := newServiceFixture(t, srv.URL)
	_ = fx.svc.Logout(context.Background(), testRawSessionTokenValue)
	_ = fx.svc.Logout(context.Background(), testRawSessionTokenValue)
	if fx.repo.calls.revoke != 2 {
		t.Errorf("Revoke called %d times, want 2 (idempotency is repository-level)", fx.repo.calls.revoke)
	}
}

// (g3) Logout: 空文字 token は no-op
func TestLogout_EmptyToken_IsNoOp(t *testing.T) {
	srv := newMockTokenServer(t, testRawIDTokenValue)
	fx := newServiceFixture(t, srv.URL)
	if err := fx.svc.Logout(context.Background(), ""); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if fx.repo.calls.revoke != 0 {
		t.Errorf("Revoke should not be called for empty token")
	}
}

// ---- Tests: log field assertions ----

// (h) 各失敗パスで log.Warn に failure_kind が明示的に渡される
func TestService_LogFailureKindOnFailure(t *testing.T) {
	cases := []struct {
		name        string
		setupErr    func(fx *serviceFixture)
		wantKindStr string
	}{
		{
			name: "state_mismatch",
			setupErr: func(fx *serviceFixture) {
				// fake で wrong query state を渡す
			},
			wantKindStr: "state_mismatch",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newMockTokenServer(t, testRawIDTokenValue)
			fx := newServiceFixture(t, srv.URL)
			tc.setupErr(fx)
			cookieVal, _ := signValidState(t, fx.cfg, oidc.ConsoleTenant, "oidc-nonce", "/dashboard", fx.clock.now)

			_, _, _, _ = fx.svc.HandleCallback(
				context.Background(), oidc.ConsoleTenant, "code", "wrong-query-state", cookieVal,
			)
			if !fx.log.hasWarnWithFailureKind(tc.wantKindStr) {
				t.Errorf("expected log.Warn with failure_kind=%s, entries=%+v", tc.wantKindStr, fx.log.entries)
			}
		})
	}
}

// (h2) BeginLogin の return_to invalid でも log に failure_kind が乗る
func TestBeginLogin_LogsFailureKindOnReturnToInvalid(t *testing.T) {
	srv := newMockTokenServer(t, testRawIDTokenValue)
	fx := newServiceFixture(t, srv.URL)
	_, _, _ = fx.svc.BeginLogin(context.Background(), oidc.ConsoleTenant, "//evil.example")
	if !fx.log.hasWarnWithFailureKind("return_to_invalid") {
		t.Errorf("expected log.Warn with failure_kind=return_to_invalid, entries=%+v", fx.log.entries)
	}
}

// ---- Tests: 機密値の非埋込（NFR 1.1 / NFR 4.2） ----

// (i) HandleCallback の各失敗パスで error message に機密値が含まれない
func TestHandleCallback_SensitiveValuesNotEmbeddedInError(t *testing.T) {
	cases := []struct {
		name       string
		setup      func(fx *serviceFixture, t *testing.T) (cookie, query, code string)
		sensitives []string
	}{
		{
			name: "state_mismatch",
			setup: func(fx *serviceFixture, t *testing.T) (string, string, string) {
				cookieVal, _ := signValidState(t, fx.cfg, oidc.ConsoleTenant, "oidc-nonce", "/dashboard", fx.clock.now)
				return cookieVal, "wrong-query-state", "code"
			},
			sensitives: []string{testServiceStateSecret, testTenantClientSecret, testRawIDTokenValue},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newMockTokenServer(t, testRawIDTokenValue)
			fx := newServiceFixture(t, srv.URL)
			cookieVal, queryState, code := tc.setup(fx, t)
			_, _, _, err := fx.svc.HandleCallback(context.Background(), oidc.ConsoleTenant, code, queryState, cookieVal)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			msg := err.Error()
			for _, s := range tc.sensitives {
				if strings.Contains(msg, s) {
					t.Errorf("error message embeds sensitive value %q: %q", s, msg)
				}
			}
			// cookieVal を含まないことも確認
			if strings.Contains(msg, cookieVal) {
				t.Errorf("error message embeds cookie raw value: %q", msg)
			}
		})
	}
}

// (i2) BeginLogin の return_to invalid 経路で error message に return_to の生値を含めない
func TestBeginLogin_SensitiveValuesNotEmbeddedInError(t *testing.T) {
	srv := newMockTokenServer(t, testRawIDTokenValue)
	fx := newServiceFixture(t, srv.URL)
	maliciousReturnTo := "//evil.example/exfiltrate?secret=DO-NOT-LEAK"
	_, _, err := fx.svc.BeginLogin(context.Background(), oidc.ConsoleTenant, maliciousReturnTo)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	msg := err.Error()
	if strings.Contains(msg, "DO-NOT-LEAK") {
		t.Errorf("error message embeds return_to value: %q", msg)
	}
	// state secret / client secret も含まれない
	if strings.Contains(msg, testServiceStateSecret) {
		t.Errorf("error message embeds state secret: %q", msg)
	}
	if strings.Contains(msg, testTenantClientSecret) {
		t.Errorf("error message embeds tenant client secret: %q", msg)
	}
}

// (i3) LookupAndRefresh の失敗パスで session token / hash 全体を error message に含めない
func TestLookupAndRefresh_SensitiveValuesNotEmbeddedInError(t *testing.T) {
	srv := newMockTokenServer(t, testRawIDTokenValue)
	fx := newServiceFixture(t, srv.URL)
	now := fx.clock.now
	sess := makeValidSession(now)
	sess.LastSeenAt = now.Add(-31 * time.Minute) // idle expired
	fx.repo.getSession = sess
	fx.repo.getIdentity = Identity{AdminUserID: sess.AdminUserID}

	_, _, err := fx.svc.LookupAndRefresh(context.Background(), testRawSessionTokenValue, oidc.ConsoleTenant, now)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	msg := err.Error()
	if strings.Contains(msg, testRawSessionTokenValue) {
		t.Errorf("error message embeds raw session token: %q", msg)
	}
	// full hash も含めない（先頭 8 文字までは OK だが、全 64 文字は禁止）
	fullHash := HashToken(testRawSessionTokenValue)
	if strings.Contains(msg, fullHash) {
		t.Errorf("error message embeds full session hash: %q", msg)
	}
}
