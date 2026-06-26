package httpserver

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	internalerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/oidc"
)

// readyzPingTimeout は `/readyz` の DB ping 上限時間。pool.Ping が長時間ブロックして
// kubelet probe を待たせないための保守的な値（本番 SLA に応じて env で調整可能にする
// 拡張は後続 Issue 範囲）。
const readyzPingTimeout = 2 * time.Second

// readHeaderTimeout は slowloris 攻撃対策のために server に必ず設定する read header 上限。
// 後続 Issue で reverse proxy（ALB 等）配下の運用に合わせて env 化する余地あり。
const readHeaderTimeout = 10 * time.Second

// Routers は後続 Issue のドメインハンドラを mount するための公開ポイント。
//
// `r.API.Mount("/devices", deviceHandler)` のように、各 domain Issue が API / Admin
// サブルータへ Handler を追加する想定（design.md Components: HTTP Server Bootstrap 節
// と整合）。本 Issue 時点では何も Mount しないため、`/api/*` `/api/admin/*` は 404
// （ただし TenantContextMiddleware / RequireSuperAdmin が先に発火するため認証なしで
// 到達した場合は 401 / 403 が優先される）。
type Routers struct {
	API   chi.Router // /api 配下
	Admin chi.Router // /api/admin 配下
}

// NewServer は chi router + middleware chain + 2 サブルータを組み立てて *http.Server と
// Routers を返す。
//
// requirements.md Req 5.1 / 5.3 / 5.4 / 5.6 / 6.2 / 6.3 / design.md Components: HTTP Server
// Bootstrap / Auth Middleware 節と整合する。配線の概要:
//   - root chain: Recoverer → RequestID → AccessLog
//   - `/healthz` `/readyz`: chain は通すが auth は無いため認証なしで応答する（middleware
//     chain の **外側** = TenantContextMiddleware より前に登録 / design.md L706）
//   - `/api/auth` / `/api/admin/auth`: root router 直下に Mount（TenantContextMiddleware の
//     **外側** / 認証未確立段階で到達するため）。`authMount` が nil の場合は Mount しない
//   - `/api`: 上記 root chain + `authMWTenant`（nil でない場合のみ）+ TenantContextMiddleware
//   - `/api/admin`: 上記 root chain + `authMWAdmin`（nil でない場合のみ）+
//     TenantContextMiddleware + RequireSuperAdmin
//
// 追加引数 `authMWTenant` / `authMWAdmin` / `authMount`（task 6.2 / Req 6.2 / 6.3）の
// 役割と nil 許容契約:
//   - `authMWTenant`: `expectedConsole=ConsoleTenant` で構築された tenant 用 auth.Middleware
//     の戻り値。nil の場合 `/api/*` には auth middleware を挟まず、A2 既存挙動の default
//     deny 401（TenantContextMiddleware の claims 不在経路）が維持される
//   - `authMWAdmin`: `expectedConsole=ConsoleAdmin` で構築された admin 用 auth.Middleware の
//     戻り値。nil 時の挙動は `authMWTenant` と同様（既存テスト互換のため）
//   - `authMount`: auth.Handler の Mount 関数。nil の場合 auth エンドポイントは登録されない。
//     非 nil の場合 `(r, "/api/auth", ConsoleTenant)` と `(r, "/api/admin/auth", ConsoleAdmin)`
//     の 2 経路で Mount し、tenant / admin 系の OIDC ログインフローを物理的に分離する
//     （Req 6.2 / 6.3 の cross-console reject を auth middleware 側で強制する経路に揃える）
//
// pool は `/readyz` の DB ping にのみ利用する（nil 許容: nil 時は `/readyz` が
// 503 を返す経路で動作する）。後続 Issue のドメインハンドラが BeginTxFunc 経由で
// pool を使うのは Handler 側の責務（本関数で pool を Handler に注入しない）。
func NewServer(
	cfg config.Config,
	log logger.Logger,
	pool *pgxpool.Pool,
	authMWTenant func(http.Handler) http.Handler,
	authMWAdmin func(http.Handler) http.Handler,
	authMount func(r chi.Router, consolePrefix string, console oidc.Console),
) (*http.Server, Routers, error) {
	r := chi.NewRouter()

	// root chain（auth 不要のヘルスチェック含めて挟む。recover で 500 ログを統一）。
	r.Use(Recoverer(log), RequestID(), AccessLog(log))

	// `/healthz` `/readyz` は TenantContext 不要で常時応答する（design.md L706）。
	r.Get("/healthz", healthzHandler)
	r.Get("/readyz", readyzHandler(pool))

	// `/api/auth` / `/api/admin/auth` は TenantContextMiddleware の **外側** に位置する
	// （ログイン前は claims が未確立であり、auth middleware の前段で OIDC ログインフローを
	// 完結させる必要があるため）。`authMount` が nil の場合は本 Issue より前の A2 既存挙動
	// （/api/auth 系は未配線で 404）を維持する。
	if authMount != nil {
		authMount(r, "/api/auth", oidc.ConsoleTenant)
		authMount(r, "/api/admin/auth", oidc.ConsoleAdmin)
	}

	// `/api` と `/api/admin` をそれぞれ独立 router として構築し、root に Mount する。
	//
	// chi の middleware は「subrouter が route にマッチした場合のみ発火」する仕様で、
	// 内部 handler が未登録の subrouter は middleware を bypass して 404 を返す。
	// 本 Issue では `/api/*` `/api/admin/*` のドメイン handler が空（後続 Issue で
	// Mount される）であるため、auth スタブ default deny の 401 / 403 を発火させる
	// には Catch-All ハンドラ `r.HandleFunc("/*", ...)` を各 subrouter に登録して
	// chi の route matching を成立させる必要がある。
	//
	// Catch-All の中身は「next handler が無い場合は 404」を返す通常のフォールバック
	// 動作とし、middleware chain（TenantContextMiddleware / RequireSuperAdmin）が
	// 先に 401 / 403 を返した場合はその応答が優先される（middleware が WriteHeader 後に
	// next.ServeHTTP を呼ばないため）。後続 Issue で各ドメインの Handler が
	// Mount された場合、より specific な route が先にマッチして Catch-All は通らない。
	//
	// /api/admin は /api の上に Mount するのではなく root に並列 Mount する（chi の
	// route matching は登録順ではなく path-tree なので、より specific な /api/admin が
	// 優先的にマッチする）。
	//
	// `authMWAdmin` / `authMWTenant`（task 6.2 / Req 6.2 / 6.3）は
	// TenantContextMiddleware の **前段**として挿入する。これにより
	//   - tenant 系 session cookie が `/api/admin/...` に提示されても `authMWAdmin` 側で
	//     `console_mismatch` を検出し即 401 + cookie 削除（cross-console reject の物理分離強制）
	//   - admin 系 session cookie が `/api/...` に提示されても `authMWTenant` 側で reject
	// auth middleware が nil の場合は本 chain から除外し、A2 既存挙動の default deny 401
	// （TenantContextMiddleware の claims 不在経路）が維持される（既存 test 互換のため）。
	adminRouter := chi.NewRouter()
	if authMWAdmin != nil {
		adminRouter.Use(authMWAdmin)
	}
	// Issue #37 (A3b): RequireAdminConsoleAndSuperAdmin は admin-console aud かつ
	// SuperAdmin の 2 条件 AND ガード。tenant-console aud で発行された SuperAdmin
	// セッションが `/api/admin/*` に到達した場合、TenantContextMiddleware は通過するが
	// 本 middleware が AuthClaims.Console を見て 403 で拒否する（Req 2.4 / 6.2）。
	// 既存 RequireSuperAdmin（audience 判定なし）は backward compat のため admin_middleware.go
	// に残置するが、本 chain には使用しない。
	adminRouter.Use(TenantContextMiddleware(log), RequireAdminConsoleAndSuperAdmin(log))
	adminRouter.HandleFunc("/*", notFoundHandler)

	apiRouter := chi.NewRouter()
	if authMWTenant != nil {
		apiRouter.Use(authMWTenant)
	}
	apiRouter.Use(TenantContextMiddleware(log))
	apiRouter.HandleFunc("/*", notFoundHandler)

	r.Mount("/api/admin", adminRouter)
	r.Mount("/api", apiRouter)

	srv := &http.Server{
		Addr:              cfg.HTTPListenAddr,
		Handler:           r,
		ReadHeaderTimeout: readHeaderTimeout,
	}

	return srv, Routers{API: apiRouter, Admin: adminRouter}, nil
}

// notFoundHandler は subrouter の catch-all として登録される fallback handler。
//
// middleware chain（TenantContextMiddleware / RequireSuperAdmin）が先に 401 / 403 を
// 返した場合は本 handler に到達しない（next.ServeHTTP が呼ばれないため）。
// middleware を通過しドメイン handler 未 mount の場合のみ本 handler が 404 を返す。
//
// `internalerrors.WriteHTTP` 経由で構造化 JSON body を返すことで、後続 Issue が
// テナント越境隠蔽用途で 404 化する経路と body フォーマットを揃える（design.md
// Error Handling 節「テナント越境隠蔽は後続 Issue で `WriteHTTP` を使い 404 化」と整合）。
func notFoundHandler(w http.ResponseWriter, r *http.Request) {
	internalerrors.WriteHTTP(w, r, internalerrors.New(
		internalerrors.CodeNotFound,
		"not found",
	), nil)
}

// healthzHandler は静的に 200 "ok" を返す liveness probe。
// DB 等の外部依存は確認しない（liveness の責務範囲外 / kubelet の再起動契機にしない）。
func healthzHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// readyzHandler は DB pool に Ping を投げて readiness を判定する。
//   - pool == nil: 503（cmd/api bootstrap 失敗で pool 未配線の防御経路 / NFR 3.2）
//   - pool.Ping ok: 200 "ok"
//   - pool.Ping err: 503 "not ready"
//
// readyzPingTimeout を context deadline として被せ、長時間ブロックを防ぐ。
func readyzHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if pool == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready"))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), readyzPingTimeout)
		defer cancel()
		if err := pool.Ping(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}
}
