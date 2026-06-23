package errors

import (
	"encoding/json"
	stdErrors "errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeLogger は ErrLogger interface を満たすテスト用 spy。
// 各メソッドの呼出回数と最終の msg / fields を記録する。
type fakeLogger struct {
	warnCalls  int
	errorCalls int
	lastMsg    string
	lastFields []any
}

func (f *fakeLogger) Warn(msg string, fields ...any) {
	f.warnCalls++
	f.lastMsg = msg
	f.lastFields = fields
}

func (f *fakeLogger) Error(msg string, fields ...any) {
	f.errorCalls++
	f.lastMsg = msg
	f.lastFields = fields
}

func TestError_ErrorMessage(t *testing.T) {
	t.Run("Cause なしのとき code と message を結合した文字列を返す", func(t *testing.T) {
		// Arrange
		e := New(CodeInvalidRequest, "name is empty")

		// Act
		got := e.Error()

		// Assert
		want := "invalid_request: name is empty"
		if got != want {
			t.Fatalf("Error() = %q, want %q", got, want)
		}
	})

	t.Run("Cause ありのとき cause が後置されること", func(t *testing.T) {
		// Arrange
		cause := stdErrors.New("boom")
		e := Wrap(CodeInternal, "internal", cause)

		// Act
		got := e.Error()

		// Assert
		want := "internal_error: internal: boom"
		if got != want {
			t.Fatalf("Error() = %q, want %q", got, want)
		}
	})
}

func TestError_IsAsCompatibility(t *testing.T) {
	t.Run("Wrap した cause が errors.Is で取れること", func(t *testing.T) {
		// Arrange
		sentinel := stdErrors.New("sentinel")
		wrapped := Wrap(CodeInternal, "wrap", sentinel)

		// Act / Assert
		if !stdErrors.Is(wrapped, sentinel) {
			t.Fatalf("errors.Is(wrapped, sentinel) = false, want true")
		}
	})

	t.Run("errors.As で *Error として取り出せること", func(t *testing.T) {
		// Arrange
		var iface error = New(CodeForbidden, "forbidden")

		// Act
		var target *Error
		ok := stdErrors.As(iface, &target)

		// Assert
		if !ok {
			t.Fatalf("errors.As(iface, &target) = false, want true")
		}
		if target.Code != CodeForbidden {
			t.Fatalf("target.Code = %q, want %q", target.Code, CodeForbidden)
		}
	})
}

func TestDefaultHTTPStatus_Mapping(t *testing.T) {
	cases := []struct {
		code   Code
		status int
	}{
		{CodeInvalidRequest, http.StatusBadRequest},
		{CodeUnauthenticated, http.StatusUnauthorized},
		{CodeForbidden, http.StatusForbidden},
		{CodeNotFound, http.StatusNotFound},
		{CodeConflict, http.StatusConflict},
		{CodeBusinessRule, http.StatusUnprocessableEntity},
		{CodeInternal, http.StatusInternalServerError},
		{CodeUpstream, http.StatusBadGateway},
		{CodeUnavailable, http.StatusServiceUnavailable},
		{CodeConfigInvalid, http.StatusInternalServerError},
		{CodeTenantCtxMissing, http.StatusInternalServerError},
		{Code("unknown_code"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		c := c
		t.Run(string(c.code), func(t *testing.T) {
			// Act
			got := defaultHTTPStatus(c.code)

			// Assert
			if got != c.status {
				t.Fatalf("defaultHTTPStatus(%q) = %d, want %d", c.code, got, c.status)
			}
		})
	}
}

func TestWriteHTTP_DomainError(t *testing.T) {
	t.Run("Code に応じた HTTP status と JSON ボディを返すこと", func(t *testing.T) {
		// Arrange
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		log := &fakeLogger{}
		err := New(CodeForbidden, "no permission")

		// Act
		WriteHTTP(rec, req, err, log)
		ClearWriter(rec)

		// Assert
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		var body httpBody
		if jerr := json.Unmarshal(rec.Body.Bytes(), &body); jerr != nil {
			t.Fatalf("json decode error: %v", jerr)
		}
		if body.Code != CodeForbidden || body.Message != "no permission" {
			t.Fatalf("body = %+v", body)
		}
		if log.warnCalls != 1 || log.errorCalls != 0 {
			t.Fatalf("expected 1 WARN log call, got warn=%d error=%d", log.warnCalls, log.errorCalls)
		}
	})

	t.Run("5xx の独自 Error は ERROR ログを 1 回出すこと", func(t *testing.T) {
		// Arrange
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		log := &fakeLogger{}
		err := New(CodeUnavailable, "db down")

		// Act
		WriteHTTP(rec, req, err, log)
		ClearWriter(rec)

		// Assert
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", rec.Code)
		}
		if log.errorCalls != 1 || log.warnCalls != 0 {
			t.Fatalf("expected 1 ERROR log call, got warn=%d error=%d", log.warnCalls, log.errorCalls)
		}
	})
}

func TestWriteHTTP_NonDomainError_DefaultsTo500(t *testing.T) {
	// Arrange
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	log := &fakeLogger{}
	raw := stdErrors.New("unexpected boom")

	// Act
	WriteHTTP(rec, req, raw, log)
	ClearWriter(rec)

	// Assert
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	var body httpBody
	if jerr := json.Unmarshal(rec.Body.Bytes(), &body); jerr != nil {
		t.Fatalf("json decode error: %v", jerr)
	}
	if body.Code != CodeInternal {
		t.Fatalf("body.Code = %q, want %q", body.Code, CodeInternal)
	}
	if log.errorCalls != 1 {
		t.Fatalf("expected 1 ERROR log call, got %d", log.errorCalls)
	}
}

func TestWriteHTTP_CalledOnce(t *testing.T) {
	// Arrange
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	log := &fakeLogger{}

	// Act: 同一 writer に 2 回呼ぶ
	WriteHTTP(rec, req, New(CodeNotFound, "absent"), log)
	WriteHTTP(rec, req, New(CodeForbidden, "denied"), log)
	ClearWriter(rec)

	// Assert: 1 回目のステータスのみが採用される
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (2 回目が抑止されること)", rec.Code)
	}
	// log も 1 回目のみ
	if log.warnCalls != 1 {
		t.Fatalf("expected 1 WARN log call, got %d", log.warnCalls)
	}
}

func TestWriteHTTP_NilLogger_NoPanic(t *testing.T) {
	// Arrange
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)

	// Act / Assert: panic しないこと
	WriteHTTP(rec, req, New(CodeInvalidRequest, "bad"), nil)
	ClearWriter(rec)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestShouldAck_NilError_AcksAndNoLog(t *testing.T) {
	// Arrange
	log := &fakeLogger{}

	// Act
	ack := ShouldAck(nil, log)

	// Assert
	if !ack {
		t.Fatalf("ack = false, want true (nil err は ack)")
	}
	if log.warnCalls != 0 || log.errorCalls != 0 {
		t.Fatalf("log should not be called for nil err, got warn=%d error=%d", log.warnCalls, log.errorCalls)
	}
}

func TestShouldAck_TransientDomainError_NacksWithWarn(t *testing.T) {
	// Arrange
	log := &fakeLogger{}
	e := &Error{Code: CodeUnavailable, Message: "down", IsTransient: true}

	// Act
	ack := ShouldAck(e, log)

	// Assert
	if ack {
		t.Fatalf("ack = true, want false (transient は nack)")
	}
	if log.warnCalls != 1 || log.errorCalls != 0 {
		t.Fatalf("expected 1 WARN log call, got warn=%d error=%d", log.warnCalls, log.errorCalls)
	}
}

func TestShouldAck_PermanentDomainError_AcksWithError(t *testing.T) {
	// Arrange
	log := &fakeLogger{}
	e := &Error{Code: CodeBusinessRule, Message: "rule", IsTransient: false}

	// Act
	ack := ShouldAck(e, log)

	// Assert
	if !ack {
		t.Fatalf("ack = false, want true (恒常的失敗は ack)")
	}
	if log.errorCalls != 1 || log.warnCalls != 0 {
		t.Fatalf("expected 1 ERROR log call, got warn=%d error=%d", log.warnCalls, log.errorCalls)
	}
}

func TestShouldAck_UnknownErrorType_NacksWithWarn(t *testing.T) {
	// Arrange
	log := &fakeLogger{}
	raw := stdErrors.New("unexpected")

	// Act
	ack := ShouldAck(raw, log)

	// Assert
	if ack {
		t.Fatalf("ack = true, want false (独自型外は CodeInternal/Transient=true として nack)")
	}
	if log.warnCalls != 1 || log.errorCalls != 0 {
		t.Fatalf("expected 1 WARN log call, got warn=%d error=%d", log.warnCalls, log.errorCalls)
	}
}

func TestShouldAck_NilLogger_NoPanic(t *testing.T) {
	// Arrange
	e := &Error{Code: CodeUnavailable, Message: "x", IsTransient: true}

	// Act / Assert: panic しない
	ack := ShouldAck(e, nil)
	if ack {
		t.Fatalf("ack = true, want false")
	}
}
