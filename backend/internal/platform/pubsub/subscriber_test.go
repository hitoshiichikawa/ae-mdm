package pubsub

import (
	"context"
	stdErrors "errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gpubsub "cloud.google.com/go/pubsub"
	"cloud.google.com/go/pubsub/pstest"
	"google.golang.org/api/option"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// topicPath は pstest.Server.Publish に渡す完全修飾 topic 名を作る。
func topicPath(topicID string) string {
	return "projects/" + testProjectID + "/topics/" + topicID
}

// runReceiveUntil は Subscriber.Run を goroutine で起動し、cond が true を返すか deadline まで
// 待ってから ctx をキャンセルして停止する。Run の戻り値（graceful shutdown 時は nil）を返す。
//
// deadline は条件成立まで待つ最大時間。条件が満たされた瞬間に停止するため、軽い条件では
// 早期に返る。nack 再配信は SDK の lease 管理を経るため数秒かかることがあり、その観測には
// 余裕を持った deadline（15s）を使う。
func runReceiveUntil(t *testing.T, sub *Subscriber, handler MessageHandler, cond func() bool) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- sub.Run(ctx, handler) }()

	deadline := time.After(15 * time.Second)
	for {
		if cond() {
			cancel()
			break
		}
		select {
		case <-deadline:
			cancel()
			t.Fatalf("condition not met within deadline")
		case <-time.After(10 * time.Millisecond):
		}
	}
	select {
	case err := <-runErr:
		return err
	case <-time.After(3 * time.Second):
		t.Fatalf("Run did not return after ctx cancel")
		return nil
	}
}

// TestSubscriber_ReceivesAndHandsToHandler は requirements 2.1 / 2.2 を検証する。
// publish したメッセージが handler に引き渡されること。
func TestSubscriber_ReceivesAndHandsToHandler(t *testing.T) {
	// Arrange
	srv := newFakeServer(t)
	cfg := testConfig()
	seedTopicAndSubscription(t, srv, testProjectID, testTopicID, testSubID)
	client := newTestClient(t, srv, cfg)
	sub, err := NewSubscriber(client, cfg, nil, nil)
	if err != nil {
		t.Fatalf("NewSubscriber: %v", err)
	}
	srv.Publish(topicPath(testTopicID), []byte("hello"), nil)

	var got [][]byte
	var mu sync.Mutex
	handler := HandlerFunc(func(_ context.Context, msg *Message) error {
		mu.Lock()
		got = append(got, msg.Data)
		mu.Unlock()
		return nil
	})

	// Act
	runErr := runReceiveUntil(t, sub, handler, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) >= 1
	})

	// Assert
	if runErr != nil {
		t.Fatalf("Run returned error: %v", runErr)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 || string(got[0]) != "hello" {
		t.Fatalf("handler did not receive published payload, got %v", got)
	}
}

// TestSubscriber_EmptyPayload_StillHandedToHandler は requirements 2.5 を検証する。
// 空 payload のメッセージも破棄せず handler に引き渡されること。
func TestSubscriber_EmptyPayload_StillHandedToHandler(t *testing.T) {
	// Arrange
	srv := newFakeServer(t)
	cfg := testConfig()
	seedTopicAndSubscription(t, srv, testProjectID, testTopicID, testSubID)
	client := newTestClient(t, srv, cfg)
	sub, err := NewSubscriber(client, cfg, nil, nil)
	if err != nil {
		t.Fatalf("NewSubscriber: %v", err)
	}
	srv.Publish(topicPath(testTopicID), []byte{}, nil)

	var called atomic.Bool
	var sawEmpty atomic.Bool
	handler := HandlerFunc(func(_ context.Context, msg *Message) error {
		called.Store(true)
		if len(msg.Data) == 0 {
			sawEmpty.Store(true)
		}
		return nil
	})

	// Act
	runErr := runReceiveUntil(t, sub, handler, func() bool { return called.Load() })

	// Assert
	if runErr != nil {
		t.Fatalf("Run returned error: %v", runErr)
	}
	if !sawEmpty.Load() {
		t.Fatalf("empty payload message was not handed to handler")
	}
}

// TestSubscriber_SubscriptionMissing_ReturnsErrorWithoutLoop は requirements 2.4 を検証する。
// subscription が存在しないとき、受信ループを開始せず構造化エラーを返すこと。
func TestSubscriber_SubscriptionMissing_ReturnsErrorWithoutLoop(t *testing.T) {
	// Arrange: topic / subscription を seed しない
	srv := newFakeServer(t)
	cfg := testConfig()
	client := newTestClient(t, srv, cfg)
	sub, err := NewSubscriber(client, cfg, nil, nil)
	if err != nil {
		t.Fatalf("NewSubscriber: %v", err)
	}

	var handlerCalled atomic.Bool
	handler := HandlerFunc(func(_ context.Context, _ *Message) error {
		handlerCalled.Store(true)
		return nil
	})

	// Act
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runErr := sub.Run(ctx, handler)

	// Assert
	if runErr == nil {
		t.Fatalf("expected error for missing subscription, got nil")
	}
	var de *pkgerrors.Error
	if !stdErrors.As(runErr, &de) || de.Code != pkgerrors.CodeNotFound {
		t.Fatalf("expected CodeNotFound, got %v", runErr)
	}
	if handlerCalled.Load() {
		t.Fatalf("receive loop must not start when subscription is missing")
	}
}

// TestSubscriber_CanceledDuringExistsCheck_ReturnsNilGracefully は finding（subscriber.go:117）
// と requirements 6.4 を検証する。存在確認の最中に ctx がキャンセル（SIGINT/SIGTERM 相当）
// された場合、致命的エラーではなく graceful shutdown として nil を返すこと。
func TestSubscriber_CanceledDuringExistsCheck_ReturnsNilGracefully(t *testing.T) {
	// Arrange: subscription は seed 済み（存在自体は問題ない）。
	srv := newFakeServer(t)
	cfg := testConfig()
	seedTopicAndSubscription(t, srv, testProjectID, testTopicID, testSubID)
	client := newTestClient(t, srv, cfg)
	sub, err := NewSubscriber(client, cfg, nil, nil)
	if err != nil {
		t.Fatalf("NewSubscriber: %v", err)
	}

	var handlerCalled atomic.Bool
	handler := HandlerFunc(func(_ context.Context, _ *Message) error {
		handlerCalled.Store(true)
		return nil
	})

	// 事前にキャンセル済みの ctx を渡す（存在確認が ctx 起因で失敗する）。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Act
	runErr := sub.Run(ctx, handler)

	// Assert: fatal 化せず nil（graceful）を返す。
	if runErr != nil {
		t.Fatalf("canceled context during existence check should return nil, got: %v", runErr)
	}
	if handlerCalled.Load() {
		t.Fatalf("receive loop must not start when context is already canceled")
	}
}

// TestSubscriber_HandlerSuccess_Acks は requirements 3.1 を検証する。
// handler 成功時はメッセージが ack され、再配信されないこと（handler は 1 回だけ呼ばれる）。
func TestSubscriber_HandlerSuccess_Acks(t *testing.T) {
	// Arrange
	srv := newFakeServer(t)
	cfg := testConfig()
	seedTopicAndSubscription(t, srv, testProjectID, testTopicID, testSubID)
	client := newTestClient(t, srv, cfg)
	sub, err := NewSubscriber(client, cfg, nil, nil)
	if err != nil {
		t.Fatalf("NewSubscriber: %v", err)
	}
	srv.Publish(topicPath(testTopicID), []byte("ok"), nil)

	var calls atomic.Int32
	handler := HandlerFunc(func(_ context.Context, _ *Message) error {
		calls.Add(1)
		return nil // ack
	})

	// Act: 1 回受信したら少し待って再配信が来ないことを観測する
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- sub.Run(ctx, handler) }()
	waitForCount(t, &calls, 1)
	time.Sleep(1500 * time.Millisecond) // 再配信ウィンドウを通過させる
	cancel()

	// Assert
	if err := <-runErr; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("acked message must not be redelivered: handler called %d times, want 1", n)
	}
}

// TestSubscriber_TransientError_Nacks_Redelivered は requirements 3.2 / 3.3 を検証する。
// handler が transient エラーを返すと nack され、再配信される（handler が複数回呼ばれる）。
func TestSubscriber_TransientError_Nacks_Redelivered(t *testing.T) {
	// Arrange
	srv := newFakeServer(t)
	cfg := testConfig()
	seedTopicAndSubscription(t, srv, testProjectID, testTopicID, testSubID)
	client := newTestClient(t, srv, cfg)
	sub, err := NewSubscriber(client, cfg, nil, nil)
	if err != nil {
		t.Fatalf("NewSubscriber: %v", err)
	}
	srv.Publish(topicPath(testTopicID), []byte("retry"), nil)

	transient := pkgerrors.New(pkgerrors.CodeUnavailable, "temporary failure")
	transient.IsTransient = true
	var calls atomic.Int32
	handler := HandlerFunc(func(_ context.Context, _ *Message) error {
		calls.Add(1)
		return transient // nack -> redeliver
	})

	// Act: 2 回以上呼ばれる（= 再配信された）ことを観測したら停止
	runErr := runReceiveUntil(t, sub, handler, func() bool { return calls.Load() >= 2 })

	// Assert
	if runErr != nil {
		t.Fatalf("Run returned error: %v", runErr)
	}
	if n := calls.Load(); n < 2 {
		t.Fatalf("transient error should be redelivered: handler called %d times, want >= 2", n)
	}
}

// TestSubscriber_PermanentError_Acks_NotRedelivered は requirements 3.4 を検証する。
// handler が恒常的（非 transient）エラーを返すと ack され、無限再配信に陥らないこと。
func TestSubscriber_PermanentError_Acks_NotRedelivered(t *testing.T) {
	// Arrange
	srv := newFakeServer(t)
	cfg := testConfig()
	seedTopicAndSubscription(t, srv, testProjectID, testTopicID, testSubID)
	client := newTestClient(t, srv, cfg)
	sub, err := NewSubscriber(client, cfg, nil, nil)
	if err != nil {
		t.Fatalf("NewSubscriber: %v", err)
	}
	srv.Publish(topicPath(testTopicID), []byte("perm"), nil)

	permanent := pkgerrors.New(pkgerrors.CodeInvalidRequest, "permanent failure") // IsTransient=false
	var calls atomic.Int32
	handler := HandlerFunc(func(_ context.Context, _ *Message) error {
		calls.Add(1)
		return permanent // ack despite error
	})

	// Act
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- sub.Run(ctx, handler) }()
	waitForCount(t, &calls, 1)
	time.Sleep(1500 * time.Millisecond) // 再配信ウィンドウを通過させる
	cancel()

	// Assert
	if err := <-runErr; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("permanent error should ack (no redelivery): handler called %d times, want 1", n)
	}
}

// TestNewSubscriber_MaxOutstandingBelowOne_Rejected は requirements 4.3 を検証する。
// opts 非 nil で MaxOutstandingMessages < 1（0 を含む）を構造化エラーで拒否すること。
// 0（ゼロ値）も「並行度ゼロ＝受信停止」の誤設定であり、拒否対象に含める。
func TestNewSubscriber_MaxOutstandingBelowOne_Rejected(t *testing.T) {
	cases := []struct {
		name  string
		value int
	}{
		{name: "zero", value: 0},
		{name: "negative", value: -1},
		{name: "negative large", value: -100},
	}
	srv := newFakeServer(t)
	cfg := testConfig()
	client := newTestClient(t, srv, cfg)

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			// Act
			sub, err := NewSubscriber(client, cfg, nil, &SubscriberOptions{MaxOutstandingMessages: c.value})
			// Assert
			if err == nil {
				t.Fatalf("MaxOutstandingMessages=%d should be rejected, got sub=%v", c.value, sub)
			}
			var de *pkgerrors.Error
			if !stdErrors.As(err, &de) || de.Code != pkgerrors.CodeConfigInvalid {
				t.Fatalf("expected CodeConfigInvalid, got %v", err)
			}
		})
	}
}

// TestNewSubscriber_NilOpts_UsesDefault は opts=nil が既定値で受理されることを検証する
// （requirements 4.1: 既定値で良い場合は opts に nil を渡す契約）。
func TestNewSubscriber_NilOpts_UsesDefault(t *testing.T) {
	// Arrange
	srv := newFakeServer(t)
	cfg := testConfig()
	client := newTestClient(t, srv, cfg)

	// Act
	sub, err := NewSubscriber(client, cfg, nil, nil)

	// Assert
	if err != nil {
		t.Fatalf("nil opts should be accepted with default, got error: %v", err)
	}
	if sub.maxOutstandingMessages != defaultMaxOutstandingMessages {
		t.Errorf("maxOutstandingMessages = %d, want default %d",
			sub.maxOutstandingMessages, defaultMaxOutstandingMessages)
	}
}

// TestNewSubscriber_MaxOutstandingAccepted は requirements 4.1 の境界（1 以上は受理）を検証する。
func TestNewSubscriber_MaxOutstandingAccepted(t *testing.T) {
	srv := newFakeServer(t)
	cfg := testConfig()
	client := newTestClient(t, srv, cfg)

	for _, v := range []int{1, 5, 1000} {
		v := v
		t.Run("accept", func(t *testing.T) {
			// Act
			sub, err := NewSubscriber(client, cfg, nil, &SubscriberOptions{MaxOutstandingMessages: v})
			// Assert
			if err != nil {
				t.Fatalf("MaxOutstandingMessages=%d should be accepted, got error: %v", v, err)
			}
			if sub.maxOutstandingMessages != v {
				t.Errorf("maxOutstandingMessages = %d, want %d", sub.maxOutstandingMessages, v)
			}
		})
	}
}

// TestSubscriber_DeadLetterPublish_Success は requirements 5.1 / 5.2 / 5.3 を検証する。
// dead-letter 送出 IF が payload と元 message_id を dead-letter topic に publish すること。
func TestSubscriber_DeadLetterPublish_Success(t *testing.T) {
	// Arrange
	srv := newFakeServer(t)
	cfg := testConfig()
	// dead-letter topic とその購読 subscription を作って届いたか検証する
	seedTopicAndSubscription(t, srv, testProjectID, testDeadLetter, testDeadLetter+"-sub")
	client := newTestClient(t, srv, cfg)
	sub, err := NewSubscriber(client, cfg, nil, nil)
	if err != nil {
		t.Fatalf("NewSubscriber: %v", err)
	}

	// Act
	err = sub.PublishToDeadLetter(context.Background(), &Message{
		ID:   "orig-123",
		Data: []byte("poison"),
	})

	// Assert: 送出成功
	if err != nil {
		t.Fatalf("PublishToDeadLetter returned error: %v", err)
	}

	// dead-letter topic に届いていることを購読側から確認する
	got := pullOne(t, srv, testProjectID, testDeadLetter+"-sub")
	if string(got.Data) != "poison" {
		t.Errorf("dead-letter payload = %q, want %q", got.Data, "poison")
	}
	if got.Attributes["original_message_id"] != "orig-123" {
		t.Errorf("dead-letter original_message_id = %q, want orig-123", got.Attributes["original_message_id"])
	}
}

// TestSubscriber_DeadLetterPublish_TopicUnset_ReturnsError は requirements 5.4 を検証する。
// dead-letter topic 未設定のとき構造化エラーを返し（呼び出し側で ack しない）こと。
func TestSubscriber_DeadLetterPublish_TopicUnset_ReturnsError(t *testing.T) {
	// Arrange: dead-letter topic を空にする
	srv := newFakeServer(t)
	cfg := testConfig()
	cfg.PubSubDeadLetterTopic = ""
	client := newTestClient(t, srv, cfg)
	sub, err := NewSubscriber(client, cfg, nil, nil)
	if err != nil {
		t.Fatalf("NewSubscriber: %v", err)
	}

	// Act
	err = sub.PublishToDeadLetter(context.Background(), &Message{ID: "x", Data: []byte("d")})

	// Assert
	if err == nil {
		t.Fatalf("expected error when dead-letter topic is unset, got nil")
	}
	var de *pkgerrors.Error
	if !stdErrors.As(err, &de) || de.Code != pkgerrors.CodeConfigInvalid {
		t.Fatalf("expected CodeConfigInvalid, got %v", err)
	}
}

// --- helpers ----------------------------------------------------------------

// waitForCount は atomic counter が n に達するまで待つ（テスト用、deadline 付き）。
func waitForCount(t *testing.T, c *atomic.Int32, n int32) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for c.Load() < n {
		select {
		case <-deadline:
			t.Fatalf("counter did not reach %d (current %d)", n, c.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// pullOne は subscription から 1 件だけメッセージを取り出して返す（テスト用）。
func pullOne(t *testing.T, srv *pstest.Server, projectID, subID string) *gpubsub.Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, err := gpubsub.NewClient(ctx, projectID, option.WithGRPCConn(dialFake(t, srv)))
	if err != nil {
		t.Fatalf("pullOne: NewClient: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	out := make(chan *gpubsub.Message, 1)
	go func() {
		_ = raw.Subscription(subID).Receive(ctx, func(_ context.Context, m *gpubsub.Message) {
			m.Ack()
			select {
			case out <- m:
			default:
			}
			cancel()
		})
	}()
	select {
	case m := <-out:
		return m
	case <-ctx.Done():
		t.Fatalf("pullOne: no message received from %s", subID)
		return nil
	}
}
