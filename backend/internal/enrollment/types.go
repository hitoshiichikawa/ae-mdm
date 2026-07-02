package enrollment

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/hitoshiichikawa/ae-mdm/internal/audit"
	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// 本ファイルは enrollment domain の DTO・enum・値オブジェクト・sentinel error・依存倒置ポートを
// 定義する（design.md「Components and Interfaces」/ tasks.md task 1）。DB / AMAPI / audit の実
// オーケストレーションは service.go / repository.go（後続 task）が担い、本ファイルは純粋な型と
// 決定論的 helper（AdditionalData.Marshal / DeriveStatus）のみを持つ。

// Mode はエンロールメントトークンのモード（enrollment_tokens.mode enum と一致 / migration 0004）。
//
// FULLY_MANAGED / DEDICATED の 2 値のみが有効。いずれも個人利用不可（PERSONAL_USAGE_DISALLOWED）で
// 発行する（Req 1.1 / 1.2）。DB 上の enum ラベルと同一文字列（fully_managed / dedicated）を用いる。
type Mode string

const (
	// ModeFullyManaged は端末全体を管理する完全管理モード（個人利用不可）。
	ModeFullyManaged Mode = "fully_managed"
	// ModeDedicated は単一/限定用途に固定する専用端末モード（個人利用不可・Kiosk ポリシー紐付け）。
	ModeDedicated Mode = "dedicated"
)

// Valid は Mode が有効な列挙値（fully_managed / dedicated）か判定する。
// 未指定（空文字）や未知の値は false（Req 1.4 の判定素材 / 不正モードハンドリングは Service が所有）。
func (m Mode) Valid() bool {
	return m == ModeFullyManaged || m == ModeDedicated
}

// ParseMode は入力文字列を Mode へ変換する。未指定/未知の値は ErrInvalidMode を返す（Req 1.4）。
//
// 決定論的な純粋関数であり、外部副作用を持たない。Service（後続 task）が発行要求の検証ゲートで用いる。
func ParseMode(s string) (Mode, error) {
	m := Mode(s)
	if !m.Valid() {
		return "", ErrInvalidMode
	}
	return m, nil
}

// トークン状態（TokenSummary.Status）。expires_at からの派生値（Req 4.1）。
const (
	// StatusActive は有効期限内のトークン。
	StatusActive = "active"
	// StatusExpired は有効期限切れのトークン。
	StatusExpired = "expired"
)

// DeriveStatus は expires_at と現在時刻 now からトークンの状態（active / expired）を派生する（Req 4.1）。
//
// clock を隠し持たず now を引数で受ける決定論的な純粋関数とする（単体テストで境界を固定するため）。
// 判定境界: now が expires_at より前（now < expires_at）なら active、それ以外（now >= expires_at）は
// expired。すなわち now == expires_at の瞬間は expired とみなす（有効期限「まで」有効）。
func DeriveStatus(expiresAt, now time.Time) string {
	if now.Before(expiresAt) {
		return StatusActive
	}
	return StatusExpired
}

// AdditionalData はトークンに付与する内部メタデータの値オブジェクト（Req 1.3）。
//
// 発行時に AMAPI の additionalData フィールドへ渡すと同時に、enrollment_tokens.additional_data へ
// snapshot 保存する。ENROLLMENT 通知の enrollmentTokenData として回送され、通知処理側が発行元
// テナントの突合に用いる（design Data Models / notification.EnrollmentNotificationHandler）。
// 秘密値（Value / QRCode）は一切含めない（NFR 3.1）。
type AdditionalData struct {
	// TenantID は発行テナントの id（通知処理時の突合キー / Req 1.3 / 3.1）。
	TenantID uuid.UUID
	// IssuedBy は発行管理者の admin_users.id（Req 1.3）。
	IssuedBy uuid.UUID
	// Mode は発行モード。ENROLLMENT 登録時の devices.mode 決定に用いる内部 metadata。
	Mode Mode
}

// additionalDataJSON は AdditionalData の JSON wire 表現（snake_case キー固定）。
//
// AMAPI additionalData / enrollment_tokens.additional_data の双方で同一 wire-format を共有するため、
// 明示的な JSON tag を持つ内部表現に写像してから marshal する。
type additionalDataJSON struct {
	TenantID string `json:"tenant_id"`
	IssuedBy string `json:"issued_by"`
	Mode     string `json:"mode"`
}

// Marshal は AdditionalData を {"tenant_id","issued_by","mode"} の JSON 文字列へ直列化する（Req 1.3）。
//
// UUID は文字列表現（uuid.Nil は "00000000-0000-0000-0000-000000000000"）で埋め込む。決定論的な
// 純粋関数であり、同一入力に対して常に同一の JSON を返す。
func (a AdditionalData) Marshal() (string, error) {
	b, err := json.Marshal(additionalDataJSON{
		TenantID: a.TenantID.String(),
		IssuedBy: a.IssuedBy.String(),
		Mode:     string(a.Mode),
	})
	if err != nil {
		// encoding/json は本 struct（全 string field）で error を返さないが、契約上 error を伝播する。
		return "", pkgerrors.Wrap(pkgerrors.CodeInternal, "enrollment additionalData marshal failed", err)
	}
	return string(b), nil
}

// IssueRequest はトークン発行要求の入力 DTO（POST /api/enrollment-tokens / Req 1.x）。
//
// Handler（後続 task）が JSON デコードし Service.IssueToken へ渡す。PolicyID は DEDICATED でのみ
// 必須（FULLY_MANAGED では nil / Req 1.5）。Duration は AMAPI 互換 Duration 文字列（空なら AMAPI 既定）。
type IssueRequest struct {
	// Mode は発行モード（fully_managed / dedicated）。未指定/不正は ErrInvalidMode（Req 1.4）。
	Mode Mode `json:"mode"`
	// PolicyID は DEDICATED で紐づける自テナントの Kiosk ポリシー id。FULLY_MANAGED では nil（Req 1.5）。
	PolicyID *uuid.UUID `json:"policy_id,omitempty"`
	// Duration はトークン有効期限の AMAPI 互換 Duration 文字列（"3600s" 等。空文字は未指定）。
	Duration string `json:"duration,omitempty"`
}

// TokenView は発行成功時に HTTP 応答で一度だけ返すトークン表現（Req 1.1 / 1.2 / NFR 3.1）。
//
// 秘密値（Value / QRCodeData）を含む唯一の返却経路であり、snapshot・監査・ログには載せない
// （NFR 3.1 / Req 5.2）。発行後の再取得は不可（QR は発行時一度きり表示）。
type TokenView struct {
	// ID はトークンの primary key。
	ID uuid.UUID `json:"id"`
	// Mode は発行モード。
	Mode Mode `json:"mode"`
	// ExpiresAt はトークン有効期限。
	ExpiresAt time.Time `json:"expires_at"`
	// Value は AMAPI が払い出したトークン秘密値（QR / 手動入力用）。永続化・監査・ログ非対象（NFR 3.1）。
	Value string `json:"value"`
	// QRCodeData は QR 表示用の秘密値 payload。永続化・監査・ログ非対象（NFR 3.1）。
	QRCodeData string `json:"qr_code_data"`
}

// TokenSummary はトークン一覧（GET /api/enrollment-tokens）の 1 件分の軽量表現（Req 4.1）。
//
// 秘密値（Value / QRCodeData）は含まず、Status は expires_at から派生（active / expired / Req 4.1）。
type TokenSummary struct {
	// ID はトークンの primary key。
	ID uuid.UUID `json:"id"`
	// Mode は発行モード。
	Mode Mode `json:"mode"`
	// PolicyID は DEDICATED で紐づけた Kiosk ポリシー id（FULLY_MANAGED では nil）。
	PolicyID *uuid.UUID `json:"policy_id,omitempty"`
	// ExpiresAt はトークン有効期限。
	ExpiresAt time.Time `json:"expires_at"`
	// Status は expires_at 由来の状態（active / expired / Req 4.1）。
	Status string `json:"status"`
}

// TokenRow は enrollment_tokens テーブルの 1 行に対応する DB 層のドメイン型（NFR 3.1）。
//
// 列構成は migration 0004（id, tenant_id, amapi_token_name, mode, policy_id, additional_data jsonb,
// expires_at, issued_by, created_at）に一致させる。PolicyID は nullable（FULLY_MANAGED で NULL）の
// ため *uuid.UUID とする。**秘密値（Value / QRCode 相当）の列・field は一切持たない**（NFR 3.1）。
type TokenRow struct {
	// ID はトークンの primary key。
	ID uuid.UUID
	// TenantID は所有テナントの id。RLS のテナント分離キー（NFR 2.1 相当 / tenant-scoped）。
	TenantID uuid.UUID
	// AMAPITokenName は AMAPI が払い出した EnrollmentToken.Name（秘密値ではない一意名）。
	AMAPITokenName string
	// Mode は発行モード（enrollment_tokens.mode enum）。
	Mode Mode
	// PolicyID は DEDICATED で紐づけた Kiosk ポリシー id。FULLY_MANAGED では nil（NULL）。
	PolicyID *uuid.UUID
	// AdditionalData は additional_data jsonb の JSON 文字列（{"tenant_id","issued_by","mode"}）。
	AdditionalData string
	// ExpiresAt はトークン有効期限。
	ExpiresAt time.Time
	// IssuedBy は発行管理者の admin_users.id。
	IssuedBy uuid.UUID
	// CreatedAt はレコード生成時刻（DB DEFAULT now()）。
	CreatedAt time.Time
}

// 以下の sentinel error は Service / Repository（後続 task）が errors.Is 比較や wrap 元として用いる
// package-level な read-only 変数。design.md「Error Handling」の Code 写像に従い対応する HTTP
// ステータスへ写像される。利用側は read-only に扱い、必要なら errors.Wrap で cause を付与する。
var (
	// ErrInvalidMode は未指定/未知のモード指定。HTTP 400（Req 1.4）。
	ErrInvalidMode = pkgerrors.New(pkgerrors.CodeInvalidRequest, "invalid enrollment mode")
	// ErrPolicyRequired は DEDICATED でありながら Kiosk ポリシーが未指定。HTTP 400（Req 1.5）。
	ErrPolicyRequired = pkgerrors.New(pkgerrors.CodeInvalidRequest, "kiosk policy is required for dedicated mode")
	// ErrTokenPersist はトークン snapshot の永続化失敗。HTTP 503（Req 1.6 の AMAPI-first 乖離ログ素材）。
	ErrTokenPersist = pkgerrors.New(pkgerrors.CodeUnavailable, "enrollment token persistence failed")
)

// 依存倒置ポート（consumer-defines-interface / primitive 型で cross-domain import を回避する）。
// enrollment domain は tenant / policy / audit の各 struct を直接 import せず、以下の最小 port を
// Service（後続 task）が受け取り、cmd/api が実 Service を配線する（policy.Service を手本）。

// enterpriseResolver は tenantID から AMAPI enterprise_name を解決する最小ポート（Req 2.3 前提）。
//
// tenant.Service.EnterpriseNameForTenant が満たす。bound（自テナント）のみ enterprise_name を返し、
// 越境は fail-closed の NotFound（越境発行拒否・存在非露出 / Req 2.3）。
type enterpriseResolver interface {
	EnterpriseNameForTenant(ctx context.Context, id uuid.UUID) (string, error)
}

// policyChecker は DEDICATED で紐づける自テナント Kiosk ポリシーを検証する最小ポート（Req 1.5）。
//
// cmd/api のアダプタが policy.Service を包んで満たす。自テナントに policyID が存在すれば AMAPI
// policy id（= DB uuid 文字列）を返し、不在/越境は存在差を露出しない NotFound を返す（Req 1.5 / 2.3）。
type policyChecker interface {
	ResolveOwnedPolicy(ctx context.Context, tenantID, policyID uuid.UUID) (amapiPolicyID string, err error)
}

// eventRecorder はトークン発行の監査イベントを Audit Service へ渡す最小ポート（Req 5.1）。
//
// audit.Service が満たす。投機的に全 audit.Service IF を要求せず、本 domain が使う Record のみに
// 限定する（policy.eventRecorder を手本）。Detail に秘密値を含めないのは呼び出し側責務（Req 5.2 / NFR 3.1）。
type eventRecorder interface {
	Record(ctx context.Context, ev audit.Event) error
}
