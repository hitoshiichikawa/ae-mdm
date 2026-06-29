package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/notification"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/httpserver"
)

// 本ファイルは、#39 で追加した退避キュー閲覧 route（`/api/admin/notifications/unassigned`）が
// `/api/admin` ガード chain（TenantContextMiddleware → RequireAdminConsoleAndSuperAdmin）を
// 一様に継承することを検証する guard 継承 smoke（Req 4.3 / 4.4）。
//
// tenant_admin_guard_test.go と同方針で、cmd/api と同じく NewServer + routers.Admin.Mount で
// 実 middleware chain を組み立て、AuthClaims を req に直接注入して 401 / 403 / 200 を確認する。
// AdminHandler が依存する UnassignedQueue は DB 不要の fake に差し替えるため、本テストは
// DATABASE_URL 不在でも無条件に走る（NewServer の pool=nil / fake queue が domain を担う）。

// ---- fake UnassignedQueue（integration_test パッケージ用の DB 不要テストダブル） ----

// fakeUnassignedQueue は notification.UnassignedQueue の最小テストダブル。ガード通過後に
// handler 本体（list）へ到達したか（List 呼出有無）を記録し、ガード拒否ケースでは List が一切
// 呼ばれないこと（認可は guard 層で完結）を併せて検証できるようにする。
type fakeUnassignedQueue struct {
	mu        sync.Mutex
	listCalls int
}

func (q *fakeUnassignedQueue) Enqueue(_ context.Context, _ notification.Envelope) error { return nil }

func (q *fakeUnassignedQueue) List(_ context.Context, _ notification.Filter) ([]notification.UnassignedNotification, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.listCalls++
	// 200 経路の確認用に非 nil の空 slice を返す（handler は `[]` + 200 を返す）。
	return []notification.UnassignedNotification{}, nil
}

func (q *fakeUnassignedQueue) calls() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.listCalls
}

// 型 assertion: fakeUnassignedQueue が notification.UnassignedQueue を満たすことを compile-time で確認する。
var _ notification.UnassignedQueue = (*fakeUnassignedQueue)(nil)

// newNotificationAdminGuardServer は NewServer（authMWAdmin=nil で admin chain を
// [TenantContextMiddleware, RequireAdminConsoleAndSuperAdmin] の 2 段だけにする）を構築し、
// notification.AdminHandler を `routers.Admin` へ Mount した HTTP handler と fake queue を返す
// （cmd/api の (9) 配線と同じ routers.Admin.Mount("/notifications/unassigned", h) を再現する）。
func newNotificationAdminGuardServer(t *testing.T) (http.Handler, *fakeUnassignedQueue) {
	t.Helper()
	log, err := logger.NewLogger(config.Config{
		LogLevel: "error", LogFormat: "json", LogOutput: "stderr",
	})
	if err != nil {
		t.Fatalf("logger.NewLogger: %v", err)
	}
	srv, routers, err := httpserver.NewServer(
		config.Config{HTTPListenAddr: ":0"},
		log,
		nil, // pool: 本テストは DB 不要（fake queue が domain を担う）
		nil, // authMWTenant: 未配線（/api/admin は対象外）
		nil, // authMWAdmin: 未配線（admin chain を 2 段ガードのみにし、claims を直接注入する）
		nil, // authMount: /api/auth は Mount しない
	)
	if err != nil {
		t.Fatalf("httpserver.NewServer: %v", err)
	}
	queue := &fakeUnassignedQueue{}
	h := notification.NewAdminHandler(queue, log)
	// /api/admin サブルータ配下に /notifications/unassigned を登録 → 最終 path は
	// /api/admin/notifications/unassigned（cmd/api の (9) 配線と一致）。
	routers.Admin.Mount("/notifications/unassigned", h)
	return srv.Handler, queue
}

// TestNotificationAdminGuard_NoClaims_Returns401 は AuthClaims を注入しない（未認証）状態で
// `/api/admin/notifications/unassigned` を叩いたとき、admin chain の前段 TenantContextMiddleware が
// default deny で 401 を返し、AdminHandler（List）に到達しないことを検証する（Req 4.3）。
func TestNotificationAdminGuard_NoClaims_Returns401(t *testing.T) {
	// Arrange
	handler, queue := newNotificationAdminGuardServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/notifications/unassigned", nil)
	// claims は注入しない（未認証セッション）。

	// Act
	handler.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401（未認証は TenantContextMiddleware が default deny する / Req 4.3）", rec.Code)
	}
	if got := queue.calls(); got != 0 {
		t.Errorf("ガード拒否時に UnassignedQueue.List が呼ばれた（calls=%d; want 0）", got)
	}
}

// TestNotificationAdminGuard_TenantConsoleAudience_Returns403 は tenant-console aud で発行された
// SuperAdmin セッションを提示したとき、RequireAdminConsoleAndSuperAdmin が audience 不一致で 403 を
// 返し、AdminHandler に到達しないことを検証する（Req 4.4 の audience 軸）。
func TestNotificationAdminGuard_TenantConsoleAudience_Returns403(t *testing.T) {
	// Arrange
	handler, queue := newNotificationAdminGuardServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/notifications/unassigned", nil)
	// tenant-console aud + SuperAdmin（audience だけが不一致）。
	req = req.WithContext(httpserver.WithAuthClaims(req.Context(), httpserver.AuthClaims{
		AdminUserID:  uuid.New(),
		IsSuperAdmin: true,
		Console:      "tenant-console",
	}))

	// Act
	handler.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403（tenant-console aud は audience 不一致で拒否 / Req 4.4）", rec.Code)
	}
	if got := queue.calls(); got != 0 {
		t.Errorf("ガード拒否時に UnassignedQueue.List が呼ばれた（calls=%d; want 0）", got)
	}
}

// TestNotificationAdminGuard_NonSuperAdmin_Returns403 は admin-console aud だが SuperAdmin でない
// セッションを提示したとき、RequireAdminConsoleAndSuperAdmin が super_admin_not_present で 403 を
// 返し、AdminHandler に到達しないことを検証する（Req 4.4 の role 軸）。
func TestNotificationAdminGuard_NonSuperAdmin_Returns403(t *testing.T) {
	// Arrange
	handler, queue := newNotificationAdminGuardServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/notifications/unassigned", nil)
	// admin-console aud + 非 SuperAdmin（role だけが不一致）。
	req = req.WithContext(httpserver.WithAuthClaims(req.Context(), httpserver.AuthClaims{
		AdminUserID:  uuid.New(),
		IsSuperAdmin: false,
		Console:      "admin-console",
	}))

	// Act
	handler.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403（非 SuperAdmin は super_admin_not_present で拒否 / Req 4.4）", rec.Code)
	}
	if got := queue.calls(); got != 0 {
		t.Errorf("ガード拒否時に UnassignedQueue.List が呼ばれた（calls=%d; want 0）", got)
	}
}

// TestNotificationAdminGuard_AdminConsoleSuperAdmin_Returns200 は admin-console aud + SuperAdmin の
// 正常 claims で叩いたとき、2 段ガードを通過して AdminHandler.list が fake queue.List を呼び 200 を
// 返すことを検証する（Req 4.3 / 4.4 の正常側ガード通過 = guard が新 route に継承されている証跡）。
func TestNotificationAdminGuard_AdminConsoleSuperAdmin_Returns200(t *testing.T) {
	// Arrange
	handler, queue := newNotificationAdminGuardServer(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/notifications/unassigned", nil)
	req = req.WithContext(httpserver.WithAuthClaims(req.Context(), adminConsoleSuperAdminClaims()))

	// Act
	handler.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200（admin-console SuperAdmin はガードを通過し AdminHandler に到達）", rec.Code)
	}
	if got := queue.calls(); got != 1 {
		t.Errorf("ガード通過後に UnassignedQueue.List が到達していない（calls=%d; want 1）", got)
	}
}
