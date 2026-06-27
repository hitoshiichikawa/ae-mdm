package pubsub

import (
	"context"
	"testing"

	gpubsub "cloud.google.com/go/pubsub"
	"cloud.google.com/go/pubsub/pstest"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/hitoshiichikawa/ae-mdm/internal/config"
)

// --- test fixtures / helpers ------------------------------------------------

const (
	testProjectID  = "ae-mdm-test"
	testTopicID    = "amapi-notifications"
	testSubID      = "amapi-notifications-sub"
	testDeadLetter = "amapi-notifications-deadletter"
)

// newFakeServer は pstest の in-process fake server を返す。
// docker / 実 emulator は使わない（cache 済 / 高速 / 決定論）。
//
// SDK の Client.Close() は WithGRPCConn で渡した接続をそのまま閉じるため、テストでは
// 「1 SDK client = 1 接続」とし、複数 client が同一接続を共有して相互に Close で殺し合う
// 状況を避ける。各 client への接続は dialFake で都度作成する。
func newFakeServer(t *testing.T) *pstest.Server {
	t.Helper()
	srv := pstest.NewServer()
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// dialFake は fake server への新規 gRPC 接続を作る（client ごとに 1 本）。
func dialFake(t *testing.T, srv *pstest.Server) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(srv.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	// 注意: 本接続を消費する SDK client が Close() で閉じる場合があるため、ここでは
	// t.Cleanup での二重 Close を避け、呼び出し側の Close に委ねる（pstest は閉じ済みでも安全）。
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// testConfig は本パッケージの想定 env を埋めた config.Config を返す。
func testConfig() config.Config {
	return config.Config{
		PubSubProjectID:       testProjectID,
		PubSubTopic:           testTopicID,
		PubSubSubscription:    testSubID,
		PubSubDeadLetterTopic: testDeadLetter,
		PubSubEmulatorHost:    "pubsub-emulator:8085", // emulator モード扱い
	}
}

// newTestClient は fake server に専用接続でつないだ Client を返す（GRPCConn 経由）。
func newTestClient(t *testing.T, srv *pstest.Server, cfg config.Config) *Client {
	t.Helper()
	c, err := NewClient(context.Background(), cfg, nil, &Options{GRPCConn: dialFake(t, srv)})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// newVerifyClient は検証用の生 SDK client を専用接続で返す。Close で接続も閉じられるが、
// 各 client が独立接続を持つため他 client へ影響しない。
func newVerifyClient(t *testing.T, srv *pstest.Server, projectID string) *gpubsub.Client {
	t.Helper()
	raw, err := gpubsub.NewClient(context.Background(), projectID, option.WithGRPCConn(dialFake(t, srv)))
	if err != nil {
		t.Fatalf("verify client: NewClient: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	return raw
}

// seedTopicAndSubscription は fake server 上に topic と pull subscription を作る。
// 専用接続を使い、生成後に閉じても他 client へ影響しない。
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
