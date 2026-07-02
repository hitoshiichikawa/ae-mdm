package integration_test

import (
	"context"
	"encoding/json"
	stdErrors "errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hitoshiichikawa/ae-mdm/internal/device"
	internalerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	platformdb "github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
)

// 本ファイルは Issue #9 (C1) task 3 の device.Repository（raw pgx + RLS + フィルタ/ページング +
// SuperAdmin 集計 + COALESCE 部分更新）に対する integration test。DATABASE_URL 未設定環境では
// requireDBURLs が t.Skip するため DB 不在でも false-fail しない（helpers_test.go の規約）。
//
// 検証する AC（tasks.md task 3 の _Requirements:_）:
//   - 1.1 / 5.1 / 5.2 / 2.4 / NFR 3.1: tenant-scoped 一覧の他テナント非可視・詳細 0 行 → NotFound（存在秘匿）
//   - 1.2 / 1.3 / 1.4: compliance / mode / sync フィルタが該当行のみ返す（sync は syncCutoff 境界）
//   - 1.5: LIMIT/OFFSET ページング
//   - 1.7: 該当ゼロのフィルタで非 nil 空 slice
//   - 6.1 / 6.3 / 6.4: SuperAdmin 集計 / tenant_id 絞り込み / 端末ゼロで非 nil 空 slice
//   - 7.2: COALESCE 部分更新で欠落フィールドの既存値保持 + affected 返却

// setupDeviceRepo は migrate-up → truncate → seedDummyData → app pool 構築までを担う共通 setup。
// 返す ctxA は tenant A の TenantContext（非 SuperAdmin）を確立済みで RLS が tenant A に閉じる。
// seedDummyData が tenant A/B に device A/B（mode fully_managed / compliance unknown /
// last_status_at NULL）を投入済み。追加の richer な device fixture は seedDevice で個別投入する。
func setupDeviceRepo(t *testing.T) (repo device.Repository, pool *pgxpool.Pool, ids seededIDs, ctxA context.Context, cleanup func()) {
	t.Helper()
	urls := requireDBURLs(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	applyMigrationsUp(t, urls.migrate)
	truncateAll(t, ctx, urls)
	pool = newAppPool(t, ctx, urls)
	ids = seedDummyData(t, ctx, pool)
	repo = device.NewRepository(pool)
	ctxA = platformdb.WithTenantContext(ctx, platformdb.TenantContext{
		TenantID:     ids.tenantAID,
		IsSuperAdmin: false,
	})
	cleanup = func() {
		pool.Close()
		cancel()
	}
	return repo, pool, ids, ctxA, cleanup
}

// seedDeviceInput は seedDevice が投入する 1 device 行の指定。空文字の jsonb はスキーマ既定
// （hardware_info='{}' / non_compliance_details='[]'）に倒す。nil の時刻は NULL / now() を用いる。
type seedDeviceInput struct {
	id                   uuid.UUID
	tenantID             uuid.UUID
	amapiName            string
	mode                 string // 'fully_managed' | 'dedicated'
	compliance           string // enum ラベル
	lastStatusAt         *time.Time
	appliedPolicyName    *string
	hardwareInfo         string // jsonb literal（空は '{}'）
	nonComplianceDetails string // jsonb literal（空は '[]'）
	enrolledAt           *time.Time
}

// seedDevice は SuperAdmin GUC 文脈で 1 device 行を投入する（seedDummyData を改変せず richer な
// fixture を足すための local helper / task 3 テスト方針）。enum は `::device_mode` /
// `::compliance_status`、jsonb は `::jsonb` で型キャストして bind する（repository.go と同じ SQL 契約を
// 実 DB で確認する副次効果を持つ）。
func seedDevice(t *testing.T, ctx context.Context, pool *pgxpool.Pool, in seedDeviceInput) {
	t.Helper()
	hw := in.hardwareInfo
	if hw == "" {
		hw = "{}"
	}
	ncd := in.nonComplianceDetails
	if ncd == "" {
		ncd = "[]"
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("seedDevice BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.is_superadmin', 'true', true)"); err != nil {
		t.Fatalf("seedDevice set_config: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO devices
		   (id, tenant_id, amapi_device_name, mode, compliance_status, last_status_at,
		    applied_policy_name, hardware_info, non_compliance_details, enrolled_at)
		 VALUES
		   ($1, $2, $3, $4::device_mode, $5::compliance_status, $6,
		    $7, $8::jsonb, $9::jsonb, COALESCE($10, now()))`,
		in.id, in.tenantID, in.amapiName, in.mode, in.compliance, in.lastStatusAt,
		in.appliedPolicyName, hw, ncd, in.enrolledAt,
	); err != nil {
		t.Fatalf("seedDevice INSERT: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("seedDevice commit: %v", err)
	}
}

// containsDeviceID は rows に id が含まれるか判定する（フィルタ / 分離テストの補助）。
func containsDeviceID(rows []device.DeviceRow, id uuid.UUID) bool {
	for _, r := range rows {
		if r.ID == id {
			return true
		}
	}
	return false
}

// wideFilter は分離検証用に十分大きいページを返す ListFilter。
func wideFilter() device.ListFilter {
	return device.ListFilter{Page: 1, PageSize: 200}
}

// TestDeviceRepository_ListByTenant_TenantScoped はシナリオ (1.1 / NFR 3.1) 対応。
// tenant A ctx の ListByTenant が自テナント行のみ返し、他テナント（B）の device を一切露出しない
// ことを RLS で検証する。
func TestDeviceRepository_ListByTenant_TenantScoped(t *testing.T) {
	// Arrange
	repo, pool, ids, ctxA, cleanup := setupDeviceRepo(t)
	defer cleanup()

	// tenant A に 1 件追加（seed の device A と合わせて 2 件）。
	extraA := uuid.New()
	seedDevice(t, ctxA, pool, seedDeviceInput{
		id: extraA, tenantID: ids.tenantAID, amapiName: "enterprises/X/devices/A2",
		mode: string(device.DeviceModeFullyManaged), compliance: string(device.ComplianceStatusCompliant),
	})

	// Act
	rows, err := repo.ListByTenant(ctxA, ids.tenantAID, wideFilter(), time.Now())
	if err != nil {
		t.Fatalf("ListByTenant: %v", err)
	}

	// Assert: 自テナント行のみ（device A / A2 の 2 件）。他テナント device B は不可視（NFR 3.1）。
	if len(rows) != 2 {
		t.Fatalf("ListByTenant 件数 = %d; want 2（自テナント行のみ）", len(rows))
	}
	for _, r := range rows {
		if r.TenantID != ids.tenantAID {
			t.Errorf("他テナント行を返した: tenant_id=%v", r.TenantID)
		}
	}
	if containsDeviceID(rows, ids.deviceBID) {
		t.Errorf("他テナント device B が一覧に露出している: %v", ids.deviceBID)
	}
}

// TestDeviceRepository_GetByID_NotFoundAndCrossTenant はシナリオ (2.4 / 5.1 / 5.2 / NFR 3.1) 対応。
// 不在 id と実在する他テナント device id の双方が、存在差を露出しない同一の ErrDeviceNotFound
// （CodeNotFound / 汎用 message）に写像されることを検証する（存在秘匿）。
func TestDeviceRepository_GetByID_NotFoundAndCrossTenant(t *testing.T) {
	// Arrange
	repo, _, ids, ctxA, cleanup := setupDeviceRepo(t)
	defer cleanup()

	// Act (5.1): 不在 id。
	_, errMissing := repo.GetByID(ctxA, ids.tenantAID, uuid.New())
	// Act (5.2): 実在する他テナント device B（RLS で 0 行）。
	_, errCross := repo.GetByID(ctxA, ids.tenantAID, ids.deviceBID)

	// Assert: 双方 ErrDeviceNotFound（CodeNotFound）。
	for name, err := range map[string]error{"不在": errMissing, "越境": errCross} {
		if !stdErrors.Is(err, device.ErrDeviceNotFound) {
			t.Fatalf("%s の GetByID は ErrDeviceNotFound を期待; got %v", name, err)
		}
		var de *internalerrors.Error
		if !stdErrors.As(err, &de) || de.Code != internalerrors.CodeNotFound {
			t.Errorf("%s: Code = NotFound を期待; got %v", name, err)
		}
	}

	// Assert: 不在と越境で同一の汎用 message（存在差非露出 / 2.4 / 5.2）。
	var deMissing, deCross *internalerrors.Error
	stdErrors.As(errMissing, &deMissing)
	stdErrors.As(errCross, &deCross)
	if deMissing.Message != deCross.Message {
		t.Errorf("不在と越境で message が異なる（存在差露出）: %q vs %q", deMissing.Message, deCross.Message)
	}
}

// TestDeviceRepository_ListByTenant_ComplianceFilter はシナリオ (1.2) 対応。
// compliance フィルタが該当分類の行のみ返すことを検証する。
func TestDeviceRepository_ListByTenant_ComplianceFilter(t *testing.T) {
	// Arrange
	repo, pool, ids, ctxA, cleanup := setupDeviceRepo(t)
	defer cleanup()

	nonCompliant := uuid.New()
	compliant := uuid.New()
	seedDevice(t, ctxA, pool, seedDeviceInput{
		id: nonCompliant, tenantID: ids.tenantAID, amapiName: "enterprises/X/devices/NC",
		mode: string(device.DeviceModeFullyManaged), compliance: string(device.ComplianceStatusNonCompliant),
	})
	seedDevice(t, ctxA, pool, seedDeviceInput{
		id: compliant, tenantID: ids.tenantAID, amapiName: "enterprises/X/devices/C",
		mode: string(device.DeviceModeFullyManaged), compliance: string(device.ComplianceStatusCompliant),
	})

	// Act
	want := device.ComplianceStatusNonCompliant
	f := wideFilter()
	f.Compliance = &want
	rows, err := repo.ListByTenant(ctxA, ids.tenantAID, f, time.Now())
	if err != nil {
		t.Fatalf("ListByTenant: %v", err)
	}

	// Assert: non_compliant のみ（compliant / unknown の device A は除外）。
	if len(rows) != 1 || rows[0].ID != nonCompliant {
		t.Fatalf("compliance フィルタが該当行のみ返さない: got %d 件", len(rows))
	}
	if containsDeviceID(rows, compliant) || containsDeviceID(rows, ids.deviceAID) {
		t.Errorf("フィルタ対象外の行が露出している")
	}
}

// TestDeviceRepository_ListByTenant_ModeFilter はシナリオ (1.3) 対応。
// mode フィルタが該当モードの行のみ返すことを検証する。
func TestDeviceRepository_ListByTenant_ModeFilter(t *testing.T) {
	// Arrange
	repo, pool, ids, ctxA, cleanup := setupDeviceRepo(t)
	defer cleanup()

	dedicated := uuid.New()
	seedDevice(t, ctxA, pool, seedDeviceInput{
		id: dedicated, tenantID: ids.tenantAID, amapiName: "enterprises/X/devices/DED",
		mode: string(device.DeviceModeDedicated), compliance: string(device.ComplianceStatusUnknown),
	})

	// Act
	want := device.DeviceModeDedicated
	f := wideFilter()
	f.Mode = &want
	rows, err := repo.ListByTenant(ctxA, ids.tenantAID, f, time.Now())
	if err != nil {
		t.Fatalf("ListByTenant: %v", err)
	}

	// Assert: dedicated のみ（fully_managed の device A は除外）。
	if len(rows) != 1 || rows[0].ID != dedicated {
		t.Fatalf("mode フィルタが該当行のみ返さない: got %d 件", len(rows))
	}
	if containsDeviceID(rows, ids.deviceAID) {
		t.Errorf("fully_managed の device A が露出している")
	}
}

// TestDeviceRepository_ListByTenant_SyncDelayedFilter はシナリオ (1.4) 対応。
// syncCutoff 境界で同期遅延フィルタが正しく分類することを検証する（strict less-than /
// NULL は非遅延）。cutoff ちょうどの端末は非遅延側に入る（境界値）。
func TestDeviceRepository_ListByTenant_SyncDelayedFilter(t *testing.T) {
	// Arrange
	repo, pool, ids, ctxA, cleanup := setupDeviceRepo(t)
	defer cleanup()

	now := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	cutoff := now.Add(-24 * time.Hour)
	delayedAt := now.Add(-48 * time.Hour) // < cutoff → 遅延
	recentAt := now.Add(-1 * time.Hour)   // >= cutoff → 非遅延
	boundaryAt := cutoff                  // == cutoff → 非遅延（strict <）

	delayed := uuid.New()
	recent := uuid.New()
	boundary := uuid.New()
	seedDevice(t, ctxA, pool, seedDeviceInput{
		id: delayed, tenantID: ids.tenantAID, amapiName: "enterprises/X/devices/DELAYED",
		mode: string(device.DeviceModeFullyManaged), compliance: string(device.ComplianceStatusUnknown),
		lastStatusAt: &delayedAt,
	})
	seedDevice(t, ctxA, pool, seedDeviceInput{
		id: recent, tenantID: ids.tenantAID, amapiName: "enterprises/X/devices/RECENT",
		mode: string(device.DeviceModeFullyManaged), compliance: string(device.ComplianceStatusUnknown),
		lastStatusAt: &recentAt,
	})
	seedDevice(t, ctxA, pool, seedDeviceInput{
		id: boundary, tenantID: ids.tenantAID, amapiName: "enterprises/X/devices/BOUNDARY",
		mode: string(device.DeviceModeFullyManaged), compliance: string(device.ComplianceStatusUnknown),
		lastStatusAt: &boundaryAt,
	})
	// device A（seed）は last_status_at NULL → 非遅延。

	// Act (遅延のみ)
	truthy := true
	fDelayed := wideFilter()
	fDelayed.SyncDelayed = &truthy
	delayedRows, err := repo.ListByTenant(ctxA, ids.tenantAID, fDelayed, cutoff)
	if err != nil {
		t.Fatalf("ListByTenant(delayed): %v", err)
	}

	// Assert: 遅延のみ = delayed の 1 件。boundary（== cutoff）は非遅延なので含まれない。
	if len(delayedRows) != 1 || delayedRows[0].ID != delayed {
		t.Fatalf("SyncDelayed=true は delayed のみを期待; got %d 件", len(delayedRows))
	}
	if containsDeviceID(delayedRows, boundary) {
		t.Errorf("cutoff ちょうどの端末が遅延側に分類された（strict < 違反）")
	}

	// Act (非遅延のみ)
	falsy := false
	fNotDelayed := wideFilter()
	fNotDelayed.SyncDelayed = &falsy
	notDelayedRows, err := repo.ListByTenant(ctxA, ids.tenantAID, fNotDelayed, cutoff)
	if err != nil {
		t.Fatalf("ListByTenant(not delayed): %v", err)
	}

	// Assert: 非遅延 = recent / boundary / NULL(device A) を含み、delayed を含まない。
	if containsDeviceID(notDelayedRows, delayed) {
		t.Errorf("遅延端末が非遅延側に露出している")
	}
	if !containsDeviceID(notDelayedRows, recent) || !containsDeviceID(notDelayedRows, boundary) {
		t.Errorf("非遅延側に recent / boundary が含まれていない")
	}
	if !containsDeviceID(notDelayedRows, ids.deviceAID) {
		t.Errorf("last_status_at NULL の device A が非遅延側に含まれていない")
	}
}

// TestDeviceRepository_ListByTenant_Pagination はシナリオ (1.5) 対応。
// LIMIT/OFFSET が enrolled_at ASC の順で正しいページを返すことを検証する。compliance フィルタで
// seed の device A（unknown）を除外し、投入 4 件のみを対象にする。
func TestDeviceRepository_ListByTenant_Pagination(t *testing.T) {
	// Arrange: compliant な 4 件を distinct enrolled_at で投入（並び順を決定論化）。
	repo, pool, ids, ctxA, cleanup := setupDeviceRepo(t)
	defer cleanup()

	seeds := make([]uuid.UUID, 4)
	for i := 0; i < 4; i++ {
		id := uuid.New()
		seeds[i] = id
		enrolled := time.Date(2020+i, 1, 1, 0, 0, 0, 0, time.UTC)
		seedDevice(t, ctxA, pool, seedDeviceInput{
			id: id, tenantID: ids.tenantAID, amapiName: "enterprises/X/devices/PAGE" + id.String(),
			mode: string(device.DeviceModeFullyManaged), compliance: string(device.ComplianceStatusCompliant),
			enrolledAt: &enrolled,
		})
	}

	want := device.ComplianceStatusCompliant
	page1 := device.ListFilter{Compliance: &want, Page: 1, PageSize: 2}
	page2 := device.ListFilter{Compliance: &want, Page: 2, PageSize: 2}

	// Act
	rows1, err := repo.ListByTenant(ctxA, ids.tenantAID, page1, time.Now())
	if err != nil {
		t.Fatalf("ListByTenant page1: %v", err)
	}
	rows2, err := repo.ListByTenant(ctxA, ids.tenantAID, page2, time.Now())
	if err != nil {
		t.Fatalf("ListByTenant page2: %v", err)
	}

	// Assert: enrolled_at ASC で page1=[2020,2021] / page2=[2022,2023]。
	if len(rows1) != 2 || rows1[0].ID != seeds[0] || rows1[1].ID != seeds[1] {
		t.Fatalf("page1 が enrolled_at ASC の先頭 2 件でない: got %+v", idsOf(rows1))
	}
	if len(rows2) != 2 || rows2[0].ID != seeds[2] || rows2[1].ID != seeds[3] {
		t.Fatalf("page2 が enrolled_at ASC の次 2 件でない: got %+v", idsOf(rows2))
	}
}

// idsOf はデバッグ出力用に DeviceRow の id 列を抽出する。
func idsOf(rows []device.DeviceRow) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

// TestDeviceRepository_ListByTenant_EmptyNonNilSlice はシナリオ (1.7) 対応。
// 該当行ゼロのフィルタでも非 nil の空 slice を返すことを検証する（境界値）。
func TestDeviceRepository_ListByTenant_EmptyNonNilSlice(t *testing.T) {
	// Arrange: unsupported な device は投入しない（seed の device A は unknown）。
	repo, _, ids, ctxA, cleanup := setupDeviceRepo(t)
	defer cleanup()

	unsupported := device.ComplianceStatusUnsupported
	f := wideFilter()
	f.Compliance = &unsupported

	// Act
	rows, err := repo.ListByTenant(ctxA, ids.tenantAID, f, time.Now())
	if err != nil {
		t.Fatalf("ListByTenant: %v", err)
	}

	// Assert: 非 nil の空 slice（Req 1.7）。
	if rows == nil {
		t.Fatalf("該当ゼロで nil slice を返した; want 非 nil 空 slice")
	}
	if len(rows) != 0 {
		t.Errorf("該当ゼロなのに %d 件返った", len(rows))
	}
}

// TestDeviceRepository_AggregateOverview_AllAndTenantFilter はシナリオ (6.1 / 6.3) 対応。
// SuperAdmin ctx 下で tenant × compliance の件数を集計し、tenantFilter で当該テナントのみへ絞れる
// ことを検証する。
func TestDeviceRepository_AggregateOverview_AllAndTenantFilter(t *testing.T) {
	// Arrange: 既知の分布を投入（device A/B は seed で unknown ×1 ずつ）。
	repo, pool, ids, ctxA, cleanup := setupDeviceRepo(t)
	defer cleanup()

	seedDevice(t, ctxA, pool, seedDeviceInput{
		id: uuid.New(), tenantID: ids.tenantAID, amapiName: "enterprises/X/devices/A-C",
		mode: string(device.DeviceModeFullyManaged), compliance: string(device.ComplianceStatusCompliant),
	})
	seedDevice(t, ctxA, pool, seedDeviceInput{
		id: uuid.New(), tenantID: ids.tenantAID, amapiName: "enterprises/X/devices/A-NC",
		mode: string(device.DeviceModeFullyManaged), compliance: string(device.ComplianceStatusNonCompliant),
	})
	seedDevice(t, ctxA, pool, seedDeviceInput{
		id: uuid.New(), tenantID: ids.tenantBID, amapiName: "enterprises/X/devices/B-C",
		mode: string(device.DeviceModeFullyManaged), compliance: string(device.ComplianceStatusCompliant),
	})

	superCtx := platformdb.WithTenantContext(ctxA, platformdb.TenantContext{IsSuperAdmin: true})

	// Act (6.1): 全テナント集計。
	all, err := repo.AggregateOverview(superCtx, nil)
	if err != nil {
		t.Fatalf("AggregateOverview(nil): %v", err)
	}

	// Assert: 総件数 = 全 device 数（seed A/B の unknown 2 + 追加 3 = 5）。
	if total := sumCounts(all); total != 5 {
		t.Fatalf("AggregateOverview(nil) の総件数 = %d; want 5", total)
	}
	if got := countFor(all, ids.tenantAID, device.ComplianceStatusNonCompliant); got != 1 {
		t.Errorf("(tenant A, non_compliant) = %d; want 1", got)
	}
	if got := countFor(all, ids.tenantBID, device.ComplianceStatusCompliant); got != 1 {
		t.Errorf("(tenant B, compliant) = %d; want 1", got)
	}

	// Act (6.3): tenant A のみへ絞り込み。
	onlyA, err := repo.AggregateOverview(superCtx, &ids.tenantAID)
	if err != nil {
		t.Fatalf("AggregateOverview(&tenantA): %v", err)
	}

	// Assert: tenant A のみ（unknown 1 + compliant 1 + non_compliant 1 = 3 件）。
	if total := sumCounts(onlyA); total != 3 {
		t.Fatalf("AggregateOverview(&tenantA) の総件数 = %d; want 3", total)
	}
	for _, c := range onlyA {
		if c.TenantID != ids.tenantAID {
			t.Errorf("tenant_id 絞り込みが効いていない: %v", c.TenantID)
		}
	}
}

// TestDeviceRepository_AggregateOverview_Empty はシナリオ (6.4) 対応。
// 端末ゼロ（全 device 削除後）で AggregateOverview が非 nil の空 slice を返すことを検証する（境界値）。
func TestDeviceRepository_AggregateOverview_Empty(t *testing.T) {
	// Arrange: seed 済み device を全削除する（SuperAdmin GUC で RLS をバイパス）。
	repo, pool, _, ctxA, cleanup := setupDeviceRepo(t)
	defer cleanup()

	superCtx := platformdb.WithTenantContext(ctxA, platformdb.TenantContext{IsSuperAdmin: true})
	if err := platformdb.BeginTxFunc(superCtx, pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(superCtx, "DELETE FROM devices")
		return err
	}); err != nil {
		t.Fatalf("device 全削除: %v", err)
	}

	// Act
	rows, err := repo.AggregateOverview(superCtx, nil)
	if err != nil {
		t.Fatalf("AggregateOverview: %v", err)
	}

	// Assert: 非 nil の空 slice（Req 6.4）。
	if rows == nil {
		t.Fatalf("端末ゼロで nil slice を返した; want 非 nil 空 slice")
	}
	if len(rows) != 0 {
		t.Errorf("端末ゼロなのに %d 件返った", len(rows))
	}
}

// TestDeviceRepository_UpdateFromStatusReport_PartialPreserve はシナリオ (7.2) 対応。
// COALESCE 部分更新で「渡したフィールドのみ更新」「欠落（nil）フィールドは既存値保持」になること、
// affected を返すこと、未登録 amapi_device_name は affected=0 になることを検証する。
func TestDeviceRepository_UpdateFromStatusReport_PartialPreserve(t *testing.T) {
	// Arrange: 非既定の初期値を持つ device を投入する（保持を確認するため）。
	repo, pool, ids, ctxA, cleanup := setupDeviceRepo(t)
	defer cleanup()

	target := uuid.New()
	const amapiName = "enterprises/X/devices/UPD"
	origPolicy := "old-policy"
	origLast := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seedDevice(t, ctxA, pool, seedDeviceInput{
		id: target, tenantID: ids.tenantAID, amapiName: amapiName,
		mode: string(device.DeviceModeFullyManaged), compliance: string(device.ComplianceStatusNonCompliant),
		lastStatusAt: &origLast, appliedPolicyName: &origPolicy,
		hardwareInfo: `{"orig":true}`, nonComplianceDetails: `[{"r":"old"}]`,
	})

	// Act: hardware_info と last_status_at のみ更新（他フィールドは nil = 欠落 → 既存値保持）。
	newHW := json.RawMessage(`{"new":true}`)
	newLast := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	affected, err := repo.UpdateFromStatusReport(ctxA, device.StatusApplyInput{
		AMAPIDeviceName:      amapiName,
		LastStatusReportTime: &newLast,
		HardwareInfo:         &newHW,
	})
	if err != nil {
		t.Fatalf("UpdateFromStatusReport: %v", err)
	}
	if affected != 1 {
		t.Fatalf("affected = %d; want 1", affected)
	}

	// Assert: 更新後の行を検証する。
	got, err := repo.GetByID(ctxA, ids.tenantAID, target)
	if err != nil {
		t.Fatalf("GetByID after update: %v", err)
	}
	// 渡したフィールドは更新される。
	if !jsonEqual(t, got.HardwareInfo, `{"new":true}`) {
		t.Errorf("hardware_info が更新されていない: %s", got.HardwareInfo)
	}
	if got.LastStatusAt == nil || !got.LastStatusAt.Equal(newLast) {
		t.Errorf("last_status_at が更新されていない: %v; want %v", got.LastStatusAt, newLast)
	}
	// 欠落フィールドは既存値保持（Req 7.2）。
	if got.ComplianceStatus != device.ComplianceStatusNonCompliant {
		t.Errorf("compliance_status が保持されていない: %q; want non_compliant", got.ComplianceStatus)
	}
	if got.AppliedPolicyName == nil || *got.AppliedPolicyName != origPolicy {
		t.Errorf("applied_policy_name が保持されていない: %v; want %q", got.AppliedPolicyName, origPolicy)
	}
	if !jsonEqual(t, got.NonComplianceDetails, `[{"r":"old"}]`) {
		t.Errorf("non_compliance_details が保持されていない: %s", got.NonComplianceDetails)
	}

	// Act + Assert: enum を含む部分更新も反映される（compliance を compliant へ / 空 non_compliance）。
	compliant := device.ComplianceStatusCompliant
	emptyNCD := json.RawMessage(`[]`)
	if _, err := repo.UpdateFromStatusReport(ctxA, device.StatusApplyInput{
		AMAPIDeviceName:      amapiName,
		ComplianceStatus:     &compliant,
		NonComplianceDetails: &emptyNCD,
	}); err != nil {
		t.Fatalf("UpdateFromStatusReport(compliance): %v", err)
	}
	got2, err := repo.GetByID(ctxA, ids.tenantAID, target)
	if err != nil {
		t.Fatalf("GetByID after compliance update: %v", err)
	}
	if got2.ComplianceStatus != device.ComplianceStatusCompliant {
		t.Errorf("compliance_status が更新されていない: %q; want compliant", got2.ComplianceStatus)
	}
	// hardware_info は前回の更新値のまま保持される（この更新では欠落）。
	if !jsonEqual(t, got2.HardwareInfo, `{"new":true}`) {
		t.Errorf("hardware_info が意図せず変化した: %s", got2.HardwareInfo)
	}

	// Act + Assert: 未登録 amapi_device_name は affected=0（未登録端末 / error に倒さない）。
	zero, err := repo.UpdateFromStatusReport(ctxA, device.StatusApplyInput{
		AMAPIDeviceName:      "enterprises/X/devices/UNKNOWN",
		LastStatusReportTime: &newLast,
	})
	if err != nil {
		t.Fatalf("未登録端末の Update は error に倒さない: got %v", err)
	}
	if zero != 0 {
		t.Errorf("未登録端末の affected = %d; want 0", zero)
	}
}

// sumCounts は集計行の Count 総和を返す。
func sumCounts(rows []device.TenantComplianceCount) int {
	total := 0
	for _, r := range rows {
		total += r.Count
	}
	return total
}

// countFor は (tenantID, compliance) グループの件数を返す（無ければ 0）。
func countFor(rows []device.TenantComplianceCount, tenantID uuid.UUID, cs device.ComplianceStatus) int {
	for _, r := range rows {
		if r.TenantID == tenantID && r.ComplianceStatus == cs {
			return r.Count
		}
	}
	return 0
}

// jsonEqual は jsonb 由来の RawMessage が期待 JSON と意味的に等しいか判定する（Postgres の jsonb
// 再整形（空白 / キー順）に依存せず比較する）。
func jsonEqual(t *testing.T, got json.RawMessage, want string) bool {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got の unmarshal 失敗: %v (%s)", err, got)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("want の unmarshal 失敗: %v", err)
	}
	gb, _ := json.Marshal(g)
	wb, _ := json.Marshal(w)
	return string(gb) == string(wb)
}
