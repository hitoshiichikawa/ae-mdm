package policy

import (
	"fmt"
	"regexp"
)

// 検証定数。マジックナンバーを意味のある名前で共有する（CLAUDE.md コード規約）。
const (
	// MaxAppCount は 1 ポリシーあたりのアプリ件数上限（umbrella #24 Req 4.5）。
	// 3,000 件「以下」を妥当とし、超過（3,001 以上）を上限超過として拒否する。
	MaxAppCount = 3000

	// MinAppCount はアプリ件数の下限（0 件）。アプリ「件数」は負値を取り得ないため、
	// 0 未満は成立しない不正入力（範囲外）として拒否する。Req 1.4 で 0 件は受理する。
	MinAppCount = 0

	// MinPasswordLength はパスワード最小桁数の許容下限。
	// AMAPI passwordMinimumLength（PasswordRequirements）は SDK v0.186.0 の field doc
	// （"A value of 0 means there is no restriction."）の通り 0 を「制限なし」を表す
	// 有効値として扱う。したがって下限は 0 とし、0 を受理して負値のみを範囲外として拒否する。
	MinPasswordLength = 0
	// MaxPasswordLength はパスワード最小桁数の許容上限（AMAPI passwordMinimumLength の上限 16）。
	MaxPasswordLength = 16

	// minWindowMinute / maxWindowMinute は WINDOWED 保守ウィンドウの分範囲（0〜1439 = 1 日の分数）。
	minWindowMinute = 0
	maxWindowMinute = 1439
)

// allowedEncryptionPolicies はセキュリティ領域 EncryptionPolicy の許容値集合（Req 3.3）。
// AMAPI encryptionPolicy enum に整合させた代表値。
var allowedEncryptionPolicies = map[string]struct{}{
	"ENABLED_WITHOUT_PASSWORD": {},
	"ENABLED_WITH_PASSWORD":    {},
}

// allowedPasswordQualities はセキュリティ領域 PasswordQuality の許容値集合（Req 3.3）。
// AMAPI passwordQuality enum（google.golang.org/api v0.186.0 androidmanagement/v1
// PasswordRequirements.PasswordQuality）の全許容値に整合させる。complexity-based の
// COMPLEXITY_LOW / COMPLEXITY_MEDIUM / COMPLEXITY_HIGH も AMAPI が受理する有効値のため含める。
var allowedPasswordQualities = map[string]struct{}{
	"PASSWORD_QUALITY_UNSPECIFIED": {},
	"BIOMETRIC_WEAK":               {},
	"SOMETHING":                    {},
	"NUMERIC":                      {},
	"NUMERIC_COMPLEX":              {},
	"ALPHABETIC":                   {},
	"ALPHANUMERIC":                 {},
	"COMPLEX":                      {},
	"COMPLEXITY_LOW":               {},
	"COMPLEXITY_MEDIUM":            {},
	"COMPLEXITY_HIGH":              {},
}

// allowedSystemUpdateTypes はシステム更新領域 Type の許容値集合（Req 4.3）。
// AMAPI systemUpdate.type enum に整合させた代表値。
var allowedSystemUpdateTypes = map[string]struct{}{
	"AUTOMATIC": {},
	"WINDOWED":  {},
	"POSTPONE":  {},
}

// packageNameSegment はパッケージ名の 1 セグメントの形式（英字始まり + 英数字・アンダースコア）。
var packageNameSegment = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

// rule は単一領域に対する table-driven な検証規則（tasks 8.1）。
//
// applicable は当該 rule が PolicyInput に適用可能か（対象領域が存在するか）を返す。
// check は不正項目を検出して result に収集する。各領域の規則を rule のスライスとして宣言し、
// Validate が順に適用することで、複数領域・複数項目の不正を 1 つの ValidationResult に
// まとめる（Req 6.2）。
type rule struct {
	// name は規則の識別名（デバッグ / テスト可読性のため）。
	name string
	// applicable は当該規則が適用される領域が入力に存在するとき true。
	applicable func(in PolicyInput) bool
	// check は不正項目を result へ追加する（applicable が true のときのみ呼ばれる）。
	check func(in PolicyInput, result *ValidationResult)
}

// rules は全領域の検証規則テーブル。新たな検証規則の追加は本スライスへの 1 エントリ追加で済む。
var rules = []rule{
	{
		name:       "app.count_limit",
		applicable: func(in PolicyInput) bool { return in.App != nil },
		check:      checkAppCount,
	},
	{
		name:       "password.minimum_length_range",
		applicable: func(in PolicyInput) bool { return in.Password != nil },
		check:      checkPasswordLength,
	},
	{
		name:       "security.required_and_enum",
		applicable: func(in PolicyInput) bool { return in.Security != nil },
		check:      checkSecurity,
	},
	{
		name:       "system_update.required_and_range",
		applicable: func(in PolicyInput) bool { return in.SystemUpdate != nil },
		check:      checkSystemUpdate,
	},
	{
		name:       "kiosk.package_name_format",
		applicable: func(in PolicyInput) bool { return in.Kiosk != nil },
		check:      checkKiosk,
	},
}

// Validate は 5 領域の設定値を検証し、検出したすべての不正項目を 1 つの ValidationResult に
// 収集して返す（Req 6.2 / 6.5）。外部副作用を持たず、同一入力に常に同一結果を返す（NFR 1）。
// nil の領域は検証対象から除外する（部分更新を許容）。すべて妥当なら Errors が空の結果を返す。
func Validate(in PolicyInput) ValidationResult {
	var result ValidationResult
	for _, r := range rules {
		if r.applicable(in) {
			r.check(in, &result)
		}
	}
	return result
}

// checkAppCount はアプリ件数が許容範囲（下限 0・上限 MaxAppCount）に収まるか検証する（Req 1）。
// 上限超過はビジネスルール違反（KindBusinessRule / Req 6.3）、負の件数（下限未満）は
// 件数として成立しない不正入力（KindInvalidField / Req 6.4）として、両者を区別して識別する。
func checkAppCount(in PolicyInput, result *ValidationResult) {
	n := in.App.AppCount
	if n < MinAppCount {
		result.add(ValidationError{
			Domain:  DomainApp,
			Field:   "AppCount",
			Kind:    KindInvalidField,
			Message: "アプリ件数が負の値です（0 以上を指定してください）",
		})
		return
	}
	if n > MaxAppCount {
		result.add(ValidationError{
			Domain:  DomainApp,
			Field:   "AppCount",
			Kind:    KindBusinessRule,
			Message: "アプリ件数が上限（3000 件）を超えています",
		})
	}
}

// checkPasswordLength はパスワード最小桁数が許容範囲内か検証する（Req 2）。
// 範囲外は invalid field（KindInvalidField / Req 6.4）として識別する。
func checkPasswordLength(in PolicyInput, result *ValidationResult) {
	n := in.Password.MinimumLength
	if n < MinPasswordLength || n > MaxPasswordLength {
		result.add(ValidationError{
			Domain:  DomainPassword,
			Field:   "MinimumLength",
			Kind:    KindInvalidField,
			Message: "パスワード最小桁数が許容範囲（0〜16）外です",
		})
	}
}

// checkSecurity はセキュリティ領域の必須項目欠落と enum 許容値外を検証する（Req 3）。
func checkSecurity(in PolicyInput, result *ValidationResult) {
	s := in.Security
	// EncryptionPolicy: 必須項目（空文字は欠落）+ enum 許容値検証。
	if s.EncryptionPolicy == "" {
		result.add(ValidationError{
			Domain:  DomainSecurity,
			Field:   "EncryptionPolicy",
			Kind:    KindInvalidField,
			Message: "必須項目 EncryptionPolicy が欠落しています",
		})
	} else if _, ok := allowedEncryptionPolicies[s.EncryptionPolicy]; !ok {
		result.add(ValidationError{
			Domain:  DomainSecurity,
			Field:   "EncryptionPolicy",
			Kind:    KindInvalidField,
			Message: "EncryptionPolicy が許容値集合外です",
		})
	}
	// PasswordQuality: 任意項目。指定された場合のみ enum 許容値検証。
	if s.PasswordQuality != "" {
		if _, ok := allowedPasswordQualities[s.PasswordQuality]; !ok {
			result.add(ValidationError{
				Domain:  DomainSecurity,
				Field:   "PasswordQuality",
				Kind:    KindInvalidField,
				Message: "PasswordQuality が許容値集合外です",
			})
		}
	}
}

// checkSystemUpdate はシステム更新領域の必須項目欠落・enum 許容値外・範囲外を検証する（Req 4）。
func checkSystemUpdate(in PolicyInput, result *ValidationResult) {
	u := in.SystemUpdate
	// Type: 必須項目（空文字は欠落）+ enum 許容値検証。
	if u.Type == "" {
		result.add(ValidationError{
			Domain:  DomainSystemUpdate,
			Field:   "Type",
			Kind:    KindInvalidField,
			Message: "必須項目 Type が欠落しています",
		})
		return
	}
	if _, ok := allowedSystemUpdateTypes[u.Type]; !ok {
		result.add(ValidationError{
			Domain:  DomainSystemUpdate,
			Field:   "Type",
			Kind:    KindInvalidField,
			Message: "Type が許容値集合外です",
		})
		return
	}
	// WINDOWED のときのみ保守ウィンドウの分範囲（0〜1439）を検証する。
	if u.Type == "WINDOWED" {
		if u.StartMinutes < minWindowMinute || u.StartMinutes > maxWindowMinute {
			result.add(ValidationError{
				Domain:  DomainSystemUpdate,
				Field:   "StartMinutes",
				Kind:    KindInvalidField,
				Message: "StartMinutes が範囲（0〜1439）外です",
			})
		}
		if u.EndMinutes < minWindowMinute || u.EndMinutes > maxWindowMinute {
			result.add(ValidationError{
				Domain:  DomainSystemUpdate,
				Field:   "EndMinutes",
				Kind:    KindInvalidField,
				Message: "EndMinutes が範囲（0〜1439）外です",
			})
		}
	}
}

// checkKiosk は Kiosk 指定アプリのパッケージ名がすべて妥当な形式か検証する（Req 5）。
// 不正な要素は Field に配列添字を含めて報告し、どの要素が不正かを機械可読に識別できる
// ようにする（Req 5.2 / 6.1）。
func checkKiosk(in PolicyInput, result *ValidationResult) {
	for i, name := range in.Kiosk.PackageNames {
		if !isValidPackageName(name) {
			result.add(ValidationError{
				Domain:  DomainKiosk,
				Field:   kioskField(i),
				Kind:    KindInvalidField,
				Message: kioskMessage(name),
			})
		}
	}
}

// kioskField は不正な Kiosk パッケージ名のフィールド名を配列添字付きで返す（Req 5.2 / 6.1）。
// 例: PackageNames[2]。呼び出し側が PackageNames 配列のどの要素が不正かを機械可読に特定できる。
func kioskField(index int) string {
	return fmt.Sprintf("PackageNames[%d]", index)
}

// kioskMessage は Kiosk パッケージ名不正の説明を組み立てる（空文字と形式不正を区別表記）。
func kioskMessage(name string) string {
	if name == "" {
		return "Kiosk パッケージ名が空文字列です"
	}
	return "Kiosk パッケージ名の形式が不正です: " + name
}

// isValidPackageName は Android パッケージ名形式を判定する（Req 5.3）。
// ドット区切りで 2 セグメント以上を持ち、各セグメントが英字始まりで英数字・アンダースコアの
// みからなる文字列のみを妥当とみなす。空文字列は不正（Req 5.4）。
func isValidPackageName(name string) bool {
	if name == "" {
		return false
	}
	segments := splitDot(name)
	if len(segments) < 2 {
		return false
	}
	for _, seg := range segments {
		if !packageNameSegment.MatchString(seg) {
			return false
		}
	}
	return true
}

// splitDot は '.' でパッケージ名を分割する。先頭・末尾・連続ドットは空セグメントを生み、
// packageNameSegment（英字始まり必須）に弾かれるため、別途の前処理は不要。
func splitDot(name string) []string {
	var out []string
	start := 0
	for i := 0; i < len(name); i++ {
		if name[i] == '.' {
			out = append(out, name[start:i])
			start = i + 1
		}
	}
	out = append(out, name[start:])
	return out
}
