package errors

import stdErrors "errors"

// ShouldAck は worker（Pub/Sub subscriber 等）の最外層で err を ack / nack 判定に写像する。
// requirements.md Req 3.4 / design.md Components: Domain Error 型節の worker_mapping 仕様に対応。
//
// 判定規則:
//   - err == nil                              → ack（log は呼ばない）
//   - *Error で IsTransient=true             → nack（WARN ログ）
//   - *Error で IsTransient=false            → ack（ERROR ログ。恒常的失敗は再配信しても結果が変わらない）
//   - 独自 Error 型でない error              → CodeInternal / IsTransient=true として wrap した上で nack（WARN ログ）
//
// 戻り値が true なら ack（再配信しない）、false なら nack（再配信させる）。
// log が nil の場合はログ副作用を持たない（判定のみを行う）。
func ShouldAck(err error, log ErrLogger) (ack bool) {
	if err == nil {
		return true
	}

	var de *Error
	if stdErrors.As(err, &de) && de != nil {
		// ack/nack 判定は IsTransient へ一元化し、ここはログレベル選択のみを担う（両者の drift を防ぐ）。
		if IsTransient(err) {
			emitWorkerLog(log, "warn", de, err)
			return false
		}
		emitWorkerLog(log, "error", de, err)
		return true
	}

	// 独自 Error 型でない error は CodeInternal / IsTransient=true として扱う。
	wrapped := &Error{
		Code:        CodeInternal,
		Message:     "unhandled worker error",
		IsTransient: true,
		Cause:       err,
	}
	emitWorkerLog(log, "warn", wrapped, err)
	return false
}

// IsTransient は err を ShouldAck と同一規則で「再処理保持（nack）すべき一時的失敗か」へ写像する純粋関数。
//
// ShouldAck の nack 判定（戻り値 false）と常に一致する（`IsTransient(err) == !ShouldAck(err, nil)`）。
// 判定規則:
//   - err == nil                    → false（成功は一時的失敗ではない）
//   - *Error で IsTransient=true    → true（一時的失敗 = nack）
//   - *Error で IsTransient=false   → false（恒常的失敗 = ack）
//   - 独自 Error 型でない error     → true（CodeInternal/IsTransient=true 相当として nack）
//
// ログ副作用を持たない点だけが ShouldAck と異なる。worker 最外層の ack/nack 写像と、その手前で
// 副作用（dedupe claim の release 等）を一時的失敗時のみ行いたい呼び出し側とで、同一規則を共有する
// ために切り出す（両者が別実装になると ack 判定と副作用が食い違う / 例: 恒常的失敗で ack 済みなのに
// claim を消して後続 duplicate を再 dispatch する不整合を防ぐ）。
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	var de *Error
	if stdErrors.As(err, &de) && de != nil {
		return de.IsTransient
	}
	// 独自 Error 型でない error は ShouldAck と同様に CodeInternal/IsTransient=true 相当として扱う。
	return true
}

// emitWorkerLog は ShouldAck のロギング副作用を集約する。
// level は "warn" または "error" を取る。
func emitWorkerLog(log ErrLogger, level string, domainErr *Error, originalErr error) {
	if log == nil {
		return
	}
	cause := domainErr.Cause
	if cause == nil {
		cause = originalErr
	}
	fields := []any{
		"code", string(domainErr.Code),
		"message", domainErr.Message,
		"is_transient", domainErr.IsTransient,
		"cause", causeMessage(cause),
	}
	switch level {
	case "warn":
		log.Warn("worker_error", fields...)
	case "error":
		log.Error("worker_error", fields...)
	}
}
