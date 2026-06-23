// Package logger は ae-mdm の全プロセスが共通利用する構造化ログのファクトリと helper を
// 提供する。requirements.md Req 2.1 / 2.2 / 2.3 / 2.4 / 2.5 / design.md Components: Logger 節に
// 対応する。
//
// 本パッケージは internal/errors を import するが、逆方向（errors → logger）の import は
// 禁止（cycle 物理回避）。Logger interface の Warn / Error シグネチャは errors.ErrLogger
// interface を structural typing で暗黙的に満たすため、errors.WriteHTTP / ShouldAck に
// Logger をそのまま渡せる。
package logger

import (
	stdErrors "errors"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	internalerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// Logger は process 内で使用する構造化ログ interface。
// Warn / Error のシグネチャは errors.ErrLogger と一致しており、structural typing で
// errors.WriteHTTP / ShouldAck にそのまま渡せる（cycle 回避のため errors → logger の直接依存は持たない）。
type Logger interface {
	Debug(msg string, fields ...any)
	Info(msg string, fields ...any)
	Warn(msg string, fields ...any)
	Error(msg string, fields ...any)
	With(fields ...any) Logger
	Sync() error
}

// Field は zap.Field の alias。helper の戻り値型として使い、可変長引数 (...any) で
// Logger 各メソッドに渡された際に内部で zap.Field として認識される。
type Field = zap.Field

// fileOpener は config.LogOutput がファイルパスを指す場合に呼び出される writer 作成関数。
// 既定実装は os.OpenFile だが、テストで差し替え可能にしてある。
var fileOpener = func(path string) (io.WriteCloser, error) {
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
}

// zapLogger は Logger interface の zap 実装。
type zapLogger struct {
	z *zap.Logger
}

// NewLogger は config から構造化 logger を構築する。
// config.LogLevel / LogFormat / LogOutput を見て zap.Core を組み立てる。
// 不正な値が指定された場合 *errors.Error{Code: CodeConfigInvalid} を返す。
func NewLogger(cfg config.Config) (Logger, error) {
	level, err := parseLevel(cfg.LogLevel)
	if err != nil {
		return nil, err
	}
	encoder, err := newEncoder(cfg.LogFormat)
	if err != nil {
		return nil, err
	}
	ws, err := newWriteSyncer(cfg.LogOutput)
	if err != nil {
		return nil, err
	}
	core := zapcore.NewCore(encoder, ws, level)
	z := zap.New(core, zap.AddCaller(), zap.AddStacktrace(zapcore.ErrorLevel))
	return &zapLogger{z: z}, nil
}

// parseLevel は文字列を zapcore.Level に変換する。debug / info / warn / error をサポート。
func parseLevel(s string) (zapcore.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return zapcore.DebugLevel, nil
	case "info", "":
		return zapcore.InfoLevel, nil
	case "warn", "warning":
		return zapcore.WarnLevel, nil
	case "error":
		return zapcore.ErrorLevel, nil
	default:
		return zapcore.InfoLevel, internalerrors.New(internalerrors.CodeConfigInvalid,
			fmt.Sprintf("unsupported log level: %q", s))
	}
}

// newEncoder は json / console を切り替える。
func newEncoder(format string) (zapcore.Encoder, error) {
	cfg := zap.NewProductionEncoderConfig()
	cfg.TimeKey = "ts"
	cfg.EncodeTime = zapcore.ISO8601TimeEncoder
	cfg.EncodeLevel = zapcore.LowercaseLevelEncoder
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "json", "":
		return zapcore.NewJSONEncoder(cfg), nil
	case "console":
		return zapcore.NewConsoleEncoder(cfg), nil
	default:
		return nil, internalerrors.New(internalerrors.CodeConfigInvalid,
			fmt.Sprintf("unsupported log format: %q", format))
	}
}

// newWriteSyncer は stderr / stdout / ファイルパスのいずれかから zapcore.WriteSyncer を構築する。
func newWriteSyncer(output string) (zapcore.WriteSyncer, error) {
	switch strings.ToLower(strings.TrimSpace(output)) {
	case "stderr", "":
		return zapcore.Lock(os.Stderr), nil
	case "stdout":
		return zapcore.Lock(os.Stdout), nil
	default:
		f, err := fileOpener(output)
		if err != nil {
			return nil, internalerrors.Wrap(internalerrors.CodeConfigInvalid,
				fmt.Sprintf("failed to open log file: %s", output), err)
		}
		return zapcore.Lock(zapcore.AddSync(f)), nil
	}
}

// toZapFields は (...any) を zap.Field slice に変換する。
//
//   - すでに zap.Field の場合はそのまま追加
//   - error の場合は zap.Error として追加（直前が key 文字列ではない場合）
//   - "key", value のペアは zap.Any として追加
//
// redaction はここで適用する（redactFields は key, value 列を前提とするが、zap.Field
// 直接渡しの場合は値の構造化が完了しているため、本実装では key/value ペアに対してのみ
// redaction を行う方針）。
func toZapFields(fields []any) []zap.Field {
	if len(fields) == 0 {
		return nil
	}
	// 先に key, value 列に対する redaction を適用。
	red := redactFields(fields)

	out := make([]zap.Field, 0, len(red)/2+1)
	i := 0
	for i < len(red) {
		switch v := red[i].(type) {
		case zap.Field:
			out = append(out, v)
			i++
		case string:
			if i+1 < len(red) {
				out = append(out, zap.Any(v, red[i+1]))
				i += 2
			} else {
				// key だけ残った異常系: 値として追加して終わる。
				out = append(out, zap.Any("invalid_field", v))
				i++
			}
		case error:
			out = append(out, zap.Error(v))
			i++
		default:
			out = append(out, zap.Any(fmt.Sprintf("field_%d", i), v))
			i++
		}
	}
	return out
}

func (l *zapLogger) Debug(msg string, fields ...any) { l.z.Debug(msg, toZapFields(fields)...) }
func (l *zapLogger) Info(msg string, fields ...any)  { l.z.Info(msg, toZapFields(fields)...) }
func (l *zapLogger) Warn(msg string, fields ...any)  { l.z.Warn(msg, toZapFields(fields)...) }
func (l *zapLogger) Error(msg string, fields ...any) { l.z.Error(msg, toZapFields(fields)...) }

func (l *zapLogger) With(fields ...any) Logger {
	return &zapLogger{z: l.z.With(toZapFields(fields)...)}
}

func (l *zapLogger) Sync() error { return l.z.Sync() }

// ----- field helpers -----

// TenantID は tenant_id を構造化フィールド化する。
func TenantID(id uuid.UUID) Field { return zap.String("tenant_id", id.String()) }

// RequestID は request_id を構造化フィールド化する。
func RequestID(id string) Field { return zap.String("request_id", id) }

// MessageID は Pub/Sub の message_id を構造化フィールド化する。
func MessageID(id string) Field { return zap.String("message_id", id) }

// ActorID は admin_user_id 等の操作主体 ID を構造化フィールド化する。
func ActorID(id uuid.UUID) Field { return zap.String("actor_id", id.String()) }

// Err は err を構造化フィールド化する。*errors.Error の場合は code / message / cause を分解し、
// それ以外は zap.Error と同様に "error" キーで包む。
//
// cause メッセージは redactCauseString で OIDC token / cookie / Authorization ヘッダ等の
// 機密パターンを redact してから出力する（PR #31 round-2 / round-3 review 由来 / Req 2.5）。
// 非 *errors.Error 経路（zap.Error fallback）でも同じ redact を適用するため zap.String で組み立てる。
func Err(err error) Field {
	if err == nil {
		return zap.Skip()
	}
	var de *internalerrors.Error
	if stdErrors.As(err, &de) && de != nil {
		causeMsg := ""
		if de.Cause != nil {
			causeMsg = redactCauseString(de.Cause.Error())
		}
		return zap.Inline(zapcore.ObjectMarshalerFunc(func(enc zapcore.ObjectEncoder) error {
			enc.AddString("error_code", string(de.Code))
			enc.AddString("error_message", de.Message)
			if causeMsg != "" {
				enc.AddString("error_cause", causeMsg)
			}
			return nil
		}))
	}
	return zap.String("error", redactCauseString(err.Error()))
}

// ----- context propagation -----

type ctxKey struct{}

var (
	defaultMu     sync.RWMutex
	defaultLogger Logger
)

// SetDefault は process 全体で使う default logger を設定する。
// 通常 cmd/api / cmd/worker の bootstrap で呼び出す。
func SetDefault(l Logger) {
	defaultMu.Lock()
	defer defaultMu.Unlock()
	defaultLogger = l
}

// Default は SetDefault で登録された logger を返す。未登録なら no-op の zap.NewNop ベースを返す。
func Default() Logger {
	defaultMu.RLock()
	defer defaultMu.RUnlock()
	if defaultLogger == nil {
		return &zapLogger{z: zap.NewNop()}
	}
	return defaultLogger
}

// WithContext は ctx に logger を埋め込んだ新しい context を返す。
func WithContext(ctx context.Context, l Logger) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, ctxKey{}, l)
}

// FromContext は ctx から logger を取り出す。未設定なら Default() を返す。
func FromContext(ctx context.Context) Logger {
	if ctx == nil {
		return Default()
	}
	if v := ctx.Value(ctxKey{}); v != nil {
		if l, ok := v.(Logger); ok && l != nil {
			return l
		}
	}
	return Default()
}
