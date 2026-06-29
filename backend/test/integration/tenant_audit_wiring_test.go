package integration_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/hitoshiichikawa/ae-mdm/internal/audit"
	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	platformdb "github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
	"github.com/hitoshiichikawa/ae-mdm/internal/tenant"
	"github.com/hitoshiichikawa/ae-mdm/internal/tenantaudit"
)

// 本ファイルは Issue #54 の tenant 監査配線アダプタ（internal/tenantaudit）を、**実 audit.Repository**
// （`audit_logs` への FK / RLS / jsonb 永続化）まで通す integration test。`DATABASE_URL` 未設定環境
// では requireDBURLs が t.Skip するため、DB 不在でも false-fail しない（helpers_test.go の規約）。
//
// PR #56 round-1 レビュー指摘（spy だけでは実 Repository の FK / RLS / JSON 永続化経路が未検証）への対応。
// アダプタ単体テスト（internal/tenantaudit/*_test.go）は spy / fake Repository で写像と委譲契約を押さえ、
// 本ファイルが実 DB 経由の永続化 → 閲覧（List）と FK 制約の実挙動を補完する。

// setupTenantAuditWiring は migrate-up → truncate → seed → 実 audit.Service へ配線したアダプタを返す。
// 返す ctx は SuperAdmin TenantContext を確立済みで、audit Repository の append-only INSERT / SELECT
// が RLS を通過できる（tenant_repository_test.go の setupTenantRepo と同方式）。
func setupTenantAuditWiring(t *testing.T) (rec *tenantaudit.Recorder, svc audit.Service, ids seededIDs, superAdminCtx context.Context, cleanup func()) {
	t.Helper()
	urls := requireDBURLs(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	applyMigrationsUp(t, urls.migrate)
	truncateAll(t, ctx, urls)
	pool := newAppPool(t, ctx, urls)
	ids = seedDummyData(t, ctx, pool)

	// 実 audit.Service（本番実装）へ実 Repository を注入し、tenantaudit アダプタで配線する。
	// retentionDays は List の保持下限が直近 INSERT を除外しないよう十分大きい値にする。
	svc = audit.NewService(
		config.Config{AuditLogRetentionDays: 365},
		audit.NewRepository(pool),
		audit.SystemClock{},
		nil,
	)
	rec = tenantaudit.NewRecorder(svc, nil)

	superAdminCtx = platformdb.WithTenantContext(ctx, platformdb.TenantContext{
		TenantID:     uuid.Nil,
		IsSuperAdmin: true,
	})
	cleanup = func() {
		pool.Close()
		cancel()
	}
	return rec, svc, ids, superAdminCtx, cleanup
}

// TestTenantAuditWiring_SuccessEventPersistsAndIsListable は、実在テナントへの成功監査イベントが
// 実 audit.Repository を通って `audit_logs` へ永続化され、List（/api/admin/audit-logs の backing）で
// 写像どおりに閲覧可能になることを検証する（Req 1.1 / 1.3 / 2.1〜2.3 / 2.8 を実 DB 経由で確認）。
func TestTenantAuditWiring_SuccessEventPersistsAndIsListable(t *testing.T) {
	// Arrange
	rec, svc, ids, ctx, cleanup := setupTenantAuditWiring(t)
	defer cleanup()

	ev := tenant.Event{
		Actor:     ids.adminAID,  // 実在 admin_users（actor_id FK を満たす）
		TenantID:  ids.tenantAID, // 実在 tenants（tenant_id FK を満たす）
		Operation: tenant.OperationCreate,
		Result:    tenant.ResultSuccess,
	}

	// Act: アダプタ → 実 audit.Service → 実 Repository → audit_logs へ INSERT。
	if err := rec.Record(ctx, ev); err != nil {
		t.Fatalf("Record() 予期しないエラー: %v", err)
	}

	// Assert: tenant_create で絞り込んだ List に、写像どおりの行が 1 件だけ現れる。
	got, err := svc.List(ctx, audit.Filter{EventType: tenantaudit.EventTypeCreate})
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List(tenant_create) 件数 = %d, want 1（実 Repository 経由の永続化 + 閲覧）", len(got))
	}
	row := got[0]
	if row.TenantID != ids.tenantAID {
		t.Errorf("TenantID = %v, want %v (Req 2.2)", row.TenantID, ids.tenantAID)
	}
	if row.ActorID != ids.adminAID {
		t.Errorf("ActorID = %v, want %v (Req 2.1)", row.ActorID, ids.adminAID)
	}
	if row.ResourceID != ids.tenantAID.String() {
		t.Errorf("ResourceID = %q, want %q (Req 2.8)", row.ResourceID, ids.tenantAID.String())
	}
	if row.Result != audit.ResultSuccess {
		t.Errorf("Result = %q, want %q (Req 2.3)", row.Result, audit.ResultSuccess)
	}
	if row.ID == uuid.Nil {
		t.Errorf("ID = uuid.Nil, want 採番済み（audit.Service が補完 / Req 2.9）")
	}
	if row.OccurredAt.IsZero() {
		t.Errorf("OccurredAt = zero, want clock 補完済み（Req 2.9）")
	}
	// detail(jsonb) に非機密の confirmation_completed が往復して載ること（Req 4.2 / 永続化往復）。
	if _, ok := row.Detail["confirmation_completed"]; !ok {
		t.Errorf("detail に confirmation_completed が往復しない: got %v", row.Detail)
	}
}

// TestTenantAuditWiring_NonexistentTenantFailureIsRejectedByFK は、**存在しない非 nil tenant_id** を
// 載せた失敗監査イベントが `audit_logs.tenant_id` の FK で拒否され、アダプタが当該エラーを伝播する
// （fail-closed / Req 3.1 / 3.2）ことを実 DB で検証する。
//
// これは現状の既知の制約を実挙動として固定するテストである: bind / disable の lookup failure 等、
// 実在しないテナント id を載せた失敗監査は永続化されず `/api/admin/audit-logs` に残らない。
// 根本対処（FK 緩和 or 失敗時 tenant_id=NULL 規則）はスキーマ責務をまたぐため本 PR スコープ外で、
// impl-notes.md「確認事項 5」で別 Issue 化を申し送る。本テストはその制約の境界（FK 拒否 + error 伝播）を
// 明示的に観測し、回帰時に検知できるようにする。
func TestTenantAuditWiring_NonexistentTenantFailureIsRejectedByFK(t *testing.T) {
	// Arrange
	rec, svc, ids, ctx, cleanup := setupTenantAuditWiring(t)
	defer cleanup()

	nonexistentTenant := uuid.New() // tenants に seed していない id
	ev := tenant.Event{
		Actor:     ids.adminAID, // actor は実在（FK 違反の原因を tenant_id に限定する）
		TenantID:  nonexistentTenant,
		Operation: tenant.OperationBind,
		Result:    tenant.ResultFailure,
	}

	// Act
	err := rec.Record(ctx, ev)

	// Assert: FK 違反で永続化が失敗し、アダプタは握りつぶさず error を伝播する（Req 3.1 / 3.2）。
	if err == nil {
		t.Fatalf("存在しない tenant_id の失敗監査が FK で拒否されず成功した（FK 制約 or 写像挙動の回帰の疑い）")
	}

	// Assert: 当該失敗イベントは audit_logs に残らない（List に現れない）。
	got, listErr := svc.List(ctx, audit.Filter{EventType: tenantaudit.EventTypeBind})
	if listErr != nil {
		t.Fatalf("List(): %v", listErr)
	}
	if len(got) != 0 {
		t.Errorf("FK 拒否された失敗監査が永続化されている: List(tenant_bind) 件数 = %d, want 0", len(got))
	}
}
