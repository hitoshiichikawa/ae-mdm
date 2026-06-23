// Package main は ae-mdm の worker プロセスのエントリポイント。
//
// worker は Cloud Pub/Sub の pull subscription を介して AMAPI からの通知
// （ENROLLMENT / STATUS_REPORT / COMMAND）を受信・処理する責務を持つが、本 Issue では
// scaffold のみで実装は後続 Issue（umbrella tasks 6.x）に委ねる。
//
// 本 entrypoint は以下 2 つの責務だけを満たす:
//   - SIGINT/SIGTERM を受けるまで :8090 で /healthz を返し続け、container を常駐させる
//     （requirements.md Requirement 5.3 で要求される healthcheck の前提）
//   - `-healthcheck` 引数で起動された場合は localhost の /healthz を 1 回叩いて
//     0 / 1 で exit する（distroless 最終 stage には shell / pgrep / curl が無いため、
//     binary 自身を healthcheck サブコマンドとして使う）
//
// なお healthz port (8090) は内部用途のみで、docker-compose.yml では publish しない。
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
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
	os.Exit(runWorker())
}

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

// runWorker は scaffold 用の最小常駐 loop。後続 Issue で Pub/Sub subscriber へ置き換える。
func runWorker() int {
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

	fmt.Fprintln(os.Stderr, "ae-mdm worker: scaffold entrypoint (Issue #1). healthz on", listenAddr)

	srvErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- err
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		fmt.Fprintln(os.Stderr, "ae-mdm worker: received signal", sig)
	case err := <-srvErr:
		fmt.Fprintln(os.Stderr, "ae-mdm worker: healthz server error:", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "ae-mdm worker: graceful shutdown error:", err)
		return 1
	}
	return 0
}
