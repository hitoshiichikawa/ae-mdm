package notification

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// NotificationType は AMAPI 通知の種別を表す string ベースの enum。
//
// 本 Issue で dispatch 対象とする 3 種別（ENROLLMENT / STATUS_REPORT / COMMAND）を
// 定数化する（Req 2.1〜2.3）。これら以外の種別（USAGE_LOGS_UPLOADED 等）や未知種別は
// Verifier では種別判定不能としては扱わず（attribute 値があれば NotificationType に格納する）、
// 対応ハンドラ未登録として Dispatcher が破棄 ack に倒す（Req 2.4 / 後続 task）。
type NotificationType string

const (
	// Enrollment はデバイスのエンロール通知（Req 2.1）。
	Enrollment NotificationType = "ENROLLMENT"
	// StatusReport はデバイス状態レポート通知（Req 2.2）。
	StatusReport NotificationType = "STATUS_REPORT"
	// Command はコマンド完了通知（Req 2.3）。
	Command NotificationType = "COMMAND"
)

// Envelope は受信した Pub/Sub メッセージを Dispatcher の段階処理が消費する正規表現へ
// パースした値オブジェクト（design.md「Verifier」Postconditions と一致）。
//
// Verifier.Parse が *pubsub.Message から構築する。EnterpriseName は空文字を取りうる
// （enterprise_name 空 / 欠落は Verifier ではエラーにせず通し、退避判定は Dispatcher の
// 責務 / Req 3.4）。
type Envelope struct {
	// MessageID は Pub/Sub が採番した一意識別子。重複排除（dedupe）の鍵であり、構造化ログの
	// message_id field に使う。
	MessageID string
	// NotificationType は通知種別（attribute から抽出 / Req 2.1〜2.3）。
	NotificationType NotificationType
	// EnterpriseName は AMAPI が払い出す Enterprise 識別子（テナント特定の鍵）。空文字は
	// 未割当退避経路へ流す（Dispatcher の責務 / Req 3.4）。
	EnterpriseName string
	// Payload はメッセージ本文（生 bytes）。退避時に jsonb 列へ格納する。機密値を含みうるため
	// error 文言・ログに補間しない（NFR 3.1）。
	Payload []byte
	// PublishTime はメッセージが publish された時刻（サーバ採番）。
	PublishTime time.Time
}

// UnassignedNotification は unassigned_notifications テーブルの 1 行に 1:1 で対応する
// 値オブジェクト（テナント未割当として退避された通知 / Req 3.2）。
//
// 後続 task（UnassignedQueue の Enqueue / List、admin_handler の閲覧）が消費する。
// 各フィールドは migration 0010 の列と対応する。
type UnassignedNotification struct {
	// ID は退避レコードの一意 ID（uuid PK）。Enqueue 時に採番する。
	ID uuid.UUID
	// MessageID は退避元通知の Pub/Sub message_id。
	MessageID string
	// NotificationType は退避元通知の種別。
	NotificationType NotificationType
	// EnterpriseName は解決できなかった enterprise_name（空文字を取りうる / Req 3.4）。
	EnterpriseName string
	// Payload は退避元通知の payload（jsonb 列に格納される生 bytes）。
	Payload []byte
	// ReceivedAt は退避（受信）時刻。
	ReceivedAt time.Time
}

// Filter は退避キュー閲覧 API（admin_handler / UnassignedQueue.List）の絞り込み条件を表す
// 値オブジェクト（design.md「UnassignedQueue」/ Req 4.2）。
//
// ポインタ型は nil を、文字列型は空文字を「未指定（絞り込みなし）」として解釈する
// （audit.Filter と同方針）。parse 検証（RFC3339 / 許可種別値）は admin_handler の責務。
type Filter struct {
	// From は received_at の開始時刻（received_at >= From）。nil は未指定。
	From *time.Time
	// To は received_at の終了時刻（received_at <= To）。nil は未指定。
	To *time.Time
	// Type は notification_type の一致条件。空文字は無条件（絞り込みなし）。
	Type string
}

// NotificationHandler は通知種別ごとの本体処理を担う抽象（Req 2.1〜2.3）。
//
// 各ドメインハンドラ（ENROLLMENT / STATUS_REPORT / COMMAND の実体）は後続 Issue の所有で
// あり、Dispatcher は NotificationType → NotificationHandler の登録 map で振り分けるのみ。
// Handle が transient な失敗（再試行で回復しうる）を返す場合、Dispatcher はそれを nack 保持に
// 写像する（Req 5.1 / 後続 task）。
type NotificationHandler interface {
	Handle(ctx context.Context, env Envelope) error
}

// TenantResolver は Dispatcher が必要とする tenant 逆引きの最小 interface
// （依存逆転 / テスト容易性 / design.md 採用案 a）。
//
// tenant.Service の TenantIDByEnterpriseName メソッドが structural typing で本 IF を満たす。
// notification は tenant パッケージを直接 import せず、Dispatcher 構築時に本 IF 経由で
// 注入を受けることでドメイン所有権境界を維持する（doc.go 依存方向ルール）。
//
// 戻り値は (tenant_id, found, error)。found=false は「テナント未割当」を表し（エラーでは
// ない）、Dispatcher の退避経路へ流す（Req 3.2）。
type TenantResolver interface {
	TenantIDByEnterpriseName(ctx context.Context, enterpriseName string) (uuid.UUID, bool, error)
}
