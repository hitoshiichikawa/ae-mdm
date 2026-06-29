package notification

import (
	"encoding/json"
	"strings"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/pubsub"
)

// notificationTypeAttribute は AMAPI が Pub/Sub メッセージの attribute に種別を載せる際の
// key 名（AMAPI Pub/Sub notification の wire-format。impl-notes「確認事項」参照）。
const notificationTypeAttribute = "notificationType"

// enterpriseNameResourcePrefix は AMAPI リソース名（payload JSON の `name` field）が取る
// 接頭辞。`enterprises/{enterpriseId}/...` から `enterprises/{enterpriseId}` を切り出す。
const enterpriseNameResourcePrefix = "enterprises/"

// Verifier は *pubsub.Message を Envelope へパース・検証する抽象（design.md「Verifier」節）。
//
// 空 payload（Req 2.6）/ 種別判定不能（Req 2.6）/ 検証失敗（Req 2.5）は破棄 ack 相当の
// *errors.Error{IsTransient:false} で分類して返す（ShouldAck が ack に倒す）。
type Verifier interface {
	// Parse は *pubsub.Message を検証して Envelope を構築する。破棄相当の失敗は
	// *errors.Error{IsTransient:false} で分類して返す。
	Parse(msg *pubsub.Message) (Envelope, error)
}

// messageVerifier は Verifier の本番実装。AMAPI Pub/Sub notification の wire-format
// （種別は attribute、enterprise_name は payload JSON のリソース名接頭辞から抽出）を解釈する。
type messageVerifier struct {
	log logger.Logger
}

// NewVerifier は Verifier を構築する。log が nil の場合は logger.Default()（未配線時は
// no-op）を採用し、DI 未配線でも構造化ログ呼び出しで panic させない（tenant.NewService と同方針）。
func NewVerifier(log logger.Logger) Verifier {
	if log == nil {
		log = logger.Default()
	}
	return &messageVerifier{log: log}
}

// amapiResource は payload JSON から enterprise_name を抽出するための最小構造。
//
// AMAPI 通知の payload はリソース（Device 等）を JSON 直列化したものであり、`name` field が
// `enterprises/{enterpriseId}/devices/{deviceId}` 形式を取る。enterprise_name は当該 name の
// `enterprises/{enterpriseId}` 接頭辞である（wire-format 解釈。impl-notes「確認事項」参照）。
type amapiResource struct {
	Name string `json:"name"`
}

// Parse は messageVerifier の Verifier 実装。
//
// 判定順序:
//  1. msg が nil → 検証失敗（Req 2.5）。
//  2. payload（msg.Data）が空（nil / 長さ 0）→ 破棄（Req 2.6）。
//  3. attribute から種別を抽出できない（key 欠落 / 空値）→ 種別判定不能（Req 2.6）。
//  4. payload が JSON として解釈できない → 検証失敗（Req 2.5）。
//
// enterprise_name は payload JSON のリソース名接頭辞から抽出する。抽出できない（name 欠落 /
// 接頭辞不一致）場合でもエラーにせず Envelope 内で空文字のまま通す（退避判定は Dispatcher の
// 責務 / Req 3.4）。
//
// 機密値（payload 生値・token 等）は error 文言・構造化ログに補間しない（NFR 3.1）。ログは
// message_id（logger.MessageID）に限定し payload を埋め込まない。
func (v *messageVerifier) Parse(msg *pubsub.Message) (Envelope, error) {
	if msg == nil {
		// 受信基盤の不変条件違反だが、破棄 ack（恒常的失敗 / 再配信しても結果が変わらない）に倒す。
		v.log.Warn("notification: nil message received; discarding")
		return Envelope{}, discardError("notification message is nil")
	}

	// 空 payload は種別別 dispatch を実行せず破棄する（Req 2.6）。
	if len(msg.Data) == 0 {
		v.log.Warn("notification: empty payload; discarding", logger.MessageID(msg.ID))
		return Envelope{}, discardError("notification payload is empty")
	}

	// 種別は attribute から抽出する（AMAPI wire-format）。key 欠落 / 空値は種別判定不能として
	// 破棄する（Req 2.6）。payload 生値はログに含めない（NFR 3.1）。
	rawType := strings.TrimSpace(msg.Attributes[notificationTypeAttribute])
	if rawType == "" {
		v.log.Warn("notification: notification type is undeterminable; discarding",
			logger.MessageID(msg.ID))
		return Envelope{}, discardError("notification type is undeterminable")
	}

	// payload が JSON として解釈できなければ検証失敗として破棄する（Req 2.5）。enterprise_name の
	// 抽出元でもあるためここで一度だけ unmarshal する。エラー文言に payload 生値を補間しない（NFR 3.1）。
	var resource amapiResource
	if err := json.Unmarshal(msg.Data, &resource); err != nil {
		v.log.Warn("notification: payload is not valid JSON; discarding",
			logger.MessageID(msg.ID))
		return Envelope{}, discardError("notification payload validation failed")
	}

	env := Envelope{
		MessageID:        msg.ID,
		NotificationType: NotificationType(rawType),
		EnterpriseName:   enterpriseNameFromResourceName(resource.Name),
		Payload:          msg.Data,
		PublishTime:      msg.PublishTime,
	}
	return env, nil
}

// enterpriseNameFromResourceName は AMAPI リソース名（`enterprises/{enterpriseId}/...`）から
// `enterprises/{enterpriseId}` を切り出す純粋関数。
//
// name が空 / 接頭辞 `enterprises/` 不一致 / enterprise ID 部が空の場合は空文字を返す
// （enterprise_name 不明 = 退避経路 / Req 3.4。Verifier はエラーにせず Dispatcher に判定を委ねる）。
func enterpriseNameFromResourceName(name string) string {
	name = strings.TrimSpace(name)
	if !strings.HasPrefix(name, enterpriseNameResourcePrefix) {
		return ""
	}
	// `enterprises/` の直後セグメント（次の `/` まで）を enterprise ID とする。
	rest := name[len(enterpriseNameResourcePrefix):]
	enterpriseID := rest
	if idx := strings.IndexByte(rest, '/'); idx >= 0 {
		enterpriseID = rest[:idx]
	}
	if enterpriseID == "" {
		return ""
	}
	return enterpriseNameResourcePrefix + enterpriseID
}

// discardError は破棄 ack 相当の恒常的失敗を表す *errors.Error を構築する。
//
// IsTransient=false にすることで ShouldAck が ack（再配信しない）に倒す（Req 2.5 / 2.6）。
// CodeBusinessRule は「取りこぼさずログ + 破棄」の分類を表す（design.md Error Categories の
// Business / 分類処理。worker では ack 完了扱いへ写像される）。message には機密値（payload
// 生値等）を補間しない固定文言のみを用いる（NFR 3.1）。
func discardError(message string) error {
	return &pkgerrors.Error{
		Code:        pkgerrors.CodeBusinessRule,
		Message:     message,
		IsTransient: false,
	}
}
