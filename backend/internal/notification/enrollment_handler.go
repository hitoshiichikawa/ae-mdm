package notification

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
)

// compliance_status の値域（migration 0006 の compliance_status ENUM に対応 / Req 3.5 / NFR 1.1）。
const (
	// complianceUnknown は判定不能 / Android 10 以上の既定 compliance（登録は継続する）。
	complianceUnknown = "unknown"
	// complianceUnsupported は Android 10 未満で管理対象にできない端末の compliance（Req 3.5）。
	complianceUnsupported = "unsupported"
	// minSupportedAndroidMajor はサポートする Android major version の下限（NFR 1.1）。
	minSupportedAndroidMajor = 10
)

// 退避理由（quarantine_reason）の非機密 enum ラベル。構造化 WARN の field にのみ用い、payload 生値・
// additionalData 生値は補間しない（NFR 3.1 / NFR 4.1）。
const (
	// quarantineReasonTenantMismatch は additionalData.tenant_id と enterprise 由来テナントの不一致（Req 3.2）。
	quarantineReasonTenantMismatch = "tenant_mismatch"
	// quarantineReasonTenantMissing は additionalData の tenant_id 欠落 / parse 不能（Req 3.3）。
	quarantineReasonTenantMissing = "tenant_missing"
	// quarantineReasonTenantCtxMissing は ctx に tenant context が未確立（安全側で退避 / NFR 2.2）。
	quarantineReasonTenantCtxMissing = "tenant_ctx_missing"
	// quarantineReasonPayloadInvalid は payload 本体が JSON として解釈できない（安全側で退避 / NFR 2.2）。
	quarantineReasonPayloadInvalid = "payload_invalid"
	// quarantineReasonDeviceNameInvalid は payload.name が AMAPI device リソース名の形式でない
	// （実端末でない junk 登録を避けて安全側で退避 / NFR 2.2）。
	quarantineReasonDeviceNameInvalid = "device_name_invalid"
	// quarantineReasonModeInvalid は additionalData.mode が devices.mode enum の値域外（毒メッセージ防止）。
	// そのまま upsert すると enum への INSERT が DB エラー→transient 扱いで再処理ループになるため退避する
	// （Req 3.6 は「再試行で回復しうる」失敗のみ保持を要求するため、非回復な不正値は退避へ倒す）。
	quarantineReasonModeInvalid = "mode_invalid"
)

// devices.mode enum（migration 0006 の device_mode）の値域。notification は enrollment パッケージを
// import しない（doc.go 依存方向規約）ため、突合に必要な enum ラベルのみを最小に複製する。
const (
	deviceModeFullyManaged = "fully_managed"
	deviceModeDedicated    = "dedicated"
)

// EnrollmentRegistrar は ENROLLMENT 通知で確定した端末を発行元テナントの端末インベントリへ
// 冪等 upsert する consumer-defined port（primitive 型 / TenantResolver を手本）。
//
// enrollment.Registrar.UpsertEnrolledDevice が structural typing で本 IF を満たす。notification は
// enrollment パッケージを直接 import せず（doc.go 依存方向ルール）、Dispatcher / cmd 配線時に本 IF
// 経由で注入を受けることでドメイン所有権境界を維持する（design.md「Notification Domain」節）。
type EnrollmentRegistrar interface {
	// UpsertEnrolledDevice は amapi_device_name をキーに、発行元テナントの devices を冪等 upsert する。
	// transient な DB 失敗は IsTransient=true の error で返し、Dispatcher の nack（再処理保持 / Req 3.6）へ写像する。
	UpsertEnrolledDevice(ctx context.Context, amapiDeviceName, mode, complianceStatus string) error
}

// enrollmentPayload は ENROLLMENT 通知の Device payload から突合・登録に必要な最小 field を抽出する
// 構造（verifier.amapiResource と同レベルの wire-format 仮定 / design.md リスク 1）。
type enrollmentPayload struct {
	// Name は AMAPI リソース名（`enterprises/{ent}/devices/{dev}`）。amapi_device_name として使う。
	Name string `json:"name"`
	// EnrollmentTokenData は発行時 additionalData（JSON 文字列）が回送される field（design.md リスク 1）。
	EnrollmentTokenData string `json:"enrollmentTokenData"`
	// SoftwareInfo は端末ソフトウェア情報。androidVersion のみを参照する（Req 3.5 / NFR 1.1）。
	SoftwareInfo struct {
		AndroidVersion string `json:"androidVersion"`
	} `json:"softwareInfo"`
}

// enrollmentAdditionalData は発行時 additionalData（`{"tenant_id","issued_by","mode"}`）を突合するための
// 最小 field。enrollment.AdditionalData の wire-format を notification 側で独自 parse する（enrollment
// パッケージは import しない / doc.go 依存方向ルール）。
type enrollmentAdditionalData struct {
	// TenantID は発行元テナント（enterprise 由来テナントとの突合キー / Req 3.1 / 3.2）。
	TenantID string `json:"tenant_id"`
	// Mode は端末モード（devices.mode の enum ラベル。fully_managed / dedicated）。
	Mode string `json:"mode"`
}

// enrollmentHandler は ENROLLMENT 通知の additionalData 突合と、登録 / 未割当退避の振り分けを担う
// NotificationHandler 実装（design.md「notification.EnrollmentNotificationHandler」節）。
type enrollmentHandler struct {
	registrar  EnrollmentRegistrar
	unassigned UnassignedQueue
	log        logger.Logger
}

// NewEnrollmentHandler は ENROLLMENT 通知のドメインハンドラを構築する（design.md L287）。
//
// registrar は enrollment.Registrar（primitive 型ポートを structural typing で満たす）、unassigned は
// 退避キュー、log は構造化ログ。log が nil の場合は logger.Default()（未配線時は no-op）を採り、DI 未配線
// でも構造化ログ呼び出しで panic させない（Dispatcher / Verifier と同方針）。
func NewEnrollmentHandler(registrar EnrollmentRegistrar, unassigned UnassignedQueue, log logger.Logger) NotificationHandler {
	if log == nil {
		log = logger.Default()
	}
	return &enrollmentHandler{registrar: registrar, unassigned: unassigned, log: log}
}

// Handle は ENROLLMENT 通知を処理する（design.md「Responsibilities & Constraints」手順 1〜4）。
//
// 手順: payload parse → additionalData parse（tenant_id 欠落 / parse 不能→退避 / Req 3.3）→
// ctx tenant context と突合（不一致 / ctx 未確立→退避 / Req 3.2 / NFR 2.2）→ 一致時のみ登録（Req 3.1）。
// 登録直前に payload.name / additionalData.mode の妥当性を検証し、不正なら安全側で退避する（毒メッセージ・
// junk 登録の防止 / register 参照）。退避は UnassignedQueue.Enqueue を自ら呼び nil（ack）を返す
// （design 採用案 / Dispatcher 無改変）。transient DB 失敗はそのまま返し nack 保持（Req 3.6）。機密値
// （payload 生値・additionalData 生値）は error 文言・構造化ログに補間しない（NFR 3.1）。
func (h *enrollmentHandler) Handle(ctx context.Context, env Envelope) error {
	payload, ok := parsePayload(env.Payload)
	if !ok {
		// payload 本体が JSON 不正 = テナント特定不能。安全側で退避する（NFR 2.2）。payload 未 parse の
		// ため device name は不明（空）で退避する。
		return h.quarantine(ctx, env, "", quarantineReasonPayloadInvalid)
	}
	// payload から特定できた対象端末名。以降の退避 / 登録の構造化 WARN に載せて事後追跡可能にする（NFR 4.1）。
	deviceName := strings.TrimSpace(payload.Name)

	add, ok := parseAdditionalData(payload.EnrollmentTokenData)
	if !ok || strings.TrimSpace(add.TenantID) == "" {
		// additionalData の tenant_id 欠落 / parse 不能は退避（Req 3.3）。
		return h.quarantine(ctx, env, deviceName, quarantineReasonTenantMissing)
	}

	tc, err := db.FromContext(ctx)
	if err != nil {
		// ctx tenant context 未確立は安全側で退避（NFR 2.2）。DB へ触れない。
		return h.quarantine(ctx, env, deviceName, quarantineReasonTenantCtxMissing)
	}

	if !tenantMatches(tc.TenantID, add.TenantID) {
		// additionalData.tenant_id と enterprise 由来テナントの不一致は退避（Req 3.2 / NFR 2.2）。
		return h.quarantine(ctx, env, deviceName, quarantineReasonTenantMismatch)
	}

	// 突合一致時のみ発行元テナントへ登録する（Req 3.1 / NFR 2.1）。
	return h.register(ctx, env, deviceName, payload, add)
}

// register は突合一致した端末を発行元テナントの端末インベントリへ冪等 upsert する（Req 3.1 / 3.5）。
//
// 登録前に payload.name / additionalData.mode の妥当性を検証する:
//   - name が AMAPI device リソース名（enterprises/{ent}/devices/{dev}）の形式でない場合、実端末を特定
//     できない不正 payload として退避する（enterprise_name だけ解決できる payload での junk 登録の防止 / NFR 2.2）。
//   - mode が devices.mode enum（fully_managed/dedicated）の値域外の場合、そのまま upsert すると enum への
//     INSERT が DB エラー→transient 扱いで無限に再処理される毒メッセージになるため退避する（Req 3.6 は
//     「再試行で回復しうる」失敗のみ保持を要求する。非回復な不正値を nack ループに載せない）。
//
// androidVersion の major が 10 未満なら compliance=unsupported（サポート対象外として残す / Req 3.5）、
// それ以外（10 以上 / 判定不能）は unknown。transient な登録失敗はそのまま返し nack 保持（Req 3.6）。
func (h *enrollmentHandler) register(ctx context.Context, env Envelope, deviceName string, payload enrollmentPayload, add enrollmentAdditionalData) error {
	if !isValidDeviceName(deviceName) {
		// payload.name が AMAPI device リソース名の形式でない = 実端末を特定できない。安全側で退避（NFR 2.2）。
		return h.quarantine(ctx, env, deviceName, quarantineReasonDeviceNameInvalid)
	}
	if !isValidDeviceMode(add.Mode) {
		// mode が devices.mode enum 値でない = 毒メッセージ。DB enum エラーによる再処理ループを避けて退避する。
		return h.quarantine(ctx, env, deviceName, quarantineReasonModeInvalid)
	}

	compliance := complianceForAndroidVersion(payload.SoftwareInfo.AndroidVersion)

	if err := h.registrar.UpsertEnrolledDevice(ctx, deviceName, add.Mode, compliance); err != nil {
		// transient DB 失敗はそのまま返し Dispatcher の nack（再処理保持 / Req 3.6）へ写像する。対象端末名を
		// 載せて登録失敗を事後追跡可能にする（NFR 4.1）。
		h.log.Warn("notification: enrolled device registration failed; will retry",
			enrollmentWarnFields(env, deviceName, "failure_kind", "registration_failed")...)
		return err
	}

	if compliance == complianceUnsupported {
		// サポート対象外端末を残した旨を運用者が対象端末とともに追跡できるよう構造化 WARN（NFR 4.1）。
		h.log.Warn("notification: enrolled device recorded as unsupported platform",
			enrollmentWarnFields(env, deviceName,
				"compliance_status", complianceUnsupported,
				"failure_kind", "unsupported_platform")...)
	}
	return nil
}

// quarantine は通知を未割当キューへ退避し ack（nil）を返す（Req 3.2 / 3.3 / NFR 2.2）。
//
// UnassignedQueue.Enqueue は内部で SuperAdmin context を確立し dedupe 記録 + 退避 INSERT を同一 tx で
// 冪等実行するため、ctx tenant context が未確立でも退避できる（unassigned.go）。退避の transient DB
// 失敗はそのまま返し nack 保持（Req 3.6）。退避理由は非機密 enum ラベルで構造化 WARN（NFR 4.1）し、
// deviceName が特定できていれば対象端末として併記する（payload 未 parse 時は空で省略）。payload 生値・
// additionalData 生値は補間しない（NFR 3.1）。
func (h *enrollmentHandler) quarantine(ctx context.Context, env Envelope, deviceName, reason string) error {
	if err := h.unassigned.Enqueue(ctx, env); err != nil {
		h.log.Warn("notification: enrollment quarantine enqueue failed; will retry",
			enrollmentWarnFields(env, deviceName,
				"quarantine_reason", reason,
				"failure_kind", "quarantine_enqueue_failed")...)
		return err
	}
	h.log.Warn("notification: enrollment notification quarantined to unassigned queue",
		enrollmentWarnFields(env, deviceName,
			"quarantine_reason", reason,
			"failure_kind", "quarantined")...)
	return nil
}

// enrollmentWarnFields は enrollment 通知処理の構造化 WARN 共通 field を組み立てる（NFR 4.1）。
//
// message_id / enterprise_name / notification_type の非機密 field を常に載せ、対象端末名
// （amapi_device_name）は payload から特定できた場合のみ載せる（payload 不正で未特定なら省略）。
// amapi_device_name は AMAPI リソース名であり秘密値ではない（NFR 3.1 の秘密値は Value / QRCode のみ）。
// extra には呼び出しごとの追加 key/value ペアを渡す。
func enrollmentWarnFields(env Envelope, deviceName string, extra ...any) []any {
	fields := []any{
		logger.MessageID(env.MessageID),
		"enterprise_name", env.EnterpriseName,
		"notification_type", string(env.NotificationType),
	}
	if deviceName != "" {
		fields = append(fields, "amapi_device_name", deviceName)
	}
	return append(fields, extra...)
}

// isValidDeviceMode は additionalData.mode が devices.mode enum（fully_managed / dedicated）の値域内かを
// 判定する純粋関数。値域外（空文字含む）は false を返し、呼び出し側で毒メッセージとして退避へ倒す。
func isValidDeviceMode(mode string) bool {
	return mode == deviceModeFullyManaged || mode == deviceModeDedicated
}

// isValidDeviceName は payload.name が AMAPI device リソース名（enterprises/{ent}/devices/{dev}）の形式かを
// 判定する純粋関数。4 セグメント かつ 各識別子が非空 の場合のみ true を返す。空文字・enterprise だけ・
// device セグメント欠落等は false を返し、実端末でない junk 登録を防ぐ（NFR 2.2）。
func isValidDeviceName(name string) bool {
	parts := strings.Split(name, "/")
	if len(parts) != 4 {
		return false
	}
	return parts[0] == "enterprises" && parts[1] != "" && parts[2] == "devices" && parts[3] != ""
}

// parsePayload は env.Payload を enrollmentPayload へ unmarshal する。JSON 不正なら ok=false を返す
// （呼び出し側が安全側で退避する / NFR 2.2）。機密値を error として持ち出さないため error は握り潰す。
func parsePayload(raw []byte) (enrollmentPayload, bool) {
	var p enrollmentPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return enrollmentPayload{}, false
	}
	return p, true
}

// parseAdditionalData は発行時 additionalData の JSON 文字列を parse する（Req 3.3）。
//
// 空文字 / JSON 不正なら ok=false を返す（呼び出し側が退避する）。機密値を error として持ち出さない
// ため error は握り潰す（NFR 3.1）。
func parseAdditionalData(raw string) (enrollmentAdditionalData, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return enrollmentAdditionalData{}, false
	}
	var a enrollmentAdditionalData
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return enrollmentAdditionalData{}, false
	}
	return a, true
}

// tenantMatches は additionalData の tenant_id 文字列を uuid として parse し、ctx 由来テナントと突合する。
//
// parse 不能（欠落は呼び出し側で先行判定済み）な tenant_id は突合不能として false を返し、呼び出し側で
// 安全側の退避へ倒す（Req 3.2 / NFR 2.2）。
func tenantMatches(ctxTenantID uuid.UUID, additionalTenantID string) bool {
	parsed, err := uuid.Parse(strings.TrimSpace(additionalTenantID))
	if err != nil {
		return false
	}
	return parsed == ctxTenantID
}

// complianceForAndroidVersion は androidVersion 文字列から compliance_status を決定する純粋関数
// （Req 3.5 / NFR 1.1）。
//
// major が 10 未満なら unsupported（サポート対象外）。10 以上 / 判定不能はいずれも unknown とし、端末を
// 登録して可視化する（安全側 = 判定不能を理由に登録を落とさない）。
func complianceForAndroidVersion(androidVersion string) string {
	if major, ok := androidMajorVersion(androidVersion); ok && major < minSupportedAndroidMajor {
		return complianceUnsupported
	}
	return complianceUnknown
}

// androidMajorVersion は androidVersion 文字列（"9" / "10" / "11.0.0" 等）から major 部を数値抽出する。
//
// 空文字 / 非数値は ok=false を返し、呼び出し側で unknown フォールバックへ倒す。
func androidMajorVersion(v string) (int, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	majorPart := v
	if idx := strings.IndexByte(v, '.'); idx >= 0 {
		majorPart = v[:idx]
	}
	major, err := strconv.Atoi(strings.TrimSpace(majorPart))
	if err != nil {
		return 0, false
	}
	return major, true
}

// enrollmentHandler が NotificationHandler を満たすことをコンパイル時に保証する（Dispatcher への注入互換）。
var _ NotificationHandler = (*enrollmentHandler)(nil)
