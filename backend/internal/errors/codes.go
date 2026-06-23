// Package errors は ae-mdm の全 internal package が共通利用する独自エラー型を提供する。
//
// 本パッケージは config / logger / db / httpserver から共通に import される基盤レイヤであり、
// import cycle を物理的に回避するため **他の internal package を import しない**。
// HTTP / Pub-Sub 経路でロギングが必要な場合は本パッケージ内で定義する ErrLogger interface を
// 受け取り、呼び出し側が logger.Logger を渡すことで structural typing で配線する。
package errors

import "net/http"

// Code はドメインエラーの機械可読な分類コード。HTTP ステータス / Pub-Sub ack-nack 判定の
// マッピングに用いられる。
type Code string

// 共通の Code 定数。requirements.md Req 3.1 / 3.3 / 3.4 と design.md の Code 列に対応。
const (
	// CodeInvalidRequest は不正な入力（バリデーション失敗等）。HTTP 400。
	CodeInvalidRequest Code = "invalid_request"
	// CodeUnauthenticated は未認証。HTTP 401。
	CodeUnauthenticated Code = "unauthenticated"
	// CodeForbidden は認可拒否。HTTP 403。
	CodeForbidden Code = "forbidden"
	// CodeNotFound はリソース不在。HTTP 404。
	CodeNotFound Code = "not_found"
	// CodeConflict は競合（重複登録等）。HTTP 409。
	CodeConflict Code = "conflict"
	// CodeBusinessRule はビジネスルール違反。HTTP 422。
	CodeBusinessRule Code = "business_rule_violation"
	// CodeInternal は予期しない内部エラー。HTTP 500。
	CodeInternal Code = "internal_error"
	// CodeUpstream は AMAPI 等の外部上流からのエラー。HTTP 502。
	CodeUpstream Code = "amapi_upstream_error"
	// CodeUnavailable はサービス利用不能（DB 不通等）。HTTP 503。
	CodeUnavailable Code = "service_unavailable"
	// CodeConfigInvalid は config の検証エラー。process exit のみで HTTP 経路では使われない。
	CodeConfigInvalid Code = "config_invalid"
	// CodeTenantCtxMissing は tenant context 未確立。通常 panic 経路で 500 化される invariant 違反。
	CodeTenantCtxMissing Code = "tenant_context_missing"
)

// defaultHTTPStatus は Code から既定の HTTP ステータスコードを返す。
// Error.HTTPStatus が 0 の場合に WriteHTTP が利用する。
// 未知の Code は 500 にフォールバックする。
func defaultHTTPStatus(code Code) int {
	switch code {
	case CodeInvalidRequest:
		return http.StatusBadRequest
	case CodeUnauthenticated:
		return http.StatusUnauthorized
	case CodeForbidden:
		return http.StatusForbidden
	case CodeNotFound:
		return http.StatusNotFound
	case CodeConflict:
		return http.StatusConflict
	case CodeBusinessRule:
		return http.StatusUnprocessableEntity
	case CodeUpstream:
		return http.StatusBadGateway
	case CodeUnavailable:
		return http.StatusServiceUnavailable
	case CodeInternal, CodeTenantCtxMissing, CodeConfigInvalid:
		return http.StatusInternalServerError
	default:
		return http.StatusInternalServerError
	}
}
