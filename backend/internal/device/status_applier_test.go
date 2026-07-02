package device

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/hitoshiichikawa/ae-mdm/internal/notification"
)

// fakeStatusRepo は Repository を stub し、UpdateFromStatusReport に渡る StatusApplyInput を
// capture するテスト用 fake。read 系 3 メソッド（ListByTenant / GetByID / AggregateOverview）は
// StatusApplier では未使用のため no-op で満たす。affected / updateErr で戻り値を差し替えられる。
type fakeStatusRepo struct {
	captured  StatusApplyInput
	calls     int
	affected  int64
	updateErr error
}

func (f *fakeStatusRepo) ListByTenant(context.Context, uuid.UUID, ListFilter, time.Time) ([]DeviceRow, error) {
	return nil, nil
}

func (f *fakeStatusRepo) GetByID(context.Context, uuid.UUID, uuid.UUID) (DeviceRow, error) {
	return DeviceRow{}, nil
}

func (f *fakeStatusRepo) AggregateOverview(context.Context, *uuid.UUID) ([]TenantComplianceCount, error) {
	return nil, nil
}

func (f *fakeStatusRepo) UpdateFromStatusReport(_ context.Context, u StatusApplyInput) (int64, error) {
	f.calls++
	f.captured = u
	return f.affected, f.updateErr
}

// rawPtr は JSON 文字列を *json.RawMessage 化するテスト helper。
func rawPtr(s string) *json.RawMessage {
	r := json.RawMessage(s)
	return &r
}

// TestStatusApplier_EmptyNonComplianceDetailsMarksCompliant は、非準拠理由が空配列で payload に
// 存在するとき compliant を導出し、non_compliance_details を渡すことを検証する（Req 3.2）。
func TestStatusApplier_EmptyNonComplianceDetailsMarksCompliant(t *testing.T) {
	// Arrange
	repo := &fakeStatusRepo{affected: 1}
	a := NewStatusApplier(repo, nil)
	report := notification.DeviceStatusReport{
		DeviceName:           "enterprises/X/devices/d1",
		NonComplianceDetails: rawPtr(`[]`),
	}

	// Act
	err := a.ApplyStatusReport(context.Background(), report)

	// Assert
	if err != nil {
		t.Fatalf("正常系で error を返すべきでない: %v", err)
	}
	if repo.captured.ComplianceStatus == nil || *repo.captured.ComplianceStatus != ComplianceStatusCompliant {
		t.Errorf("空 non_compliance_details は compliant を導出すべき: got %v", repo.captured.ComplianceStatus)
	}
	if repo.captured.NonComplianceDetails == nil {
		t.Error("payload に存在する non_compliance_details は Repository へ渡すべき")
	}
}

// TestStatusApplier_NonEmptyNonComplianceDetailsMarksNonCompliant は、非準拠理由が非空配列で
// 存在するとき non_compliant を導出し、理由 jsonb を渡すことを検証する（Req 3.2）。
func TestStatusApplier_NonEmptyNonComplianceDetailsMarksNonCompliant(t *testing.T) {
	// Arrange
	repo := &fakeStatusRepo{affected: 1}
	a := NewStatusApplier(repo, nil)
	report := notification.DeviceStatusReport{
		DeviceName:           "enterprises/X/devices/d1",
		NonComplianceDetails: rawPtr(`[{"settingName":"passwordPolicies"}]`),
	}

	// Act
	err := a.ApplyStatusReport(context.Background(), report)

	// Assert
	if err != nil {
		t.Fatalf("正常系で error を返すべきでない: %v", err)
	}
	if repo.captured.ComplianceStatus == nil || *repo.captured.ComplianceStatus != ComplianceStatusNonCompliant {
		t.Errorf("非空 non_compliance_details は non_compliant を導出すべき: got %v", repo.captured.ComplianceStatus)
	}
	if repo.captured.NonComplianceDetails == nil || string(*repo.captured.NonComplianceDetails) != `[{"settingName":"passwordPolicies"}]` {
		t.Errorf("非準拠理由 jsonb がそのまま渡るべき: got %v", repo.captured.NonComplianceDetails)
	}
}

// TestStatusApplier_MissingNonComplianceDetailsLeavesComplianceUnchanged は、非準拠理由が payload に
// 欠落（nil）しているとき compliance_status / non_compliance_details のいずれも更新しない（nil の
// まま Repository へ渡し COALESCE で既存値保持）ことを検証する（Req 3.2・7.2）。
func TestStatusApplier_MissingNonComplianceDetailsLeavesComplianceUnchanged(t *testing.T) {
	// Arrange
	repo := &fakeStatusRepo{affected: 1}
	a := NewStatusApplier(repo, nil)
	report := notification.DeviceStatusReport{
		DeviceName: "enterprises/X/devices/d1",
		// NonComplianceDetails は nil（欠落）。
	}

	// Act
	err := a.ApplyStatusReport(context.Background(), report)

	// Assert
	if err != nil {
		t.Fatalf("正常系で error を返すべきでない: %v", err)
	}
	if repo.captured.ComplianceStatus != nil {
		t.Errorf("欠落時は compliance_status を更新すべきでない: got %v", *repo.captured.ComplianceStatus)
	}
	if repo.captured.NonComplianceDetails != nil {
		t.Errorf("欠落時は non_compliance_details を更新すべきでない: got %v", repo.captured.NonComplianceDetails)
	}
}

// TestStatusApplier_ComplianceDerivationDefaults は、compliant/non_compliant 判定の境界・異常系の
// 既定（null / 空白のみ → compliant / array 以外の malformed → 非準拠へ安全側で倒す）を検証する。
func TestStatusApplier_ComplianceDerivationDefaults(t *testing.T) {
	tests := []struct {
		name string
		ncd  string
		want ComplianceStatus
	}{
		{name: "空配列は compliant", ncd: `[]`, want: ComplianceStatusCompliant},
		{name: "非空配列は non_compliant", ncd: `[{"s":"x"}]`, want: ComplianceStatusNonCompliant},
		{name: "JSON null は compliant に倒す", ncd: `null`, want: ComplianceStatusCompliant},
		{name: "空白のみは compliant に倒す", ncd: `   `, want: ComplianceStatusCompliant},
		{name: "array でない malformed は非準拠へ安全側に倒す", ncd: `{"broken":true}`, want: ComplianceStatusNonCompliant},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			repo := &fakeStatusRepo{affected: 1}
			a := NewStatusApplier(repo, nil)
			report := notification.DeviceStatusReport{
				DeviceName:           "enterprises/X/devices/d1",
				NonComplianceDetails: rawPtr(tt.ncd),
			}

			// Act
			if err := a.ApplyStatusReport(context.Background(), report); err != nil {
				t.Fatalf("error を返すべきでない: %v", err)
			}

			// Assert
			if repo.captured.ComplianceStatus == nil || *repo.captured.ComplianceStatus != tt.want {
				t.Errorf("compliance 導出 = %v; want %q", repo.captured.ComplianceStatus, tt.want)
			}
		})
	}
}

// TestStatusApplier_PartialPayloadKeepsMissingFieldsNil は、任意フィールドが欠落した payload で
// 存在するフィールドのみ非 nil、欠落フィールドは nil のまま Repository へ渡る（COALESCE で
// 既存値保持される契約）ことを検証する（Req 7.2）。
func TestStatusApplier_PartialPayloadKeepsMissingFieldsNil(t *testing.T) {
	// Arrange: name + softwareInfo のみ持つ部分 payload 相当。
	repo := &fakeStatusRepo{affected: 1}
	a := NewStatusApplier(repo, nil)
	report := notification.DeviceStatusReport{
		DeviceName:   "enterprises/X/devices/d1",
		SoftwareInfo: rawPtr(`{"androidVersion":"14"}`),
	}

	// Act
	if err := a.ApplyStatusReport(context.Background(), report); err != nil {
		t.Fatalf("部分 payload で error を返すべきでない: %v", err)
	}

	// Assert
	got := repo.captured
	if got.AMAPIDeviceName != "enterprises/X/devices/d1" {
		t.Errorf("AMAPIDeviceName は一致キーとして渡すべき: got %q", got.AMAPIDeviceName)
	}
	if got.SoftwareInfo == nil {
		t.Error("存在する SoftwareInfo は非 nil で渡すべき")
	}
	if got.HardwareInfo != nil {
		t.Error("欠落 HardwareInfo は nil のまま渡すべき（更新しない / Req 7.2）")
	}
	if got.AppliedPolicyName != nil {
		t.Error("欠落 AppliedPolicyName は nil のまま渡すべき（更新しない / Req 7.2）")
	}
	if got.InstalledApps != nil {
		t.Error("欠落 InstalledApps は nil のまま渡すべき（更新しない / Req 7.2）")
	}
	if got.LastStatusReportTime != nil {
		t.Error("欠落 LastStatusReportTime は nil のまま渡すべき（更新しない / Req 7.2）")
	}
}

// TestStatusApplier_AffectedZeroIsNoOpAck は、affected=0（未登録端末）でも error に倒さず nil を
// 返す（no-op ack）ことを検証する（Open Questions / design.md L288）。
func TestStatusApplier_AffectedZeroIsNoOpAck(t *testing.T) {
	// Arrange: Repository が affected=0 を返す（未登録 amapi_device_name）。
	repo := &fakeStatusRepo{affected: 0}
	a := NewStatusApplier(repo, nil)
	report := notification.DeviceStatusReport{DeviceName: "enterprises/X/devices/unknown"}

	// Act
	err := a.ApplyStatusReport(context.Background(), report)

	// Assert
	if err != nil {
		t.Errorf("未登録端末（affected=0）は error に倒さず nil を返すべき: got %v", err)
	}
	if repo.calls != 1 {
		t.Errorf("UpdateFromStatusReport は 1 回呼ばれるべき: got %d", repo.calls)
	}
}

// TestStatusApplier_RepositoryErrorIsPropagated は、Repository が返した error を再分類せず
// そのまま透過することを検証する（ack/nack 判定は Dispatcher の責務 / design.md Postconditions）。
func TestStatusApplier_RepositoryErrorIsPropagated(t *testing.T) {
	// Arrange
	sentinel := stderrors.New("update failed")
	repo := &fakeStatusRepo{affected: 1, updateErr: sentinel}
	a := NewStatusApplier(repo, nil)
	report := notification.DeviceStatusReport{DeviceName: "enterprises/X/devices/d1"}

	// Act
	err := a.ApplyStatusReport(context.Background(), report)

	// Assert
	if !stderrors.Is(err, sentinel) {
		t.Errorf("Repository のエラーは再分類せず透過すべき: got %v", err)
	}
}
