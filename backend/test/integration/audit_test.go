package integration_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/hitoshiichikawa/ae-mdm/internal/audit"
	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/authz"
	platformdb "github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/httpserver"
)

// 本ファイルは Issue #5 task 6.1 の audit integration test。実 PostgreSQL に接続し、
// task 2 / task 5 の `_Requirements_partial:_`（実 DB INSERT・NULL bind・cross-tenant 可視 /
// wiring 起因の 401/403 ガード回帰）を解消する。DATABASE_URL 未設定環境では各テストが
// 自身で t.Skip するため DB 不在でも false-fail しない（手本: auth_repository_test.go /
// db_tenant_isolation_test.go / helpers_test.go の requireDBURLs / seedDummyData）。
//
// シナリオ（tasks.md task 6.1 L198-217 と 1:1）:
//   (a) tenant 文脈で Insert→Select：他テナント B / NULL 行が不可視（Req 2.9 / 4.3 / NFR 2.1）
//   (b) SuperAdmin 文脈で全件 occurred_at 降順（Req 3.1 / 3.5）
//   (c) SuperAdmin 文脈 + Filter.TenantID 指定で当該テナントのみ（Req 3.2）
//   (d) tenant / SuperAdmin いずれの文脈でも直接 UPDATE/DELETE が拒否（Req 6.1 / 6.2 / 6.3）
//   (e) SuperAdmin 文脈で NULL テナント行 Insert 後、その行への UPDATE/DELETE 拒否（Req 6.4）
//   (f) 保持期間下限：retention 起点より前の occurred_at 行を Service.List が除外（Req 5.2 / 5.3）
//   (g) 通常テナント文脈で他テナント tenant_id 行の Insert が WITH CHECK で拒否（NFR 2.2）+
//       保持期間内レコードが欠損なく取得（NFR 1.2）
//   (h) 絞り込み 0 件で Service.List が空 slice + nil（Req 2.8 / 3.4）
//   (i) routing スモーク：audit handler を Mount した test server で `/api/audit-logs` /
//       `/api/admin/audit-logs` が認証なしアクセス時に 401（Req 4.2 / 4.4 の wiring 起因回帰）

// auditFixedNow は audit integration test で使う基準時刻（UTC 固定）。
// fixedClock として Service へ渡し、保持期間下限（retentionFloor = now - retentionDays）を
// 決定的にする（手本: internal/audit/service_test.go の fixedNow）。
var auditFixedNow = time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)

// auditFixedClock は固定時刻を返す audit.Clock 実装（保持期間下限算出を決定的にする）。
type auditFixedClock struct {
	now time.Time
}

func (c auditFixedClock) Now() time.Time { return c.now }

// auditTestRetentionDays は audit integration test の保持期間（既定 180 日）。
const auditTestRetentionDays = 180

// recordViaService は tenant / SuperAdmin の TenantContext を確立した上で
// 実 Service.Record（→ Repository.Insert）で audit_logs に 1 行追記する。
// occurredAt を明示することで保持期間境界テストを決定的にする。
func recordViaService(
	t *testing.T,
	ctx context.Context,
	svc audit.Service,
	tc platformdb.TenantContext,
	ev audit.Event,
) {
	t.Helper()
	ctxTC := platformdb.WithTenantContext(ctx, tc)
	if err := svc.Record(ctxTC, ev); err != nil {
		t.Fatalf("svc.Record: %v", err)
	}
}

// listViaService は指定 TenantContext を確立した上で実 Service.List（→ Repository.Select）を
// 呼び、保持期間下限付きの結果を返す。
func listViaService(
	t *testing.T,
	ctx context.Context,
	svc audit.Service,
	tc platformdb.TenantContext,
	f audit.Filter,
) []audit.Event {
	t.Helper()
	ctxTC := platformdb.WithTenantContext(ctx, tc)
	events, err := svc.List(ctxTC, f)
	if err != nil {
		t.Fatalf("svc.List: %v", err)
	}
	return events
}

// TestAuditIntegration_TenantContextIsolation_OtherTenantAndNullInvisible はシナリオ (a) 対応。
// tenant A 文脈で Service.Record した A 行のみが tenant A 文脈の Service.List で返り、
// SuperAdmin が seed した tenant B 行・NULL テナント行が **不可視** であることを確認する。
// RLS audit_logs_select（自テナント分 or SuperAdmin）がデータ層で Req 4.3 を担保している
// （TenantAdmin が他テナント監査ログを閲覧できず存在自体も露出しない / Req 2.9 / 4.3 / NFR 2.1）。
func TestAuditIntegration_TenantContextIsolation_OtherTenantAndNullInvisible(t *testing.T) {
	// Arrange
	urls := requireDBURLs(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	applyMigrationsUp(t, urls.migrate)
	truncateAll(t, ctx, urls)
	pool := newAppPool(t, ctx, urls)
	defer pool.Close()
	ids := seedDummyData(t, ctx, pool)

	repo := audit.NewRepository(pool)
	svc := audit.NewService(config.Config{AuditLogRetentionDays: auditTestRetentionDays}, repo, auditFixedClock{now: auditFixedNow})

	tcA := platformdb.TenantContext{TenantID: ids.tenantAID, IsSuperAdmin: false}
	tcB := platformdb.TenantContext{TenantID: ids.tenantBID, IsSuperAdmin: false}
	tcSA := platformdb.TenantContext{TenantID: uuid.Nil, IsSuperAdmin: true}

	// 本テスト固有の event_type prefix。seedDummyData が事前投入する seed.event.* 行と
	// 区別するため、Filter.EventType で本テストの追記行のみに絞って厳密件数を検証する。
	const evType = "audit.isolation"

	// tenant A 文脈で A 行を追記（実 Service.Record → Repository.Insert / 実 NULL 非対象）。
	recordViaService(t, ctx, svc, tcA, audit.Event{
		TenantID:   ids.tenantAID,
		ActorID:    ids.adminAID,
		EventType:  evType,
		Result:     audit.ResultSuccess,
		OccurredAt: auditFixedNow.Add(-1 * time.Hour),
	})
	// tenant B 文脈で B 行を追記（同 event_type だが tenant B 帰属）。
	recordViaService(t, ctx, svc, tcB, audit.Event{
		TenantID:   ids.tenantBID,
		ActorID:    ids.adminBID,
		EventType:  evType,
		Result:     audit.ResultSuccess,
		OccurredAt: auditFixedNow.Add(-2 * time.Hour),
	})
	// SuperAdmin 文脈で NULL テナント行を追記（実 NULL bind の検証 / Req 1.2）。
	recordViaService(t, ctx, svc, tcSA, audit.Event{
		TenantID:   uuid.Nil,
		ActorID:    ids.adminAID,
		EventType:  evType,
		Result:     audit.ResultSuccess,
		OccurredAt: auditFixedNow.Add(-3 * time.Hour),
	})

	// Act: tenant A 文脈で本 event_type のみを List
	got := listViaService(t, ctx, svc, tcA, audit.Filter{EventType: evType})

	// Assert: A 行のみが返り、B 行・NULL 行は不可視
	if len(got) != 1 {
		t.Fatalf("tenant A 文脈での List 件数 = %d; want 1（A 行のみ。B / NULL は RLS で不可視）", len(got))
	}
	if got[0].TenantID != ids.tenantAID {
		t.Errorf("返却行の TenantID = %v; want %v（tenant A 行）", got[0].TenantID, ids.tenantAID)
	}
}

// TestAuditIntegration_SuperAdminSeesAllDesc はシナリオ (b) 対応。
// SuperAdmin 文脈の Service.List が tenant A / B + NULL 行を occurred_at 降順で全件返す
// （RLS の is_superadmin 句で全テナント + NULL 可視 / Req 3.1 / 3.5）。
func TestAuditIntegration_SuperAdminSeesAllDesc(t *testing.T) {
	// Arrange
	urls := requireDBURLs(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	applyMigrationsUp(t, urls.migrate)
	truncateAll(t, ctx, urls)
	pool := newAppPool(t, ctx, urls)
	defer pool.Close()
	ids := seedDummyData(t, ctx, pool)

	repo := audit.NewRepository(pool)
	svc := audit.NewService(config.Config{AuditLogRetentionDays: auditTestRetentionDays}, repo, auditFixedClock{now: auditFixedNow})

	tcA := platformdb.TenantContext{TenantID: ids.tenantAID, IsSuperAdmin: false}
	tcB := platformdb.TenantContext{TenantID: ids.tenantBID, IsSuperAdmin: false}
	tcSA := platformdb.TenantContext{TenantID: uuid.Nil, IsSuperAdmin: true}

	// 本テスト固有の event_type。seedDummyData の seed.event.* と区別して厳密件数を検証する。
	const evType = "audit.crossview"

	// 降順検証のため occurred_at を意図的にずらして 3 行 seed する。
	// 先頭が最新になるよう ResourceID で個体を識別する（B 行が最新 = -1h）。
	recordViaService(t, ctx, svc, tcA, audit.Event{
		TenantID: ids.tenantAID, ActorID: ids.adminAID, EventType: evType, ResourceID: "row-a",
		Result: audit.ResultSuccess, OccurredAt: auditFixedNow.Add(-3 * time.Hour),
	})
	recordViaService(t, ctx, svc, tcB, audit.Event{
		TenantID: ids.tenantBID, ActorID: ids.adminBID, EventType: evType, ResourceID: "row-b",
		Result: audit.ResultSuccess, OccurredAt: auditFixedNow.Add(-1 * time.Hour),
	})
	recordViaService(t, ctx, svc, tcSA, audit.Event{
		TenantID: uuid.Nil, ActorID: ids.adminAID, EventType: evType, ResourceID: "row-system",
		Result: audit.ResultSuccess, OccurredAt: auditFixedNow.Add(-2 * time.Hour),
	})

	// Act: SuperAdmin 文脈で本 event_type のみを List（全テナント + NULL が可視）
	got := listViaService(t, ctx, svc, tcSA, audit.Filter{EventType: evType})

	// Assert: 3 件すべて（A + B + NULL）が occurred_at 降順で返る
	if len(got) != 3 {
		t.Fatalf("SuperAdmin 文脈での List 件数 = %d; want 3（A + B + NULL）", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].OccurredAt.Before(got[i].OccurredAt) {
			t.Errorf("occurred_at 降順違反: got[%d]=%v は got[%d]=%v より前", i-1, got[i-1].OccurredAt, i, got[i].OccurredAt)
		}
	}
	// 先頭は最も新しい B 行（-1h）であること。
	if got[0].ResourceID != "row-b" {
		t.Errorf("先頭行 ResourceID = %q; want %q（最新の B 行）", got[0].ResourceID, "row-b")
	}
}

// TestAuditIntegration_SuperAdminFilterByTenant はシナリオ (c) 対応。
// SuperAdmin 文脈 + Filter.TenantID=A 指定時、当該テナント A の行のみが返る（Req 3.2）。
func TestAuditIntegration_SuperAdminFilterByTenant(t *testing.T) {
	// Arrange
	urls := requireDBURLs(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	applyMigrationsUp(t, urls.migrate)
	truncateAll(t, ctx, urls)
	pool := newAppPool(t, ctx, urls)
	defer pool.Close()
	ids := seedDummyData(t, ctx, pool)

	repo := audit.NewRepository(pool)
	svc := audit.NewService(config.Config{AuditLogRetentionDays: auditTestRetentionDays}, repo, auditFixedClock{now: auditFixedNow})

	tcA := platformdb.TenantContext{TenantID: ids.tenantAID, IsSuperAdmin: false}
	tcB := platformdb.TenantContext{TenantID: ids.tenantBID, IsSuperAdmin: false}
	tcSA := platformdb.TenantContext{TenantID: uuid.Nil, IsSuperAdmin: true}

	// 本テスト固有の event_type。seedDummyData の seed.event.* と区別する。
	const evType = "audit.filterbytenant"

	recordViaService(t, ctx, svc, tcA, audit.Event{
		TenantID: ids.tenantAID, ActorID: ids.adminAID, EventType: evType,
		Result: audit.ResultSuccess, OccurredAt: auditFixedNow.Add(-1 * time.Hour),
	})
	recordViaService(t, ctx, svc, tcB, audit.Event{
		TenantID: ids.tenantBID, ActorID: ids.adminBID, EventType: evType,
		Result: audit.ResultSuccess, OccurredAt: auditFixedNow.Add(-1 * time.Hour),
	})

	// Act: SuperAdmin 文脈 + tenant A 絞り込み（本 event_type のみ）
	tenantA := ids.tenantAID
	got := listViaService(t, ctx, svc, tcSA, audit.Filter{TenantID: &tenantA, EventType: evType})

	// Assert: A 行のみ
	if len(got) != 1 {
		t.Fatalf("Filter.TenantID=A での List 件数 = %d; want 1（A 行のみ）", len(got))
	}
	if got[0].TenantID != ids.tenantAID {
		t.Errorf("返却行の TenantID = %v; want %v", got[0].TenantID, ids.tenantAID)
	}
}

// TestAuditIntegration_AppendOnly_UpdateDeleteRejected はシナリオ (d) 対応。
// tenant / SuperAdmin いずれの文脈でも、Service が seed した audit_logs 行への直接
// UPDATE / DELETE が拒否される（RLS policy 不在 + app_user REVOKE の二重防御 /
// Req 6.1 / 6.2 / 6.3）。permission denied（SQLSTATE 42501）を期待する。
func TestAuditIntegration_AppendOnly_UpdateDeleteRejected(t *testing.T) {
	// Arrange
	urls := requireDBURLs(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	applyMigrationsUp(t, urls.migrate)
	truncateAll(t, ctx, urls)
	pool := newAppPool(t, ctx, urls)
	defer pool.Close()
	ids := seedDummyData(t, ctx, pool)

	repo := audit.NewRepository(pool)
	svc := audit.NewService(config.Config{AuditLogRetentionDays: auditTestRetentionDays}, repo, auditFixedClock{now: auditFixedNow})

	tcA := platformdb.TenantContext{TenantID: ids.tenantAID, IsSuperAdmin: false}
	tcSA := platformdb.TenantContext{TenantID: uuid.Nil, IsSuperAdmin: true}

	// tenant A 文脈で A 行を Service.Record（以降この行を改竄しようとする）。
	rowID := uuid.New()
	recordViaService(t, ctx, svc, tcA, audit.Event{
		ID: rowID, TenantID: ids.tenantAID, ActorID: ids.adminAID,
		EventType: "audit.target", Result: audit.ResultSuccess,
		OccurredAt: auditFixedNow.Add(-1 * time.Hour),
	})

	t.Run("tenant 文脈の UPDATE は permission denied", func(t *testing.T) {
		ctxA := platformdb.WithTenantContext(ctx, tcA)
		err := platformdb.BeginTxFunc(ctxA, pool, func(tx pgx.Tx) error {
			_, execErr := tx.Exec(ctx,
				`UPDATE audit_logs SET event_type = 'tampered' WHERE id = $1`, rowID)
			return execErr
		})
		if err == nil {
			t.Fatalf("tenant 文脈の UPDATE が成功した（permission denied を期待）")
		}
		assertPgError(t, err, sqlstatePermissionDenied, "permission denied")
	})

	t.Run("tenant 文脈の DELETE は permission denied", func(t *testing.T) {
		ctxA := platformdb.WithTenantContext(ctx, tcA)
		err := platformdb.BeginTxFunc(ctxA, pool, func(tx pgx.Tx) error {
			_, execErr := tx.Exec(ctx, `DELETE FROM audit_logs WHERE id = $1`, rowID)
			return execErr
		})
		if err == nil {
			t.Fatalf("tenant 文脈の DELETE が成功した（permission denied を期待）")
		}
		assertPgError(t, err, sqlstatePermissionDenied, "permission denied")
	})

	t.Run("SuperAdmin 文脈の UPDATE は permission denied", func(t *testing.T) {
		ctxSA := platformdb.WithTenantContext(ctx, tcSA)
		err := platformdb.BeginTxFunc(ctxSA, pool, func(tx pgx.Tx) error {
			_, execErr := tx.Exec(ctx,
				`UPDATE audit_logs SET event_type = 'tampered' WHERE id = $1`, rowID)
			return execErr
		})
		if err == nil {
			t.Fatalf("SuperAdmin 文脈の UPDATE が成功した（permission denied を期待）")
		}
		assertPgError(t, err, sqlstatePermissionDenied, "permission denied")
	})

	t.Run("SuperAdmin 文脈の DELETE は permission denied", func(t *testing.T) {
		ctxSA := platformdb.WithTenantContext(ctx, tcSA)
		err := platformdb.BeginTxFunc(ctxSA, pool, func(tx pgx.Tx) error {
			_, execErr := tx.Exec(ctx, `DELETE FROM audit_logs WHERE id = $1`, rowID)
			return execErr
		})
		if err == nil {
			t.Fatalf("SuperAdmin 文脈の DELETE が成功した（permission denied を期待）")
		}
		assertPgError(t, err, sqlstatePermissionDenied, "permission denied")
	})
}

// TestAuditIntegration_AppendOnly_NullTenantRowImmutable はシナリオ (e) 対応。
// SuperAdmin 文脈で NULL テナント行を Service.Record した後、当該行への UPDATE / DELETE が
// 拒否されることを確認する（NULL テナント行も append-only / Req 6.4）。
func TestAuditIntegration_AppendOnly_NullTenantRowImmutable(t *testing.T) {
	// Arrange
	urls := requireDBURLs(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	applyMigrationsUp(t, urls.migrate)
	truncateAll(t, ctx, urls)
	pool := newAppPool(t, ctx, urls)
	defer pool.Close()
	ids := seedDummyData(t, ctx, pool)

	repo := audit.NewRepository(pool)
	svc := audit.NewService(config.Config{AuditLogRetentionDays: auditTestRetentionDays}, repo, auditFixedClock{now: auditFixedNow})

	tcSA := platformdb.TenantContext{TenantID: uuid.Nil, IsSuperAdmin: true}

	// SuperAdmin 文脈で NULL テナント行を Service.Record（実 NULL bind / Req 1.2）。
	nullRowID := uuid.New()
	recordViaService(t, ctx, svc, tcSA, audit.Event{
		ID: nullRowID, TenantID: uuid.Nil, ActorID: ids.adminAID,
		EventType: "audit.system.target", Result: audit.ResultSuccess,
		OccurredAt: auditFixedNow.Add(-1 * time.Hour),
	})

	// 追記が NULL テナントで成立したことを SuperAdmin 文脈の List で確認する（NULL bind 検証）。
	got := listViaService(t, ctx, svc, tcSA, audit.Filter{})
	foundNull := false
	for _, ev := range got {
		if ev.ID == nullRowID {
			foundNull = true
			if ev.TenantID != uuid.Nil {
				t.Errorf("NULL テナント行の TenantID = %v; want uuid.Nil（NULL bind）", ev.TenantID)
			}
		}
	}
	if !foundNull {
		t.Fatalf("SuperAdmin の List に NULL テナント行が含まれない（Insert / NULL bind 失敗）")
	}

	t.Run("NULL 行への UPDATE は permission denied", func(t *testing.T) {
		ctxSA := platformdb.WithTenantContext(ctx, tcSA)
		err := platformdb.BeginTxFunc(ctxSA, pool, func(tx pgx.Tx) error {
			_, execErr := tx.Exec(ctx,
				`UPDATE audit_logs SET event_type = 'tampered' WHERE id = $1`, nullRowID)
			return execErr
		})
		if err == nil {
			t.Fatalf("NULL 行への UPDATE が成功した（permission denied を期待）")
		}
		assertPgError(t, err, sqlstatePermissionDenied, "permission denied")
	})

	t.Run("NULL 行への DELETE は permission denied", func(t *testing.T) {
		ctxSA := platformdb.WithTenantContext(ctx, tcSA)
		err := platformdb.BeginTxFunc(ctxSA, pool, func(tx pgx.Tx) error {
			_, execErr := tx.Exec(ctx, `DELETE FROM audit_logs WHERE id = $1`, nullRowID)
			return execErr
		})
		if err == nil {
			t.Fatalf("NULL 行への DELETE が成功した（permission denied を期待）")
		}
		assertPgError(t, err, sqlstatePermissionDenied, "permission denied")
	})
}

// TestAuditIntegration_RetentionFloor_ExcludesOlderRows はシナリオ (f) 対応。
// retention 起点（fixedNow - 180 日）より前の occurred_at を持つ行を seed し、
// Service.List が当該行を除外する（保持期間内の行のみ返る / Req 5.2 / 5.3）。
// fixedClock で effectiveFrom が決定的になることを利用する。
func TestAuditIntegration_RetentionFloor_ExcludesOlderRows(t *testing.T) {
	// Arrange
	urls := requireDBURLs(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	applyMigrationsUp(t, urls.migrate)
	truncateAll(t, ctx, urls)
	pool := newAppPool(t, ctx, urls)
	defer pool.Close()
	ids := seedDummyData(t, ctx, pool)

	repo := audit.NewRepository(pool)
	svc := audit.NewService(config.Config{AuditLogRetentionDays: auditTestRetentionDays}, repo, auditFixedClock{now: auditFixedNow})

	tcA := platformdb.TenantContext{TenantID: ids.tenantAID, IsSuperAdmin: false}

	// 本テスト固有の event_type。seedDummyData の seed.event.A（occurred_at=now / 保持期間内）と
	// 区別して、保持期間境界の効果のみを厳密に検証する。
	const evType = "audit.retention"

	// 保持期間内（fixedNow - 30 日）の行。
	insideID := uuid.New()
	recordViaService(t, ctx, svc, tcA, audit.Event{
		ID: insideID, TenantID: ids.tenantAID, ActorID: ids.adminAID,
		EventType: evType, ResourceID: "inside", Result: audit.ResultSuccess,
		OccurredAt: auditFixedNow.AddDate(0, 0, -30),
	})
	// 保持期間外（fixedNow - 200 日 < 起点 180 日前）の行。
	outsideID := uuid.New()
	recordViaService(t, ctx, svc, tcA, audit.Event{
		ID: outsideID, TenantID: ids.tenantAID, ActorID: ids.adminAID,
		EventType: evType, ResourceID: "outside", Result: audit.ResultSuccess,
		OccurredAt: auditFixedNow.AddDate(0, 0, -200),
	})

	// Act: tenant A 文脈で List（from 未指定 → effectiveFrom = retentionFloor = -180 日）。
	// 本 event_type のみに絞り、保持期間境界の効果を厳密に検証する。
	got := listViaService(t, ctx, svc, tcA, audit.Filter{EventType: evType})

	// Assert: 保持期間内の行のみが返る（保持期間外は除外 / Req 5.2 / 5.3）
	if len(got) != 1 {
		t.Fatalf("retention 起点での List 件数 = %d; want 1（保持期間内のみ）", len(got))
	}
	if got[0].ID != insideID {
		t.Errorf("返却行 ID = %v; want %v（保持期間内の行）", got[0].ID, insideID)
	}
	for _, ev := range got {
		if ev.ID == outsideID {
			t.Errorf("保持期間外の行が結果に含まれている（除外されるべき / Req 5.2）")
		}
	}
}

// TestAuditIntegration_NormalTenant_CrossTenantInsertRejectedAndInTenantRetained は (g) 対応。
// 通常テナント文脈（tenant A）で他テナント B の tenant_id を持つ行の Service.Record が
// audit_logs_insert の WITH CHECK で拒否され（NFR 2.2 / 機密混入拒否）、同シナリオで保持期間内
// レコードが欠損なく取得できる（NFR 1.2）ことを確認する。
func TestAuditIntegration_NormalTenant_CrossTenantInsertRejectedAndInTenantRetained(t *testing.T) {
	// Arrange
	urls := requireDBURLs(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	applyMigrationsUp(t, urls.migrate)
	truncateAll(t, ctx, urls)
	pool := newAppPool(t, ctx, urls)
	defer pool.Close()
	ids := seedDummyData(t, ctx, pool)

	repo := audit.NewRepository(pool)
	svc := audit.NewService(config.Config{AuditLogRetentionDays: auditTestRetentionDays}, repo, auditFixedClock{now: auditFixedNow})

	tcA := platformdb.TenantContext{TenantID: ids.tenantAID, IsSuperAdmin: false}
	ctxA := platformdb.WithTenantContext(ctx, tcA)

	// g-1: tenant A 文脈で tenant_id=B の行を Service.Record → WITH CHECK 拒否（NFR 2.2）。
	crossErr := svc.Record(ctxA, audit.Event{
		TenantID:   ids.tenantBID, // 要求元(A)と不一致のテナント識別子
		ActorID:    ids.adminAID,
		EventType:  "audit.crosstenant.attempt",
		Result:     audit.ResultSuccess,
		OccurredAt: auditFixedNow.Add(-1 * time.Hour),
	})
	if crossErr == nil {
		t.Fatalf("cross-tenant な Service.Record が成功した（WITH CHECK 拒否を期待 / NFR 2.2）")
	}

	// g-2: 同じ tenant A 文脈で保持期間内の自テナント行を複数追記し、欠損なく取得できる（NFR 1.2）。
	// seedDummyData の seed.event.A と区別するため本テスト固有の event_type で絞り込む。
	const evType = "audit.intenant"
	wantCount := 3
	for i := 0; i < wantCount; i++ {
		recordViaService(t, ctx, svc, tcA, audit.Event{
			TenantID:   ids.tenantAID,
			ActorID:    ids.adminAID,
			EventType:  evType,
			Result:     audit.ResultSuccess,
			OccurredAt: auditFixedNow.AddDate(0, 0, -(i + 1)), // すべて保持期間内
		})
	}

	got := listViaService(t, ctx, svc, tcA, audit.Filter{EventType: evType})
	if len(got) != wantCount {
		t.Fatalf("保持期間内の自テナント行の取得件数 = %d; want %d（欠損なし / NFR 1.2）", len(got), wantCount)
	}
	for _, ev := range got {
		if ev.TenantID != ids.tenantAID {
			t.Errorf("取得行に tenant A 以外が混入: TenantID = %v", ev.TenantID)
		}
	}
}

// TestAuditIntegration_EmptyResult_ReturnsEmptySliceNilError はシナリオ (h) 対応。
// 絞り込み条件に一致する行が 1 件も無いとき、Service.List が空 slice + nil error を返す
// （Req 2.8 / 3.4 = 0 件は正常応答）。
func TestAuditIntegration_EmptyResult_ReturnsEmptySliceNilError(t *testing.T) {
	// Arrange
	urls := requireDBURLs(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	applyMigrationsUp(t, urls.migrate)
	truncateAll(t, ctx, urls)
	pool := newAppPool(t, ctx, urls)
	defer pool.Close()
	ids := seedDummyData(t, ctx, pool)

	repo := audit.NewRepository(pool)
	svc := audit.NewService(config.Config{AuditLogRetentionDays: auditTestRetentionDays}, repo, auditFixedClock{now: auditFixedNow})

	tcA := platformdb.TenantContext{TenantID: ids.tenantAID, IsSuperAdmin: false}

	// 一致しない event_type で絞り込み（seed 行はあるが条件に一致しない）。
	recordViaService(t, ctx, svc, tcA, audit.Event{
		TenantID: ids.tenantAID, ActorID: ids.adminAID, EventType: "audit.exists",
		Result: audit.ResultSuccess, OccurredAt: auditFixedNow.Add(-1 * time.Hour),
	})

	ctxA := platformdb.WithTenantContext(ctx, tcA)

	// Act
	got, err := svc.List(ctxA, audit.Filter{EventType: "audit.no.such.event"})

	// Assert: 空 slice + nil error（panic / 例外ではない正常応答）
	if err != nil {
		t.Fatalf("0 件時に error が返った（nil を期待）: %v", err)
	}
	if got == nil {
		t.Errorf("0 件時の結果が nil slice（空 slice を期待 / Req 2.8）")
	}
	if len(got) != 0 {
		t.Errorf("0 件時の結果件数 = %d; want 0", len(got))
	}
}

// TestAuditIntegration_RoutingSmoke_UnauthenticatedReturns401 はシナリオ (i) 対応。
// task 5 の wiring（audit handler の Mount）を踏襲した test server に対し、認証なしで
// `/api/audit-logs` / `/api/admin/audit-logs` へアクセスすると 401 が返ることを確認する
// （Req 4.2 / 4.4 の wiring 起因回帰 / task 5 の `_Requirements_partial:_` を解消）。
//
// 本ケースは「audit handler が Mount された後も 401/403 ガードが先行する」回帰に限定する。
// 固定ガードの SuperAdmin 強制は #37 既存 test がカバーするため、ここでは認証なし時に
// 監査ログ handler 本体へ到達せず 401 で閉じることのみを検証する。本テストは httpserver の
// default deny（auth middleware 未配線でも TenantContextMiddleware が claims 不在で 401）に
// 依拠するため DB を必要としないが、tasks.md L191 の「結合テスト」配置に合わせ本ファイルに
// 置く（http_subrouter_mount_test.go の newIntegrationHTTPServer 作法を踏襲）。
func TestAuditIntegration_RoutingSmoke_UnauthenticatedReturns401(t *testing.T) {
	// Arrange: httpserver.NewServer を組み、task 5 と同じく audit handler を 2 サブルータへ Mount する。
	log, err := logger.NewLogger(config.Config{
		LogLevel: "error", LogFormat: "json", LogOutput: "stderr",
	})
	if err != nil {
		t.Fatalf("logger.NewLogger: %v", err)
	}
	srv, routers, err := httpserver.NewServer(
		config.Config{HTTPListenAddr: ":0"},
		log,
		nil, // pool 不要（401 ガードは TenantContextMiddleware の claims 不在経路で発火）
		nil, // authMWTenant: 未配線（default deny 401 を確認）
		nil, // authMWAdmin: 同上
		nil, // authMount: /api/auth は Mount しない
	)
	if err != nil {
		t.Fatalf("httpserver.NewServer: %v", err)
	}

	// task 5 (cmd/api) と同じ配線で audit handler を Mount する（svc=nil でも 401 で
	// handler 本体に到達しないため routing スモークの目的を満たす）。
	authorizer := authz.New()
	auditHandler := audit.NewHandler(nil, authorizer, log)
	auditAdminHandler := audit.NewAdminHandler(nil, authorizer, log)
	routers.API.Mount("/audit-logs", auditHandler)
	routers.Admin.Mount("/audit-logs", auditAdminHandler)

	ts := httptest.NewServer(srv.Handler)
	defer ts.Close()

	cases := []struct {
		name string
		path string
	}{
		{"tenant-console /api/audit-logs", "/api/audit-logs"},
		{"admin-console /api/admin/audit-logs", "/api/admin/audit-logs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Act
			resp, err := http.Get(ts.URL + tc.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			defer func() { _ = resp.Body.Close() }()
			_, _ = io.ReadAll(resp.Body)

			// Assert: 認証なしは 401（handler 本体へ到達せず先行ガードが閉じる / Req 4.2 / 4.4）
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("GET %s status = %d; want 401（認証なしは先行ガードで閉じる）", tc.path, resp.StatusCode)
			}
		})
	}
}
