package device

import (
	"encoding/json"
	stderrors "errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// fakeDeviceRow は rowScanner を満たすテスト用 1 行。Scan へ渡す列値を保持し、注入された
// scanErr があればそれを返す（実 PostgreSQL に依存せず scanDeviceRow を単体テストする）。
//
// 列順は scanDeviceRow の Scan 順（id, tenant_id, amapi_device_name, mode, applied_policy_id,
// applied_policy_name, hardware_info, software_info, compliance_status, non_compliance_details,
// installed_apps, last_status_at, enrolled_at）に一致させる。enum 列は生ラベル string、jsonb 列は
// 生 bytes []byte を保持し、scanDeviceRow が DeviceMode / ComplianceStatus / json.RawMessage へ
// 写像することを検証する。
type fakeDeviceRow struct {
	id                uuid.UUID
	tenantID          uuid.UUID
	amapiName         string
	mode              string
	appliedPolicyID   *uuid.UUID
	appliedPolicyName *string
	hardware          []byte
	software          []byte
	compliance        string
	nonCompliance     []byte
	installedApps     []byte
	lastStatusAt      *time.Time
	enrolledAt        time.Time
	scanErr           error
}

func (f *fakeDeviceRow) Scan(dest ...any) error {
	if f.scanErr != nil {
		return f.scanErr
	}
	if len(dest) != 13 {
		return fmt.Errorf("fakeDeviceRow.Scan: dest 件数 = %d; want 13", len(dest))
	}
	*(dest[0].(*uuid.UUID)) = f.id
	*(dest[1].(*uuid.UUID)) = f.tenantID
	*(dest[2].(*string)) = f.amapiName
	*(dest[3].(*string)) = f.mode
	*(dest[4].(**uuid.UUID)) = f.appliedPolicyID
	*(dest[5].(**string)) = f.appliedPolicyName
	*(dest[6].(*[]byte)) = f.hardware
	*(dest[7].(*[]byte)) = f.software
	*(dest[8].(*string)) = f.compliance
	*(dest[9].(*[]byte)) = f.nonCompliance
	*(dest[10].(*[]byte)) = f.installedApps
	*(dest[11].(**time.Time)) = f.lastStatusAt
	*(dest[12].(*time.Time)) = f.enrolledAt
	return nil
}

// TestScanDeviceRow_AllColumns は scanDeviceRow が全列を型付きで DeviceRow に束ねること
// （NFR 2.1）を検証する。enum 列は DeviceMode / ComplianceStatus へ、jsonb 列は json.RawMessage へ、
// nullable 列は非 NULL の pointer へ写像される。
func TestScanDeviceRow_AllColumns(t *testing.T) {
	// Arrange
	id := uuid.New()
	tenantID := uuid.New()
	policyID := uuid.New()
	policyName := "enterprises/X/policies/applied"
	last := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	enrolled := time.Date(2026, 6, 1, 8, 0, 0, 0, time.UTC)
	row := &fakeDeviceRow{
		id:                id,
		tenantID:          tenantID,
		amapiName:         "enterprises/X/devices/A1",
		mode:              string(DeviceModeDedicated),
		appliedPolicyID:   &policyID,
		appliedPolicyName: &policyName,
		hardware:          []byte(`{"brand":"acme"}`),
		software:          []byte(`{"os":"android"}`),
		compliance:        string(ComplianceStatusNonCompliant),
		nonCompliance:     []byte(`[{"reason":"blocked"}]`),
		installedApps:     []byte(`[{"pkg":"com.example"}]`),
		lastStatusAt:      &last,
		enrolledAt:        enrolled,
	}

	// Act
	got, err := scanDeviceRow(row)

	// Assert
	if err != nil {
		t.Fatalf("scanDeviceRow: %v", err)
	}
	if got.ID != id || got.TenantID != tenantID {
		t.Errorf("ID/TenantID mismatch: got %v/%v", got.ID, got.TenantID)
	}
	if got.AMAPIDeviceName != "enterprises/X/devices/A1" {
		t.Errorf("AMAPIDeviceName = %q", got.AMAPIDeviceName)
	}
	if got.Mode != DeviceModeDedicated {
		t.Errorf("Mode = %q; want %q", got.Mode, DeviceModeDedicated)
	}
	if got.ComplianceStatus != ComplianceStatusNonCompliant {
		t.Errorf("ComplianceStatus = %q; want %q", got.ComplianceStatus, ComplianceStatusNonCompliant)
	}
	if got.AppliedPolicyID == nil || *got.AppliedPolicyID != policyID {
		t.Errorf("AppliedPolicyID = %v; want %v", got.AppliedPolicyID, policyID)
	}
	if got.AppliedPolicyName == nil || *got.AppliedPolicyName != policyName {
		t.Errorf("AppliedPolicyName = %v; want %q", got.AppliedPolicyName, policyName)
	}
	if string(got.HardwareInfo) != `{"brand":"acme"}` {
		t.Errorf("HardwareInfo = %s", got.HardwareInfo)
	}
	if string(got.NonComplianceDetails) != `[{"reason":"blocked"}]` {
		t.Errorf("NonComplianceDetails = %s", got.NonComplianceDetails)
	}
	if got.LastStatusAt == nil || !got.LastStatusAt.Equal(last) {
		t.Errorf("LastStatusAt = %v; want %v", got.LastStatusAt, last)
	}
	if !got.EnrolledAt.Equal(enrolled) {
		t.Errorf("EnrolledAt = %v; want %v", got.EnrolledAt, enrolled)
	}
}

// TestScanDeviceRow_NullableNulls は nullable 列（applied_policy_id / applied_policy_name /
// last_status_at）が NULL（nil ポインタ）の行で対応フィールドが nil のまま保たれること、jsonb 空値が
// json.RawMessage として保持されることを検証する（境界値 / Req 2.5 の DB 層下支え）。
func TestScanDeviceRow_NullableNulls(t *testing.T) {
	// Arrange
	row := &fakeDeviceRow{
		id:                uuid.New(),
		tenantID:          uuid.New(),
		amapiName:         "enterprises/X/devices/A2",
		mode:              string(DeviceModeFullyManaged),
		appliedPolicyID:   nil,
		appliedPolicyName: nil,
		hardware:          []byte(`{}`),
		software:          []byte(`{}`),
		compliance:        string(ComplianceStatusUnknown),
		nonCompliance:     []byte(`[]`),
		installedApps:     []byte(`[]`),
		lastStatusAt:      nil,
		enrolledAt:        time.Now(),
	}

	// Act
	got, err := scanDeviceRow(row)

	// Assert
	if err != nil {
		t.Fatalf("scanDeviceRow: %v", err)
	}
	if got.AppliedPolicyID != nil {
		t.Errorf("AppliedPolicyID = %v; want nil", got.AppliedPolicyID)
	}
	if got.AppliedPolicyName != nil {
		t.Errorf("AppliedPolicyName = %v; want nil", got.AppliedPolicyName)
	}
	if got.LastStatusAt != nil {
		t.Errorf("LastStatusAt = %v; want nil", got.LastStatusAt)
	}
	if got.ComplianceStatus != ComplianceStatusUnknown {
		t.Errorf("ComplianceStatus = %q; want unknown", got.ComplianceStatus)
	}
	if string(got.HardwareInfo) != `{}` || string(got.InstalledApps) != `[]` {
		t.Errorf("jsonb 空値が保持されない: hw=%s apps=%s", got.HardwareInfo, got.InstalledApps)
	}
}

// TestScanDeviceRow_ScanError は Scan が error を返した場合、そのまま伝播される
// （上位 GetByID / ListByTenant が NotFound / Unavailable 写像を担う）ことを検証する（異常系）。
func TestScanDeviceRow_ScanError(t *testing.T) {
	// Arrange
	sentinel := fmt.Errorf("simulated scan failure")
	row := &fakeDeviceRow{scanErr: sentinel}

	// Act
	_, err := scanDeviceRow(row)

	// Assert
	if !stderrors.Is(err, sentinel) {
		t.Fatalf("scanDeviceRow は scan error をそのまま返すべき: got %v", err)
	}
}

// TestMapGetError_NoRows は 0 行（pgx.ErrNoRows）が存在差非露出の ErrDeviceNotFound
// （CodeNotFound / 404 / 汎用 message）に写像されることを検証する（Req 2.4 / 5.1 / 5.2 / NFR 3.1）。
func TestMapGetError_NoRows(t *testing.T) {
	// Arrange / Act
	err := mapGetError(pgx.ErrNoRows)

	// Assert
	var de *pkgerrors.Error
	if !stderrors.As(err, &de) || de.Code != pkgerrors.CodeNotFound {
		t.Fatalf("err = %v; want CodeNotFound", err)
	}
	if !stderrors.Is(err, ErrDeviceNotFound) {
		t.Errorf("err は ErrDeviceNotFound と Is 一致すべき: got %v", err)
	}
	// 存在差非露出: message に device id 等の識別子を補間しない汎用文言であること。
	if de.Message != "device not found" {
		t.Errorf("message = %q; want 汎用 'device not found'", de.Message)
	}
}

// TestMapGetError_OtherDBError は 0 行以外の DB エラーが CodeUnavailable（503）に写像されること
// （異常系 / fail-closed）、Cause が保持されることを検証する。
func TestMapGetError_OtherDBError(t *testing.T) {
	// Arrange
	sentinel := fmt.Errorf("connection refused")

	// Act
	err := mapGetError(sentinel)

	// Assert
	var de *pkgerrors.Error
	if !stderrors.As(err, &de) || de.Code != pkgerrors.CodeUnavailable {
		t.Fatalf("err = %v; want CodeUnavailable", err)
	}
	if !stderrors.Is(err, sentinel) {
		t.Errorf("Cause が sentinel を保持していない: %v", err)
	}
}

// TestMapGetError_Nil は成功（err == nil）が nil を返すことを検証する（境界値）。
func TestMapGetError_Nil(t *testing.T) {
	if got := mapGetError(nil); got != nil {
		t.Fatalf("mapGetError(nil) = %v; want nil", got)
	}
}

// TestJSONBArg は *json.RawMessage が jsonb bind 値へ写像されること（nil → NULL bind の nil []byte /
// 非 nil → 生 bytes）を検証する（境界値 / UpdateFromStatusReport の COALESCE 部分更新 / Req 7.2）。
func TestJSONBArg(t *testing.T) {
	// nil ポインタ → nil []byte（pgx が NULL bind → COALESCE で既存値保持）。
	if got := jsonbArg(nil); got != nil {
		t.Errorf("jsonbArg(nil) = %v; want nil []byte (NULL bind)", got)
	}
	// 非 nil → 生 bytes。
	raw := json.RawMessage(`{"k":1}`)
	if got := jsonbArg(&raw); string(got) != `{"k":1}` {
		t.Errorf("jsonbArg = %s; want raw bytes", got)
	}
}

// TestComplianceArg は *ComplianceStatus が enum bind 用 text へ写像されること（nil → NULL bind /
// 非 nil → ラベル string）を検証する（境界値 / UpdateFromStatusReport の COALESCE 部分更新 / Req 7.2）。
func TestComplianceArg(t *testing.T) {
	// nil ポインタ → nil（pgx が NULL bind → COALESCE で既存値保持）。
	if got := complianceArg(nil); got != nil {
		t.Errorf("complianceArg(nil) = %v; want nil (NULL bind)", got)
	}
	// 非 nil → ラベル文字列。
	cs := ComplianceStatusCompliant
	got := complianceArg(&cs)
	if got == nil || *got != "compliant" {
		t.Errorf("complianceArg = %v; want \"compliant\"", got)
	}
}

// compile-time check: *fakeDeviceRow は rowScanner を満たす。
var _ rowScanner = (*fakeDeviceRow)(nil)
