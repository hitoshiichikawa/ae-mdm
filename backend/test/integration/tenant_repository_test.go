package integration_test

import (
	"context"
	stdErrors "errors"
	"testing"
	"time"

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

// 以下は Issue #52（#38 follow-up: Tenant Bind 競合制御強化）task 7.1 で追加した
// 予約 / 解放 / 回収 / 束縛 / binding↔disable の競合テスト。先行 task 3.1（Req 1.1/1.4/1.5）・
// task 3.2（Req 2.1）が `_Requirements_partial:_` 明示で task 7.1 へ deferred した「実 PostgreSQL を
// 要する affected rows / sweep 挙動」の regression を解消する。DATABASE_URL 未設定環境では
// requireDBURLs が t.Skip するため DB 不在でも false-fail しない（helpers_test.go の規約）。
//
// 検証シナリオ（tasks.md task 7.1 L84-90）:
//   (1) ReserveBinding 並行: 同一 id 2 回で affected 1/0（Req 1.1 / NFR 2.1、orphan 防止の DB 層証跡）
//   (2) ReleaseBinding: binding→pending_bind affected=1、pending_bind 行は affected=0（Req 1.5）
//   (3) RecoverStaleBindings: 古い binding（updated_at 過去）のみ回収、新しい binding 据え置き（Req 2.1）
//   (4) UpdateBound WHERE status='binding': pending_bind 行への UpdateBound affected=0（Req 1.4 の WHERE 変更回帰）
//       / binding 行への UpdateBound affected=1 で bound 確定（対比・正常系）
//   (5) binding↔disable: binding 行に UpdateDisabled affected=1、その後 UpdateBound affected=0（Req 1.6）
//   (6) signup_url_name 永続化往復: Insert→Get で signup_url_name 一致 / 空文字は NULL→空文字写像（Req 3.1）
//
// enum 4 値 + signup_url_name 列の可逆性（Req 4.1）は既存 `migrations_reversible_test`（0017 込みの
// 全 down→up）が担保するため本ファイルには重複追加しない。本ファイルの各テストが 0017 適用済み
// スキーマ上で `binding` enum 値と `signup_url_name` 列を実際に往復させることで Req 4.1 の実挙動
// 証跡を兼ねる（tasks.md L90 の整理）。

// TestTenantRepository_ReserveBinding_ConcurrentWinnerAndLoser はシナリオ (1) 対応。
// 同一 pending_bind テナントへの ReserveBinding を 2 回発行し、1 回目のみ勝者（affected=1、
// pending_bind→binding）、2 回目は敗者（affected=0、既に binding）となることを確認する。
// これは並行 bind で勝者 1 要求のみが CreateEnterprise に進み orphan Enterprise を作らない
// ことの DB 層証跡（Req 1.1 / NFR 2.1）。
func TestTenantRepository_ReserveBinding_ConcurrentWinnerAndLoser(t *testing.T) {
	// Arrange
	repo, ctx, cleanup := setupTenantRepo(t)
	defer cleanup()
	id := uuid.New()
	if err := repo.Insert(ctx, tenant.TenantRow{ID: id, Name: "Reserve Target"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// Act + Assert: 1 回目の予約は勝者（pending_bind→binding）で affected=1
	affected1, err := repo.ReserveBinding(ctx, id)
	if err != nil {
		t.Fatalf("ReserveBinding(1): %v", err)
	}
	if affected1 != 1 {
		t.Fatalf("ReserveBinding(1) affected = %d; want 1（勝者）", affected1)
	}

	// Act + Assert: 2 回目の予約は敗者（既に binding）で affected=0
	affected2, err := repo.ReserveBinding(ctx, id)
	if err != nil {
		t.Fatalf("ReserveBinding(2): %v", err)
	}
	if affected2 != 0 {
		t.Errorf("ReserveBinding(2) affected = %d; want 0（敗者・既に binding）", affected2)
	}

	// Assert: 状態は binding（勝者が確定した予約状態 / orphan 防止の DB 層証跡）
	got, err := repo.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != tenant.StatusBinding {
		t.Errorf("Get().Status = %q; want %q", got.Status, tenant.StatusBinding)
	}
}

// TestTenantRepository_ReleaseBinding_AffectedAndIdempotent はシナリオ (2) 対応。
// binding 行への初回 ReleaseBinding は affected=1 で pending_bind へ戻り、2 回目（既に
// pending_bind の行）は WHERE status='binding' に掛からず affected=0 となる。binding でない
// 行は解放対象外であることを併せて確認する（Req 1.5 / CreateEnterprise 失敗時の再 bind 可能化）。
func TestTenantRepository_ReleaseBinding_AffectedAndIdempotent(t *testing.T) {
	// Arrange: binding 行を作る（Insert→ReserveBinding）
	repo, ctx, cleanup := setupTenantRepo(t)
	defer cleanup()
	id := uuid.New()
	if err := repo.Insert(ctx, tenant.TenantRow{ID: id, Name: "Release Target"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if affected, err := repo.ReserveBinding(ctx, id); err != nil || affected != 1 {
		t.Fatalf("ReserveBinding(setup): affected=%d err=%v; want affected=1 err=nil", affected, err)
	}

	// Act + Assert: binding→pending_bind 解放は affected=1
	affected1, err := repo.ReleaseBinding(ctx, id)
	if err != nil {
		t.Fatalf("ReleaseBinding(1): %v", err)
	}
	if affected1 != 1 {
		t.Fatalf("ReleaseBinding(1) affected = %d; want 1", affected1)
	}
	got, err := repo.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get after release: %v", err)
	}
	if got.Status != tenant.StatusPendingBind {
		t.Errorf("Get().Status = %q; want %q（解放後）", got.Status, tenant.StatusPendingBind)
	}

	// Act + Assert: 既に pending_bind の行への再解放は affected=0（binding でない行は対象外）
	affected2, err := repo.ReleaseBinding(ctx, id)
	if err != nil {
		t.Fatalf("ReleaseBinding(2): %v", err)
	}
	if affected2 != 0 {
		t.Errorf("ReleaseBinding(2) affected = %d; want 0（既に pending_bind）", affected2)
	}
}

// TestTenantRepository_RecoverStaleBindings_RecoversOnlyStale はシナリオ (3) 対応。
// 2 つの binding 行のうち updated_at を過去へ寄せた古い方だけがしきい値に掛かって回収され
// （pending_bind へ戻る）、新しい方は binding のまま据え置かれることを確認する（Req 2.1）。
//
// `tenants.updated_at` には自動更新トリガーが無く、Repository には updated_at を過去へ設定する
// メソッドが無いため、エイジングは pool 直叩き（SuperAdmin ctx で RLS 通過）で行う。setupTenantRepo
// は repo しか返さないので、isolation test と同様に個別 setup を組む（helper シグネチャは変更しない）。
func TestTenantRepository_RecoverStaleBindings_RecoversOnlyStale(t *testing.T) {
	// Arrange: pool 直叩きが必要なため isolation test と同じ手順で個別 setup する。
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

	// 2 つの binding 行を用意（各 Insert→ReserveBinding）。
	staleID := uuid.New()
	freshID := uuid.New()
	for _, id := range []uuid.UUID{staleID, freshID} {
		if err := repo.Insert(saCtx, tenant.TenantRow{ID: id, Name: "Recover Target"}); err != nil {
			t.Fatalf("Insert(%v): %v", id, err)
		}
		if affected, err := repo.ReserveBinding(saCtx, id); err != nil || affected != 1 {
			t.Fatalf("ReserveBinding(%v): affected=%d err=%v; want affected=1 err=nil", id, affected, err)
		}
	}

	// stale 側の updated_at を 1 時間前へ寄せる（pool 直叩き / SuperAdmin ctx で RLS 通過）。
	// しきい値評価は DB 側 now() 基準（RecoverStaleBindings 実装）なので DB 内で相対的に古くする。
	if err := platformdb.BeginTxFunc(saCtx, pool, func(tx pgx.Tx) error {
		_, execErr := tx.Exec(ctx,
			`UPDATE tenants SET updated_at = now() - make_interval(secs => $1) WHERE id = $2`,
			(1 * time.Hour).Seconds(), staleID)
		return execErr
	}); err != nil {
		t.Fatalf("updated_at のエイジング UPDATE: %v", err)
	}

	// Act: しきい値 30 分で回収（stale=1h 前は該当、fresh=直近は非該当）。
	recovered, err := repo.RecoverStaleBindings(saCtx, 30*time.Minute)
	if err != nil {
		t.Fatalf("RecoverStaleBindings: %v", err)
	}

	// Assert: 回収対象は stale 側 1 件のみ。
	if len(recovered) != 1 {
		t.Fatalf("recovered len = %d; want 1（stale のみ回収）", len(recovered))
	}
	if recovered[0] != staleID {
		t.Errorf("recovered[0] = %v; want %v（stale id）", recovered[0], staleID)
	}

	// Assert: stale 側は pending_bind へ回収、fresh 側は binding 据え置き。
	gotStale, err := repo.Get(saCtx, staleID)
	if err != nil {
		t.Fatalf("Get(stale): %v", err)
	}
	if gotStale.Status != tenant.StatusPendingBind {
		t.Errorf("stale.Status = %q; want %q（回収済み）", gotStale.Status, tenant.StatusPendingBind)
	}
	gotFresh, err := repo.Get(saCtx, freshID)
	if err != nil {
		t.Fatalf("Get(fresh): %v", err)
	}
	if gotFresh.Status != tenant.StatusBinding {
		t.Errorf("fresh.Status = %q; want %q（据え置き）", gotFresh.Status, tenant.StatusBinding)
	}
}

// TestTenantRepository_UpdateBound_PendingBindRowNotAffected はシナリオ (4) 対応。
// #52 で UpdateBound の WHERE が status='pending_bind' から status='binding' へ変更された回帰を
// 検証する。ReserveBinding を経ていない pending_bind 行への UpdateBound は WHERE に掛からず
// affected=0 となり、bound へ部分遷移しない（Req 1.4 / Req 4.3）。
func TestTenantRepository_UpdateBound_PendingBindRowNotAffected(t *testing.T) {
	// Arrange: pending_bind 行（Insert 直後、ReserveBinding せず）
	repo, ctx, cleanup := setupTenantRepo(t)
	defer cleanup()
	id := uuid.New()
	if err := repo.Insert(ctx, tenant.TenantRow{ID: id, Name: "PendingBind Row"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// Act: WHERE status='binding' へ変更されたため pending_bind 行は対象外
	affected, err := repo.UpdateBound(ctx, id, "enterprises/LC9001")
	if err != nil {
		t.Fatalf("UpdateBound: %v", err)
	}

	// Assert: affected=0（Req 1.4 の WHERE 変更回帰）かつ状態は pending_bind のまま（部分遷移なし）
	if affected != 0 {
		t.Errorf("UpdateBound(pending_bind) affected = %d; want 0（WHERE binding）", affected)
	}
	got, err := repo.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != tenant.StatusPendingBind {
		t.Errorf("Get().Status = %q; want %q（bound へ部分遷移しない）", got.Status, tenant.StatusPendingBind)
	}
}

// TestTenantRepository_UpdateBound_BindingRowConfirmsBound はシナリオ (4) の対比（正常系）。
// 予約勝者（binding）への UpdateBound は affected=1 で bound へ確定し enterprise_name を保存する
// （2 段確定の確定側 / Req 1.4）。シナリオ (4) の pending_bind 行 affected=0 と対で WHERE binding の
// 意味を明確化する。
func TestTenantRepository_UpdateBound_BindingRowConfirmsBound(t *testing.T) {
	// Arrange: binding 行（Insert→ReserveBinding）
	repo, ctx, cleanup := setupTenantRepo(t)
	defer cleanup()
	id := uuid.New()
	if err := repo.Insert(ctx, tenant.TenantRow{ID: id, Name: "Binding Row"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if affected, err := repo.ReserveBinding(ctx, id); err != nil || affected != 1 {
		t.Fatalf("ReserveBinding(setup): affected=%d err=%v; want affected=1 err=nil", affected, err)
	}

	// Act: 予約勝者（binding）への UpdateBound は affected=1 で bound 確定
	affected, err := repo.UpdateBound(ctx, id, "enterprises/LC9002")
	if err != nil {
		t.Fatalf("UpdateBound: %v", err)
	}

	// Assert: affected=1 かつ bound + enterprise_name 保存
	if affected != 1 {
		t.Fatalf("UpdateBound(binding) affected = %d; want 1", affected)
	}
	got, err := repo.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != tenant.StatusBound {
		t.Errorf("Get().Status = %q; want %q", got.Status, tenant.StatusBound)
	}
	if got.EnterpriseName != "enterprises/LC9002" {
		t.Errorf("Get().EnterpriseName = %q; want %q", got.EnterpriseName, "enterprises/LC9002")
	}
}

// TestTenantRepository_BindingDisableConflict はシナリオ (5) 対応。
// binding 行への UpdateDisabled は affected=1 で disabled へ遷移し監査列（disabled_at /
// disabled_by）を記録する。無効化の勝者確定後、同 id への UpdateBound は status が binding では
// ないため affected=0（binding↔disable 競合の敗者）となり、予約中要求と無効化要求のいずれか
// 一方のみが確定することを確認する（Req 1.6）。
func TestTenantRepository_BindingDisableConflict(t *testing.T) {
	// Arrange: binding 行（Insert→ReserveBinding）
	repo, ctx, cleanup := setupTenantRepo(t)
	defer cleanup()
	id := uuid.New()
	actor := uuid.New()
	if err := repo.Insert(ctx, tenant.TenantRow{ID: id, Name: "Binding Disable Target"}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if affected, err := repo.ReserveBinding(ctx, id); err != nil || affected != 1 {
		t.Fatalf("ReserveBinding(setup): affected=%d err=%v; want affected=1 err=nil", affected, err)
	}

	// Act + Assert: binding 行への UpdateDisabled は affected=1 で disabled 遷移
	disabledAffected, err := repo.UpdateDisabled(ctx, id, actor)
	if err != nil {
		t.Fatalf("UpdateDisabled: %v", err)
	}
	if disabledAffected != 1 {
		t.Fatalf("UpdateDisabled(binding) affected = %d; want 1", disabledAffected)
	}
	got, err := repo.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get after disable: %v", err)
	}
	if got.Status != tenant.StatusDisabled {
		t.Errorf("Get().Status = %q; want %q", got.Status, tenant.StatusDisabled)
	}
	if got.DisabledAt == nil {
		t.Errorf("Get().DisabledAt = nil; want 非 nil（監査列記録）")
	}
	if got.DisabledBy == nil {
		t.Errorf("Get().DisabledBy = nil; want %v", actor)
	} else if *got.DisabledBy != actor {
		t.Errorf("Get().DisabledBy = %v; want %v", *got.DisabledBy, actor)
	}

	// Act + Assert: 無効化の勝者確定後、同 id への UpdateBound は affected=0（binding↔disable 競合の敗者）
	boundAffected, err := repo.UpdateBound(ctx, id, "enterprises/LC9003")
	if err != nil {
		t.Fatalf("UpdateBound after disable: %v", err)
	}
	if boundAffected != 0 {
		t.Errorf("UpdateBound(disabled) affected = %d; want 0（binding↔disable 競合の敗者 / Req 1.6）", boundAffected)
	}
}

// TestTenantRepository_SignupURLName_PersistRoundTrip はシナリオ (6) 対応。
// Insert で永続化した signup_url_name が Get で往復一致し、未指定（空文字）の行は
// NULL→空文字写像で Get も空文字になることを確認する（発行元テナントへの束縛の正本 / Req 3.1）。
func TestTenantRepository_SignupURLName_PersistRoundTrip(t *testing.T) {
	// Arrange
	repo, ctx, cleanup := setupTenantRepo(t)
	defer cleanup()

	// Act + Assert: signup_url_name 指定で Insert→Get が一致（永続化往復）
	withURL := uuid.New()
	const signupURLName = "signupUrls/C-ABC123DEF"
	if err := repo.Insert(ctx, tenant.TenantRow{ID: withURL, Name: "With SignupURL", SignupURLName: signupURLName}); err != nil {
		t.Fatalf("Insert(with signup_url_name): %v", err)
	}
	got, err := repo.Get(ctx, withURL)
	if err != nil {
		t.Fatalf("Get(with signup_url_name): %v", err)
	}
	if got.SignupURLName != signupURLName {
		t.Errorf("Get().SignupURLName = %q; want %q（永続化往復）", got.SignupURLName, signupURLName)
	}

	// Act + Assert: signup_url_name 未指定（空文字）は NULL→空文字写像で Get も空文字
	withoutURL := uuid.New()
	if err := repo.Insert(ctx, tenant.TenantRow{ID: withoutURL, Name: "Without SignupURL"}); err != nil {
		t.Fatalf("Insert(without signup_url_name): %v", err)
	}
	gotEmpty, err := repo.Get(ctx, withoutURL)
	if err != nil {
		t.Fatalf("Get(without signup_url_name): %v", err)
	}
	if gotEmpty.SignupURLName != "" {
		t.Errorf("Get().SignupURLName = %q; want \"\"（NULL→空文字写像）", gotEmpty.SignupURLName)
	}
}
