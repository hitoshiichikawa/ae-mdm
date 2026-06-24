package auth

import (
	"encoding/base64"
	"encoding/json"
	stderrors "errors"
	"net/http"
	"strings"
	"testing"
	"time"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/oidc"
)

// テスト用定数: secret は HMAC-SHA256 に十分な長さ（32 byte 以上）。
var (
	testStateSecret = []byte("test-state-secret-0123456789abcdef-32byte")
	testStateTTL    = 10 * time.Minute
)

// validPayload は典型的な StatePayload を返す（IssuedAt は now 引数で差し替え可）。
func validPayload(now time.Time) StatePayload {
	return StatePayload{
		Nonce:     "abc-def-0123456789abcd",
		OIDCNonce: "oidc-nonce-fedcba9876543210",
		Console:   oidc.ConsoleTenant,
		ReturnTo:  "/admin/dashboard",
		IssuedAt:  now,
	}
}

// assertStateFailureKind は err が *pkgerrors.Error{Code: CodeUnauthenticated} で、
// Cause チェーンに want failureKind を含むことを assert する。
func assertStateFailureKind(t *testing.T, err error, want failureKind) {
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

// (a) Sign → Verify 往復で StatePayload が cookie 経由で復元される
func TestStateCookie_SignVerify_RoundTripRestoresPayload(t *testing.T) {
	// Arrange
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	payload := validPayload(now)

	// Act
	cookieValue, err := Sign(payload, testStateSecret)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	got, err := Verify(cookieValue, payload.Nonce, testStateSecret, testStateTTL, now)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	// Assert: ReturnTo / OIDCNonce / Console / Nonce / IssuedAt がすべて復元される
	if got.ReturnTo != payload.ReturnTo {
		t.Errorf("ReturnTo = %q, want %q", got.ReturnTo, payload.ReturnTo)
	}
	if got.OIDCNonce != payload.OIDCNonce {
		t.Errorf("OIDCNonce = %q, want %q", got.OIDCNonce, payload.OIDCNonce)
	}
	if got.Console != payload.Console {
		t.Errorf("Console = %s, want %s", got.Console, payload.Console)
	}
	if got.Nonce != payload.Nonce {
		t.Errorf("Nonce = %q, want %q", got.Nonce, payload.Nonce)
	}
	if !got.IssuedAt.Equal(payload.IssuedAt) {
		t.Errorf("IssuedAt = %v, want %v", got.IssuedAt, payload.IssuedAt)
	}
}

// (b) MAC tamper → failure_kind=state_invalid
func TestStateCookie_MACTamper_FailsAsStateInvalid(t *testing.T) {
	// Arrange
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	payload := validPayload(now)
	cookieValue, err := Sign(payload, testStateSecret)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	// MAC 部分（最後のドット以降）を別の値に差し替える
	idx := strings.LastIndex(cookieValue, ".")
	if idx < 0 {
		t.Fatalf("cookie format unexpected: %s", cookieValue)
	}
	tamperedMAC := base64.RawURLEncoding.EncodeToString([]byte("attacker-mac-bytes-32-aaaaaaaaaa"))
	tampered := cookieValue[:idx+1] + tamperedMAC

	// Act
	_, err = Verify(tampered, payload.Nonce, testStateSecret, testStateTTL, now)

	// Assert
	assertStateFailureKind(t, err, FailureKindStateInvalid)
}

// (c) TTL 超過 → failure_kind=state_expired
func TestStateCookie_TTLExpired_FailsAsStateExpired(t *testing.T) {
	// Arrange
	issuedAt := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	payload := validPayload(issuedAt)
	cookieValue, err := Sign(payload, testStateSecret)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	// now が issuedAt + ttl + 1ns → 失効
	now := issuedAt.Add(testStateTTL).Add(time.Nanosecond)

	// Act
	_, err = Verify(cookieValue, payload.Nonce, testStateSecret, testStateTTL, now)

	// Assert
	assertStateFailureKind(t, err, FailureKindStateExpired)
}

// (c2) TTL 境界: now == IssuedAt + ttl は受理（boundary 確認）
func TestStateCookie_TTLBoundary_ExactEdgeIsAccepted(t *testing.T) {
	issuedAt := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	payload := validPayload(issuedAt)
	cookieValue, err := Sign(payload, testStateSecret)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	now := issuedAt.Add(testStateTTL) // exactly at boundary

	_, err = Verify(cookieValue, payload.Nonce, testStateSecret, testStateTTL, now)
	if err != nil {
		t.Fatalf("Verify at exact TTL boundary should be accepted, got: %v", err)
	}
}

// (d) Nonce（payload 中身）を改竄 → MAC 不一致で failure_kind=state_invalid
func TestStateCookie_PayloadNonceTamper_FailsAsStateInvalid(t *testing.T) {
	// Arrange
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	payload := validPayload(now)
	cookieValue, err := Sign(payload, testStateSecret)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	// payload 部分（最後のドット以前）を改竄
	idx := strings.LastIndex(cookieValue, ".")
	if idx < 0 {
		t.Fatalf("cookie format unexpected: %s", cookieValue)
	}
	originalRaw, err := base64.RawURLEncoding.DecodeString(cookieValue[:idx])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var tampered StatePayload
	if err := json.Unmarshal(originalRaw, &tampered); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tampered.Nonce = "attacker-nonce-99999999999999"
	tamperedJSON, err := json.Marshal(tampered)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	tamperedCookie := base64.RawURLEncoding.EncodeToString(tamperedJSON) + cookieValue[idx:]

	// Act: query state は元の Nonce のままで提示。MAC 不一致で state_invalid に倒れる
	_, err = Verify(tamperedCookie, payload.Nonce, testStateSecret, testStateTTL, now)

	// Assert
	assertStateFailureKind(t, err, FailureKindStateInvalid)
}

// (e) queryState と cookie 内 Nonce 不一致 → failure_kind=state_mismatch
func TestStateCookie_QueryStateMismatch_FailsAsStateMismatch(t *testing.T) {
	// Arrange
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	payload := validPayload(now)
	cookieValue, err := Sign(payload, testStateSecret)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	// query 側の state を別の値に
	queryState := "attacker-different-state-value"

	// Act
	_, err = Verify(cookieValue, queryState, testStateSecret, testStateTTL, now)

	// Assert
	assertStateFailureKind(t, err, FailureKindStateMismatch)
}

// (f) cookie 不在（空文字）→ failure_kind=state_invalid
func TestStateCookie_EmptyCookieValue_FailsAsStateInvalid(t *testing.T) {
	// Arrange
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// Act
	_, err := Verify("", "any-query-state", testStateSecret, testStateTTL, now)

	// Assert
	assertStateFailureKind(t, err, FailureKindStateInvalid)
}

// (f2) cookie フォーマット不正（ドット無し）→ failure_kind=state_invalid
func TestStateCookie_MalformedCookieValue_FailsAsStateInvalid(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	_, err := Verify("not-a-valid-cookie-format", "any-query-state", testStateSecret, testStateTTL, now)

	assertStateFailureKind(t, err, FailureKindStateInvalid)
}

// (f3) base64url decode 失敗 → failure_kind=state_invalid
func TestStateCookie_Base64DecodeFailure_FailsAsStateInvalid(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// "!" は base64url で不正な文字
	_, err := Verify("!!!.!!!", "any-query-state", testStateSecret, testStateTTL, now)

	assertStateFailureKind(t, err, FailureKindStateInvalid)
}

// (g) CookieAttributes(ttl) の属性検証
func TestStateCookie_CookieAttributes_ReturnsExpectedAttributes(t *testing.T) {
	// Arrange
	ttl := 10 * time.Minute

	// Act
	c := CookieAttributes(ttl)

	// Assert
	if c.Name != stateCookieName {
		t.Errorf("Name = %q, want %q", c.Name, stateCookieName)
	}
	if c.Name != "__Host-ae_mdm_state" {
		t.Errorf("Name constant mismatch: %q", c.Name)
	}
	if !c.HttpOnly {
		t.Errorf("HttpOnly = false, want true")
	}
	if !c.Secure {
		t.Errorf("Secure = false, want true")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want SameSiteLaxMode", c.SameSite)
	}
	if c.Path != "/" {
		t.Errorf("Path = %q, want \"/\"", c.Path)
	}
	want := int(ttl / time.Second)
	if c.MaxAge != want {
		t.Errorf("MaxAge = %d, want %d", c.MaxAge, want)
	}
}

// (h) ExpireCookieAttributes() の属性検証
func TestStateCookie_ExpireCookieAttributes_ReturnsDeletionCookie(t *testing.T) {
	// Act
	c := ExpireCookieAttributes()

	// Assert
	if c.Name != stateCookieName {
		t.Errorf("Name = %q, want %q", c.Name, stateCookieName)
	}
	if c.Value != "" {
		t.Errorf("Value = %q, want \"\"", c.Value)
	}
	// Go の http.Cookie 規約: MaxAge < 0 のみが削除 cookie として扱われる
	if c.MaxAge >= 0 {
		t.Errorf("MaxAge = %d, want negative (deletion cookie)", c.MaxAge)
	}
	if c.MaxAge != -1 {
		t.Errorf("MaxAge = %d, want -1 specifically", c.MaxAge)
	}
	if c.Path != "/" {
		t.Errorf("Path = %q, want \"/\"", c.Path)
	}
	if !c.HttpOnly {
		t.Errorf("HttpOnly = false, want true")
	}
	if !c.Secure {
		t.Errorf("Secure = false, want true")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want SameSiteLaxMode", c.SameSite)
	}
}

// (i) 機密値の非埋込契約: err.Error() に secret / cookie 生値 / queryState が含まれない
func TestStateCookie_VerifyError_DoesNotEmbedSensitiveValues(t *testing.T) {
	// Arrange
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	payload := validPayload(now)
	cookieValue, err := Sign(payload, testStateSecret)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	queryState := "secret-query-state-value"
	secretStr := string(testStateSecret)

	// Act: 不一致経路（state_mismatch）で error を生成
	_, err = Verify(cookieValue, queryState, testStateSecret, testStateTTL, now)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}

	// Assert: err.Error() に secret / cookie 生値 / query state が含まれない
	msg := err.Error()
	if strings.Contains(msg, secretStr) {
		t.Errorf("error message embeds state secret: %q", msg)
	}
	if strings.Contains(msg, cookieValue) {
		t.Errorf("error message embeds cookie raw value: %q", msg)
	}
	if strings.Contains(msg, queryState) {
		t.Errorf("error message embeds query state value: %q", msg)
	}
}

// (i2) MAC tamper / expired / invalid 各経路でも機密値が leak しないことを確認
func TestStateCookie_VerifyError_NoSensitiveLeak_AllFailureKinds(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	payload := validPayload(now)
	cookieValue, _ := Sign(payload, testStateSecret)
	secretStr := string(testStateSecret)

	cases := []struct {
		name      string
		cookie    string
		query     string
		ttl       time.Duration
		evalAt    time.Time
		extraStrs []string
	}{
		{
			name:      "expired",
			cookie:    cookieValue,
			query:     payload.Nonce,
			ttl:       testStateTTL,
			evalAt:    now.Add(testStateTTL).Add(time.Second),
			extraStrs: []string{cookieValue},
		},
		{
			name:      "empty",
			cookie:    "",
			query:     "any",
			ttl:       testStateTTL,
			evalAt:    now,
			extraStrs: nil,
		},
		{
			name:      "malformed",
			cookie:    "x.y",
			query:     "any",
			ttl:       testStateTTL,
			evalAt:    now,
			extraStrs: []string{"x.y"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Verify(tc.cookie, tc.query, testStateSecret, tc.ttl, tc.evalAt)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			msg := err.Error()
			if strings.Contains(msg, secretStr) {
				t.Errorf("error message embeds state secret: %q", msg)
			}
			for _, s := range tc.extraStrs {
				if s == "" {
					continue
				}
				if strings.Contains(msg, s) {
					t.Errorf("error message embeds sensitive value %q: %q", s, msg)
				}
			}
		})
	}
}
