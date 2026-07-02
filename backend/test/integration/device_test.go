package integration_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/hitoshiichikawa/ae-mdm/internal/device"
	"github.com/hitoshiichikawa/ae-mdm/internal/notification"
)

// 本ファイルは Issue #9 (C1) task 6 の device.StatusApplier（write）に対する integration test。
// test-only 配線で notification.StatusHandler → device.StatusApplier → device.Repository を通し、
// STATUS_REPORT 適用後に device.Service.Get が反映を返す read-after-write（NFR 2.1）と、部分 payload
// での既存値保持（Req 7.1・7.2）を実 DB で検証する。DATABASE_URL 未設定環境では setupDeviceRepo →
// requireDBURLs が t.Skip するため DB 不在でも false-fail しない（helpers_test.go の規約 /
// device_repository_test.go の harness を再利用）。
//
// tenant ctx は本来 Dispatcher が確立するが（design.md Preconditions）、本 test では Dispatcher 相当
// を setupDeviceRepo が返す ctxA（tenant A の TenantContext）で代替する。

// TestDeviceStatusApply_ReadAfterWrite はシナリオ (NFR 2.1 / Req 7.1) 対応。
// STATUS_REPORT payload を StatusHandler へ渡すと StatusApplier → Repository を経て端末属性が
// 更新され、直後の Service.Get が反映後の属性（applied_policy_name / compliance_status /
// last_status_at / hardware_info）を返すことを検証する（read-after-write）。
func TestDeviceStatusApply_ReadAfterWrite(t *testing.T) {
	// Arrange
	repo, pool, ids, ctxA, cleanup := setupDeviceRepo(t)
	defer cleanup()

	target := uuid.New()
	const amapiName = "enterprises/X/devices/APPLY"
	seedDevice(t, ctxA, pool, seedDeviceInput{
		id: target, tenantID: ids.tenantAID, amapiName: amapiName,
		mode: string(device.DeviceModeFullyManaged), compliance: string(device.ComplianceStatusUnknown),
		hardwareInfo: `{"orig":true}`,
	})

	// test-only 配線: StatusHandler → StatusApplier → Repository。
	applier := device.NewStatusApplier(repo, nil)
	handler := notification.NewStatusHandler(applier, nil)

	// STATUS_REPORT payload（AMAPI Device JSON）。name = seed した amapi_device_name。
	statusTime := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	payload := []byte(`{
	  "name": "` + amapiName + `",
	  "lastStatusReportTime": "2026-06-30T12:00:00Z",
	  "appliedPolicyName": "enterprises/X/policies/p1",
	  "nonComplianceDetails": [{"settingName":"passwordPolicies"}],
	  "hardwareInfo": {"brand":"Google"},
	  "softwareInfo": {"androidVersion":"14"}
	}`)
	env := notification.Envelope{
		MessageID:        "msg-apply-1",
		NotificationType: notification.StatusReport,
		EnterpriseName:   "enterprises/X",
		Payload:          payload,
		PublishTime:      statusTime,
	}

	// Act
	if err := handler.Handle(ctxA, env); err != nil {
		t.Fatalf("STATUS_REPORT Handle: %v", err)
	}

	// Assert (read-after-write / NFR 2.1)
	svc := device.NewService(repo, device.SystemClock{}, 24)
	got, err := svc.Get(ctxA, ids.tenantAID, target)
	if err != nil {
		t.Fatalf("Service.Get: %v", err)
	}
	if got.AppliedPolicyName != "enterprises/X/policies/p1" {
		t.Errorf("applied_policy_name が反映されていない: %q", got.AppliedPolicyName)
	}
	if got.ComplianceStatus != device.ComplianceStatusNonCompliant {
		t.Errorf("compliance_status = %q; want non_compliant（非空 non_compliance_details）", got.ComplianceStatus)
	}
	if got.LastStatusAt == nil || !got.LastStatusAt.Equal(statusTime) {
		t.Errorf("last_status_at が反映されていない: %v; want %v", got.LastStatusAt, statusTime)
	}
	if !jsonEqual(t, got.HardwareInfo, `{"brand":"Google"}`) {
		t.Errorf("hardware_info が反映されていない: %s", got.HardwareInfo)
	}
}

// TestDeviceStatusApply_PartialPayloadPreservesExisting はシナリオ (Req 7.1 / 7.2) 対応。
// 一部フィールドのみ含む STATUS_REPORT を適用しても、payload に含まれないフィールドの既存値が
// COALESCE 部分更新で破壊されないことを検証する。
func TestDeviceStatusApply_PartialPayloadPreservesExisting(t *testing.T) {
	// Arrange: 非既定の初期値を持つ device を投入する（保持を確認するため）。
	repo, pool, ids, ctxA, cleanup := setupDeviceRepo(t)
	defer cleanup()

	target := uuid.New()
	const amapiName = "enterprises/X/devices/APPLY2"
	origPolicy := "orig-policy"
	origLast := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seedDevice(t, ctxA, pool, seedDeviceInput{
		id: target, tenantID: ids.tenantAID, amapiName: amapiName,
		mode: string(device.DeviceModeFullyManaged), compliance: string(device.ComplianceStatusNonCompliant),
		lastStatusAt: &origLast, appliedPolicyName: &origPolicy,
		hardwareInfo: `{"orig":true}`, nonComplianceDetails: `[{"r":"old"}]`,
	})

	applier := device.NewStatusApplier(repo, nil)
	handler := notification.NewStatusHandler(applier, nil)

	// 部分 payload: softwareInfo のみ更新（他フィールドは欠落 → 既存値保持）。
	payload := []byte(`{"name":"` + amapiName + `","softwareInfo":{"androidVersion":"15"}}`)
	env := notification.Envelope{
		MessageID:        "msg-apply-partial",
		NotificationType: notification.StatusReport,
		EnterpriseName:   "enterprises/X",
		Payload:          payload,
		PublishTime:      time.Now(),
	}

	// Act
	if err := handler.Handle(ctxA, env); err != nil {
		t.Fatalf("STATUS_REPORT Handle(partial): %v", err)
	}

	// Assert
	svc := device.NewService(repo, device.SystemClock{}, 24)
	got, err := svc.Get(ctxA, ids.tenantAID, target)
	if err != nil {
		t.Fatalf("Service.Get: %v", err)
	}
	// 渡したフィールドは更新される。
	if !jsonEqual(t, got.SoftwareInfo, `{"androidVersion":"15"}`) {
		t.Errorf("software_info が更新されていない: %s", got.SoftwareInfo)
	}
	// 欠落フィールドは既存値保持（COALESCE 部分更新 / Req 7.2）。
	if got.AppliedPolicyName != origPolicy {
		t.Errorf("applied_policy_name が保持されていない: %q; want %q", got.AppliedPolicyName, origPolicy)
	}
	if got.ComplianceStatus != device.ComplianceStatusNonCompliant {
		t.Errorf("compliance_status が保持されていない: %q; want non_compliant", got.ComplianceStatus)
	}
	if got.LastStatusAt == nil || !got.LastStatusAt.Equal(origLast) {
		t.Errorf("last_status_at が保持されていない: %v; want %v", got.LastStatusAt, origLast)
	}
	if !jsonEqual(t, got.HardwareInfo, `{"orig":true}`) {
		t.Errorf("hardware_info が保持されていない: %s", got.HardwareInfo)
	}
	if !jsonEqual(t, got.NonComplianceDetails, `[{"r":"old"}]`) {
		t.Errorf("non_compliance_details が保持されていない: %s", got.NonComplianceDetails)
	}
}
