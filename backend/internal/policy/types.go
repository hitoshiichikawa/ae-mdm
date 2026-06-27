package policy

import "github.com/hitoshiichikawa/ae-mdm/internal/errors"

// Domain は検証対象の 5 領域を表す機械可読な識別子（Req 6.1）。
// 検証エラーがどの領域に属するかを呼び出し側が判別するために用いる。
type Domain string

const (
	// DomainApp はアプリ領域（件数上限）。
	DomainApp Domain = "app"
	// DomainPassword はパスワード領域（最小桁数の数値範囲）。
	DomainPassword Domain = "password"
	// DomainSecurity はセキュリティ領域（必須項目 / enum 値）。
	DomainSecurity Domain = "security"
	// DomainSystemUpdate はシステム更新領域（必須項目 / 範囲・形式）。
	DomainSystemUpdate Domain = "system_update"
	// DomainKiosk は Kiosk 領域（パッケージ名形式）。
	DomainKiosk Domain = "kiosk"
)

// ErrorKind は検証エラーの種別（Req 6.3 / 6.4）。上限超過（ビジネスルール違反）と
// 項目フォーマット不正（invalid field）を機械可読に区別する。errors package の Code に
// 対応づけることで、呼び出し側が HTTP ステータス（422 / 400）へマッピングできる。
type ErrorKind string

const (
	// KindBusinessRule はビジネスルール違反（アプリ件数の上限超過）。HTTP 422 相当。
	KindBusinessRule ErrorKind = "business_rule"
	// KindInvalidField は不正入力（必須項目欠落 / 範囲外 / 形式不正）。HTTP 400 相当。
	KindInvalidField ErrorKind = "invalid_field"
)

// Code は ErrorKind を errors package の Code（HTTP ステータスのマッピング元）へ変換する。
// KindBusinessRule → CodeBusinessRule(422)、それ以外 → CodeInvalidRequest(400)。
// 呼び出し側（Policy Service）は本 Code を errors.New 等に渡して HTTP レスポンスを組み立てる。
func (k ErrorKind) Code() errors.Code {
	if k == KindBusinessRule {
		return errors.CodeBusinessRule
	}
	return errors.CodeInvalidRequest
}

// ValidationError は 1 件の不正項目を表す機械可読な検証エラー（Req 6.1 / 6.3 / 6.4）。
// 領域 / 対象フィールド / 種別を保持し、呼び出し側が管理者へ詳細提示できるようにする。
type ValidationError struct {
	// Domain は不正が検出された領域（アプリ / パスワード / セキュリティ / システム更新 / Kiosk）。
	Domain Domain
	// Field は不正な対象フィールド名（ドメイン入力型のフィールドに対応）。
	Field string
	// Kind は種別（business rule 違反 / invalid field）。
	Kind ErrorKind
	// Message は人間可読な説明（管理者向け詳細）。機密値は含めない。
	Message string
}

// ValidationResult は 1 回の検証で収集したすべての不正項目を保持する（Req 6.2 / 6.5）。
// すべて妥当な場合 Errors は空スライス（len 0）となり、IsValid が true を返す。
type ValidationResult struct {
	// Errors は検出されたすべての ValidationError（複数領域・複数項目を 1 結果に収集）。
	Errors []ValidationError
}

// IsValid は検証エラーが 1 件も無いとき true を返す（Req 6.5）。
func (r ValidationResult) IsValid() bool {
	return len(r.Errors) == 0
}

// add は検証エラーを 1 件追加する内部 helper。rule 群が検出した不正項目を収集する。
func (r *ValidationResult) add(e ValidationError) {
	r.Errors = append(r.Errors, e)
}

// AppPolicy はアプリ領域の入力（Req 1）。AMAPI Policy の applications 配列に相当する
// ドメイン入力で、本 Validator は件数のみを検証する（個々のアプリ設定は対象外）。
type AppPolicy struct {
	// AppCount は割り当てるアプリ件数。1 ポリシーあたり MaxAppCount（3,000）以下が妥当。
	AppCount int
}

// PasswordPolicy はパスワード領域の入力（Req 2）。AMAPI の passwordMinimumLength に相当する
// 最小桁数を保持する。
type PasswordPolicy struct {
	// MinimumLength はパスワード最小桁数。許容範囲は
	// [MinPasswordLength, MaxPasswordLength]（AMAPI passwordMinimumLength の 0〜16。
	// 0 は AMAPI 仕様上「制限なし」を表す有効値）。
	MinimumLength int
}

// SecurityPolicy はセキュリティ領域の入力（Req 3）。AMAPI Policy の
// encryptionPolicy / passwordPolicies[].passwordQuality を代表フィールドとして検証する。
//
// EncryptionPolicy は必須かつ enum（許容値外を拒否）、PasswordQuality は enum（許容値外を拒否）。
type SecurityPolicy struct {
	// EncryptionPolicy は暗号化ポリシー（必須項目 + enum）。AMAPI encryptionPolicy 相当。
	// 空文字は必須項目欠落として拒否、許容値集合外は invalid field として拒否する。
	EncryptionPolicy string
	// PasswordQuality はパスワード品質（enum）。AMAPI passwordQuality 相当。
	// 許容値集合外は invalid field として拒否する。空文字は未指定として受理する
	// （必須ではなく、指定された場合のみ enum 検証する）。
	PasswordQuality string
}

// SystemUpdatePolicy はシステム更新領域の入力（Req 4）。AMAPI Policy の systemUpdate に相当。
//
// Type は必須かつ enum。Type == "WINDOWED" のとき StartMinutes / EndMinutes は
// 0〜1439（1 日の分数範囲）に収まることを要する。
type SystemUpdatePolicy struct {
	// Type はシステム更新タイプ（必須 + enum）。AMAPI systemUpdate.type 相当。
	// 空文字は必須項目欠落として拒否、許容値集合外は invalid field として拒否する。
	Type string
	// StartMinutes は WINDOWED 時の保守ウィンドウ開始（0〜1439 分）。
	StartMinutes int
	// EndMinutes は WINDOWED 時の保守ウィンドウ終了（0〜1439 分）。
	EndMinutes int
}

// KioskPolicy は Kiosk 領域の入力（Req 5）。Kiosk として指定するアプリのパッケージ名集合。
type KioskPolicy struct {
	// PackageNames は Kiosk 指定アプリのパッケージ名集合。各要素が妥当な Android パッケージ名
	// 形式（ドット区切り 2 セグメント以上 / 各セグメント英字始まり + 英数字・アンダースコア）で
	// あることを要する。空文字列要素は形式不正として拒否する。
	PackageNames []string
}

// PolicyInput は 5 領域の設定値を束ねた Validator のドメイン入力型。
// 各領域はポインタとし、nil（未指定）の領域は検証対象から除外する（部分更新を許容）。
type PolicyInput struct {
	// App はアプリ領域。nil の場合アプリ件数検証を skip する。
	App *AppPolicy
	// Password はパスワード領域。nil の場合パスワード桁数検証を skip する。
	Password *PasswordPolicy
	// Security はセキュリティ領域。nil の場合セキュリティ検証を skip する。
	Security *SecurityPolicy
	// SystemUpdate はシステム更新領域。nil の場合システム更新検証を skip する。
	SystemUpdate *SystemUpdatePolicy
	// Kiosk は Kiosk 領域。nil の場合 Kiosk 検証を skip する。
	Kiosk *KioskPolicy
}
