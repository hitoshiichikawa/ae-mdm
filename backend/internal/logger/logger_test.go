package logger

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	stdErrors "errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	internalerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// newTestLogger は buffer に書き出す Logger を返すテスト用ヘルパ。
// level は parseLevel で解釈される文字列を渡す。
func newTestLogger(t *testing.T, level string) (Logger, *threadSafeBuffer) {
	t.Helper()
	lvl, err := parseLevel(level)
	if err != nil {
		t.Fatalf("parseLevel(%q): %v", level, err)
	}
	encCfg := zap.NewProductionEncoderConfig()
	encCfg.TimeKey = "" // 簡素化
	encCfg.EncodeLevel = zapcore.LowercaseLevelEncoder
	enc := zapcore.NewJSONEncoder(encCfg)
	buf := &threadSafeBuffer{}
	core := zapcore.NewCore(enc, zapcore.AddSync(buf), lvl)
	return &zapLogger{z: zap.New(core)}, buf
}

// threadSafeBuffer は test 中の concurrent 書込みを安全にする lazy な buffer。
type threadSafeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *threadSafeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *threadSafeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func decodeLogLine(t *testing.T, raw string) map[string]any {
	t.Helper()
	out := map[string]any{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &out); err != nil {
		t.Fatalf("failed to decode log line %q: %v", raw, err)
	}
	return out
}

func TestLogger_AddsFieldsHelpers(t *testing.T) {
	// Arrange
	l, buf := newTestLogger(t, "info")
	tid := uuid.New()
	aid := uuid.New()

	// Act
	l.Info("started",
		TenantID(tid),
		RequestID("req-123"),
		MessageID("msg-456"),
		ActorID(aid),
	)

	// Assert
	m := decodeLogLine(t, buf.String())
	if m["tenant_id"] != tid.String() {
		t.Errorf("tenant_id = %v, want %v", m["tenant_id"], tid.String())
	}
	if m["request_id"] != "req-123" {
		t.Errorf("request_id = %v, want req-123", m["request_id"])
	}
	if m["message_id"] != "msg-456" {
		t.Errorf("message_id = %v, want msg-456", m["message_id"])
	}
	if m["actor_id"] != aid.String() {
		t.Errorf("actor_id = %v, want %v", m["actor_id"], aid.String())
	}
}

func TestLogger_LevelThreshold_FiltersBelow(t *testing.T) {
	t.Run("level=info のとき debug は出力されない", func(t *testing.T) {
		l, buf := newTestLogger(t, "info")
		l.Debug("hidden")
		if buf.String() != "" {
			t.Fatalf("debug log should be filtered, got %q", buf.String())
		}
	})

	t.Run("level=debug のとき debug が出力される", func(t *testing.T) {
		l, buf := newTestLogger(t, "debug")
		l.Debug("visible")
		if !strings.Contains(buf.String(), "visible") {
			t.Fatalf("debug log should be emitted, got %q", buf.String())
		}
	})

	t.Run("level=warn のとき info が抑止される", func(t *testing.T) {
		l, buf := newTestLogger(t, "warn")
		l.Info("hidden")
		l.Warn("emitted")
		s := buf.String()
		if strings.Contains(s, "hidden") {
			t.Fatalf("info should be filtered with warn level, got %q", s)
		}
		if !strings.Contains(s, "emitted") {
			t.Fatalf("warn should be emitted, got %q", s)
		}
	})
}

func TestRedactFields_RedactsSecretsBySubstring(t *testing.T) {
	cases := []struct {
		key       string
		redacted  bool
	}{
		{"session_secret", true},
		{"id_token", true},
		{"access_token", true},
		{"refresh_token", true},
		{"cookie", true},
		{"set-cookie", true}, // substring 一致
		{"google_application_credentials", true},
		{"sa_json", true},
		{"private_key", true},
		{"password", true},
		{"X-User-Password", true}, // case-insensitive
		{"tenant_id", false},
		{"request_id", false},
		{"latency_ms", false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.key, func(t *testing.T) {
			in := []any{c.key, "secret-value-xxx"}
			out := redactFields(in)
			val := out[1]
			if c.redacted {
				if val != redactedPlaceholder {
					t.Fatalf("key %q should be redacted, got %v", c.key, val)
				}
			} else {
				if val == redactedPlaceholder {
					t.Fatalf("key %q should NOT be redacted, got %v", c.key, val)
				}
			}
		})
	}
}

func TestLogger_RedactsSecretFieldValues(t *testing.T) {
	// Arrange
	l, buf := newTestLogger(t, "info")

	// Act
	l.Info("oidc verify",
		"id_token", "eyJhbGciOiJIUzI1NiJ9.payload.sig",
		"tenant_id", "tenant-xxx",
		"cookie", "session=abcdef",
	)

	// Assert
	m := decodeLogLine(t, buf.String())
	if m["id_token"] != redactedPlaceholder {
		t.Errorf("id_token should be redacted, got %v", m["id_token"])
	}
	if m["cookie"] != redactedPlaceholder {
		t.Errorf("cookie should be redacted, got %v", m["cookie"])
	}
	if m["tenant_id"] != "tenant-xxx" {
		t.Errorf("tenant_id should remain unmodified, got %v", m["tenant_id"])
	}
}

func TestLogger_With_PreservesFields(t *testing.T) {
	// Arrange
	l, buf := newTestLogger(t, "info")
	tid := uuid.New()
	scoped := l.With(TenantID(tid))

	// Act
	scoped.Info("scoped event")

	// Assert
	m := decodeLogLine(t, buf.String())
	if m["tenant_id"] != tid.String() {
		t.Errorf("With() did not preserve tenant_id, got %v", m["tenant_id"])
	}
}

func TestErr_DomainError_StructuredFields(t *testing.T) {
	// Arrange
	l, buf := newTestLogger(t, "info")
	cause := stdErrors.New("boom")
	domain := internalerrors.Wrap(internalerrors.CodeUnavailable, "db is down", cause)

	// Act
	l.Error("upstream failure", Err(domain))

	// Assert
	m := decodeLogLine(t, buf.String())
	if m["error_code"] != string(internalerrors.CodeUnavailable) {
		t.Errorf("error_code = %v, want %v", m["error_code"], internalerrors.CodeUnavailable)
	}
	if m["error_message"] != "db is down" {
		t.Errorf("error_message = %v, want \"db is down\"", m["error_message"])
	}
	if m["error_cause"] != "boom" {
		t.Errorf("error_cause = %v, want \"boom\"", m["error_cause"])
	}
}

func TestErr_NilError_SkipsField(t *testing.T) {
	// Arrange
	l, buf := newTestLogger(t, "info")

	// Act
	l.Info("ok", Err(nil))

	// Assert: error_ プレフィックスが入らない
	s := buf.String()
	if strings.Contains(s, "error_code") {
		t.Fatalf("nil error should not emit error_* fields, got %q", s)
	}
}

func TestFromContext_DefaultWhenAbsent(t *testing.T) {
	// Arrange
	ctx := context.Background()

	// Act
	l := FromContext(ctx)

	// Assert
	if l == nil {
		t.Fatalf("FromContext should never return nil")
	}
	// no panic
	l.Info("via default")
}

func TestWithContext_RoundTrip(t *testing.T) {
	// Arrange
	l, _ := newTestLogger(t, "info")
	ctx := WithContext(context.Background(), l)

	// Act
	got := FromContext(ctx)

	// Assert
	if got != l {
		t.Fatalf("FromContext did not return the value put by WithContext")
	}
}

// satisfiesErrLogger は Logger.Warn / Logger.Error が *errors.ErrLogger interface を
// 暗黙的に満たすことの compile-time verification。
//
// 直接 errors.WriteHTTP / ShouldAck の引数として渡せることが design.md の structural typing
// 配線の前提なので、interface 一致を test で固定する。
func TestLogger_SatisfiesErrLoggerInterface(t *testing.T) {
	var l Logger = &zapLogger{z: zap.NewNop()}
	var _ internalerrors.ErrLogger = l // compile-time check
	// 実体としても WARN / ERROR を呼べる
	l.Warn("test")
	l.Error("test")
}

func TestNewLogger_AppliesConfigDefaults(t *testing.T) {
	// Arrange: stderr 出力にしてプロセス stderr を汚さない設計はテスト用 helper に任せ、
	// ここでは NewLogger が config から構築されることだけ確認する。
	cfg := config.Config{
		LogLevel:  "info",
		LogFormat: "json",
		LogOutput: "stderr",
	}

	// Act
	l, err := NewLogger(cfg)

	// Assert
	if err != nil {
		t.Fatalf("NewLogger error: %v", err)
	}
	if l == nil {
		t.Fatalf("NewLogger returned nil")
	}
}

func TestNewLogger_RejectsInvalidLevel(t *testing.T) {
	cfg := config.Config{LogLevel: "loud", LogFormat: "json", LogOutput: "stderr"}
	_, err := NewLogger(cfg)
	if err == nil {
		t.Fatalf("expected error for invalid level")
	}
	var de *internalerrors.Error
	if !stdErrors.As(err, &de) || de.Code != internalerrors.CodeConfigInvalid {
		t.Fatalf("expected CodeConfigInvalid, got %v", err)
	}
}

func TestNewLogger_RejectsInvalidFormat(t *testing.T) {
	cfg := config.Config{LogLevel: "info", LogFormat: "xml", LogOutput: "stderr"}
	_, err := NewLogger(cfg)
	if err == nil {
		t.Fatalf("expected error for invalid format")
	}
}

func TestNewLogger_FileOutput(t *testing.T) {
	// Arrange: fileOpener を差し替えて in-memory writer に書き出す
	orig := fileOpener
	t.Cleanup(func() { fileOpener = orig })

	var captured io.Writer
	tsBuf := &threadSafeBuffer{}
	captured = tsBuf
	fileOpener = func(path string) (io.WriteCloser, error) {
		return nopWriteCloser{captured}, nil
	}

	cfg := config.Config{LogLevel: "info", LogFormat: "json", LogOutput: "/tmp/fake.log"}

	// Act
	l, err := NewLogger(cfg)
	if err != nil {
		t.Fatalf("NewLogger error: %v", err)
	}
	l.Info("hello")

	// Assert
	if !strings.Contains(tsBuf.String(), "hello") {
		t.Fatalf("expected log to be written to swapped writer, got %q", tsBuf.String())
	}
}

type nopWriteCloser struct{ w io.Writer }

func (n nopWriteCloser) Write(p []byte) (int, error) { return n.w.Write(p) }
func (n nopWriteCloser) Close() error                { return nil }
