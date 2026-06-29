package integration_test

import (
	"context"
	stdErrors "errors"
	"math"
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
//   (j) List 自テナント scoped（自テナント行のみ created_at ASC / 他テナント行不可視 / Req 4.4）
//   (k) Get 他テナント越境 → ErrPolicyNotFound（RLS / 存在差非露出 / Req 4.4 / 4.5）
//   (l) UpdateSnapshotSerialized 不在行は reflect を呼ばない（AMAPI 未反映 = 乖離防止 / Req 1.5）
//   (m) golden path lifecycle: Insert→Get→Update→Get→Delete→Get(NotFound)（Req 1.x / 4.4 / 5.3）
//   (n) 連続更新で snapshot が最新反映済みと一致（中間状態を残さない / Req 1.5）

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

// TestPolicyRepository_InsertGet_LargeVersion はシナリオ (a) の型幅境界対応（PR #60 review round 4 /
// Req 1.3 / NFR 2.1）。AMAPI 反映済み version は `amapi.PolicyBody.Version int64` 由来で、
// `PolicyRow.Version` も int64。policies.version 列が int32（integer）のままだと int32 範囲外の
// version で INSERT が "integer out of range" 失敗し snapshot 永続化が成立しない。migration 0017 で
// 列を bigint へ広げたことで int32 範囲外の version が round-trip することを実 DB で検証する。
func TestPolicyRepository_InsertGet_LargeVersion(t *testing.T) {
	// Arrange: int32 上限を超える version（bigint でのみ保持可能）。
	repo, ids, ctx, cleanup := setupPolicyRepo(t)
	defer cleanup()

	newID := uuid.New()
	const largeVersion = int64(math.MaxInt32) + 1 // 2147483648（int32 範囲外）
	row := policy.PolicyRow{
		ID:              newID,
		TenantID:        ids.tenantAID,
		Name:            "large-version-policy",
		AMAPIPolicyName: "enterprises/X/policies/" + newID.String(),
		Body:            map[string]any{"label": "frontline"},
		Version:         largeVersion,
	}

	// Act: Insert（旧 integer 列ではここで "integer out of range" 失敗していた）
	if _, err := repo.Insert(ctx, row); err != nil {
		t.Fatalf("Insert with int32-overflow version: %v", err)
	}

	// Act: Get
	got, err := repo.Get(ctx, ids.tenantAID, newID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// Assert: int32 範囲外 version が無損失で読み戻る（bigint / NFR 2.1）。
	if got.Version != largeVersion {
		t.Errorf("Get().Version = %d; want %d (int32 範囲外 version の無損失永続化)", got.Version, largeVersion)
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

// TestPolicyRepository_List_TenantScoped はシナリオ (j) 対応。
// tenant A ctx の List が自テナント行のみを created_at ASC で返し、他テナント（B）の policy を
// 一切露出しないこと（RLS / Req 4.4）。seed の policy A に加えて policy A2 を投入し、複数行の
// 並び順と他テナント不可視を同時に検証する。
func TestPolicyRepository_List_TenantScoped(t *testing.T) {
	// Arrange: tenant A に 2 件目の policy を追加（seed の policy A と合わせて 2 件）。
	repo, ids, ctx, cleanup := setupPolicyRepo(t)
	defer cleanup()

	policyA2 := uuid.New()
	if _, err := repo.Insert(ctx, policy.PolicyRow{
		ID:              policyA2,
		TenantID:        ids.tenantAID,
		Name:            "policy-A2",
		AMAPIPolicyName: "enterprises/X/policies/" + policyA2.String(),
		Body:            map[string]any{"order": 2},
		Version:         1,
	}); err != nil {
		t.Fatalf("Insert policy-A2: %v", err)
	}

	// Act
	rows, err := repo.List(ctx, ids.tenantAID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	// Assert: 自テナント行のみ（policy A / A2 の 2 件）。他テナント policy B は不可視（Req 4.4）。
	if len(rows) != 2 {
		t.Fatalf("List 件数 = %d; want 2（自テナント行のみ）: %+v", len(rows), rows)
	}
	for _, r := range rows {
		if r.TenantID != ids.tenantAID {
			t.Errorf("List が他テナント行を返した: tenant_id=%v", r.TenantID)
		}
		if r.ID == ids.policyBID {
			t.Errorf("List に他テナント policy B が露出している: %v", r.ID)
		}
	}
	// created_at ASC: seed の policy A（先に挿入）→ policy A2（後に挿入）の順。
	if rows[0].ID != ids.policyAID || rows[1].ID != policyA2 {
		t.Errorf("List は created_at ASC で返すべき: got [%v, %v]", rows[0].ID, rows[1].ID)
	}
}

// TestPolicyRepository_Get_CrossTenant はシナリオ (k) 対応。
// tenant A ctx で他テナント（B）の policy を Get すると、RLS で 0 行 → 存在差を露出しない
// ErrPolicyNotFound（CodeNotFound）に写像されること（Req 4.4 / 4.5）。実在する policy B id を
// 指定しても「未検出」と区別不能な応答になることを実 DB / RLS で確認する。
func TestPolicyRepository_Get_CrossTenant(t *testing.T) {
	// Arrange
	repo, ids, ctx, cleanup := setupPolicyRepo(t)
	defer cleanup()

	// Act: tenant A ctx で実在する他テナント policy B を Get。
	_, err := repo.Get(ctx, ids.tenantAID, ids.policyBID)

	// Assert: 実在しても RLS で 0 行 → 存在差非露出の ErrPolicyNotFound（Req 4.5）。
	if !stdErrors.Is(err, policy.ErrPolicyNotFound) {
		t.Fatalf("他テナント policy の Get は ErrPolicyNotFound を期待; got %v", err)
	}
	var domainErr *internalerrors.Error
	if !stdErrors.As(err, &domainErr) || domainErr.Code != internalerrors.CodeNotFound {
		t.Errorf("Code = NotFound を期待; got %v", err)
	}
}

// TestPolicyRepository_UpdateSnapshotSerialized_RowMissing_SkipsReflect はシナリオ (l) 対応。
// 不在行への UpdateSnapshotSerialized は reflect の前に FOR UPDATE 存在再確認で 0 行を検出し、
// reflect（AMAPI 反映）を一切呼ばずに affected=0 を返すこと（「DB に行が無いのに AMAPI だけ更新
// される」乖離を未然に防ぐ / Req 1.5 / 4.4 / 4.5）。
func TestPolicyRepository_UpdateSnapshotSerialized_RowMissing_SkipsReflect(t *testing.T) {
	// Arrange
	repo, ids, ctx, cleanup := setupPolicyRepo(t)
	defer cleanup()

	reflectCalled := false
	reflect := func(_ context.Context) (int64, error) {
		reflectCalled = true
		return 1, nil
	}
	updRow := policy.PolicyRow{
		ID:              uuid.New(), // 不在 id。
		TenantID:        ids.tenantAID,
		Name:            "ghost",
		AMAPIPolicyName: "enterprises/X/policies/ghost",
		Body:            map[string]any{},
	}

	// Act
	_, affected, err := repo.UpdateSnapshotSerialized(ctx, updRow, reflect)

	// Assert: reflect 未実行（AMAPI 未反映）+ affected=0（Req 1.5 / 4.4）。
	if err != nil {
		t.Fatalf("不在行の Update は error に倒さない: got %v", err)
	}
	if reflectCalled {
		t.Error("不在行では reflect（AMAPI 反映）を呼んではならない（乖離防止 / Req 1.5）")
	}
	if affected != 0 {
		t.Errorf("affected = %d; want 0", affected)
	}
}

// TestPolicyRepository_Lifecycle_GoldenPath はシナリオ (m) 対応。
// Insert→Get→UpdateSnapshotSerialized→Get→Delete→Get(NotFound) の repository-level golden path を
// 実 DB に対して通し、各段の永続化が後続段に反映されること（Req 1.x / 4.4 / 5.3）を検証する。
// HTTP routing 経由の E2E は deferrable task 7 のスコープだが、本テストで AC 1.5 の統合挙動（実
// repository + reflect fake）を確認する。
func TestPolicyRepository_Lifecycle_GoldenPath(t *testing.T) {
	// Arrange
	repo, ids, ctx, cleanup := setupPolicyRepo(t)
	defer cleanup()

	newID := uuid.New()

	// (1) Insert
	if _, err := repo.Insert(ctx, policy.PolicyRow{
		ID:              newID,
		TenantID:        ids.tenantAID,
		Name:            "lifecycle",
		AMAPIPolicyName: "enterprises/X/policies/" + newID.String(),
		Body:            map[string]any{"stage": "created"},
		Version:         1,
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// (2) Get → 作成内容が読み戻せる。
	got, err := repo.Get(ctx, ids.tenantAID, newID)
	if err != nil {
		t.Fatalf("Get after insert: %v", err)
	}
	if got.Name != "lifecycle" {
		t.Errorf("Get after insert name = %q; want lifecycle", got.Name)
	}

	// (3) Update（reflect が反映済み version 12 を返す）。
	_, affected, err := repo.UpdateSnapshotSerialized(ctx, policy.PolicyRow{
		ID:              newID,
		TenantID:        ids.tenantAID,
		Name:            "lifecycle-updated",
		AMAPIPolicyName: "enterprises/X/policies/" + newID.String(),
		Body:            map[string]any{"stage": "updated"},
	}, noopReflect(12))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if affected != 1 {
		t.Fatalf("Update affected = %d; want 1", affected)
	}

	// (4) Get → 更新内容が永続化されている。
	got, err = repo.Get(ctx, ids.tenantAID, newID)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if got.Name != "lifecycle-updated" || got.Version != 12 {
		t.Errorf("更新が永続化されていない: name=%q version=%d", got.Name, got.Version)
	}
	if s, _ := got.Body["stage"].(string); s != "updated" {
		t.Errorf("Body が更新されていない: stage=%v", got.Body["stage"])
	}

	// (5) Delete → affected=1（割当端末なしのため FK 違反にならない）。
	delAff, err := repo.Delete(ctx, ids.tenantAID, newID)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if delAff != 1 {
		t.Fatalf("Delete affected = %d; want 1", delAff)
	}

	// (6) Get → 削除後は ErrPolicyNotFound（Req 4.4 / 5.3）。
	if _, err := repo.Get(ctx, ids.tenantAID, newID); !stdErrors.Is(err, policy.ErrPolicyNotFound) {
		t.Fatalf("削除後の Get は ErrPolicyNotFound を期待; got %v", err)
	}
}

// TestPolicyRepository_ConsecutiveUpdates_SnapshotMatchesLatest はシナリオ (n) 対応。
// 同一 policy への連続更新で、永続化された snapshot が「最新の反映済み内容」と一致し、AMAPI 反映前の
// 中間状態を確定 snapshot として残さないこと（Req 1.5）。advisory lock 直列化下で 2 回連続更新し、
// 最終 Get が 2 回目の version / body と一致することを確認する。
func TestPolicyRepository_ConsecutiveUpdates_SnapshotMatchesLatest(t *testing.T) {
	// Arrange: seed 済み policy A を起点に連続更新する。
	repo, ids, ctx, cleanup := setupPolicyRepo(t)
	defer cleanup()

	// Update 1: reflect version 10 / body step=1。
	if _, affected, err := repo.UpdateSnapshotSerialized(ctx, policy.PolicyRow{
		ID:              ids.policyAID,
		TenantID:        ids.tenantAID,
		Name:            "update-1",
		AMAPIPolicyName: "enterprises/X/policies/A",
		Body:            map[string]any{"step": float64(1)},
	}, noopReflect(10)); err != nil || affected != 1 {
		t.Fatalf("Update 1: affected=%d err=%v", affected, err)
	}

	// Update 2: reflect version 20 / body step=2。
	if _, affected, err := repo.UpdateSnapshotSerialized(ctx, policy.PolicyRow{
		ID:              ids.policyAID,
		TenantID:        ids.tenantAID,
		Name:            "update-2",
		AMAPIPolicyName: "enterprises/X/policies/A",
		Body:            map[string]any{"step": float64(2)},
	}, noopReflect(20)); err != nil || affected != 1 {
		t.Fatalf("Update 2: affected=%d err=%v", affected, err)
	}

	// Act: 最終 snapshot を読み戻す。
	got, err := repo.Get(ctx, ids.tenantAID, ids.policyAID)
	if err != nil {
		t.Fatalf("Get after consecutive updates: %v", err)
	}

	// Assert: snapshot は最新（2 回目）の反映済み内容と一致し、中間状態（step=1 / version 10）を
	// 残さない（Req 1.5）。
	if got.Name != "update-2" || got.Version != 20 {
		t.Errorf("最新反映済みと不一致: name=%q version=%d; want update-2 / 20", got.Name, got.Version)
	}
	if s, _ := got.Body["step"].(float64); s != 2 {
		t.Errorf("snapshot body が最新と不一致: step=%v; want 2", got.Body["step"])
	}
}
