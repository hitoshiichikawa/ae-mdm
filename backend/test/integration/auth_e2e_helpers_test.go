package integration_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	jose "github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"
	"github.com/jackc/pgx/v5/pgxpool"
	goidc "golang.org/x/oauth2"

	"github.com/hitoshiichikawa/ae-mdm/internal/auth"
	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/httpserver"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/oidc"
)

// 本ファイルは Issue #33 task 6.4 の auth e2e integration test 群が共通利用する
// helper を集約する（mock OIDC IdP / fake clock / recording logger / HTTP server 構築）。

// authE2EConst は auth e2e test で繰り返し使う固定文字列を集約する。
const (
	authE2ETenantClientID     = "tenant-console-e2e"
	authE2EAdminClientID      = "admin-console-e2e"
	authE2ETenantClientSecret = "tenant-secret-e2e-DO-NOT-USE-IN-PROD"
	authE2EAdminClientSecret  = "admin-secret-e2e-DO-NOT-USE-IN-PROD"
	authE2EIssuerPath         = "/realms/e2e"
	// 32 byte HMAC secret（state cookie MAC 用）。
	authE2EStateMACSecret = "0123456789abcdef0123456789abcdef"
	authE2ETenantCallback = "http://127.0.0.1:18080/api/auth/callback"
	authE2EAdminCallback  = "http://127.0.0.1:18081/api/admin/auth/callback"
)

// authE2EConfig は test 用 config を構築する。timeout 値は test 内で書き換える前提。
func authE2EConfig(issuer string) config.Config {
	return config.Config{
		OIDCTenantIssuerURL:    issuer,
		OIDCTenantClientID:     authE2ETenantClientID,
		OIDCTenantClientSecret: authE2ETenantClientSecret,
		OIDCTenantRedirectURL:  authE2ETenantCallback,
		OIDCAdminIssuerURL:     issuer,
		OIDCAdminClientID:      authE2EAdminClientID,
		OIDCAdminClientSecret:  authE2EAdminClientSecret,
		OIDCAdminRedirectURL:   authE2EAdminCallback,
		SessionIdleTimeout:     30 * time.Minute,
		SessionAbsoluteTimeout: 8 * time.Hour,
		StateCookieTTL:         10 * time.Minute,
		StateMACSecret:         authE2EStateMACSecret,
		LogLevel:               "warn",
		LogFormat:              "json",
		LogOutput:              "stderr",
	}
}

// e2eIDPServer は OIDC discovery / JWKS / token endpoint を提供する mock IdP。
//
// callback フローを e2e で完結させるため、token endpoint は code → id_token / access_token
// を返す（実 IdP の代替）。test 側は事前に nextTokenResponse を設定して、IdP が返す
// id_token の subject / aud / nonce / email / issuer を制御する。
type e2eIDPServer struct {
	t      *testing.T
	srv    *httptest.Server
	issuer string

	mu   sync.Mutex
	keys []*e2eSigningKey

	// nextTokenResponse は次の /token 呼び出しで返す id_token 用の claims をテスト側が
	// セットする。token endpoint がそれを使って kid-1 で署名した id_token を返す。
	nextTokenResponse *e2eTokenResponse

	jwksHits  int32
	tokenHits int32

	// lastBasicAuth は token endpoint へ届いた Authorization header の生値（test 側で
	// `client_secret_basic` を assert したい場合に参照）。
	lastBasicAuth string
}

// e2eSigningKey は kid 付き RSA 鍵ペア。
type e2eSigningKey struct {
	kid     string
	private *rsa.PrivateKey
}

// e2eTokenResponse は test が事前に設定する「次の token endpoint レスポンス」内容。
//
// SignWith != nil の場合、idtoken はその鍵で署名される（攻撃シナリオ等を再現したい場合に
// kid を IdP の本物 kid と一致させたまま別 private key で署名する用途）。nil なら idp が
// 保持する keys[0] で署名する。OverrideIDToken != "" の場合、その文字列をそのまま id_token
// として返す（state cookie が削除されないバグなど低レベル経路の再現用 / 本 task では未使用
// だが mock として汎用化しておく）。
type e2eTokenResponse struct {
	Subject         string
	Aud             string
	Email           string
	Nonce           string
	Issuer          string // 空文字なら idp.issuer を使う
	ExpDelta        time.Duration
	IatDelta        time.Duration
	Kid             string
	SignWith        *rsa.PrivateKey
	OverrideIDToken string
	// OmitIDToken は token endpoint レスポンスから id_token を除外する（upstream_oidc_token 経路）。
	OmitIDToken bool
}

// newE2EIDPServer は test 用 mock IdP を起動して返す。Cleanup は t.Cleanup で登録する。
func newE2EIDPServer(t *testing.T) *e2eIDPServer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	s := &e2eIDPServer{t: t, keys: []*e2eSigningKey{{kid: "kid-e2e", private: key}}}

	mux := http.NewServeMux()
	mux.HandleFunc(authE2EIssuerPath+"/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                s.issuer,
			"authorization_endpoint":                s.issuer + "/auth",
			"token_endpoint":                        s.issuer + "/token",
			"jwks_uri":                              s.issuer + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc(authE2EIssuerPath+"/jwks", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&s.jwksHits, 1)
		s.mu.Lock()
		defer s.mu.Unlock()
		var jwks struct {
			Keys []e2eJWKEntry `json:"keys"`
		}
		for _, k := range s.keys {
			jwks.Keys = append(jwks.Keys, e2eJWKOf(k))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jwks)
	})
	mux.HandleFunc(authE2EIssuerPath+"/token", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&s.tokenHits, 1)
		s.mu.Lock()
		s.lastBasicAuth = r.Header.Get("Authorization")
		resp := s.nextTokenResponse
		s.mu.Unlock()
		if resp == nil {
			http.Error(w, `{"error":"server_error"}`, http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		body := map[string]any{
			"access_token": "access-token-e2e",
			"token_type":   "Bearer",
			"expires_in":   3600,
		}
		if !resp.OmitIDToken {
			body["id_token"] = s.buildIDToken(resp)
		}
		_ = json.NewEncoder(w).Encode(body)
	})

	s.srv = httptest.NewServer(mux)
	s.issuer = s.srv.URL + authE2EIssuerPath
	t.Cleanup(s.srv.Close)
	return s
}

// buildIDToken は nextTokenResponse の指示に従い id_token を組み立てる。
func (s *e2eIDPServer) buildIDToken(r *e2eTokenResponse) string {
	if r.OverrideIDToken != "" {
		return r.OverrideIDToken
	}
	now := time.Now()
	iss := r.Issuer
	if iss == "" {
		iss = s.issuer
	}
	expDelta := r.ExpDelta
	if expDelta == 0 {
		expDelta = 5 * time.Minute
	}
	claims := map[string]any{
		"iss":   iss,
		"sub":   r.Subject,
		"aud":   r.Aud,
		"exp":   now.Add(expDelta).Unix(),
		"iat":   now.Add(r.IatDelta).Unix(),
		"email": r.Email,
	}
	if r.Nonce != "" {
		claims["nonce"] = r.Nonce
	}

	kid := r.Kid
	if kid == "" {
		s.mu.Lock()
		kid = s.keys[0].kid
		s.mu.Unlock()
	}
	signKey := r.SignWith
	if signKey == nil {
		s.mu.Lock()
		signKey = s.keys[0].private
		s.mu.Unlock()
	}
	return signIDToken(s.t, signKey, kid, claims)
}

// setNextToken は次の token endpoint レスポンス内容を設定する。
func (s *e2eIDPServer) setNextToken(resp e2eTokenResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cpy := resp
	s.nextTokenResponse = &cpy
}

// e2eJWKEntry は JWKS endpoint の 1 鍵分 JSON 表現。
type e2eJWKEntry struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func e2eJWKOf(k *e2eSigningKey) e2eJWKEntry {
	pub := k.private.PublicKey
	return e2eJWKEntry{
		Kty: "RSA",
		Kid: k.kid,
		Use: "sig",
		Alg: "RS256",
		N:   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}

// signIDToken は claims を kid 指定で署名し raw JWT 文字列を返す。
func signIDToken(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	signerOpts := (&jose.SignerOptions{}).WithType("JWT")
	if kid != "" {
		signerOpts = signerOpts.WithHeader("kid", kid)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, signerOpts)
	if err != nil {
		t.Fatalf("jose.NewSigner: %v", err)
	}
	raw, err := josejwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatalf("josejwt.Signed.Serialize: %v", err)
	}
	return raw
}

// fakeClock は auth.Clock 実装。Now() で固定時刻を返す。
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// recordingLogger は logger.Logger を実装し、Warn / Error の field を slice に記録する。
//
// failure_kind / console / session_hash_prefix 等の構造化 field assertion に使う。
type recordingLogger struct {
	mu      sync.Mutex
	entries []recordedLogEntry
}

type recordedLogEntry struct {
	Level   string
	Message string
	Fields  map[string]any
}

func newRecordingLogger() *recordingLogger { return &recordingLogger{} }

func (l *recordingLogger) record(level, msg string, fields []any) {
	m := make(map[string]any, len(fields)/2)
	for i := 0; i+1 < len(fields); i += 2 {
		key, ok := fields[i].(string)
		if !ok {
			continue
		}
		m[key] = fields[i+1]
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, recordedLogEntry{Level: level, Message: msg, Fields: m})
}

func (l *recordingLogger) Debug(msg string, fields ...any)        { l.record("debug", msg, fields) }
func (l *recordingLogger) Info(msg string, fields ...any)         { l.record("info", msg, fields) }
func (l *recordingLogger) Warn(msg string, fields ...any)         { l.record("warn", msg, fields) }
func (l *recordingLogger) Error(msg string, fields ...any)        { l.record("error", msg, fields) }
func (l *recordingLogger) With(fields ...any) logger.Logger        { return l }
func (l *recordingLogger) Sync() error                             { return nil }

// hasFailureKind は記録された任意 Warn / Error エントリの failure_kind field が want に
// 一致するエントリが存在するかを返す。
func (l *recordingLogger) hasFailureKind(want string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.entries {
		if v, ok := e.Fields["failure_kind"]; ok {
			if s, ok2 := v.(string); ok2 && s == want {
				return true
			}
		}
	}
	return false
}

// hasFieldEqual は任意 entry の任意 field 値が want と一致するエントリが存在するかを返す。
// Issue #37: admin guard の `authz_deny_reason` / `console` 等の任意 field assertion に使う。
func (l *recordingLogger) hasFieldEqual(key, want string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.entries {
		if v, ok := e.Fields[key]; ok {
			if s, ok2 := v.(string); ok2 && s == want {
				return true
			}
		}
	}
	return false
}

// hasFailureKindWithConsole は failure_kind + console の両方が一致するエントリを探す。
func (l *recordingLogger) hasFailureKindWithConsole(wantKind, wantConsole string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.entries {
		kindVal, _ := e.Fields["failure_kind"].(string)
		consoleVal, _ := e.Fields["console"].(string)
		if kindVal == wantKind && consoleVal == wantConsole {
			return true
		}
	}
	return false
}

// e2eAuthStack は auth e2e test 全体で共有する dependency 配線。
type e2eAuthStack struct {
	cfg          config.Config
	pool         *pgxpool.Pool
	repo         auth.Repository
	verifier     oidc.Verifier
	svc          auth.Service
	tenantMW     func(http.Handler) http.Handler
	adminMW      func(http.Handler) http.Handler
	handler      *auth.Handler
	clock        *fakeClock
	log          *recordingLogger
	idp          *e2eIDPServer
	oauth2Tenant *goidc.Config
	oauth2Admin  *goidc.Config
}

// newE2EAuthStack は test 用の認証 stack を構築する。pool は呼び出し側が用意した
// pgxpool.Pool（DB 接続済み）を渡す。
//
// idp は本 helper 内で起動する mock IdP。テスト側は idp.setNextToken で id_token を制御し、
// HTTP 経路上 callback を駆動する。
func newE2EAuthStack(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *e2eAuthStack {
	t.Helper()
	idp := newE2EIDPServer(t)
	log := newRecordingLogger()
	cfg := authE2EConfig(idp.issuer)

	verifier, err := oidc.NewVerifier(ctx, cfg)
	if err != nil {
		t.Fatalf("oidc.NewVerifier: %v", err)
	}

	clock := &fakeClock{now: time.Now().UTC()}
	repo := auth.NewRepository(pool)

	tenantEndpoint := verifier.TenantEndpoint()
	adminEndpoint := verifier.AdminEndpoint()
	// test 側が token endpoint URL を mock IdP に向けたいので、Endpoint を明示的に
	// セットする（NewVerifier 由来の値は既に discovery 結果の URL を持っている）。
	scopes := []string{"openid", "email", "profile"}
	oauth2Tenant := &goidc.Config{
		ClientID:     cfg.OIDCTenantClientID,
		ClientSecret: cfg.OIDCTenantClientSecret,
		RedirectURL:  cfg.OIDCTenantRedirectURL,
		Scopes:       scopes,
		Endpoint:     tenantEndpoint,
	}
	oauth2Admin := &goidc.Config{
		ClientID:     cfg.OIDCAdminClientID,
		ClientSecret: cfg.OIDCAdminClientSecret,
		RedirectURL:  cfg.OIDCAdminRedirectURL,
		Scopes:       scopes,
		Endpoint:     adminEndpoint,
	}
	configs := map[oidc.Console]*goidc.Config{
		oidc.ConsoleTenant: oauth2Tenant,
		oidc.ConsoleAdmin:  oauth2Admin,
	}

	svc := auth.NewService(cfg, verifier, repo, configs, clock, auth.TokenGenerator(auth.New), log)
	tenantMW := auth.NewMiddleware(svc, oidc.ConsoleTenant, log, clock)
	adminMW := auth.NewMiddleware(svc, oidc.ConsoleAdmin, log, clock)
	handler := auth.NewHandler(svc, log)

	return &e2eAuthStack{
		cfg:          cfg,
		pool:         pool,
		repo:         repo,
		verifier:     verifier,
		svc:          svc,
		tenantMW:     tenantMW,
		adminMW:      adminMW,
		handler:      handler,
		clock:        clock,
		log:          log,
		idp:          idp,
		oauth2Tenant: oauth2Tenant,
		oauth2Admin:  oauth2Admin,
	}
}

// newAuthHTTPServer は auth stack 配線済みの test HTTP server を起動する。
//
// /api/auth, /api/admin/auth は root 直下に Mount し、/api と /api/admin には auth
// middleware + protectedHandler を経由する probe route を mount する（middleware の挙動を
// e2e で観測するため）。
func newAuthHTTPServer(t *testing.T, stack *e2eAuthStack) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()

	// auth エンドポイントは root 直下に Mount（middleware の外側 / Req 6.2 / 6.3）
	stack.handler.Mount(r, "/api/auth", oidc.ConsoleTenant)
	stack.handler.Mount(r, "/api/admin/auth", oidc.ConsoleAdmin)

	// /api/probe: tenant middleware を通過する protected route。
	apiRouter := chi.NewRouter()
	apiRouter.Use(stack.tenantMW)
	apiRouter.Get("/probe", probeHandler)
	r.Mount("/api", apiRouter)

	// /api/admin/probe: admin middleware を通過する protected route。
	adminRouter := chi.NewRouter()
	adminRouter.Use(stack.adminMW)
	adminRouter.Get("/probe", probeHandler)
	r.Mount("/api/admin", adminRouter)

	ts := httptest.NewServer(r)
	t.Cleanup(ts.Close)
	return ts
}

// probeHandler は middleware が AuthClaims を ctx 注入したかを検査する probe handler。
// 200 OK で claims の admin_user_id / is_super_admin を JSON で返す。
func probeHandler(w http.ResponseWriter, r *http.Request) {
	claims, ok := httpserver.AuthClaimsFromContext(r.Context())
	if !ok {
		http.Error(w, "no auth claims", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"admin_user_id":  claims.AdminUserID.String(),
		"is_super_admin": claims.IsSuperAdmin,
	})
}

// noRedirectClient は 302 を自動追従しない HTTP client を返す。
func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// findCookie は Set-Cookie 列から name 一致の最初の cookie を返す。
func findCookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, c := range cookies {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// extractQuery は URL から指定クエリ key の値を返す helper。
func extractQuery(t *testing.T, rawurl, key string) string {
	t.Helper()
	u, err := url.Parse(rawurl)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", rawurl, err)
	}
	v := u.Query().Get(key)
	if v == "" {
		t.Fatalf("URL に key=%q が含まれない: %q", key, rawurl)
	}
	return v
}
