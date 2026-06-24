package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/oidc"
)

// stateCookieName は state 保護 cookie の Set-Cookie 名。
//
// `__Host-` プレフィックスにより以下が強制される（design.md「State Cookie」節 / Req 2.3）:
//   - Path=/ 必須
//   - Secure 必須
//   - Domain 属性指定不可（sub-domain 跨ぎを物理的に禁止）
const stateCookieName = "__Host-ae_mdm_state"

// StatePayload は state cookie に格納する MAC 保護下の値。
//
// design.md「State Cookie」節および requirements.md Req 2.x と整合。Nonce / OIDCNonce は
// OAuth `state` と OIDC `nonce` で別パラメータ（RFC OIDC Core 1.0 §3.1.2.1）であり、
// 同値を使い回さない（BeginLogin で `crypto/rand` で独立に生成する）。
type StatePayload struct {
	// Nonce は OAuth `state` クエリ用 16 byte base64url（CSRF 防止）。
	// cookie 内値と query state の constant-time 比較に使用する。
	Nonce string `json:"nonce"`
	// OIDCNonce は OIDC `nonce` 用 16 byte base64url（authorization code injection 防止）。
	// 認可リクエストの `nonce` パラメータと ID トークン `nonce` クレームの constant-time
	// 一致確認に使用する。
	OIDCNonce string `json:"oidc_nonce"`
	// Console は当該 state を発行したコンソール種別（tenant / admin）。
	// callback 側 console と一致しなければ Service が state_console_mismatch で reject する。
	Console oidc.Console `json:"console"`
	// ReturnTo は callback 成功後の遷移先 URL（同一オリジン内相対パス。BeginLogin で validate 済み）。
	// MAC 保護下なので tamper されない前提。
	ReturnTo string `json:"return_to"`
	// IssuedAt は state cookie 発行時刻（TTL 判定基準）。
	IssuedAt time.Time `json:"issued_at"`
}

// failureKind は auth domain の失敗種別を表す sentinel error 型。
//
// platform/oidc の failureKind とは独立に再定義する（依存方向: auth → oidc は許容するが
// auth が oidc の private 識別子に依存しないため。design.md「Domain Layer (Auth)」節 +
// 「機密値を Cause メッセージ本文に埋め込まない実装契約」節 / NFR 4.1）。
//
// Cause チェーンに含めることで Service / Logger が `errors.As` / `errors.Is` で識別し、
// 構造化ログの `failure_kind` field に surface する。
type failureKind string

func (f failureKind) Error() string { return string(f) }

const (
	// FailureKindStateInvalid は state cookie の形式不正・MAC 不一致・decode 失敗
	// （Req 2.6 のうち、cookie 不在 / MAC 検証失敗）。
	FailureKindStateInvalid failureKind = "state_invalid"
	// FailureKindStateExpired は state cookie の TTL 超過（Req 2.4 / 2.6）。
	FailureKindStateExpired failureKind = "state_expired"
	// FailureKindStateMismatch は query state と cookie 内 Nonce の不一致（Req 2.7）。
	FailureKindStateMismatch failureKind = "state_mismatch"
)

// Sign は payload を JSON 化し HMAC-SHA256 で MAC 署名した cookie 値文字列を返す。
//
// フォーマット: `base64url(json(payload)) + "." + base64url(MAC)`。
// base64url は no-padding（base64.RawURLEncoding）。secret は 32 byte 以上を想定する
// （config 側で `len >= 32` を validate 済み）。
func Sign(payload StatePayload, secret []byte) (string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		// json.Marshal は通常失敗しないが、不正な値が紛れ込むと UnsupportedTypeError 等で
		// 失敗する。秘密値を含めずに wrap する。
		return "", pkgerrors.Wrap(pkgerrors.CodeInternal, "state payload marshal failed", err)
	}
	encodedBody := base64.RawURLEncoding.EncodeToString(body)
	mac := computeMAC(secret, encodedBody)
	encodedMAC := base64.RawURLEncoding.EncodeToString(mac)
	return encodedBody + "." + encodedMAC, nil
}

// Verify は cookie 値と query state の一致を MAC + TTL + Nonce の 3 段で検証する。
//
// 検証順序:
//  1. cookie 値が空文字 → FailureKindStateInvalid
//  2. フォーマット split 失敗 / base64url decode 失敗 / json decode 失敗 → FailureKindStateInvalid
//  3. MAC 不一致（constant-time compare） → FailureKindStateInvalid
//  4. now > IssuedAt + ttl → FailureKindStateExpired（境界 now == IssuedAt+ttl は受理）
//  5. queryState != payload.Nonce（constant-time compare） → FailureKindStateMismatch
//
// 失敗時は `*errors.Error{Code: CodeUnauthenticated}` を返し、Cause チェーンに failureKind
// sentinel を含める。error wrap message に secret / cookie 生値 / queryState を文字列補間
// しない（NFR 1.1 / NFR 4.2）。
func Verify(cookieValue, queryState string, secret []byte, ttl time.Duration, now time.Time) (StatePayload, error) {
	// 1. 空文字（cookie 不在）
	if cookieValue == "" {
		return StatePayload{}, stateInvalid("state cookie missing")
	}
	// 2. フォーマット split
	idx := strings.LastIndex(cookieValue, ".")
	if idx <= 0 || idx == len(cookieValue)-1 {
		return StatePayload{}, stateInvalid("state cookie format invalid")
	}
	encodedBody := cookieValue[:idx]
	encodedMAC := cookieValue[idx+1:]

	body, err := base64.RawURLEncoding.DecodeString(encodedBody)
	if err != nil {
		return StatePayload{}, stateInvalid("state cookie body decode failed")
	}
	mac, err := base64.RawURLEncoding.DecodeString(encodedMAC)
	if err != nil {
		return StatePayload{}, stateInvalid("state cookie mac decode failed")
	}

	// 3. MAC 検証（constant-time）
	expectedMAC := computeMAC(secret, encodedBody)
	if subtle.ConstantTimeCompare(mac, expectedMAC) != 1 {
		return StatePayload{}, stateInvalid("state cookie mac mismatch")
	}

	// payload unmarshal は MAC 検証通過後（攻撃者が任意の JSON を unmarshal させる経路を断つ）。
	var payload StatePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return StatePayload{}, stateInvalid("state payload unmarshal failed")
	}

	// 4. TTL 検証（境界: now == IssuedAt + ttl は受理）
	if now.Sub(payload.IssuedAt) > ttl {
		return StatePayload{}, stateExpired("state cookie ttl exceeded")
	}

	// 5. queryState と Nonce の一致（constant-time）
	if subtle.ConstantTimeCompare([]byte(queryState), []byte(payload.Nonce)) != 1 {
		return StatePayload{}, stateMismatch("query state does not match cookie nonce")
	}

	return payload, nil
}

// CookieAttributes は state cookie の Set-Cookie 属性を返す。
//
// Value は呼び出し側が Sign の戻り値で設定する設計（本 helper は属性のみを提供）。
// MaxAge = int(ttl/time.Second) で、cfg.StateCookieTTL から導出される（10 分以下 / Req 2.4）。
func CookieAttributes(ttl time.Duration) http.Cookie {
	return http.Cookie{
		Name:     stateCookieName,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(ttl / time.Second),
	}
}

// ExpireCookieAttributes は state cookie の削除用 Set-Cookie 属性を返す。
//
// Go の `http.Cookie` 規約: `MaxAge < 0` のみが削除 cookie として `Set-Cookie` ヘッダに
// `Max-Age=0` 属性 + 過去日付の `Expires` 属性を出力する。`MaxAge = 0` だと Max-Age
// 属性自体が省略され session cookie 化して削除が成立しない（Req 2.8 違反になる）。
//
// session cookie 用の `session.ExpireCookieAttributes()` とは Name が異なるため、
// state cookie 削除には必ず本 helper を使用する。
func ExpireCookieAttributes() http.Cookie {
	return http.Cookie{
		Name:     stateCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	}
}

// computeMAC は HMAC-SHA256(secret, encodedBody) を計算して raw bytes を返す。
func computeMAC(secret []byte, encodedBody string) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(encodedBody))
	return h.Sum(nil)
}

// stateInvalid は CodeUnauthenticated + FailureKindStateInvalid を Cause に含むエラーを構築する。
//
// message は固定文字列のみを使い、secret / cookie 生値 / queryState を含めない（NFR 1.1 / NFR 4.2）。
func stateInvalid(message string) *pkgerrors.Error {
	return pkgerrors.Wrap(pkgerrors.CodeUnauthenticated, message, FailureKindStateInvalid)
}

// stateExpired は CodeUnauthenticated + FailureKindStateExpired を Cause に含むエラーを構築する。
func stateExpired(message string) *pkgerrors.Error {
	return pkgerrors.Wrap(pkgerrors.CodeUnauthenticated, message, FailureKindStateExpired)
}

// stateMismatch は CodeUnauthenticated + FailureKindStateMismatch を Cause に含むエラーを構築する。
func stateMismatch(message string) *pkgerrors.Error {
	return pkgerrors.Wrap(pkgerrors.CodeUnauthenticated, message, FailureKindStateMismatch)
}
