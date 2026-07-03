package device

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/notification"
)

// StatusApplier は STATUS_REPORT 通知を契機とした端末属性の部分更新を担う唯一の書込み経路
// （read 系 Service とは別型 / design.md「device.StatusApplier（write）」節 / Req 7.1・7.2・3.2）。
//
// notification.DeviceStatusWriter port を実装し（device → notification の一方向依存。notification は
// device を import しないため循環しない / doc.go 依存方向）、notification.DeviceStatusReport を
// StatusApplyInput へ写像して Repository.UpdateFromStatusReport（COALESCE 部分更新）へ委譲する。
type StatusApplier struct {
	repo Repository
	log  logger.Logger
}

// NewStatusApplier は StatusApplier を構築する。log が nil の場合は logger.Default()（未配線時は
// no-op）を採用し、DI 未配線でも構造化ログ呼び出しで panic させない（notification.NewStatusHandler
// と同じ nil-safe 方針）。
func NewStatusApplier(repo Repository, log logger.Logger) *StatusApplier {
	if log == nil {
		log = logger.Default()
	}
	return &StatusApplier{repo: repo, log: log}
}

// ApplyStatusReport は notification.DeviceStatusWriter の実装。
//
// notification.DeviceStatusReport を StatusApplyInput へ写像し、Repository.UpdateFromStatusReport へ
// 委譲する。任意フィールドは pointer / *json.RawMessage をそのまま渡し、nil（payload 欠落）は
// COALESCE で既存値保持される（Req 7.2）。compliance_status は PolicyCompliant（AMAPI Device の
// authoritative な boolean）または NonComplianceDetails のいずれかが payload に存在するときに
// 算出・更新する（両方欠落なら未更新 / Req 3.2・7.1・7.2）。non_compliance_details 列は
// NonComplianceDetails が存在するときのみ更新する（欠落時は COALESCE で既存値保持）。
// unsupported は本経路で書き込まない（Open Questions / design.md L285）。
//
// Repository の戻り error は再分類せず透過する（ack/nack 判定は Dispatcher の責務 / design.md
// Postconditions）。affected=0（未登録端末 / ENROLLMENT 未処理）は error に倒さず nil を返して
// 完了扱いとし、構造化 WARN を残す（Open Questions / design.md L288）。ログには payload 生値・
// hardware/software info の生値を補間せず、端末識別子 amapi_device_name のみ載せる（NFR 3.1）。
func (a *StatusApplier) ApplyStatusReport(ctx context.Context, r notification.DeviceStatusReport) error {
	input := StatusApplyInput{
		AMAPIDeviceName:      r.DeviceName,
		LastStatusReportTime: r.LastStatusReportTime,
		AppliedPolicyName:    r.AppliedPolicyName,
		HardwareInfo:         r.HardwareInfo,
		SoftwareInfo:         r.SoftwareInfo,
		InstalledApps:        r.InstalledApps,
	}
	// PolicyCompliant または NonComplianceDetails が payload に存在する時のみ compliance を算出して
	// 更新する（Req 3.2・7.1）。両方欠落なら compliance_status は未更新（COALESCE で既存値保持 / Req 7.2）。
	// non_compliance_details 列は payload に存在する時のみ渡す（欠落時は nil で既存値保持）。
	if r.PolicyCompliant != nil || r.NonComplianceDetails != nil {
		input.NonComplianceDetails = r.NonComplianceDetails
		status := deriveComplianceStatus(r.PolicyCompliant, r.NonComplianceDetails)
		input.ComplianceStatus = &status
	}

	affected, err := a.repo.UpdateFromStatusReport(ctx, input)
	if err != nil {
		return err
	}
	if affected == 0 {
		// 未登録端末（ENROLLMENT 未処理）は error に倒さず no-op ack。payload 生値は補間しない（NFR 3.1）。
		a.log.Warn("device: STATUS_REPORT の対象端末が未登録のため no-op ack",
			"amapi_device_name", r.DeviceName)
	}
	return nil
}

// deriveComplianceStatus は payload に存在する compliance シグナル（policyCompliant / 非準拠理由）
// からコンプライアンス分類を導出する（Req 3.2・7.1）。呼び出し側は policyCompliant または
// nonComplianceDetails のいずれかが存在するときのみ本関数を呼ぶ。
//
// 判定順（安全側 non_compliant を優先）:
//  1. policyCompliant:false は AMAPI Device の authoritative な非準拠シグナル → non_compliant。
//  2. 非空 nonComplianceDetails も非準拠を示す（policyCompliant:true と矛盾しても未検知の非準拠を
//     見逃さないため安全側 non_compliant に倒す）→ non_compliant。
//  3. 上記いずれでもない（policyCompliant:true、または空 nonComplianceDetails）→ compliant。
//
// unsupported は本経路で導出しない（read の第 4 分類 / 書込み契機は Open Questions）。
func deriveComplianceStatus(policyCompliant *bool, details *json.RawMessage) ComplianceStatus {
	if policyCompliant != nil && !*policyCompliant {
		return ComplianceStatusNonCompliant
	}
	if details != nil && hasNonComplianceDetails(*details) {
		return ComplianceStatusNonCompliant
	}
	return ComplianceStatusCompliant
}

// hasNonComplianceDetails は非準拠理由 jsonb が「非空の非準拠理由を持つ」かを判定する。
//
// 判定は JSON array として解釈した長さで行う。空配列 / 空白のみ / JSON null は false（非準拠理由
// なし）。AMAPI 契約上 array のはずだが array として解釈できない異常系は、安全側に倒して true
// （非準拠あり扱い）を既定とする（未検知の非準拠を見逃さないため / impl-notes 参照）。
func hasNonComplianceDetails(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return false
	}
	var details []json.RawMessage
	if err := json.Unmarshal(trimmed, &details); err != nil {
		return true
	}
	return len(details) > 0
}

// 型 assertion: StatusApplier が notification.DeviceStatusWriter を満たすことを compile-time で確認する。
var _ notification.DeviceStatusWriter = (*StatusApplier)(nil)
