// Package main は ae-mdm の api プロセスのエントリポイント。
//
// 本 Issue（#1: プロジェクト骨格・Docker Compose）の時点では、umbrella spec の
// Technology Stack / File Structure Plan の物理化のみを目的としており、ドメインロジック
// （internal/tenant, internal/auth, internal/policy 等）の実装は後続 Issue で追加される。
//
// 本 entrypoint は以下 2 つの責務だけを満たす:
//   - SIGINT/SIGTERM を受けるまで 8080/tcp で待機し /healthz を返す
//     （requirements.md Requirement 5.3 / 6.4 で要求される healthcheck の前提）
//   - `-healthcheck` 引数で起動された場合は localhost の /healthz を 1 回叩いて
//     0 / 1 で exit する（distroless 最終 stage には shell / pgrep / curl が無いため、
//     binary 自身を healthcheck サブコマンドとして使う）
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
	listenAddr      = ":8080"
	healthURL       = "http://127.0.0.1:8080/healthz"
	healthCheckArg  = "-healthcheck"
	shutdownTimeout = 5 * time.Second
	healthTimeout   = 3 * time.Second
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == healthCheckArg {
		os.Exit(runHealthcheck())
	}
	os.Exit(runServer())
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

// runServer は scaffold 用の最小常駐 server。後続 Issue で chi router + ドメイン
// handler に置き換える前提。
func runServer() int {
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

	fmt.Fprintln(os.Stderr, "ae-mdm api: scaffold entrypoint (Issue #1). listening on", listenAddr)

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
		fmt.Fprintln(os.Stderr, "ae-mdm api: received signal", sig)
	case err := <-srvErr:
		fmt.Fprintln(os.Stderr, "ae-mdm api: server error:", err)
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "ae-mdm api: graceful shutdown error:", err)
		return 1
	}
	return 0
}
