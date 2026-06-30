package tenant

import (
	"context"
	stderrors "errors"
	"time"

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

	// UpdateBound は binding（予約中）状態のテナントを bound へ遷移させ enterprise_name を保存する
	// （Req 1.4 / 2 段確定の確定側）。
	//
	// `WHERE id=$ AND status='binding'` の条件付き UPDATE で、影響行数（affected）を返す。
	// bound への確定は予約勝者（binding 行）からのみ許可する（#52 で WHERE を pending_bind→binding
	// へ変更 / Req 1.4）。affected=0 は呼び出し側（Service）が「現状態が binding でない＝競合 /
	// 既に解放・無効化された」と判定する材料（Req 1.2 / 1.3）。別テナントへの同一 enterprise_name
	// 投入は部分一意 index 違反（pgerrcode 23505 / `uq_tenants_enterprise_name`）となり
	// `*errors.Error{Code: CodeConflict}` に写像する。
	UpdateBound(ctx context.Context, id uuid.UUID, enterpriseName string) (int64, error)

	// ReserveBinding は pending_bind 状態のテナントを binding（予約中）へ原子遷移する（Req 1.1）。
	//
	// `WHERE id=$ AND status='pending_bind'` の条件付き UPDATE で、影響行数（affected）を返す。
	// 並行 bind の勝者のみ affected=1 となり、敗者は affected=0 となる（Service が 409 競合・
	// CreateEnterprise 未呼出と判定する材料 / Req 1.2 / 1.3 の orphan 防止の核）。
	ReserveBinding(ctx context.Context, id uuid.UUID) (int64, error)

	// ReleaseBinding は binding（予約中）状態のテナントを pending_bind へ戻す（Req 1.5）。
	//
	// `WHERE id=$ AND status='binding'` の条件付き UPDATE で、影響行数（affected）を返す。
	// CreateEnterprise 失敗時に予約勝者の binding 行を pending_bind へ解放し、後続の再 bind を
	// 可能化する（enterprise 識別子未保存のまま回復 / Req 4.3）。affected=0 は当該行が既に
	// binding でない（解放済み / 無効化済み等）ことを示す。
	ReleaseBinding(ctx context.Context, id uuid.UUID) (int64, error)

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

// nullableString は空文字を NULL（nil）へ、非空をそのまま *string へ写像する汎用ヘルパ。
//
// nullable な text 列（enterprise_name / signup_url_name）の INSERT 引数化に用いる。
// pending_bind 行は enterprise_name を NULL で持ち、signup_url_name も未発行時は NULL とする。
// DB に依存しない純粋写像であり、in-package 単体テストの seam を提供する（DRY・単一責務）。
func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Insert は Repository.Insert の実装。
func (r *repository) Insert(ctx context.Context, t TenantRow) error {
	ctx = superAdminContext(ctx)
	return db.BeginTxFunc(ctx, r.pool, func(tx pgx.Tx) error {
		// status は常に pending_bind で作成する（NFR 1.1 / 作成直後の初期状態）。
		// enterprise_name は未確定のため NULL を入れる（空文字 → NULL 写像）。
		// signup_url_name は発行元束縛の正本（Req 3.1）。未発行（空文字）は NULL を書く。
		if _, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, name, status, enterprise_name, signup_url_name)
			 VALUES ($1, $2, $3, $4, $5)`,
			t.ID, t.Name, string(StatusPendingBind),
			nullableString(t.EnterpriseName), nullableString(t.SignupURLName),
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
// enterprise_name / signup_url_name の NULL は空文字へ写像する（TenantRow 側は string 表現）。
//
// SELECT 列順と Scan 引数順は厳密に一致させる必要がある。signup_url_name は
// enterprise_name の直後に並べる（Get / List の SELECT 文も同順で signup_url_name を含む）。
func scanTenantRow(row pgx.Row) (TenantRow, error) {
	var (
		t             TenantRow
		statusStr     string
		enterpriseSQL *string
		signupURLSQL  *string
	)
	if err := row.Scan(
		&t.ID, &t.Name, &statusStr, &enterpriseSQL, &signupURLSQL,
		&t.CreatedAt, &t.UpdatedAt, &t.DisabledAt, &t.DisabledBy,
	); err != nil {
		return TenantRow{}, err
	}
	t.Status = Status(statusStr)
	if enterpriseSQL != nil {
		t.EnterpriseName = *enterpriseSQL
	}
	if signupURLSQL != nil {
		t.SignupURLName = *signupURLSQL
	}
	return t, nil
}

// Get は Repository.Get の実装。
func (r *repository) Get(ctx context.Context, id uuid.UUID) (TenantRow, error) {
	ctx = superAdminContext(ctx)
	var result TenantRow
	err := db.BeginTxFunc(ctx, r.pool, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`SELECT id, name, status, enterprise_name, signup_url_name, created_at, updated_at, disabled_at, disabled_by
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
			`SELECT id, name, status, enterprise_name, signup_url_name, created_at, updated_at, disabled_at, disabled_by
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
		// WHERE status='binding' で条件付き UPDATE する。bound への確定は予約勝者（binding）
		// からのみ許可し、pending_bind / bound / disabled には遷移しない（2 段確定の核 / Req 1.4）。
		// affected=0 を Service が競合 / 不正状態として写像する（Req 1.2 / 1.3）。
		ct, err := tx.Exec(ctx,
			`UPDATE tenants
			 SET status = 'bound', enterprise_name = $1, updated_at = now()
			 WHERE id = $2 AND status = 'binding'`,
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

// ReserveBinding は pending_bind 状態のテナントを binding（予約中）へ原子遷移する（Req 1.1）。
//
// `WHERE id=$ AND status='pending_bind'` の条件付き UPDATE で、影響行数（affected）を返す。
// 同一テナントへ並行 bind が到達した場合、本遷移に成功した 1 要求のみが affected=1（勝者）と
// なり、敗者は affected=0 となる（Service が 409 競合・CreateEnterprise 未呼出と判定する材料 /
// Req 1.2 / 1.3 の orphan 防止の核）。affected rows 実挙動の検証は task 7.1 の integration test
// へ deferred（実 PostgreSQL を要するため）。
//
// 本メソッドは `Repository` interface 宣言を持つ（消費側 Service が task 4.1 で interface 経由で
// 呼ぶ）。
func (r *repository) ReserveBinding(ctx context.Context, id uuid.UUID) (int64, error) {
	ctx = superAdminContext(ctx)
	var affected int64
	err := db.BeginTxFunc(ctx, r.pool, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx,
			`UPDATE tenants
			 SET status = 'binding', updated_at = now()
			 WHERE id = $1 AND status = 'pending_bind'`,
			id,
		)
		if err != nil {
			return pkgerrors.Wrap(
				pkgerrors.CodeUnavailable,
				"tenant binding reserve failed",
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

// ReleaseBinding は binding（予約中）状態のテナントを pending_bind へ戻す（Req 1.5）。
//
// `WHERE id=$ AND status='binding'` の条件付き UPDATE で、影響行数（affected）を返す。
// CreateEnterprise 失敗時に予約勝者の binding 行を pending_bind へ解放し、後続の再 bind を
// 可能化する（enterprise 識別子未保存のまま回復 / Req 4.3）。affected=0 は当該行が既に binding
// でない（解放済み / 無効化済み等）ことを示す。affected rows 実挙動の検証は task 7.1 の
// integration test へ deferred。
//
// 本メソッドは `Repository` interface 宣言を持つ（消費側 Service が task 4.1 で interface 経由で
// 呼ぶ）。
func (r *repository) ReleaseBinding(ctx context.Context, id uuid.UUID) (int64, error) {
	ctx = superAdminContext(ctx)
	var affected int64
	err := db.BeginTxFunc(ctx, r.pool, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx,
			`UPDATE tenants
			 SET status = 'pending_bind', updated_at = now()
			 WHERE id = $1 AND status = 'binding'`,
			id,
		)
		if err != nil {
			return pkgerrors.Wrap(
				pkgerrors.CodeUnavailable,
				"tenant binding release failed",
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

// RecoverStaleBindings は updated_at が閾値より古い binding 行を pending_bind へ一括回収する（Req 2.1）。
//
// `WHERE status='binding' AND updated_at < now() - make_interval(secs => $1) RETURNING id` で
// 中断した予約（クラッシュ / AMAPI タイムアウトで binding のまま放置された行）を再び bind 可能・
// 無効化可能な状態へ戻し、回収された tenant id 群を返す。しきい値は DB 側 `now()` 基準で評価する
// （アプリ / DB のクロック乖離を避ける / design L289-293）。olderThan は秒（float64）として渡す。
// 回収が 0 件でも非 nil の空 slice を返す（List の慣習踏襲）。RLS 下挙動・しきい値境界の検証は
// task 7.1 の integration test へ deferred。
//
// 本メソッドは `Repository` interface には未宣言（具象 `*repository` メソッドのみ）。interface
// 宣言と fakeRepository 追従は消費側 task 5.2 / 4.1 へ deferred する（build-safe）。
func (r *repository) RecoverStaleBindings(ctx context.Context, olderThan time.Duration) ([]uuid.UUID, error) {
	ctx = superAdminContext(ctx)
	// 0 件でも非 nil の空 slice を返す（Req 2.1 / List と同型）。
	recovered := make([]uuid.UUID, 0)
	err := db.BeginTxFunc(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`UPDATE tenants
			 SET status = 'pending_bind', updated_at = now()
			 WHERE status = 'binding' AND updated_at < now() - make_interval(secs => $1)
			 RETURNING id`,
			olderThan.Seconds(),
		)
		if err != nil {
			return pkgerrors.Wrap(
				pkgerrors.CodeUnavailable,
				"tenant stale binding recover failed",
				err,
			)
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if scanErr := rows.Scan(&id); scanErr != nil {
				return pkgerrors.Wrap(
					pkgerrors.CodeUnavailable,
					"tenant recovered id scan failed",
					scanErr,
				)
			}
			recovered = append(recovered, id)
		}
		if err := rows.Err(); err != nil {
			return pkgerrors.Wrap(
				pkgerrors.CodeUnavailable,
				"tenant stale binding recover iteration failed",
				err,
			)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return recovered, nil
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
