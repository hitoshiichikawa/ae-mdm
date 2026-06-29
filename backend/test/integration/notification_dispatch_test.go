package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	"github.com/hitoshiichikawa/ae-mdm/internal/notification"
	platformdb "github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/pubsub"
	"github.com/hitoshiichikawa/ae-mdm/internal/tenant"
)

// 本ファイルは Issue #39 (A6b) task 5.1 の Notification Dispatcher 結合テスト（実 PostgreSQL）。
// subscriber / Pub/Sub emulator を介さず `Dispatcher.Handle` を直接駆動し（design.md
// 「Testing Strategy」Integration Tests / Risk「worker entry への wire-in が scope 外」と整合）、
// 実 dedupe / unassigned repository + 実 tenant 逆引き Service を結線して dispatch 経路を回帰検証する。
// 種別 handler は呼び出し回数を数える mock（実ドメインハンドラは本 Issue scope 外）。
// DATABASE_URL 未設定環境では requireDBURLs が t.Skip するため DB 不在でも false-fail しない
// （helpers_test.go の規約 / tenant_repository_test.go / audit_test.go の作法を踏襲）。
//
// 検証シナリオ（tasks.md task 5.1 / requirements.md Req 6.1〜6.4 / 3.1 と 1:1）:
//   (a) 同一 MessageID を 2 回 Handle → 種別 handler 呼び出しが 1 回のみ（Req 6.1 / 1.2 / 1.3）
//   (b) 未登録 enterprise_name の通知 → unassigned_notifications に INSERT され ack（Req 6.2 / 3.2 / 3.3）
//   (c) ENROLLMENT / STATUS_REPORT / COMMAND が各 mock handler へ振り分け（Req 6.3 / 2.1-2.3）
//   (d) 退避済み通知が admin_handler 経由 / UnassignedQueue.List から取得でき from/to/type 絞り込み（Req 6.4 / 4.1 / 4.2）
//   (e) bound テナントの enterprise_name 解決時に tenant context が確立され handler が呼ばれる（Req 3.1 経路）

// boundEnterpriseName は本テストで bound テナントに割り当てる enterprise_name。
// Verifier は payload JSON の `name`（`enterprises/{id}/...`）から `enterprises/{id}` 接頭辞を
// enterprise_name として抽出するため（task 2 wire-format 前提）、tenant の enterprise_name と
// payload の name 接頭辞を一致させて逆引きを成立させる。
const boundEnterpriseName = "enterprises/LC9001"

// countingHandler は NotificationHandler の mock。種別ごとの呼び出し回数と、handler 到達時に
// 確立されていた tenant context（TenantID）を記録する（実ドメインハンドラは本 Issue scope 外）。
type countingHandler struct {
	hits          int
	tenantCtxSeen bool
	gotTenantID   uuid.UUID
}

// Handle は NotificationHandler を満たす。呼び出し回数を数え、tenant context を観測する。
func (h *countingHandler) Handle(ctx context.Context, _ notification.Envelope) error {
	h.hits++
	if tc, err := platformdb.FromContext(ctx); err == nil {
		h.tenantCtxSeen = true
		h.gotTenantID = tc.TenantID
	}
	return nil
}

// dispatchFixture は結合テストで使う実依存（dedupe / unassigned / tenant Service）と mock handler を
// 束ねた構築物。各テストの Arrange を共通化する。
type dispatchFixture struct {
	ctx        context.Context
	dispatcher *notification.Dispatcher
	unassigned notification.UnassignedQueue
	handlers   map[notification.NotificationType]*countingHandler
	tenantRepo tenant.Repository
	saCtx      context.Context // SuperAdmin TenantContext を確立済みの ctx（tenant fixture 作成用）
	cleanup    func()
}

// setupDispatch は migrate-up → truncate → app pool 構築 → 実 repository / Service / Dispatcher 結線
// までを担う共通 setup（tenant_repository_test.go の setupTenantRepo / audit_test.go の作法を踏襲）。
//
// 種別 handler は ENROLLMENT / STATUS_REPORT / COMMAND の 3 つを countingHandler で登録する。
func setupDispatch(t *testing.T) dispatchFixture {
	t.Helper()
	urls := requireDBURLs(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	applyMigrationsUp(t, urls.migrate)
	truncateAll(t, ctx, urls)
	pool := newAppPool(t, ctx, urls)

	dedupe := notification.NewDedupe(pool)
	unassigned := notification.NewUnassignedQueue(pool)
	tenantRepo := tenant.NewRepository(pool)
	tenantSvc := tenant.NewService(tenantRepo, nil, nil, config.Config{}, nil)
	verifier := notification.NewVerifier(nil)

	handlers := map[notification.NotificationType]*countingHandler{
		notification.Enrollment:   {},
		notification.StatusReport: {},
		notification.Command:      {},
	}
	registry := map[notification.NotificationType]notification.NotificationHandler{
		notification.Enrollment:   handlers[notification.Enrollment],
		notification.StatusReport: handlers[notification.StatusReport],
		notification.Command:      handlers[notification.Command],
	}

	dispatcher := notification.NewDispatcher(verifier, dedupe, unassigned, tenantSvc, registry, nil)

	saCtx := platformdb.WithTenantContext(ctx, platformdb.TenantContext{
		TenantID:     uuid.Nil,
		IsSuperAdmin: true,
	})

	return dispatchFixture{
		ctx:        ctx,
		dispatcher: dispatcher,
		unassigned: unassigned,
		handlers:   handlers,
		tenantRepo: tenantRepo,
		saCtx:      saCtx,
		cleanup: func() {
			pool.Close()
			cancel()
		},
	}
}

// seedBoundTenant は enterprise_name=name の bound テナントを 1 件作成し、その tenant_id を返す
// （tenant_repository_test.go の Insert → UpdateBound パターン。seedDummyData は bound でも
// enterprise_name を設定しないため、逆引き成立には UpdateBound で enterprise_name を保存する）。
func (f dispatchFixture) seedBoundTenant(t *testing.T, name string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := f.tenantRepo.Insert(f.saCtx, tenant.TenantRow{ID: id, Name: "Bound " + name}); err != nil {
		t.Fatalf("Insert(bound tenant): %v", err)
	}
	if _, err := f.tenantRepo.UpdateBound(f.saCtx, id, name); err != nil {
		t.Fatalf("UpdateBound(%q): %v", name, err)
	}
	return id
}

// newMessage は wire-format（attribute notificationType + payload JSON `name`）に従って
// *pubsub.Message を組み立てる（task 2 Verifier の解釈に厳密に合わせる）。
//
// enterpriseName が非空なら payload の `name` を `<enterpriseName>/devices/<msgID>` にして
// Verifier がその接頭辞 `enterprises/{id}` を抽出できるようにする。空なら `name` を持たない
// payload にして enterprise_name 不明（退避経路 / Req 3.4）を再現する。
func newMessage(msgID string, ntype notification.NotificationType, enterpriseName string) *pubsub.Message {
	var payload []byte
	if enterpriseName != "" {
		payload = []byte(fmt.Sprintf(`{"name":%q}`, enterpriseName+"/devices/"+msgID))
	} else {
		// name を持たない有効 JSON（enterprise_name 抽出不能 → Dispatcher が退避判定 / Req 3.4）。
		payload = []byte(`{"foo":"bar"}`)
	}
	return &pubsub.Message{
		ID:          msgID,
		Data:        payload,
		Attributes:  map[string]string{"notificationType": string(ntype)},
		PublishTime: time.Now(),
	}
}

// TestNotificationDispatch_DuplicateMessageID_DispatchedOnce はシナリオ (a) 対応（Req 6.1 / 1.2 / 1.3）。
// 同一 MessageID の通知を 2 回 Handle すると、1 回目は handler へ dispatch + dedupe 記録され、
// 2 回目は既処理判定で dispatch されず即 ack されることを実 DB で検証する。
func TestNotificationDispatch_DuplicateMessageID_DispatchedOnce(t *testing.T) {
	// Arrange: bound テナントを用意し、その enterprise_name を解決させる ENROLLMENT 通知を作る。
	f := setupDispatch(t)
	defer f.cleanup()
	tenantID := f.seedBoundTenant(t, boundEnterpriseName)
	msg := newMessage("msg-dup-001", notification.Enrollment, boundEnterpriseName)

	// Act: 同一 MessageID を 2 回 Handle する。
	if err := f.dispatcher.Handle(f.ctx, msg); err != nil {
		t.Fatalf("Handle(1 回目): %v（ack 完了を期待）", err)
	}
	if err := f.dispatcher.Handle(f.ctx, msg); err != nil {
		t.Fatalf("Handle(2 回目): %v（既処理は即 ack を期待）", err)
	}

	// Assert: ENROLLMENT handler 呼び出しは 1 回のみ（重複排除 / Req 6.1）。
	if got := f.handlers[notification.Enrollment].hits; got != 1 {
		t.Errorf("ENROLLMENT handler 呼び出し回数 = %d; want 1（同一 MessageID は 1 回のみ dispatch / Req 6.1）", got)
	}
	// 1 回目で確立された tenant context が bound テナントであること（Req 3.1 経路の補完）。
	if id := f.handlers[notification.Enrollment].gotTenantID; id != tenantID {
		t.Errorf("handler 到達時の TenantID = %v; want %v（bound テナント解決 / Req 3.1）", id, tenantID)
	}
}

// TestNotificationDispatch_UnresolvableEnterprise_Quarantined はシナリオ (b) 対応（Req 6.2 / 3.2 / 3.3）。
// 未登録（bound テナント不在）の enterprise_name を持つ通知は handler へ dispatch されず、
// unassigned_notifications に退避され ack されることを実 DB で検証する。
func TestNotificationDispatch_UnresolvableEnterprise_Quarantined(t *testing.T) {
	// Arrange: bound テナントを作らず（= 逆引き不能）、未登録 enterprise_name の COMMAND 通知を作る。
	f := setupDispatch(t)
	defer f.cleanup()
	const unknownEnterprise = "enterprises/UNKNOWN-XYZ"
	msg := newMessage("msg-unassigned-001", notification.Command, unknownEnterprise)

	// Act: Handle（退避経路 → ack 完了扱い / Req 3.3）。
	if err := f.dispatcher.Handle(f.ctx, msg); err != nil {
		t.Fatalf("Handle(未割当): %v（退避は ack 完了を期待）", err)
	}

	// Assert: いずれの handler も呼ばれない（dispatch せず退避 / Req 3.2）。
	for ntype, h := range f.handlers {
		if h.hits != 0 {
			t.Errorf("退避経路で handler が呼ばれた: type=%s hits=%d; want 0（dispatch しない / Req 3.2）", ntype, h.hits)
		}
	}
	// 退避レコードが unassigned_notifications に 1 件 INSERT され、List から取得できる（Req 6.2）。
	got, err := f.unassigned.List(f.saCtx, notification.Filter{})
	if err != nil {
		t.Fatalf("UnassignedQueue.List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("退避件数 = %d; want 1（未割当通知が退避される / Req 6.2）", len(got))
	}
	if got[0].MessageID != msg.ID {
		t.Errorf("退避レコードの MessageID = %q; want %q", got[0].MessageID, msg.ID)
	}
	if got[0].EnterpriseName != unknownEnterprise {
		t.Errorf("退避レコードの EnterpriseName = %q; want %q", got[0].EnterpriseName, unknownEnterprise)
	}
	if got[0].NotificationType != notification.Command {
		t.Errorf("退避レコードの NotificationType = %q; want %q", got[0].NotificationType, notification.Command)
	}
}

// TestNotificationDispatch_TypeRouting_DispatchesToCorrectHandler はシナリオ (c)(e) 対応
// （Req 6.3 / 2.1-2.3 / 3.1）。bound テナントに解決される ENROLLMENT / STATUS_REPORT / COMMAND の
// 3 通知を Handle すると、各種別が対応する mock handler へ 1 回ずつ振り分けられ、handler 到達時に
// tenant context（bound テナント）が確立されていることを実 DB で検証する。
func TestNotificationDispatch_TypeRouting_DispatchesToCorrectHandler(t *testing.T) {
	// Arrange: bound テナント 1 件を用意し、3 種別の通知を同一 enterprise_name に解決させる。
	f := setupDispatch(t)
	defer f.cleanup()
	tenantID := f.seedBoundTenant(t, boundEnterpriseName)

	cases := []struct {
		name  string
		msgID string
		ntype notification.NotificationType
	}{
		{"ENROLLMENT", "msg-route-enroll", notification.Enrollment},
		{"STATUS_REPORT", "msg-route-status", notification.StatusReport},
		{"COMMAND", "msg-route-command", notification.Command},
	}

	// Act: 各種別を 1 件ずつ Handle する（MessageID は種別ごとに一意 → dedupe で抑止されない）。
	for _, c := range cases {
		msg := newMessage(c.msgID, c.ntype, boundEnterpriseName)
		if err := f.dispatcher.Handle(f.ctx, msg); err != nil {
			t.Fatalf("Handle(%s): %v（ack 完了を期待）", c.name, err)
		}
	}

	// Assert: 各種別 handler がちょうど 1 回ずつ呼ばれ、tenant context が確立されている。
	for _, c := range cases {
		h := f.handlers[c.ntype]
		if h.hits != 1 {
			t.Errorf("%s handler 呼び出し回数 = %d; want 1（種別振り分け / Req 6.3）", c.name, h.hits)
		}
		if !h.tenantCtxSeen {
			t.Errorf("%s handler 到達時に tenant context が未確立; want 確立済み（Req 3.1）", c.name)
		}
		if h.gotTenantID != tenantID {
			t.Errorf("%s handler 到達時の TenantID = %v; want %v（bound テナント / Req 3.1）", c.name, h.gotTenantID, tenantID)
		}
	}
}

// TestNotificationDispatch_QuarantinedVisibleViaAdminAPI はシナリオ (d) 対応（Req 6.4 / 4.1 / 4.2）。
// 退避済み通知が退避キュー閲覧 API（admin_handler を test server に Mount）から取得でき、
// type 絞り込みが効くことを実 DB で検証する（Req 6.4 は「閲覧 API から取得」を明示）。
func TestNotificationDispatch_QuarantinedVisibleViaAdminAPI(t *testing.T) {
	// Arrange: bound テナントを作らず、種別の異なる 2 件の未割当通知を退避させる。
	f := setupDispatch(t)
	defer f.cleanup()
	enrollMsg := newMessage("msg-admin-enroll", notification.Enrollment, "enterprises/UNKNOWN-A")
	commandMsg := newMessage("msg-admin-command", notification.Command, "enterprises/UNKNOWN-B")
	if err := f.dispatcher.Handle(f.ctx, enrollMsg); err != nil {
		t.Fatalf("Handle(enroll 退避): %v", err)
	}
	if err := f.dispatcher.Handle(f.ctx, commandMsg); err != nil {
		t.Fatalf("Handle(command 退避): %v", err)
	}

	// admin_handler を test server に Mount し、SuperAdmin TenantContext を注入する middleware を被せる
	// （401/403 は RequireAdminConsoleAndSuperAdmin の責務であり、本テストは閲覧経路の検証に集中する。
	// audit_test.go の injectAuthClaimsMW と同方針で、ガードを bypass して handler 本体へ到達させる）。
	adminHandler := notification.NewAdminHandler(f.unassigned, nil)
	root := chi.NewRouter()
	root.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := platformdb.WithTenantContext(r.Context(), platformdb.TenantContext{
				TenantID:     uuid.Nil,
				IsSuperAdmin: true,
			})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	})
	root.Mount("/api/admin/notifications/unassigned", adminHandler)
	ts := httptest.NewServer(root)
	defer ts.Close()

	// Act + Assert (d-1): 絞り込みなしで全件（2 件）が取得できる（Req 6.4 / 4.1）。
	all := getUnassignedViaAPI(t, ts.URL+"/api/admin/notifications/unassigned")
	if len(all) != 2 {
		t.Fatalf("admin API 全件取得 = %d; want 2（退避済み通知の閲覧 / Req 6.4 / 4.1）", len(all))
	}

	// Act + Assert (d-2): type=COMMAND 絞り込みで COMMAND の 1 件のみが返る（Req 6.4 / 4.2）。
	filtered := getUnassignedViaAPI(t, ts.URL+"/api/admin/notifications/unassigned?type=COMMAND")
	if len(filtered) != 1 {
		t.Fatalf("admin API type=COMMAND 絞り込み = %d; want 1（type 絞り込み / Req 6.4 / 4.2）", len(filtered))
	}
	if filtered[0].MessageID != commandMsg.ID {
		t.Errorf("絞り込み結果の MessageID = %q; want %q（COMMAND 退避）", filtered[0].MessageID, commandMsg.ID)
	}
	if filtered[0].NotificationType != string(notification.Command) {
		t.Errorf("絞り込み結果の NotificationType = %q; want %q", filtered[0].NotificationType, notification.Command)
	}

	// Act + Assert (d-3): from/to の時刻範囲絞り込みが効く（未来 from は 0 件 / Req 6.4 / 4.2）。
	future := time.Now().Add(1 * time.Hour).UTC().Format(time.RFC3339)
	none := getUnassignedViaAPI(t, ts.URL+"/api/admin/notifications/unassigned?from="+future)
	if len(none) != 0 {
		t.Errorf("admin API from=未来 絞り込み = %d; want 0（received_at >= from の範囲外 / Req 4.2）", len(none))
	}
}

// getUnassignedViaAPI は退避キュー閲覧 API を GET し、200 + JSON 配列を decode して返す helper。
func getUnassignedViaAPI(t *testing.T, url string) []notification.UnassignedNotificationDTO {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d; want 200（body=%s）", url, resp.StatusCode, string(body))
	}
	var out []notification.UnassignedNotificationDTO
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("退避キュー閲覧 API 応答の JSON decode 失敗: %v（body=%s）", err, string(body))
	}
	return out
}
