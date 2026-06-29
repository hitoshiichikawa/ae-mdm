package notification

import (
	"context"

	"github.com/google/uuid"

	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/pubsub"
)

// Dispatcher は pubsub.MessageHandler を実装し、1 メッセージを段階処理で orchestrate する
// （design.md「Dispatcher」節 / Req 1.1〜1.4 / 2.1〜2.6 / 3.1〜3.5 / 5.1〜5.3）。
//
// 段階: Verify → Dedup(fast-path) → 種別サポート判定 → Resolve →
// (未割当なら Unassigned へ冪等退避 / それ以外は tenant context 確立 + 種別 handler 振り分け → 成功後に dedupe 記録)。
//
//   - 種別サポート判定（Req 2.4）は **退避（Req 3.2）より前**に置く。未対応/未登録（対応 handler
//     不在）の通知は退避せず取りこぼさずログを残して完了扱いにする。これにより未知種別（例: AMAPI
//     がトピック作成時に送る notificationType=test）が enterprise_name 未解決時に退避キューを汚さない
//     （types.go「未知種別は破棄 ack」設計意図と整合）。
//   - 未割当退避（Req 3.2 / 3.3）は dedupe 記録と退避 INSERT を **同一 tx で原子的**に行い、
//     message_id 単位で冪等にする（UnassignedQueue.Enqueue が担保 / design.md L207-208 の「dedupe
//     記録と副作用を同一 tx に閉じる」ideal を handler を介さない退避経路で実現）。中間状態
//     （dedupe 記録のみ残り退避が失敗）を作らず、退避キューに重複行を作らない（Req 1.3 / 5.1）。
//   - 種別 dispatch は **handler 成功の後に dedupe 記録**する record-after-success（design.md
//     L161 / L382 / L462-467）。handler が失敗したら dedupe を記録せず error を返し、transient なら
//     再配信で再処理させる（喪失させない / Req 5.1 / 5.3）。handler の実体は IF 経由の本 Issue 外
//     依存で同一 tx に閉じられないため、「成功した処理のみ dedupe される」不変条件で二重 *更新* を
//     防ぐ。並行同一 MessageID の dedupe 記録は PK + ON CONFLICT で 1 行へ収束する（Req 1.2 / 1.3）。
//     handler は at-least-once 配信前提で冪等であることが各 handler 側 Issue の責務であり、稀な
//     並行同時到達で handler が複数回呼ばれても冪等性が吸収する（design.md L462-467 Risk）。
//
// 各段の失敗は errors.ShouldAck が解釈する error 種別（IsTransient）へ写像する（worker 最外層
// subscriber が ack/nack を決定）。
//
//   - 完了扱い（ack） = IsTransient=false の error または nil:
//     既処理（Req 1.2）/ 未割当退避成功・冪等 no-op（Req 3.3）/ 未対応・未登録種別（Req 2.4）/
//     検証失敗・空 payload（Req 2.5 / 2.6）/ 恒常的 handler 失敗（再配信しても結果が変わらない）
//   - 再処理保持（nack） = IsTransient=true の error:
//     dedupe 記録 / 退避の永続化失敗（Req 1.4）/ tenant 逆引きの DB 失敗 / transient な
//     handler 失敗（Req 5.1 / 5.3）
type Dispatcher struct {
	verifier       Verifier
	dedupe         Dedupe
	unassigned     UnassignedQueue
	tenantResolver TenantResolver
	handlers       map[NotificationType]NotificationHandler
	log            logger.Logger
}

// NewDispatcher は Dispatcher を構築する。
//
// handlers は NotificationType → NotificationHandler の登録 map。登録されていない種別は
// 取りこぼさずログを残した上で完了扱いにする（Req 2.4）。log が nil の場合は logger.Default()
// （未配線時は no-op）を採り、DI 未配線でも構造化ログ呼び出しで panic させない（Verifier と同方針）。
func NewDispatcher(
	verifier Verifier,
	dedupe Dedupe,
	unassigned UnassignedQueue,
	tenantResolver TenantResolver,
	handlers map[NotificationType]NotificationHandler,
	log logger.Logger,
) *Dispatcher {
	if log == nil {
		log = logger.Default()
	}
	return &Dispatcher{
		verifier:       verifier,
		dedupe:         dedupe,
		unassigned:     unassigned,
		tenantResolver: tenantResolver,
		handlers:       handlers,
		log:            log,
	}
}

// Handle は pubsub.MessageHandler の実装。1 メッセージを段階処理する（design.md フロー図と 1:1）。
//
// 戻り値は errors.ShouldAck で ack/nack に写像される（nil / IsTransient=false → ack、
// IsTransient=true → nack）。機密値（payload 生値・token）は error 文言・構造化ログに補間しない
// （NFR 3.1）。ログ field は message_id（logger.MessageID）と非機密 field に限定する。
func (d *Dispatcher) Handle(ctx context.Context, msg *pubsub.Message) error {
	// 1. Verify: 空 payload / 種別判定不能 / 検証失敗は破棄 ack 相当（IsTransient=false / Req 2.5 / 2.6）。
	env, err := d.verifier.Parse(msg)
	if err != nil {
		// Verifier 内で破棄ログは出力済み。ここでは error をそのまま返し ShouldAck に ack 写像を委ねる。
		return err
	}

	// 2. Dedup fast-path: 既処理 MessageID は種別別 dispatch を実行せず即 ack（Req 1.2）。判定の
	//    DB 失敗は transient のため nack 保持（Req 1.4）。dedupe 記録は handler 成功後に行う
	//    record-after-success のため、本 SELECT は「成功済み処理の再配信」を安価に弾く既処理判定で
	//    あり、再配信ループでの二重 dispatch を防ぐ主経路でもある。並行同一 MessageID の記録自体は
	//    後段 MarkProcessed の PK + ON CONFLICT で 1 行へ収束する（Req 1.2 / 1.3）。
	processed, err := d.dedupe.IsProcessed(ctx, env.MessageID)
	if err != nil {
		d.log.Warn("notification: dedupe lookup failed; will retry",
			logger.MessageID(env.MessageID))
		return err
	}
	if processed {
		d.log.Info("notification: duplicate message; acking without dispatch",
			logger.MessageID(env.MessageID))
		return nil
	}

	// 3. 種別サポート判定: 未対応/未登録（対応 handler 不在）は取りこぼさずログを残して完了扱い
	//    （Req 2.4）。この判定を **退避（Req 3.2）より前**に置くことで、未知種別（例: AMAPI が
	//    トピック作成時に 1 度だけ送る notificationType=test 等）が enterprise_name 未解決時に
	//    unassigned_notifications へ誤って退避されるのを防ぐ（types.go「未知種別は破棄 ack」設計
	//    意図と整合）。handler を呼ばないため dedupe 記録を残さず、将来 handler 登録後の再配信で
	//    再処理しうる。
	handler, registered := d.handlers[env.NotificationType]
	if !registered {
		d.log.Warn("notification: no handler registered for notification type; acking without dispatch",
			logger.MessageID(env.MessageID),
			"notification_type", string(env.NotificationType))
		return nil
	}

	// 4. Resolve: enterprise_name → tenant_id を解決する。空 enterprise_name は DB を叩かず
	//    未割当（退避経路）へ倒す（Req 3.4）。
	tenantID, found, err := d.resolveTenant(ctx, env)
	if err != nil {
		// tenant 逆引きの DB 失敗は transient（再処理保持 / Req 1.4 経路）。
		d.log.Warn("notification: tenant resolution failed; will retry",
			logger.MessageID(env.MessageID))
		return err
	}
	if !found {
		// 5. 未割当退避: dedupe 記録 + 退避 INSERT を同一 tx で原子的・冪等に行い ack 完了扱い
		//    （Req 3.2 / 3.3）。いずれのテナントリソースも更新しない（NFR 2.1 / 2.2 / Req 3.5）。
		return d.enqueueUnassigned(ctx, env)
	}

	// 6. tenant context を確立して種別 handler を呼び、成功後に dedupe 記録する（Req 1.1 / 3.1 / NFR 2.1）。
	return d.dispatchToHandler(ctx, env, tenantID, handler)
}

// resolveTenant は enterprise_name から tenant_id を解決する。
//
// enterprise_name が空文字なら逆引きを行わず found=false を返し、退避経路へ倒す（Req 3.4 /
// task 2 wire-format 前提: Verifier は enterprise_name 不明を空文字で通す）。非空は
// TenantResolver へ委譲する。DB 失敗（transient）はそのまま伝播し worker の nack に委ねる。
func (d *Dispatcher) resolveTenant(ctx context.Context, env Envelope) (uuid.UUID, bool, error) {
	if env.EnterpriseName == "" {
		// enterprise_name 空/欠落は退避経路（Req 3.4）。DB を叩かない。
		return uuid.Nil, false, nil
	}
	return d.tenantResolver.TenantIDByEnterpriseName(ctx, env.EnterpriseName)
}

// enqueueUnassigned は未割当通知を冪等に退避し ack 完了扱い（nil）を返す（Req 3.2 / 3.3）。
//
// 退避の冪等性・原子性（dedupe 記録 + 退避 INSERT を同一 tx で実行し、message_id 単位で重複を
// 弾く）は UnassignedQueue.Enqueue が担保する（unassigned.go 参照）。本関数は ack/nack 写像と
// ログのみを担う:
//   - Enqueue 成功（新規退避 / 並行・既処理の冪等 no-op いずれも）→ ack 完了扱い（Req 3.3）
//   - Enqueue 失敗（transient）→ そのまま返し worker の nack 保持に委ねる。dedupe 記録と退避 INSERT は
//     原子的なので失敗時に中間状態（記録のみ残る）を作らず、再配信時に再退避できる（取りこぼし防止 /
//     重複行防止 / Req 1.4 / 5.1 / NFR 2.2）。
func (d *Dispatcher) enqueueUnassigned(ctx context.Context, env Envelope) error {
	if err := d.unassigned.Enqueue(ctx, env); err != nil {
		d.log.Warn("notification: unassigned enqueue failed; will retry",
			logger.MessageID(env.MessageID))
		return err
	}
	d.log.Info("notification: message has no resolvable tenant; quarantined to unassigned queue",
		logger.MessageID(env.MessageID),
		"notification_type", string(env.NotificationType))
	return nil
}

// dispatchToHandler は tenant context を確立して種別別 handler を呼び、**成功後に dedupe 記録**する
// record-after-success（Req 1.1 / 2.1〜2.3 / 3.1 / 5.1〜5.3 / NFR 2.1。design.md L161 / L382 /
// L462-467）。handler は呼び出し側 Handle が登録確認済みのものを渡す（未対応/未登録は Handle が
// 事前に ack 済み / Req 2.4）。
//
//   - tenant context を確立してから handler を呼ぶ（Req 3.1 / NFR 2.1）。handler 以下の DB アクセスは
//     本 context の tenant_id に閉じる。
//   - handler 失敗: dedupe を **記録せず** error をそのまま返し、ShouldAck に ack/nack を委ねる。
//     transient（errors.IsTransient=true）失敗は dedupe 記録が無いので再配信時に再処理される
//     （喪失させない / Req 5.1 / 5.3）。恒常的（non-transient）失敗も記録せず ShouldAck が ack に
//     倒す（再配信しても結果が変わらない）。これにより dedupe には「成功した処理」だけが残る。
//   - handler 成功: dedupe 記録（MarkProcessed）を行ってから ack（Req 1.1 / 5.2）。記録の DB 失敗は
//     transient nack（Req 1.4）。handler は冪等前提のため、再配信で handler が再実行されても安全。
//
// 設計上の trade-off（design.md L462-467 Risk「dedupe 記録と handler 副作用の非原子性」）: handler の
// 実体は IF 経由の本 Issue 外依存のため dedupe 記録と handler 副作用を単一 tx に閉じられない。
// record-after-success は handler 成功後の worker 停止や記録失敗で handler が再実行されうる（=
// at-most-once ではない）が、handler 冪等性がこれを吸収し、**通知を喪失しない**ことを優先する（Req
// 5.3 の no-loss 不変条件を、並行同時到達時の handler 一回実行より優先。退避経路は handler 非関与の
// ため単一 tx で原子化できるが、handler 経路は構造的に不可）。
func (d *Dispatcher) dispatchToHandler(ctx context.Context, env Envelope, tenantID uuid.UUID, handler NotificationHandler) error {
	// tenant context を確立してから handler を呼ぶ（Req 3.1 / NFR 2.1）。handler 以下の DB
	// アクセスは本 context の tenant_id に閉じる。
	handlerCtx := db.WithTenantContext(ctx, db.TenantContext{TenantID: tenantID})
	if err := handler.Handle(handlerCtx, env); err != nil {
		// handler 失敗時は dedupe を記録しない（成功した処理のみ dedupe される / design.md L466）。
		// transient なら記録が無いので再配信で再処理され、恒常的なら ShouldAck が ack に倒す。
		d.log.Warn("notification: handler failed",
			logger.MessageID(env.MessageID),
			"notification_type", string(env.NotificationType))
		return err
	}

	// handler 成功後に dedupe 記録する（record-after-success / Req 1.1 / 5.2）。記録の DB 失敗は
	// transient nack（Req 1.4）。再配信で handler が再実行されても冪等性が吸収する。
	if err := d.dedupe.MarkProcessed(ctx, env.MessageID, env.NotificationType); err != nil {
		d.log.Warn("notification: dedupe mark failed after handler success; will retry",
			logger.MessageID(env.MessageID),
			"notification_type", string(env.NotificationType))
		return err
	}

	d.log.Info("notification: dispatched and recorded",
		logger.MessageID(env.MessageID),
		"notification_type", string(env.NotificationType))
	return nil
}
