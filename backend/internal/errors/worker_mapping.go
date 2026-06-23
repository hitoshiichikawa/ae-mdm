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
		if de.IsTransient {
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
