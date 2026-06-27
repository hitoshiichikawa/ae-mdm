package main

import (
	"context"
	stdErrors "errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	gpubsub "cloud.google.com/go/pubsub"
	"cloud.google.com/go/pubsub/pstest"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/pubsub"
)

const (
	testProjectID = "ae-mdm-worker-test"
	testTopicID   = "amapi-notifications"
	testSubID     = "amapi-notifications-sub"
	// ephemeralAddr は healthz server をテスト用に空きポートで bind する（:8090 競合回避）。
	ephemeralAddr = "127.0.0.1:0"
)

// newFakeServer は pstest の in-process fake server を返す（docker / 実 emulator 不要）。
func newFakeServer(t *testing.T) *pstest.Server {
	t.Helper()
	srv := pstest.NewServer()
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// dialFake は fake server への新規接続を作る（client ごとに 1 本）。
func dialFake(t *testing.T, srv *pstest.Server) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(srv.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// workerTestConfig は worker テスト用の config（emulator モード扱い）。
func workerTestConfig() config.Config {
	return config.Config{
		PubSubProjectID:    testProjectID,
		PubSubTopic:        testTopicID,
		PubSubSubscription: testSubID,
		PubSubEmulatorHost: "fake", // 接続自体は GRPCConn で行う
	}
}

// seedTopicAndSubscription は fake server 上に topic と pull subscription を作る。
func seedTopicAndSubscription(t *testing.T, srv *pstest.Server, projectID, topicID, subID string) {
	t.Helper()
	ctx := context.Background()
	raw, err := gpubsub.NewClient(ctx, projectID, option.WithGRPCConn(dialFake(t, srv)))
	if err != nil {
		t.Fatalf("seed: NewClient: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	topic, err := raw.CreateTopic(ctx, topicID)
	if err != nil {
		t.Fatalf("seed: CreateTopic: %v", err)
	}
	if _, err := raw.CreateSubscription(ctx, subID, gpubsub.SubscriptionConfig{Topic: topic}); err != nil {
		t.Fatalf("seed: CreateSubscription: %v", err)
	}
}

// newWorkerSubscriber は fake server に専用接続でつないだ Subscriber を返す。
func newWorkerSubscriber(t *testing.T, srv *pstest.Server, cfg config.Config) *pubsub.Subscriber {
	t.Helper()
	client, err := pubsub.NewClient(context.Background(), cfg, nil, &pubsub.Options{GRPCConn: dialFake(t, srv)})
	if err != nil {
		t.Fatalf("pubsub.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	sub, err := pubsub.NewSubscriber(client, cfg, nil, nil)
	if err != nil {
		t.Fatalf("pubsub.NewSubscriber: %v", err)
	}
	return sub
}

// TestSuperviseWorker_SubscriberFatalError_ReturnsNonZero は requirements 6.5 / NFR 1.1 を検証する。
// subscriber 起動が致命的に失敗（subscription 不在）した場合、非ゼロ終了コードを返すこと。
func TestSuperviseWorker_SubscriberFatalError_ReturnsNonZero(t *testing.T) {
	// Arrange: topic / subscription を seed しない → Run は CodeNotFound で即時失敗する
	srv := newFakeServer(t)
	cfg := workerTestConfig()
	sub := newWorkerSubscriber(t, srv, cfg)
	noopHandler := pubsub.HandlerFunc(func(_ context.Context, _ *pubsub.Message) error { return nil })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Act
	code := superviseWorker(ctx, logger.Default(), ephemeralAddr, sub, noopHandler)

	// Assert
	if code != 1 {
		t.Fatalf("subscriber fatal error should fail-fast with code 1, got %d", code)
	}
}

// TestSuperviseWorker_GracefulShutdownOnSignal は requirements 6.4 / NFR 2.1 を検証する。
// ctx キャンセル（停止シグナル相当）で subscriber 受信を止め、healthz を graceful shutdown して
// 終了コード 0 を返すこと。
func TestSuperviseWorker_GracefulShutdownOnSignal(t *testing.T) {
	// Arrange: 正常な subscription を用意する
	srv := newFakeServer(t)
	cfg := workerTestConfig()
	seedTopicAndSubscription(t, srv, testProjectID, testTopicID, testSubID)
	sub := newWorkerSubscriber(t, srv, cfg)

	var received atomic.Int32
	handler := pubsub.HandlerFunc(func(_ context.Context, _ *pubsub.Message) error {
		received.Add(1)
		return nil
	})
	srv.Publish("projects/"+testProjectID+"/topics/"+testTopicID, []byte("hi"), nil)

	ctx, cancel := context.WithCancel(context.Background())

	// Act: superviseWorker を goroutine で動かし、メッセージ受信後にキャンセルする
	done := make(chan int, 1)
	go func() { done <- superviseWorker(ctx, logger.Default(), ephemeralAddr, sub, handler) }()

	waitForCount(t, &received, 1) // 受信ループが稼働していることを確認
	cancel()                      // 停止シグナル相当

	// Assert
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("graceful shutdown should return code 0, got %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("superviseWorker did not return after ctx cancel (graceful shutdown timed out)")
	}
}

// TestPendingDispatchHandler_RetainsViaNack は finding（main.go:141）を検証する。
// 実 Dispatcher 未配線（#36）の間、暫定 handler が ack せず nack（再配信保持）に写像されること。
// ack してしまうと未処理の AMAPI 通知を恒久喪失するため、ShouldAck=false（=nack）であることを確かめる。
func TestPendingDispatchHandler_RetainsViaNack(t *testing.T) {
	// Arrange
	handler := pendingDispatchHandler(logger.Default())

	// Act
	err := handler.Handle(context.Background(), &pubsub.Message{ID: "m-1"})

	// Assert
	if err == nil {
		t.Fatalf("placeholder handler must not ack (return nil); want a transient error for redelivery")
	}
	if pkgerrors.ShouldAck(err, nil) {
		t.Fatalf("placeholder handler error must map to nack (retain), but ShouldAck=true (ack/drop)")
	}
}

// TestGracefulShutdown_WaitsForSubscriberDrain は finding（main.go:185）と requirements 6.4 を検証する。
// 停止シグナル後、subscriber の受信ループ完了（処理中メッセージの確定）を待ってから 0 を返すこと。
func TestGracefulShutdown_WaitsForSubscriberDrain(t *testing.T) {
	// Arrange: subscriber がまだ drain 中（一定時間後に完了通知）。
	srv := &http.Server{Addr: ephemeralAddr}
	subErr := make(chan error, 1)
	drained := make(chan struct{})
	go func() {
		time.Sleep(150 * time.Millisecond)
		close(drained)
		subErr <- nil
	}()

	// Act
	start := time.Now()
	code := gracefulShutdown(srv, subErr, logger.Default(), 2*time.Second)
	elapsed := time.Since(start)

	// Assert: subscriber 完了を待ってから戻る。
	select {
	case <-drained:
	default:
		t.Fatalf("gracefulShutdown returned before subscriber drained")
	}
	if code != 0 {
		t.Fatalf("clean drain should return code 0, got %d", code)
	}
	if elapsed < 150*time.Millisecond {
		t.Fatalf("gracefulShutdown did not wait for subscriber drain (elapsed %s)", elapsed)
	}
}

// TestGracefulShutdown_TimesOutWhenSubscriberHangs は requirements NFR 2.2 を検証する。
// 猶予時間内に処理中メッセージの確定が終わらない場合、非ゼロ終了コードを返すこと。
func TestGracefulShutdown_TimesOutWhenSubscriberHangs(t *testing.T) {
	// Arrange: subErr が永遠に来ない（drain がハングする）。
	srv := &http.Server{Addr: ephemeralAddr}
	subErr := make(chan error, 1)

	// Act
	code := gracefulShutdown(srv, subErr, logger.Default(), 100*time.Millisecond)

	// Assert
	if code != 1 {
		t.Fatalf("drain timeout should fail-fast with code 1 (NFR 2.2), got %d", code)
	}
}

// TestGracefulShutdown_SubscriberDrainError_ReturnsNonZero は drain 中の subscriber エラーが
// 非ゼロ終了になることを検証する（requirements 6.5）。
func TestGracefulShutdown_SubscriberDrainError_ReturnsNonZero(t *testing.T) {
	// Arrange
	srv := &http.Server{Addr: ephemeralAddr}
	subErr := make(chan error, 1)
	subErr <- stdErrors.New("drain boom")

	// Act
	code := gracefulShutdown(srv, subErr, logger.Default(), 2*time.Second)

	// Assert
	if code != 1 {
		t.Fatalf("subscriber drain error should return code 1, got %d", code)
	}
}

// waitForCount は atomic counter が n に達するまで待つ（テスト用、deadline 付き）。
func waitForCount(t *testing.T, c *atomic.Int32, n int32) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for c.Load() < n {
		select {
		case <-deadline:
			t.Fatalf("counter did not reach %d (current %d)", n, c.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}
}
