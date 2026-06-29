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
// 段階: Verify → Dedup → Resolve → (未割当なら Unassigned 退避) → tenant context 確立 →
// 種別 handler 振り分け → 成功時 dedupe 記録。各段の失敗は errors.ShouldAck が解釈する
// error 種別（IsTransient）へ写像する（worker 最外層 subscriber が ack/nack を決定）。
//
//   - 完了扱い（ack） = IsTransient=false の error または nil:
//     既処理（Req 1.2）/ 未割当退避成功（Req 3.3）/ 未登録種別（Req 2.4）/
//     検証失敗・空 payload（Req 2.5 / 2.6）
//   - 再処理保持（nack） = IsTransient=true の error:
//     dedupe / 退避の永続化失敗（Req 1.4）/ tenant 逆引きの DB 失敗 / transient な
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

	// 2. Dedup: 既処理 MessageID は種別別 dispatch を実行せず即 ack（Req 1.2）。判定の DB 失敗は
	//    transient のため nack 保持（Req 1.4）。
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

	// 3. Resolve: enterprise_name → tenant_id を解決する。空 enterprise_name は DB を叩かず
	//    未割当（退避経路）へ倒す（Req 3.4）。
	tenantID, found, err := d.resolveTenant(ctx, env)
	if err != nil {
		// tenant 逆引きの DB 失敗は transient（再処理保持 / Req 1.4 経路）。
		d.log.Warn("notification: tenant resolution failed; will retry",
			logger.MessageID(env.MessageID))
		return err
	}
	if !found {
		// 4. 未割当退避: unassigned へ INSERT し dedupe 記録した上で ack 完了扱い（Req 3.2 / 3.3）。
		//    いずれのテナントリソースも更新しない（NFR 2.1 / 2.2 / Req 3.5）。
		return d.enqueueUnassigned(ctx, env)
	}

	// 5. tenant context を確立してから種別 handler を呼ぶ（Req 3.1 / NFR 2.1）。
	return d.dispatchToHandler(ctx, env, tenantID)
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

// enqueueUnassigned は未割当通知を退避し dedupe 記録した上で ack 完了扱い（nil）を返す
// （Req 3.2 / 3.3）。
//
// 退避 INSERT 失敗は transient のため nack 保持（取りこぼし防止 / Req 1.4 経路 / NFR 2.2）。
// 退避成功後の dedupe 記録失敗も transient のため nack（再配信時に既処理判定で退避は
// ON CONFLICT 等で多重化しないが、本 Issue では退避 INSERT に冪等制約を置かないため dedupe を
// handler 成功後と同様「退避成功後」に記録して二重退避を防ぐ）。
func (d *Dispatcher) enqueueUnassigned(ctx context.Context, env Envelope) error {
	if err := d.unassigned.Enqueue(ctx, env); err != nil {
		d.log.Warn("notification: unassigned enqueue failed; will retry",
			logger.MessageID(env.MessageID))
		return err
	}
	d.log.Info("notification: message has no resolvable tenant; quarantined to unassigned queue",
		logger.MessageID(env.MessageID),
		"notification_type", string(env.NotificationType))

	if err := d.dedupe.MarkProcessed(ctx, env.MessageID, env.NotificationType); err != nil {
		d.log.Warn("notification: dedupe mark failed after unassigned enqueue; will retry",
			logger.MessageID(env.MessageID))
		return err
	}
	return nil
}

// dispatchToHandler は tenant context を確立してから種別別 handler を呼び、成功時に dedupe を
// 記録する（Req 2.1〜2.4 / 3.1 / 5.1〜5.3 / NFR 2.1）。
//
//   - 未登録種別: 取りこぼさずログを残した上で完了扱い（nil → ack / Req 2.4）。dedupe は記録
//     しない（処理していないため）。
//   - handler 失敗: error をそのまま返し ShouldAck に ack/nack を委ねる（transient → nack / Req 5.1）。
//     失敗時は dedupe を記録せず再処理を許す（Risk: dedupe 記録は handler 成功後）。
//   - 成功: dedupe を記録した上で ack（Req 1.1 / 5.2）。記録失敗は transient → nack（Req 1.4）。
func (d *Dispatcher) dispatchToHandler(ctx context.Context, env Envelope, tenantID uuid.UUID) error {
	handler, ok := d.handlers[env.NotificationType]
	if !ok {
		// 未対応/未登録種別は取りこぼさずログ + 完了扱い（Req 2.4）。
		d.log.Warn("notification: no handler registered for notification type; acking without dispatch",
			logger.MessageID(env.MessageID),
			"notification_type", string(env.NotificationType))
		return nil
	}

	// tenant context を確立してから handler を呼ぶ（Req 3.1 / NFR 2.1）。handler 以下の DB
	// アクセスは本 context の tenant_id に閉じる。
	handlerCtx := db.WithTenantContext(ctx, db.TenantContext{TenantID: tenantID})
	if err := handler.Handle(handlerCtx, env); err != nil {
		// transient な失敗は nack 保持（Req 5.1 / 5.3）、恒常的失敗は ack。dedupe は記録しない。
		d.log.Warn("notification: handler failed",
			logger.MessageID(env.MessageID),
			"notification_type", string(env.NotificationType))
		return err
	}

	// handler 成功後に dedupe を記録する（Risk: 成功した処理のみ dedupe / Req 1.1 / 5.2）。
	if err := d.dedupe.MarkProcessed(ctx, env.MessageID, env.NotificationType); err != nil {
		d.log.Warn("notification: dedupe mark failed after successful dispatch; will retry",
			logger.MessageID(env.MessageID))
		return err
	}
	d.log.Info("notification: dispatched and recorded",
		logger.MessageID(env.MessageID),
		"notification_type", string(env.NotificationType))
	return nil
}
