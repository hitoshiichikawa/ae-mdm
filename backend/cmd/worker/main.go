// Package main は ae-mdm の worker プロセスのエントリポイント。
//
// worker は Cloud Pub/Sub の pull subscription を介して AMAPI からの通知
// （ENROLLMENT / STATUS_REPORT / COMMAND）を受信・処理する責務を持つが、本 Issue
// （#2: 共通基盤）の時点では Pub/Sub subscriber 本体は実装しない（後続 Issue
// umbrella tasks 6.x に委ねる）。
//
// 本 entrypoint の責務:
//   - config.Load → logger.NewLogger の bootstrap を行い、process global の
//     default logger を確立する（NFR 4.1 / NFR 4.2: api / worker / CLI 共通基盤）。
//     後続 Issue が internal/platform/pubsub を import するだけで完結できる土台を提供。
//   - SIGINT/SIGTERM を受けるまで :8090 で /healthz を返し続け、container を常駐させる
//     （docker-compose / k8s liveness probe の前提）。
//   - `-healthcheck` 引数で起動された場合は localhost の /healthz を 1 回叩いて
//     0 / 1 で exit する（distroless 最終 stage には shell / pgrep / curl が無いため、
//     binary 自身を healthcheck サブコマンドとして使う / #1 由来）。
//
// 初期化失敗時は exit code 1 + 構造化 ERROR ログ（NFR 3.1）。
// なお healthz port (8090) は内部用途のみで、docker-compose.yml では publish しない。
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
)

const (
	listenAddr      = ":8090"
	healthURL       = "http://127.0.0.1:8090/healthz"
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
		fmt.Fprintln(os.Stderr, "ae-mdm worker healthcheck: request failed:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "ae-mdm worker healthcheck: unexpected status:", resp.StatusCode)
		return 1
	}
	return 0
}

// runBootstrap は cmd/worker の起動時 bootstrap。順序:
//
//  1. config.Load() … env 検証 fail-fast（CodeConfigInvalid）
//  2. logger.NewLogger(cfg) + SetDefault … 以降のログを構造化
//  3. healthz HTTP server 起動 + signal 待ち
//
// 本 Issue では Pub/Sub subscriber を実装しないため、(3) は scaffold の常駐 loop に留まる。
// 後続 Issue（umbrella tasks 6.x）で subscriber.Run(ctx, log, ...) を ListenAndServe と
// 並列で起動する想定。
func runBootstrap(ctx context.Context) int {
	// (1) config
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ae-mdm worker: config.Load failed:", err)
		return 1
	}

	// (2) logger
	log, err := logger.NewLogger(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ae-mdm worker: logger.NewLogger failed:", err)
		return 1
	}
	logger.SetDefault(log)
	defer func() { _ = log.Sync() }()

	// (3) healthz + signal
	return runHealthzServer(ctx, log)
}

// runHealthzServer は :8090 で /healthz を返す最小常駐 HTTP server を起動し、
// SIGINT/SIGTERM 受信時に shutdownTimeout（5s）以内で graceful shutdown を行う。
//
// 本 Issue では Pub/Sub subscriber は未配線。後続 Issue で subscriber を
// goroutine で並列起動し、本関数の select に subscriber 完了経路を加える想定。
func runHealthzServer(parent context.Context, log logger.Logger) int {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	sigCtx, stop := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srvErr := make(chan error, 1)
	go func() {
		log.Info("ae-mdm worker: healthz ListenAndServe", "addr", listenAddr)
		if err := srv.ListenAndServe(); err != nil && !stdErrors.Is(err, http.ErrServerClosed) {
			srvErr <- err
		}
		close(srvErr)
	}()

	select {
	case <-sigCtx.Done():
		log.Info("ae-mdm worker: shutdown signal received")
	case err := <-srvErr:
		if err != nil {
			log.Error("ae-mdm worker: healthz server error", logger.Err(err))
			return 1
		}
		return 0
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("ae-mdm worker: graceful shutdown error", logger.Err(err))
		return 1
	}
	return 0
}
