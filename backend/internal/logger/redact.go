package logger

import (
	"regexp"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// redactKeySubstrings は機密情報を含む field 名のサブストリング allowlist。
// field 名（lower-case 化したもの）がこれらのいずれかを含む場合、その値を redactedPlaceholder で
// 置換する。requirements.md Req 2.5 / design.md Security Considerations 節と整合。
var redactKeySubstrings = []string{
	"session_secret",
	"id_token",
	"access_token",
	"refresh_token",
	"cookie",
	"google_application_credentials",
	"sa_json",
	"private_key",
	"password",
}

// redactedPlaceholder は redaction 対象 field の値を置換する固定文字列。
const redactedPlaceholder = "***"

// causePatternReplacers は cause 文字列内の機密パターンを検出して redact する正規表現。
// PR #31 round-2 / round-3 review 由来。OIDC token / cookie / Authorization header が
// upstream error 経由で cause に紛れ込んだ場合の平文ログ流出を防ぐ。
//
// 検出対象:
//   - JWT 形式（`eyJ...` で始まる長文字列。Header.Payload.Signature の 3 段構造）
//   - `Bearer <token>` 形式
//   - `Authorization: <...>` ヘッダ値
//   - `Cookie: <...>` / `Set-Cookie: <...>` 値
//   - `session=<value>` / `id_token=<value>` 等の代表的 query param / cookie attr
var causePatternReplacers = []struct {
	re   *regexp.Regexp
	repl string
}{
	// JWT-like: eyJ で始まる 20 文字以上の base64url 文字列が `.` で連結された 3 segments。
	{regexp.MustCompile(`eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+`), "<redacted_jwt>"},
	// Bearer scheme（Authorization header もここで吸収）
	{regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]+`), "Bearer <redacted>"},
	{regexp.MustCompile(`(?i)authorization:\s*\S+`), "Authorization: <redacted>"},
	// Cookie ヘッダ / Set-Cookie 値
	{regexp.MustCompile(`(?i)set-cookie:\s*[^\s;]+`), "Set-Cookie: <redacted>"},
	{regexp.MustCompile(`(?i)\bcookie:\s*[^\s;]+`), "Cookie: <redacted>"},
	// 代表的な秘密値の key=value 形式（session_secret / *_token / cookie value）
	{regexp.MustCompile(`(?i)(session_secret|id_token|access_token|refresh_token|session)=([^\s&;"]+)`), "$1=<redacted>"},
}

// shouldRedact は field 名が allowlist サブストリングのいずれかを含むかを判定する。
// 大文字小文字を無視して評価する。
func shouldRedact(key string) bool {
	if key == "" {
		return false
	}
	lower := strings.ToLower(key)
	for _, sub := range redactKeySubstrings {
		if strings.Contains(lower, sub) {
			return true
		}
	}
	return false
}

// redactFields は zap.Logger に渡す前に fields slice を走査し、redaction 対象の値を
// 固定文字列に置換した新しい slice を返す。fields は key, value, key, value, ... の交互列を
// 期待する（zap.SugaredLogger / Logger.With の慣習）。
//
// 副次効果として、各要素が zap.Field の場合は redactZapField で key/value を直接編集する
// （PR #31 round-2 / round-3 review 由来。`zap.String("id_token", token)` 直渡しの平文出力を防ぐ）。
func redactFields(fields []any) []any {
	if len(fields) == 0 {
		return fields
	}
	out := make([]any, len(fields))
	copy(out, fields)
	// (1) zap.Field 直渡しを redact（位置に関係なく key を見る）
	for i, v := range out {
		if zf, ok := v.(zap.Field); ok {
			out[i] = redactZapField(zf)
		}
	}
	// (2) key, value ペアを redact（key は string、value は次要素）
	for i := 0; i+1 < len(out); i += 2 {
		key, ok := out[i].(string)
		if !ok {
			continue
		}
		if shouldRedact(key) {
			out[i+1] = redactedPlaceholder
		}
	}
	return out
}

// redactZapField は zap.Field の key を redaction allowlist と照合し、該当時は値を
// redactedPlaceholder で置き換えた新しい zap.Field を返す（元の field は変更しない）。
// 非該当時は元の field をそのまま返す。
//
// zap.Field は zapcore.Field の alias で {Key, Type, Integer, String, Interface} を持つ。
// String 型・Interface 型・整数型のいずれにも対応するため、type を StringType に固定して
// String フィールドだけ書き換える単純な戦略を採る（数値秘密は本プロジェクトで該当例なし）。
func redactZapField(f zap.Field) zap.Field {
	if !shouldRedact(f.Key) {
		return f
	}
	return zap.String(f.Key, redactedPlaceholder)
}

// redactCauseString は free-text の cause メッセージから機密パターンを検出して redact する。
// causePatternReplacers の各正規表現を順に適用し、置換後の文字列を返す。空文字列はそのまま返す。
//
// 本関数は `logger.Err` / `errors.WriteHTTP` / `errors.ShouldAck` の cause 出力経路で利用し、
// upstream 由来の error メッセージに OIDC token / cookie / Authorization 値が含まれた場合の
// 平文ログ流出を防ぐ（PR #31 round-2 / round-3 review 由来）。
func redactCauseString(s string) string {
	if s == "" {
		return s
	}
	out := s
	for _, r := range causePatternReplacers {
		out = r.re.ReplaceAllString(out, r.repl)
	}
	return out
}

// 以下、zapcore 互換 stub: redact.go から zap package を import するために
// 直接の API 利用を増やすと無関係 import 増になるため、本ファイル内では zap / zapcore は
// 型 alias を介してのみ参照する。実際の使用は logger.go 側で行う。
var _ zapcore.Field = zap.Field{}
