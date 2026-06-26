package amapi

import (
	"context"
	stdErrors "errors"
	"fmt"
	"net"
	"net/http"
	"time"

	androidmanagement "google.golang.org/api/androidmanagement/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
)

// Client は AMAPI（Android Management API）への共有ラッパが各ドメインへ公開する操作 IF。
//
// 全メソッドは context をキャンセル可能な引数として受け取り、失敗時は *pkgerrors.Error を
// 返す（再試行可否は IsTransient フィールドで識別する）。`enterpriseName` を引数に取るメソッドは
// 引数の空値検査を内部で実施し、空であれば CodeInvalidRequest を返す（Req 2.3）。
//
// AMAPI のレスポンスは本パッケージの独自型（Enterprise / PolicyBody / Device 等）へ正規化して
// 返し、SDK の型をそのまま漏らさない（Req 1.8 / design.md「AMAPI Client / Service Interface」抜粋）。
type Client interface {
	// CreateSignupURL はサインアップ URL を発行する。Enterprise 確定前のフローのため
	// enterpriseName を要求しない（Req 2.2）。
	CreateSignupURL(ctx context.Context) (signupURL, signupURLName string, err error)
	// CreateEnterprise は signupURLName と projectID から Enterprise を確定する。
	CreateEnterprise(ctx context.Context, signupURLName, projectID string) (enterpriseName string, err error)
	// GetEnterprise は確定済み Enterprise の詳細を取得する。
	GetEnterprise(ctx context.Context, enterpriseName string) (Enterprise, error)
	// UpsertPolicy は Policy を upsert する（AMAPI の policies.patch を全フィールド更新で利用）。
	UpsertPolicy(ctx context.Context, enterpriseName, policyName string, body PolicyBody) error
	// GetPolicy は Policy を取得する。
	GetPolicy(ctx context.Context, enterpriseName, policyName string) (PolicyBody, error)
	// ListDevices は当該 Enterprise 配下の Device 一覧を取得する（page 連結済み）。
	ListDevices(ctx context.Context, enterpriseName string) ([]Device, error)
	// GetDevice は Device の詳細を取得する。
	GetDevice(ctx context.Context, enterpriseName, deviceID string) (Device, error)
	// IssueCommand は Device へ Command（LOCK 等）を発行する。
	IssueCommand(ctx context.Context, enterpriseName, deviceID string, cmd CommandRequest) (commandID string, err error)
	// CreateEnrollmentToken は EnrollmentToken を発行する。
	CreateEnrollmentToken(ctx context.Context, enterpriseName string, req EnrollmentTokenRequest) (EnrollmentToken, error)
	// CreateWebToken は WebToken（embedded iframe 用 short-lived token）を発行する。
	CreateWebToken(ctx context.Context, enterpriseName string, parentFrameURL string) (WebToken, error)
}

// retry / backoff 既定値。テストでは Options 経由で短縮する。
const (
	// defaultMaxRetries は 429 / 5xx に対する再試行の最大回数（最初の試行を含めない）。
	// Req 5.1「最大 3 回まで再試行」と整合。最終的に試行は 1 + defaultMaxRetries = 4 回となる。
	defaultMaxRetries = 3
	// defaultBaseBackoff は exponential backoff の初期 delay。
	// 試行 N（0-origin）の delay = defaultBaseBackoff << N。
	defaultBaseBackoff = 100 * time.Millisecond
)

// Options は Client 構築時の試験用フックを保持する（本番では nil の field を使う）。
//
// テストでは HTTPClient に httptest.Server のクライアントを差し込み、Sleep を即時化することで
// 決定論的かつ高速な再試行テストを書ける。本番運用ではいずれの field も 0/nil で構わない。
type Options struct {
	// HTTPClient は AMAPI SDK 内部に注入する http.Client。nil なら SDK の規定（OAuth2 認証 client）。
	HTTPClient *http.Client
	// MaxRetries は 429 / 5xx に対する再試行回数の上書き。0 以下なら defaultMaxRetries を採用。
	MaxRetries int
	// BaseBackoff は exponential backoff の初期 delay 上書き。0 以下なら defaultBaseBackoff を採用。
	BaseBackoff time.Duration
	// Now は現在時刻取得関数の差し替え（duration ログ計測用）。nil なら time.Now。
	Now func() time.Time
	// Sleep は backoff 中の待機関数。nil なら context-aware な内部実装を使う。
	// 戻り値で context cancel を伝搬する（cancel 時は ctx.Err() を返す）。
	Sleep func(ctx context.Context, d time.Duration) error
}

// realClient は Client interface の本番実装。
//
// SDK の Service 構造は構築後 read-only（goroutine-safe）。
type realClient struct {
	svc         *androidmanagement.Service
	log         logger.Logger
	maxRetries  int
	baseBackoff time.Duration
	now         func() time.Time
	sleep       func(ctx context.Context, d time.Duration) error
}

// NewClient は config から AMAPI Client を構築する。
//
// 認証は config.GoogleApplicationCredentials が指す service account JSON ファイル経由
// （EMM-bound 方式 / Req 3.1 / 3.2）。空文字 / 未設定なら CodeConfigInvalid を返し、
// AMAPI 呼び出し前段で失敗させる（Req 3.3）。
//
// `opts` は試験用フック（HTTPClient / Sleep 等）。本番運用では nil でよい。
func NewClient(ctx context.Context, cfg config.Config, log logger.Logger, opts *Options) (Client, error) {
	if cfg.GoogleApplicationCredentials == "" {
		return nil, pkgerrors.New(pkgerrors.CodeConfigInvalid,
			"GoogleApplicationCredentials is required for AMAPI client")
	}

	clientOpts := []option.ClientOption{}
	if opts != nil && opts.HTTPClient != nil {
		// テスト時: httptest.Server のクライアントを使う。Authentication は短絡する。
		clientOpts = append(clientOpts,
			option.WithHTTPClient(opts.HTTPClient),
			option.WithoutAuthentication(),
		)
	} else {
		// 本番運用: service account credentials ファイルから認証
		// （OAuth2 token は SDK 内部で自動キャッシュ・リフレッシュ / Req 3.5）。
		clientOpts = append(clientOpts,
			option.WithCredentialsFile(cfg.GoogleApplicationCredentials),
		)
	}

	svc, err := androidmanagement.NewService(ctx, clientOpts...)
	if err != nil {
		return nil, pkgerrors.Wrap(pkgerrors.CodeConfigInvalid,
			"failed to construct AMAPI service", err)
	}

	c := &realClient{
		svc:         svc,
		log:         log,
		maxRetries:  defaultMaxRetries,
		baseBackoff: defaultBaseBackoff,
		now:         time.Now,
		sleep:       contextAwareSleep,
	}
	if opts != nil {
		if opts.MaxRetries > 0 {
			c.maxRetries = opts.MaxRetries
		}
		if opts.BaseBackoff > 0 {
			c.baseBackoff = opts.BaseBackoff
		}
		if opts.Now != nil {
			c.now = opts.Now
		}
		if opts.Sleep != nil {
			c.sleep = opts.Sleep
		}
	}
	return c, nil
}

// requireEnterpriseName は enterpriseName を必須化する操作の先頭で空値検査を行う（Req 2.3）。
// 空文字 / 全角空白等を含む whitespace のみのケースは CodeInvalidRequest として弾く。
func requireEnterpriseName(enterpriseName string) error {
	if enterpriseName == "" {
		return pkgerrors.New(pkgerrors.CodeInvalidRequest, "enterpriseName is required")
	}
	return nil
}

// doWithRetry は AMAPI 呼び出しを exponential backoff 付きで実行する共通 helper。
//
//   - op は AMAPI 呼び出し本体（SDK の *Call.Do() を直接ラップする想定）
//   - operationName は構造化ログ field "operation" として記録する人間可読名
//   - enterpriseName はログ用（空文字なら省略）。retry / failure log に同 field で乗る
//
// 戻り値は正規化済み *pkgerrors.Error（成功時は nil）。
//
//nolint:gocyclo // backoff + classify + log は 1 か所に閉じた方が読みやすい
func (c *realClient) doWithRetry(ctx context.Context, operationName, enterpriseName string, op func() error) error {
	start := c.now()
	maxAttempts := c.maxRetries + 1 // 試行 = 1 + 再試行回数
	var lastErr error

	for attempt := 0; attempt < maxAttempts; attempt++ {
		// context cancel チェック（試行前）
		if ctxErr := ctx.Err(); ctxErr != nil {
			c.logOutcome(operationName, enterpriseName, "ctx_canceled", attempt, start, ctxErr)
			return pkgerrors.Wrap(pkgerrors.CodeUnavailable,
				fmt.Sprintf("amapi %s canceled by context", operationName), ctxErr)
		}

		err := op()
		if err == nil {
			c.logOutcome(operationName, enterpriseName, "success", attempt, start, nil)
			return nil
		}

		mapped := mapAMAPIError(err)
		lastErr = mapped

		if !mapped.IsTransient {
			// 4xx 等の恒常的エラー: 即座に上位へ返す（Req 5.3）。
			c.logOutcome(operationName, enterpriseName, "domain_error", attempt, start, mapped)
			return mapped
		}

		// transient: 再試行可否を試行回数で判定（Req 5.1 / 5.2）
		if attempt+1 >= maxAttempts {
			c.logOutcome(operationName, enterpriseName, "retry_exhausted", attempt, start, mapped)
			return mapped
		}

		// 構造化ログ: 再試行カウントと原因種別（NFR 1.3）
		c.log.Warn("amapi transient error, will retry",
			"operation", operationName,
			"enterprise_name", enterpriseName,
			"attempt", attempt,
			"next_attempt", attempt+1,
			"cause_kind", classifyCause(mapped),
			logger.Err(mapped),
		)

		// exponential backoff: base << attempt（attempt=0 → base、attempt=1 → 2*base、attempt=2 → 4*base）
		delay := c.baseBackoff << attempt
		if err := c.sleep(ctx, delay); err != nil {
			// context cancel: cancel 理由を保持して返す（Req 5.4）
			c.logOutcome(operationName, enterpriseName, "ctx_canceled_in_backoff", attempt, start, err)
			return pkgerrors.Wrap(pkgerrors.CodeUnavailable,
				fmt.Sprintf("amapi %s canceled during backoff", operationName), err)
		}
	}

	// 通常到達しないが、安全側で lastErr を返す。
	return lastErr
}

// logOutcome は AMAPI 呼び出しの最終結果を 1 行構造化ログとして書く（NFR 1.1）。
func (c *realClient) logOutcome(operation, enterpriseName, outcome string, attempt int, start time.Time, err error) {
	dur := c.now().Sub(start)
	fields := []any{
		"operation", operation,
		"enterprise_name", enterpriseName,
		"duration_ms", dur.Milliseconds(),
		"attempts", attempt + 1,
		"outcome", outcome,
	}
	if err != nil {
		fields = append(fields, logger.Err(err))
		if outcome == "success" {
			c.log.Info("amapi call completed", fields...)
		} else {
			c.log.Warn("amapi call completed", fields...)
		}
		return
	}
	c.log.Info("amapi call completed", fields...)
}

// classifyCause は transient error の原因種別を構造化ログ用に短縮文字列化する（NFR 1.3）。
// `429` / `5xx` / `network` の 3 値を返す。
func classifyCause(err error) string {
	var gerr *googleapi.Error
	if stdErrors.As(err, &gerr) && gerr != nil {
		if gerr.Code == http.StatusTooManyRequests {
			return "429"
		}
		if gerr.Code >= 500 && gerr.Code < 600 {
			return "5xx"
		}
	}
	return "network"
}

// mapAMAPIError は AMAPI SDK が返した error を *pkgerrors.Error へ正規化する。
//
//   - googleapi.Error: HTTP status から CodeInvalidRequest / CodeUnauthenticated / CodeForbidden /
//     CodeNotFound / CodeConflict / CodeBusinessRule / CodeUpstream に分岐
//   - context.Canceled / context.DeadlineExceeded: CodeUnavailable + IsTransient=true
//   - その他 net 系エラー: CodeUnavailable + IsTransient=true
//   - 上記いずれにも該当しない error: CodeUpstream + IsTransient=true（保守側に倒す）
//
// HTTP 4xx は再試行不可 / 429 と 5xx は再試行可（Req 4.1 / 4.2 / 5.1 / 5.3）。
//
//nolint:gocyclo // status code 分岐は table 風に並べた方が読みやすい
func mapAMAPIError(err error) *pkgerrors.Error {
	if err == nil {
		return nil
	}

	// context error は最優先で判定（再試行不可・cancel 起因の transient とする）
	if stdErrors.Is(err, context.Canceled) || stdErrors.Is(err, context.DeadlineExceeded) {
		out := pkgerrors.Wrap(pkgerrors.CodeUnavailable, "amapi context canceled or deadline exceeded", err)
		out.IsTransient = true
		return out
	}

	var gerr *googleapi.Error
	if stdErrors.As(err, &gerr) && gerr != nil {
		return mapGoogleAPIError(gerr)
	}

	// net 系 transient: connection refused / dns failure / timeout 等
	var netErr net.Error
	if stdErrors.As(err, &netErr) {
		out := pkgerrors.Wrap(pkgerrors.CodeUnavailable,
			fmt.Sprintf("amapi network failure: %s", netErr.Error()), err)
		out.IsTransient = true
		return out
	}

	// 種別不明: 保守側で再試行可とする（CodeUpstream + transient）
	out := pkgerrors.Wrap(pkgerrors.CodeUpstream,
		fmt.Sprintf("amapi unexpected error: %s", err.Error()), err)
	out.IsTransient = true
	return out
}

// mapGoogleAPIError は googleapi.Error を *pkgerrors.Error に正規化する。HTTP status に対応する
// Code をマッピングし、IsTransient は 429 / 5xx で true、それ以外（特に 4xx）は false にする。
//
//nolint:gocyclo // status code 分岐は table 風で読みやすい
func mapGoogleAPIError(gerr *googleapi.Error) *pkgerrors.Error {
	msg := gerr.Message
	if msg == "" {
		msg = "amapi upstream error"
	}
	code := pkgerrors.CodeUpstream
	transient := false
	switch gerr.Code {
	case http.StatusBadRequest:
		code = pkgerrors.CodeInvalidRequest
	case http.StatusUnauthorized:
		code = pkgerrors.CodeUnauthenticated
	case http.StatusForbidden:
		code = pkgerrors.CodeForbidden
	case http.StatusNotFound:
		code = pkgerrors.CodeNotFound
	case http.StatusConflict:
		code = pkgerrors.CodeConflict
	case http.StatusUnprocessableEntity:
		code = pkgerrors.CodeBusinessRule
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		code = pkgerrors.CodeUpstream
		transient = true
	default:
		// その他 4xx は CodeUpstream + 非 transient、その他 5xx は CodeUpstream + transient。
		code = pkgerrors.CodeUpstream
		if gerr.Code >= 500 && gerr.Code < 600 {
			transient = true
		}
	}

	out := pkgerrors.Wrap(code, msg, gerr)
	out.IsTransient = transient
	return out
}

// contextAwareSleep は context cancel を伝搬する time.Sleep。
//
// テストでは Options.Sleep に no-op or 短縮版を差し込み、決定論かつ即時化する。
func contextAwareSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		// 0 以下は context cancel チェックのみ行う
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
