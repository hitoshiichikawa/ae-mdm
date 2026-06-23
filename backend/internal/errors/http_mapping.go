package errors

import (
	"context"
	"encoding/json"
	stdErrors "errors"
	"net/http"
	"sync"
)

// ErrLogger は WriteHTTP / ShouldAck が利用する極小ロガー interface。
// internal/errors は logger を import しない（import cycle 回避）。internal/logger.Logger の
// 具象実装は本 interface を structural typing で暗黙的に満たすため、呼び出し側が logger.Logger
// をそのまま渡せる。
type ErrLogger interface {
	Warn(msg string, fields ...any)
	Error(msg string, fields ...any)
}

// httpBody は WriteHTTP が JSON 応答として書き出す共通フォーマット。
type httpBody struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
}

// writtenCtxKey は request context に「既に書き込み済み」を記録するための key。
// 呼び出し側が WithContext で sentinel を立てておけば二重呼び出しを防げる。
type writtenCtxKey struct{}

// writtenWriters は同じ ResponseWriter に対する 2 回目以降の WriteHTTP を抑止するための
// プロセスローカルな sentinel ストア。HTTP リクエスト終了後は w 参照が消えるため、エントリは
// 短命であり、ハンドラの defer 終了時に削除する。テストやハンドラ書込み完了の判定が
// request context で代替されない場合のフォールバックとして使う。
var writtenWriters sync.Map // map[http.ResponseWriter]struct{}

// WriteHTTP は HTTP ハンドラ最外層で err を JSON 応答に変換し、必要に応じて WARN / ERROR
// ログを出す。requirements.md Req 3.3 / 3.5 と design.md Components: Domain Error 型節に対応。
//
//   - err が *Error の場合: EffectiveHTTPStatus() でステータスを決定。5xx は ERROR、4xx は WARN
//     としてログ（log が nil でなければ）
//   - err が *Error 以外の場合: CodeInternal / HTTPStatus=500 / IsTransient=true として wrap し
//     ERROR ログ
//
// 同じ ResponseWriter に対する 2 回目の呼び出しは sentinel で抑止する（called-once 契約。
// design.md「Postconditions」と整合）。リクエスト処理終了時の cleanup は呼び出し側に委ねる
// （実運用では recover middleware の defer で `ClearWriter(w)` を呼ぶ）。
func WriteHTTP(w http.ResponseWriter, r *http.Request, err error, log ErrLogger) {
	if w == nil {
		return
	}
	// (1) request context 経由の sentinel チェック（middleware が WithWritten ctx を立てた場合）。
	if r != nil {
		if v := r.Context().Value(writtenCtxKey{}); v != nil {
			return
		}
	}
	// (2) writer pointer ベースの sentinel チェック（同 writer に対する重複呼出）。
	if _, loaded := writtenWriters.LoadOrStore(w, struct{}{}); loaded {
		return
	}

	domainErr := toDomainError(err)
	status := domainErr.EffectiveHTTPStatus()

	emitHTTPLog(log, domainErr, err, status)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	body := httpBody{Code: domainErr.Code, Message: domainErr.Message}
	// エンコード失敗は応答ボディを諦めるしかないため無視する（headers は既送）。
	_ = json.NewEncoder(w).Encode(body)
}

// ClearWriter は WriteHTTP の sentinel エントリを除去する。HTTP ハンドラの defer 終了時に
// 呼び出し、writer 参照のリークを防ぐ。テスト用にも有用。
func ClearWriter(w http.ResponseWriter) {
	if w == nil {
		return
	}
	writtenWriters.Delete(w)
}

// WithWritten は request context に書込み済み sentinel を立てた context を返す。
// recover middleware が応答書込み後にこれを使うと、後段の handler が WriteHTTP を再呼出
// しても無効化される。
func WithWritten(ctx context.Context) context.Context {
	return context.WithValue(ctx, writtenCtxKey{}, struct{}{})
}

// toDomainError は err が *Error なら return、そうでなければ CodeInternal で wrap する。
func toDomainError(err error) *Error {
	if err == nil {
		return &Error{Code: CodeInternal, Message: "unknown error", IsTransient: true}
	}
	var de *Error
	if stdErrors.As(err, &de) && de != nil {
		return de
	}
	return &Error{
		Code:        CodeInternal,
		Message:     "internal server error",
		IsTransient: true,
		Cause:       err,
	}
}

// emitHTTPLog は WriteHTTP のロギング副作用を集約する。log が nil なら no-op。
// 5xx は ERROR、4xx は WARN として 1 回だけ呼ぶ。
func emitHTTPLog(log ErrLogger, domainErr *Error, originalErr error, status int) {
	if log == nil {
		return
	}
	cause := domainErr.Cause
	if cause == nil {
		cause = originalErr
	}
	switch {
	case status >= 500:
		log.Error("http_error",
			"code", string(domainErr.Code),
			"status", status,
			"message", domainErr.Message,
			"cause", causeMessage(cause),
		)
	case status >= 400:
		log.Warn("http_error",
			"code", string(domainErr.Code),
			"status", status,
			"message", domainErr.Message,
			"cause", causeMessage(cause),
		)
	}
}

// causeMessage は cause が nil でなければそのメッセージを、nil なら空文字を返す。
func causeMessage(cause error) string {
	if cause == nil {
		return ""
	}
	return cause.Error()
}
