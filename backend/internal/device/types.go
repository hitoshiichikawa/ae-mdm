package device

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// 本ファイルは device ドメインの application 層（Service / Repository / StatusApplier / Handler）が
// 共有する enum・DTO・DB 層 scan 型・sentinel error を定義する。型名・フィールド名は design.md
// 「Data Models > DTO 形状」と Components の各 interface シグネチャを canonical とし、勝手に
// 増減しない。enum の文字列値は migration 0006 の DB ENUM（compliance_status / device_mode）と
// 厳密一致させる（Req 3.1 / 3.4）。

// ComplianceStatus は端末のコンプライアンス分類（Req 3.1 / 3.4）。
//
// 値は migration 0006 の compliance_status ENUM（'compliant' / 'non_compliant' / 'unknown' /
// 'unsupported'）と厳密一致する。DB 由来の値をそのまま read 経路で返却するため（Service は
// 再判定せず stored 値を返す / design.md「device.Service」節）、両者の値集合が乖離すると
// scan / 返却で不整合を生む。
type ComplianceStatus string

const (
	// ComplianceStatusCompliant は準拠。
	ComplianceStatusCompliant ComplianceStatus = "compliant"
	// ComplianceStatusNonCompliant は非準拠（非準拠理由を併記 / Req 3.2）。
	ComplianceStatusNonCompliant ComplianceStatus = "non_compliant"
	// ComplianceStatusUnknown は未確認（状態通知が未観測。devices.compliance_status の
	// DB DEFAULT / Req 3.3）。
	ComplianceStatusUnknown ComplianceStatus = "unknown"
	// ComplianceStatusUnsupported はサポート対象外（第 4 分類 / Req 3.4）。本 Issue では
	// read のみで、StatusApplier はこの値を書き込まない（書込み契機は Open Questions）。
	ComplianceStatusUnsupported ComplianceStatus = "unsupported"
)

// DeviceMode は端末の管理モード（Req 1.3）。
//
// 値は migration 0006 の device_mode ENUM（'fully_managed' / 'dedicated'）と厳密一致する。
type DeviceMode string

const (
	// DeviceModeFullyManaged は完全管理モード（Fully Managed）。
	DeviceModeFullyManaged DeviceMode = "fully_managed"
	// DeviceModeDedicated は専用端末モード（Dedicated / Kiosk 相当）。
	DeviceModeDedicated DeviceMode = "dedicated"
)

// ListFilter は端末一覧（GET /api/devices）の絞り込み・ページング条件（Req 1.1〜1.6）。
//
// pointer フィールドは nil のとき「無条件（絞り込まない）」を意味する。Service が
// Clock.Now() と閾値から syncCutoff を算出し、SyncDelayed フィルタの評価に用いる。
type ListFilter struct {
	// Compliance はコンプライアンス分類の絞り込み。nil=無条件（Req 1.2）。
	Compliance *ComplianceStatus
	// Mode は管理モードの絞り込み。nil=無条件（Req 1.3）。
	Mode *DeviceMode
	// SyncDelayed は同期遅延の絞り込み。nil=無条件 / true=遅延のみ / false=非遅延のみ（Req 1.4）。
	SyncDelayed *bool
	// Page は 1-based のページ番号。既定 1（Handler が未指定時に補完 / Req 1.5）。
	Page int
	// PageSize は 1 ページの取得件数。既定 50 / 上限 200（Handler が clamp / Req 1.5）。
	PageSize int
}

// DeviceSummary は端末一覧の 1 行（Req 1.x）。API 応答であり snake_case の JSON tag を付与する
// （policy.PolicyView / PolicySummary 慣習）。
type DeviceSummary struct {
	// ID は端末の primary key。
	ID uuid.UUID `json:"id"`
	// AMAPIDeviceName は AMAPI Device name（devices.amapi_device_name / テナント内一意）。
	AMAPIDeviceName string `json:"amapi_device_name"`
	// Mode は管理モード。
	Mode DeviceMode `json:"mode"`
	// ComplianceStatus はコンプライアンス分類（4 分類 / Req 3.1）。
	ComplianceStatus ComplianceStatus `json:"compliance_status"`
	// LastStatusAt は最終同期時刻。一度も STATUS_REPORT 未受信なら nil（null 出力）。
	LastStatusAt *time.Time `json:"last_status_at"`
	// SyncDelayed は同期遅延フラグ（Service が syncCutoff との比較で算出 / Req 4.x）。
	SyncDelayed bool `json:"sync_delayed"`
}

// DeviceDetail は端末詳細（GET /api/devices/{id}）の応答（Req 2.x / 3.x / 4.3）。
//
// jsonb 由来の属性は空でも {} / [] を返す（Req 2.5）ため json.RawMessage で保持し、Service は
// 空値を握りつぶさずそのまま返す。API 応答であり snake_case の JSON tag を付与する。
type DeviceDetail struct {
	// ID は端末の primary key。
	ID uuid.UUID `json:"id"`
	// AMAPIDeviceName は AMAPI Device name。
	AMAPIDeviceName string `json:"amapi_device_name"`
	// Mode は管理モード。
	Mode DeviceMode `json:"mode"`
	// AppliedPolicyName は STATUS_REPORT が報告する適用中ポリシー名（空可 / Req 2.1）。
	// 割当 intent の applied_policy_id（#40）とは別概念の報告値。
	AppliedPolicyName string `json:"applied_policy_name"`
	// HardwareInfo はハードウェア情報 jsonb。空は {}（Req 2.5）。
	HardwareInfo json.RawMessage `json:"hardware_info"`
	// SoftwareInfo はソフトウェア情報 jsonb。空は {}（Req 2.5）。
	SoftwareInfo json.RawMessage `json:"software_info"`
	// ComplianceStatus はコンプライアンス分類（4 分類 / Req 3.1）。
	ComplianceStatus ComplianceStatus `json:"compliance_status"`
	// NonComplianceDetails は非準拠理由 jsonb。空は []（Req 3.2）。
	NonComplianceDetails json.RawMessage `json:"non_compliance_details"`
	// InstalledApps はインストール済み管理対象アプリ jsonb。空は []（Req 2.2 / 2.5）。
	InstalledApps json.RawMessage `json:"installed_apps"`
	// LastStatusAt は最終同期時刻。未受信なら nil（Req 2.3）。
	LastStatusAt *time.Time `json:"last_status_at"`
	// SyncDelayed は同期遅延フラグ（Req 2.3 / 4.x）。
	SyncDelayed bool `json:"sync_delayed"`
	// EnrolledAt はエンロール時刻（devices.enrolled_at）。
	EnrolledAt time.Time `json:"enrolled_at"`
}

// TenantOverview は全テナント横断 overview の 1 テナント分（Req 6.1）。API 応答であり
// snake_case の JSON tag を付与する。Breakdown は 4 分類を 0 埋めした内訳。
type TenantOverview struct {
	// TenantID は集計対象テナントの id。
	TenantID uuid.UUID `json:"tenant_id"`
	// DeviceCount は当該テナントの端末総数。
	DeviceCount int `json:"device_count"`
	// Breakdown はコンプライアンス 4 分類ごとの端末数（0 埋め）。
	Breakdown map[ComplianceStatus]int `json:"breakdown"`
}

// TenantComplianceCount は Repository.AggregateOverview の戻り型（DB 層）。
//
// SuperAdmin ctx 下の GROUP BY tenant_id, compliance_status の集計 1 行に対応する flat な
// 件数行であり、Service が tenant 単位に畳み込んで TenantOverview へ変換する（Req 6.1）。
// DB 層型のため JSON tag は付与しない。
type TenantComplianceCount struct {
	// TenantID は集計対象テナントの id。
	TenantID uuid.UUID
	// ComplianceStatus は当該グループのコンプライアンス分類。
	ComplianceStatus ComplianceStatus
	// Count は (tenant_id, compliance_status) グループの端末数。
	Count int
}

// DeviceRow は devices テーブル（migration 0006 + 0019）の 1 行に対応する DB 層 scan 型。
//
// Repository が列ごとに型付き scan する（jsonb → json.RawMessage、nullable → pointer /
// policy.PolicyRow 手本）。DB 層型のため JSON tag は付与せず、Service が DeviceSummary /
// DeviceDetail へ写像する。列対応: id / tenant_id / amapi_device_name / mode /
// applied_policy_id（nullable）/ applied_policy_name（nullable / 0019）/ hardware_info /
// software_info / compliance_status / non_compliance_details / installed_apps /
// last_status_at（nullable）/ enrolled_at。
type DeviceRow struct {
	// ID は端末の primary key（devices.id）。
	ID uuid.UUID
	// TenantID は所有テナントの id。RLS のテナント分離キー。
	TenantID uuid.UUID
	// AMAPIDeviceName は AMAPI Device name（devices.amapi_device_name）。
	AMAPIDeviceName string
	// Mode は管理モード（devices.mode）。
	Mode DeviceMode
	// AppliedPolicyID は割当 intent のポリシー id（nullable / 未割当は nil / #40）。
	AppliedPolicyID *uuid.UUID
	// AppliedPolicyName は STATUS_REPORT 報告値の適用中ポリシー名（nullable / migration 0019）。
	AppliedPolicyName *string
	// HardwareInfo はハードウェア情報 jsonb（devices.hardware_info / 空は {}）。
	HardwareInfo json.RawMessage
	// SoftwareInfo はソフトウェア情報 jsonb（devices.software_info / 空は {}）。
	SoftwareInfo json.RawMessage
	// ComplianceStatus はコンプライアンス分類（devices.compliance_status / DEFAULT 'unknown'）。
	ComplianceStatus ComplianceStatus
	// NonComplianceDetails は非準拠理由 jsonb（devices.non_compliance_details / 空は []）。
	NonComplianceDetails json.RawMessage
	// InstalledApps はインストール済みアプリ jsonb（devices.installed_apps / 空は []）。
	InstalledApps json.RawMessage
	// LastStatusAt は最終同期時刻（devices.last_status_at / nullable）。
	LastStatusAt *time.Time
	// EnrolledAt はエンロール時刻（devices.enrolled_at）。
	EnrolledAt time.Time
}

// StatusApplyInput は Repository.UpdateFromStatusReport の引数（Req 7.1 / 7.2）。
//
// COALESCE 部分更新のため任意フィールドを pointer で保持し、nil は「payload 欠落 → 既存値保持
// （COALESCE($n, col)）」を意味する（Req 7.2）。AMAPIDeviceName のみ一致キーとして必須。
// ComplianceStatus は StatusApplier が NonComplianceDetails から算出したときのみ非 nil にする。
type StatusApplyInput struct {
	// AMAPIDeviceName は UPDATE の一致キー（devices.amapi_device_name / 必須）。
	AMAPIDeviceName string
	// LastStatusReportTime は最終同期時刻。nil=payload 欠落（更新しない）。
	LastStatusReportTime *time.Time
	// AppliedPolicyName は適用中ポリシー名。nil=欠落。
	AppliedPolicyName *string
	// ComplianceStatus はコンプライアンス分類。nil=欠落（StatusApplier が算出した時のみ非 nil）。
	ComplianceStatus *ComplianceStatus
	// NonComplianceDetails は非準拠理由 jsonb。nil=欠落。
	NonComplianceDetails *json.RawMessage
	// HardwareInfo はハードウェア情報 jsonb。nil=欠落。
	HardwareInfo *json.RawMessage
	// SoftwareInfo はソフトウェア情報 jsonb。nil=欠落。
	SoftwareInfo *json.RawMessage
	// InstalledApps はインストール済みアプリ jsonb。nil=欠落。
	InstalledApps *json.RawMessage
}

// ErrDeviceNotFound は端末不在 / 他テナント越境（RLS で 0 行）を表す sentinel error（HTTP 404）。
//
// 不在と越境で同一の汎用 message を返すことで存在有無を秘匿する（Req 2.4 / 5.1 / 5.2）。
// Service / Repository が errors.Is 比較や wrap 元として用いる read-only 変数
// （policy.ErrPolicyNotFound と同型）。
var ErrDeviceNotFound = pkgerrors.New(pkgerrors.CodeNotFound, "device not found")
