package notification

import (
	"context"
	stderrors "errors"
	"testing"
	"time"
)

// fakeStatusWriter は DeviceStatusWriter の呼び出しを記録するテスト spy。
type fakeStatusWriter struct {
	calls   int                // ApplyStatusReport の呼び出し回数
	last    DeviceStatusReport // 直近に受領した DeviceStatusReport
	returns error              // ApplyStatusReport が返すエラー（透過確認用）
}

func (f *fakeStatusWriter) ApplyStatusReport(_ context.Context, r DeviceStatusReport) error {
	f.calls++
	f.last = r
	return f.returns
}

// fullStatusReportPayload は全フィールドを持つ STATUS_REPORT の AMAPI Device JSON（camelCase）。
const fullStatusReportPayload = `{
  "name": "enterprises/LC0123abcd/devices/dev-1",
  "lastStatusReportTime": "2026-06-29T12:00:00Z",
  "appliedPolicyName": "enterprises/LC0123abcd/policies/policy-1",
  "nonComplianceDetails": [{"settingName":"passwordPolicies"}],
  "hardwareInfo": {"brand":"Google","model":"Pixel"},
  "softwareInfo": {"androidVersion":"14"},
  "applicationReports": [{"packageName":"com.example.app"}]
}`

// newStatusEnvelope は STATUS_REPORT の Envelope をインラインで組み立てるテスト helper。
func newStatusEnvelope(id string, payload []byte) Envelope {
	return Envelope{
		MessageID:        id,
		NotificationType: StatusReport,
		EnterpriseName:   "enterprises/LC0123abcd",
		Payload:          payload,
		PublishTime:      time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC),
	}
}

// TestHandle_FullPayloadMapsAllFields は全フィールドを持つ STATUS_REPORT payload が
// StatusReport へ写像され、ApplyStatusReport が 1 回だけ呼ばれること、DeviceName（AMAPI name）
// と各 optional pointer が非 nil で伝播することを検証する（Req 7.1）。
func TestHandle_FullPayloadMapsAllFields(t *testing.T) {
	// Arrange
	spy := &fakeStatusWriter{}
	h := NewStatusHandler(spy, nil)
	env := newStatusEnvelope("msg-full", []byte(fullStatusReportPayload))

	// Act
	err := h.Handle(context.Background(), env)

	// Assert
	if err != nil {
		t.Fatalf("正常 payload で error を返すべきでない: %v", err)
	}
	if spy.calls != 1 {
		t.Fatalf("ApplyStatusReport は 1 回呼ばれるべき: got %d", spy.calls)
	}
	got := spy.last
	if got.DeviceName != "enterprises/LC0123abcd/devices/dev-1" {
		t.Errorf("DeviceName は AMAPI name をそのまま保持すべき: got %q", got.DeviceName)
	}
	wantTime := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	if got.LastStatusReportTime == nil || !got.LastStatusReportTime.Equal(wantTime) {
		t.Errorf("LastStatusReportTime mismatch: got %v, want %v", got.LastStatusReportTime, wantTime)
	}
	if got.AppliedPolicyName == nil || *got.AppliedPolicyName != "enterprises/LC0123abcd/policies/policy-1" {
		t.Errorf("AppliedPolicyName mismatch: got %v", got.AppliedPolicyName)
	}
	if got.NonComplianceDetails == nil {
		t.Error("NonComplianceDetails は非 nil で写像されるべき")
	}
	if got.HardwareInfo == nil {
		t.Error("HardwareInfo は非 nil で写像されるべき")
	}
	if got.SoftwareInfo == nil {
		t.Error("SoftwareInfo は非 nil で写像されるべき")
	}
	if got.InstalledApps == nil {
		t.Error("InstalledApps は非 nil で写像されるべき")
	}
}

// TestHandle_PartialPayloadKeepsMissingFieldsNil は一部フィールドが欠落した payload について、
// 欠落フィールドの pointer が nil のまま（＝更新しない / 破壊しない）ApplyStatusReport が
// 1 回呼ばれ、存在するフィールドのみ非 nil で伝播することを検証する（Req 7.2）。
func TestHandle_PartialPayloadKeepsMissingFieldsNil(t *testing.T) {
	// Arrange: hardwareInfo / nonComplianceDetails / appliedPolicyName / lastStatusReportTime /
	// applicationReports を欠落させ、name + softwareInfo のみ持つ payload。
	spy := &fakeStatusWriter{}
	h := NewStatusHandler(spy, nil)
	payload := []byte(`{"name":"enterprises/LC01/devices/d1","softwareInfo":{"androidVersion":"14"}}`)
	env := newStatusEnvelope("msg-partial", payload)

	// Act
	err := h.Handle(context.Background(), env)

	// Assert
	if err != nil {
		t.Fatalf("部分 payload で error を返すべきでない: %v", err)
	}
	if spy.calls != 1 {
		t.Fatalf("ApplyStatusReport は 1 回呼ばれるべき: got %d", spy.calls)
	}
	got := spy.last
	if got.SoftwareInfo == nil {
		t.Error("存在する softwareInfo は非 nil で伝播すべき")
	}
	if got.HardwareInfo != nil {
		t.Error("欠落した hardwareInfo は nil のままであるべき（更新しない / Req 7.2）")
	}
	if got.NonComplianceDetails != nil {
		t.Error("欠落した nonComplianceDetails は nil のままであるべき（更新しない / Req 7.2）")
	}
	if got.AppliedPolicyName != nil {
		t.Error("欠落した appliedPolicyName は nil のままであるべき（更新しない / Req 7.2）")
	}
	if got.LastStatusReportTime != nil {
		t.Error("欠落した lastStatusReportTime は nil のままであるべき（更新しない / Req 7.2）")
	}
	if got.InstalledApps != nil {
		t.Error("欠落した applicationReports は nil のままであるべき（更新しない / Req 7.2）")
	}
}

// TestHandle_EmptyPayloadIsDiscarded は空 payload（長さ 0 / nil）が破棄 ack 相当で分類され、
// ApplyStatusReport が呼ばれないことを検証する（Req 7.2 failure path / Verifier 分類方針と整合）。
func TestHandle_EmptyPayloadIsDiscarded(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
	}{
		{name: "長さ 0 payload は破棄される", payload: []byte{}},
		{name: "nil payload は破棄される", payload: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			spy := &fakeStatusWriter{}
			h := NewStatusHandler(spy, nil)
			env := newStatusEnvelope("msg-empty", tt.payload)

			// Act
			err := h.Handle(context.Background(), env)

			// Assert
			assertDiscardError(t, err)
			if spy.calls != 0 {
				t.Errorf("破棄時は ApplyStatusReport を呼ぶべきでない: got %d", spy.calls)
			}
		})
	}
}

// TestHandle_MalformedJSONIsDiscarded は JSON として解釈できない payload が破棄 ack 相当で
// 分類され、ApplyStatusReport が呼ばれないことを検証する（Req 7.2 failure path）。
func TestHandle_MalformedJSONIsDiscarded(t *testing.T) {
	// Arrange
	spy := &fakeStatusWriter{}
	h := NewStatusHandler(spy, nil)
	env := newStatusEnvelope("msg-badjson", []byte(`{not-json`))

	// Act
	err := h.Handle(context.Background(), env)

	// Assert
	assertDiscardError(t, err)
	if spy.calls != 0 {
		t.Errorf("破棄時は ApplyStatusReport を呼ぶべきでない: got %d", spy.calls)
	}
}

// TestHandle_EmptyDeviceNameIsDiscarded は更新キーとなる name（DeviceName）が抽出できない
// payload（欠落 / 空文字 / 空白のみ）が破棄 ack 相当で分類され、ApplyStatusReport が
// 呼ばれないことを検証する（Invariant: DeviceName 空は更新キー不能 / Req 7.2 failure path）。
func TestHandle_EmptyDeviceNameIsDiscarded(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{name: "name field 不在のとき破棄される", payload: `{"softwareInfo":{"androidVersion":"14"}}`},
		{name: "name が空文字のとき破棄される", payload: `{"name":""}`},
		{name: "name が空白のみのとき破棄される", payload: `{"name":"   "}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			spy := &fakeStatusWriter{}
			h := NewStatusHandler(spy, nil)
			env := newStatusEnvelope("msg-noname", []byte(tt.payload))

			// Act
			err := h.Handle(context.Background(), env)

			// Assert
			assertDiscardError(t, err)
			if spy.calls != 0 {
				t.Errorf("破棄時は ApplyStatusReport を呼ぶべきでない: got %d", spy.calls)
			}
		})
	}
}

// TestHandle_WriterErrorIsPropagated は ApplyStatusReport が返したエラーを、StatusHandler が
// transient/permanent を再分類せずそのまま透過することを検証する（Req 7.1 の dispatch 契約 /
// ack-nack 判定は Dispatcher の責務）。
func TestHandle_WriterErrorIsPropagated(t *testing.T) {
	// Arrange
	sentinel := stderrors.New("writer failed")
	spy := &fakeStatusWriter{returns: sentinel}
	h := NewStatusHandler(spy, nil)
	env := newStatusEnvelope("msg-writererr", []byte(fullStatusReportPayload))

	// Act
	err := h.Handle(context.Background(), env)

	// Assert
	if spy.calls != 1 {
		t.Fatalf("ApplyStatusReport は 1 回呼ばれるべき: got %d", spy.calls)
	}
	if !stderrors.Is(err, sentinel) {
		t.Errorf("writer のエラーは再分類せず透過すべき: got %v", err)
	}
}
