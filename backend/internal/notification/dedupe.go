package notification

import (
	"context"
	stderrors "errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
)

// claimSQL は notification_dedupe への冪等記録（claim）INSERT 文。
//
// PK = message_id + ON CONFLICT (message_id) DO NOTHING により、同一 MessageID の並行 INSERT は
// DB が一意 index で直列化し、勝者のみ RowsAffected=1、敗者は 0 になる（Req 1.3 = 並行同一
// MessageID の直列化）。Dispatcher は dispatch / 退避の実行 **前** にこの claim を取り、勝者だけが
// 後続処理へ進むことで handler の二重実行を防ぐ（Req 1.1 = 記録した上で後続処理を実行）。
const claimSQL = `INSERT INTO notification_dedupe (message_id, notification_type) ` +
	`VALUES ($1, $2) ON CONFLICT (message_id) DO NOTHING`

// releaseSQL は claim を取り消す（dedupe 記録を削除する）DELETE 文。
//
// claim 後の後続処理（dispatch / 退避）が失敗したとき、勝者が自身の claim を取り消して
// 再配信時の再処理を可能にするために使う（Req 5.1 / 5.3 = 一時的失敗は喪失させず再処理保持）。
const releaseSQL = `DELETE FROM notification_dedupe WHERE message_id = $1`

// isProcessedSQL は MessageID の既処理判定 SELECT 文（行の存在確認 / Req 1.2）。
const isProcessedSQL = `SELECT 1 FROM notification_dedupe WHERE message_id = $1`

// Dedupe は notification_dedupe で MessageID 単位の冪等性を担保する抽象
// （design.md「Dedupe」節 / Req 1.1〜1.4 / 5.1 / 5.3）。
type Dedupe interface {
	// IsProcessed は messageID が既に処理済み（記録済み）かを返す（Req 1.2 の fast-path）。
	IsProcessed(ctx context.Context, messageID string) (bool, error)
	// Claim は messageID を notification_dedupe へ原子的に記録し、本呼び出しが行を INSERT
	// できた（= 自身が dispatch 担当に選ばれた）場合のみ claimed=true を返す（Req 1.1 / 1.3）。
	//
	// ON CONFLICT (message_id) DO NOTHING + RowsAffected 判定により、並行する同一 MessageID の
	// うち 1 件だけが claimed=true となり、残りは claimed=false（既に他者が claim 済み）になる。
	// 呼び出し側は claimed=false を「重複として dispatch せず ack」に倒す（Req 1.2 / 1.3）。
	// 永続化失敗は transient な *errors.Error で返す（Req 1.4 → worker nack）。
	Claim(ctx context.Context, messageID string, notificationType NotificationType) (claimed bool, err error)
	// Release は messageID の claim（dedupe 記録）を削除する。claim 後の後続処理が失敗したとき、
	// 勝者が自身の claim を取り消して再配信時の再処理を可能にする（Req 5.1 / 5.3）。
	// 永続化失敗は transient な *errors.Error で返す（Req 1.4）。
	Release(ctx context.Context, messageID string) error
}

// dedupe は Dedupe の pgxpool ベース実装。
//
// notification_dedupe は tenant_id を持たない cross-tenant infra テーブルであり、RLS
// （migration 0011）は SuperAdmin only。tenant.Repository と同型で SuperAdmin context を
// 確立してから BeginTxFunc で tx を開く（doc.go 依存方向ルール / 既存スキーマ 0010 / 0011 を消費）。
type dedupe struct {
	pool *pgxpool.Pool
}

// NewDedupe は Dedupe を構築する。pool は cmd bootstrap が構築する共有 pgxpool.Pool を渡す
// （tenant.NewRepository と同方式）。
func NewDedupe(pool *pgxpool.Pool) Dedupe {
	return &dedupe{pool: pool}
}

// superAdminContext は ctx に SuperAdmin TenantContext を埋め込む。
//
// notification_dedupe / unassigned_notifications は SuperAdmin only RLS（0011）であり、
// 越境アクセスには SuperAdmin context が必要（tenant/repository.go の同名 helper と同型）。
func superAdminContext(ctx context.Context) context.Context {
	return db.WithTenantContext(ctx, db.TenantContext{
		TenantID:     uuid.Nil,
		IsSuperAdmin: true,
	})
}

// IsProcessed は Dedupe.IsProcessed の実装（Req 1.2）。
//
// 行が存在すれば true、存在しなければ（pgx.ErrNoRows）false かつ非エラーを返す。bool 写像は
// 純粋関数 mapIsProcessedScan に切り出し単体検証可能にする。DB 失敗は wrapDedupePersistErr で
// transient 写像する（Req 1.4）。
func (d *dedupe) IsProcessed(ctx context.Context, messageID string) (bool, error) {
	ctx = superAdminContext(ctx)
	var processed bool
	err := db.BeginTxFunc(ctx, d.pool, func(tx pgx.Tx) error {
		var one int
		scanErr := tx.QueryRow(ctx, isProcessedSQL, messageID).Scan(&one)
		found, mapErr := mapIsProcessedScan(scanErr)
		if mapErr != nil {
			return mapErr
		}
		processed = found
		return nil
	})
	if err != nil {
		// fn 内部の scan 失敗だけでなく、BeginTx / SetLocalTenant / Commit 由来の失敗も
		// transient へ正規化する（さもないと CodeInternal/非 transient のまま ShouldAck が
		// ack 判定し通知を喪失する / Req 1.4）。wrapDedupePersistErr は transient 既写像を二重
		// wrap しない。
		return false, wrapDedupePersistErr(err)
	}
	return processed, nil
}

// Claim は Dedupe.Claim の実装（Req 1.1 / 1.3）。
//
// INSERT ... ON CONFLICT (message_id) DO NOTHING の RowsAffected で claim 成否を判定する。
// 同一 MessageID の並行 INSERT は DB の一意 index が直列化し、勝者のみ RowsAffected=1（claimed
// =true）、既に行がある場合は 0（claimed=false）となる。fn 内部・外部いずれの DB 失敗も
// wrapDedupePersistErr で transient 写像する（Req 1.4）。
func (d *dedupe) Claim(ctx context.Context, messageID string, notificationType NotificationType) (bool, error) {
	ctx = superAdminContext(ctx)
	var claimed bool
	err := db.BeginTxFunc(ctx, d.pool, func(tx pgx.Tx) error {
		ct, execErr := tx.Exec(ctx, claimSQL, messageID, string(notificationType))
		if execErr != nil {
			return wrapDedupePersistErr(execErr)
		}
		// RowsAffected=1 なら本呼び出しが INSERT を成立させた（dispatch 担当に選ばれた）。
		// 0 なら ON CONFLICT で no-op = 既に他者が claim 済み（重複）。
		claimed = ct.RowsAffected() == 1
		return nil
	})
	if err != nil {
		return false, wrapDedupePersistErr(err)
	}
	return claimed, nil
}

// Release は Dedupe.Release の実装（Req 5.1 / 5.3）。
//
// claim 後の後続処理（dispatch / 退避）が失敗したとき、勝者が自身の dedupe 記録を削除して
// 再配信時の再処理を可能にする。fn 内部・外部いずれの DB 失敗も wrapDedupePersistErr で
// transient 写像する（Req 1.4。wrapDedupePersistErr(nil) は nil を返すため成功時は nil）。
func (d *dedupe) Release(ctx context.Context, messageID string) error {
	ctx = superAdminContext(ctx)
	err := db.BeginTxFunc(ctx, d.pool, func(tx pgx.Tx) error {
		if _, execErr := tx.Exec(ctx, releaseSQL, messageID); execErr != nil {
			return wrapDedupePersistErr(execErr)
		}
		return nil
	})
	return wrapDedupePersistErr(err)
}

// mapIsProcessedScan は IsProcessed の SELECT 結果（scan の error）を (found, error) へ写像する
// 純粋関数。実 DB に依存せず単体テスト可能にするための切り出し。
//
//   - scanErr == nil           → 行あり = 既処理（true, nil）
//   - scanErr == pgx.ErrNoRows → 行なし = 未処理（false, nil）。エラーにしない（Req 1.2）
//   - その他の scanErr         → DB 失敗として transient 写像（Req 1.4）
func mapIsProcessedScan(scanErr error) (bool, error) {
	if scanErr == nil {
		return true, nil
	}
	if stderrors.Is(scanErr, pgx.ErrNoRows) {
		return false, nil
	}
	return false, wrapDedupePersistErr(scanErr)
}

// wrapDedupePersistErr は notification_dedupe の永続化 / 参照失敗（DB 不通等）を
// transient な *errors.Error へ写像する純粋関数（Req 1.4）。
//
// IsTransient=true により worker 最外層（ShouldAck）は nack（再処理保持）に倒し、通知の
// 喪失を防ぐ。IsTransient=true は errors.Wrap が設定しないため、tenant の逆引き同様に
// &errors.Error{...} をリテラル構築する。message には機密値（query 生値等）を補間しない
// 固定文言を用いる（NFR 3.1）。
//
// 本関数は BeginTxFunc の fn 内部（scan / exec 失敗）と外部（BeginTx / SetLocalTenant / Commit
// 失敗 = CodeInternal/非 transient）の両方の wrap 点で共用するため:
//   - cause == nil               → nil（成功経路をエラー化しない）
//   - cause が既に transient *Error → そのまま返す（二重 wrap 回避。fn 内部で写像済みの経路）
//   - それ以外（非 transient error） → transient *Error に包む（外側 tx 失敗の救済 / Req 1.4）
func wrapDedupePersistErr(cause error) error {
	if cause == nil {
		return nil
	}
	var de *pkgerrors.Error
	if stderrors.As(cause, &de) && de.IsTransient {
		return cause
	}
	return &pkgerrors.Error{
		Code:        pkgerrors.CodeUnavailable,
		Message:     "notification dedupe persistence failed",
		IsTransient: true,
		Cause:       cause,
	}
}
