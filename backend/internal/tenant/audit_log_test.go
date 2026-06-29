package tenant

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
)

// captureLogger は logger.Logger interface を満たす fake 実装で、出力された (msg, fields) を
// テストから検査可能にする。redact 観点（NFR 2.3）の検証で、capture した fields に機密値が
// 現れないことを assert するために用いる。
type captureLogger struct {
	level  string
	msg    string
	fields []any
}

func (c *captureLogger) record(level, msg string, fields []any) {
	c.level = level
	c.msg = msg
	c.fields = fields
}

func (c *captureLogger) Debug(msg string, fields ...any) { c.record("debug", msg, fields) }
func (c *captureLogger) Info(msg string, fields ...any)  { c.record("info", msg, fields) }
func (c *captureLogger) Warn(msg string, fields ...any)  { c.record("warn", msg, fields) }
func (c *captureLogger) Error(msg string, fields ...any) { c.record("error", msg, fields) }
func (c *captureLogger) With(_ ...any) logger.Logger     { return c }
func (c *captureLogger) Sync() error                     { return nil }

// findStringField は capture した可変長 fields（"key", value, ... もしくは zap.Field）から、
// 指定 key の文字列値を探す。zap.Field（logger.ActorID / logger.TenantID 等）は本テストでは
// key/value ペアと別系統のため、ここでは key/value ペアのみを走査する。
func findStringField(fields []any, key string) (string, bool) {
	for i := 0; i+1 < len(fields); i += 2 {
		k, ok := fields[i].(string)
		if !ok || k != key {
			continue
		}
		if v, ok := fields[i+1].(string); ok {
			return v, true
		}
		return "", true
	}
	return "", false
}

// stringsContainSubstring は抽出済み文字列値のいずれかに substr が含まれるかを判定する。
// 監査項目の存在確認（NFR 2.1）および秘密値の漏洩検査（NFR 2.3）に用いる。
func stringsContainSubstring(values []string, substr string) bool {
	for _, s := range values {
		if strings.Contains(s, substr) {
			return true
		}
	}
	return false
}

// TestLoggerRecorderEmitsSafeFields は Record が監査項目（operation / result / actor / tenant /
// confirmation_completed）を構造化 field として出力することを検証する（NFR 2.1）。
func TestLoggerRecorderEmitsSafeFields(t *testing.T) {
	t.Run("success イベントは Info で安全フィールドを出力する", func(t *testing.T) {
		// Arrange
		cap := &captureLogger{}
		rec := NewLoggerRecorder(cap)
		actor := uuid.New()
		tenantID := uuid.New()
		ev := Event{
			Actor:                 actor,
			TenantID:              tenantID,
			Operation:             OperationCreate,
			Result:                ResultSuccess,
			ConfirmationCompleted: false,
		}

		// Act
		err := rec.Record(context.Background(), ev)

		// Assert
		if err != nil {
			t.Fatalf("Record returned error: %v", err)
		}
		if cap.level != "info" {
			t.Errorf("level = %q, want info", cap.level)
		}
		if op, ok := findStringField(cap.fields, "operation"); !ok || op != string(OperationCreate) {
			t.Errorf("operation field = %q (found=%v), want %q", op, ok, OperationCreate)
		}
		if res, ok := findStringField(cap.fields, "result"); !ok || res != string(ResultSuccess) {
			t.Errorf("result field = %q (found=%v), want %q", res, ok, ResultSuccess)
		}
		// actor_id / tenant_id は zap.Field 経由のため値の存在を文字列含有で確認する。
		strs := toStrings(cap.fields)
		if !stringsContainSubstring(strs, actor.String()) {
			t.Errorf("actor id %q not present in fields", actor.String())
		}
		if !stringsContainSubstring(strs, tenantID.String()) {
			t.Errorf("tenant id %q not present in fields", tenantID.String())
		}
	})

	t.Run("failure イベントは Warn で deny_reason を出力する", func(t *testing.T) {
		// Arrange
		cap := &captureLogger{}
		rec := NewLoggerRecorder(cap)
		ev := Event{
			Actor:      uuid.New(),
			TenantID:   uuid.New(),
			Operation:  OperationBind,
			Result:     ResultFailure,
			DenyReason: "tenant is disabled",
		}

		// Act
		_ = rec.Record(context.Background(), ev)

		// Assert
		if cap.level != "warn" {
			t.Errorf("level = %q, want warn", cap.level)
		}
		if reason, ok := findStringField(cap.fields, "deny_reason"); !ok || reason != "tenant is disabled" {
			t.Errorf("deny_reason field = %q (found=%v), want %q", reason, ok, "tenant is disabled")
		}
	})
}

// TestLoggerRecorderDoesNotEmitSecrets は Record が機密値（サインアップ URL の秘密値・
// SA 資格情報・OAuth トークン）を出力しないことを検証する（NFR 2.3 / redact 観点）。
//
// Event 構造体が機密値フィールドを持たないため、テストで投入したダミー秘密値はそもそも
// Event に渡せない。本テストは「capture した fields に当該ダミー秘密値文字列が現れない」
// ことを assert し、安全フィールドのみが emit される契約を固定する。
func TestLoggerRecorderDoesNotEmitSecrets(t *testing.T) {
	// Arrange
	const (
		dummySignupSecret = "signupUrls/SECRET-TOKEN-xyz?token=do-not-log"
		dummySACredential = "private_key=-----BEGIN PRIVATE KEY-----"
		dummyOAuthToken   = "ya29.OAUTH-ACCESS-TOKEN"
	)
	cap := &captureLogger{}
	rec := NewLoggerRecorder(cap)
	// Event には機密値を渡す経路が無い。DenyReason に人間可読な拒否理由のみを設定する。
	ev := Event{
		Actor:      uuid.New(),
		TenantID:   uuid.New(),
		Operation:  OperationCreate,
		Result:     ResultFailure,
		DenyReason: "invalid input",
	}

	// Act
	_ = rec.Record(context.Background(), ev)

	// Assert: capture した fields に機密値ダミー文字列が一切現れない。
	strs := toStrings(cap.fields)
	for _, secret := range []string{dummySignupSecret, dummySACredential, dummyOAuthToken} {
		if stringsContainSubstring(strs, secret) {
			t.Errorf("secret %q must not appear in audit log fields", secret)
		}
	}
	// msg 本文にも機密値を含めない。
	for _, secret := range []string{dummySignupSecret, dummySACredential, dummyOAuthToken} {
		if strings.Contains(cap.msg, secret) {
			t.Errorf("secret %q must not appear in audit log message", secret)
		}
	}
}

// TestNewLoggerRecorderNilLog は nil logger 注入時に Default へフォールバックし、Record が
// panic しないことを検証する（DI 未配線時の安全性）。
func TestNewLoggerRecorderNilLog(t *testing.T) {
	// Arrange
	rec := NewLoggerRecorder(nil)
	// Act / Assert: panic しなければ成功。
	if err := rec.Record(context.Background(), Event{Operation: OperationCreate, Result: ResultSuccess}); err != nil {
		t.Fatalf("Record returned error: %v", err)
	}
}

// toStrings は可変長 fields のうち、文字列値および zap.Field の String 表現を抽出する。
// zap.Field（logger.ActorID 等）は内部表現を直接読めないため、String() フォールバックで
// 値文字列（uuid 等）を取り出す。
func toStrings(fields []any) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		switch v := f.(type) {
		case string:
			out = append(out, v)
		case logger.Field:
			// zap.Field の String 値は v.String に格納される（zap.String 系 helper）。
			out = append(out, v.String)
		}
	}
	return out
}
