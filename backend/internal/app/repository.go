package app

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
)

// Repository は `tenant_apps`（migration 0008）の List / Upsert / ApprovedPackages を raw pgx +
// RLS で集約する（design.md「App Repository」節 / tasks.md 1）。
//
// すべてのメソッドは tenant-scoped な `TenantContext`（Handler が継承する自テナント文脈）の
// まま `db.BeginTxFunc(ctx, pool, ...)` で tx を開き、RLS（`tenant_isolation_tenant_apps` /
// migration 0011）による自テナント限定に分離を委ねる。policy.Repository と同様、App 操作は
// SuperAdmin へ **昇格しない**（他テナント行は SELECT で 0 行、INSERT/UPDATE は WITH CHECK で
// 物理拒否される / design「テナント分離」節 / Req 2.3 / 4.x）。DB 失敗はいずれも
// `errors.Wrap(CodeUnavailable, ...)`（HTTP 503）へ写像する。
type Repository interface {
	// List は自テナントの承認済みアプリを approved_at 昇順で返す（Req 2.1 / 2.3）。
	//
	// 0 件のときは非 nil の空 slice を返す（Req 2.2）。RLS + tenant_id 述語により自テナント行
	// のみが可視で、他テナント行は返さない（Req 2.3）。
	List(ctx context.Context, tenantID uuid.UUID) ([]TenantAppRow, error)

	// Upsert は複数 SyncApp を単一 tx でアトミックに tenant_apps へ反映し反映件数を返す
	// （Req 3.1 / 3.2）。
	//
	// `INSERT ... ON CONFLICT (tenant_id, package_name) DO UPDATE` により、同一 package の
	// 重複同期はレコードを増やさず title / icon_url / approved_at を更新する（Req 3.2）。
	// 空リスト（len==0）は tx を開かず `(0, nil)` を早期 return する（Req 3.4）。いずれか 1 件でも
	// 失敗すれば単一 tx が rollback され、0 件反映のまま error を返す（部分反映を残さない / Req 3.6）。
	Upsert(ctx context.Context, tenantID uuid.UUID, apps []SyncApp) (count int, err error)

	// ApprovedPackages は packageNames のうち自テナントで承認済みの package 集合を返す（Req 5.1）。
	//
	// 空 packageNames は tx を開かず非 nil の空 map を早期 return する。RLS + tenant_id 述語で
	// 自テナント境界に閉じる。
	ApprovedPackages(ctx context.Context, tenantID uuid.UUID, packageNames []string) (map[string]struct{}, error)
}

// repository は Repository の pgxpool ベース実装。
type repository struct {
	pool *pgxpool.Pool
}

// NewRepository は Repository を構築する。pool は cmd/api bootstrap が構築する共有
// pgxpool.Pool を渡す（policy.NewRepository / tenant.NewRepository と同方式）。
func NewRepository(pool *pgxpool.Pool) Repository {
	return &repository{pool: pool}
}

// rowScanner は scanTenantAppRow が依存する最小 interface（pgx.Rows / pgx.Row が満たす）。
// 実 PostgreSQL に依存せず scanTenantAppRow を単体テストするための extension point
// （policy.Repository / audit.Repository の rowScanner と同型）。
type rowScanner interface {
	Scan(dest ...any) error
}

// rowsScanner は collectTenantAppRows / collectPackageSet が依存する最小 interface。
// pgx.Rows が満たし、fake rows を注入して行イテレーション（0 行 / N 行 / Err）を実 DB 非依存に
// 単体テストするための extension point（rowScanner の複数行版）。
type rowsScanner interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close()
}

// compile-time check: pgx.Rows は rowsScanner を満たす（配線が seam 契約と乖離したら build 失敗）。
var _ rowsScanner = (pgx.Rows)(nil)

// selectTenantAppColumns は SELECT 句の列順（scanTenantAppRow の Scan 順と一致させる / NFR 2.1）。
const selectTenantAppColumns = `id, tenant_id, package_name, title, icon_url, approved_at`

// listTenantAppsSQL は自テナントの承認済みアプリを approved_at 昇順で取得する（Req 2.1 / 2.3）。
// `tenant_id = $1` 述語は RLS との二重防御であり、SuperAdmin 昇格 / RLS バイパスは行わない。
const listTenantAppsSQL = `SELECT ` + selectTenantAppColumns + `
FROM tenant_apps
WHERE tenant_id = $1
ORDER BY approved_at ASC`

// upsertTenantAppSQL は 1 件の SyncApp を tenant_apps へ upsert する（Req 3.1 / 3.2）。
// ON CONFLICT (tenant_id, package_name) DO UPDATE により重複 package はレコードを増やさず更新する。
// approved_at は insert / update とも now()。id は呼び出し側が uuid.New() で採番する
// （0008 の id 列に DEFAULT が無いため）。
const upsertTenantAppSQL = `INSERT INTO tenant_apps (id, tenant_id, package_name, title, icon_url, approved_at)
VALUES ($1, $2, $3, $4, $5, now())
ON CONFLICT (tenant_id, package_name)
DO UPDATE SET title = EXCLUDED.title, icon_url = EXCLUDED.icon_url, approved_at = now()`

// approvedPackagesSQL は packageNames のうち自テナントで承認済みの package_name を取得する（Req 5.1）。
// `tenant_id = $1` 述語は RLS との二重防御であり、SuperAdmin 昇格 / RLS バイパスは行わない。
const approvedPackagesSQL = `SELECT package_name
FROM tenant_apps
WHERE tenant_id = $1 AND package_name = ANY($2)`

// scanTenantAppRow は tenant_apps の 1 行を列ごとに型付きで TenantAppRow へ走査する
// （List 用 / NFR 2.1）。
//
// `icon_url`（nullable）は `*string` へ scan する（NULL は nil ポインタのまま）。Scan の error は
// wrap せずそのまま返し、呼び出し側（collectTenantAppRows が CodeUnavailable 写像）に委ねる。
func scanTenantAppRow(row rowScanner) (TenantAppRow, error) {
	var a TenantAppRow
	if err := row.Scan(
		&a.ID,
		&a.TenantID,
		&a.PackageName,
		&a.Title,
		&a.IconURL,
		&a.ApprovedAt,
	); err != nil {
		return TenantAppRow{}, err
	}
	return a, nil
}

// collectTenantAppRows は rows を走査して TenantAppRow の slice へ集約する（List の本体委譲先）。
//
// 0 行でも非 nil の空 slice を返す（Req 2.2）。行順は SQL の ORDER BY を保持する。scan / Err の
// 失敗は `CodeUnavailable`（503）へ wrap する。rows は本関数が Close する。
func collectTenantAppRows(rows rowsScanner) ([]TenantAppRow, error) {
	defer rows.Close()
	out := make([]TenantAppRow, 0)
	for rows.Next() {
		a, err := scanTenantAppRow(rows)
		if err != nil {
			return nil, pkgerrors.Wrap(pkgerrors.CodeUnavailable, "tenant_apps row scan failed", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, pkgerrors.Wrap(pkgerrors.CodeUnavailable, "tenant_apps list iteration failed", err)
	}
	return out, nil
}

// collectPackageSet は rows を走査して package_name の集合（map[string]struct{}）へ集約する
// （ApprovedPackages の本体委譲先 / Req 5.1）。
//
// 0 行でも非 nil の空 map を返す。scan / Err の失敗は `CodeUnavailable`（503）へ wrap する。
// rows は本関数が Close する。
func collectPackageSet(rows rowsScanner) (map[string]struct{}, error) {
	defer rows.Close()
	out := make(map[string]struct{})
	for rows.Next() {
		var pkg string
		if err := rows.Scan(&pkg); err != nil {
			return nil, pkgerrors.Wrap(pkgerrors.CodeUnavailable, "tenant_apps approved scan failed", err)
		}
		out[pkg] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, pkgerrors.Wrap(pkgerrors.CodeUnavailable, "tenant_apps approved iteration failed", err)
	}
	return out, nil
}

// List は Repository.List の実装。
func (r *repository) List(ctx context.Context, tenantID uuid.UUID) ([]TenantAppRow, error) {
	// 0 件でも非 nil の空 slice を返す（Req 2.2）。
	out := make([]TenantAppRow, 0)
	err := db.BeginTxFunc(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, listTenantAppsSQL, tenantID)
		if err != nil {
			return pkgerrors.Wrap(pkgerrors.CodeUnavailable, "tenant_apps list query failed", err)
		}
		collected, cerr := collectTenantAppRows(rows)
		if cerr != nil {
			return cerr
		}
		out = collected
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Upsert は Repository.Upsert の実装。
//
// 空リストは tx を開かず `(0, nil)` を早期 return する（Req 3.4）。複数 SyncApp を単一 tx 内で
// 順に upsert し RowsAffected を積算する。いずれか 1 件でも失敗すれば BeginTxFunc が rollback し、
// 反映件数 0 のまま error を返す（部分反映を残さない / Req 3.6）。
func (r *repository) Upsert(ctx context.Context, tenantID uuid.UUID, apps []SyncApp) (int, error) {
	if len(apps) == 0 {
		// 空リストは pool へ触れず 0 件で早期 return（Req 3.4）。
		return 0, nil
	}
	var count int
	err := db.BeginTxFunc(ctx, r.pool, func(tx pgx.Tx) error {
		for _, a := range apps {
			// 0008 の id 列に DEFAULT が無いため新規行の id を uuid.New() で採番する。
			// ON CONFLICT 時は EXCLUDED 側の id は使われず既存行が更新される。
			ct, err := tx.Exec(ctx, upsertTenantAppSQL,
				uuid.New(), tenantID, a.PackageName, a.Title, a.IconURL,
			)
			if err != nil {
				return pkgerrors.Wrap(pkgerrors.CodeUnavailable, "tenant_apps upsert failed", err)
			}
			count += int(ct.RowsAffected())
		}
		return nil
	})
	if err != nil {
		// 単一 tx rollback により 0 件反映のまま伝達する（Req 3.6）。
		return 0, err
	}
	return count, nil
}

// ApprovedPackages は Repository.ApprovedPackages の実装。
//
// 空 packageNames は tx を開かず非 nil の空 map を早期 return する（不要な DB 往復を避ける）。
func (r *repository) ApprovedPackages(ctx context.Context, tenantID uuid.UUID, packageNames []string) (map[string]struct{}, error) {
	if len(packageNames) == 0 {
		// 空入力は pool へ触れず非 nil 空 map を早期 return。
		return make(map[string]struct{}), nil
	}
	out := make(map[string]struct{})
	err := db.BeginTxFunc(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, approvedPackagesSQL, tenantID, packageNames)
		if err != nil {
			return pkgerrors.Wrap(pkgerrors.CodeUnavailable, "tenant_apps approved query failed", err)
		}
		collected, cerr := collectPackageSet(rows)
		if cerr != nil {
			return cerr
		}
		out = collected
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
