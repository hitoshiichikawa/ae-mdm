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
	"golang.org/x/oauth2"

	"github.com/hitoshiichikawa/ae-mdm/internal/audit"
	"github.com/hitoshiichikawa/ae-mdm/internal/auth"
	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/amapi"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/authz"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/httpserver"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/oidc"
	"github.com/hitoshiichikawa/ae-mdm/internal/tenant"
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
//  9. ListenAndServe goroutine + signal.NotifyContext で graceful shutdown
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
	// （Audit Service 未実装のため interim の logger 実装）をオーケストレーションする。Handler を
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
	tenantRecorder := tenant.NewLoggerRecorder(log)
	tenantSvc := tenant.NewService(tenantRepo, amapiClient, tenantRecorder, cfg, log)
	tenantHandler := tenant.NewHandler(tenantSvc, log)
	tenantHandler.Mount(routers.Admin)

	// (9) ListenAndServe + graceful shutdown
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
