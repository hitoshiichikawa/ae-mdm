package integration_test

import (
	"context"
	stdErrors "errors"
	"testing"

	"github.com/google/uuid"

	internalerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	platformdb "github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
	"github.com/hitoshiichikawa/ae-mdm/internal/policy"
)

// 本ファイルは Issue #40 (B2b) task 2.1 の policy.Repository（raw pgx + RLS + 複合 FK +
// per-policy advisory lock 直列化）に対する integration test。DATABASE_URL 未設定環境では
// requireDBURLs が t.Skip するため、DB 不在でも false-fail しない（helpers_test.go の規約）。
//
// tasks.md task 2.1 が要求する SQL 分岐を実 DB に対して直接検証する:
//   (a) Insert→Get の round-trip（RETURNING timestamps 充填 / version 永続化 / Req 1.3 / NFR 2.1）
//   (b) UpdateSnapshotSerialized affected=1（reflect 実行 → 反映済み version で UPDATE / Req 1.5）
//   (c) UpdateSnapshotSerialized affected=0（不在行 / Service が NotFound 写像する材料 / Req 4.4 / 4.5）
//   (d) UpdateSnapshotSerialized reflect error → snapshot 未確定（AMAPI 反映失敗時 / Req 1.4 / 1.5）
//   (e) Get 不在 → ErrPolicyNotFound（CodeNotFound / Req 4.4 / 4.5）
//   (f) Delete 割当済み policy → FK 違反 → ErrDeleteConflict（CodeConflict / Req 5.3）
//   (g) Delete 不在 → affected=0（Req 5.3）
//   (h) AssignPolicyToDevice 他テナント policy → 複合 FK 違反 → ErrPolicyNotFound（Req 3.2 / 4.2）
//   (i) AssignPolicyToDevice 他テナント device → affected=0（Req 3.3 / 4.3）

// setupPolicyRepo は migrate-up → truncate → seedDummyData → app pool 構築までを担う共通 setup。
// 返す ctx は tenant A の TenantContext（非 SuperAdmin）を確立済みで、RLS が tenant A に閉じる。
// seedDummyData が tenant A/B に policy A/B・device A/B を投入済み。
func setupPolicyRepo(t *testing.T) (repo policy.Repository, ids seededIDs, ctxA context.Context, cleanup func()) {
	t.Helper()
	urls := requireDBURLs(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	applyMigrationsUp(t, urls.migrate)
	truncateAll(t, ctx, urls)
	pool := newAppPool(t, ctx, urls)
	ids = seedDummyData(t, ctx, pool)
	repo = policy.NewRepository(pool)
	ctxA = platformdb.WithTenantContext(ctx, platformdb.TenantContext{
		TenantID:     ids.tenantAID,
		IsSuperAdmin: false,
	})
	cleanup = func() {
		pool.Close()
		cancel()
	}
	return repo, ids, ctxA, cleanup
}

// noopReflect は AMAPI 反映を伴わない reflect（DB 分岐のみを検証する用途）。返す version を固定する。
func noopReflect(version int64) policy.ReflectFunc {
	return func(_ context.Context) (int64, error) {
		return version, nil
	}
}

// TestPolicyRepository_InsertGet_RoundTrip はシナリオ (a) 対応。
// Insert が RETURNING で timestamps を充填し、Get が version / name / body を型付きで読み戻す
// （NFR 2.1 / Req 1.3）。
func TestPolicyRepository_InsertGet_RoundTrip(t *testing.T) {
	// Arrange
	repo, ids, ctx, cleanup := setupPolicyRepo(t)
	defer cleanup()

	newID := uuid.New()
	row := policy.PolicyRow{
		ID:              newID,
		TenantID:        ids.tenantAID,
		Name:            "kiosk-policy",
		AMAPIPolicyName: "enterprises/X/policies/" + newID.String(),
		Body:            map[string]any{"kioskMode": true, "label": "frontline"},
		Version:         4,
	}

	// Act: Insert
	inserted, err := repo.Insert(ctx, row)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// Assert: RETURNING で timestamps が非 zero（Req 1.3）
	if inserted.CreatedAt.IsZero() || inserted.UpdatedAt.IsZero() {
		t.Errorf("Insert は created_at / updated_at を充填すべき: got %+v / %+v", inserted.CreatedAt, inserted.UpdatedAt)
	}

	// Act: Get
	got, err := repo.Get(ctx, ids.tenantAID, newID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// Assert: 列ごと型付き scan（NFR 2.1）
	if got.ID != newID || got.Name != "kiosk-policy" {
		t.Errorf("Get round-trip mismatch: got id=%v name=%q", got.ID, got.Name)
	}
	if got.Version != 4 {
		t.Errorf("Get().Version = %d; want 4", got.Version)
	}
	if b, ok := got.Body["kioskMode"].(bool); !ok || !b {
		t.Errorf("Get().Body[kioskMode] = %v; want true", got.Body["kioskMode"])
	}
}

// TestPolicyRepository_UpdateSnapshotSerialized_Affected はシナリオ (b) 対応。
// reflect（AMAPI 反映 + version 取得を模す）が advisory lock 保持中に実行され、返された反映済み
// version で snapshot が UPDATE されること（affected=1）を検証する（Req 1.5）。
func TestPolicyRepository_UpdateSnapshotSerialized_Affected(t *testing.T) {
	// Arrange: seed 済み policy A を更新する。
	repo, ids, ctx, cleanup := setupPolicyRepo(t)
	defer cleanup()

	reflectCalled := false
	const reflectedVersion = int64(9)
	reflect := func(_ context.Context) (int64, error) {
		reflectCalled = true
		return reflectedVersion, nil
	}
	updRow := policy.PolicyRow{
		ID:              ids.policyAID,
		TenantID:        ids.tenantAID,
		Name:            "renamed-policy",
		AMAPIPolicyName: "enterprises/X/policies/A",
		Body:            map[string]any{"updated": true},
	}

	// Act
	persisted, affected, err := repo.UpdateSnapshotSerialized(ctx, updRow, reflect)
	if err != nil {
		t.Fatalf("UpdateSnapshotSerialized: %v", err)
	}

	// Assert: reflect が実行され affected=1、反映済み version が充填される（Req 1.5）。
	if !reflectCalled {
		t.Error("reflect は advisory lock 保持中に実行されるべき")
	}
	if affected != 1 {
		t.Fatalf("affected = %d; want 1", affected)
	}
	if persisted.Version != reflectedVersion {
		t.Errorf("persisted.Version = %d; want %d（反映済み version）", persisted.Version, reflectedVersion)
	}

	// Assert: 再 Get で snapshot が永続化されている。
	got, err := repo.Get(ctx, ids.tenantAID, ids.policyAID)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if got.Name != "renamed-policy" || got.Version != reflectedVersion {
		t.Errorf("更新が永続化されていない: got name=%q version=%d", got.Name, got.Version)
	}
	if b, ok := got.Body["updated"].(bool); !ok || !b {
		t.Errorf("Body が更新されていない: got %+v", got.Body)
	}
}

// TestPolicyRepository_UpdateSnapshotSerialized_NoRows はシナリオ (c) 対応。
// 不在 id への UpdateSnapshotSerialized は affected=0（error には倒さない）を返し、Service が
// NotFound 写像する材料にすること（Req 4.4 / 4.5）。
func TestPolicyRepository_UpdateSnapshotSerialized_NoRows(t *testing.T) {
	// Arrange
	repo, ids, ctx, cleanup := setupPolicyRepo(t)
	defer cleanup()

	missingID := uuid.New()
	updRow := policy.PolicyRow{
		ID:              missingID,
		TenantID:        ids.tenantAID,
		Name:            "ghost",
		AMAPIPolicyName: "enterprises/X/policies/ghost",
		Body:            map[string]any{},
	}

	// Act
	_, affected, err := repo.UpdateSnapshotSerialized(ctx, updRow, noopReflect(1))

	// Assert（Req 4.4 / 4.5）。
	if err != nil {
		t.Fatalf("不在行の Update は error に倒さない: got %v", err)
	}
	if affected != 0 {
		t.Errorf("affected = %d; want 0", affected)
	}
}

// TestPolicyRepository_UpdateSnapshotSerialized_ReflectError はシナリオ (d) 対応。
// reflect が error を返した（AMAPI 反映失敗）場合、snapshot を書かずに rollback し error を伝達する
// こと、既存 snapshot が変更されないことを検証する（Req 1.4 / 1.5）。
func TestPolicyRepository_UpdateSnapshotSerialized_ReflectError(t *testing.T) {
	// Arrange
	repo, ids, ctx, cleanup := setupPolicyRepo(t)
	defer cleanup()

	before, err := repo.Get(ctx, ids.tenantAID, ids.policyAID)
	if err != nil {
		t.Fatalf("Get before: %v", err)
	}

	sentinel := stdErrors.New("amapi upsert failed")
	reflect := func(_ context.Context) (int64, error) {
		return 0, sentinel
	}
	updRow := policy.PolicyRow{
		ID:              ids.policyAID,
		TenantID:        ids.tenantAID,
		Name:            "should-not-apply",
		AMAPIPolicyName: "enterprises/X/policies/A",
		Body:            map[string]any{"applied": true},
	}

	// Act
	_, _, err = repo.UpdateSnapshotSerialized(ctx, updRow, reflect)

	// Assert: reflect の error が伝達される（Req 1.4）。
	if !stdErrors.Is(err, sentinel) {
		t.Fatalf("reflect の error を伝達すべき: got %v", err)
	}

	// Assert: snapshot は未変更（中間状態を確定しない / Req 1.5）。
	after, err := repo.Get(ctx, ids.tenantAID, ids.policyAID)
	if err != nil {
		t.Fatalf("Get after: %v", err)
	}
	if after.Name != before.Name {
		t.Errorf("reflect 失敗で snapshot が変更された: name %q → %q", before.Name, after.Name)
	}
}

// TestPolicyRepository_Get_NotFound はシナリオ (e) 対応。
// 不在 id の Get が ErrPolicyNotFound（CodeNotFound / 存在差非露出）を返すこと（Req 4.4 / 4.5）。
func TestPolicyRepository_Get_NotFound(t *testing.T) {
	// Arrange
	repo, ids, ctx, cleanup := setupPolicyRepo(t)
	defer cleanup()

	// Act
	_, err := repo.Get(ctx, ids.tenantAID, uuid.New())

	// Assert
	if !stdErrors.Is(err, policy.ErrPolicyNotFound) {
		t.Fatalf("err は ErrPolicyNotFound を期待; got %v", err)
	}
	var domainErr *internalerrors.Error
	if !stdErrors.As(err, &domainErr) || domainErr.Code != internalerrors.CodeNotFound {
		t.Errorf("Code = NotFound を期待; got %v", err)
	}
}

// TestPolicyRepository_Delete_FKConflict はシナリオ (f)(g) 対応。
// 割当済み端末が存在する policy の Delete が複合 FK 違反 → ErrDeleteConflict（CodeConflict / 409）に
// 写像されること、不在 id の Delete が affected=0 を返すこと（Req 5.3）。
func TestPolicyRepository_Delete_FKConflict(t *testing.T) {
	// Arrange: device A を policy A に割り当て、policy A を参照状態にする。
	repo, ids, ctx, cleanup := setupPolicyRepo(t)
	defer cleanup()

	affected, err := repo.AssignPolicyToDevice(ctx, ids.tenantAID, ids.deviceAID, ids.policyAID)
	if err != nil {
		t.Fatalf("AssignPolicyToDevice(setup): %v", err)
	}
	if affected != 1 {
		t.Fatalf("割当 setup の affected = %d; want 1", affected)
	}

	// Act: 割当済み policy A を Delete → FK 違反
	_, err = repo.Delete(ctx, ids.tenantAID, ids.policyAID)

	// Assert（Req 5.3）。
	if !stdErrors.Is(err, policy.ErrDeleteConflict) {
		t.Fatalf("err は ErrDeleteConflict を期待; got %v", err)
	}
	var domainErr *internalerrors.Error
	if !stdErrors.As(err, &domainErr) || domainErr.Code != internalerrors.CodeConflict {
		t.Errorf("Code = Conflict を期待; got %v", err)
	}

	// Act + Assert: 不在 id の Delete は affected=0（Req 5.3）。
	delAff, err := repo.Delete(ctx, ids.tenantAID, uuid.New())
	if err != nil {
		t.Fatalf("不在 id の Delete は error に倒さない: got %v", err)
	}
	if delAff != 0 {
		t.Errorf("不在 Delete の affected = %d; want 0", delAff)
	}
}

// TestPolicyRepository_Assign_CrossTenant はシナリオ (h)(i) 対応。
// 他テナント policy 指定は複合 FK 違反 → ErrPolicyNotFound（存在差非露出）、他テナント device 指定は
// RLS / WHERE tenant_id で affected=0 になること（Req 3.2 / 3.3 / 4.2 / 4.3）。
func TestPolicyRepository_Assign_CrossTenant(t *testing.T) {
	// Arrange
	repo, ids, ctx, cleanup := setupPolicyRepo(t)
	defer cleanup()

	// Act + Assert (h): tenant A ctx で device A に他テナント policy B を割当 → 複合 FK 違反。
	_, err := repo.AssignPolicyToDevice(ctx, ids.tenantAID, ids.deviceAID, ids.policyBID)
	if !stdErrors.Is(err, policy.ErrPolicyNotFound) {
		t.Fatalf("他テナント policy 割当は ErrPolicyNotFound を期待; got %v", err)
	}

	// Act + Assert (i): tenant A ctx で他テナント device B を割当先指定 → RLS で 0 行。
	affected, err := repo.AssignPolicyToDevice(ctx, ids.tenantAID, ids.deviceBID, ids.policyAID)
	if err != nil {
		t.Fatalf("他テナント device 割当は error に倒さない: got %v", err)
	}
	if affected != 0 {
		t.Errorf("他テナント device 割当の affected = %d; want 0", affected)
	}
}
