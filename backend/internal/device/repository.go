package device

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
)

// Repository は `devices`（migration 0006 + 0019）への tenant-scoped 参照・SuperAdmin 集計・
// STATUS_REPORT 部分更新を raw pgx + RLS で集約する（design.md「device.Repository」節 / tasks.md 3）。
//
// tenant-scoped メソッド（ListByTenant / GetByID / UpdateFromStatusReport）は ambient な
// `TenantContext`（Handler は Middleware 由来、StatusApplier は Dispatcher 由来）のまま
// `db.BeginTxFunc` で tx を開き、RLS（`tenant_isolation_devices` / migration 0011）による自テナント
// 限定に分離を委ねる。tenant.Repository / auth.Repository が SuperAdmin context へ昇格するのとは
// 対照的に、device の tenant-scoped 操作は **昇格しない**（他テナント行は SELECT / UPDATE ともに
// 0 行 / policy.Repository と同方針）。二重防御として `WHERE tenant_id=$` も明示する。
//
// `AggregateOverview` のみ cross-tenant 集計のため、呼び出し側（AdminHandler）が確立した
// SuperAdmin `TenantContext`（IsSuperAdmin=true）の ctx を信頼する（audit.AdminHandler 手本）。
type Repository interface {
	// ListByTenant は自テナント端末をフィルタ + ページングで返す（Req 1.1〜1.5）。
	//
	// syncCutoff は同期遅延フィルタ用の閾値時刻（now-threshold, Service が算出）。
	// f.SyncDelayed != nil のとき、true は `last_status_at < syncCutoff`（遅延のみ）、false は
	// `last_status_at IS NULL OR last_status_at >= syncCutoff`（非遅延のみ / NULL は非遅延）を適用する。
	// ページングは `LIMIT f.PageSize OFFSET (f.Page-1)*f.PageSize` + 決定論的 ORDER BY。
	// 0 件でも非 nil の空 slice を返す（Req 1.7）。
	ListByTenant(ctx context.Context, tenantID uuid.UUID, f ListFilter, syncCutoff time.Time) ([]DeviceRow, error)

	// GetByID は自テナント端末 1 行を返す（Req 2.4 / 5.1 / 5.2）。
	//
	// 0 行（不在 / RLS により他テナント行も 0 行）は存在差を露出しない `ErrDeviceNotFound`
	// （CodeNotFound / 汎用 message）に写像する。
	GetByID(ctx context.Context, tenantID, deviceID uuid.UUID) (DeviceRow, error)

	// AggregateOverview は SuperAdmin ctx 下でテナント別 × compliance_status の件数を集計する
	// （Req 6.1 / 6.3 / 6.4）。tenantFilter != nil のとき当該テナントのみへ絞り込む。
	// 0 件のときは非 nil の空 slice を返す（Req 6.4）。
	AggregateOverview(ctx context.Context, tenantFilter *uuid.UUID) ([]TenantComplianceCount, error)

	// UpdateFromStatusReport は amapi_device_name 一致行を部分更新し affected 行数を返す（Req 7.2）。
	//
	// 各カラムは `COALESCE($n, col)` で「payload 欠落（nil）→ 既存値保持」を実現する。
	// affected=0 は未登録端末（error には倒さない / StatusApplier が no-op ack する材料）。
	UpdateFromStatusReport(ctx context.Context, u StatusApplyInput) (int64, error)
}

// repository は Repository の pgxpool ベース実装。
type repository struct {
	pool *pgxpool.Pool
}

// NewRepository は Repository を構築する。pool は cmd/api bootstrap が構築する共有
// pgxpool.Pool を渡す（policy.NewRepository / audit.NewRepository と同方式）。
func NewRepository(pool *pgxpool.Pool) Repository {
	return &repository{pool: pool}
}

// rowScanner は scanDeviceRow が依存する最小 interface（pgx.Rows / pgx.Row が満たす）。
// 実 PostgreSQL に依存せず scanDeviceRow を単体テストするための extension point
// （policy.rowScanner / audit.rowScanner と同型）。
type rowScanner interface {
	Scan(dest ...any) error
}

// selectDeviceColumns は SELECT 句の列順（scanDeviceRow の Scan 順と一致させる / NFR 2.1）。
// migration 0006 + 0019 の列に対応する。
const selectDeviceColumns = `id, tenant_id, amapi_device_name, mode, applied_policy_id, applied_policy_name, ` +
	`hardware_info, software_info, compliance_status, non_compliance_details, installed_apps, last_status_at, enrolled_at`

// scanDeviceRow は devices の 1 行を列ごとに型付きで DeviceRow へ走査する（ListByTenant / GetByID 共通）。
//
// enum 列（mode / compliance_status）は text format の生ラベルを一旦 string へ scan してから
// DeviceMode / ComplianceStatus へ写像し、jsonb 列（hardware_info / software_info /
// non_compliance_details / installed_apps）は生 bytes を一旦 []byte へ scan してから
// json.RawMessage へ写像する（notification.scanUnassigned の jsonb→[]byte 手本に倣い、pgx v5 の
// 名前付き型 scan の不確実性を避ける）。nullable 列（applied_policy_id / applied_policy_name /
// last_status_at）は pointer へ直接 scan し、NULL は nil ポインタのまま保つ。Scan の error は
// wrap せずそのまま返し、呼び出し側（GetByID は NotFound / Unavailable 写像、ListByTenant は
// Unavailable 写像）に委ねる。
func scanDeviceRow(row rowScanner) (DeviceRow, error) {
	var (
		d             DeviceRow
		mode          string
		compliance    string
		hardware      []byte
		software      []byte
		nonCompliance []byte
		installedApps []byte
	)
	if err := row.Scan(
		&d.ID,
		&d.TenantID,
		&d.AMAPIDeviceName,
		&mode,
		&d.AppliedPolicyID,
		&d.AppliedPolicyName,
		&hardware,
		&software,
		&compliance,
		&nonCompliance,
		&installedApps,
		&d.LastStatusAt,
		&d.EnrolledAt,
	); err != nil {
		return DeviceRow{}, err
	}
	d.Mode = DeviceMode(mode)
	d.ComplianceStatus = ComplianceStatus(compliance)
	d.HardwareInfo = json.RawMessage(hardware)
	d.SoftwareInfo = json.RawMessage(software)
	d.NonComplianceDetails = json.RawMessage(nonCompliance)
	d.InstalledApps = json.RawMessage(installedApps)
	return d, nil
}

// mapGetError は GetByID の lookup error を写像する。0 行（pgx.ErrNoRows）は存在差を露出しない
// `ErrDeviceNotFound`（CodeNotFound / 404）へ、それ以外は fail-closed で `CodeUnavailable`（503）へ
// wrap する（Req 2.4 / 5.1 / 5.2）。err == nil は nil を返す。
func mapGetError(err error) error {
	if err == nil {
		return nil
	}
	if stderrors.Is(err, pgx.ErrNoRows) {
		// 存在差非露出: ErrDeviceNotFound の汎用 message を保ちつつ DB cause も保持する。
		return wrapSentinel(ErrDeviceNotFound, err)
	}
	return pkgerrors.Wrap(pkgerrors.CodeUnavailable, "device lookup failed", err)
}

// wrapSentinel は package sentinel（*errors.Error）の Code / 汎用 message を保ちつつ、DB cause も
// 保持した新規 *errors.Error を返す。Cause を `sentinel + dbErr` の多重 wrap にすることで、
// 呼び出し側の `errors.Is(err, sentinel)`（Service の NotFound 写像）と `errors.Is(err, dbErr)`
// （診断）の双方を成立させる（policy.wrapSentinel と同型）。
func wrapSentinel(sentinel *pkgerrors.Error, dbErr error) error {
	return pkgerrors.Wrap(sentinel.Code, sentinel.Message, fmt.Errorf("%w: %w", sentinel, dbErr))
}

// jsonbArg は *json.RawMessage を jsonb bind 値へ写像する。nil は typed-nil []byte（pgx が NULL bind →
// `COALESCE(NULL::jsonb, col)` で既存値保持）、非 nil は生 bytes を bind する（UpdateFromStatusReport
// の COALESCE 部分更新用 / Req 7.2）。
func jsonbArg(p *json.RawMessage) []byte {
	if p == nil {
		return nil
	}
	return []byte(*p)
}

// complianceArg は *ComplianceStatus を enum bind 用の text 値へ写像する。nil は NULL bind
// （`COALESCE(NULL::compliance_status, col)` で既存値保持）、非 nil はラベル文字列を bind する
// （UpdateFromStatusReport の COALESCE 部分更新用 / Req 7.2）。
func complianceArg(p *ComplianceStatus) *string {
	if p == nil {
		return nil
	}
	s := string(*p)
	return &s
}

// ListByTenant は Repository.ListByTenant の実装。
//
// 動的 WHERE をプレースホルダ番号を動的採番して安全に組み立てる。tenant_id 条件を必ず先頭に置き
// （RLS の二重防御 / NFR 3.1）、Compliance / Mode / SyncDelayed フィルタを条件付きで追加する。
// ページング安定化のため決定論的 ORDER BY（enrolled_at ASC, id ASC）を付ける。0 件でも非 nil の
// 空 slice を返す（Req 1.7）。
func (r *repository) ListByTenant(ctx context.Context, tenantID uuid.UUID, f ListFilter, syncCutoff time.Time) ([]DeviceRow, error) {
	// 0 件でも非 nil の空 slice を返す（Req 1.7）。
	out := make([]DeviceRow, 0)
	err := db.BeginTxFunc(ctx, r.pool, func(tx pgx.Tx) error {
		conditions := []string{"tenant_id = $1"}
		args := []any{tenantID}

		if f.Compliance != nil {
			args = append(args, string(*f.Compliance))
			conditions = append(conditions, fmt.Sprintf("compliance_status = $%d::compliance_status", len(args)))
		}
		if f.Mode != nil {
			args = append(args, string(*f.Mode))
			conditions = append(conditions, fmt.Sprintf("mode = $%d::device_mode", len(args)))
		}
		if f.SyncDelayed != nil {
			args = append(args, syncCutoff)
			if *f.SyncDelayed {
				// 遅延のみ: strict less-than（閾値ちょうどは非遅延 / Service.isSyncDelayed と一致）。
				conditions = append(conditions, fmt.Sprintf("last_status_at < $%d", len(args)))
			} else {
				// 非遅延のみ: NULL（未受信）は遅延扱いしないため含める（Req 1.4）。
				conditions = append(conditions, fmt.Sprintf("(last_status_at IS NULL OR last_status_at >= $%d)", len(args)))
			}
		}

		// ページング（既定値補完は Handler 責務 / repository は受領値をそのまま使う / Req 1.5）。
		args = append(args, f.PageSize)
		limitPh := len(args)
		args = append(args, (f.Page-1)*f.PageSize)
		offsetPh := len(args)

		sql := `SELECT ` + selectDeviceColumns + ` FROM devices WHERE ` + strings.Join(conditions, " AND ") +
			` ORDER BY enrolled_at ASC, id ASC` +
			fmt.Sprintf(" LIMIT $%d OFFSET $%d", limitPh, offsetPh)

		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return pkgerrors.Wrap(pkgerrors.CodeUnavailable, "device list query failed", err)
		}
		defer rows.Close()
		for rows.Next() {
			d, scanErr := scanDeviceRow(rows)
			if scanErr != nil {
				return pkgerrors.Wrap(pkgerrors.CodeUnavailable, "device row scan failed", scanErr)
			}
			out = append(out, d)
		}
		if err := rows.Err(); err != nil {
			return pkgerrors.Wrap(pkgerrors.CodeUnavailable, "device list iteration failed", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetByID は Repository.GetByID の実装。
func (r *repository) GetByID(ctx context.Context, tenantID, deviceID uuid.UUID) (DeviceRow, error) {
	var result DeviceRow
	err := db.BeginTxFunc(ctx, r.pool, func(tx pgx.Tx) error {
		// RLS により自テナント行のみ可視。tenant_id 条件は二重防御として明示する（Req 5.1 / 5.2 / NFR 3.1）。
		row := tx.QueryRow(ctx,
			`SELECT `+selectDeviceColumns+`
			 FROM devices
			 WHERE id = $1 AND tenant_id = $2`,
			deviceID, tenantID,
		)
		d, err := scanDeviceRow(row)
		if err != nil {
			// 0 行 → ErrDeviceNotFound（汎用 message）、その他 → Unavailable（Req 2.4 / 5.1 / 5.2）。
			return mapGetError(err)
		}
		result = d
		return nil
	})
	if err != nil {
		return DeviceRow{}, err
	}
	return result, nil
}

// AggregateOverview は Repository.AggregateOverview の実装。
//
// SuperAdmin ctx（呼び出し側が確立）を信頼し、`GROUP BY tenant_id, compliance_status` で flat な
// 件数行を集計する。tenantFilter != nil のとき `WHERE tenant_id=$1` で当該テナントのみへ絞る
// （Req 6.3）。ORDER BY を付けて結果順序を決定論的にする。0 件は非 nil の空 slice（Req 6.4）。
func (r *repository) AggregateOverview(ctx context.Context, tenantFilter *uuid.UUID) ([]TenantComplianceCount, error) {
	// 0 件でも非 nil の空 slice を返す（Req 6.4）。
	out := make([]TenantComplianceCount, 0)
	err := db.BeginTxFunc(ctx, r.pool, func(tx pgx.Tx) error {
		sql := `SELECT tenant_id, compliance_status, COUNT(*) FROM devices`
		args := []any{}
		if tenantFilter != nil {
			args = append(args, *tenantFilter)
			sql += ` WHERE tenant_id = $1`
		}
		sql += ` GROUP BY tenant_id, compliance_status ORDER BY tenant_id, compliance_status`

		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return pkgerrors.Wrap(pkgerrors.CodeUnavailable, "device overview query failed", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				c      TenantComplianceCount
				status string
			)
			if err := rows.Scan(&c.TenantID, &status, &c.Count); err != nil {
				return pkgerrors.Wrap(pkgerrors.CodeUnavailable, "device overview scan failed", err)
			}
			c.ComplianceStatus = ComplianceStatus(status)
			out = append(out, c)
		}
		if err := rows.Err(); err != nil {
			return pkgerrors.Wrap(pkgerrors.CodeUnavailable, "device overview iteration failed", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateFromStatusReport は Repository.UpdateFromStatusReport の実装。
//
// ambient tenant ctx のまま（昇格しない）、`WHERE amapi_device_name=$1` で一致行を部分更新する。
// (tenant_id, amapi_device_name) は UNIQUE かつ RLS が自テナントに閉じるため、一致は高々 1 行。
// 各カラムは `COALESCE($n, col)` で「payload 欠落（nil bind → NULL）→ 既存値保持」を実現する
// （Req 7.2）。enum は `::compliance_status`、jsonb は `::jsonb` で型キャストする。affected=0 は
// 未登録端末 / 他テナント越境（RLS で 0 行）を示し、error には倒さない。
func (r *repository) UpdateFromStatusReport(ctx context.Context, u StatusApplyInput) (int64, error) {
	var affected int64
	err := db.BeginTxFunc(ctx, r.pool, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx,
			`UPDATE devices
			 SET applied_policy_name    = COALESCE($2, applied_policy_name),
			     compliance_status      = COALESCE($3::compliance_status, compliance_status),
			     non_compliance_details = COALESCE($4::jsonb, non_compliance_details),
			     hardware_info          = COALESCE($5::jsonb, hardware_info),
			     software_info          = COALESCE($6::jsonb, software_info),
			     installed_apps         = COALESCE($7::jsonb, installed_apps),
			     last_status_at         = COALESCE($8, last_status_at)
			 WHERE amapi_device_name = $1`,
			u.AMAPIDeviceName,
			u.AppliedPolicyName,
			complianceArg(u.ComplianceStatus),
			jsonbArg(u.NonComplianceDetails),
			jsonbArg(u.HardwareInfo),
			jsonbArg(u.SoftwareInfo),
			jsonbArg(u.InstalledApps),
			u.LastStatusReportTime,
		)
		if err != nil {
			return pkgerrors.Wrap(pkgerrors.CodeUnavailable, "device status update failed", err)
		}
		affected = ct.RowsAffected()
		return nil
	})
	if err != nil {
		return 0, err
	}
	return affected, nil
}
