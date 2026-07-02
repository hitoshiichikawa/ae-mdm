package integration_test

import (
	"context"
	stdErrors "errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	internalerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	platformdb "github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
	"github.com/hitoshiichikawa/ae-mdm/internal/tenant"
)

// 本ファイルは Issue #38 (A4b) task 3.1 の tenant.Repository（raw SQL + SuperAdmin ctx +
// 競合制御）に対する integration test。DATABASE_URL 未設定環境では requireDBURLs が t.Skip
// するため、DB 不在でも false-fail しない（helpers_test.go の規約）。
//
// 検証シナリオ（tasks.md task 3.1 L35）:
//   (a) SuperAdmin ctx での CRUD（Insert→Get→List / NFR 3.1 / Req 1.1 / 4.1 / 4.2）
//   (b) 不在 id の Get で CodeNotFound（Req 4.3）
//   (c) 0 件 List で空 slice（Req 4.4）
//   (d) UpdateBound 成功で bound + enterprise_name 保存、二重 UpdateBound で 2 回目 affected=0（Req 2.1 / 2.5）
//   (e) 別 tenant への同一 enterprise_name で 23505 → CodeConflict（Req 2.1）
//   (f) UpdateDisabled 成功で disabled + 監査列、二重 UpdateDisabled で 2 回目 affected=0（Req 3.1 / 3.4）
//   (g) 非 SuperAdmin ctx で SuperAdmin が作った tenant が見えない（テナント分離 / Req 6.5 / 1.4 系）

// setupTenantRepo は migrate-up → truncate → app pool 構築までを担う共通 setup。
// 返す ctx は SuperAdmin TenantContext を確立済みで、Repository 経由の CRUD に使える。
func setupTenantRepo(t *testing.T) (repo tenant.Repository, superAdminCtx context.Context, cleanup func()) {
	t.Helper()
	urls := requireDBURLs(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	applyMigrationsUp(t, urls.migrate)
	truncateAll(t, ctx, urls)
	pool := newAppPool(t, ctx, urls)
	repo = tenant.NewRepository(pool)
	// Repository は内部で SuperAdmin context を確立するため、外側 ctx は素の ctx で良いが、
	// テスト helper の一貫性のため SuperAdmin TenantContext を載せた ctx も返す
	// （非 SuperAdmin 分離テストでは別 ctx を明示的に構築する）。
	superAdminCtx = platformdb.WithTenantContext(ctx, platformdb.TenantContext{
		TenantID:     uuid.Nil,
		IsSuperAdmin: true,
	})
	cleanup = func() {
		pool.Close()
		cancel()
	}
	return repo, superAdminCtx, cleanup
}

// TestTenantRepository_CRUD_SuperAdminContext はシナリオ (a)(c) 対応。
// SuperAdmin ctx で Insert→Get→List の一連が成立し、List が pending_bind 行を返すこと、
// 空状態の List が空 slice であることを確認する（NFR 3.1 / Req 1.1 / 4.1 / 4.2 / 4.4）。
func TestTenantRepository_CRUD_SuperAdminContext(t *testing.T) {
	// Arrange
	repo, ctx, cleanup := setupTenantRepo(t)
	defer cleanup()

	// Act + Assert: 空状態の List は空 slice（非 nil / Req 4.4）
	empty, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List(empty): %v", err)
	}
	if empty == nil {
		t.Fatalf("List(empty) は非 nil の空 slice を期待; got nil")
	}
	if len(empty) != 0 {
		t.Fatalf("List(empty) len = %d; want 0", len(empty))
	}

	// Act: Insert → Get
	id := uuid.New()
	if err := repo.Insert(ctx, tenant.TenantRow{ID: id, Name: "Acme Corp"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, err := repo.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// Assert: 作成直後は pending_bind / enterprise_name 空 / 監査列は nil（Req 1.1 / NFR 1.1）
	if got.ID != id {
		t.Errorf("Get().ID = %v; want %v", got.ID, id)
	}
	if got.Name != "Acme Corp" {
		t.Errorf("Get().Name = %q; want %q", got.Name, "Acme Corp")
	}
	if got.Status != tenant.StatusPendingBind {
		t.Errorf("Get().Status = %q; want %q", got.Status, tenant.StatusPendingBind)
	}
	if got.EnterpriseName != "" {
		t.Errorf("Get().EnterpriseName = %q; want \"\"（pending_bind は未設定）", got.EnterpriseName)
	}
	if got.DisabledAt != nil {
		t.Errorf("Get().DisabledAt = %v; want nil", got.DisabledAt)
	}
	if got.DisabledBy != nil {
		t.Errorf("Get().DisabledBy = %v; want nil", got.DisabledBy)
	}

	// Act + Assert: List が 1 件返す（Req 4.1）
	list, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List len = %d; want 1", len(list))
	}
	if list[0].ID != id {
		t.Errorf("List[0].ID = %v; want %v", list[0].ID, id)
	}
}

// TestTenantRepository_Get_NotFound はシナリオ (b) 対応。
// 不在 id の Get が `*errors.Error{Code: CodeNotFound}` を返すこと（Req 4.3）。
func TestTenantRepository_Get_NotFound(t *testing.T) {
	// Arrange
	repo, ctx, cleanup := setupTenantRepo(t)
	defer cleanup()

	// Act
	_, err := repo.Get(ctx, uuid.New())

	// Assert
	if err == nil {
		t.Fatalf("不在 id の Get はエラーを期待; got nil")
	}
	var domainErr *internalerrors.Error
	if !stdErrors.As(err, &domainErr) {
		t.Fatalf("err は *errors.Error を期待; got %T (%v)", err, err)
	}
	if domainErr.Code != internalerrors.CodeNotFound {
		t.Errorf("Code = %q; want %q", domainErr.Code, internalerrors.CodeNotFound)
	}
}

// TestTenantRepository_UpdateBound_AffectedAndDoubleBind はシナリオ (d) 対応。
// pending_bind 行への初回 UpdateBound は affected=1 で bound + enterprise_name 保存、
// 同一 id への 2 回目 UpdateBound は status が pending_bind でないため affected=0
// （二重バインド競合の材料 / Req 2.1 / 2.2 / 2.3 / 2.5）。
func TestTenantRepository_UpdateBound_AffectedAndDoubleBind(t *testing.T) {
	// Arrange
	repo, ctx, cleanup := setupTenantRepo(t)
	defer cleanup()
	id := uuid.New()
	if err := repo.Insert(ctx, tenant.TenantRow{ID: id, Name: "Bind Target"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// Act + Assert: 初回 bind は affected=1
	affected1, err := repo.UpdateBound(ctx, id, "enterprises/LC0001")
	if err != nil {
		t.Fatalf("UpdateBound(1): %v", err)
	}
	if affected1 != 1 {
		t.Fatalf("UpdateBound(1) affected = %d; want 1", affected1)
	}

	got, err := repo.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get after bind: %v", err)
	}
	if got.Status != tenant.StatusBound {
		t.Errorf("Get().Status = %q; want %q", got.Status, tenant.StatusBound)
	}
	if got.EnterpriseName != "enterprises/LC0001" {
		t.Errorf("Get().EnterpriseName = %q; want %q", got.EnterpriseName, "enterprises/LC0001")
	}

	// Act + Assert: 同一 id への 2 回目 bind は status!=pending_bind なので affected=0
	affected2, err := repo.UpdateBound(ctx, id, "enterprises/LC0002")
	if err != nil {
		t.Fatalf("UpdateBound(2): %v", err)
	}
	if affected2 != 0 {
		t.Errorf("UpdateBound(2) affected = %d; want 0（既に bound）", affected2)
	}
}

// TestTenantRepository_UpdateBound_DuplicateEnterpriseName_Conflict はシナリオ (e) 対応。
// 別 tenant に既存と同一の enterprise_name を bind すると部分一意 index 違反（23505）が
// `*errors.Error{Code: CodeConflict}` に写像されること（Req 2.1）。
func TestTenantRepository_UpdateBound_DuplicateEnterpriseName_Conflict(t *testing.T) {
	// Arrange: 2 つの pending_bind tenant を用意し、1 つ目を bind する
	repo, ctx, cleanup := setupTenantRepo(t)
	defer cleanup()
	idA := uuid.New()
	idB := uuid.New()
	if err := repo.Insert(ctx, tenant.TenantRow{ID: idA, Name: "Tenant A"}); err != nil {
		t.Fatalf("Insert A: %v", err)
	}
	if err := repo.Insert(ctx, tenant.TenantRow{ID: idB, Name: "Tenant B"}); err != nil {
		t.Fatalf("Insert B: %v", err)
	}
	const sharedEnterprise = "enterprises/SHARED01"
	if _, err := repo.UpdateBound(ctx, idA, sharedEnterprise); err != nil {
		t.Fatalf("UpdateBound A: %v", err)
	}

	// Act: tenant B に同一 enterprise_name を bind
	_, err := repo.UpdateBound(ctx, idB, sharedEnterprise)

	// Assert: 部分一意 index 違反が CodeConflict に写像される
	if err == nil {
		t.Fatalf("別 tenant への同一 enterprise_name bind はエラーを期待; got nil")
	}
	var domainErr *internalerrors.Error
	if !stdErrors.As(err, &domainErr) {
		t.Fatalf("err は *errors.Error を期待; got %T (%v)", err, err)
	}
	if domainErr.Code != internalerrors.CodeConflict {
		t.Errorf("Code = %q; want %q", domainErr.Code, internalerrors.CodeConflict)
	}
}

// TestTenantRepository_UpdateDisabled_AffectedAndDoubleDisable はシナリオ (f) 対応。
// 初回 UpdateDisabled は affected=1 で disabled + 監査列（disabled_at / disabled_by）記録、
// 2 回目は status='disabled' なので affected=0（二重無効化競合の材料 / Req 3.1 / 3.4）。
func TestTenantRepository_UpdateDisabled_AffectedAndDoubleDisable(t *testing.T) {
	// Arrange
	repo, ctx, cleanup := setupTenantRepo(t)
	defer cleanup()
	id := uuid.New()
	actor := uuid.New()
	if err := repo.Insert(ctx, tenant.TenantRow{ID: id, Name: "Disable Target"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// Act + Assert: 初回 disable は affected=1
	affected1, err := repo.UpdateDisabled(ctx, id, actor)
	if err != nil {
		t.Fatalf("UpdateDisabled(1): %v", err)
	}
	if affected1 != 1 {
		t.Fatalf("UpdateDisabled(1) affected = %d; want 1", affected1)
	}

	got, err := repo.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get after disable: %v", err)
	}
	if got.Status != tenant.StatusDisabled {
		t.Errorf("Get().Status = %q; want %q", got.Status, tenant.StatusDisabled)
	}
	if got.DisabledAt == nil {
		t.Errorf("Get().DisabledAt = nil; want 非 nil（監査列記録 / Req 3.1）")
	}
	if got.DisabledBy == nil {
		t.Errorf("Get().DisabledBy = nil; want %v", actor)
	} else if *got.DisabledBy != actor {
		t.Errorf("Get().DisabledBy = %v; want %v", *got.DisabledBy, actor)
	}

	// Act + Assert: 2 回目 disable は status='disabled' なので affected=0
	affected2, err := repo.UpdateDisabled(ctx, id, actor)
	if err != nil {
		t.Fatalf("UpdateDisabled(2): %v", err)
	}
	if affected2 != 0 {
		t.Errorf("UpdateDisabled(2) affected = %d; want 0（既に disabled）", affected2)
	}
}

// TestTenantRepository_NonSuperAdminContext_TenantIsolation はシナリオ (g) 対応。
// SuperAdmin ctx で作成した tenant が、非 SuperAdmin ctx（別 tenant 文脈）からは
// RLS（tenant_isolation_tenants / 0011）により 0 件で見えないことを確認する
// （テナント分離 / Req 6.5 / 1.4 系 / NFR 3.1）。
//
// Repository は内部で SuperAdmin context を確立するため、非 SuperAdmin の可視性は
// pool 直叩きで検証する（Repository 越しでは常に SuperAdmin context が確立される設計）。
func TestTenantRepository_NonSuperAdminContext_TenantIsolation(t *testing.T) {
	// Arrange: SuperAdmin Repository で tenant を作成
	urls := requireDBURLs(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	applyMigrationsUp(t, urls.migrate)
	truncateAll(t, ctx, urls)
	pool := newAppPool(t, ctx, urls)
	defer pool.Close()

	repo := tenant.NewRepository(pool)
	saCtx := platformdb.WithTenantContext(ctx, platformdb.TenantContext{
		TenantID:     uuid.Nil,
		IsSuperAdmin: true,
	})
	createdID := uuid.New()
	if err := repo.Insert(saCtx, tenant.TenantRow{ID: createdID, Name: "Isolated Tenant"}); err != nil {
		t.Fatalf("Insert(SuperAdmin): %v", err)
	}

	// Act + Assert: 別 tenant 文脈（非 SuperAdmin）では当該 tenant 行が 0 件
	otherTenantCtx := platformdb.WithTenantContext(ctx, platformdb.TenantContext{
		TenantID:     uuid.New(), // SuperAdmin が作った tenant とは別 id
		IsSuperAdmin: false,
	})
	var visible int
	err := platformdb.BeginTxFunc(otherTenantCtx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM tenants WHERE id = $1`, createdID,
		).Scan(&visible)
	})
	if err != nil {
		t.Fatalf("BeginTxFunc(non-SuperAdmin): %v", err)
	}
	if visible != 0 {
		t.Errorf("非 SuperAdmin 文脈での tenant 可視件数 = %d; want 0（RLS で分離）", visible)
	}

	// Sanity: SuperAdmin 文脈では当該 tenant が見える
	got, err := repo.Get(saCtx, createdID)
	if err != nil {
		t.Fatalf("Get(SuperAdmin): %v", err)
	}
	if got.ID != createdID {
		t.Errorf("SuperAdmin Get().ID = %v; want %v", got.ID, createdID)
	}
}

// TestTenantRepository_TenantIDByEnterpriseName は Issue #39 (A6b) task 1.1 の逆引き
// （enterprise_name → tenant_id）に対する integration test。bound 解決 / 未 bound・不在は
// found=false の経路を実 DB で検証する（Req 3.1 / 3.2）。DATABASE_URL 未設定環境では
// requireDBURLs が t.Skip するため DB 不在でも false-fail しない（helpers_test.go の規約）。
//
// 検証シナリオ:
//
//	(a) bound テナントの enterprise_name で逆引きすると当該 tenant_id + found=true（Req 3.1）
//	(b) pending_bind テナント（未 bound）の検索では found=false（Req 3.2）
//	(c) 存在しない enterprise_name の検索では found=false（Req 3.2）
func TestTenantRepository_TenantIDByEnterpriseName(t *testing.T) {
	// Arrange: bound テナント 1 件と pending_bind テナント 1 件を用意する。
	repo, ctx, cleanup := setupTenantRepo(t)
	defer cleanup()

	const boundEnterprise = "enterprises/LC0042"
	boundID := uuid.New()
	pendingID := uuid.New()
	if err := repo.Insert(ctx, tenant.TenantRow{ID: boundID, Name: "Bound Tenant"}); err != nil {
		t.Fatalf("Insert(bound): %v", err)
	}
	if err := repo.Insert(ctx, tenant.TenantRow{ID: pendingID, Name: "Pending Tenant"}); err != nil {
		t.Fatalf("Insert(pending): %v", err)
	}
	if _, err := repo.UpdateBound(ctx, boundID, boundEnterprise); err != nil {
		t.Fatalf("UpdateBound(bound): %v", err)
	}

	// Act + Assert (a): bound テナントの enterprise_name で逆引きできる（Req 3.1）。
	gotID, found, err := repo.TenantIDByEnterpriseName(ctx, boundEnterprise)
	if err != nil {
		t.Fatalf("TenantIDByEnterpriseName(bound): %v", err)
	}
	if !found {
		t.Fatalf("TenantIDByEnterpriseName(bound) found = false; want true（bound 解決）")
	}
	if gotID != boundID {
		t.Errorf("TenantIDByEnterpriseName(bound) id = %v; want %v", gotID, boundID)
	}

	// Act + Assert (b): pending_bind テナント（未 bound）は enterprise_name が NULL で
	// status!='bound' のため解決できず found=false（Req 3.2）。
	// pending_bind 行の enterprise_name は NULL なので、空文字 / 任意文字列のいずれでも 0 件になる。
	_, foundPending, err := repo.TenantIDByEnterpriseName(ctx, "enterprises/PENDING-NONE")
	if err != nil {
		t.Fatalf("TenantIDByEnterpriseName(pending): %v", err)
	}
	if foundPending {
		t.Errorf("TenantIDByEnterpriseName(未 bound) found = true; want false（未割当 / Req 3.2）")
	}

	// Act + Assert (c): 存在しない enterprise_name は found=false（Req 3.2）。
	_, foundAbsent, err := repo.TenantIDByEnterpriseName(ctx, "enterprises/DOES-NOT-EXIST")
	if err != nil {
		t.Fatalf("TenantIDByEnterpriseName(absent): %v", err)
	}
	if foundAbsent {
		t.Errorf("TenantIDByEnterpriseName(不在) found = true; want false（未割当 / Req 3.2）")
	}
}
