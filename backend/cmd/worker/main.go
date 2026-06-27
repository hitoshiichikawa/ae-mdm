// Package main は ae-mdm の worker プロセスのエントリポイント。
//
// worker は Cloud Pub/Sub の pull subscription を介して AMAPI からの通知
// （ENROLLMENT / STATUS_REPORT / COMMAND）を受信・処理する。本 Issue #35（tasks 6.1）で
// Pub/Sub subscriber 基盤（internal/platform/pubsub）を配線する。通知種別ごとの dispatch・
// 冪等処理・未割当退避・Envelope パースは別 Issue #36（tasks 6.2）に委ねるため、本 entrypoint は
// 暫定 handler（受信を message_id 付きでログし、transient エラーを返して nack＝再配信保持する）を
// 注入する。実 Dispatcher が無い間に ack すると未処理の AMAPI 通知を恒久的に喪失するため、
// ack せず subscription の retention 内で保持し、#36 で実 Dispatcher に差し替える。
//
// 本 entrypoint の責務:
//   - config.Load → logger.NewLogger の bootstrap を行い、process global の default logger を
//     確立する（NFR 4.1 / NFR 4.2: api / worker / CLI 共通基盤）。
//   - Pub/Sub client を構築し、emulator モードでは topic / subscription / dead-letter topic を
//     冪等 ensure した上で subscriber の受信ループを起動する（requirements 6.1 / 6.2）。
//   - :8090 で /healthz を返し続け、container を常駐させる（requirements 6.3 /
//     docker-compose / k8s liveness probe の前提）。
//   - SIGINT/SIGTERM 受信時に subscriber 受信を停止し、shutdownTimeout（5s）以内で graceful
//     shutdown を行う（requirements 6.4 / NFR 2.x）。
//   - subscriber 起動失敗時は構造化 ERROR ログを出力し非ゼロ終了する（requirements 6.5 / NFR 1.1）。
//   - `-healthcheck` 引数で起動された場合は localhost の /healthz を 1 回叩いて 0 / 1 で exit する
//     （distroless 最終 stage には shell / pgrep / curl が無いため、binary 自身を healthcheck
//     サブコマンドとして使う / #1 由来）。
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
	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/pubsub"
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
//  3. Pub/Sub client 構築 + emulator resource ensure + subscriber 起動 + healthz 常駐
//
// (3) で subscriber 起動に失敗した場合は構造化 ERROR ログを出して非ゼロ終了する
// （requirements 6.5 / NFR 1.1）。
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

	// (3) Pub/Sub client + subscriber + healthz
	return runWorker(ctx, cfg, log)
}

// runWorker は Pub/Sub client を構築し、emulator モードでは resource を ensure した上で
// subscriber を起動し、healthz server と並走させる。
//
// 失敗経路（いずれも構造化 ERROR ログ + 非ゼロ終了 / requirements 6.5 / NFR 1.1）:
//   - Pub/Sub client 構築失敗
//   - emulator resource ensure 失敗
//   - subscriber 構築失敗
//   - subscriber 受信ループの致命的エラー（subscription 不在等 / requirements 2.4）
//   - healthz server の致命的エラー
//
// SIGINT/SIGTERM 受信時は ctx をキャンセルして subscriber 受信を止め、healthz を 5s 以内で
// graceful shutdown する（requirements 6.4 / NFR 2.x）。
func runWorker(parent context.Context, cfg config.Config, log logger.Logger) int {
	sigCtx, stop := signal.NotifyContext(parent, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client, err := pubsub.NewClient(sigCtx, cfg, log, nil)
	if err != nil {
		log.Error("ae-mdm worker: failed to construct Pub/Sub client", logger.Err(err))
		return 1
	}
	defer func() { _ = client.Close() }()

	// emulator モードのみ resource を冪等 ensure（本番は IaC 前提で何もしない）。
	if err := client.EnsureEmulatorResources(sigCtx, cfg); err != nil {
		log.Error("ae-mdm worker: failed to ensure emulator Pub/Sub resources", logger.Err(err))
		return 1
	}

	// MaxOutstandingMessages を運用者設定（PUBSUB_MAX_OUTSTANDING_MESSAGES）から配線する
	// （requirements 4.1）。1 未満なら NewSubscriber が CodeConfigInvalid を返し fail-fast する
	// （requirements 4.3 / 6.5）。
	subscriber, err := pubsub.NewSubscriber(client, cfg, log, &pubsub.SubscriberOptions{
		MaxOutstandingMessages: cfg.PubSubMaxOutstandingMessages,
	})
	if err != nil {
		log.Error("ae-mdm worker: failed to construct Pub/Sub subscriber", logger.Err(err))
		return 1
	}

	return superviseWorker(sigCtx, log, listenAddr, subscriber, pendingDispatchHandler(log))
}

// pendingDispatchHandler は #36 の実 Dispatcher が配線されるまでの暫定 handler を返す。
//
// 実 Dispatcher が無い間にメッセージを ack すると、未処理の AMAPI 通知（ENROLLMENT /
// STATUS_REPORT / COMMAND）を恒久的に喪失する。これを避けるため本 handler は ack せず、
// transient なエラーを返して nack＝再配信保持する（errors.ShouldAck 経由で nack 判定 /
// requirements 3.3）。メッセージは subscription の retention 内で保持され、#36 で実
// Dispatcher に差し替えた時点で処理される。
func pendingDispatchHandler(log logger.Logger) pubsub.MessageHandler {
	return pubsub.HandlerFunc(func(_ context.Context, msg *pubsub.Message) error {
		log.Warn("ae-mdm worker: notification retained for redelivery (dispatcher pending #36)",
			logger.MessageID(msg.ID))
		out := pkgerrors.New(pkgerrors.CodeUnavailable,
			"notification dispatcher not yet wired (#36); message retained for redelivery")
		out.IsTransient = true
		return out
	})
}

// superviseWorker は subscriber 受信ループと healthz server を並走させ、停止条件を監視する。
//
//   - subscriber がエラーで停止 → 構造化 ERROR ログ + 1（fail-fast / requirements 6.5）
//   - healthz server がエラーで停止 → 構造化 ERROR ログ + 1
//   - SIGINT/SIGTERM（sigCtx.Done） → subscriber 停止 + healthz を 5s graceful shutdown + 0
//
// addr は healthz server の listen アドレス（本番は listenAddr=:8090。テストでは ephemeral
// ポートを渡して並行実行時のポート競合を避ける）。
func superviseWorker(sigCtx context.Context, log logger.Logger, addr string, subscriber *pubsub.Subscriber, handler pubsub.MessageHandler) int {
	// healthz server。SIGINT/SIGTERM は sigCtx 側で捕捉するため、ここでは ListenAndServe の
	// エラーのみ監視する。
	srv := &http.Server{
		Addr:              addr,
		Handler:           newHealthzMux(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	srvErr := make(chan error, 1)
	go func() {
		log.Info("ae-mdm worker: healthz ListenAndServe", "addr", listenAddr)
		if err := srv.ListenAndServe(); err != nil && !stdErrors.Is(err, http.ErrServerClosed) {
			srvErr <- err
			return
		}
		srvErr <- nil
	}()

	// subscriber 受信ループ。sigCtx キャンセルで graceful に停止し nil を返す。
	subErr := make(chan error, 1)
	go func() {
		log.Info("ae-mdm worker: starting Pub/Sub subscriber")
		subErr <- subscriber.Run(sigCtx, handler)
	}()

	select {
	case <-sigCtx.Done():
		log.Info("ae-mdm worker: shutdown signal received")
		// 停止シグナル受信。subscriber 受信ループの完了（処理中メッセージの確定）を待ってから
		// healthz を停止する（requirements 6.4 / NFR 2.1 / 2.2）。
		return gracefulShutdown(srv, subErr, log, shutdownTimeout)
	case err := <-subErr:
		if err != nil {
			// subscriber の致命的エラー（subscription 不在等）は fail-fast（requirements 6.5）。
			log.Error("ae-mdm worker: subscriber terminated with error", logger.Err(err))
			shutdownHealthz(srv, log)
			return 1
		}
		// エラーなく subscriber が終了した場合（通常は ctx キャンセル時のみ）。
		log.Info("ae-mdm worker: subscriber stopped")
		return shutdownHealthz(srv, log)
	case err := <-srvErr:
		if err != nil {
			log.Error("ae-mdm worker: healthz server error", logger.Err(err))
			return 1
		}
		return 0
	}
}

// gracefulShutdown は SIGINT/SIGTERM 受信後の停止処理を timeout の単一予算で行う
// （本番では shutdownTimeout=5s を渡す）。
//
// sigCtx は既にキャンセル済みのため、subscriber.Run の Receive は outstanding メッセージの
// callback を drain した後に返る。本関数はその完了（subErr）を待ってから healthz server を
// 停止する（requirements 6.4: 処理中メッセージを規定の猶予時間内で確定させる）。
//
//   - 猶予時間内に subscriber が drain 完了 → healthz を残り時間で停止し 0
//   - drain が猶予時間を超過 → 構造化 ERROR ログ + 1（NFR 2.2）
//   - subscriber が drain 中にエラーを返した → 構造化 ERROR ログ + 1
func gracefulShutdown(srv *http.Server, subErr <-chan error, log logger.Logger, timeout time.Duration) int {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	select {
	case err := <-subErr:
		if err != nil {
			log.Error("ae-mdm worker: subscriber drain error during shutdown", logger.Err(err))
			_ = srv.Shutdown(ctx)
			return 1
		}
		log.Info("ae-mdm worker: subscriber drained")
	case <-ctx.Done():
		// 猶予時間内に処理中メッセージの確定が終わらなかった（NFR 2.2）。
		log.Error("ae-mdm worker: graceful shutdown timed out waiting for subscriber drain")
		return 1
	}

	if err := srv.Shutdown(ctx); err != nil {
		log.Error("ae-mdm worker: graceful shutdown error", logger.Err(err))
		return 1
	}
	return 0
}

// newHealthzMux は :8090 /healthz を返す最小 mux を作る（requirements 6.3）。
func newHealthzMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// shutdownHealthz は healthz server を shutdownTimeout（5s）以内で graceful shutdown する。
// 期限内に完了しなければ構造化 ERROR ログ + 非ゼロ終了（NFR 2.2）。
func shutdownHealthz(srv *http.Server, log logger.Logger) int {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("ae-mdm worker: graceful shutdown error", logger.Err(err))
		return 1
	}
	return 0
}
