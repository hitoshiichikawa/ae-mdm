package notification

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
)

// DeviceStatusWriter は STATUS_REPORT 由来の端末属性更新を device ドメインへ委譲するための
// port（依存逆転 / design.md「Notification Domain > notification.StatusHandler」節）。
//
// notification は device を import しない（doc.go 依存方向不変条件）。実体は device.StatusApplier
// （後続 task 6）が本 IF を実装し、Dispatcher 構築時に structural typing で注入される
// （一方向: device → notification。逆方向 import は発生させない）。
type DeviceStatusWriter interface {
	// ApplyStatusReport は DeviceStatusReport の内容を該当端末（DeviceName 一致）へ部分適用する。
	ApplyStatusReport(ctx context.Context, r DeviceStatusReport) error
}

// DeviceStatusReport は STATUS_REPORT 通知が運ぶ AMAPI Device リソースを、部分更新（Req 7.2）
// 可能な形へパースした値オブジェクト（design.md「notification.StatusHandler」Contracts と一致）。
//
// design.md は本値オブジェクトを StatusReport と表記するが、同 package には既に STATUS_REPORT
// 種別を表す NotificationType 定数 StatusReport（types.go / Issue #39）が存在し名前が衝突する。
// spec 本文は書き換えず、型名のみ DeviceStatusReport へ改名して衝突を回避した（impl-notes.md
// 「確認事項」参照）。
//
// 任意フィールドは pointer / *json.RawMessage で保持し、nil を「payload に当該フィールドが
// 欠落（= 更新しない）」として表現する（Req 7.2）。DeviceName（AMAPI `name`）のみ必須であり、
// 空は不正 payload（更新キー不能 → 破棄 ack / Invariant）。
//
// 各フィールドの JSON tag は AMAPI Device リソースの wire-format（camelCase）に対応させる。
// DeviceName は verifier の接頭辞切り出しをせず、フル resource 名（enterprises/{id}/devices/{id}
// = devices.amapi_device_name 一致キー）をそのまま保持する。InstalledApps の AMAPI field 名
// applicationReports は一次情報未確認のため impl-notes.md「確認事項」を参照。
type DeviceStatusReport struct {
	// DeviceName は AMAPI リソース名（devices.amapi_device_name 一致キー / 必須）。
	DeviceName string `json:"name"`
	// LastStatusReportTime は端末が状態を報告した時刻。nil = payload 欠落（更新しない）。
	LastStatusReportTime *time.Time `json:"lastStatusReportTime"`
	// AppliedPolicyName は端末に適用中のポリシー名（報告値）。nil = 欠落。
	AppliedPolicyName *string `json:"appliedPolicyName"`
	// NonComplianceDetails は非準拠理由。nil = 欠落 / 非 nil（[] 含む）で compliance 再判定対象。
	NonComplianceDetails *json.RawMessage `json:"nonComplianceDetails"`
	// HardwareInfo はハードウェア情報。nil = 欠落。
	HardwareInfo *json.RawMessage `json:"hardwareInfo"`
	// SoftwareInfo はソフトウェア情報。nil = 欠落。
	SoftwareInfo *json.RawMessage `json:"softwareInfo"`
	// InstalledApps はインストール済みアプリ一覧。nil = 欠落。
	InstalledApps *json.RawMessage `json:"applicationReports"`
}

// StatusHandler は STATUS_REPORT 通知の本体処理（NotificationHandler 実装 / Req 7.1・7.2）。
//
// Envelope.Payload（AMAPI Device JSON）を DeviceStatusReport へパースし、DeviceStatusWriter へ 1 回
// dispatch する。空 payload / malformed JSON / DeviceName 空は破棄 ack 相当の
// *errors.Error{IsTransient:false} で分類する（Verifier の破棄 ack 分類方針と整合）。
type StatusHandler struct {
	w   DeviceStatusWriter
	log logger.Logger
}

// NewStatusHandler は StatusHandler を構築する。log が nil の場合は logger.Default()（未配線時は
// no-op）を採用し、DI 未配線でも構造化ログ呼び出しで panic させない（NewVerifier と同方針）。
func NewStatusHandler(w DeviceStatusWriter, log logger.Logger) *StatusHandler {
	if log == nil {
		log = logger.Default()
	}
	return &StatusHandler{w: w, log: log}
}

// Handle は StatusHandler の NotificationHandler 実装。
//
// 前提: Envelope.NotificationType == STATUS_REPORT（Dispatcher が振り分け済み）。tenant ctx は
// Dispatcher が確立済み（本 handler は確立しない）。
//
// 判定順序（防御的に破棄 ack へ倒す）:
//  1. payload が空（長さ 0）→ 破棄 ack（Verifier の空 payload 破棄方針と整合）。
//  2. payload が JSON として解釈できない → 破棄 ack。
//  3. DeviceName（AMAPI name）が空 → 更新キー不能として破棄 ack（Invariant）。
//
// 上記いずれでもなければ w.ApplyStatusReport を 1 回呼び、その戻り error をそのまま返す
// （transient/permanent の再分類はしない。ack/nack 判定は Dispatcher の責務 / design.md
// Postconditions）。機密値（payload 生値）を error 文言・構造化ログに補間しない（NFR 3.1）。
func (h *StatusHandler) Handle(ctx context.Context, env Envelope) error {
	// 空 payload は更新すべき内容が無いため破棄する（Verifier の空 payload 破棄方針と整合）。
	if len(env.Payload) == 0 {
		h.log.Warn("notification: STATUS_REPORT payload is empty; discarding",
			logger.MessageID(env.MessageID))
		return discardError("STATUS_REPORT payload is empty")
	}

	// payload が JSON として解釈できなければ破棄する。エラー文言・ログに payload 生値を補間しない（NFR 3.1）。
	var report DeviceStatusReport
	if err := json.Unmarshal(env.Payload, &report); err != nil {
		h.log.Warn("notification: STATUS_REPORT payload is not valid JSON; discarding",
			logger.MessageID(env.MessageID))
		return discardError("STATUS_REPORT payload validation failed")
	}

	// DeviceName（更新キー）が空なら該当端末を特定できないため破棄する（Invariant）。
	if strings.TrimSpace(report.DeviceName) == "" {
		h.log.Warn("notification: STATUS_REPORT device name is empty; discarding",
			logger.MessageID(env.MessageID))
		return discardError("STATUS_REPORT device name is empty")
	}

	// device ドメインへ部分更新を委譲する（1 回）。戻り error は再分類せず透過する。
	return h.w.ApplyStatusReport(ctx, report)
}
