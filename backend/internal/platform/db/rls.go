package db

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	internalerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// SetLocalTenant は tx 内で PostgreSQL の GUC `app.tenant_id` と
// `app.is_superadmin` を tx-local（is_local=true）で設定する。RLS ポリシーが
// `current_setting('app.tenant_id', true)::uuid` / `current_setting('app.is_superadmin', true)::boolean`
// で参照する前提で、本ヘルパが発行する SQL によって二重防御の DB 層が成立する
// （requirements.md Req 4.3 / 4.4 / NFR 1.1）。
//
// 設計上の根拠（design.md Components: TxManager + RLS Helper 節）:
//   - PostgreSQL の `SET LOCAL ... = $1` はバインドパラメータ不可のため、
//     `SELECT set_config(key, value, is_local=true)` 形式で発行する。
//   - SuperAdmin の cross-tenant 操作（TenantID == uuid.Nil）では
//     `app.tenant_id` を **set しない**（current_setting が NULL → default deny に倒れる）。
//   - SuperAdmin の場合は `app.is_superadmin = 'true'` を追加発行し、RLS ポリシーの
//     OR 句で全テナント横断アクセスを許可する。
//
// 通常運用では本関数は BeginTxFunc から内部呼び出しされ、外部から直接呼び出すことは
// 想定しない（design.md「外部から直接呼ぶことは想定しない」）。
func SetLocalTenant(ctx context.Context, tx pgx.Tx, tc TenantContext) error {
	if tx == nil {
		return internalerrors.New(
			internalerrors.CodeInternal,
			"db: SetLocalTenant に nil tx が渡された",
		)
	}

	// SuperAdmin の cross-tenant 操作（TenantID==uuid.Nil）では app.tenant_id を set しない。
	// それ以外のすべてのリクエストでは TenantID を tx-local GUC として設定する。
	if !(tc.IsSuperAdmin && tc.TenantID == uuid.Nil) {
		if _, err := tx.Exec(
			ctx,
			"SELECT set_config('app.tenant_id', $1, true)",
			tc.TenantID.String(),
		); err != nil {
			return internalerrors.Wrap(
				internalerrors.CodeInternal,
				"db: app.tenant_id の設定に失敗",
				err,
			)
		}
	}

	if tc.IsSuperAdmin {
		if _, err := tx.Exec(
			ctx,
			"SELECT set_config('app.is_superadmin', 'true', true)",
		); err != nil {
			return internalerrors.Wrap(
				internalerrors.CodeInternal,
				"db: app.is_superadmin の設定に失敗",
				err,
			)
		}
	}

	return nil
}
