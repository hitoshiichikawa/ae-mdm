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
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/httpserver"
)

const (
	healthURL       = "http://127.0.0.1:8080/healthz"
	healthCheckArg  = "-healthcheck"
	shutdownTimeout = 5 * time.Second
	healthTimeout   = 3 * time.Second
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == healthCheckArg {
		os.Exit(runHealthcheck())
	}
	os.Exit(runBootstrap(context.Background()))
}

// runHealthcheck は同一プロセスバイナリを healthcheck CLI として呼んだとき用。
// distroless 最終 stage には curl / wget が存在しないため、binary を再利用する。
func runHealthcheck() int {
	client := &http.Client{Timeout: healthTimeout}
	resp, err := client.Get(healthURL)
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

// runBootstrap は cmd/api の起動時 bootstrap。順序:
//
//  1. config.Load() … env 検証 fail-fast（CodeConfigInvalid）
//  2. logger.NewLogger(cfg) + SetDefault … 以降のログを構造化
//  3. db.NewPool(ctx, cfg) … 起動時 Ping 含む（CodeUnavailable）
//  4. httpserver.NewServer(cfg, log, pool) … 2 サブルータ mount
//  5. ListenAndServe goroutine + signal.NotifyContext で graceful shutdown
//
// いずれかの初期化失敗で exit code 1 + 構造化 ERROR ログを出す（NFR 3.1）。
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

	// (4) http server
	srv, _, err := httpserver.NewServer(cfg, log, pool)
	if err != nil {
		log.Error("ae-mdm api: httpserver.NewServer failed",
			logger.Err(err),
		)
		return 1
	}

	// (5) ListenAndServe + graceful shutdown
	return runHTTPServer(ctx, srv, log)
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
