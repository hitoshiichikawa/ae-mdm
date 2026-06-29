package notification

import (
	"context"
	stderrors "errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
)

// enqueueUnassignedSQL は unassigned_notifications への退避 INSERT 文（列順を固定）。
//
// id は uuid 採番、enterprise_name は NOT NULL 列のため空文字をそのまま bind する
// （解決できなかった enterprise_name は空文字を取りうる / Req 3.4）。payload は jsonb 列
// （migration 0010 `payload jsonb NOT NULL DEFAULT '{}'`）であり、空 payload は `{}` に
// 写像してから bind する（NULL を bind しない / audit の emptyJSONObject と同方針）。
const enqueueUnassignedSQL = `INSERT INTO unassigned_notifications ` +
	`(id, message_id, notification_type, enterprise_name, payload) ` +
	`VALUES ($1, $2, $3, $4, $5)`

// selectUnassignedColumns は List の SELECT 句の列順（scanUnassigned の Scan 順と一致させる）。
const selectUnassignedColumns = `id, message_id, notification_type, enterprise_name, payload, received_at`

// emptyJSONObject は payload が空のときに bind する空 jsonb（既存スキーマ
// `payload jsonb NOT NULL DEFAULT '{}'` と整合させ、NULL ではなく `{}` を格納する / Req 3.5）。
const emptyJSONObject = "{}"

// UnassignedQueue はテナント未割当通知の退避 INSERT と閲覧 List を提供する抽象
// （design.md「UnassignedQueue」節 / Req 3.2 / 3.3 / 3.5 / 4.1 / 4.2）。
type UnassignedQueue interface {
	// Enqueue は env を unassigned_notifications へ 1 行退避する（Req 3.2）。
	//
	// id は uuid 採番。tenant scoped テーブルには一切触れず、いずれのテナントリソースも
	// 更新しない（NFR 2.2 / Req 3.5）。永続化失敗は *errors.Error{CodeUnavailable,
	// IsTransient:true} で返し、呼び出し側 Dispatcher の nack 保持（再処理）に委ねる（Req 1.4）。
	Enqueue(ctx context.Context, env Envelope) error

	// List は Filter（from/to/type）を適用した退避済み通知を received_at 降順で返す（Req 4.1 / 4.2）。
	//
	// 0 件のときは非 nil の空 slice を返す（admin_handler が 200 + `[]` を返せるように / Req 4.1）。
	List(ctx context.Context, f Filter) ([]UnassignedNotification, error)
}

// unassignedQueue は UnassignedQueue の pgxpool ベース実装。
//
// unassigned_notifications は tenant_id を持たない cross-tenant infra テーブルであり、RLS
// （migration 0011）は SuperAdmin only。dedupe / tenant.Repository と同型で SuperAdmin context を
// 確立してから BeginTxFunc で tx を開く（doc.go 依存方向ルール / 既存スキーマ 0010 / 0011 を消費）。
type unassignedQueue struct {
	pool *pgxpool.Pool
}

// NewUnassignedQueue は UnassignedQueue を構築する。pool は cmd bootstrap が構築する
// 共有 pgxpool.Pool を渡す（tenant.NewRepository / NewDedupe と同方式）。
func NewUnassignedQueue(pool *pgxpool.Pool) UnassignedQueue {
	return &unassignedQueue{pool: pool}
}

// Enqueue は UnassignedQueue.Enqueue の実装（Req 3.2 / 3.5）。
func (q *unassignedQueue) Enqueue(ctx context.Context, env Envelope) error {
	ctx = superAdminContext(ctx)
	err := db.BeginTxFunc(ctx, q.pool, func(tx pgx.Tx) error {
		if _, execErr := tx.Exec(ctx,
			enqueueUnassignedSQL,
			uuid.New(),
			env.MessageID,
			string(env.NotificationType),
			env.EnterpriseName,
			payloadOrEmptyJSON(env.Payload),
		); execErr != nil {
			return wrapUnassignedPersistErr(execErr)
		}
		return nil
	})
	// fn 内部の INSERT 失敗だけでなく、BeginTx / SetLocalTenant / Commit 由来の失敗（CodeInternal/
	// 非 transient）も transient へ正規化する。さもないと退避永続化失敗が ack 判定され通知を
	// 喪失する（Req 1.4 / NFR 2.2）。wrapUnassignedPersistErr(nil) は nil を返す。
	return wrapUnassignedPersistErr(err)
}

// List は UnassignedQueue.List の実装（Req 4.1 / 4.2）。
//
// WHERE 句組み立ては純粋関数 buildUnassignedListQuery に委ね、本メソッドは BeginTxFunc 経由で
// 実行と scan のみを担う。0 件は非 nil 空 slice + nil を返す（Req 4.1）。SELECT / scan 失敗は
// transient な *errors.Error で wrap する（Req 1.4 経路と同型 / admin_handler は 503 に写像）。
func (q *unassignedQueue) List(ctx context.Context, f Filter) ([]UnassignedNotification, error) {
	ctx = superAdminContext(ctx)
	sql, args := buildUnassignedListQuery(f)

	// 0 件でも非 nil の空 slice を返す（Req 4.1）。
	out := make([]UnassignedNotification, 0)
	err := db.BeginTxFunc(ctx, q.pool, func(tx pgx.Tx) error {
		rows, queryErr := tx.Query(ctx, sql, args...)
		if queryErr != nil {
			return wrapUnassignedPersistErr(queryErr)
		}
		defer rows.Close()

		for rows.Next() {
			n, scanErr := scanUnassigned(rows)
			if scanErr != nil {
				return wrapUnassignedPersistErr(scanErr)
			}
			out = append(out, n)
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			return wrapUnassignedPersistErr(rowsErr)
		}
		return nil
	})
	if err != nil {
		// fn 内部の query/scan 失敗だけでなく、BeginTx / SetLocalTenant / Commit 由来の失敗も
		// transient へ正規化する（admin_handler が 503 に写像 / CodeInternal の素通しを防ぐ）。
		return nil, wrapUnassignedPersistErr(err)
	}
	return out, nil
}

// buildUnassignedListQuery は Filter から動的 SQL と $n bind args を組み立てる純粋関数。
// 実 DB に依存せず単体テスト可能にするための切り出し（audit.buildSelectQuery と同型）。
//
// WHERE 句組み立て規約:
//   - Filter.From != nil で received_at >= $n（Req 4.2）
//   - Filter.To != nil で received_at <= $n（Req 4.2）
//   - Filter.Type != "" で notification_type = $n（Req 4.2）
//   - いずれも未指定なら無条件（全件 / Req 4.1）
//   - ORDER BY received_at DESC（新しい退避を先頭に）
//
// すべて pgx の $n プレースホルダで bind し SQL injection を回避する。args の順序は WHERE 句に
// 追加した順であり、戻り値の sql 中の $n と 1:1 で対応する。
func buildUnassignedListQuery(f Filter) (string, []any) {
	conditions := make([]string, 0, 3)
	args := make([]any, 0, 3)

	if f.From != nil {
		args = append(args, *f.From)
		conditions = append(conditions, fmt.Sprintf("received_at >= $%d", len(args)))
	}
	if f.To != nil {
		args = append(args, *f.To)
		conditions = append(conditions, fmt.Sprintf("received_at <= $%d", len(args)))
	}
	if f.Type != "" {
		args = append(args, f.Type)
		conditions = append(conditions, fmt.Sprintf("notification_type = $%d", len(args)))
	}

	sql := "SELECT " + selectUnassignedColumns + " FROM unassigned_notifications"
	if len(conditions) > 0 {
		sql += " WHERE " + strings.Join(conditions, " AND ")
	}
	sql += " ORDER BY received_at DESC"
	return sql, args
}

// unassignedRowScanner は scanUnassigned が依存する最小 interface（pgx.Rows / pgx.Row が満たす）。
type unassignedRowScanner interface {
	Scan(dest ...any) error
}

// scanUnassigned は unassigned_notifications の 1 行を UnassignedNotification に写像する。
//
// notification_type は string 列を NotificationType へ写像、payload は jsonb 列の生 bytes を
// そのまま運ぶ（機密値を含みうるため加工しない / NFR 3.1）。
func scanUnassigned(row unassignedRowScanner) (UnassignedNotification, error) {
	var (
		n       UnassignedNotification
		typeStr string
		payload []byte
	)
	if err := row.Scan(
		&n.ID,
		&n.MessageID,
		&typeStr,
		&n.EnterpriseName,
		&payload,
		&n.ReceivedAt,
	); err != nil {
		return UnassignedNotification{}, err
	}
	n.NotificationType = NotificationType(typeStr)
	n.Payload = payload
	return n, nil
}

// payloadOrEmptyJSON は payload が空（nil / 長さ 0）のとき空 jsonb `{}` を返し、非空はそのまま
// 返す（jsonb NOT NULL 列に NULL を bind しないため / Req 3.5。audit.marshalDetail と同方針）。
func payloadOrEmptyJSON(payload []byte) []byte {
	if len(payload) == 0 {
		return []byte(emptyJSONObject)
	}
	return payload
}

// wrapUnassignedPersistErr は unassigned_notifications の永続化 / 参照失敗（DB 不通等）を
// transient な *errors.Error へ写像する純粋関数（Req 1.4 経路と同型）。
//
// IsTransient=true により Dispatcher は退避失敗を nack（再処理保持）に倒し、通知の喪失を
// 防ぐ（NFR 2.2 と整合）。IsTransient=true は errors.Wrap が設定しないため
// &errors.Error{...} をリテラル構築する（dedupe.wrapDedupePersistErr と同型）。message には
// 機密値（query 生値等）を補間しない固定文言を用いる（NFR 3.1）。
//
// BeginTxFunc の fn 内部（exec / query / scan 失敗）と外部（BeginTx / SetLocalTenant / Commit
// 失敗）の両方の wrap 点で共用するため、dedupe.wrapDedupePersistErr と同じく nil-safe かつ
// transient 既写像に対して idempotent にする:
//   - cause == nil               → nil
//   - cause が既に transient *Error → そのまま返す（二重 wrap 回避）
//   - それ以外（非 transient error） → transient *Error に包む（Req 1.4）
func wrapUnassignedPersistErr(cause error) error {
	if cause == nil {
		return nil
	}
	var de *pkgerrors.Error
	if stderrors.As(cause, &de) && de.IsTransient {
		return cause
	}
	return &pkgerrors.Error{
		Code:        pkgerrors.CodeUnavailable,
		Message:     "unassigned notification persistence failed",
		IsTransient: true,
		Cause:       cause,
	}
}
