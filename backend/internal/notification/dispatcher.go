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
// 段階: Verify → Dedup(fast-path) → Resolve → claim → (未割当なら Unassigned 退避 / それ以外は
// tenant context 確立 + 種別 handler 振り分け)。dedupe の記録は **dispatch / 退避を実行する前**に
// 原子的 claim（INSERT ON CONFLICT DO NOTHING + RowsAffected）で行い、勝者だけが後続処理へ進む。
// これにより同一 MessageID が並行到達しても handler / 退避は 1 件のみが実行される（Req 1.1 = 記録
// した上で後続処理を実行 / Req 1.3 = 直列化）。claim 後の後続処理が失敗した場合は claim を release
// して再処理を許す（Req 5.1 / 5.3）。各段の失敗は errors.ShouldAck が解釈する error 種別
// （IsTransient）へ写像する（worker 最外層 subscriber が ack/nack を決定）。
//
//   - 完了扱い（ack） = IsTransient=false の error または nil:
//     既処理（Req 1.2）/ 並行重複で claim 敗北（Req 1.3）/ 未割当退避成功（Req 3.3）/
//     未登録種別（Req 2.4）/ 検証失敗・空 payload（Req 2.5 / 2.6）
//   - 再処理保持（nack） = IsTransient=true の error:
//     dedupe claim / 退避の永続化失敗（Req 1.4）/ tenant 逆引きの DB 失敗 / transient な
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
	//    DB 失敗は transient のため nack 保持（Req 1.4）。並行重複の最終的な直列化は後段の claim
	//    （dedupe.Claim）が DB の一意制約で担保するため、本 SELECT は確定済み重複を安価に弾く
	//    最適化であり、ここを通過した並行重複は claim で 1 件に絞られる。
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
	//    未割当（退避経路）へ倒す（Req 3.4）。解決は冪等のため claim より前に行い、claim 失敗時の
	//    無駄な取り消しを避ける。
	tenantID, found, err := d.resolveTenant(ctx, env)
	if err != nil {
		// tenant 逆引きの DB 失敗は transient（再処理保持 / Req 1.4 経路）。
		d.log.Warn("notification: tenant resolution failed; will retry",
			logger.MessageID(env.MessageID))
		return err
	}
	if !found {
		// 4. 未割当退避: claim を先取りしてから unassigned へ INSERT し ack 完了扱い（Req 3.2 / 3.3）。
		//    いずれのテナントリソースも更新しない（NFR 2.1 / 2.2 / Req 3.5）。
		return d.enqueueUnassigned(ctx, env)
	}

	// 5. claim を先取りしてから tenant context を確立し種別 handler を呼ぶ（Req 1.1 / 3.1 / NFR 2.1）。
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

// enqueueUnassigned は未割当通知を claim-first で退避し ack 完了扱い（nil）を返す（Req 3.2 / 3.3）。
//
// 退避 INSERT の前に dedupe.Claim を取り、claim できた（claimed=true）場合のみ退避する。これにより
// 並行する同一 MessageID は 1 件だけが退避し、残りは退避せず ack される（Req 1.3）。
// unassigned_notifications は message_id に一意制約を持たない（既存スキーマ 0010 を消費 / 新規
// マイグレーションは scope 外）ため、退避の冪等性は dedupe claim を gate にして担保する。
//
//   - claim の DB 失敗 → transient nack（Req 1.4）
//   - claimed=false（並行重複 / 既 claim）→ 二重退避を防ぐため退避せず ack（Req 1.3 / 3.3）
//   - 退避 INSERT 失敗 → claim を release してから transient nack（取りこぼし防止 / 重複行防止 /
//     Req 5.1 / NFR 2.2）。release により再配信時に再度 claim + 退避できる
func (d *Dispatcher) enqueueUnassigned(ctx context.Context, env Envelope) error {
	claimed, err := d.dedupe.Claim(ctx, env.MessageID, env.NotificationType)
	if err != nil {
		d.log.Warn("notification: dedupe claim failed before quarantine; will retry",
			logger.MessageID(env.MessageID))
		return err
	}
	if !claimed {
		// 既に他者が claim 済み（並行重複 / 既処理）。二重退避を防ぐため退避せず ack（Req 1.3 / 3.3）。
		d.log.Info("notification: message already claimed; skipping quarantine (duplicate)",
			logger.MessageID(env.MessageID))
		return nil
	}

	if err := d.unassigned.Enqueue(ctx, env); err != nil {
		// 退避失敗は claim を取り消して再配信時の再退避を可能にする（重複行を防ぐ / Req 5.1 / NFR 2.2）。
		d.releaseClaim(ctx, env.MessageID)
		d.log.Warn("notification: unassigned enqueue failed; will retry",
			logger.MessageID(env.MessageID))
		return err
	}
	d.log.Info("notification: message has no resolvable tenant; quarantined to unassigned queue",
		logger.MessageID(env.MessageID),
		"notification_type", string(env.NotificationType))
	return nil
}

// dispatchToHandler は claim-first で tenant context を確立してから種別別 handler を呼ぶ
// （Req 1.1 / 1.3 / 2.1〜2.4 / 3.1 / 5.1〜5.3 / NFR 2.1）。
//
//   - 未登録種別: 取りこぼさずログを残した上で完了扱い（nil → ack / Req 2.4）。claim は取らない
//     （処理しないため dedupe 記録を残さない）。
//   - claim を handler 実行の **前** に取る: INSERT ON CONFLICT で並行する同一 MessageID を直列化し、
//     勝者（claimed=true）のみ handler を呼ぶ（Req 1.1 / 1.3 = 二重実行を防ぐ）。
//   - claim の DB 失敗 → transient nack（Req 1.4）。claimed=false（並行重複）→ handler を呼ばず ack。
//   - handler 失敗: claim を release してから error をそのまま返し ShouldAck に ack/nack を委ねる
//     （transient → nack で再配信時に再処理 / Req 5.1 / 5.3）。release により「成功した処理のみ
//     dedupe 記録を残す」invariant を維持する。
//   - 成功: claim をそのまま残して ack（Req 1.1 / 5.2）。
func (d *Dispatcher) dispatchToHandler(ctx context.Context, env Envelope, tenantID uuid.UUID) error {
	handler, ok := d.handlers[env.NotificationType]
	if !ok {
		// 未対応/未登録種別は取りこぼさずログ + 完了扱い（Req 2.4）。claim を取らないため dedupe
		// 記録は残らず、将来 handler が登録された後の再配信で再処理しうる。
		d.log.Warn("notification: no handler registered for notification type; acking without dispatch",
			logger.MessageID(env.MessageID),
			"notification_type", string(env.NotificationType))
		return nil
	}

	// handler を呼ぶ前に claim を取る（Req 1.1 / 1.3）。並行する同一 MessageID は 1 件だけが
	// claimed=true となり、残りは claimed=false で handler を呼ばず ack する。
	claimed, err := d.dedupe.Claim(ctx, env.MessageID, env.NotificationType)
	if err != nil {
		d.log.Warn("notification: dedupe claim failed before dispatch; will retry",
			logger.MessageID(env.MessageID))
		return err
	}
	if !claimed {
		d.log.Info("notification: message already claimed; skipping dispatch (duplicate)",
			logger.MessageID(env.MessageID),
			"notification_type", string(env.NotificationType))
		return nil
	}

	// tenant context を確立してから handler を呼ぶ（Req 3.1 / NFR 2.1）。handler 以下の DB
	// アクセスは本 context の tenant_id に閉じる。
	handlerCtx := db.WithTenantContext(ctx, db.TenantContext{TenantID: tenantID})
	if err := handler.Handle(handlerCtx, env); err != nil {
		// handler 失敗は claim を取り消す（transient は再配信で再処理 / Req 5.1 / 5.3）。
		// これにより dedupe には「成功した処理」だけが残る。
		d.releaseClaim(ctx, env.MessageID)
		d.log.Warn("notification: handler failed",
			logger.MessageID(env.MessageID),
			"notification_type", string(env.NotificationType))
		return err
	}

	d.log.Info("notification: dispatched and recorded",
		logger.MessageID(env.MessageID),
		"notification_type", string(env.NotificationType))
	return nil
}

// releaseClaim は claim 後の後続処理（dispatch / 退避）が失敗したときに自身の dedupe claim を
// 取り消す best-effort helper（Req 5.1 / 5.3）。
//
// Release 自体の失敗は呼び出し元が返す元の失敗 error を上書きせず、構造化 ERROR ログで観測
// 可能にする（NFR 3.1）。Release が失敗すると claim が orphan として残り、後続再配信が既処理
// 判定（IsProcessed）で ack され通知を喪失しうる稀なケースがあるため、運用者が事後追跡できる
// よう WARN ではなく ERROR レベルで残す。
func (d *Dispatcher) releaseClaim(ctx context.Context, messageID string) {
	if err := d.dedupe.Release(ctx, messageID); err != nil {
		d.log.Error("notification: dedupe claim release failed; claim may be orphaned",
			logger.MessageID(messageID))
	}
}
