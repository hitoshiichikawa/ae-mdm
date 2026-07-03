// Package main は ae-mdm の api プロセスのエントリポイント。
//
// 本 entrypoint は以下 2 つの責務を満たす:
//   - 通常起動: config.Load → logger.NewLogger → db.NewPool → httpserver.NewServer の
//     bootstrap を順に行い、ListenAndServe で常駐。SIGINT/SIGTERM で graceful shutdown
//     （5s 上限）。初期化失敗時は exit code 1 + 構造化 ERROR ログ（NFR 3.1 / 3.2）。
//   - `-healthcheck` 引数で起動された場合は localhost の /healthz を 1 回叩いて
//     0 / 1 で exit する（distroless 最終 stage には shell / pgrep / curl が無いため、
//     binary 自身を healthcheck サブコマンドとして使う / #1 由来）。
//
// `/healthz` `/readyz` 自体は internal/platform/httpserver 側が提供する
// （design.md Components: HTTP Server Bootstrap 節 / L706 と整合）。
package main

import (
	"context"
	stdErrors "errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	goidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"

	"github.com/hitoshiichikawa/ae-mdm/internal/app"
	"github.com/hitoshiichikawa/ae-mdm/internal/audit"
	"github.com/hitoshiichikawa/ae-mdm/internal/auth"
	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	"github.com/hitoshiichikawa/ae-mdm/internal/device"
	"github.com/hitoshiichikawa/ae-mdm/internal/enrollment"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/notification"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/amapi"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/authz"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/httpserver"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/oidc"
	"github.com/hitoshiichikawa/ae-mdm/internal/policy"
	"github.com/hitoshiichikawa/ae-mdm/internal/tenant"
	"github.com/hitoshiichikawa/ae-mdm/internal/tenantaudit"
)

const (
	defaultHealthPort = "8080"
	healthCheckArg    = "-healthcheck"
	shutdownTimeout   = 5 * time.Second
	healthTimeout     = 3 * time.Second
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == healthCheckArg {
		os.Exit(runHealthcheck())
	}
	os.Exit(runBootstrap(context.Background()))
}

// runHealthcheck は同一プロセスバイナリを healthcheck CLI として呼んだとき用。
// distroless 最終 stage には curl / wget が存在しないため、binary を再利用する。
//
// 接続先 URL は HTTP_LISTEN_ADDR env から導出する（PR #31 round-1 / round-2 / round-3
// review 由来）。`:9090` のような non-default に切り替えても healthcheck が同じ port を
// 叩けるようにするための運用対応。HTTP_LISTEN_ADDR が未設定 / 解析不能のときは
// 既定 :8080 にフォールバックする。
func runHealthcheck() int {
	client := &http.Client{Timeout: healthTimeout}
	url := healthcheckURL(os.Getenv("HTTP_LISTEN_ADDR"))
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ae-mdm api healthcheck: request failed:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "ae-mdm api healthcheck: unexpected status:", resp.StatusCode)
		return 1
	}
	return 0
}

// healthcheckURL は HTTP_LISTEN_ADDR env から /healthz の loopback URL を組み立てる。
//   - `":8080"` `"0.0.0.0:8080"` のような listen addr → `http://127.0.0.1:8080/healthz`
//   - `"127.0.0.1:9090"` の場合は host をそのまま loopback として再利用
//   - 未設定 / 解析失敗時は :8080 フォールバック
func healthcheckURL(listenAddr string) string {
	port := defaultHealthPort
	if listenAddr != "" {
		if _, p, err := net.SplitHostPort(strings.TrimSpace(listenAddr)); err == nil && p != "" {
			port = p
		}
	}
	return "http://127.0.0.1:" + port + "/healthz"
}

// runBootstrap は cmd/api の起動時 bootstrap。順序:
//
//  1. config.Load() … env 検証 fail-fast（CodeConfigInvalid）
//  2. logger.NewLogger(cfg) + SetDefault … 以降のログを構造化
//  3. db.NewPool(ctx, cfg) … 起動時 Ping 含む（CodeUnavailable）
//  4. oidc.NewVerifier(ctx, cfg) … 両 issuer の discovery / JWKS prefetch（NFR 3.2）
//  5. auth.NewRepository(pool) + auth.NewService(...) + tenant / admin の
//     auth.NewMiddleware(...) を構築（Req 6.2 / 6.3 の物理分離強制）
//  6. httpserver.NewServer(cfg, log, pool, authMWTenant, authMWAdmin, authMount) …
//     2 サブルータ mount + auth エンドポイント mount
//  7. audit domain（Repository / Service / Handler / AdminHandler）の DI 配線 +
//     Routers.API / Routers.Admin への `/audit-logs` Mount（Issue #5 / A5）
//  8. tenant domain（Repository / Service / Handler）の DI 配線 +
//     Routers.Admin への `/tenants` Mount（Issue #38 / A4b）
//  9. notification domain（UnassignedQueue / AdminHandler）の DI 配線 +
//     Routers.Admin への `/notifications/unassigned` Mount（Issue #39 / A6b）
//  10. policy domain（Repository / Service / Handler）の DI 配線 +
//     Routers.API への `/policies` Mount（Issue #40 / B2b）。既存 amapiClient / auditSvc /
//     tenantSvc / authorizer を再利用する（新規構築しない）。policySvc を main レベルで構築し
//     (11) enrollment と共有する（Issue #7）
//  11. enrollment domain（TokenRepository / Service / Handler）の DI 配線 +
//     Routers.API への `/enrollment-tokens` Mount（Issue #7 / B1）。既存 amapiClient / auditSvc /
//     tenantSvc / authorizer / policySvc を再利用する。cmd/worker は変更しない（design リスク 7）
//  12. app domain（Repository / Service / Handler）の DI 配線 +
//     Routers.API への `/play-tokens` / `/apps` / `/apps/sync` Mount（Issue #11 / D1）。既存
//     amapiClient / auditSvc / tenantSvc / authorizer を再利用する（新規構築しない）
//  13. device domain（Repository / Service / Handler / AdminHandler）の DI 配線 +
//     Routers.API への `/devices` / Routers.Admin への `/devices/overview` Mount（Issue #9 / C1）。
//     既存 pool / authorizer / cfg / log を再利用する（新規構築しない）
//  14. ListenAndServe goroutine + signal.NotifyContext で graceful shutdown
//
// いずれかの初期化失敗で exit code 1 + 構造化 ERROR ログを出す（NFR 3.1 / 3.2）。
// pool は defer で Close する（shutdown 順序: HTTP server.Shutdown → pool.Close）。
func runBootstrap(ctx context.Context) int {
	// (1) config
	cfg, err := config.Load()
	if err != nil {
		// logger 未構築段階の失敗のため stderr に直接書く（後段の logger.SetDefault 呼び出し
		// 後の経路と分けて、起動最序段の事故が誤って logger 経路に流れないようにする）。
		fmt.Fprintln(os.Stderr, "ae-mdm api: config.Load failed:", err)
		return 1
	}

	// (2) logger
	log, err := logger.NewLogger(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ae-mdm api: logger.NewLogger failed:", err)
		return 1
	}
	logger.SetDefault(log)
	defer func() { _ = log.Sync() }()

	// (3) db pool
	pool, err := db.NewPool(ctx, cfg)
	if err != nil {
		log.Error("ae-mdm api: db.NewPool failed",
			logger.Err(err),
		)
		return 1
	}
	defer pool.Close()

	// (4) OIDC Verifier（tenant / admin 双方の discovery を起動時に実行 / NFR 3.2 の
	// fail-closed bootstrap）
	verifier, err := oidc.NewVerifier(ctx, cfg)
	if err != nil {
		log.Error("ae-mdm api: oidc.NewVerifier failed",
			logger.Err(err),
		)
		return 1
	}

	// (5) auth domain の DI 配線
	//
	// Repository は pgxpool 経由で SuperAdmin context 下に sessions / state_nonces /
	// admin_users へアクセスする。Service は Verifier / Repository / Clock / TokenGenerator
	// を組み合わせて BeginLogin / HandleCallback / LookupAndRefresh / Logout を提供する。
	// `auth.TokenGenerator(auth.New)` は本番用の opaque session token 生成器（32 byte
	// crypto/rand + base64url no-padding）。テストでは fake fn を差し込んで CSPRNG 失敗経路を
	// 観測する（design.md「Auth Service」 / impl-notes Task 5.1）。
	clock := auth.SystemClock{}
	repo := auth.NewRepository(pool)
	oauth2Configs := buildOAuth2Configs(cfg, verifier)
	svc := auth.NewService(cfg, verifier, repo, oauth2Configs, clock, auth.TokenGenerator(auth.New), log)

	// tenant / admin 系の Auth Middleware を **別インスタンス**で構築する。
	//
	// - tenant 用は `expectedConsole=ConsoleTenant` を closure に固定し、admin cookie が
	//   `/api/...` に提示された場合に `console_mismatch` で 401 + cookie 削除する
	//   （Issue #33 Req 6.2 / 6.3 の cross-console reject 物理分離強制）。
	// - admin 用は Issue #37 (#44 PR iteration round 1) で `expectedConsole=ConsoleAny` に
	//   変更し、auth middleware が console 照合を skip する。後段の
	//   `httpserver.RequireAdminConsoleAndSuperAdmin` ガードが Req 2.4 / 2.5 に従って
	//   tenant-console aud を **403**、非 SuperAdmin role を **403** で拒否する
	//   （auth middleware 側で 401 を先取りすると Req 2.4 の 403 経路に到達できないため）。
	authMWTenant := auth.NewMiddleware(svc, oidc.ConsoleTenant, log, clock)
	authMWAdmin := auth.NewMiddleware(svc, oidc.ConsoleAny, log, clock)

	// auth.Handler.Mount を `authMount` として注入する。`httpserver.NewServer` が
	// `(r, "/api/auth", ConsoleTenant)` と `(r, "/api/admin/auth", ConsoleAdmin)` の
	// 2 度呼びを担当する。
	authHandler := auth.NewHandler(svc, log)
	authMount := func(r chi.Router, consolePrefix string, console oidc.Console) {
		authHandler.Mount(r, consolePrefix, console)
	}

	// (6) http server
	//
	// 戻り値 routers（`Routers.API` = /api 配下 / `Routers.Admin` = /api/admin 配下）を
	// 受け取り、後段 (7) で audit domain・(8) で tenant domain の Handler を Mount する
	// 公開ポイントとして使う。
	srv, routers, err := httpserver.NewServer(cfg, log, pool, authMWTenant, authMWAdmin, authMount)
	if err != nil {
		log.Error("ae-mdm api: httpserver.NewServer failed",
			logger.Err(err),
		)
		return 1
	}

	// (7) audit domain（A5 / Issue #5）の DI 配線 + Mount
	//
	// cfg / pool / log は既存 bootstrap で構築済みのものを再利用する（新規構築しない）。
	// Repository は pgxpool 経由で audit_logs へ append-only INSERT / 保持下限付き SELECT を
	// 行い、Service が記録時の ID/OccurredAt 補完と閲覧時の保持期間下限算出を担う。
	// 閲覧 Handler / AdminHandler は authz.Authorizer の `audit_log read` 許可マトリクスで
	// RBAC / テナント分離を判定する（Req 4.1 / 4.4 / 4.6）。
	//   - tenant-console 経路: `routers.API.Mount("/audit-logs", auditHandler)`
	//     → 実 path `/api/audit-logs`（own-tenant 閲覧 / Req 2.x）
	//   - admin-console 経路: `routers.Admin.Mount("/audit-logs", auditAdminHandler)`
	//     → 実 path `/api/admin/audit-logs`（cross-tenant 閲覧 / 固定ガード
	//       RequireAdminConsoleAndSuperAdmin 配下 / Req 3.x / 4.4）
	auditRepo := audit.NewRepository(pool)
	auditSvc := audit.NewService(cfg, auditRepo, audit.SystemClock{}, log)
	authorizer := authz.New()
	auditHandler := audit.NewHandler(auditSvc, authorizer, log)
	auditAdminHandler := audit.NewAdminHandler(auditSvc, authorizer, log)
	routers.API.Mount("/audit-logs", auditHandler)
	routers.Admin.Mount("/audit-logs", auditAdminHandler)

	// (8) tenant domain（A4b / Issue #38）の DI 配線 + Mount
	//
	// cfg / pool / log は既存 bootstrap で構築済みのものを再利用する。Repository は pgxpool 経由で
	// SuperAdmin context 下に tenants へアクセスし、Service は AMAPI Client（#34）と監査記録ポート
	// （(7) で構築済みの audit Service へ tenantaudit アダプタ経由で配線済み / #54）を
	// オーケストレーションする。これにより tenant の作成 / bind / 無効化イベントが audit_logs へ
	// 永続化され、`/api/admin/audit-logs` で閲覧可能になる（#54 Req 1 / 5.3）。Handler を
	// `routers.Admin`（/api/admin chain + RequireAdminConsoleAndSuperAdmin ガード継承）へ Mount し、
	// `/api/admin/tenants` 配下 5 endpoint を稼働させる（Req 6.1）。
	//
	// AMAPI Client は service account credentials（GOOGLE_APPLICATION_CREDENTIALS / required env）
	// から構築する。構築失敗（資格情報不正等）は他の初期化失敗と同様に exit code 1 で fail-fast する。
	amapiClient, err := amapi.NewClient(ctx, cfg, log, nil)
	if err != nil {
		log.Error("ae-mdm api: amapi.NewClient failed",
			logger.Err(err),
		)
		return 1
	}
	tenantRepo := tenant.NewRepository(pool)
	tenantRecorder := buildTenantRecorder(auditSvc, log)
	tenantSvc := tenant.NewService(tenantRepo, amapiClient, tenantRecorder, cfg, log)
	tenantHandler := tenant.NewHandler(tenantSvc, log)
	tenantHandler.Mount(routers.Admin)

	// (9) notification domain（A6b / Issue #39）の退避キュー閲覧 API の DI 配線 + Mount
	//
	// cfg / pool / log は既存 bootstrap で構築済みのものを再利用する（新規構築しない）。
	// UnassignedQueue は pgxpool 経由で SuperAdmin context 下に cross-tenant infra テーブル
	// `unassigned_notifications` へアクセスし、退避済み通知の List を担う（既存スキーマ 0010 /
	// 0011 を消費）。AdminHandler を `routers.Admin`（/api/admin chain +
	// RequireAdminConsoleAndSuperAdmin ガード継承）へ Mount し、`/api/admin/notifications/unassigned`
	// を稼働させる（admin-console / SuperAdmin 専用の退避キュー閲覧 / Req 4.1 / 4.2）。401（Req 4.3）/
	// 403（Req 4.4）は固定ガード middleware が担い、handler は filter parse（400 / Req 4.5）と
	// 200 / 503 に集中する（audit/tenant の (7)(8) 配線ブロックと同パターン）。
	//
	// Dispatcher 本体（worker 経路）の wire-in は #35 の責務であり本 Issue scope 外。本 api
	// entrypoint では退避キュー閲覧 API のみを配線する（design.md Overview / Non-Goals と整合）。
	unassignedQueue := notification.NewUnassignedQueue(pool)
	notificationAdminHandler := notification.NewAdminHandler(unassignedQueue, log)
	routers.Admin.Mount("/notifications/unassigned", notificationAdminHandler)

	// (10) policy domain（B2b / Issue #40）の DI 配線 + Mount
	//
	// pool / log / routers は既存 bootstrap で構築済みのものを再利用する。Policy ドメインの
	// 共有ラッパ依存（AMAPI 反映の amapiClient / 監査記録の auditSvc / enterprise_name 解決の
	// tenantSvc / RBAC 判定の authorizer）も (7)(8) で構築済みのインスタンスをそのまま再利用し、
	// 新規構築しない（NFR 2.2: AMAPI 反映は共有ラッパ経由 / Req 5.1: 作成イベント監査）。
	//   - Repository は pgxpool 経由で tenant-scoped context のまま policies へアクセスし、RLS に
	//     テナント分離を委ねる（SuperAdmin 昇格しない）。
	//   - Service は upsertClient（amapiClient）/ eventRecorder（auditSvc）/ enterpriseResolver
	//     （tenantSvc）/ logger を組み合わせて upsert / 割当 / 参照 / 削除を提供する。authz は
	//     持たず Handler の責務（design Components）。
	//   - Handler を `routers.API`（/api chain）へ Mount し、`/api/policies` 配下 6 endpoint を
	//     稼働させる。authorizer で own-tenant `policy` RBAC を判定する（Req 4.1）。
	//
	// policySvc は本 (10) で main レベルに構築し、(11) enrollment の policyChecker アダプタと共有する
	// （Issue #7 / B1 / task 3）。DEDICATED 発行時の自テナント Kiosk policy 存在検証に policy.Service.Get を
	// 再利用することで、policy の AMAPI policy id（= DB uuid 文字列）を enrollment の PolicyName へ渡す。
	policySvc := buildPolicyService(pool, amapiClient, auditSvc, tenantSvc, log)
	policyHandler := buildPolicyHandler(policySvc, authorizer, log)
	routers.API.Mount("/policies", policyHandler)

	// (11) enrollment domain（B1 / Issue #7）の DI 配線 + Mount
	//
	// pool / log / routers / 共有ラッパ（amapiClient / auditSvc / tenantSvc / authorizer）は既存
	// bootstrap で構築済みのインスタンスをそのまま再利用し、新規構築しない（NFR 3.1 / Req 5.1）。
	//   - TokenRepository は pgxpool 経由で tenant-scoped context のまま enrollment_tokens へアクセスし、
	//     RLS にテナント分離を委ねる（SuperAdmin 昇格しない / 既存スキーマ 0004 を消費）。
	//   - Service は amapiClient（AMAPI 発行）/ auditSvc（発行監査 / Req 5.1）/ tenantSvc（enterprise_name
	//     解決 / Req 2.3）/ policyChecker アダプタ（(10) の policySvc 共有 / DEDICATED 検証 / Req 1.5）を
	//     組み合わせて IssueToken / ListTokens を提供する。authz は持たず Handler の責務（design Components）。
	//   - Handler を `routers.API`（/api chain）へ Mount し、`/api/enrollment-tokens` の POST 発行 /
	//     GET 一覧を稼働させる。authorizer で own-tenant `enrollment_token` RBAC を判定する（Req 2.1 / 2.2）。
	//
	// cmd/worker は本 Issue では変更しない（ENROLLMENT 通知の worker 配線は #36 の責務 / design リスク 7）。
	enrollmentHandler := buildEnrollmentHandler(pool, amapiClient, auditSvc, authorizer, tenantSvc, policySvc, log)
	routers.API.Mount("/enrollment-tokens", enrollmentHandler)

	// (12) app domain（D1 / Issue #11）の DI 配線 + Mount
	//
	// pool / log / routers は既存 bootstrap で構築済みのものを再利用する。App ドメインの共有ラッパ
	// 依存（webToken 発行の amapiClient / 同期監査の auditSvc / enterprise_name 解決 + bind gate の
	// tenantSvc / RBAC 判定の authorizer）も (7)(8) で構築済みのインスタンスをそのまま再利用し、
	// 新規構築しない（NFR 2.1: AMAPI 反映は共有ラッパ経由）。
	//   - Repository は pgxpool 経由で tenant-scoped context のまま tenant_apps へアクセスし、RLS に
	//     テナント分離を委ねる（SuperAdmin 昇格しない）。
	//   - Service は webTokenClient（amapiClient）/ eventRecorder（auditSvc）/ enterpriseResolver
	//     （tenantSvc）/ logger を組み合わせて webToken 発行 / カタログ参照・同期 / 承認済み read seam を
	//     提供する。authz は持たず Handler の責務（design Components / policy と同方針）。
	//   - Handler は 3 endpoint が別々のトップレベルパスを持つため `Mount(routers.API)` で root 相対に
	//     直接登録する（policy の `Mount("/prefix", h)` ではなく tenant.Handler.Mount パターン）。
	//     `routers.API`（/api chain + TenantContextMiddleware による RLS tenant-scoped）配下で
	//     `/api/play-tokens` / `/api/apps` / `/api/apps/sync` を稼働させ、authorizer で own-tenant
	//     `app` RBAC を判定する（Req 4.1）。
	appHandler := buildAppHandler(pool, amapiClient, auditSvc, authorizer, tenantSvc, log)
	appHandler.Mount(routers.API)

	// (13) device domain（C1 / Issue #9）の DI 配線 + Mount
	//
	// pool / authorizer / cfg / log は既存 bootstrap で構築済みのものを再利用する（新規構築しない）。
	// device ドメインは STATUS_REPORT 反映を除く read 系（一覧 / 詳細 / 横断 overview）を提供する。
	//   - Repository は pgxpool 経由で tenant-scoped context のまま devices へアクセスし、RLS に
	//     テナント分離を委ねる（SuperAdmin 昇格しない）。overview のみ AdminHandler が SuperAdmin
	//     TenantContext を確立して cross-tenant 集計する。
	//   - Service（read）は Repository / Clock（本番 SystemClock）/ 同期遅延閾値
	//     （cfg.DeviceSyncDelayThresholdHours / 既定 24h / Req 4.2）を組み合わせて List / Get / Overview を
	//     提供する。write メソッドを持たない（HTTP 直接書込み不可 / Req 7.3 の型担保）。
	//   - tenant-console Handler を `routers.API`（/api chain）へ `/devices` で Mount し、own-tenant
	//     `device` RBAC を判定する（Req 1.x / 2.x）。
	//   - admin-console AdminHandler を `routers.Admin`（/api/admin chain + RequireAdminConsoleAndSuperAdmin
	//     ガード継承）へ `/devices/overview` で Mount し、cross-tenant `device read` 二重防御 + SuperAdmin
	//     TenantContext で全テナント横断集計する（Req 6.x）。
	//
	// STATUS_REPORT 反映（notification.StatusHandler ← device.StatusApplier 注入）は worker 経路であり、
	// cmd/worker の handlers map 本配線は #36 の責務（本 api entrypoint では配線しない / design.md Risks）。
	deviceHandler := buildDeviceHandler(pool, authorizer, cfg, log)
	deviceAdminHandler := buildDeviceAdminHandler(pool, authorizer, cfg, log)
	routers.API.Mount("/devices", deviceHandler)
	routers.Admin.Mount("/devices/overview", deviceAdminHandler)

	// (14) ListenAndServe + graceful shutdown
	return runHTTPServer(ctx, srv, log)
}

// buildOAuth2Configs は tenant / admin の oauth2.Config を 1 つの map に束ねて構築する。
//
//   - Scopes には goidc.ScopeOpenID（"openid"）/ "email" / "profile" を必ず含める
//     （openid scope 不在だと OIDC IdP は authorization code フローで id_token を発行せず、
//     後段 token.Extra("id_token") が空文字 → upstream_oidc_token で 502 に化けて Req 1.x が
//     成立しない / tasks.md 5.1 詳細項目および impl-notes Task 5.1 と整合）。
//   - Endpoint は verifier.TenantEndpoint() / verifier.AdminEndpoint() を使う。両 helper は
//     内部で AuthStyle = oauth2.AuthStyleInHeader を明示上書き済みで、client_secret_basic 固定を
//     契約として強制する（tasks.md 6.3 詳細項目 + impl-notes Task 2.1 と整合）。
func buildOAuth2Configs(cfg config.Config, verifier oidc.Verifier) map[oidc.Console]*oauth2.Config {
	scopes := []string{goidc.ScopeOpenID, "email", "profile"}
	return map[oidc.Console]*oauth2.Config{
		oidc.ConsoleTenant: {
			ClientID:     cfg.OIDCTenantClientID,
			ClientSecret: cfg.OIDCTenantClientSecret,
			RedirectURL:  cfg.OIDCTenantRedirectURL,
			Scopes:       scopes,
			Endpoint:     verifier.TenantEndpoint(),
		},
		oidc.ConsoleAdmin: {
			ClientID:     cfg.OIDCAdminClientID,
			ClientSecret: cfg.OIDCAdminClientSecret,
			RedirectURL:  cfg.OIDCAdminRedirectURL,
			Scopes:       scopes,
			Endpoint:     verifier.AdminEndpoint(),
		},
	}
}

// buildTenantRecorder は tenant ドメインの監査記録ポート（tenant.EventRecorder）を本番 DI 用に
// 構築する（#54 Req 1.4 / 5.3）。
//
// interim の構造化ログ記録器（`tenant.NewLoggerRecorder`）ではなく、監査ログ Service へ委譲する
// `tenantaudit` アダプタを返すことを **単一の真実源**として固定する。これにより tenant の作成 /
// bind / 無効化イベントが `audit_logs` へ永続化され `/api/admin/audit-logs` で閲覧可能になる。
//
// 本 wiring を独立関数として切り出すのは、main の本番配線が誤って interim recorder へ退行して
// いないことを `cmd/api` の単体テスト（main_test.go）で **型レベルに回帰検知**できるようにするため
// （`buildOAuth2Configs` と同じ testability 方針。アダプタを直接生成する unit test では main wiring の
// 退行を捕捉できないという PR #56 round-5 review 指摘への対応）。
func buildTenantRecorder(auditSvc audit.Service, log logger.Logger) tenant.EventRecorder {
	return tenantaudit.NewRecorder(auditSvc, log)
}

// buildPolicyService は policy ドメインの本番 DI（Repository → Service）を構築する
// （Issue #40 / B2b / NFR 2.2 / Req 5.1）。
//
// 共有ラッパ依存（amapiClient / auditSvc / tenantSvc）はいずれも runBootstrap (7)(8) で構築済みの
// インスタンスを **再利用** する前提で受け取り、本 helper 内で新規構築しない。これにより Policy の
// AMAPI 反映が共有 amapi ラッパ経由（NFR 2.2）/ 作成イベント監査が共有 audit Service 経由（Req 5.1）で
// あることを配線レベルで固定する。
//
// 戻り値の policy.Service は runBootstrap (10) で policy.Handler の構築に用いるほか、(11) の enrollment
// domain が DEDICATED 発行時の自テナント Kiosk policy 存在検証（policyChecker アダプタ）に **共有** する
// （Issue #7 / B1 / task 3）。policy.NewRepository は pool を接続せず保持するだけのため、テストでは nil を
// 渡せる（buildTenantRecorder テストが repo=nil で構築するのと同方針 / live DB 非依存）。
func buildPolicyService(
	pool *pgxpool.Pool,
	amapiClient amapi.Client,
	auditSvc audit.Service,
	tenantSvc tenant.Service,
	log logger.Logger,
) policy.Service {
	policyRepo := policy.NewRepository(pool)
	return policy.NewService(policyRepo, amapiClient, auditSvc, tenantSvc, log)
}

// buildPolicyHandler は共有 policy.Service から policy ドメインの本番 Handler を構築する
// （Issue #40 / B2b / Req 4.1）。
//
// policySvc は runBootstrap (10) で buildPolicyService により構築済みのインスタンスを **再利用** する
// 前提で受け取る（enrollment domain と共有 / Issue #7 / task 3）。policy.Service は authorizer を持たず
// （authz は Handler の責務 / design Components）、authorizer は policy.NewHandler の第 2 引数へ渡す。
//
// 本 wiring を独立関数として切り出すのは、main の本番配線が誤って Policy domain の DI を落とす /
// stub へ巻き戻していないことを `cmd/api` の単体テスト（main_test.go）で **型レベルに回帰検知**できる
// ようにするため（`buildTenantRecorder` / `buildOAuth2Configs` と同じ testability 方針）。
func buildPolicyHandler(
	policySvc policy.Service,
	authorizer *authz.Authorizer,
	log logger.Logger,
) *policy.Handler {
	return policy.NewHandler(policySvc, authorizer, log)
}

// enrollmentPolicyChecker は policy.Service を包んで enrollment domain の policyChecker ポート
// （consumer-defined / primitive 型 `ResolveOwnedPolicy`）を structural typing で満たす cmd/api 層の
// アダプタ（Issue #7 / B1 / task 3 / Req 1.5 / 2.3）。
//
// enrollment domain は policy domain を直接 import しない（cross-domain import 回避 / doc.go 依存方向規約）
// ため、本アダプタが cmd/api 側で両者を橋渡しする。
type enrollmentPolicyChecker struct {
	svc policy.Service
}

// ResolveOwnedPolicy は自テナントに policyID が存在すれば AMAPI policy id を返す（不在 / 越境は NotFound）。
//
// policy.Service.Get で自テナント policy の存在を検証する。不在 / 他テナント越境は policy domain が存在差
// 非露出の ErrPolicyNotFound（404）へ写像済みのため、そのまま伝達する（Req 2.3）。存在する場合は
// policyID.String() を AMAPI policy id として返す。これは policy の AMAPI policy id が DB uuid 文字列と
// 一致する命名不変条件（policy.service.Create の buildAMAPIPolicyName(enterprise, id.String()) /
// design「Existing Architecture Analysis」）に依拠する。
func (c enrollmentPolicyChecker) ResolveOwnedPolicy(ctx context.Context, tenantID, policyID uuid.UUID) (string, error) {
	if _, err := c.svc.Get(ctx, tenantID, policyID); err != nil {
		return "", err
	}
	return policyID.String(), nil
}

// buildEnrollmentHandler は enrollment ドメインの本番 DI（TokenRepository → Service → Handler）を
// 1 箇所に束ねて構築する（Issue #7 / B1 / task 3 / Req 2.1 / 2.2 / 2.3 / 4.1）。
//
// 共有ラッパ依存（amapiClient / auditSvc / tenantSvc / authorizer）および policySvc はいずれも runBootstrap
// (7)(8)(10) で構築済みのインスタンスを **再利用** する前提で受け取り、本 helper 内で新規構築しない。これに
// より enrollment の AMAPI 発行が共有 amapi ラッパ経由 / 発行監査が共有 audit Service 経由（Req 5.1）/
// DEDICATED 検証が共有 policy.Service 経由（Req 1.5）であることを配線レベルで固定する。enrollment.Service は
// authorizer を持たず（authz は Handler の責務 / design Components）、authorizer は enrollment.NewHandler の
// 第 2 引数へ渡す。
//
// 本 wiring を独立関数として切り出すのは、main の本番配線が誤って enrollment domain の DI を落とす /
// stub へ巻き戻していないことを `cmd/api` の単体テスト（main_test.go）で **型レベルに回帰検知**できる
// ようにするため（`buildPolicyHandler` / `buildTenantRecorder` と同じ testability 方針）。pool は
// enrollment.NewTokenRepository が接続せず保持するだけのため、テストでは nil を渡せる。
func buildEnrollmentHandler(
	pool *pgxpool.Pool,
	amapiClient amapi.Client,
	auditSvc audit.Service,
	authorizer *authz.Authorizer,
	tenantSvc tenant.Service,
	policySvc policy.Service,
	log logger.Logger,
) *enrollment.Handler {
	tokenRepo := enrollment.NewTokenRepository(pool)
	enrollmentSvc := enrollment.NewService(
		tokenRepo,
		amapiClient,
		auditSvc,
		tenantSvc,
		enrollmentPolicyChecker{svc: policySvc},
		enrollment.SystemClock{},
		log,
	)
	return enrollment.NewHandler(enrollmentSvc, authorizer, log)
}

// buildAppHandler は app ドメインの本番 DI（Repository → Service → Handler）を 1 箇所に束ねて
// 構築する（Issue #11 / D1 / NFR 2.1 / Req 4.1）。
//
// 共有ラッパ依存（amapiClient / auditSvc / tenantSvc / authorizer）はいずれも runBootstrap (7)(8) で
// 構築済みのインスタンスを **再利用** する前提で受け取り、本 helper 内で新規構築しない。これにより
// App の webToken 発行 / カタログ同期反映が共有 amapi ラッパ経由（NFR 2.1）/ 同期監査が共有 audit
// Service 経由であることを配線レベルで固定する。app.Service は authorizer を持たず（authz は Handler の
// 責務 / design Components）、authorizer は app.NewHandler の第 2 引数へ渡す。
//
// app.NewService の deps 順は policy.NewService 最終形（repo, client, recorder, tenants, log）と同一で、
// client=amapiClient（webTokenClient）/ recorder=auditSvc（eventRecorder）/ tenants=tenantSvc
// （enterpriseResolver）を束ねる（impl-notes Task 2 / 3 の DI 順と整合）。
//
// 本 wiring を独立関数として切り出すのは、main の本番配線が誤って App domain の DI を落とす /
// stub へ巻き戻していないことを `cmd/api` の単体テスト（main_test.go）で **型レベルに回帰検知**できる
// ようにするため（`buildPolicyHandler` / `buildTenantRecorder` と同じ testability 方針）。pool は
// app.NewRepository が接続せず保持するだけのため、テストでは nil を渡せる。
func buildAppHandler(
	pool *pgxpool.Pool,
	amapiClient amapi.Client,
	auditSvc audit.Service,
	authorizer *authz.Authorizer,
	tenantSvc tenant.Service,
	log logger.Logger,
) *app.Handler {
	appRepo := app.NewRepository(pool)
	appSvc := app.NewService(appRepo, amapiClient, auditSvc, tenantSvc, log)
	return app.NewHandler(appSvc, authorizer, log)
}

// buildDeviceService は device ドメインの本番 DI（Repository → read Service）を構築する
// （Issue #9 / C1 / Req 1.x / 2.x / 4.2 / 6.x）。
//
// pool / cfg は runBootstrap で構築済みのインスタンスを **再利用** する前提で受け取り、本 helper 内で
// 新規構築しない。Repository は pgxpool 経由で tenant-scoped context のまま devices へアクセスし RLS に
// テナント分離を委ねる（SuperAdmin 昇格しない）。Service（read）は Clock（本番 SystemClock）と同期遅延
// 閾値（cfg.DeviceSyncDelayThresholdHours / 既定 24h / Req 4.2）を注入する。device.Service は write
// メソッドを持たず HTTP 直接書込み不可を型で担保する（Req 7.3）。device.NewRepository は pool を接続せず
// 保持するだけのため、テストでは nil を渡せる（buildPolicyService と同方針 / live DB 非依存）。
//
// 戻り値の device.Service は buildDeviceHandler（tenant-console）と buildDeviceAdminHandler
// （admin-console overview）の双方の構築に用いられる。
func buildDeviceService(pool *pgxpool.Pool, cfg config.Config) device.Service {
	deviceRepo := device.NewRepository(pool)
	return device.NewService(deviceRepo, device.SystemClock{}, cfg.DeviceSyncDelayThresholdHours)
}

// buildDeviceHandler は device ドメインの tenant-console read Handler（GET /api/devices, /{id}）を
// 構築する（Issue #9 / C1 / Req 1.x / 2.x）。
//
// pool / authorizer / cfg / log は runBootstrap で構築済みのインスタンスを **再利用** する前提で受け取り、
// 本 helper 内で新規構築しない。Service は buildDeviceService で組み立て、authorizer は own-tenant
// `device` RBAC 判定のため device.NewHandler の第 2 引数へ渡す。
//
// 本 wiring を独立関数として切り出すのは、main の本番配線が誤って device domain の DI を落とす /
// stub へ巻き戻していないことを `cmd/api` の単体テスト（main_test.go）で **型レベルに回帰検知** できる
// ようにするため（buildPolicyHandler / buildAppHandler と同じ testability 方針）。
func buildDeviceHandler(
	pool *pgxpool.Pool,
	authorizer *authz.Authorizer,
	cfg config.Config,
	log logger.Logger,
) *device.Handler {
	return device.NewHandler(buildDeviceService(pool, cfg), authorizer, log)
}

// buildDeviceAdminHandler は device ドメインの admin-console 横断 overview Handler
// （GET /api/admin/devices/overview）を構築する（Issue #9 / C1 / Req 6.x）。
//
// pool / authorizer / cfg / log は runBootstrap で構築済みのインスタンスを **再利用** する前提で受け取り、
// 本 helper 内で新規構築しない。Service は buildDeviceHandler と同じ buildDeviceService で組み立て、
// authorizer は cross-tenant `device read` の二重防御判定のため device.NewAdminHandler の第 2 引数へ渡す
// （SuperAdmin TenantContext 確立 + probe-tenant authz / Req 6.2）。
//
// 本 wiring を独立関数として切り出すのは、buildDeviceHandler と同じく本番配線の退行（device admin domain の
// DI 欠落）を main_test.go で型レベルに回帰検知できるようにするため（buildPolicyHandler と同じ testability 方針）。
func buildDeviceAdminHandler(
	pool *pgxpool.Pool,
	authorizer *authz.Authorizer,
	cfg config.Config,
	log logger.Logger,
) *device.AdminHandler {
	return device.NewAdminHandler(buildDeviceService(pool, cfg), authorizer, log)
}

// runHTTPServer は srv.ListenAndServe を goroutine で起動し、SIGINT/SIGTERM 受信時に
// shutdownTimeout（5s）以内で graceful shutdown を行う。
//
// signal 経路と server error 経路を select で多重化し、いずれかの完了で shutdown へ。
// shutdown 失敗時は exit code 1。
func runHTTPServer(parent context.Context, srv *http.Server, log logger.Logger) int {
	sigCtx, stop := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srvErr := make(chan error, 1)
	go func() {
		log.Info("ae-mdm api: ListenAndServe", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !stdErrors.Is(err, http.ErrServerClosed) {
			srvErr <- err
		}
		close(srvErr)
	}()

	select {
	case <-sigCtx.Done():
		log.Info("ae-mdm api: shutdown signal received")
	case err := <-srvErr:
		if err != nil {
			log.Error("ae-mdm api: server error", logger.Err(err))
			return 1
		}
		return 0
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("ae-mdm api: graceful shutdown error", logger.Err(err))
		return 1
	}
	return 0
}
