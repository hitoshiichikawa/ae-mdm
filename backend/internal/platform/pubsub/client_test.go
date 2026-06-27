package pubsub

import (
	"context"
	stdErrors "errors"
	"testing"

	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// TestNewClient_ConstructsWithProjectID は requirements 1.1 を検証する。
// 設定された projectID で client が構築され、ProjectID() で参照できること。
func TestNewClient_ConstructsWithProjectID(t *testing.T) {
	// Arrange
	srv := newFakeServer(t)
	cfg := testConfig()

	// Act
	c, err := NewClient(context.Background(), cfg, nil, &Options{GRPCConn: dialFake(t, srv)})

	// Assert
	if err != nil {
		t.Fatalf("NewClient returned error: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if c.ProjectID() != testProjectID {
		t.Errorf("ProjectID() = %q, want %q", c.ProjectID(), testProjectID)
	}
}

// TestNewClient_EmulatorVsProductionOptions は requirements 1.2 / 1.3 を検証する。
// emulator host の有無で接続 option（endpoint / 認証）が切り替わること。
// option は機械検査が難しいため、buildClientOptions の件数差で切替を検証する
// （emulator: endpoint + no-auth + insecure dial = 3 件 / 本番: ADC = 0 件）。
func TestNewClient_EmulatorVsProductionOptions(t *testing.T) {
	t.Run("emulator host 非空のとき endpoint 切替 option が付与される", func(t *testing.T) {
		// Arrange
		cfg := testConfig()
		cfg.PubSubEmulatorHost = "pubsub-emulator:8085"

		// Act
		opts := buildClientOptions(cfg, nil)

		// Assert
		if len(opts) != 3 {
			t.Fatalf("emulator mode should produce 3 client options, got %d", len(opts))
		}
	})

	t.Run("emulator host 空のとき本番 ADC（option なし）になる", func(t *testing.T) {
		// Arrange
		cfg := testConfig()
		cfg.PubSubEmulatorHost = ""

		// Act
		opts := buildClientOptions(cfg, nil)

		// Assert
		if len(opts) != 0 {
			t.Fatalf("production mode should produce 0 client options (ADC), got %d", len(opts))
		}
	})

	t.Run("GRPCConn 注入時は接続をそのまま使う（テスト経路）", func(t *testing.T) {
		// Arrange
		srv := newFakeServer(t)
		cfg := testConfig()

		// Act
		opts := buildClientOptions(cfg, &Options{GRPCConn: dialFake(t, srv)})

		// Assert
		if len(opts) != 1 {
			t.Fatalf("GRPCConn injection should produce exactly 1 option, got %d", len(opts))
		}
	})
}

// TestNewClient_BuildFailure_ReturnsStructuredError は requirements 1.4 / NFR 1.1 を検証する。
// projectID が空のとき SDK 構築が失敗し、構造化エラー（*pkgerrors.Error / transient）を返す。
func TestNewClient_BuildFailure_ReturnsStructuredError(t *testing.T) {
	// Arrange: projectID 空は SDK の NewClient がエラーを返す
	cfg := config.Config{PubSubProjectID: ""}

	// Act
	c, err := NewClient(context.Background(), cfg, nil, nil)

	// Assert
	if err == nil {
		t.Fatalf("expected error for empty projectID, got nil (client=%v)", c)
	}
	var de *pkgerrors.Error
	if !stdErrors.As(err, &de) {
		t.Fatalf("expected *pkgerrors.Error, got %T: %v", err, err)
	}
	if de.Code != pkgerrors.CodeUnavailable {
		t.Errorf("error code = %q, want %q", de.Code, pkgerrors.CodeUnavailable)
	}
	if !de.IsTransient {
		t.Errorf("client construction failure should be transient")
	}
	if c != nil {
		t.Errorf("partial client should not be returned on failure, got %v", c)
	}
}

// TestClient_Close_Idempotent は requirements 1.5 / 1.6 を検証する。
// Close は接続を解放し、再 Close でも追加エラーを起こさず正常終了する。
func TestClient_Close_Idempotent(t *testing.T) {
	// Arrange
	srv := newFakeServer(t)
	cfg := testConfig()
	c, err := NewClient(context.Background(), cfg, nil, &Options{GRPCConn: dialFake(t, srv)})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	// Act
	first := c.Close()
	second := c.Close()
	third := c.Close()

	// Assert
	if first != nil {
		t.Errorf("first Close should succeed, got %v", first)
	}
	if second != nil {
		t.Errorf("second Close should be no-op (nil), got %v", second)
	}
	if third != nil {
		t.Errorf("third Close should be no-op (nil), got %v", third)
	}
}

// TestEnsureEmulatorResources_CreatesIdempotently は requirements 2.4 / 6.3 を検証する
// （emulator モードでの自動 ensure）。topic / subscription / dead-letter topic が作成され、
// 2 回目の呼び出しでも冪等にエラーなく完了すること。
func TestEnsureEmulatorResources_CreatesIdempotently(t *testing.T) {
	// Arrange
	srv := newFakeServer(t)
	cfg := testConfig()
	c := newTestClient(t, srv, cfg)
	ctx := context.Background()

	// Act: 1 回目
	if err := c.EnsureEmulatorResources(ctx, cfg); err != nil {
		t.Fatalf("EnsureEmulatorResources (1st) returned error: %v", err)
	}
	// Act: 2 回目（冪等性）
	if err := c.EnsureEmulatorResources(ctx, cfg); err != nil {
		t.Fatalf("EnsureEmulatorResources (2nd) returned error: %v", err)
	}

	// Assert: 各リソースが存在する
	raw := newVerifyClient(t, srv, testProjectID)
	for _, tc := range []struct {
		name   string
		exists func() (bool, error)
	}{
		{"main topic", func() (bool, error) { return raw.Topic(testTopicID).Exists(ctx) }},
		{"dead-letter topic", func() (bool, error) { return raw.Topic(testDeadLetter).Exists(ctx) }},
		{"subscription", func() (bool, error) { return raw.Subscription(testSubID).Exists(ctx) }},
	} {
		ok, err := tc.exists()
		if err != nil {
			t.Fatalf("%s existence check: %v", tc.name, err)
		}
		if !ok {
			t.Errorf("%s was not created by EnsureEmulatorResources", tc.name)
		}
	}
}

// TestEnsureEmulatorResources_NoopInProduction は requirements 2.4 を検証する。
// 本番モード（emulator host 空）では一切自動作成しないこと。
func TestEnsureEmulatorResources_NoopInProduction(t *testing.T) {
	// Arrange
	srv := newFakeServer(t)
	cfg := testConfig()
	cfg.PubSubEmulatorHost = "" // 本番モード
	c := newTestClient(t, srv, cfg)
	ctx := context.Background()

	// Act
	if err := c.EnsureEmulatorResources(ctx, cfg); err != nil {
		t.Fatalf("EnsureEmulatorResources returned error: %v", err)
	}

	// Assert: topic は作成されていない
	raw := newVerifyClient(t, srv, testProjectID)
	topicExists, err := raw.Topic(testTopicID).Exists(ctx)
	if err != nil {
		t.Fatalf("topic existence check: %v", err)
	}
	if topicExists {
		t.Errorf("production mode must not auto-create the topic")
	}
}
