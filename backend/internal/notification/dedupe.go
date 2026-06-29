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

// markProcessedSQL は notification_dedupe への冪等記録 INSERT 文。
//
// PK = message_id + ON CONFLICT (message_id) DO NOTHING により、同一 MessageID の並行記録は
// 直列化され 2 回目以降は no-op になる（Req 1.3 = 並行同一 MessageID の直列化）。
const markProcessedSQL = `INSERT INTO notification_dedupe (message_id, notification_type) ` +
	`VALUES ($1, $2) ON CONFLICT (message_id) DO NOTHING`

// isProcessedSQL は MessageID の既処理判定 SELECT 文（行の存在確認 / Req 1.2）。
const isProcessedSQL = `SELECT 1 FROM notification_dedupe WHERE message_id = $1`

// Dedupe は notification_dedupe で MessageID 単位の冪等性を担保する抽象
// （design.md「Dedupe」節 / Req 1.1〜1.4）。
type Dedupe interface {
	// IsProcessed は messageID が既に処理済み（記録済み）かを返す（Req 1.2）。
	IsProcessed(ctx context.Context, messageID string) (bool, error)
	// MarkProcessed は messageID を notification_dedupe へ記録する（ON CONFLICT DO NOTHING /
	// Req 1.1 / 1.3）。
	MarkProcessed(ctx context.Context, messageID string, notificationType NotificationType) error
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
		return false, err
	}
	return processed, nil
}

// MarkProcessed は Dedupe.MarkProcessed の実装（Req 1.1 / 1.3）。
//
// ON CONFLICT (message_id) DO NOTHING により、同一 MessageID の並行記録は直列化され 2 回目
// 以降は no-op となる。永続化失敗は wrapDedupePersistErr で transient 写像する（Req 1.4）。
func (d *dedupe) MarkProcessed(ctx context.Context, messageID string, notificationType NotificationType) error {
	ctx = superAdminContext(ctx)
	return db.BeginTxFunc(ctx, d.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, markProcessedSQL, messageID, string(notificationType)); err != nil {
			return wrapDedupePersistErr(err)
		}
		return nil
	})
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
func wrapDedupePersistErr(cause error) error {
	return &pkgerrors.Error{
		Code:        pkgerrors.CodeUnavailable,
		Message:     "notification dedupe persistence failed",
		IsTransient: true,
		Cause:       cause,
	}
}
