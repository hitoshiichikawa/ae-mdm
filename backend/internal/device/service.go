package device

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Service は端末インベントリの読み取り系ユースケース（一覧 / 詳細 / 横断 overview）と、
// コンプライアンス分類の返却・同期遅延判定を担う（design.md「device.Service」節）。
//
// **write メソッドを持たない**ことで「HTTP 経由での端末属性の直接書込み不可」（Req 7.3）を
// 型レベルの不変条件として担保する（端末属性の write 経路は StatusApplier のみ / doc.go）。
// read 系のため監査記録は行わない（design.md「device.Service」節 / #40 と同方針）。
type Service interface {
	// List は自テナント端末をフィルタ + ページング付きで返す（Req 1.1〜1.5 / 1.7）。
	//
	// syncCutoff（Clock.Now() - 閾値）を算出し、Repository のフィルタ評価と per-row SyncDelayed
	// 算出の双方で共用する。0 件でも非 nil の空 slice を返す（Req 1.7）。
	List(ctx context.Context, tenantID uuid.UUID, f ListFilter) ([]DeviceSummary, error)

	// Get は自テナント端末 1 件の詳細を返す（Req 2.1〜2.5 / 3.1〜3.3）。
	//
	// DeviceRow を DeviceDetail へ写像し、stored compliance_status を 4 分類でそのまま返す
	// （再判定しない / 未観測は 'unknown' / Req 3.1・3.3）。不在 / 越境は Repository が写像済みの
	// ErrDeviceNotFound（404 / 存在差非露出）をそのまま伝達する（Req 2.4 / 5.1 / 5.2）。
	Get(ctx context.Context, tenantID, deviceID uuid.UUID) (DeviceDetail, error)

	// Overview は全テナント横断の集計を返す（Req 6.1 / 6.3 / 6.4）。
	//
	// Repository.AggregateOverview の flat な件数行を tenant 単位に畳み込み、4 分類を 0 埋めした
	// TenantOverview 列へ変換する。tenantFilter != nil のとき当該テナントのみ。全体 0 件は非 nil の
	// 空 slice（Req 6.4）。SuperAdmin ctx の確立は呼び出し側（AdminHandler）の責務。
	Overview(ctx context.Context, tenantFilter *uuid.UUID) ([]TenantOverview, error)
}

// jsonb 空属性の default 値（Req 2.5）。object 属性（hardware/software）は {}、array 属性
// （non_compliance_details/installed_apps）は [] を補填し、null ではなく空構造として返す。
const (
	defaultJSONObject = "{}"
	defaultJSONArray  = "[]"
)

// service は Service の本番実装。
//
// deps は read に必要な最小依存に絞る（監査・logger は read 系では本質的に不要 / design.md）:
//   - repo:      device.Repository（tenant-scoped 参照 / SuperAdmin 集計）
//   - clock:     Clock（同期遅延判定の現在時刻 / fake clock でテストを決定的にする）
//   - threshold: 同期遅延判定の閾値（config.DeviceSyncDelayThresholdHours から換算 / Req 4.2）
type service struct {
	repo      Repository
	clock     Clock
	threshold time.Duration
}

// NewService は read 系 Service を構築する。
//
// syncDelayThresholdHours は同期遅延判定の閾値（時間単位 / 既定 24）。config 型全体ではなく閾値の
// int を注入することで Service を config から独立させ、fake clock + 任意閾値で単体テストしやすく
// する（Req 4.2）。負値は運用上想定しない（呼び出し側 config が既定 24 を補完済み）が、そのまま
// time.Duration へ換算する。
func NewService(repo Repository, clock Clock, syncDelayThresholdHours int) Service {
	return &service{
		repo:      repo,
		clock:     clock,
		threshold: time.Duration(syncDelayThresholdHours) * time.Hour,
	}
}

// syncCutoff は同期遅延判定の基準時刻（現在時刻 - 閾値）を算出する。List のフィルタ評価と
// per-row SyncDelayed 算出で同一値を共用するため、1 リクエスト内で 1 回だけ算出して渡し回す。
func (s *service) syncCutoff() time.Time {
	return s.clock.Now().Add(-s.threshold)
}

// isSyncDelayed は最終同期時刻が cutoff より前（strict less-than）かを判定する（Req 4.1・4.3）。
//
// lastStatusAt == nil（一度も STATUS_REPORT 未受信）は遅延扱いしない（Req 4.1 が最終同期時刻の
// 存在を前提とするため / design.md 設計判断）。cutoff ちょうど（等値）は Before が false のため
// 非遅延（Req 4.3 の strict `<`）。
func isSyncDelayed(lastStatusAt *time.Time, cutoff time.Time) bool {
	return lastStatusAt != nil && lastStatusAt.Before(cutoff)
}

// List は Service.List の実装。
func (s *service) List(ctx context.Context, tenantID uuid.UUID, f ListFilter) ([]DeviceSummary, error) {
	cutoff := s.syncCutoff()
	rows, err := s.repo.ListByTenant(ctx, tenantID, f, cutoff)
	if err != nil {
		return nil, err
	}
	// 0 件でも非 nil の空 slice を返す（Req 1.7）。
	summaries := make([]DeviceSummary, 0, len(rows))
	for _, row := range rows {
		summaries = append(summaries, rowToSummary(row, cutoff))
	}
	return summaries, nil
}

// Get は Service.Get の実装。
func (s *service) Get(ctx context.Context, tenantID, deviceID uuid.UUID) (DeviceDetail, error) {
	row, err := s.repo.GetByID(ctx, tenantID, deviceID)
	if err != nil {
		// 不在 / 越境は Repository が ErrDeviceNotFound（404 / 存在差非露出）へ写像済み（Req 2.4 / 5.1 / 5.2）。
		return DeviceDetail{}, err
	}
	return rowToDetail(row, s.syncCutoff()), nil
}

// Overview は Service.Overview の実装。
func (s *service) Overview(ctx context.Context, tenantFilter *uuid.UUID) ([]TenantOverview, error) {
	rows, err := s.repo.AggregateOverview(ctx, tenantFilter)
	if err != nil {
		return nil, err
	}
	return foldOverview(rows), nil
}

// rowToSummary は DeviceRow を一覧 1 行（DeviceSummary）へ写像する。SyncDelayed は cutoff との
// 比較で算出する（filter と同一 cutoff を共用 / Req 4.x）。
func rowToSummary(row DeviceRow, cutoff time.Time) DeviceSummary {
	return DeviceSummary{
		ID:               row.ID,
		AMAPIDeviceName:  row.AMAPIDeviceName,
		Mode:             row.Mode,
		ComplianceStatus: row.ComplianceStatus,
		LastStatusAt:     row.LastStatusAt,
		SyncDelayed:      isSyncDelayed(row.LastStatusAt, cutoff),
	}
}

// rowToDetail は DeviceRow を詳細（DeviceDetail）へ写像する（Req 2.1〜2.5 / 3.1〜3.3）。
//
// applied_policy_name の NULL は "" に写像し、jsonb 空属性は null ではなく {} / [] を補填する
// （Req 2.5）。compliance_status は stored 値をそのまま返す（再判定しない / Req 3.1・3.3）。
// SyncDelayed は cutoff との比較で算出する（Req 2.3 / 4.x）。
func rowToDetail(row DeviceRow, cutoff time.Time) DeviceDetail {
	appliedPolicyName := ""
	if row.AppliedPolicyName != nil {
		appliedPolicyName = *row.AppliedPolicyName
	}
	return DeviceDetail{
		ID:                   row.ID,
		AMAPIDeviceName:      row.AMAPIDeviceName,
		Mode:                 row.Mode,
		AppliedPolicyName:    appliedPolicyName,
		HardwareInfo:         rawOrDefault(row.HardwareInfo, defaultJSONObject),
		SoftwareInfo:         rawOrDefault(row.SoftwareInfo, defaultJSONObject),
		ComplianceStatus:     row.ComplianceStatus,
		NonComplianceDetails: rawOrDefault(row.NonComplianceDetails, defaultJSONArray),
		InstalledApps:        rawOrDefault(row.InstalledApps, defaultJSONArray),
		LastStatusAt:         row.LastStatusAt,
		SyncDelayed:          isSyncDelayed(row.LastStatusAt, cutoff),
		EnrolledAt:           row.EnrolledAt,
	}
}

// rawOrDefault は jsonb 属性が空（nil / 0 byte）のとき default（{} または []）を補填する（Req 2.5）。
// DB は既定で {} / [] を格納するため通常は non-empty だが、未取得で空の場合に null を返さず空構造で
// 返すことを保証する。
func rawOrDefault(raw json.RawMessage, def string) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(def)
	}
	return raw
}

// foldOverview は flat な件数行（TenantComplianceCount）を tenant 単位の TenantOverview へ畳み込む
// （Req 6.1 / 6.4）。
//
// 各 TenantOverview の Breakdown は 4 分類（compliant / non_compliant / unknown / unsupported）を
// 必ず 0 埋めで初期化してから件数を加算する。DeviceCount は当該 tenant の全 count 合計。tenant の
// 出現順（Repository は tenant_id 順で返す）を保つため slice + index map で畳み込む。全体 0 件は
// 非 nil の空 slice を返す（Req 6.4）。
func foldOverview(rows []TenantComplianceCount) []TenantOverview {
	order := make([]uuid.UUID, 0)
	byTenant := make(map[uuid.UUID]*TenantOverview)
	for _, r := range rows {
		ov, ok := byTenant[r.TenantID]
		if !ok {
			ov = &TenantOverview{
				TenantID:  r.TenantID,
				Breakdown: newBreakdown(),
			}
			byTenant[r.TenantID] = ov
			order = append(order, r.TenantID)
		}
		ov.Breakdown[r.ComplianceStatus] += r.Count
		ov.DeviceCount += r.Count
	}
	// 出現順を保った非 nil 空 slice（全体 0 件でも空 slice を返す / Req 6.4）。
	out := make([]TenantOverview, 0, len(order))
	for _, id := range order {
		out = append(out, *byTenant[id])
	}
	return out
}

// newBreakdown は 4 分類を 0 埋めした Breakdown map を返す（未出現分類も 0 で明示する / Req 6.1）。
func newBreakdown() map[ComplianceStatus]int {
	return map[ComplianceStatus]int{
		ComplianceStatusCompliant:    0,
		ComplianceStatusNonCompliant: 0,
		ComplianceStatusUnknown:      0,
		ComplianceStatusUnsupported:  0,
	}
}

// 型 assertion: service が Service interface（read 3 メソッド）を満たすことを compile-time で確認する。
var _ Service = (*service)(nil)
