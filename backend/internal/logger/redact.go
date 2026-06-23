package logger

import "strings"

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
func redactFields(fields []any) []any {
	if len(fields) == 0 {
		return fields
	}
	out := make([]any, len(fields))
	copy(out, fields)
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
