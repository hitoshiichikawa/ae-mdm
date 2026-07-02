package device

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

// ---- Fake Clock（固定時刻を返し、同期遅延判定を決定的にする / Req 4.x） ----

type fakeClock struct {
	now time.Time
}

func (c fakeClock) Now() time.Time { return c.now }

// ---- Fake Repository（返す rows / err と、Service から渡された引数を検証） ----

type fakeServiceRepo struct {
	listRows  []DeviceRow
	listErr   error
	gotFilter ListFilter
	gotCutoff time.Time

	getRow DeviceRow
	getErr error

	overviewRows    []TenantComplianceCount
	overviewErr     error
	gotTenantFilter *uuid.UUID
}

func (r *fakeServiceRepo) ListByTenant(_ context.Context, _ uuid.UUID, f ListFilter, cutoff time.Time) ([]DeviceRow, error) {
	r.gotFilter = f
	r.gotCutoff = cutoff
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.listRows, nil
}

func (r *fakeServiceRepo) GetByID(_ context.Context, _, _ uuid.UUID) (DeviceRow, error) {
	if r.getErr != nil {
		return DeviceRow{}, r.getErr
	}
	return r.getRow, nil
}

func (r *fakeServiceRepo) AggregateOverview(_ context.Context, tenantFilter *uuid.UUID) ([]TenantComplianceCount, error) {
	r.gotTenantFilter = tenantFilter
	if r.overviewErr != nil {
		return nil, r.overviewErr
	}
	return r.overviewRows, nil
}

func (r *fakeServiceRepo) UpdateFromStatusReport(_ context.Context, _ StatusApplyInput) (int64, error) {
	// read Service は write を呼ばないため no-op（interface 充足のみ）。
	return 0, nil
}

// compile-time check: *fakeServiceRepo は Repository を満たす。
var _ Repository = (*fakeServiceRepo)(nil)

// fixedNow はテストで用いる固定現在時刻。
var fixedNow = time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

// TestIsSyncDelayed は同期遅延判定の境界（超過 / ちょうど / 直後 / NULL）を検証する（Req 4.1・4.3）。
//
// strict less-than のため cutoff ちょうど（等値）は非遅延（Req 4.3）、last_status_at=NULL は
// 非遅延（Req 4.1）。
func TestIsSyncDelayed(t *testing.T) {
	// Arrange
	cutoff := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	before := cutoff.Add(-time.Hour)
	after := cutoff.Add(time.Hour)
	tests := []struct {
		name string
		last *time.Time
		want bool
	}{
		{"最終同期が閾値超過（cutoff より前）のとき遅延と判定する", &before, true},
		{"最終同期が閾値ちょうど（cutoff と等値）のとき非遅延と判定する", &cutoff, false},
		{"最終同期が閾値内（cutoff より後）のとき非遅延と判定する", &after, false},
		{"最終同期が未観測（NULL）のとき非遅延と判定する", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Act
			got := isSyncDelayed(tt.last, cutoff)
			// Assert
			if got != tt.want {
				t.Errorf("isSyncDelayed = %v; want %v", got, tt.want)
			}
		})
	}
}

// TestService_List_空でも非nil空sliceを返す は端末 0 件のとき List がエラーとせず非 nil の空 slice を
// 返すことを検証する（Req 1.7 / 空入力）。
func TestService_List_空でも非nil空sliceを返す(t *testing.T) {
	// Arrange
	repo := &fakeServiceRepo{listRows: []DeviceRow{}}
	svc := NewService(repo, fakeClock{now: fixedNow}, 24)

	// Act
	got, err := svc.List(context.Background(), uuid.New(), ListFilter{})

	// Assert
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got == nil {
		t.Fatalf("List は 0 件でも非 nil の空 slice を返すべき")
	}
	if len(got) != 0 {
		t.Errorf("len(got) = %d; want 0", len(got))
	}
}

// TestService_List_perRowで同期遅延を算出する は per-row SyncDelayed が filter と同一 cutoff を
// 共用して算出されること、cutoff が Repository へ渡ることを検証する（Req 2.3 / 4.1・4.3）。
func TestService_List_perRowで同期遅延を算出する(t *testing.T) {
	// Arrange
	cutoff := fixedNow.Add(-24 * time.Hour)
	old := cutoff.Add(-time.Hour)      // cutoff より前 → 遅延
	exact := cutoff                    // cutoff ちょうど → 非遅延（Req 4.3）
	recent := fixedNow.Add(-time.Hour) // cutoff より後 → 非遅延
	rows := []DeviceRow{
		{ID: uuid.New(), LastStatusAt: &old},
		{ID: uuid.New(), LastStatusAt: &exact},
		{ID: uuid.New(), LastStatusAt: &recent},
		{ID: uuid.New(), LastStatusAt: nil}, // NULL → 非遅延（Req 4.1）
	}
	repo := &fakeServiceRepo{listRows: rows}
	svc := NewService(repo, fakeClock{now: fixedNow}, 24)

	// Act
	got, err := svc.List(context.Background(), uuid.New(), ListFilter{})

	// Assert
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []bool{true, false, false, false}
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d; want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].SyncDelayed != want[i] {
			t.Errorf("row[%d].SyncDelayed = %v; want %v", i, got[i].SyncDelayed, want[i])
		}
	}
	// filter と per-row 算出で共用する cutoff が Repository へ渡ること（同一 cutoff の共用）。
	if !repo.gotCutoff.Equal(cutoff) {
		t.Errorf("Repository へ渡した cutoff = %v; want %v", repo.gotCutoff, cutoff)
	}
}

// TestService_同期遅延の閾値が設定値で境界を動かす は閾値（設定値）が同期遅延の境界を実際に
// 動かすことを検証する（Req 4.2）。同一端末でも閾値 24h では遅延、48h では非遅延になる。
func TestService_同期遅延の閾値が設定値で境界を動かす(t *testing.T) {
	// Arrange: 最終同期が現在時刻の 30 時間前の端末。
	last := fixedNow.Add(-30 * time.Hour)
	id := uuid.New()
	row := DeviceRow{ID: id, LastStatusAt: &last}

	// Act + Assert: 閾値 24h では cutoff=now-24h、last(-30h) は cutoff より前 → 遅延。
	got24, err := NewService(&fakeServiceRepo{getRow: row}, fakeClock{now: fixedNow}, 24).
		Get(context.Background(), uuid.New(), id)
	if err != nil {
		t.Fatalf("Get(閾値 24h): %v", err)
	}
	if !got24.SyncDelayed {
		t.Errorf("閾値 24h では -30h 端末は遅延であるべき (SyncDelayed=%v)", got24.SyncDelayed)
	}

	// Act + Assert: 閾値 48h では cutoff=now-48h、last(-30h) は cutoff より後 → 非遅延。
	got48, err := NewService(&fakeServiceRepo{getRow: row}, fakeClock{now: fixedNow}, 48).
		Get(context.Background(), uuid.New(), id)
	if err != nil {
		t.Fatalf("Get(閾値 48h): %v", err)
	}
	if got48.SyncDelayed {
		t.Errorf("閾値 48h では -30h 端末は非遅延であるべき (SyncDelayed=%v)", got48.SyncDelayed)
	}
}

// TestService_Get_全属性を詳細へ写像する は DeviceRow の全属性が DeviceDetail へ写像されること
// を検証する（Req 2.1・2.2・2.3・3.1・3.2）。非準拠端末では compliance と非準拠理由 jsonb を併記する。
func TestService_Get_全属性を詳細へ写像する(t *testing.T) {
	// Arrange
	last := fixedNow.Add(-48 * time.Hour) // 閾値 24h 超過 → 遅延
	enrolled := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	policyName := "enterprises/X/policies/applied"
	id := uuid.New()
	row := DeviceRow{
		ID:                   id,
		AMAPIDeviceName:      "enterprises/X/devices/A1",
		Mode:                 DeviceModeDedicated,
		AppliedPolicyName:    &policyName,
		HardwareInfo:         json.RawMessage(`{"brand":"acme"}`),
		SoftwareInfo:         json.RawMessage(`{"os":"android"}`),
		ComplianceStatus:     ComplianceStatusNonCompliant,
		NonComplianceDetails: json.RawMessage(`[{"reason":"blocked"}]`),
		InstalledApps:        json.RawMessage(`[{"pkg":"com.example"}]`),
		LastStatusAt:         &last,
		EnrolledAt:           enrolled,
	}
	svc := NewService(&fakeServiceRepo{getRow: row}, fakeClock{now: fixedNow}, 24)

	// Act
	got, err := svc.Get(context.Background(), uuid.New(), id)

	// Assert
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != id {
		t.Errorf("ID = %v; want %v", got.ID, id)
	}
	if got.AMAPIDeviceName != "enterprises/X/devices/A1" {
		t.Errorf("AMAPIDeviceName = %q", got.AMAPIDeviceName)
	}
	if got.Mode != DeviceModeDedicated { // Req 2.1 管理モード
		t.Errorf("Mode = %q; want %q", got.Mode, DeviceModeDedicated)
	}
	if got.AppliedPolicyName != policyName { // Req 2.1 適用中ポリシー（報告値）
		t.Errorf("AppliedPolicyName = %q; want %q", got.AppliedPolicyName, policyName)
	}
	if string(got.HardwareInfo) != `{"brand":"acme"}` { // Req 2.1 ハードウェア情報
		t.Errorf("HardwareInfo = %s", got.HardwareInfo)
	}
	if string(got.SoftwareInfo) != `{"os":"android"}` { // Req 2.1 ソフトウェア情報
		t.Errorf("SoftwareInfo = %s", got.SoftwareInfo)
	}
	if got.ComplianceStatus != ComplianceStatusNonCompliant { // Req 3.1 分類
		t.Errorf("ComplianceStatus = %q; want %q", got.ComplianceStatus, ComplianceStatusNonCompliant)
	}
	if string(got.NonComplianceDetails) != `[{"reason":"blocked"}]` { // Req 3.2 非準拠理由併記
		t.Errorf("NonComplianceDetails = %s", got.NonComplianceDetails)
	}
	if string(got.InstalledApps) != `[{"pkg":"com.example"}]` { // Req 2.2 インストール済みアプリ
		t.Errorf("InstalledApps = %s", got.InstalledApps)
	}
	if got.LastStatusAt == nil || !got.LastStatusAt.Equal(last) { // Req 2.1 最終同期時刻
		t.Errorf("LastStatusAt = %v; want %v", got.LastStatusAt, last)
	}
	if !got.SyncDelayed { // Req 2.3 同期遅延フラグ
		t.Errorf("SyncDelayed = %v; want true", got.SyncDelayed)
	}
	if !got.EnrolledAt.Equal(enrolled) {
		t.Errorf("EnrolledAt = %v; want %v", got.EnrolledAt, enrolled)
	}
}

// TestService_Get_空属性はデフォルト値で返す は未取得で空の属性が null ではなく {} / [] で返り、
// applied_policy_name の NULL が "" に写像され、NULL 最終同期が非遅延になることを検証する
// （Req 2.5 / 3.3 / 4.1 の境界）。
func TestService_Get_空属性はデフォルト値で返す(t *testing.T) {
	// Arrange: jsonb 属性が空（nil）、applied_policy_name NULL、compliance 未観測（unknown）、
	// last_status_at NULL の端末。
	id := uuid.New()
	row := DeviceRow{
		ID:                   id,
		AMAPIDeviceName:      "enterprises/X/devices/A2",
		Mode:                 DeviceModeFullyManaged,
		AppliedPolicyName:    nil,
		HardwareInfo:         nil,
		SoftwareInfo:         nil,
		ComplianceStatus:     ComplianceStatusUnknown,
		NonComplianceDetails: nil,
		InstalledApps:        nil,
		LastStatusAt:         nil,
	}
	svc := NewService(&fakeServiceRepo{getRow: row}, fakeClock{now: fixedNow}, 24)

	// Act
	got, err := svc.Get(context.Background(), uuid.New(), id)

	// Assert
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AppliedPolicyName != "" { // NULL → ""
		t.Errorf("AppliedPolicyName = %q; want empty", got.AppliedPolicyName)
	}
	if string(got.HardwareInfo) != defaultJSONObject { // Req 2.5 空 object → {}
		t.Errorf("HardwareInfo = %s; want %s", got.HardwareInfo, defaultJSONObject)
	}
	if string(got.SoftwareInfo) != defaultJSONObject { // Req 2.5
		t.Errorf("SoftwareInfo = %s; want %s", got.SoftwareInfo, defaultJSONObject)
	}
	if string(got.NonComplianceDetails) != defaultJSONArray { // Req 2.5 空 array → []
		t.Errorf("NonComplianceDetails = %s; want %s", got.NonComplianceDetails, defaultJSONArray)
	}
	if string(got.InstalledApps) != defaultJSONArray { // Req 2.5
		t.Errorf("InstalledApps = %s; want %s", got.InstalledApps, defaultJSONArray)
	}
	if got.ComplianceStatus != ComplianceStatusUnknown { // Req 3.3 未観測は unknown をそのまま
		t.Errorf("ComplianceStatus = %q; want unknown", got.ComplianceStatus)
	}
	if got.SyncDelayed { // Req 4.1 NULL 最終同期 → 非遅延
		t.Errorf("SyncDelayed = %v; want false（NULL は非遅延）", got.SyncDelayed)
	}
}

// TestService_Get_compliance分類をそのまま返す は stored compliance_status が 4 分類のいずれでも
// 再判定されず、そのまま返却されることを検証する（Req 3.1・3.3）。
func TestService_Get_compliance分類をそのまま返す(t *testing.T) {
	statuses := []ComplianceStatus{
		ComplianceStatusCompliant,
		ComplianceStatusNonCompliant,
		ComplianceStatusUnknown,
		ComplianceStatusUnsupported,
	}
	for _, st := range statuses {
		t.Run(string(st), func(t *testing.T) {
			// Arrange
			repo := &fakeServiceRepo{getRow: DeviceRow{ID: uuid.New(), ComplianceStatus: st}}
			svc := NewService(repo, fakeClock{now: fixedNow}, 24)

			// Act
			got, err := svc.Get(context.Background(), uuid.New(), uuid.New())

			// Assert
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.ComplianceStatus != st {
				t.Errorf("ComplianceStatus = %q; want %q（stored 値をそのまま返す）", got.ComplianceStatus, st)
			}
		})
	}
}

// TestService_Get_Repositoryのエラーをそのまま伝達する は不在 / 越境時に Repository が写像した
// ErrDeviceNotFound をそのまま伝達することを検証する（異常系 / Req 2.4・5.1・5.2 の Service 面）。
func TestService_Get_Repositoryのエラーをそのまま伝達する(t *testing.T) {
	// Arrange
	repo := &fakeServiceRepo{getErr: ErrDeviceNotFound}
	svc := NewService(repo, fakeClock{now: fixedNow}, 24)

	// Act
	_, err := svc.Get(context.Background(), uuid.New(), uuid.New())

	// Assert
	if !stderrors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("err = %v; want ErrDeviceNotFound", err)
	}
}

// TestService_Overview_flat件数行をtenant単位に畳み込む は flat な件数行が tenant 単位に畳み込まれ、
// 4 分類が 0 埋めされ、DeviceCount が合計され、出現順が保たれることを検証する（Req 6.1）。
func TestService_Overview_flat件数行をtenant単位に畳み込む(t *testing.T) {
	// Arrange: Repository は tenant_id 順の flat 件数行を返す（tenantA が先、tenantB が後）。
	tenantA := uuid.New()
	tenantB := uuid.New()
	rows := []TenantComplianceCount{
		{TenantID: tenantA, ComplianceStatus: ComplianceStatusCompliant, Count: 2},
		{TenantID: tenantA, ComplianceStatus: ComplianceStatusNonCompliant, Count: 1},
		{TenantID: tenantB, ComplianceStatus: ComplianceStatusUnknown, Count: 3},
	}
	svc := NewService(&fakeServiceRepo{overviewRows: rows}, fakeClock{now: fixedNow}, 24)

	// Act
	got, err := svc.Overview(context.Background(), nil)

	// Assert
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d; want 2", len(got))
	}
	// 出現順（tenant_id 順）を保つ。
	if got[0].TenantID != tenantA || got[1].TenantID != tenantB {
		t.Fatalf("tenant 出現順が保たれていない: got[0]=%v got[1]=%v", got[0].TenantID, got[1].TenantID)
	}
	// DeviceCount は当該 tenant の全 count 合計。
	if got[0].DeviceCount != 3 {
		t.Errorf("tenantA DeviceCount = %d; want 3", got[0].DeviceCount)
	}
	if got[1].DeviceCount != 3 {
		t.Errorf("tenantB DeviceCount = %d; want 3", got[1].DeviceCount)
	}
	// Breakdown は 4 分類 0 埋め（未出現分類も 0 で明示）。
	wantA := map[ComplianceStatus]int{
		ComplianceStatusCompliant:    2,
		ComplianceStatusNonCompliant: 1,
		ComplianceStatusUnknown:      0,
		ComplianceStatusUnsupported:  0,
	}
	if !reflect.DeepEqual(got[0].Breakdown, wantA) {
		t.Errorf("tenantA Breakdown = %v; want %v", got[0].Breakdown, wantA)
	}
	wantB := map[ComplianceStatus]int{
		ComplianceStatusCompliant:    0,
		ComplianceStatusNonCompliant: 0,
		ComplianceStatusUnknown:      3,
		ComplianceStatusUnsupported:  0,
	}
	if !reflect.DeepEqual(got[1].Breakdown, wantB) {
		t.Errorf("tenantB Breakdown = %v; want %v", got[1].Breakdown, wantB)
	}
}

// TestService_Overview_全体0件で非nil空sliceを返す は集計対象が 0 件のとき Overview がエラーとせず
// 非 nil の空 slice を返すことを検証する（Req 6.4 / 空入力）。
func TestService_Overview_全体0件で非nil空sliceを返す(t *testing.T) {
	// Arrange
	svc := NewService(&fakeServiceRepo{overviewRows: []TenantComplianceCount{}}, fakeClock{now: fixedNow}, 24)

	// Act
	got, err := svc.Overview(context.Background(), nil)

	// Assert
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if got == nil {
		t.Fatalf("Overview は 0 件でも非 nil の空 slice を返すべき")
	}
	if len(got) != 0 {
		t.Errorf("len(got) = %d; want 0", len(got))
	}
}
