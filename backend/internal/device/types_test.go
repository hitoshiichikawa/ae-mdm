package device

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

// 本テストは task 2（型定義・骨格）の宣言面を検証する。振る舞いロジックは無いため、
// enum 値集合（DB enum compliance_status / device_mode との一致）・JSON tag（snake_case /
// 空 jsonb の {}/[] 表現）・zero-value（pointer 欠落 = 無条件 / payload 欠落）の 3 観点で
// 契約を固定する（_Requirements: 3.1, 3.4, 2.5_ の宣言面）。

func TestEnumValueSets(t *testing.T) {
	t.Run("ComplianceStatus の 4 定数が期待文字列に一致し unsupported を第 4 分類に含むとき DB enum と値集合が一致する", func(t *testing.T) {
		// Arrange: migration 0006 の compliance_status ENUM 値と 1:1 対応
		want := []struct {
			got ComplianceStatus
			exp string
		}{
			{ComplianceStatusCompliant, "compliant"},
			{ComplianceStatusNonCompliant, "non_compliant"},
			{ComplianceStatusUnknown, "unknown"},
			{ComplianceStatusUnsupported, "unsupported"},
		}

		// Act
		set := map[ComplianceStatus]struct{}{}
		for _, c := range want {
			set[c.got] = struct{}{}
		}

		// Assert: 各値が期待文字列と一致
		for _, c := range want {
			if string(c.got) != c.exp {
				t.Errorf("ComplianceStatus 値不一致: got %q, want %q", string(c.got), c.exp)
			}
		}
		// Assert: 第 4 分類 unsupported が値集合に含まれる（Req 3.4）
		if _, ok := set[ComplianceStatusUnsupported]; !ok {
			t.Errorf("第 4 分類 unsupported が値集合に含まれない")
		}
		// Assert: 4 分類が相互に異なる（Req 3.1）
		if len(set) != 4 {
			t.Errorf("ComplianceStatus は 4 分類であるべき: got %d", len(set))
		}
	})

	t.Run("DeviceMode の 2 定数が期待文字列に一致するとき DB enum device_mode と値集合が一致する", func(t *testing.T) {
		// Arrange: migration 0006 の device_mode ENUM 値と 1:1 対応
		want := []struct {
			got DeviceMode
			exp string
		}{
			{DeviceModeFullyManaged, "fully_managed"},
			{DeviceModeDedicated, "dedicated"},
		}

		// Act / Assert
		for _, m := range want {
			if string(m.got) != m.exp {
				t.Errorf("DeviceMode 値不一致: got %q, want %q", string(m.got), m.exp)
			}
		}
	})
}

func TestDTOJSONEncoding(t *testing.T) {
	t.Run("DeviceDetail の空 jsonb 属性が snake_case キーで {}/[] として出力されるとき Req 2.5 の空属性宣言を満たす", func(t *testing.T) {
		// Arrange: zero-value 近傍。空 jsonb は {} / [] を明示（Service/Repository が空を埋める前提）
		d := DeviceDetail{
			HardwareInfo:         json.RawMessage(`{}`),
			SoftwareInfo:         json.RawMessage(`{}`),
			NonComplianceDetails: json.RawMessage(`[]`),
			InstalledApps:        json.RawMessage(`[]`),
		}

		// Act
		raw, err := json.Marshal(d)
		if err != nil {
			t.Fatalf("json.Marshal 失敗: %v", err)
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("json.Unmarshal 失敗: %v", err)
		}

		// Assert: snake_case キーが揃う（policy.PolicyView 慣習）
		wantKeys := []string{
			"id", "amapi_device_name", "mode", "applied_policy_name",
			"hardware_info", "software_info", "compliance_status",
			"non_compliance_details", "installed_apps", "last_status_at",
			"sync_delayed", "enrolled_at",
		}
		for _, k := range wantKeys {
			if _, ok := got[k]; !ok {
				t.Errorf("DeviceDetail JSON に snake_case キー %q が無い", k)
			}
		}
		// Assert: 空 jsonb が {} / [] として表現される（Req 2.5）
		if string(got["hardware_info"]) != "{}" {
			t.Errorf("hardware_info の空表現が {} でない: got %s", got["hardware_info"])
		}
		if string(got["software_info"]) != "{}" {
			t.Errorf("software_info の空表現が {} でない: got %s", got["software_info"])
		}
		if string(got["non_compliance_details"]) != "[]" {
			t.Errorf("non_compliance_details の空表現が [] でない: got %s", got["non_compliance_details"])
		}
		if string(got["installed_apps"]) != "[]" {
			t.Errorf("installed_apps の空表現が [] でない: got %s", got["installed_apps"])
		}
	})

	t.Run("DeviceSummary が snake_case キーで JSON 出力され nil LastStatusAt が null になるとき一覧 API 応答形状を満たす", func(t *testing.T) {
		// Arrange: zero-value（LastStatusAt は nil pointer）
		s := DeviceSummary{}

		// Act
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("json.Marshal 失敗: %v", err)
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("json.Unmarshal 失敗: %v", err)
		}

		// Assert: snake_case キーが揃う
		wantKeys := []string{"id", "amapi_device_name", "mode", "compliance_status", "last_status_at", "sync_delayed"}
		for _, k := range wantKeys {
			if _, ok := got[k]; !ok {
				t.Errorf("DeviceSummary JSON に snake_case キー %q が無い", k)
			}
		}
		// Assert: nil *time.Time は null として出力される（未同期端末の欠落表現）
		if string(got["last_status_at"]) != "null" {
			t.Errorf("nil LastStatusAt は null であるべき: got %s", got["last_status_at"])
		}
	})
}

func TestZeroValueAndPointerAbsence(t *testing.T) {
	t.Run("ListFilter の zero-value で pointer フィルタが nil（無条件）であるとき既定は全件対象になる", func(t *testing.T) {
		// Arrange / Act
		var f ListFilter

		// Assert
		if f.Compliance != nil {
			t.Errorf("Compliance の zero-value は nil（無条件）であるべき")
		}
		if f.Mode != nil {
			t.Errorf("Mode の zero-value は nil（無条件）であるべき")
		}
		if f.SyncDelayed != nil {
			t.Errorf("SyncDelayed の zero-value は nil（無条件）であるべき")
		}
		if f.Page != 0 || f.PageSize != 0 {
			t.Errorf("Page/PageSize の zero-value は 0（未指定）であるべき: page=%d size=%d", f.Page, f.PageSize)
		}
	})

	t.Run("StatusApplyInput の zero-value で任意フィールドが nil（payload 欠落）であるとき COALESCE 既存値保持の前提を満たす", func(t *testing.T) {
		// Arrange / Act
		var u StatusApplyInput

		// Assert
		if u.AMAPIDeviceName != "" {
			t.Errorf("AMAPIDeviceName の zero-value は空であるべき")
		}
		nilChecks := map[string]bool{
			"LastStatusReportTime": u.LastStatusReportTime == nil,
			"AppliedPolicyName":    u.AppliedPolicyName == nil,
			"ComplianceStatus":     u.ComplianceStatus == nil,
			"NonComplianceDetails": u.NonComplianceDetails == nil,
			"HardwareInfo":         u.HardwareInfo == nil,
			"SoftwareInfo":         u.SoftwareInfo == nil,
			"InstalledApps":        u.InstalledApps == nil,
		}
		for name, isNil := range nilChecks {
			if !isNil {
				t.Errorf("%s の zero-value は nil（payload 欠落）であるべき", name)
			}
		}
	})

	t.Run("DeviceRow の nullable 列が pointer / jsonb 列が json.RawMessage として zero-value で nil のとき scan 契約を満たす", func(t *testing.T) {
		// Arrange / Act
		var r DeviceRow

		// Assert: nullable 列は pointer（NULL → nil）
		if r.AppliedPolicyID != nil {
			t.Errorf("AppliedPolicyID(nullable) の zero-value は nil であるべき")
		}
		if r.AppliedPolicyName != nil {
			t.Errorf("AppliedPolicyName(nullable / migration 0019) の zero-value は nil であるべき")
		}
		if r.LastStatusAt != nil {
			t.Errorf("LastStatusAt(nullable) の zero-value は nil であるべき")
		}
		// Assert: jsonb 列は json.RawMessage
		if r.HardwareInfo != nil || r.InstalledApps != nil {
			t.Errorf("jsonb 列(json.RawMessage) の zero-value は nil であるべき")
		}
	})

	t.Run("TenantOverview.Breakdown が 4 分類 0 埋め map として集計操作可能なとき横断集計応答形状を満たす", func(t *testing.T) {
		// Arrange: 4 分類 0 埋め
		ov := TenantOverview{
			TenantID:    uuid.New(),
			DeviceCount: 0,
			Breakdown: map[ComplianceStatus]int{
				ComplianceStatusCompliant:    0,
				ComplianceStatusNonCompliant: 0,
				ComplianceStatusUnknown:      0,
				ComplianceStatusUnsupported:  0,
			},
		}

		// Act: 集計加算を模す
		ov.Breakdown[ComplianceStatusNonCompliant]++
		ov.DeviceCount++

		// Assert
		if len(ov.Breakdown) != 4 {
			t.Errorf("Breakdown は 4 分類 0 埋めであるべき: got %d", len(ov.Breakdown))
		}
		if ov.Breakdown[ComplianceStatusNonCompliant] != 1 {
			t.Errorf("Breakdown の加算が反映されない")
		}
		if ov.Breakdown[ComplianceStatusCompliant] != 0 {
			t.Errorf("未加算分類は 0 のままであるべき")
		}
	})
}
