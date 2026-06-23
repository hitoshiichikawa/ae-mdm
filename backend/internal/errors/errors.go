package errors

import "fmt"

// Error は ae-mdm の独自エラー型。requirements.md Req 3.1 / 3.2 / 3.3 / 3.4 / 3.5 と
// design.md Components: Domain Error 型と HTTP/Pub-Sub マッピング節に対応する。
//
// HTTPStatus は 0 の場合に Code から defaultHTTPStatus 経由で決定される。
// IsTransient は worker（Pub/Sub subscriber 等）の ack/nack 判定に利用され、true なら一時的失敗
// として nack（再試行を期待）、false なら恒常的失敗として ack（再配信しても結果は変わらない）。
// Cause は wrap 元の error で、errors.Is / errors.As 互換のために Unwrap で公開される。
type Error struct {
	Code        Code
	Message     string
	HTTPStatus  int
	IsTransient bool
	Cause       error
}

// New は cause を持たない独自 Error を作る。
// HTTPStatus は 0 のまま返し、WriteHTTP 時に Code から自動決定される。
func New(code Code, message string) *Error {
	return &Error{Code: code, Message: message}
}

// Wrap は cause を持つ独自 Error を作る。
// 既存の error を保持しつつ Code / Message を上書きする用途に使う。
func Wrap(code Code, message string, cause error) *Error {
	return &Error{Code: code, Message: message, Cause: cause}
}

// Error は error interface を満たす。Cause がある場合は ": <cause>" を後置する。
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap は errors.Is / errors.As が cause を辿れるようにする。
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// EffectiveHTTPStatus は HTTPStatus が 0 のとき Code から既定値を返し、そうでなければ
// 設定された値をそのまま返す。WriteHTTP の内部 helper としても使う。
func (e *Error) EffectiveHTTPStatus() int {
	if e == nil {
		return defaultHTTPStatus(CodeInternal)
	}
	if e.HTTPStatus != 0 {
		return e.HTTPStatus
	}
	return defaultHTTPStatus(e.Code)
}
