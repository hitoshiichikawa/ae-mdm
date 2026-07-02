package enrollment

import (
	"context"
	stderrors "errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
)

// deviceExecer は upsertDeviceSQL を実行する最小 seam。pgx.Tx が自然に満たす
// （Exec(ctx, sql, args...) (pgconn.CommandTag, error)）。実 PostgreSQL に依存せず bind 値と
// SQL 文を単体テストで検証するための extension point（repository.rowScanner / db.txBeginner と同型）。
type deviceExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// upsertDeviceSQL は ENROLLMENT 通知で確定した端末を発行元テナントの devices へ冪等 upsert する
// INSERT ... ON CONFLICT 文（design.md「enrollment.Registrar」節 223 行 / Req 3.4）。
//
// ON CONFLICT (tenant_id, amapi_device_name) は devices の UNIQUE (tenant_id, amapi_device_name)
// 制約（migration 0006）に対応し、同一端末についての複数回通知を重複登録せず既存行の更新へ
// 冪等に写像する。enrolled_at / id は DO UPDATE で触れず初回値を保持し、hardware_info /
// software_info は DB default（'{}'）に委ねて本 Issue では更新しない（design 224 / STATUS_REPORT は別 Issue）。
const upsertDeviceSQL = `INSERT INTO devices (id, tenant_id, amapi_device_name, mode, compliance_status)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (tenant_id, amapi_device_name)
DO UPDATE SET mode = EXCLUDED.mode, compliance_status = EXCLUDED.compliance_status`

// Registrar は ENROLLMENT 通知で確定した端末を発行元テナントの devices テーブルへ冪等 upsert する
// （design.md「enrollment.Registrar」節 / Req 3.1 / 3.4 / NFR 2.1）。
//
// UpsertEnrolledDevice は notification.EnrollmentRegistrar ポート（primitive 型のみ）を structural
// typing で満たすため、enrollment は notification を import しない（doc.go の依存方向規約）。
type Registrar struct {
	pool *pgxpool.Pool

	// withTx は tenant-scoped tx 内で deviceExecer を用いた upsert を実行する seam。本番では
	// beginTxDeviceExec（db.BeginTxFunc 経由で RLS 分離 / pgx.Tx が deviceExecer を満たす）を束縛する。
	// 単体テストは fake execer を注入して実 PostgreSQL 無しに bind 値 / SQL / ガードを検証する。
	withTx func(ctx context.Context, fn func(deviceExecer) error) error
}

// NewRegistrar は Registrar を構築する。pool は cmd/api bootstrap が構築する共有 pgxpool.Pool を
// 渡す（NewTokenRepository と同方式。task 6 結合テストは enrollment.NewRegistrar(pool) を呼ぶ）。
func NewRegistrar(pool *pgxpool.Pool) *Registrar {
	r := &Registrar{pool: pool}
	r.withTx = r.beginTxDeviceExec
	return r
}

// beginTxDeviceExec は本番 seam。db.BeginTxFunc で tenant-scoped tx を開き（RLS が自テナント限定に
// 分離 / SuperAdmin 昇格しない）、pgx.Tx を deviceExecer として fn に渡す（repository.Insert と同型）。
func (r *Registrar) beginTxDeviceExec(ctx context.Context, fn func(deviceExecer) error) error {
	return db.BeginTxFunc(ctx, r.pool, func(tx pgx.Tx) error {
		return fn(tx)
	})
}

// UpsertEnrolledDevice は端末を発行元テナントの devices へ冪等 upsert する（Req 3.1 / 3.4）。
//
// notification.EnrollmentRegistrar ポート（primitive 型のみ）を structural typing で満たす。手順:
//  1. db.FromContext で TenantContext を取得する。ctx 未確立（FromContext がエラー）は越境更新を
//     構造的に防ぐため DB へ触れずにガードし、FromContext のエラー（CodeTenantCtxMissing・
//     非 transient = プログラミング前提違反）をそのまま伝播する（NFR 2.1）。
//  2. db.BeginTxFunc で tenant-scoped tx を開き、tenant_id を ctx 由来で明示 bind して upsert する。
//  3. DB 失敗は CodeUnavailable + IsTransient=true へ正規化し、handler → Dispatcher の nack
//     （再処理保持 / Req 3.6）へ写像する。Message には payload 生値・機密値を補間しない（NFR 3.1）。
func (r *Registrar) UpsertEnrolledDevice(ctx context.Context, amapiDeviceName, mode, complianceStatus string) error {
	tc, err := db.FromContext(ctx)
	if err != nil {
		// ctx 未確立は安全側ガード。DB へ触れず（越境更新不可）、非 transient のまま伝播する。
		return err
	}
	return asTransientUpsertErr(r.withTx(ctx, func(exec deviceExecer) error {
		args := buildUpsertArgs(tc.TenantID, amapiDeviceName, mode, complianceStatus)
		if _, err := exec.Exec(ctx, upsertDeviceSQL, args...); err != nil {
			return &pkgerrors.Error{
				Code:        pkgerrors.CodeUnavailable,
				Message:     "enrolled device upsert failed",
				Cause:       err,
				IsTransient: true,
			}
		}
		return nil
	}))
}

// buildUpsertArgs は upsertDeviceSQL の位置引数を組み立てる純粋 helper。
//
// id は uuid.New()（初回 INSERT のみ有効。ON CONFLICT 時は既存 id を保持）。tenant_id は ctx の
// TenantContext.TenantID を明示 bind し、RLS と併せて越境更新を物理的に不能にする（NFR 2.1）。
// mode / compliance_status は引数の enum ラベル文字列（fully_managed/dedicated・unknown/unsupported 等）
// をそのまま bind する。
func buildUpsertArgs(tenantID uuid.UUID, amapiDeviceName, mode, complianceStatus string) []any {
	return []any{uuid.New(), tenantID, amapiDeviceName, mode, complianceStatus}
}

// asTransientUpsertErr は withTx（db.BeginTxFunc 経由）が返した err を transient な *errors.Error へ
// 正規化する（Req 3.6 = DB 失敗は再処理保持 = nack へ写像 / tenant.asTransientReverseLookupErr を手本）。
//
// fn 内部で既に transient *Error へ写像済みなら二重 wrap せずそのまま返し、BeginTx /
// SetLocalTenant / Commit 由来の非 transient error のみ transient に包む。Message には機密値を
// 補間しない固定文言を用いる（NFR 3.1）。
func asTransientUpsertErr(cause error) error {
	if cause == nil {
		return nil
	}
	var de *pkgerrors.Error
	if stderrors.As(cause, &de) && de.IsTransient {
		return cause
	}
	return &pkgerrors.Error{
		Code:        pkgerrors.CodeUnavailable,
		Message:     "enrolled device upsert failed",
		IsTransient: true,
		Cause:       cause,
	}
}
