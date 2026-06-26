// Package amapi は Android Management API（AMAPI）への薄い共有ラッパを提供する
// プラットフォーム層パッケージである。
//
// 本パッケージは Issue #34（A4a: AMAPI Client 共有ラッパ）の実装で、umbrella #24 の
// 設計（design.md「AMAPI Client」節）に準拠する。各ドメインサービス（Tenant / Policy /
// Device / Command / Enrollment / App / Notification）は、AMAPI への REST / SDK 呼び出しを
// 直接行わず、本ラッパが提供する `Client` interface を経由する。
//
// 主責務:
//   - 自社 GCP プロジェクトのサービスアカウント資格情報による AMAPI 認証（EMM-bound 方式）
//   - 4xx を呼び出し側起因のドメインエラー、5xx / 429 / ネットワーク失敗を再試行可能エラーに
//     正規化（internal/errors の Code 体系へのマッピング）
//   - 429 / 5xx に対する exponential backoff 再試行（最大 3 回 = 4 試行）
//   - 全操作で enterprise 識別子（`enterpriseName`）を必須化（テナント分離の物理担保）
//   - AMAPI のレスポンスをそのまま透過せず、本ラッパ独自型へ正規化して返す
//
// テストでは `StubClient`（同パッケージ内）を差し替えることで実 AMAPI 呼び出しを発生させず
// 各ドメインの単体テストが可能になる（Req 6.1 / 6.2 / 6.3）。
package amapi

// Enterprise は AMAPI の Enterprise リソースを本ラッパ独自型で表現する。
//
// 設計（umbrella #24 design.md「AMAPI Client / Service Interface」抜粋）に従い、AMAPI SDK の
// `*androidmanagement.Enterprise` をそのまま漏らさず、MVP で必要な最小フィールドのみを公開する。
type Enterprise struct {
	// Name は AMAPI が払い出す Enterprise 一意名（"enterprises/{enterpriseId}" 形式）。
	Name string
	// DisplayName は管理者向けに表示する Enterprise 名（AMAPI の enterpriseDisplayName）。
	DisplayName string
}

// PolicyBody は AMAPI の Policy リソースを本ラッパ独自型で表現する。
//
// MVP では「name」と「version」のみを正規化し、policy 本体は raw JSON フィールド集合として
// 保持する（umbrella #24 では policy schema 詳細は domain Service の責務）。
type PolicyBody struct {
	// Name は AMAPI が払い出す Policy 一意名
	// （"enterprises/{enterpriseId}/policies/{policyId}" 形式）。
	Name string
	// Version は AMAPI が払い出す Policy のバージョン番号。upsert 後の取得時に participant
	// adoption の同期に利用される。
	Version int64
	// Raw は AMAPI の Policy ボディそのものを保持する（任意 key-value）。MVP では domain
	// Service 側が raw JSON として組み立てるため、本ラッパは pass-through で扱う。
	Raw map[string]any
}

// Device は AMAPI の Device リソースを本ラッパ独自型で表現する。
type Device struct {
	// Name は AMAPI が払い出す Device 一意名
	// （"enterprises/{enterpriseId}/devices/{deviceId}" 形式）。
	Name string
	// State は AMAPI の Device state（"ACTIVE" / "DISABLED" / "PROVISIONING" 等）。
	State string
	// PolicyName は当該 Device に適用中の Policy 名。
	PolicyName string
	// LastStatusReportTime は最終 status report 受信時刻（AMAPI 払い出しの RFC3339 文字列を
	// そのまま保持。domain Service 側で time.Time に解析する）。
	LastStatusReportTime string
}

// CommandRequest は AMAPI の Command issue リクエストを本ラッパ独自型で表現する。
//
// MVP では LOCK / RESET_PASSWORD / REBOOT 等の type と任意 NewPassword を保持する程度に留め、
// domain Service 側で必要なフィールドを集約する。
type CommandRequest struct {
	// Type は AMAPI の Command type 文字列（"LOCK" / "RESET_PASSWORD" / "REBOOT" 等）。
	Type string
	// Duration は Command の有効期限（"600s" 等の AMAPI 互換 Duration 文字列。空文字なら未指定）。
	Duration string
	// NewPassword は RESET_PASSWORD 時の新パスワード（その他の Type 時は空文字）。
	NewPassword string
}

// EnrollmentTokenRequest は AMAPI の EnrollmentToken 作成リクエストを本ラッパ独自型で表現する。
type EnrollmentTokenRequest struct {
	// PolicyName は enrollment 後に Device へ適用する Policy 一意名（"policies/{policyId}" の
	// 末尾部分のみでも、AMAPI が enterprise 配下で解決する）。
	PolicyName string
	// Duration は Token の有効期限（"3600s" 等の AMAPI 互換 Duration 文字列。空文字なら 1 時間）。
	Duration string
	// AdditionalData は AMAPI の additionalData フィールド（1024 文字以内、任意の文字列）。
	AdditionalData string
	// AllowPersonalUsage は AMAPI の allowPersonalUsage 列挙
	// （"PERSONAL_USAGE_ALLOWED" / "PERSONAL_USAGE_DISALLOWED" 等。空文字なら未指定）。
	AllowPersonalUsage string
}

// EnrollmentToken は AMAPI が払い出した EnrollmentToken を本ラッパ独自型で表現する。
type EnrollmentToken struct {
	// Name は AMAPI が払い出す EnrollmentToken の一意名
	// （"enterprises/{enterpriseId}/enrollmentTokens/{tokenId}" 形式）。
	Name string
	// Value は enrollment QR 等で利用する token 値。秘密値であり、構造化ログには出さない。
	Value string
	// QRCode は AMAPI が同梱する QR コード payload 文字列（任意）。
	QRCode string
	// ExpirationTime は token の expiration（AMAPI 払い出しの RFC3339 文字列）。
	ExpirationTime string
}

// WebToken は AMAPI が払い出した WebToken を本ラッパ独自型で表現する。
//
// admin / tenant コンソールの embedded iframe（Managed Play / Web Apps 等）に渡す short-lived
// token。Value は秘密値であり、構造化ログには出さない（NFR 1.2）。
type WebToken struct {
	// Name は AMAPI が払い出す WebToken の一意名
	// （"enterprises/{enterpriseId}/webTokens/{webTokenId}" 形式）。
	Name string
	// Value は iframe 埋め込み時に渡す token 値。秘密値。
	Value string
}
