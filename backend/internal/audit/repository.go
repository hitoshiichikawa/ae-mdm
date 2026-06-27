package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
)

// insertAuditLogSQL は audit_logs への append-only INSERT 文（列順を固定）。
//
// detail は jsonb（bytea ではなく json.Marshal 済みバイト列を bind）、tenant_id は NULL 許容。
const insertAuditLogSQL = `INSERT INTO audit_logs ` +
	`(id, tenant_id, actor_id, event_type, resource_id, detail, result, occurred_at) ` +
	`VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`

// emptyJSONObject は Detail が nil のときに bind する空 jsonb（既存スキーマ
// `detail jsonb NOT NULL DEFAULT '{}'` と整合させ、NULL ではなく `{}` を格納する / Req 1.1）。
const emptyJSONObject = "{}"

// repository は Repository の pgxpool ベース実装。
//
// 自テナント分離 / SuperAdmin 横断可視は呼び出し側が確立した ctx の TenantContext と RLS が
// 担うため、Repository は SELECT の WHERE 句に自テナント条件を書かない（Req 2.9 / 3.5 / NFR 2.1）。
type repository struct {
	pool *pgxpool.Pool
}

// NewRepository は audit_logs に対する append-only INSERT と保持下限付き SELECT を提供する
// Repository を構築する。
//
// pool は cmd/api bootstrap が構築する共有 pgxpool.Pool を渡す。Repository は INSERT / SELECT の
// みを提供し UPDATE / DELETE を発行するメソッドを持たない（Req 1.5 / 6.1 を IF レベルで担保。
// 記録後の不変性は DB 層の append-only 強制と二重に担保される）。
func NewRepository(pool *pgxpool.Pool) Repository {
	return &repository{pool: pool}
}

// Insert は Repository.Insert の実装。Event を audit_logs へ 1 レコード追記する（Req 1.1 / 1.2）。
//
// Detail は json.Marshal して jsonb bind（nil は空 jsonb `{}` / 既存スキーマ DEFAULT と整合）、
// TenantID == uuid.Nil は NULL bind する（Req 1.2）。永続化失敗は
// *errors.Error{Code: CodeUnavailable} で wrap し（Cause に DB err / 機密値や query 生値を
// message 本文へ補間しない / Req 1.6 / NFR 3.1）、成功扱いしない。
func (r *repository) Insert(ctx context.Context, ev Event) error {
	detailJSON, err := marshalDetail(ev.Detail)
	if err != nil {
		// detail の直列化失敗は呼び出し側の不正入力に近いが、永続化前段の失敗として
		// fail-closed で扱う。機密値（detail 生値）は message 本文に補間しない（NFR 3.1）。
		return pkgerrors.Wrap(
			pkgerrors.CodeUnavailable,
			"audit log detail marshal failed",
			err,
		)
	}

	return db.BeginTxFunc(ctx, r.pool, func(tx pgx.Tx) error {
		_, execErr := tx.Exec(ctx,
			insertAuditLogSQL,
			ev.ID,
			nullableTenantID(ev.TenantID),
			ev.ActorID,
			string(ev.EventType),
			ev.ResourceID,
			detailJSON,
			string(ev.Result),
			ev.OccurredAt,
		)
		if execErr != nil {
			// 機密値（detail 生値）・query 生値は wrap message に補間しない（fixed message / NFR 3.1）。
			return pkgerrors.Wrap(
				pkgerrors.CodeUnavailable,
				"audit log insert failed",
				execErr,
			)
		}
		return nil
	})
}

// Select は Repository.Select の実装。effectiveFrom（保持下限）と Filter から動的 SQL を
// 組み立て occurred_at 降順で取得する（Req 2.x / 3.x / 5.2 / 5.3）。
//
// WHERE 句組み立ては純粋関数 buildSelectQuery に委ね、本メソッドは BeginTxFunc 経由で実行と
// scan のみを担う。SELECT / scan 失敗は *errors.Error{Code: CodeUnavailable} で wrap する
// （NFR 3.2）。0 行は空 slice + nil を返す（Req 2.8 / 3.4）。
func (r *repository) Select(ctx context.Context, f Filter, effectiveFrom time.Time) ([]Event, error) {
	sql, args := buildSelectQuery(f, effectiveFrom)

	events := make([]Event, 0)
	err := db.BeginTxFunc(ctx, r.pool, func(tx pgx.Tx) error {
		rows, queryErr := tx.Query(ctx, sql, args...)
		if queryErr != nil {
			return pkgerrors.Wrap(
				pkgerrors.CodeUnavailable,
				"audit log select failed",
				queryErr,
			)
		}
		defer rows.Close()

		for rows.Next() {
			ev, scanErr := scanEvent(rows)
			if scanErr != nil {
				return pkgerrors.Wrap(
					pkgerrors.CodeUnavailable,
					"audit log scan failed",
					scanErr,
				)
			}
			events = append(events, ev)
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			return pkgerrors.Wrap(
				pkgerrors.CodeUnavailable,
				"audit log rows iteration failed",
				rowsErr,
			)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return events, nil
}

// selectAuditLogColumns は SELECT 句の列順（scanEvent の Scan 順と一致させる）。
const selectAuditLogColumns = `id, tenant_id, actor_id, event_type, resource_id, detail, result, occurred_at`

// buildSelectQuery は effectiveFrom（保持下限）と Filter から動的 SQL と $n bind args を
// 組み立てる純粋関数。実 DB に依存せず単体テスト可能にするための切り出し。
//
// WHERE 句組み立て規約（design.md「Audit Repository」Invariants と整合）:
//   - occurred_at >= effectiveFrom を **常に** 付与（保持下限 / Req 5.2 / 5.3）
//   - Filter.To != nil で occurred_at <= $n（Req 2.7）
//   - Filter.EventType != "" で event_type = $n（Req 2.2 / 3.3）
//   - Filter.ActorID != "" で actor_id = $n（Req 2.3 / 3.3）
//   - Filter.ResourceID != "" で resource_id = $n（Req 2.4 / 3.3）
//   - Filter.TenantID != nil で tenant_id = $n（admin 横断の明示絞り込みのみ / Req 3.2）
//   - 自テナント条件は書かず RLS に委ねる（Req 2.6 / 2.9 / 3.5 / NFR 2.1）
//   - ORDER BY occurred_at DESC（Req 2.1 / 3.1）
//
// すべて pgx の $n プレースホルダで bind し SQL injection を回避する。args の順序は WHERE 句に
// 追加した順（$1 が effectiveFrom）であり、戻り値の sql 中の $n と 1:1 で対応する。
func buildSelectQuery(f Filter, effectiveFrom time.Time) (string, []any) {
	conditions := make([]string, 0, 6)
	args := make([]any, 0, 6)

	// 保持下限は常に付与する（Req 5.2 / 5.3）。
	args = append(args, effectiveFrom)
	conditions = append(conditions, fmt.Sprintf("occurred_at >= $%d", len(args)))

	if f.To != nil {
		args = append(args, *f.To)
		conditions = append(conditions, fmt.Sprintf("occurred_at <= $%d", len(args)))
	}
	if f.EventType != "" {
		args = append(args, string(f.EventType))
		conditions = append(conditions, fmt.Sprintf("event_type = $%d", len(args)))
	}
	if f.ActorID != "" {
		args = append(args, f.ActorID)
		conditions = append(conditions, fmt.Sprintf("actor_id = $%d", len(args)))
	}
	if f.ResourceID != "" {
		args = append(args, f.ResourceID)
		conditions = append(conditions, fmt.Sprintf("resource_id = $%d", len(args)))
	}
	if f.TenantID != nil {
		args = append(args, *f.TenantID)
		conditions = append(conditions, fmt.Sprintf("tenant_id = $%d", len(args)))
	}

	sql := "SELECT " + selectAuditLogColumns + " FROM audit_logs WHERE " +
		strings.Join(conditions, " AND ") +
		" ORDER BY occurred_at DESC"
	return sql, args
}

// rowScanner は scanEvent が依存する最小 interface（pgx.Rows / pgx.Row が満たす）。
type rowScanner interface {
	Scan(dest ...any) error
}

// scanEvent は audit_logs の 1 行を Event に写像する。
//
// tenant_id が NULL の行は Event.TenantID = uuid.Nil に戻す（NULL テナント / Req 1.2）。
// detail(jsonb) は map[string]any に unmarshal する。
func scanEvent(row rowScanner) (Event, error) {
	var (
		ev         Event
		tenantID   *uuid.UUID
		eventType  string
		resourceID string
		detailJSON []byte
		result     string
	)
	if err := row.Scan(
		&ev.ID,
		&tenantID,
		&ev.ActorID,
		&eventType,
		&resourceID,
		&detailJSON,
		&result,
		&ev.OccurredAt,
	); err != nil {
		return Event{}, err
	}

	if tenantID != nil {
		ev.TenantID = *tenantID
	}
	ev.EventType = EventType(eventType)
	ev.ResourceID = resourceID
	ev.Result = ResultType(result)

	detail, err := unmarshalDetail(detailJSON)
	if err != nil {
		return Event{}, err
	}
	ev.Detail = detail
	return ev, nil
}

// marshalDetail は Detail map を jsonb bind 用のバイト列に直列化する。
//
// nil の場合は空 jsonb `{}` を返す（既存スキーマ DEFAULT '{}' と整合 / NULL を bind しない）。
func marshalDetail(detail map[string]any) ([]byte, error) {
	if detail == nil {
		return []byte(emptyJSONObject), nil
	}
	return json.Marshal(detail)
}

// unmarshalDetail は jsonb 列のバイト列を map[string]any に復元する。
//
// 空・NULL 相当（長さ 0）の場合は空 map を返す（保持下限の付与で NOT NULL 列を読むため通常は
// 非空だが、防御的に空 map を返す）。
func unmarshalDetail(detailJSON []byte) (map[string]any, error) {
	if len(detailJSON) == 0 {
		return map[string]any{}, nil
	}
	detail := make(map[string]any)
	if err := json.Unmarshal(detailJSON, &detail); err != nil {
		return nil, err
	}
	return detail, nil
}

// nullableTenantID は uuid.Nil を NULL bind（nil *uuid.UUID）に、それ以外を非 NULL bind に
// 写像する（Req 1.2 = NULL テナントは tenant_id を NULL として bind する）。
func nullableTenantID(tenantID uuid.UUID) *uuid.UUID {
	if tenantID == uuid.Nil {
		return nil
	}
	return &tenantID
}
