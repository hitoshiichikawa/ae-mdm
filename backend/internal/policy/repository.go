package policy

import (
	"context"
	stderrors "errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
)

// Repository は `policies` の CRUD と `devices.applied_policy_id` UPDATE を raw pgx + RLS で
// 集約する（design.md「policy.Repository」節 / tasks.md 2.1）。
//
// すべてのメソッドは tenant-scoped な `TenantContext`（Handler が継承する自テナント文脈）の
// まま `db.BeginTxFunc(ctx, pool, ...)` で tx を開き、RLS（`tenant_isolation_policies` /
// migration 0011）による自テナント限定に分離を委ねる。tenant.Repository / auth.Repository が
// SuperAdmin context へ昇格するのとは対照的に、Policy 操作は **昇格しない**（他テナント行は
// SELECT で 0 行、UPDATE/INSERT は複合 FK で物理拒否される / design「テナント分離」節）。
type Repository interface {
	// Insert は AMAPI 反映後の policy snapshot を 1 行 INSERT する（Req 1.3）。
	//
	// 永続化失敗（接続不通等）は `*errors.Error{Code: CodeUnavailable}` で wrap する。
	Insert(ctx context.Context, row PolicyRow) error

	// Update は既存 policy の snapshot を更新し影響行数を返す（Req 1.5）。
	//
	// `WHERE id=$ AND tenant_id=$` の条件付き UPDATE で、affected=0 は呼び出し側（Service）が
	// 「不在 / 他テナント＝NotFound」と判定する材料（Req 4.4 / 4.5）。error には倒さない。
	Update(ctx context.Context, row PolicyRow) (int64, error)

	// Get は id で自テナントの policy 1 行を取得する（Req 4.4）。
	//
	// 0 行（不在 / RLS により他テナント行も 0 行）は存在差を露出しない `ErrPolicyNotFound`
	// （CodeNotFound / 汎用 message）に写像する（Req 4.2 / 4.4 / 4.5）。
	Get(ctx context.Context, tenantID, policyID uuid.UUID) (PolicyRow, error)

	// List は自テナントの policy 一覧を created_at 昇順で返す（Req 4.4）。
	//
	// 0 件のときは非 nil の空 slice を返す。RLS により自テナント行のみが可視（Req 4.2 / 4.4）。
	List(ctx context.Context, tenantID uuid.UUID) ([]PolicyRow, error)

	// Delete は自テナントの policy を削除し影響行数を返す（Req 5.3）。
	//
	// `WHERE id=$ AND tenant_id=$` の条件付き DELETE で、affected=0 は Service が NotFound と
	// 判定する材料。割当済み端末が存在する policy の DELETE は複合 FK 違反（pgerrcode 23503）に
	// なり、`ErrDeleteConflict`（CodeConflict / 409）へ写像する（design 確認事項 3 推奨案 / Req 5.3）。
	Delete(ctx context.Context, tenantID, policyID uuid.UUID) (int64, error)

	// AssignPolicyToDevice は自テナント device の applied_policy_id を policyID へ UPDATE し
	// 影響行数を返す（Req 3.1 / 4.3）。
	//
	// `WHERE id=$deviceID AND tenant_id=$tenantID` で行う。
	//   - 他テナント device 指定 → affected=0 を `(0, nil)` で返し Service が NotFound 判定（Req 3.3）。
	//   - 他テナント policy 指定 → 複合 FK `(applied_policy_id, tenant_id)→policies(id,tenant_id)`
	//     違反（23503）を `ErrPolicyNotFound`（CodeNotFound / 存在差非露出）に写像（Req 3.2 / 4.2 / 4.5）。
	AssignPolicyToDevice(ctx context.Context, tenantID, deviceID, policyID uuid.UUID) (int64, error)
}

// repository は Repository の pgxpool ベース実装。
type repository struct {
	pool *pgxpool.Pool
}

// NewRepository は Repository を構築する。pool は cmd/api bootstrap が構築する共有
// pgxpool.Pool を渡す（tenant.NewRepository / audit.NewRepository と同方式）。
func NewRepository(pool *pgxpool.Pool) Repository {
	return &repository{pool: pool}
}

// rowScanner は scanPolicyRow が依存する最小 interface（pgx.Rows / pgx.Row が満たす）。
// 実 PostgreSQL に依存せず scanPolicyRow を単体テストするための extension point
// （audit.Repository の rowScanner と同型）。
type rowScanner interface {
	Scan(dest ...any) error
}

// selectPolicyColumns は SELECT 句の列順（scanPolicyRow の Scan 順と一致させる / NFR 2.1）。
const selectPolicyColumns = `id, tenant_id, name, amapi_policy_name, body, version, updated_by, created_at, updated_at`

// scanPolicyRow は policies の 1 行を列ごとに型付きで PolicyRow へ走査する（Get / List 共通 /
// NFR 2.1）。
//
// `body jsonb` は `map[string]any` へ、`updated_by`（nullable）は `*uuid.UUID` へ scan する
// （NULL は nil ポインタのまま / impl-notes task1 learning と整合）。Scan の error は wrap せず
// そのまま返し、呼び出し側（Get は NotFound / Unavailable 写像、List は Unavailable 写像）に委ねる。
func scanPolicyRow(row rowScanner) (PolicyRow, error) {
	var p PolicyRow
	if err := row.Scan(
		&p.ID,
		&p.TenantID,
		&p.Name,
		&p.AMAPIPolicyName,
		&p.Body,
		&p.Version,
		&p.UpdatedBy,
		&p.CreatedAt,
		&p.UpdatedAt,
	); err != nil {
		return PolicyRow{}, err
	}
	return p, nil
}

// nullableUpdatedBy は UpdatedBy ポインタを bind 値へ写像する。nil はそのまま NULL bind
// （pgx は nil *uuid.UUID を NULL として送る）、非 nil は値 bind になる。
func nullableUpdatedBy(updatedBy *uuid.UUID) *uuid.UUID {
	return updatedBy
}

// mapGetError は Get の lookup error を写像する。0 行（pgx.ErrNoRows）は存在差を露出しない
// `ErrPolicyNotFound`（CodeNotFound / 404）へ、それ以外は fail-closed で `CodeUnavailable`
// （503）へ wrap する（Req 4.2 / 4.4 / 4.5）。
func mapGetError(err error) error {
	if err == nil {
		return nil
	}
	if stderrors.Is(err, pgx.ErrNoRows) {
		// 存在差非露出: ErrPolicyNotFound の汎用 message を保ちつつ DB cause も保持する。
		return wrapSentinel(ErrPolicyNotFound, err)
	}
	return pkgerrors.Wrap(pkgerrors.CodeUnavailable, "policy lookup failed", err)
}

// wrapSentinel は package sentinel（*errors.Error）の Code / 汎用 message を保ちつつ、DB cause も
// 保持した新規 *errors.Error を返す。Cause を `sentinel + dbErr` の多重 wrap にすることで、
// 呼び出し側の `errors.Is(err, sentinel)`（Service の NotFound/Conflict 写像）と
// `errors.Is(err, dbErr)`（診断）の双方を成立させる（service_types.go の sentinel 利用契約）。
func wrapSentinel(sentinel *pkgerrors.Error, dbErr error) error {
	return pkgerrors.Wrap(sentinel.Code, sentinel.Message, fmt.Errorf("%w: %w", sentinel, dbErr))
}

// mapDeleteError は Delete の exec error を写像する。割当済み端末ありの FK 違反（pgerrcode
// 23503）は `ErrDeleteConflict`（CodeConflict / 409）へ、それ以外は `CodeUnavailable`（503）へ
// wrap する（design 確認事項 3 推奨案 / Req 5.3）。err == nil は nil を返す。
func mapDeleteError(err error) error {
	if err == nil {
		return nil
	}
	if isForeignKeyViolation(err) {
		return wrapSentinel(ErrDeleteConflict, err)
	}
	return pkgerrors.Wrap(pkgerrors.CodeUnavailable, "policy delete failed", err)
}

// mapAssignError は AssignPolicyToDevice の exec error を写像する。他テナント policy 指定の
// 複合 FK 違反（23503）は存在差を露出しない `ErrPolicyNotFound`（CodeNotFound / 404）へ、
// それ以外は `CodeUnavailable`（503）へ wrap する（Req 3.2 / 4.2 / 4.5）。err == nil は nil を返す
// （affected=0 の他テナント device 経路は err==nil で Service が NotFound 判定する / Req 3.3）。
func mapAssignError(err error) error {
	if err == nil {
		return nil
	}
	if isForeignKeyViolation(err) {
		return wrapSentinel(ErrPolicyNotFound, err)
	}
	return pkgerrors.Wrap(pkgerrors.CodeUnavailable, "policy assign failed", err)
}

// isForeignKeyViolation は error が PostgreSQL の外部キー違反（pgerrcode 23503）か判定する
// （auth/tenant の UniqueViolation 検出と同方式の *pgconn.PgError 解析）。
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return stderrors.As(err, &pgErr) && pgErr.Code == pgerrcode.ForeignKeyViolation
}

// Insert は Repository.Insert の実装。
func (r *repository) Insert(ctx context.Context, row PolicyRow) error {
	return db.BeginTxFunc(ctx, r.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO policies (id, tenant_id, name, amapi_policy_name, body, version, updated_by)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			row.ID, row.TenantID, row.Name, row.AMAPIPolicyName, row.Body, row.Version,
			nullableUpdatedBy(row.UpdatedBy),
		); err != nil {
			return pkgerrors.Wrap(pkgerrors.CodeUnavailable, "policy insert failed", err)
		}
		return nil
	})
}

// Update は Repository.Update の実装。
func (r *repository) Update(ctx context.Context, row PolicyRow) (int64, error) {
	var affected int64
	err := db.BeginTxFunc(ctx, r.pool, func(tx pgx.Tx) error {
		// WHERE id=$ AND tenant_id=$ で自テナント行のみ UPDATE する。affected=0 は Service が
		// 不在 / 他テナント（NotFound）として写像する材料（Req 4.4 / 4.5）。
		ct, err := tx.Exec(ctx,
			`UPDATE policies
			 SET name = $1, amapi_policy_name = $2, body = $3, version = $4,
			     updated_by = $5, updated_at = now()
			 WHERE id = $6 AND tenant_id = $7`,
			row.Name, row.AMAPIPolicyName, row.Body, row.Version,
			nullableUpdatedBy(row.UpdatedBy), row.ID, row.TenantID,
		)
		if err != nil {
			return pkgerrors.Wrap(pkgerrors.CodeUnavailable, "policy update failed", err)
		}
		affected = ct.RowsAffected()
		return nil
	})
	if err != nil {
		return 0, err
	}
	return affected, nil
}

// Get は Repository.Get の実装。
func (r *repository) Get(ctx context.Context, tenantID, policyID uuid.UUID) (PolicyRow, error) {
	var result PolicyRow
	err := db.BeginTxFunc(ctx, r.pool, func(tx pgx.Tx) error {
		// RLS により自テナント行のみ可視。tenant_id 条件は二重防御として明示する（Req 4.2 / 4.4）。
		row := tx.QueryRow(ctx,
			`SELECT `+selectPolicyColumns+`
			 FROM policies
			 WHERE id = $1 AND tenant_id = $2`,
			policyID, tenantID,
		)
		p, err := scanPolicyRow(row)
		if err != nil {
			// 0 行 → ErrPolicyNotFound（汎用 message）、その他 → Unavailable（Req 4.5）。
			return mapGetError(err)
		}
		result = p
		return nil
	})
	if err != nil {
		return PolicyRow{}, err
	}
	return result, nil
}

// List は Repository.List の実装。
func (r *repository) List(ctx context.Context, tenantID uuid.UUID) ([]PolicyRow, error) {
	// 0 件でも非 nil の空 slice を返す。
	rowsOut := make([]PolicyRow, 0)
	err := db.BeginTxFunc(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT `+selectPolicyColumns+`
			 FROM policies
			 WHERE tenant_id = $1
			 ORDER BY created_at ASC`,
			tenantID,
		)
		if err != nil {
			return pkgerrors.Wrap(pkgerrors.CodeUnavailable, "policy list query failed", err)
		}
		defer rows.Close()
		for rows.Next() {
			p, scanErr := scanPolicyRow(rows)
			if scanErr != nil {
				return pkgerrors.Wrap(pkgerrors.CodeUnavailable, "policy row scan failed", scanErr)
			}
			rowsOut = append(rowsOut, p)
		}
		if err := rows.Err(); err != nil {
			return pkgerrors.Wrap(pkgerrors.CodeUnavailable, "policy list iteration failed", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rowsOut, nil
}

// Delete は Repository.Delete の実装。
func (r *repository) Delete(ctx context.Context, tenantID, policyID uuid.UUID) (int64, error) {
	var affected int64
	err := db.BeginTxFunc(ctx, r.pool, func(tx pgx.Tx) error {
		// WHERE id=$ AND tenant_id=$ で自テナント行のみ DELETE する。割当済み端末ありの policy は
		// 複合 FK 違反（23503）で失敗し、ErrDeleteConflict（409）に写像する（design 確認事項 3 / Req 5.3）。
		ct, err := tx.Exec(ctx,
			`DELETE FROM policies WHERE id = $1 AND tenant_id = $2`,
			policyID, tenantID,
		)
		if err != nil {
			return mapDeleteError(err)
		}
		affected = ct.RowsAffected()
		return nil
	})
	if err != nil {
		return 0, err
	}
	return affected, nil
}

// AssignPolicyToDevice は Repository.AssignPolicyToDevice の実装。
func (r *repository) AssignPolicyToDevice(ctx context.Context, tenantID, deviceID, policyID uuid.UUID) (int64, error) {
	var affected int64
	err := db.BeginTxFunc(ctx, r.pool, func(tx pgx.Tx) error {
		// WHERE id=$deviceID AND tenant_id=$tenantID で自テナント device のみ UPDATE する。
		//   - 他テナント device → affected=0（err==nil）で Service が NotFound 判定（Req 3.3）。
		//   - 他テナント policy → 複合 FK 違反(23503) を ErrPolicyNotFound に写像（Req 3.2 / 4.2）。
		ct, err := tx.Exec(ctx,
			`UPDATE devices
			 SET applied_policy_id = $1
			 WHERE id = $2 AND tenant_id = $3`,
			policyID, deviceID, tenantID,
		)
		if err != nil {
			return mapAssignError(err)
		}
		affected = ct.RowsAffected()
		return nil
	})
	if err != nil {
		return 0, err
	}
	return affected, nil
}
