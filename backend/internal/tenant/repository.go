package tenant

import (
	"context"
	stderrors "errors"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
)

// Repository は `tenants` テーブルへの raw SQL アクセス（SuperAdmin context +
// BeginTxFunc）と競合制御を集約する（design.md「tenant.Repository」節 / tasks.md 3.1）。
//
// すべてのメソッドは内部で SuperAdmin context（`db.TenantContext{IsSuperAdmin: true}`）を
// 確立した上で `db.BeginTxFunc` 経由でトランザクションを開く。Tenant 管理操作は admin-console
// 由来の SuperAdmin に限定され（Req 6.x / #37 ガード配下）、SuperAdmin のみ全 tenants 行を
// 可視にする RLS（`tenant_isolation_tenants` / migration 0011）と整合する。Repository は
// `internal/auth/repository.go` の `superAdminContext` + `BeginTxFunc` パターンを踏襲する。
type Repository interface {
	// Insert は status=pending_bind で tenants に 1 行 INSERT する（Req 1.1 / NFR 1.1 / 3.1）。
	//
	// enterprise_name は pending_bind では未確定のため NULL を入れる（TenantRow.EnterpriseName
	// が空文字なら NULL を書き込む）。INSERT 失敗（接続不通等）は `CodeUnavailable` で wrap する。
	Insert(ctx context.Context, t TenantRow) error

	// Get は id でテナント 1 行を取得する（Req 4.2）。
	//
	// 0 行（不在）の場合は `pgx.ErrNoRows` を `*errors.Error{Code: CodeNotFound}` に写像する
	// （Req 4.3 / 存在差を露出しない汎用 message）。enterprise_name の NULL は空文字に、
	// disabled_at / disabled_by の NULL は nil ポインタに写像する。
	Get(ctx context.Context, id uuid.UUID) (TenantRow, error)

	// List は全テナントを created_at 昇順で返す（Req 4.1）。
	//
	// 登録済みテナントが 0 件のときは非 nil の空 slice を返す（Req 4.4）。
	List(ctx context.Context) ([]TenantRow, error)

	// UpdateBound は pending_bind 状態のテナントを bound へ遷移させ enterprise_name を保存する
	// （Req 2.1 / 2.2 / 2.3）。
	//
	// `WHERE id=$ AND status='pending_bind'` の条件付き UPDATE で、影響行数（affected）を返す。
	// affected=0 は呼び出し側（Service）が「現状態が pending_bind でない＝競合」と判定する材料
	// （Req 2.5）。別テナントへの同一 enterprise_name 投入は部分一意 index 違反（pgerrcode
	// 23505 / `uq_tenants_enterprise_name`）となり `*errors.Error{Code: CodeConflict}` に写像する。
	UpdateBound(ctx context.Context, id uuid.UUID, enterpriseName string) (int64, error)

	// UpdateDisabled は disabled 以外の状態のテナントを disabled へ遷移させ、無効化監査列
	// （disabled_at=now() / disabled_by=actor）を記録する（Req 3.1）。
	//
	// `WHERE id=$ AND status!='disabled'` の条件付き UPDATE で、影響行数（affected）を返す。
	// affected=0 は呼び出し側（Service）が「既に disabled＝二重無効化の競合」と判定する材料
	// （Req 3.4）。
	UpdateDisabled(ctx context.Context, id uuid.UUID, actor uuid.UUID) (int64, error)
}

// repository は Repository の pgxpool ベース実装。
type repository struct {
	pool *pgxpool.Pool
}

// NewRepository は Repository を構築する。pool は cmd/api bootstrap が構築する
// 共有 pgxpool.Pool を渡す（`internal/auth.NewRepository` と同方式）。
func NewRepository(pool *pgxpool.Pool) Repository {
	return &repository{pool: pool}
}

// superAdminContext は ctx に SuperAdmin TenantContext を埋め込む。
//
// Tenant 管理操作は SuperAdmin 専用であり、全 tenants 行を可視にするには SuperAdmin
// context が必要（RLS `tenant_isolation_tenants` / 0011）。`internal/auth/repository.go` の
// 同名 helper と同型。TenantContext 未確立のまま BeginTxFunc を呼ぶと panic ガードに掛かる。
func superAdminContext(ctx context.Context) context.Context {
	return db.WithTenantContext(ctx, db.TenantContext{
		TenantID:     uuid.Nil,
		IsSuperAdmin: true,
	})
}

// nullableEnterpriseName は空文字を NULL（nil）へ、非空をそのまま *string へ写像する。
// pending_bind 行は enterprise_name を NULL で持つため、INSERT の引数化に用いる。
func nullableEnterpriseName(name string) *string {
	if name == "" {
		return nil
	}
	return &name
}

// Insert は Repository.Insert の実装。
func (r *repository) Insert(ctx context.Context, t TenantRow) error {
	ctx = superAdminContext(ctx)
	return db.BeginTxFunc(ctx, r.pool, func(tx pgx.Tx) error {
		// status は常に pending_bind で作成する（NFR 1.1 / 作成直後の初期状態）。
		// enterprise_name は未確定のため NULL を入れる（空文字 → NULL 写像）。
		if _, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, name, status, enterprise_name)
			 VALUES ($1, $2, $3, $4)`,
			t.ID, t.Name, string(StatusPendingBind), nullableEnterpriseName(t.EnterpriseName),
		); err != nil {
			return pkgerrors.Wrap(
				pkgerrors.CodeUnavailable,
				"tenant insert failed",
				err,
			)
		}
		return nil
	})
}

// scanTenantRow は 1 行分の tenants 列を TenantRow へ走査する（Get / List 共通）。
// enterprise_name の NULL は空文字へ写像する（TenantRow.EnterpriseName は string 表現）。
func scanTenantRow(row pgx.Row) (TenantRow, error) {
	var (
		t             TenantRow
		statusStr     string
		enterpriseSQL *string
	)
	if err := row.Scan(
		&t.ID, &t.Name, &statusStr, &enterpriseSQL,
		&t.CreatedAt, &t.UpdatedAt, &t.DisabledAt, &t.DisabledBy,
	); err != nil {
		return TenantRow{}, err
	}
	t.Status = Status(statusStr)
	if enterpriseSQL != nil {
		t.EnterpriseName = *enterpriseSQL
	}
	return t, nil
}

// Get は Repository.Get の実装。
func (r *repository) Get(ctx context.Context, id uuid.UUID) (TenantRow, error) {
	ctx = superAdminContext(ctx)
	var result TenantRow
	err := db.BeginTxFunc(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`SELECT id, name, status, enterprise_name, created_at, updated_at, disabled_at, disabled_by
			 FROM tenants
			 WHERE id = $1`,
			id,
		)
		t, err := scanTenantRow(row)
		if err != nil {
			if stderrors.Is(err, pgx.ErrNoRows) {
				// 不在は存在差を露出しない汎用 message で CodeNotFound に写像（Req 4.3 / 6.5）。
				return pkgerrors.Wrap(
					pkgerrors.CodeNotFound,
					"tenant not found",
					err,
				)
			}
			return pkgerrors.Wrap(
				pkgerrors.CodeUnavailable,
				"tenant lookup failed",
				err,
			)
		}
		result = t
		return nil
	})
	if err != nil {
		return TenantRow{}, err
	}
	return result, nil
}

// List は Repository.List の実装。
func (r *repository) List(ctx context.Context) ([]TenantRow, error) {
	ctx = superAdminContext(ctx)
	// 0 件でも非 nil の空 slice を返す（Req 4.4）。
	tenants := make([]TenantRow, 0)
	err := db.BeginTxFunc(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, name, status, enterprise_name, created_at, updated_at, disabled_at, disabled_by
			 FROM tenants
			 ORDER BY created_at ASC`,
		)
		if err != nil {
			return pkgerrors.Wrap(
				pkgerrors.CodeUnavailable,
				"tenant list query failed",
				err,
			)
		}
		defer rows.Close()
		for rows.Next() {
			t, scanErr := scanTenantRow(rows)
			if scanErr != nil {
				return pkgerrors.Wrap(
					pkgerrors.CodeUnavailable,
					"tenant row scan failed",
					scanErr,
				)
			}
			tenants = append(tenants, t)
		}
		if err := rows.Err(); err != nil {
			return pkgerrors.Wrap(
				pkgerrors.CodeUnavailable,
				"tenant list iteration failed",
				err,
			)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return tenants, nil
}

// UpdateBound は Repository.UpdateBound の実装。
func (r *repository) UpdateBound(ctx context.Context, id uuid.UUID, enterpriseName string) (int64, error) {
	ctx = superAdminContext(ctx)
	var affected int64
	err := db.BeginTxFunc(ctx, r.pool, func(tx pgx.Tx) error {
		// WHERE status='pending_bind' で条件付き UPDATE する。bound / disabled には遷移しない
		// （affected=0 を Service が競合 / 不正状態として写像する / Req 2.2 / 2.5）。
		ct, err := tx.Exec(ctx,
			`UPDATE tenants
			 SET status = 'bound', enterprise_name = $1, updated_at = now()
			 WHERE id = $2 AND status = 'pending_bind'`,
			enterpriseName, id,
		)
		if err != nil {
			// 別テナントへの同一 enterprise_name は部分一意 index 違反（23505）→ 競合（Req 2.1）。
			var pgErr *pgconn.PgError
			if stderrors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation {
				return pkgerrors.Wrap(
					pkgerrors.CodeConflict,
					"enterprise name already bound to another tenant",
					err,
				)
			}
			return pkgerrors.Wrap(
				pkgerrors.CodeUnavailable,
				"tenant bind update failed",
				err,
			)
		}
		affected = ct.RowsAffected()
		return nil
	})
	if err != nil {
		return 0, err
	}
	return affected, nil
}

// UpdateDisabled は Repository.UpdateDisabled の実装。
func (r *repository) UpdateDisabled(ctx context.Context, id uuid.UUID, actor uuid.UUID) (int64, error) {
	ctx = superAdminContext(ctx)
	var affected int64
	err := db.BeginTxFunc(ctx, r.pool, func(tx pgx.Tx) error {
		// WHERE status!='disabled' で条件付き UPDATE する。既に disabled の行は更新されず
		// affected=0 となり、Service が二重無効化の競合として写像する（Req 3.4）。
		ct, err := tx.Exec(ctx,
			`UPDATE tenants
			 SET status = 'disabled', disabled_at = now(), disabled_by = $1, updated_at = now()
			 WHERE id = $2 AND status != 'disabled'`,
			actor, id,
		)
		if err != nil {
			return pkgerrors.Wrap(
				pkgerrors.CodeUnavailable,
				"tenant disable update failed",
				err,
			)
		}
		affected = ct.RowsAffected()
		return nil
	})
	if err != nil {
		return 0, err
	}
	return affected, nil
}
