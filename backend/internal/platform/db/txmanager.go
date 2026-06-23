package db

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	internalerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// txBeginner は BeginTxFunc が依存する最小 interface。*pgxpool.Pool が自然に満たし、
// テスト時には fake 実装を注入できるようにするための extension point。
//
// 公開 API（BeginTxFunc）の signature は design.md（Components: TxManager + RLS Helper 節）に
// 揃えて *pgxpool.Pool を受けるが、内部で本 interface 経由に縛ることで unit test が
// 実 PostgreSQL に依存せず panic ガード / rollback / re-panic / commit の挙動を検証できる。
type txBeginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// BeginTxFunc は DB トランザクション境界を **関数で囲う**形に統一するエントリポイント。
//
// 動作（requirements.md Req 4.2 / 4.3 / 4.4 / 4.5 / NFR 1.1 / design.md Components:
// TxManager + RLS Helper 節と整合）:
//   1. ctx に TenantContext が無ければ *errors.Error{Code: CodeTenantCtxMissing} を panic する
//      （上位 recover middleware が 500 + 構造化 ERROR ログに写像する前提 / task 4.1 で配線）。
//   2. pool.BeginTx(ctx, pgx.TxOptions{}) で tx を開始する。
//   3. SetLocalTenant(ctx, tx, tc) を発行（失敗時は rollback してから error を返す）。
//   4. fn(tx) を実行する。
//      - fn が error を返した → rollback + その error をそのまま返す
//      - fn が panic した → rollback + re-panic（recover はせず上位 middleware に委譲）
//      - 正常終了 → commit（commit エラーは *errors.Error{Code: CodeInternal} で wrap）
//
// nested tx は本 Issue では未対応（design.md 同節「nested tx は本 Issue では未対応」）。
func BeginTxFunc(
	ctx context.Context,
	pool *pgxpool.Pool,
	fn func(tx pgx.Tx) error,
) error {
	if pool == nil {
		return internalerrors.New(
			internalerrors.CodeInternal,
			"db: BeginTxFunc に nil pool が渡された",
		)
	}
	return beginTxFuncWith(ctx, pool, fn)
}

// beginTxFuncWith は txBeginner interface 経由で動く実装本体。テストでは fake pool を
// 渡せるよう公開 API から分離してある。
func beginTxFuncWith(
	ctx context.Context,
	pool txBeginner,
	fn func(tx pgx.Tx) error,
) error {
	tc, err := FromContext(ctx)
	if err != nil {
		// 通常運用では発生しないことが invariant。recover middleware で 500 化される
		// 前提で *errors.Error{Code: CodeTenantCtxMissing} を panic する。
		panic(internalerrors.New(
			internalerrors.CodeTenantCtxMissing,
			"db: BeginTxFunc に TenantContext が確立されていない ctx が渡された",
		))
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return internalerrors.Wrap(
			internalerrors.CodeInternal,
			"db: tx の開始に失敗",
			err,
		)
	}

	if err := setLocalAndInvoke(ctx, tx, tc, fn); err != nil {
		// setLocalAndInvoke 側で rollback 済み。
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return internalerrors.Wrap(
			internalerrors.CodeInternal,
			"db: tx の commit に失敗",
			err,
		)
	}
	return nil
}

// setLocalAndInvoke は GUC 設定 → fn 実行 → 必要なら rollback までを担う内部 helper。
// fn の panic は recover せず再 panic させるため、本関数は fn の panic 時に
// rollback してから re-panic する責務を持つ。
func setLocalAndInvoke(
	ctx context.Context,
	tx pgx.Tx,
	tc TenantContext,
	fn func(tx pgx.Tx) error,
) (retErr error) {
	// fn の panic 時に rollback + re-panic する。
	defer func() {
		if p := recover(); p != nil {
			// 戻り値の error を上書きしないようにし、rollback してから re-panic する。
			_ = tx.Rollback(ctx)
			panic(p)
		}
	}()

	if err := SetLocalTenant(ctx, tx, tc); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}

	if err := fn(tx); err != nil {
		_ = tx.Rollback(ctx)
		return err
	}
	return nil
}
