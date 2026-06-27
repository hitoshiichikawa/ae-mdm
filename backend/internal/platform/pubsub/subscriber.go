package pubsub

import (
	"context"
	stdErrors "errors"
	"time"

	gpubsub "cloud.google.com/go/pubsub"

	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
)

// Message は subscriber が handler へ渡す受信メッセージの最小表現。
//
// 本型は dispatcher の Envelope（Issue #36）ではなく、Pub/Sub から受け取った raw な
// メッセージをそのまま表す。Envelope へのパースは #36 のスコープであり、本パッケージは
// payload をパースせず handler に引き渡す（requirements 2.2 / 2.5）。
type Message struct {
	// ID は Pub/Sub サーバが採番したメッセージ識別子。構造化ログ field（message_id）に使う。
	ID string
	// Data はメッセージ本文（payload）。空 payload の場合は長さ 0 の slice / nil となる。
	Data []byte
	// Attributes はメッセージに付与された key-value 属性。
	Attributes map[string]string
	// PublishTime はメッセージが publish された時刻（サーバ採番、read-only）。
	PublishTime time.Time
}

// MessageHandler は受信メッセージ 1 件を処理する抽象。Issue #36 の Dispatcher がこの IF を
// 満たす形で注入される。本 Issue では暫定 handler（ログのみ）を配線する。
//
// Handle がエラーを返した場合、subscriber は errors.ShouldAck を経由して ack / nack を決定する
// （requirements 3.2）。
type MessageHandler interface {
	Handle(ctx context.Context, msg *Message) error
}

// HandlerFunc は関数を MessageHandler として使うためのアダプタ（テスト・暫定 handler 用）。
type HandlerFunc func(ctx context.Context, msg *Message) error

// Handle は MessageHandler interface を満たす。
func (f HandlerFunc) Handle(ctx context.Context, msg *Message) error { return f(ctx, msg) }

// defaultMaxOutstandingMessages は opts=nil（未指定）時に採る既定値。
// SDK の DefaultReceiveSettings と揃え、明示設定が無い場合の並行度を保つ。
const defaultMaxOutstandingMessages = 1000

// SubscriberOptions は Subscriber の挙動を調整する設定。
type SubscriberOptions struct {
	// MaxOutstandingMessages は同時に未確定（ack 待ち）で保持するメッセージ数の上限
	// （requirements 4.1 / 4.2）。SubscriberOptions を非 nil で渡す場合は本フィールドを
	// 1 以上で明示すること。0（ゼロ値）を含む 1 未満を指定すると NewSubscriber が
	// 構造化エラーを返す（requirements 4.3）。「既定値で良い」場合は opts 自体に nil を渡す。
	MaxOutstandingMessages int
}

// Subscriber は pull subscription からの受信ループと ack / nack 抽象、dead-letter 送出抽象を
// 担う（requirements 2 / 3 / 5）。
type Subscriber struct {
	client                 *Client
	subscriptionID         string
	deadLetterTopic        string
	maxOutstandingMessages int
	log                    logger.Logger
}

// NewSubscriber は Client と config から Subscriber を構築する。
//
// opts が nil の場合は MaxOutstandingMessages に既定値（defaultMaxOutstandingMessages）を採る。
// opts が非 nil の場合、opts.MaxOutstandingMessages が 1 未満（0 を含む）であれば
// *pkgerrors.Error{Code: CodeConfigInvalid} を返して構築を拒否する（requirements 4.3。
// 0 は SDK の「並行度ゼロ＝受信停止」と区別がつかない誤設定であり、未指定とは扱わない。
// 既定値で良い場合は opts 自体に nil を渡すこと）。
func NewSubscriber(client *Client, cfg config.Config, log logger.Logger, opts *SubscriberOptions) (*Subscriber, error) {
	if client == nil {
		return nil, pkgerrors.New(pkgerrors.CodeConfigInvalid, "pubsub client is required for Subscriber")
	}
	if log == nil {
		log = logger.Default()
	}

	maxOutstanding := defaultMaxOutstandingMessages
	if opts != nil {
		if opts.MaxOutstandingMessages < 1 {
			// 1 未満（0 / 負値）は不正設定として拒否する（requirements 4.3）。
			return nil, pkgerrors.New(pkgerrors.CodeConfigInvalid,
				"MaxOutstandingMessages must be >= 1")
		}
		maxOutstanding = opts.MaxOutstandingMessages
	}

	return &Subscriber{
		client:                 client,
		subscriptionID:         cfg.PubSubSubscription,
		deadLetterTopic:        cfg.PubSubDeadLetterTopic,
		maxOutstandingMessages: maxOutstanding,
		log:                    log,
	}, nil
}

// Run は設定された subscription に対する pull 受信ループを開始する（requirements 2.1）。
//
// 受信開始前に subscription の存在を確認し、不在の場合は構造化エラーを返して受信ループを
// 開始しない（requirements 2.4）。ctx がキャンセルされると受信を停止して nil を返す
// （graceful shutdown / requirements 6.4）。受信中に SDK が返した致命的エラーは
// *pkgerrors.Error として返す（NFR 1.1）。
//
// 各メッセージは toMessage で raw 表現に変換し、handler.Handle を呼ぶ。結果は
// errors.ShouldAck（既存 worker_mapping）を経由して ack / nack を決定する（requirements 3.x）。
func (s *Subscriber) Run(ctx context.Context, handler MessageHandler) error {
	if handler == nil {
		return pkgerrors.New(pkgerrors.CodeConfigInvalid, "MessageHandler is required for Subscriber.Run")
	}

	sub := s.client.raw.Subscription(s.subscriptionID)

	// 受信開始前の存在確認（requirements 2.4）。subscription 不在で受信ループを始めると
	// SDK が後段で曖昧なエラーを返すため、ここで明示的に fail-fast する。
	exists, err := sub.Exists(ctx)
	if err != nil {
		// 存在確認中に ctx がキャンセル（SIGINT/SIGTERM 等）された場合は致命的失敗ではなく
		// graceful shutdown として nil を返す（requirements 6.4 / NFR 2.1）。gRPC は
		// codes.Canceled を context.Canceled でない status 値で返すことがあるため、
		// 渡された ctx 自体の状態で判定する。
		if ctx.Err() != nil {
			s.log.Info("pubsub: subscription existence check canceled during shutdown",
				"subscription", s.subscriptionID)
			return nil
		}
		out := pkgerrors.Wrap(pkgerrors.CodeUnavailable,
			"failed to check Pub/Sub subscription existence", err)
		out.IsTransient = true
		s.log.Error("pubsub: subscription existence check failed",
			"subscription", s.subscriptionID, logger.Err(out))
		return out
	}
	if !exists {
		out := pkgerrors.New(pkgerrors.CodeNotFound,
			"Pub/Sub subscription does not exist: "+s.subscriptionID)
		s.log.Error("pubsub: subscription not found", "subscription", s.subscriptionID, logger.Err(out))
		return out
	}

	sub.ReceiveSettings.MaxOutstandingMessages = s.maxOutstandingMessages

	s.log.Info("pubsub: starting receive loop",
		"subscription", s.subscriptionID,
		"max_outstanding_messages", s.maxOutstandingMessages)

	// Receive は ctx キャンセルで nil を返す（graceful shutdown）。それ以外の error は
	// 受信基盤の致命的失敗として上位へ伝播する（NFR 1.1）。
	err = sub.Receive(ctx, func(msgCtx context.Context, raw *gpubsub.Message) {
		s.handleOne(msgCtx, handler, raw)
	})
	// ctx キャンセル起因の終了（context.Canceled もしくは渡した ctx 自体の done）は
	// graceful shutdown とみなし nil を返す（requirements 6.4）。
	if err != nil && !stdErrors.Is(err, context.Canceled) && ctx.Err() == nil {
		out := pkgerrors.Wrap(pkgerrors.CodeUnavailable, "Pub/Sub receive loop failed", err)
		out.IsTransient = true
		s.log.Error("pubsub: receive loop terminated with error",
			"subscription", s.subscriptionID, logger.Err(out))
		return out
	}
	s.log.Info("pubsub: receive loop stopped", "subscription", s.subscriptionID)
	return nil
}

// handleOne は受信メッセージ 1 件を handler に渡し、結果に応じて ack / nack する。
//
//   - メッセージ ID を message_id field で受信ログに記録する（requirements 2.3）
//   - handler 成功 → ack、エラー → errors.ShouldAck で ack / nack を判定（requirements 3.x）
//   - ack / nack の結果と message_id を構造化ログに記録する（requirements 3.5 / NFR 1.2）
func (s *Subscriber) handleOne(ctx context.Context, handler MessageHandler, raw *gpubsub.Message) {
	s.log.Info("pubsub: message received", logger.MessageID(raw.ID))

	// 空 payload も破棄せず handler に渡す（requirements 2.5）。toMessage は Data をそのまま運ぶ。
	msg := toMessage(raw)
	handleErr := handler.Handle(ctx, msg)

	// ShouldAck は既存 worker_mapping を再利用（requirements 3.2）。WARN / ERROR ログは
	// ShouldAck 内部で出力されるため、ここでは ack / nack 結果のみを追加記録する。
	if pkgerrors.ShouldAck(handleErr, s.log) {
		raw.Ack()
		s.log.Info("pubsub: message acked", logger.MessageID(raw.ID))
		return
	}
	raw.Nack()
	s.log.Warn("pubsub: message nacked (will be redelivered)", logger.MessageID(raw.ID))
}

// PublishToDeadLetter は処理不能メッセージを設定された dead-letter topic に送出する
// 抽象 IF（requirements 5.1 / 5.2）。
//
// dead-letter topic 名が未設定（空文字）の場合は *pkgerrors.Error{Code: CodeConfigInvalid} を
// 返し、メッセージを ack しない（requirements 5.4。送出先が無いまま喪失することを防ぐ）。
// 送出成功時は message_id 付きで構造化ログを記録する（requirements 5.3）。送出失敗時は
// 構造化エラーを返す（呼び出し側で当該メッセージを ack しないこと / requirements 5.4）。
func (s *Subscriber) PublishToDeadLetter(ctx context.Context, msg *Message) error {
	if msg == nil {
		return pkgerrors.New(pkgerrors.CodeInvalidRequest, "message is required for dead-letter publish")
	}
	if s.deadLetterTopic == "" {
		out := pkgerrors.New(pkgerrors.CodeConfigInvalid,
			"dead-letter topic is not configured (PUBSUB_DEAD_LETTER_TOPIC); message not acked")
		s.log.Error("pubsub: dead-letter publish skipped (topic not configured)",
			logger.MessageID(msg.ID), logger.Err(out))
		return out
	}

	topic := s.client.raw.Topic(s.deadLetterTopic)
	defer topic.Stop()

	// 元メッセージの payload と識別子（attribute として原 message_id を保持）を publish する
	// （requirements 5.2）。
	attrs := cloneAttributes(msg.Attributes)
	attrs["original_message_id"] = msg.ID

	result := topic.Publish(ctx, &gpubsub.Message{
		Data:       msg.Data,
		Attributes: attrs,
	})
	serverID, err := result.Get(ctx)
	if err != nil {
		out := pkgerrors.Wrap(pkgerrors.CodeUnavailable, "failed to publish to dead-letter topic", err)
		out.IsTransient = true
		s.log.Error("pubsub: dead-letter publish failed",
			"dead_letter_topic", s.deadLetterTopic, logger.MessageID(msg.ID), logger.Err(out))
		return out
	}

	s.log.Info("pubsub: message published to dead-letter topic",
		"dead_letter_topic", s.deadLetterTopic,
		"dead_letter_message_id", serverID,
		logger.MessageID(msg.ID))
	return nil
}

// toMessage は SDK の *pubsub.Message を本パッケージの最小表現 Message へ変換する。
// 空 payload（Data が nil / 長さ 0）もそのまま運び、handler 側で破棄判定を委ねる（requirements 2.5）。
func toMessage(raw *gpubsub.Message) *Message {
	return &Message{
		ID:          raw.ID,
		Data:        raw.Data,
		Attributes:  raw.Attributes,
		PublishTime: raw.PublishTime,
	}
}

// cloneAttributes は attributes map を shallow copy する（nil 安全）。
// 元メッセージの map を直接変更しないために使う。
func cloneAttributes(src map[string]string) map[string]string {
	dst := make(map[string]string, len(src)+1)
	for k, v := range src {
		dst[k] = v
	}
	return dst
}
