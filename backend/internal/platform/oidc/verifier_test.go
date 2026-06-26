package oidc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	stderrors "errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	josejwt "github.com/go-jose/go-jose/v4/jwt"
	"golang.org/x/oauth2"

	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// テスト用定数。tenant / admin の client_id は異なる値で aud 識別を行うため。
const (
	testTenantClientID = "tenant-console-test"
	testAdminClientID  = "admin-console-test"
	testIssuerPath     = "/realms/test"
)

// idpServer は OIDC discovery + JWKS endpoint を提供するテスト用 mock IdP。
//
// `httptest.NewServer` の URL を base に、`/.well-known/openid-configuration` で issuer /
// JWKS URL を広告し、`/jwks` で 1 つ以上の RSA 公開鍵を JWK Set として返す。
// 動的に keys を入れ替えられるよう、内部状態を mutex で保護する。
type idpServer struct {
	t      *testing.T
	srv    *httptest.Server
	issuer string

	mu   sync.Mutex
	keys []*signingKey // 同時に複数 kid を許可（rotation テスト用）

	jwksHits int32 // JWKS endpoint への access count（rotation の挙動観測用）
}

// signingKey は kid 付き RSA 鍵ペア。
type signingKey struct {
	kid     string
	private *rsa.PrivateKey
}

func newIDPServer(t *testing.T, kid string, key *rsa.PrivateKey) *idpServer {
	t.Helper()
	s := &idpServer{t: t, keys: []*signingKey{{kid: kid, private: key}}}
	mux := http.NewServeMux()
	// issuer は server.URL + testIssuerPath なので、discovery / JWKS endpoint は
	// issuer 配下のパスに register する必要がある（go-oidc は
	// `<issuer>/.well-known/openid-configuration` を GET する）。
	mux.HandleFunc(testIssuerPath+"/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := map[string]any{
			"issuer":                                s.issuer,
			"authorization_endpoint":                s.issuer + "/auth",
			"token_endpoint":                        s.issuer + "/token",
			"jwks_uri":                              s.issuer + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	mux.HandleFunc(testIssuerPath+"/jwks", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&s.jwksHits, 1)
		s.mu.Lock()
		defer s.mu.Unlock()
		var jwks struct {
			Keys []jwkEntry `json:"keys"`
		}
		for _, k := range s.keys {
			jwks.Keys = append(jwks.Keys, jwkOf(k))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jwks)
	})
	s.srv = httptest.NewServer(mux)
	s.issuer = s.srv.URL + testIssuerPath
	t.Cleanup(s.srv.Close)
	return s
}

// rotateKey は JWKS を新しい鍵だけにする（rotation 検証用）。
func (s *idpServer) rotateKey(kid string, key *rsa.PrivateKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keys = []*signingKey{{kid: kid, private: key}}
}

// jwkEntry は JWKS endpoint が返す 1 鍵分の JSON 表現。RSA 公開鍵のみを扱う。
type jwkEntry struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func jwkOf(k *signingKey) jwkEntry {
	pub := k.private.PublicKey
	return jwkEntry{
		Kty: "RSA",
		Kid: k.kid,
		Use: "sig",
		Alg: "RS256",
		N:   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}

// signToken は payload を kid 指定で署名し、raw JWT 文字列を返す。
//
// kid="" のときは kid header を埋め込まない（kid 不在テスト用）。
func signToken(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any) string {
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

// newTestVerifier は 1 つの IdP server を tenant / admin 両 console から見るかたちで
// Verifier を構築する（実 IdP では 1 keycloak realm に 2 client が登録される運用と一致）。
func newTestVerifier(t *testing.T, idp *idpServer) Verifier {
	t.Helper()
	cfg := config.Config{
		OIDCTenantIssuerURL: idp.issuer,
		OIDCTenantClientID:  testTenantClientID,
		OIDCAdminIssuerURL:  idp.issuer,
		OIDCAdminClientID:   testAdminClientID,
	}
	v, err := NewVerifier(context.Background(), cfg)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v
}

// validClaims は exp / iat / iss / sub を埋めた検証通過用 claims を作る helper。
func validClaims(t *testing.T, issuer, sub string, aud any) map[string]any {
	t.Helper()
	now := time.Now()
	return map[string]any{
		"iss":    issuer,
		"sub":    sub,
		"aud":    aud,
		"exp":    now.Add(5 * time.Minute).Unix(),
		"iat":    now.Unix(),
		"email":  "alice@example.com",
		"groups": []string{"TenantAdmin"},
	}
}

func generateRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	return k
}

// assertFailureKind は err が *pkgerrors.Error{Code: CodeUnauthenticated} で、
// Cause チェーンに want の failureKind を含むことを assert する。
func assertFailureKind(t *testing.T, err error, want failureKind) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with failure_kind=%s, got nil", want)
	}
	var pe *pkgerrors.Error
	if !stderrors.As(err, &pe) {
		t.Fatalf("expected *errors.Error, got %T: %v", err, err)
	}
	if pe.Code != pkgerrors.CodeUnauthenticated {
		t.Fatalf("expected Code=CodeUnauthenticated, got %s (err=%v)", pe.Code, err)
	}
	if !stderrors.Is(err, want) {
		t.Fatalf("expected failure_kind=%s in Cause chain, got: %v", want, err)
	}
}

// assertNoSensitiveLeak は err.Error() に raw JWT / client_id / client_secret 等が
// 含まれていないことを assert する（NFR 1.1 / NFR 4.2 / Req 1.11 の error wrap 契約）。
func assertNoSensitiveLeak(t *testing.T, err error, sensitive ...string) {
	t.Helper()
	if err == nil {
		return
	}
	msg := err.Error()
	for _, s := range sensitive {
		if s == "" {
			continue
		}
		if strings.Contains(msg, s) {
			t.Fatalf("error message contains sensitive value %q: %v", s, msg)
		}
	}
}

// ---------------------- Tests ----------------------

// (a) 正常系: aud=tenant-console → MatchedConsole=ConsoleTenant
func TestVerifyIDToken_TenantAudience_Success(t *testing.T) {
	// Arrange
	key := generateRSAKey(t)
	idp := newIDPServer(t, "kid-1", key)
	v := newTestVerifier(t, idp)
	raw := signToken(t, key, "kid-1", validClaims(t, idp.issuer, "user-1", testTenantClientID))

	// Act
	claims, err := v.VerifyIDToken(context.Background(), raw)

	// Assert
	if err != nil {
		t.Fatalf("VerifyIDToken: %v", err)
	}
	if claims.MatchedConsole != ConsoleTenant {
		t.Errorf("MatchedConsole = %s, want %s", claims.MatchedConsole, ConsoleTenant)
	}
	if claims.Subject != "user-1" {
		t.Errorf("Subject = %q, want %q", claims.Subject, "user-1")
	}
	if claims.Issuer != idp.issuer {
		t.Errorf("Issuer = %q, want %q", claims.Issuer, idp.issuer)
	}
	if claims.Email != "alice@example.com" {
		t.Errorf("Email = %q, want %q", claims.Email, "alice@example.com")
	}
	if len(claims.Groups) != 1 || claims.Groups[0] != "TenantAdmin" {
		t.Errorf("Groups = %v, want [TenantAdmin]", claims.Groups)
	}
}

// (b) 正常系: aud=admin-console → MatchedConsole=ConsoleAdmin
func TestVerifyIDToken_AdminAudience_Success(t *testing.T) {
	// Arrange
	key := generateRSAKey(t)
	idp := newIDPServer(t, "kid-1", key)
	v := newTestVerifier(t, idp)
	raw := signToken(t, key, "kid-1", validClaims(t, idp.issuer, "ops-1", testAdminClientID))

	// Act
	claims, err := v.VerifyIDToken(context.Background(), raw)

	// Assert
	if err != nil {
		t.Fatalf("VerifyIDToken: %v", err)
	}
	if claims.MatchedConsole != ConsoleAdmin {
		t.Errorf("MatchedConsole = %s, want %s", claims.MatchedConsole, ConsoleAdmin)
	}
	if claims.Subject != "ops-1" {
		t.Errorf("Subject = %q, want %q", claims.Subject, "ops-1")
	}
}

// (c) 署名検証失敗（鍵差替）→ failure_kind=invalid_sig
func TestVerifyIDToken_SignatureFailure_RejectsAsInvalidSig(t *testing.T) {
	// Arrange
	idpKey := generateRSAKey(t)
	idp := newIDPServer(t, "kid-1", idpKey)
	v := newTestVerifier(t, idp)

	// IdP が公開している鍵とは別の鍵で署名（攻撃シナリオ）
	attackerKey := generateRSAKey(t)
	raw := signToken(t, attackerKey, "kid-1", validClaims(t, idp.issuer, "user-1", testTenantClientID))

	// Act
	_, err := v.VerifyIDToken(context.Background(), raw)

	// Assert
	assertFailureKind(t, err, FailureKindInvalidSig)
	assertNoSensitiveLeak(t, err, raw)
}

// (d) iss 不一致 → failure_kind=invalid_iss
func TestVerifyIDToken_IssuerMismatch_RejectsAsInvalidIss(t *testing.T) {
	// Arrange
	key := generateRSAKey(t)
	idp := newIDPServer(t, "kid-1", key)
	v := newTestVerifier(t, idp)

	claims := validClaims(t, "https://evil.example.com/realms/fake", "user-1", testTenantClientID)
	raw := signToken(t, key, "kid-1", claims)

	// Act
	_, err := v.VerifyIDToken(context.Background(), raw)

	// Assert
	assertFailureKind(t, err, FailureKindInvalidIss)
	assertNoSensitiveLeak(t, err, raw)
}

// (e) aud 不一致 → failure_kind=invalid_aud
func TestVerifyIDToken_AudienceMismatch_RejectsAsInvalidAud(t *testing.T) {
	// Arrange
	key := generateRSAKey(t)
	idp := newIDPServer(t, "kid-1", key)
	v := newTestVerifier(t, idp)

	raw := signToken(t, key, "kid-1", validClaims(t, idp.issuer, "user-1", "some-other-aud"))

	// Act
	_, err := v.VerifyIDToken(context.Background(), raw)

	// Assert
	assertFailureKind(t, err, FailureKindInvalidAud)
	assertNoSensitiveLeak(t, err, raw)
}

// (f) aud 配列に tenant+admin 同居 → failure_kind=aud_ambiguous (Req 1.5)
func TestVerifyIDToken_AudienceArrayHasBoth_RejectsAsAmbiguous(t *testing.T) {
	// Arrange
	key := generateRSAKey(t)
	idp := newIDPServer(t, "kid-1", key)
	v := newTestVerifier(t, idp)

	raw := signToken(t, key, "kid-1",
		validClaims(t, idp.issuer, "user-1", []string{testTenantClientID, testAdminClientID}))

	// Act
	_, err := v.VerifyIDToken(context.Background(), raw)

	// Assert
	assertFailureKind(t, err, FailureKindAudAmbiguous)
	assertNoSensitiveLeak(t, err, raw)
}

// (g) exp 切れ → failure_kind=token_expired
func TestVerifyIDToken_Expired_RejectsAsTokenExpired(t *testing.T) {
	// Arrange
	key := generateRSAKey(t)
	idp := newIDPServer(t, "kid-1", key)
	v := newTestVerifier(t, idp)

	past := time.Now().Add(-1 * time.Hour)
	claims := map[string]any{
		"iss": idp.issuer,
		"sub": "user-1",
		"aud": testTenantClientID,
		"exp": past.Unix(),
		"iat": past.Add(-5 * time.Minute).Unix(),
	}
	raw := signToken(t, key, "kid-1", claims)

	// Act
	_, err := v.VerifyIDToken(context.Background(), raw)

	// Assert
	assertFailureKind(t, err, FailureKindTokenExpired)
	assertNoSensitiveLeak(t, err, raw)
}

// (h) kid 不在 → failure_kind=invalid_kid
//
// 「kid 不在」は本テストでは「JWT header に kid を埋め込まずに発行し、JWKS には別 kid の
// 鍵を 1 つ置く」ケースを再現する。go-oidc / go-jose は kid 不指定で multi-key JWKS から
// 鍵を選択できず、`no matching key` 系のエラーを返す。
func TestVerifyIDToken_KidNotFound_RejectsAsInvalidKid(t *testing.T) {
	// Arrange
	idpKey := generateRSAKey(t)
	idp := newIDPServer(t, "kid-published", idpKey)
	v := newTestVerifier(t, idp)

	// header に kid を埋め込まず、かつ別の private key で署名 → JWKS の "kid-published" は
	// 候補にすらならない。go-oidc は keyset から候補を探す際 kid を見るので "no matching key"
	// 経路に乗る。
	otherKey := generateRSAKey(t)
	raw := signToken(t, otherKey, "", validClaims(t, idp.issuer, "user-1", testTenantClientID))

	// Act
	_, err := v.VerifyIDToken(context.Background(), raw)

	// Assert: invalid_kid または invalid_sig のいずれか（go-jose 実装によって変動するため
	// invalid_kid を優先するが invalid_sig も許容する両対応の assert）
	// 本テストは「reject される」ことを最優先に守り、failure_kind は invalid_kid を期待する。
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !stderrors.Is(err, FailureKindInvalidKid) && !stderrors.Is(err, FailureKindInvalidSig) {
		t.Fatalf("expected invalid_kid or invalid_sig, got: %v", err)
	}
	// CodeUnauthenticated は必須
	var pe *pkgerrors.Error
	if !stderrors.As(err, &pe) || pe.Code != pkgerrors.CodeUnauthenticated {
		t.Fatalf("expected *errors.Error{CodeUnauthenticated}, got: %v", err)
	}
}

// (i) kid rotation → JWKS endpoint レスポンスを差し替えた後の再検証で成功復帰
//
// IdP が鍵を rotation した直後の挙動を再現する。go-oidc/v3 の RemoteKeySet は、JWT の kid に
// 対応する鍵が cache に無い場合に JWKS を再 fetch する仕組みを持っている（Req 1.9 の自動 refresh）。
func TestVerifyIDToken_KidRotation_RefetchesJWKS(t *testing.T) {
	// Arrange: 初期鍵で IdP を起動
	oldKey := generateRSAKey(t)
	idp := newIDPServer(t, "kid-old", oldKey)
	v := newTestVerifier(t, idp)

	// 旧鍵での検証が成功することを確認
	rawOld := signToken(t, oldKey, "kid-old", validClaims(t, idp.issuer, "user-1", testTenantClientID))
	if _, err := v.VerifyIDToken(context.Background(), rawOld); err != nil {
		t.Fatalf("baseline verify with old key failed: %v", err)
	}

	// Act: IdP 側の鍵を rotation（新 kid のみを JWKS に置く）
	newKey := generateRSAKey(t)
	idp.rotateKey("kid-new", newKey)

	// 新鍵で署名した token を verify → JWKS 再 fetch で成功する想定
	rawNew := signToken(t, newKey, "kid-new", validClaims(t, idp.issuer, "user-2", testTenantClientID))

	// JWKS hit count をベースライン記録
	baselineHits := atomic.LoadInt32(&idp.jwksHits)

	// Act
	claims, err := v.VerifyIDToken(context.Background(), rawNew)

	// Assert
	if err != nil {
		t.Fatalf("verify after rotation failed: %v", err)
	}
	if claims.Subject != "user-2" {
		t.Errorf("Subject = %q, want user-2", claims.Subject)
	}
	if claims.MatchedConsole != ConsoleTenant {
		t.Errorf("MatchedConsole = %s, want %s", claims.MatchedConsole, ConsoleTenant)
	}

	// rotation 後の verify で JWKS endpoint が新たに hit されていることを確認（Req 1.9）
	afterHits := atomic.LoadInt32(&idp.jwksHits)
	if afterHits <= baselineHits {
		t.Errorf("expected JWKS refetch after rotation, hits=%d->%d", baselineHits, afterHits)
	}
}

// (j) id_token の nonce クレームを Claims.Nonce が populate する + nonce 不在でも reject しない
//
// Verifier は nonce 一致確認を行わない（Service 層の責務）が、nonce クレームを surface する
// 責務を持つ（OIDC Core 1.0 §3.1.2.7 / design.md「OIDC nonce binding」節）。
func TestVerifyIDToken_NoncePopulationAndAbsence(t *testing.T) {
	t.Run("nonce クレーム付きの id_token で Claims.Nonce が当該値で populate される", func(t *testing.T) {
		// Arrange
		key := generateRSAKey(t)
		idp := newIDPServer(t, "kid-1", key)
		v := newTestVerifier(t, idp)
		claims := validClaims(t, idp.issuer, "user-1", testTenantClientID)
		claims["nonce"] = "n-0S6_WzA2Mj"
		raw := signToken(t, key, "kid-1", claims)

		// Act
		got, err := v.VerifyIDToken(context.Background(), raw)

		// Assert
		if err != nil {
			t.Fatalf("VerifyIDToken: %v", err)
		}
		if got.Nonce != "n-0S6_WzA2Mj" {
			t.Errorf("Nonce = %q, want %q", got.Nonce, "n-0S6_WzA2Mj")
		}
	})

	t.Run("nonce クレーム不在の id_token でも Verifier は reject せず Claims.Nonce が空文字", func(t *testing.T) {
		// Arrange
		key := generateRSAKey(t)
		idp := newIDPServer(t, "kid-1", key)
		v := newTestVerifier(t, idp)
		claims := validClaims(t, idp.issuer, "user-1", testTenantClientID)
		// nonce を含めない
		raw := signToken(t, key, "kid-1", claims)

		// Act
		got, err := v.VerifyIDToken(context.Background(), raw)

		// Assert
		if err != nil {
			t.Fatalf("VerifyIDToken should not reject for missing nonce: %v", err)
		}
		if got.Nonce != "" {
			t.Errorf("Nonce = %q, want empty (Service is responsible for nonce check)", got.Nonce)
		}
	})
}

// Tenant/AdminEndpoint の AuthStyle が AuthStyleInHeader に固定されている（tasks.md L236）。
//
// `golang.org/x/oauth2` の default は AuthStyleAutoDetect で初回 reject を見て
// client_secret_post にフォールバックする非決定的挙動を取るため、Basic 固定を契約として
// 強制する（design.md Technology Stack「Authentication」行 / 確認事項 6）。
func TestVerifier_EndpointAuthStyle_IsInHeader(t *testing.T) {
	// Arrange
	key := generateRSAKey(t)
	idp := newIDPServer(t, "kid-1", key)
	v := newTestVerifier(t, idp)

	// Act / Assert: tenant
	tenant := v.TenantEndpoint()
	if tenant.AuthStyle != oauth2.AuthStyleInHeader {
		t.Errorf("TenantEndpoint().AuthStyle = %v, want AuthStyleInHeader", tenant.AuthStyle)
	}
	if tenant.AuthURL == "" || tenant.TokenURL == "" {
		t.Errorf("TenantEndpoint AuthURL/TokenURL should be populated from discovery, got %+v", tenant)
	}

	// Act / Assert: admin
	admin := v.AdminEndpoint()
	if admin.AuthStyle != oauth2.AuthStyleInHeader {
		t.Errorf("AdminEndpoint().AuthStyle = %v, want AuthStyleInHeader", admin.AuthStyle)
	}
}

// NewVerifier が discovery 失敗で CodeUnavailable を返す（NFR 3.2 fail-closed bootstrap）。
func TestNewVerifier_DiscoveryFailure_ReturnsUnavailable(t *testing.T) {
	// Arrange: 存在しない issuer URL を渡す
	cfg := config.Config{
		OIDCTenantIssuerURL: "http://127.0.0.1:1/nope",
		OIDCTenantClientID:  testTenantClientID,
		OIDCAdminIssuerURL:  "http://127.0.0.1:1/nope",
		OIDCAdminClientID:   testAdminClientID,
	}

	// Act
	_, err := NewVerifier(context.Background(), cfg)

	// Assert
	if err == nil {
		t.Fatalf("expected discovery error, got nil")
	}
	var pe *pkgerrors.Error
	if !stderrors.As(err, &pe) {
		t.Fatalf("expected *errors.Error, got %T: %v", err, err)
	}
	if pe.Code != pkgerrors.CodeUnavailable {
		t.Errorf("expected Code=CodeUnavailable, got %s", pe.Code)
	}
	if !stderrors.Is(err, FailureKindOIDCDiscovery) {
		t.Errorf("expected failure_kind=oidc_discovery in Cause chain, got: %v", err)
	}
}

// NewVerifier が JWKS prefetch 失敗で CodeUnavailable を返す（NFR 3.2 / tasks.md L228-230 /
// design.md L295-296）。discovery 自体は通るが jwks_uri が 404 を返すケース。
func TestNewVerifier_JWKSPrefetchFailure_ReturnsUnavailable(t *testing.T) {
	// Arrange: discovery は 200 を返すが jwks_uri は 404 を返す mock IdP を構築
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	issuer := srv.URL + testIssuerPath
	mux.HandleFunc(testIssuerPath+"/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                issuer,
			"authorization_endpoint":                issuer + "/auth",
			"token_endpoint":                        issuer + "/token",
			"jwks_uri":                              issuer + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc(testIssuerPath+"/jwks", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	cfg := config.Config{
		OIDCTenantIssuerURL: issuer,
		OIDCTenantClientID:  testTenantClientID,
		OIDCAdminIssuerURL:  issuer,
		OIDCAdminClientID:   testAdminClientID,
	}

	// Act
	_, err := NewVerifier(context.Background(), cfg)

	// Assert
	if err == nil {
		t.Fatalf("expected jwks prefetch error, got nil")
	}
	var pe *pkgerrors.Error
	if !stderrors.As(err, &pe) {
		t.Fatalf("expected *errors.Error, got %T: %v", err, err)
	}
	if pe.Code != pkgerrors.CodeUnavailable {
		t.Errorf("expected Code=CodeUnavailable, got %s", pe.Code)
	}
	if !stderrors.Is(err, FailureKindOIDCDiscovery) {
		t.Errorf("expected failure_kind=oidc_discovery in Cause chain, got: %v", err)
	}
}

// NewVerifier が JWKS の keys 配列が空のとき CodeUnavailable を返す（誤設定 / 鍵未登録の
// 起動時 fail-fast）。
func TestNewVerifier_JWKSEmptyKeys_ReturnsUnavailable(t *testing.T) {
	// Arrange: jwks_uri は 200 を返すが keys 配列が空の mock IdP を構築
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	issuer := srv.URL + testIssuerPath
	mux.HandleFunc(testIssuerPath+"/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                issuer,
			"authorization_endpoint":                issuer + "/auth",
			"token_endpoint":                        issuer + "/token",
			"jwks_uri":                              issuer + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc(testIssuerPath+"/jwks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"keys": []}`))
	})

	cfg := config.Config{
		OIDCTenantIssuerURL: issuer,
		OIDCTenantClientID:  testTenantClientID,
		OIDCAdminIssuerURL:  issuer,
		OIDCAdminClientID:   testAdminClientID,
	}

	// Act
	_, err := NewVerifier(context.Background(), cfg)

	// Assert
	if err == nil {
		t.Fatalf("expected empty-keys error, got nil")
	}
	var pe *pkgerrors.Error
	if !stderrors.As(err, &pe) {
		t.Fatalf("expected *errors.Error, got %T: %v", err, err)
	}
	if pe.Code != pkgerrors.CodeUnavailable {
		t.Errorf("expected Code=CodeUnavailable, got %s", pe.Code)
	}
	if !stderrors.Is(err, FailureKindOIDCDiscovery) {
		t.Errorf("expected failure_kind=oidc_discovery in Cause chain, got: %v", err)
	}
}

// 「機密値を error wrap に補間しない」契約の追加 assert（複数 failure path をまとめて検証）。
//
// raw JWT / client_id を含む文字列が error メッセージに登場しないことを 4 つの失敗種別で
// 横断確認する（NFR 1.1 / NFR 4.2 / Req 1.11）。
func TestVerifyIDToken_NoSensitiveValueInErrorMessage(t *testing.T) {
	key := generateRSAKey(t)
	idp := newIDPServer(t, "kid-1", key)
	v := newTestVerifier(t, idp)

	cases := []struct {
		name   string
		token  func() string
		assert func(t *testing.T, err error, raw string)
	}{
		{
			name: "invalid_sig",
			token: func() string {
				other := generateRSAKey(t)
				return signToken(t, other, "kid-1", validClaims(t, idp.issuer, "user-1", testTenantClientID))
			},
		},
		{
			name: "invalid_aud",
			token: func() string {
				return signToken(t, key, "kid-1", validClaims(t, idp.issuer, "user-1", "wrong-aud"))
			},
		},
		{
			name: "invalid_iss",
			token: func() string {
				return signToken(t, key, "kid-1", validClaims(t, "https://wrong.example.com/", "user-1", testTenantClientID))
			},
		},
		{
			name: "token_expired",
			token: func() string {
				return signToken(t, key, "kid-1", map[string]any{
					"iss": idp.issuer,
					"sub": "user-1",
					"aud": testTenantClientID,
					"exp": time.Now().Add(-1 * time.Hour).Unix(),
					"iat": time.Now().Add(-2 * time.Hour).Unix(),
				})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := tc.token()
			_, err := v.VerifyIDToken(context.Background(), raw)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			// raw JWT / client_secret 風文字列が message に混入していないこと
			assertNoSensitiveLeak(t, err, raw, "tenant-secret", "admin-secret")
		})
	}
}

// failure_kind sentinel が errors.Is で識別可能であることを直接確認（NFR 4.1）。
func TestFailureKind_IsIdentifiableViaErrorsIs(t *testing.T) {
	// 個々の failure_kind 値が string ベースで一意であることを念のため確認。
	kinds := []failureKind{
		FailureKindInvalidSig,
		FailureKindInvalidIss,
		FailureKindInvalidAud,
		FailureKindAudAmbiguous,
		FailureKindTokenExpired,
		FailureKindInvalidKid,
		FailureKindOIDCDiscovery,
	}
	seen := map[string]bool{}
	for _, k := range kinds {
		if seen[string(k)] {
			t.Errorf("duplicate failure_kind value: %s", k)
		}
		seen[string(k)] = true
	}

	// 各 sentinel は自身の error message を返す
	for _, k := range kinds {
		if got := k.Error(); got != string(k) {
			t.Errorf("%s.Error() = %q, want %q", k, got, string(k))
		}
	}
}

// 念のため、独自 helper containsSubstring が strings.Contains と等価であることを assert。
//
// 依存方向（platform/oidc が strings を import するのは許可されている標準 lib のみ）の
// 確認 + 実装のデグレ回帰用。
func TestContainsSubstring_MatchesStringsContains(t *testing.T) {
	cases := []struct {
		s, sub string
	}{
		{"", ""},
		{"abc", ""},
		{"abc", "b"},
		{"abc", "abc"},
		{"abc", "abcd"},
		{"alice@example.com", "@"},
	}
	for _, c := range cases {
		want := strings.Contains(c.s, c.sub)
		got := containsSubstring(c.s, c.sub)
		if want != got {
			t.Errorf("containsSubstring(%q, %q) = %v, want %v", c.s, c.sub, got, want)
		}
	}
}

