// Package pubsub は ae-mdm の worker が AMAPI 通知を Cloud Pub/Sub の pull subscription
// から受信するためのクライアント基盤を提供する。
//
// 本パッケージは以下を担う（Issue #35 / tasks 6.1）:
//   - emulator / 本番 GCP を切り替える Pub/Sub client の構築（client.go）
//   - pull subscription の受信ループと ack / nack 抽象、dead-letter 送出抽象（subscriber.go）
//
// 通知種別ごとの dispatch・冪等処理・未割当退避・Envelope パースは別 Issue #36 のスコープ
// であり、本パッケージは raw payload を handler に引き渡す土台のみを提供する。
//
// 接続先の切替方針:
//   - config.PubSubEmulatorHost が非空のとき、当該 emulator エンドポイントへ平文 gRPC で
//     接続する（option.WithEndpoint + WithoutAuthentication + insecure transport credentials）。
//     Pub/Sub SDK は PUBSUB_EMULATOR_HOST 環境変数も自動参照するが、worker 起動時の env に
//     依存せず config 経由で明示注入できるよう、本パッケージでは config 値を一次情報とする。
//   - config.PubSubEmulatorHost が空のとき、本番 GCP の Pub/Sub に ADC（Application Default
//     Credentials）で接続する。
package pubsub

import (
	"context"
	"sync"

	gpubsub "cloud.google.com/go/pubsub"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
)

// Client は ae-mdm が扱う Pub/Sub client の薄いラッパ。SDK の *pubsub.Client を保持し、
// Subscriber 構築や dead-letter 送出のための Topic / Subscription ハンドルを供給する。
//
// Close は多重呼び出し安全（idempotent）であり、2 回目以降の Close は no-op として nil を返す
// （requirements 1.5 / 1.6）。本型は構築後 read-only で goroutine-safe に利用できる。
type Client struct {
	raw       *gpubsub.Client
	projectID string
	log       logger.Logger

	closeOnce sync.Once
	closeErr  error
}

// Options は Client 構築時の試験用フックを保持する（本番では nil でよい）。
//
// テストでは GRPCConn に pstest（in-process fake）の *grpc.ClientConn を差し込み、
// docker / 実 emulator なしで決定論的な受信テストを書ける。
type Options struct {
	// GRPCConn は SDK 構築時に注入する gRPC 接続。非 nil の場合、emulator host / ADC の
	// 切替を行わず当該接続をそのまま使う（pstest 用）。
	GRPCConn *grpc.ClientConn
}

// NewClient は config から Pub/Sub Client を構築する。
//
// 認証・接続先は以下の優先順で決定する:
//  1. opts.GRPCConn が非 nil → 当該接続をそのまま使う（テスト用 / requirements 影響なし）
//  2. cfg.PubSubEmulatorHost が非空 → emulator エンドポイントへ平文接続（requirements 1.2）
//  3. 上記いずれでもない → 本番 GCP の Pub/Sub へ ADC で接続（requirements 1.3）
//
// 構築に失敗した場合は *pkgerrors.Error{Code: CodeUnavailable, IsTransient: true} を返し、
// 部分初期化されたリソースを残さない（requirements 1.4 / NFR 1.1）。
func NewClient(ctx context.Context, cfg config.Config, log logger.Logger, opts *Options) (*Client, error) {
	if log == nil {
		// DI 未配線でも構造化ログ呼び出しで panic させない（出力先が無いだけ）。
		log = logger.Default()
	}

	clientOpts := buildClientOptions(cfg, opts)

	raw, err := gpubsub.NewClient(ctx, cfg.PubSubProjectID, clientOpts...)
	if err != nil {
		// 接続先未到達・認証失敗等。worker は fail-fast で非ゼロ終了する（NFR 1.1）。
		out := pkgerrors.Wrap(pkgerrors.CodeUnavailable,
			"failed to construct Pub/Sub client", err)
		out.IsTransient = true
		return nil, out
	}

	return &Client{
		raw:       raw,
		projectID: cfg.PubSubProjectID,
		log:       log,
	}, nil
}

// buildClientOptions は接続先切替の option.ClientOption 列を組み立てる。
//
// emulator モードでは insecure transport credentials を明示し、ADC を要求しない
// （ローカル開発で認証情報が無くても接続できるようにする / requirements 1.2）。
func buildClientOptions(cfg config.Config, opts *Options) []option.ClientOption {
	if opts != nil && opts.GRPCConn != nil {
		return []option.ClientOption{option.WithGRPCConn(opts.GRPCConn)}
	}
	if cfg.PubSubEmulatorHost != "" {
		return []option.ClientOption{
			option.WithEndpoint(cfg.PubSubEmulatorHost),
			option.WithoutAuthentication(),
			option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
		}
	}
	// 本番 GCP: ADC（Application Default Credentials）で接続する（requirements 1.3）。
	return nil
}

// ProjectID は構築に使った Pub/Sub プロジェクト識別子を返す。
func (c *Client) ProjectID() string { return c.projectID }

// Close は保持している SDK client の接続・内部リソースを解放する（requirements 1.5）。
//
// sync.Once により多重 Close でも追加のエラーを発生させず、初回の結果を返す（requirements 1.6）。
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		if c.raw != nil {
			c.closeErr = c.raw.Close()
		}
	})
	return c.closeErr
}

// EnsureEmulatorResources は emulator モード（cfg.PubSubEmulatorHost が非空）のときに限り、
// topic / subscription / dead-letter topic を冪等に ensure（不在なら作成）する。
//
// 本処理は `make up`（emulator）で worker が実際に受信可能になるための土台であり、
// 本番モード（emulator host 空）では一切自動作成しない（IaC 前提 / requirements 2.4 が本番側で
// 成立するように、本番では subscription 不在をエラーとして surface させる）。emulator host が
// 空の場合は no-op で nil を返す。
//
// 作成対象:
//   - cfg.PubSubTopic（メイン topic）
//   - cfg.PubSubDeadLetterTopic（設定されている場合のみ）
//   - cfg.PubSubSubscription（メイン topic に紐づく pull subscription）
//
// 既存リソースは AlreadyExists として扱い、冪等にスキップする。
func (c *Client) EnsureEmulatorResources(ctx context.Context, cfg config.Config) error {
	if cfg.PubSubEmulatorHost == "" {
		// 本番モード: 自動作成しない（requirements 2.4）。
		return nil
	}

	mainTopic, err := c.ensureTopic(ctx, cfg.PubSubTopic)
	if err != nil {
		return err
	}
	if cfg.PubSubDeadLetterTopic != "" {
		if _, err := c.ensureTopic(ctx, cfg.PubSubDeadLetterTopic); err != nil {
			return err
		}
	}
	if err := c.ensureSubscription(ctx, cfg.PubSubSubscription, mainTopic); err != nil {
		return err
	}

	c.log.Info("pubsub: emulator resources ensured",
		"topic", cfg.PubSubTopic,
		"subscription", cfg.PubSubSubscription,
		"dead_letter_topic", cfg.PubSubDeadLetterTopic)
	return nil
}

// ensureTopic は topic を冪等に作成する。既存なら作成をスキップしてハンドルを返す。
//
// Exists 確認と Create の間に別 worker が同一 topic を作る TOCTOU 競合があり得るため、
// Create が codes.AlreadyExists を返した場合は冪等成功として既存ハンドルを返す
// （並行起動時に AlreadyExists を fatal 化しない）。
func (c *Client) ensureTopic(ctx context.Context, topicID string) (*gpubsub.Topic, error) {
	topic := c.raw.Topic(topicID)
	exists, err := topic.Exists(ctx)
	if err != nil {
		return nil, pkgerrors.Wrap(pkgerrors.CodeUnavailable,
			"failed to check Pub/Sub topic existence: "+topicID, err)
	}
	if exists {
		return topic, nil
	}
	created, err := c.raw.CreateTopic(ctx, topicID)
	if err != nil {
		if status.Code(err) == codes.AlreadyExists {
			// 並行起動で別 worker が先に作成済み。既存ハンドルを返して冪等に続行する。
			return c.raw.Topic(topicID), nil
		}
		return nil, pkgerrors.Wrap(pkgerrors.CodeUnavailable,
			"failed to create Pub/Sub topic: "+topicID, err)
	}
	return created, nil
}

// ensureSubscription は topic に紐づく pull subscription を冪等に作成する。
//
// ensureTopic と同様、Create が codes.AlreadyExists を返した場合は冪等成功として扱う
// （並行起動の TOCTOU 競合対策）。
func (c *Client) ensureSubscription(ctx context.Context, subID string, topic *gpubsub.Topic) error {
	sub := c.raw.Subscription(subID)
	exists, err := sub.Exists(ctx)
	if err != nil {
		return pkgerrors.Wrap(pkgerrors.CodeUnavailable,
			"failed to check Pub/Sub subscription existence: "+subID, err)
	}
	if exists {
		return nil
	}
	if _, err := c.raw.CreateSubscription(ctx, subID, gpubsub.SubscriptionConfig{Topic: topic}); err != nil {
		if status.Code(err) == codes.AlreadyExists {
			// 並行起動で別 worker が先に作成済み。冪等成功として続行する。
			return nil
		}
		return pkgerrors.Wrap(pkgerrors.CodeUnavailable,
			"failed to create Pub/Sub subscription: "+subID, err)
	}
	return nil
}
