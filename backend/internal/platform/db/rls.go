package db

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	internalerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// SetLocalTenant は tx 内で PostgreSQL の GUC `app.tenant_id` と
// `app.is_superadmin` を tx-local（is_local=true）で **両方とも毎回** 設定する。
// RLS ポリシーが
// `current_setting('app.tenant_id', true)::uuid` / `current_setting('app.is_superadmin', true)::boolean`
// で参照する前提で、本ヘルパが発行する SQL によって二重防御の DB 層が成立する
// （requirements.md Req 4.3 / 4.4 / NFR 1.1）。
//
// 設計上の根拠（design.md Components: TxManager + RLS Helper 節 / PR #31 round-3 review 由来）:
//   - PostgreSQL の `SET LOCAL ... = $1` はバインドパラメータ不可のため、
//     `SELECT set_config(key, value, is_local=true)` 形式で発行する。
//   - 一方の GUC だけを set する設計は、pgxpool で再利用された connection に
//     前回 tx の残骸（空文字 / 不正型）が残っていた場合や 0011_enable_rls.up.sql の
//     `::uuid` / `::boolean` cast で `invalid input syntax` を引き起こすリスクがあるため、
//     **両 GUC を常に有効値で初期化**する方針へ切り替える（round-3 high 指摘）。
//   - SuperAdmin の cross-tenant 操作（TenantID == uuid.Nil）では
//     `app.tenant_id` に `uuid.Nil.String()`（"00000000-0000-0000-0000-000000000000"）を
//     セットし、`app.is_superadmin = 'true'` を併発行。OR 句の RLS ポリシーは
//     is_superadmin 経由で全行可視となる（tenant_id 比較は zero uuid と既存行が一致しない
//     ため副作用無し）。
//   - 通常テナント文脈では `app.tenant_id = tc.TenantID.String()` と
//     `app.is_superadmin = 'false'` を発行し、cast 例外と GUC 未初期化の両方を排除する。
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

	tenantIDStr := tc.TenantID.String()
	if tc.IsSuperAdmin && tc.TenantID == uuid.Nil {
		// SuperAdmin / cross-tenant: zero uuid を入れて cast 例外を回避する。
		// 既存行で id=uuid.Nil の tenant_id を持つ行は存在しないため、
		// tenant_id 比較は常に false になり is_superadmin 経由のみで全行可視。
		tenantIDStr = uuid.Nil.String()
	}

	if _, err := tx.Exec(
		ctx,
		"SELECT set_config('app.tenant_id', $1, true)",
		tenantIDStr,
	); err != nil {
		return internalerrors.Wrap(
			internalerrors.CodeInternal,
			"db: app.tenant_id の設定に失敗",
			err,
		)
	}

	isSuperAdminStr := "false"
	if tc.IsSuperAdmin {
		isSuperAdminStr = "true"
	}
	if _, err := tx.Exec(
		ctx,
		"SELECT set_config('app.is_superadmin', $1, true)",
		isSuperAdminStr,
	); err != nil {
		return internalerrors.Wrap(
			internalerrors.CodeInternal,
			"db: app.is_superadmin の設定に失敗",
			err,
		)
	}

	return nil
}
