package integration_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hitoshiichikawa/ae-mdm/internal/audit"
	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	"github.com/hitoshiichikawa/ae-mdm/internal/enrollment"
	"github.com/hitoshiichikawa/ae-mdm/internal/notification"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/amapi"
	platformdb "github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/pubsub"
	"github.com/hitoshiichikawa/ae-mdm/internal/tenant"
)

// 本ファイルは Issue #7 (B1 エンロール) task 6 のエンロールフロー結合テスト（実 PostgreSQL）。
// subscriber / Pub/Sub emulator を介さず `Dispatcher.Handle` を直接駆動し、ENROLLMENT 種別に
// 本 Issue の実ドメインハンドラ（`notification.NewEnrollmentHandler` + `enrollment.NewRegistrar`）を
// 登録した in-test Dispatcher で「発行 → 模擬 ENROLLMENT 通知 → devices 登録」の主動線を回帰検証する。
// cmd/worker は無変更（design.md リスク 7: 全 handler 揃い次第 #36 が本番 wire-in する）。
// DATABASE_URL 未設定環境では requireDBURLs が t.Skip するため DB 不在でも false-fail しない
// （helpers_test.go の規約 / notification_dispatch_test.go の作法を踏襲）。
//
// 検証シナリオ（tasks.md task 6 / requirements.md と 1:1）:
//   (1) IssueToken で発行（snapshot 確認）→ additionalData に発行元 tenant_id を載せた通知を
//       Handle → devices に該当 amapi_device_name の行が 1 件登録（Req 6.1 / 3.1）
//   (2) additionalData.tenant_id を enterprise 由来と不一致にした通知 → unassigned_notifications に
//       退避・devices 無変化（Req 6.2 / 3.2 / NFR 2.2）
//   (3) 同一 amapi_device_name の通知を 2 回 Handle → devices 件数 1 のまま冪等更新（Req 6.3 / 3.4）
//   (4) 無効（tenant_id 欠落 = 突合不能）通知で当該テナントに端末が作られないこと（Req 4.2 の observable「未登録」）
//
// 手本 notification_dispatch_test.go の setupDispatch は種別 handler を countingHandler mock で登録し
// pool を露出しないため、本ファイルは helpers_test.go の低レベルヘルパ（requireDBURLs / migrate /
// truncate / newAppPool）のみを再利用し、実 Registrar を注入した Dispatcher と pool を露出する専用
// setup を組む。helpers_test.go / notification_dispatch_test.go は書き換えない（新規ファイル内で完結）。

// stubPolicyChecker は enrollment.Service の policyChecker ポート（DEDICATED でのみ呼ばれる）を
// 満たす no-op。本結合テストは FULLY_MANAGED で発行するため ResolveOwnedPolicy は呼ばれない。
type stubPolicyChecker struct{}

// ResolveOwnedPolicy は policyChecker を structural typing で満たす（FULLY_MANAGED では未使用）。
func (stubPolicyChecker) ResolveOwnedPolicy(_ context.Context, _, _ uuid.UUID) (string, error) {
	return "", nil
}

// enrollNotificationPayload は模擬 ENROLLMENT 通知の Device payload。enrollment_handler.go の
// enrollmentPayload の parse 仕様（name / enrollmentTokenData / softwareInfo.androidVersion）に厳密に
// 合わせる。enrollmentTokenData は発行時 additionalData（JSON 文字列）を回送する field（design リスク 1）。
type enrollNotificationPayload struct {
	Name                string                     `json:"name"`
	EnrollmentTokenData string                     `json:"enrollmentTokenData"`
	SoftwareInfo        enrollNotificationSoftware `json:"softwareInfo"`
}

// enrollNotificationSoftware は softwareInfo.androidVersion のみを持つ最小構造（Req 3.5 / NFR 1.1）。
type enrollNotificationSoftware struct {
	AndroidVersion string `json:"androidVersion"`
}

// enrollFixture はエンロールフロー結合テストの共通 Arrange（migrate→truncate→pool→in-test Dispatcher）を束ねる。
type enrollFixture struct {
	ctx        context.Context
	saCtx      context.Context // SuperAdmin TenantContext 確立済み（cross-tenant 検証クエリ用）
	pool       *pgxpool.Pool
	dispatcher *notification.Dispatcher
	tenantSvc  tenant.Service
	unassigned notification.UnassignedQueue
	cleanup    func()
}

// setupEnrollmentFlow は migrate-up → truncate → app pool 構築 → ENROLLMENT 実ハンドラ登録済み
// Dispatcher の結線までを担う共通 setup（notification_dispatch_test.go の setupDispatch と同型だが、
// 種別 handler を実 enrollment ドメインハンドラにし、pool を露出する）。
func setupEnrollmentFlow(t *testing.T) enrollFixture {
	t.Helper()
	urls := requireDBURLs(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	applyMigrationsUp(t, urls.migrate)
	truncateAll(t, ctx, urls)
	pool := newAppPool(t, ctx, urls)

	dedupe := notification.NewDedupe(pool)
	unassigned := notification.NewUnassignedQueue(pool)
	verifier := notification.NewVerifier(nil)
	tenantSvc := tenant.NewService(tenant.NewRepository(pool), nil, nil, config.Config{}, nil)

	// ENROLLMENT 種別に本 Issue の実ドメインハンドラを登録する。registrar は実 devices upsert
	// （enrollment.NewRegistrar(pool)）、退避キューは Dispatcher と同じ実 UnassignedQueue を共有する。
	enrollHandler := notification.NewEnrollmentHandler(enrollment.NewRegistrar(pool), unassigned, nil)
	registry := map[notification.NotificationType]notification.NotificationHandler{
		notification.Enrollment: enrollHandler,
	}
	dispatcher := notification.NewDispatcher(verifier, dedupe, unassigned, tenantSvc, registry, nil)

	saCtx := platformdb.WithTenantContext(ctx, platformdb.TenantContext{
		TenantID:     uuid.Nil,
		IsSuperAdmin: true,
	})

	return enrollFixture{
		ctx:        ctx,
		saCtx:      saCtx,
		pool:       pool,
		dispatcher: dispatcher,
		tenantSvc:  tenantSvc,
		unassigned: unassigned,
		cleanup: func() {
			pool.Close()
			cancel()
		},
	}
}

// seedBoundTenantWithAdmin は enterprise_name=enterpriseName の bound テナントと、その配下の
// admin_user を 1 件ずつ SuperAdmin 文脈で直接 INSERT し、(tenant_id, admin_user_id) を返す。
//
// 手本 notification_dispatch_test.go の seedBoundTenant（Insert→UpdateBound）は #52 で UpdateBound の
// WHERE が status='binding' に変更された結果、pending_bind 行の enterprise_name を bound 化できなくなって
// おり（Issue #39 側の latent な回帰 / impl-notes 確認事項参照）、本テストの enterprise_name 逆引き前提を
// 満たせない。そのため status='bound' + enterprise_name を 1 INSERT で確定する直接 seed を用いる
// （seedDummyData と同じ SuperAdmin GUC 直 INSERT 作法）。admin_user は enrollment_tokens.issued_by /
// audit_logs.actor_id の FK を満たすため発行元テナント配下に用意する。
func seedBoundTenantWithAdmin(t *testing.T, ctx context.Context, pool *pgxpool.Pool, enterpriseName string) (tenantID, adminID uuid.UUID) {
	t.Helper()
	tenantID = uuid.New()
	adminID = uuid.New()

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("BeginTx(seed): %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// SuperAdmin 文脈で RLS をバイパスして seed する（seedDummyData と同方針）。
	if _, err := tx.Exec(ctx, "SELECT set_config('app.is_superadmin', 'true', true)"); err != nil {
		t.Fatalf("set_config app.is_superadmin: %v", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO tenants (id, name, status, enterprise_name) VALUES ($1, $2, 'bound', $3)`,
		tenantID, "Bound "+enterpriseName, enterpriseName); err != nil {
		t.Fatalf("INSERT tenants(bound): %v", err)
	}
	const testOIDCIssuer = "https://idp.test.example.com/realms/ae-mdm-test"
	if _, err := tx.Exec(ctx,
		`INSERT INTO admin_users (id, oidc_subject, oidc_issuer, email, tenant_id) VALUES ($1, $2, $3, $4, $5)`,
		adminID, "sub-enroll-"+adminID.String(), testOIDCIssuer, "enroll-"+adminID.String()+"@example.com", tenantID); err != nil {
		t.Fatalf("INSERT admin_users: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	return tenantID, adminID
}

// buildIssuingService は実 DB（TokenRepository / audit）+ amapi StubClient を結線した enrollment.Service を
// 構築する。tenantSvc を enterpriseResolver に、client を AMAPI 発行の stub に注入する（policyChecker は
// FULLY_MANAGED では未使用）。
func buildIssuingService(pool *pgxpool.Pool, tenantSvc tenant.Service, client *amapi.StubClient) enrollment.Service {
	auditSvc := audit.NewService(config.Config{}, audit.NewRepository(pool), audit.SystemClock{}, nil)
	return enrollment.NewService(
		enrollment.NewTokenRepository(pool),
		client,
		auditSvc,
		tenantSvc,
		stubPolicyChecker{},
		enrollment.SystemClock{},
		nil,
	)
}

// newEnrollmentMessage は wire-format（attribute notificationType=ENROLLMENT + payload JSON）に従って
// 模擬 ENROLLMENT 通知を組み立てる。deviceResourceName は payload の `name`（`enterprises/{ent}/devices/{dev}`）
// で、Verifier がその接頭辞から enterprise_name を抽出し、enrollment_handler が全体を amapi_device_name に使う。
// tokenData は enrollmentTokenData（発行時 additionalData の JSON 文字列）として回送する。
func newEnrollmentMessage(t *testing.T, msgID, deviceResourceName, tokenData, androidVersion string) *pubsub.Message {
	t.Helper()
	payload, err := json.Marshal(enrollNotificationPayload{
		Name:                deviceResourceName,
		EnrollmentTokenData: tokenData,
		SoftwareInfo:        enrollNotificationSoftware{AndroidVersion: androidVersion},
	})
	if err != nil {
		t.Fatalf("marshal enrollment notification payload: %v", err)
	}
	return &pubsub.Message{
		ID:          msgID,
		Data:        payload,
		Attributes:  map[string]string{"notificationType": string(notification.Enrollment)},
		PublishTime: time.Now(),
	}
}

// countRows は SuperAdmin 文脈で count(*) クエリを実行し件数を返す（devices / enrollment_tokens は
// tenant-scoped RLS のため cross-tenant 検証は SuperAdmin context で行う / notification_dispatch_test.go の
// saCtx 検証作法と同型）。
func countRows(t *testing.T, saCtx context.Context, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	err := platformdb.BeginTxFunc(saCtx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(saCtx, sql, args...).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count query failed (%q): %v", sql, err)
	}
	return n
}

// selectDeviceModeCompliance は devices の 1 行から mode / compliance_status を SuperAdmin 文脈で取得する。
// 呼び出し側が事前に件数 1 を確認済みの前提（不在は t.Fatal）。
func selectDeviceModeCompliance(t *testing.T, saCtx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID, amapiDeviceName string) (mode, compliance string) {
	t.Helper()
	err := platformdb.BeginTxFunc(saCtx, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(saCtx,
			`SELECT mode, compliance_status FROM devices WHERE tenant_id = $1 AND amapi_device_name = $2`,
			tenantID, amapiDeviceName).Scan(&mode, &compliance)
	})
	if err != nil {
		t.Fatalf("select device(%q): %v", amapiDeviceName, err)
	}
	return mode, compliance
}

// TestEnrollmentFlow_IssueThenEnroll_RegistersDeviceForIssuingTenant はシナリオ (1) 対応（Req 6.1 / 3.1）。
// enrollment.Service.IssueToken で FULLY_MANAGED トークンを発行し（snapshot 確認 + additionalData 捕捉）、
// 発行元 tenant_id を載せた模擬 ENROLLMENT 通知を Dispatcher.Handle すると、発行元テナントの devices に
// 該当 amapi_device_name の行が 1 件登録されることを実 DB で検証する。
func TestEnrollmentFlow_IssueThenEnroll_RegistersDeviceForIssuingTenant(t *testing.T) {
	// Arrange: bound テナント + admin を seed し、発行元テナントの enterprise_name を解決可能にする。
	f := setupEnrollmentFlow(t)
	defer f.cleanup()
	tenantID, adminID := seedBoundTenantWithAdmin(t, f.ctx, f.pool, boundEnterpriseName)

	// StubClient は発行時 additionalData を捕捉し、秘密値（Value / QRCode）と RFC3339 の ExpirationTime を返す。
	var capturedTokenData string
	stub := &amapi.StubClient{
		OnCreateEnrollmentToken: func(_ context.Context, _ string, req amapi.EnrollmentTokenRequest) (amapi.EnrollmentToken, error) {
			capturedTokenData = req.AdditionalData
			return amapi.EnrollmentToken{
				Name:           boundEnterpriseName + "/enrollmentTokens/tok-1",
				Value:          "secret-token-value",
				QRCode:         "secret-qr-code",
				ExpirationTime: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			}, nil
		},
	}
	svc := buildIssuingService(f.pool, f.tenantSvc, stub)
	issuerCtx := platformdb.WithTenantContext(f.ctx, platformdb.TenantContext{TenantID: tenantID, AdminUserID: adminID})

	// Act 1: FULLY_MANAGED トークンを発行する。
	view, err := svc.IssueToken(issuerCtx, adminID, tenantID, enrollment.IssueRequest{Mode: enrollment.ModeFullyManaged})
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}

	// Assert 1: QR 表示用の秘密値が一度だけ返り（Req 1.1）、enrollment_tokens に snapshot が 1 件残る（Req 6.1）。
	if view.Value == "" {
		t.Errorf("TokenView.Value が空; want 秘密値（QR 表示用データを一度だけ返す / Req 1.1）")
	}
	if got := countRows(t, f.saCtx, f.pool, `SELECT count(*) FROM enrollment_tokens WHERE tenant_id = $1`, tenantID); got != 1 {
		t.Fatalf("enrollment_tokens snapshot 件数 = %d; want 1（発行 snapshot 確認 / Req 6.1）", got)
	}
	if capturedTokenData == "" {
		t.Fatalf("発行時 additionalData を捕捉できなかった; 通知 payload に載せられない")
	}

	// Act 2: 発行元 tenant_id を回送する additionalData を載せた模擬 ENROLLMENT 通知を Handle する。
	deviceName := boundEnterpriseName + "/devices/enrolled-device-1"
	msg := newEnrollmentMessage(t, "msg-enroll-flow-1", deviceName, capturedTokenData, "13")
	if err := f.dispatcher.Handle(f.ctx, msg); err != nil {
		t.Fatalf("Dispatcher.Handle(発行→通知): %v（ack 完了を期待）", err)
	}

	// Assert 2: 発行元テナントの devices に該当 amapi_device_name の行が 1 件登録される（Req 6.1 / 3.1）。
	if got := countRows(t, f.saCtx, f.pool, `SELECT count(*) FROM devices WHERE tenant_id = $1 AND amapi_device_name = $2`, tenantID, deviceName); got != 1 {
		t.Fatalf("devices 登録件数 = %d; want 1（発行元テナントへ端末登録 / Req 6.1 / 3.1）", got)
	}
	mode, compliance := selectDeviceModeCompliance(t, f.saCtx, f.pool, tenantID, deviceName)
	if mode != "fully_managed" {
		t.Errorf("devices.mode = %q; want fully_managed（additionalData.mode 写像 / Req 3.1）", mode)
	}
	if compliance != "unknown" {
		t.Errorf("devices.compliance_status = %q; want unknown（androidVersion 13 は unknown / Req 3.1）", compliance)
	}
}

// TestEnrollmentFlow_TenantMismatchNotification_QuarantinedWithoutDeviceRegistration はシナリオ (2) 対応
// （Req 6.2 / 3.2 / NFR 2.2）。enterprise は bound テナントに解決されるが additionalData.tenant_id を別
// テナント（不一致）にした通知は、unassigned_notifications へ退避され、いずれのテナントの devices も
// 更新されないことを実 DB で検証する。
func TestEnrollmentFlow_TenantMismatchNotification_QuarantinedWithoutDeviceRegistration(t *testing.T) {
	// Arrange: 発行元 bound テナントを seed。additionalData.tenant_id は enterprise 由来と一致しない別 uuid にする。
	f := setupEnrollmentFlow(t)
	defer f.cleanup()
	tenantID, _ := seedBoundTenantWithAdmin(t, f.ctx, f.pool, boundEnterpriseName)

	otherTenantID := uuid.New()
	tokenData, err := enrollment.AdditionalData{TenantID: otherTenantID, IssuedBy: uuid.New(), Mode: enrollment.ModeFullyManaged}.Marshal()
	if err != nil {
		t.Fatalf("AdditionalData.Marshal: %v", err)
	}
	deviceName := boundEnterpriseName + "/devices/mismatch-device"
	msg := newEnrollmentMessage(t, "msg-enroll-mismatch-1", deviceName, tokenData, "13")

	// Act: Handle（enterprise は bound テナントに解決されるが additionalData.tenant_id が不一致 → 退避）。
	if err := f.dispatcher.Handle(f.ctx, msg); err != nil {
		t.Fatalf("Dispatcher.Handle(不一致): %v（退避は ack 完了を期待）", err)
	}

	// Assert: unassigned_notifications に 1 件退避される（Req 6.2 / 3.2）。
	quarantined, err := f.unassigned.List(f.saCtx, notification.Filter{})
	if err != nil {
		t.Fatalf("UnassignedQueue.List: %v", err)
	}
	if len(quarantined) != 1 {
		t.Fatalf("退避件数 = %d; want 1（tenant_id 不一致は未割当退避 / Req 6.2 / 3.2）", len(quarantined))
	}
	if quarantined[0].MessageID != msg.ID {
		t.Errorf("退避レコードの MessageID = %q; want %q", quarantined[0].MessageID, msg.ID)
	}
	// devices は無変化: 発行元テナントも、いずれのテナントも更新しない（NFR 2.2）。
	if got := countRows(t, f.saCtx, f.pool, `SELECT count(*) FROM devices WHERE tenant_id = $1`, tenantID); got != 0 {
		t.Errorf("発行元テナントの devices 件数 = %d; want 0（不一致は端末を登録しない / NFR 2.2 / 6.2）", got)
	}
	if got := countRows(t, f.saCtx, f.pool, `SELECT count(*) FROM devices`); got != 0 {
		t.Errorf("全 devices 件数 = %d; want 0（いずれのテナントも更新しない / NFR 2.2）", got)
	}
}

// TestEnrollmentFlow_DuplicateDeviceNotification_IdempotentSingleDevice はシナリオ (3) 対応（Req 6.3 / 3.4）。
// 同一 amapi_device_name の ENROLLMENT 通知を（MessageID を変えて dedupe fast-path を回避しつつ）2 回
// Handle しても、devices が重複登録されず件数 1 のまま冪等更新されることを実 DB で検証する。
func TestEnrollmentFlow_DuplicateDeviceNotification_IdempotentSingleDevice(t *testing.T) {
	// Arrange: bound テナントと一致する additionalData を持つ、同一 amapi_device_name の通知を 2 件（msgID は別）用意。
	f := setupEnrollmentFlow(t)
	defer f.cleanup()
	tenantID, _ := seedBoundTenantWithAdmin(t, f.ctx, f.pool, boundEnterpriseName)

	tokenData, err := enrollment.AdditionalData{TenantID: tenantID, IssuedBy: uuid.New(), Mode: enrollment.ModeFullyManaged}.Marshal()
	if err != nil {
		t.Fatalf("AdditionalData.Marshal: %v", err)
	}
	deviceName := boundEnterpriseName + "/devices/idempotent-device"
	// MessageID を変えて dedupe fast-path を回避し、同一 amapi_device_name の 2 回登録を実際に走らせる。
	msg1 := newEnrollmentMessage(t, "msg-enroll-idem-1", deviceName, tokenData, "13")
	msg2 := newEnrollmentMessage(t, "msg-enroll-idem-2", deviceName, tokenData, "13")

	// Act: 同一 amapi_device_name の通知を 2 回 Handle する。
	if err := f.dispatcher.Handle(f.ctx, msg1); err != nil {
		t.Fatalf("Dispatcher.Handle(1 回目): %v", err)
	}
	if err := f.dispatcher.Handle(f.ctx, msg2); err != nil {
		t.Fatalf("Dispatcher.Handle(2 回目): %v", err)
	}

	// Assert: devices は重複登録されず件数 1 のまま（ON CONFLICT (tenant_id, amapi_device_name) 冪等 upsert / Req 6.3 / 3.4）。
	if got := countRows(t, f.saCtx, f.pool, `SELECT count(*) FROM devices WHERE tenant_id = $1 AND amapi_device_name = $2`, tenantID, deviceName); got != 1 {
		t.Fatalf("devices 件数 = %d; want 1（同一端末の複数通知で重複登録しない / Req 6.3 / 3.4）", got)
	}
}

// TestEnrollmentFlow_InvalidNotification_NoDeviceCreatedForTenant はシナリオ (4) 対応（Req 4.2 の
// observable「未登録」）。additionalData に tenant_id を持たない（突合不能 = 無効トークン相当）通知は
// テナント特定できず退避され、当該テナントの端末インベントリに端末が作られないことを実 DB で検証する。
func TestEnrollmentFlow_InvalidNotification_NoDeviceCreatedForTenant(t *testing.T) {
	// Arrange: bound テナントを seed。additionalData に tenant_id を持たない（無効 / 突合不能）通知を用意する。
	f := setupEnrollmentFlow(t)
	defer f.cleanup()
	tenantID, _ := seedBoundTenantWithAdmin(t, f.ctx, f.pool, boundEnterpriseName)

	// tenant_id 欠落の additionalData（無効トークン相当。突合できず登録されない / Req 3.3 経路）。
	invalidTokenData := `{"issued_by":"` + uuid.New().String() + `","mode":"fully_managed"}`
	deviceName := boundEnterpriseName + "/devices/invalid-device"
	msg := newEnrollmentMessage(t, "msg-enroll-invalid-1", deviceName, invalidTokenData, "13")

	// Act: Handle（tenant_id 欠落 → 突合不能 → 退避 / 登録なし）。
	if err := f.dispatcher.Handle(f.ctx, msg); err != nil {
		t.Fatalf("Dispatcher.Handle(無効通知): %v（退避は ack 完了を期待）", err)
	}

	// Assert: 当該テナントに端末が作られない（Req 4.2 の observable「未登録」）。
	if got := countRows(t, f.saCtx, f.pool, `SELECT count(*) FROM devices WHERE tenant_id = $1`, tenantID); got != 0 {
		t.Fatalf("発行元テナントの devices 件数 = %d; want 0（無効通知で端末は登録されない / Req 4.2）", got)
	}
	if got := countRows(t, f.saCtx, f.pool, `SELECT count(*) FROM devices WHERE amapi_device_name = $1`, deviceName); got != 0 {
		t.Errorf("当該 amapi_device_name の devices 件数 = %d; want 0（Req 4.2）", got)
	}
}
