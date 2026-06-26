package amapi

import (
	"context"
	stdErrors "errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/api/googleapi"

	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
)

// ---- ヘルパ ----

// fakeLogger は logger.Logger interface を満たすテスト用 spy。
// 各メソッドの呼出回数を atomic counter で記録する。`With` は自身を返す。
type fakeLogger struct {
	debugCalls atomic.Int64
	infoCalls  atomic.Int64
	warnCalls  atomic.Int64
	errorCalls atomic.Int64
}

func (l *fakeLogger) Debug(msg string, fields ...any) { l.debugCalls.Add(1) }
func (l *fakeLogger) Info(msg string, fields ...any)  { l.infoCalls.Add(1) }
func (l *fakeLogger) Warn(msg string, fields ...any)  { l.warnCalls.Add(1) }
func (l *fakeLogger) Error(msg string, fields ...any) { l.errorCalls.Add(1) }
func (l *fakeLogger) With(fields ...any) logger.Logger { return l }
func (l *fakeLogger) Sync() error                      { return nil }

// endpointRewriter は SDK が `https://androidmanagement.googleapis.com/...` へ送るリクエストを
// httptest.Server の URL に書き換える RoundTripper。AMAPI SDK は内部で固定 BasePath を持つため、
// テスト時はこの層でリクエスト URL を書き換える必要がある。
type endpointRewriter struct {
	base    string
	wrapped http.RoundTripper
}

func (e *endpointRewriter) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := req.URL.Parse(e.base + req.URL.Path)
	if err != nil {
		return nil, err
	}
	u.RawQuery = req.URL.RawQuery
	req.URL = u
	req.Host = u.Host
	if e.wrapped != nil {
		return e.wrapped.RoundTrip(req)
	}
	return http.DefaultTransport.RoundTrip(req)
}

// newTestClient は httptest.Server を AMAPI backend として注入した realClient を構築する。
//
// Sleep を即時化することで exponential backoff のテストを決定論かつ高速にする。
func newTestClient(t *testing.T, server *httptest.Server, baseBackoff time.Duration) (*realClient, *fakeLogger) {
	t.Helper()
	log := &fakeLogger{}
	cfg := config.Config{
		GoogleApplicationCredentials: "test-credentials.json",
	}
	httpClient := &http.Client{Transport: &endpointRewriter{
		base:    server.URL,
		wrapped: server.Client().Transport,
	}}
	c, err := NewClient(context.Background(), cfg, log, &Options{
		HTTPClient:  httpClient,
		MaxRetries:  defaultMaxRetries,
		BaseBackoff: baseBackoff,
		Now:         time.Now,
		Sleep: func(ctx context.Context, d time.Duration) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
				return nil
			}
		},
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}
	return c.(*realClient), log
}

// ---- NewClient テスト ----

func TestNewClient_GoogleApplicationCredentialsEmpty_ReturnsConfigInvalid(t *testing.T) {
	// Arrange
	cfg := config.Config{GoogleApplicationCredentials: ""}

	// Act
	_, err := NewClient(context.Background(), cfg, &fakeLogger{}, nil)

	// Assert
	if err == nil {
		t.Fatalf("NewClient(empty credentials) = nil error, want error")
	}
	var de *pkgerrors.Error
	if !stdErrors.As(err, &de) {
		t.Fatalf("error is not *pkgerrors.Error: %T", err)
	}
	if de.Code != pkgerrors.CodeConfigInvalid {
		t.Fatalf("code = %q, want %q", de.Code, pkgerrors.CodeConfigInvalid)
	}
}

func TestNewClient_WithHTTPClient_UsesNoAuth(t *testing.T) {
	// Arrange: 任意の OK 応答を返す server
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"url":"https://signup","name":"signupUrls/abc"}`))
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv, time.Microsecond)

	// Act: 構築できれば成功
	if c == nil {
		t.Fatalf("realClient is nil")
	}
	if c.maxRetries != defaultMaxRetries {
		t.Fatalf("maxRetries = %d, want %d", c.maxRetries, defaultMaxRetries)
	}
}

// ---- requireEnterpriseName テスト ----

func TestRequireEnterpriseName_EmptyReturnsInvalidRequest(t *testing.T) {
	// Act
	err := requireEnterpriseName("")

	// Assert
	if err == nil {
		t.Fatalf("requireEnterpriseName(\"\") = nil, want error")
	}
	var de *pkgerrors.Error
	if !stdErrors.As(err, &de) {
		t.Fatalf("error is not *pkgerrors.Error: %T", err)
	}
	if de.Code != pkgerrors.CodeInvalidRequest {
		t.Fatalf("code = %q, want %q", de.Code, pkgerrors.CodeInvalidRequest)
	}
}

func TestRequireEnterpriseName_NonEmptyReturnsNil(t *testing.T) {
	if err := requireEnterpriseName("enterprises/x"); err != nil {
		t.Fatalf("requireEnterpriseName(\"enterprises/x\") = %v, want nil", err)
	}
}

// ---- mapAMAPIError table-driven test ----

func TestMapAMAPIError_GoogleAPIError_Mapping(t *testing.T) {
	cases := []struct {
		status    int
		wantCode  pkgerrors.Code
		transient bool
	}{
		{http.StatusBadRequest, pkgerrors.CodeInvalidRequest, false},
		{http.StatusUnauthorized, pkgerrors.CodeUnauthenticated, false},
		{http.StatusForbidden, pkgerrors.CodeForbidden, false},
		{http.StatusNotFound, pkgerrors.CodeNotFound, false},
		{http.StatusConflict, pkgerrors.CodeConflict, false},
		{http.StatusUnprocessableEntity, pkgerrors.CodeBusinessRule, false},
		{http.StatusTooManyRequests, pkgerrors.CodeUpstream, true},
		{http.StatusInternalServerError, pkgerrors.CodeUpstream, true},
		{http.StatusBadGateway, pkgerrors.CodeUpstream, true},
		{http.StatusServiceUnavailable, pkgerrors.CodeUpstream, true},
		{http.StatusGatewayTimeout, pkgerrors.CodeUpstream, true},
		// その他 4xx は CodeUpstream + 非 transient
		{http.StatusPaymentRequired, pkgerrors.CodeUpstream, false},
		// その他 5xx は CodeUpstream + transient
		{599, pkgerrors.CodeUpstream, true},
	}
	for _, c := range cases {
		c := c
		t.Run(fmt.Sprintf("status_%d", c.status), func(t *testing.T) {
			// Arrange
			gerr := &googleapi.Error{Code: c.status, Message: "from amapi"}

			// Act
			out := mapAMAPIError(gerr)

			// Assert
			if out == nil {
				t.Fatalf("mapAMAPIError = nil, want non-nil")
			}
			if out.Code != c.wantCode {
				t.Fatalf("Code = %q, want %q", out.Code, c.wantCode)
			}
			if out.IsTransient != c.transient {
				t.Fatalf("IsTransient = %v, want %v", out.IsTransient, c.transient)
			}
			if out.Message != "from amapi" {
				t.Fatalf("Message = %q, want %q", out.Message, "from amapi")
			}
			if !stdErrors.Is(out, gerr) {
				t.Fatalf("errors.Is(out, gerr) = false, want true")
			}
		})
	}
}

func TestMapAMAPIError_ContextCanceled_IsTransientUnavailable(t *testing.T) {
	// Act
	out := mapAMAPIError(context.Canceled)

	// Assert
	if out == nil {
		t.Fatalf("mapAMAPIError(context.Canceled) = nil")
	}
	if out.Code != pkgerrors.CodeUnavailable {
		t.Fatalf("Code = %q, want %q", out.Code, pkgerrors.CodeUnavailable)
	}
	if !out.IsTransient {
		t.Fatalf("IsTransient = false, want true")
	}
}

func TestMapAMAPIError_DeadlineExceeded_IsTransientUnavailable(t *testing.T) {
	// Act
	out := mapAMAPIError(context.DeadlineExceeded)

	// Assert
	if out.Code != pkgerrors.CodeUnavailable {
		t.Fatalf("Code = %q, want %q", out.Code, pkgerrors.CodeUnavailable)
	}
	if !out.IsTransient {
		t.Fatalf("IsTransient = false, want true")
	}
}

// fakeNetError は net.Error を満たす test fake。
type fakeNetError struct {
	msg     string
	timeout bool
}

func (f *fakeNetError) Error() string   { return f.msg }
func (f *fakeNetError) Timeout() bool   { return f.timeout }
func (f *fakeNetError) Temporary() bool { return false }

func TestMapAMAPIError_NetworkError_IsTransientUnavailable(t *testing.T) {
	// Arrange
	netErr := &fakeNetError{msg: "dial tcp: connection refused"}

	// Act
	out := mapAMAPIError(netErr)

	// Assert
	if out.Code != pkgerrors.CodeUnavailable {
		t.Fatalf("Code = %q, want %q", out.Code, pkgerrors.CodeUnavailable)
	}
	if !out.IsTransient {
		t.Fatalf("IsTransient = false, want true")
	}
}

func TestMapAMAPIError_UnknownError_FallsBackToUpstreamTransient(t *testing.T) {
	// Arrange: net.Error でも googleapi.Error でもない error
	raw := stdErrors.New("something unexpected")

	// Act
	out := mapAMAPIError(raw)

	// Assert: 保守側に倒して transient + CodeUpstream
	if out.Code != pkgerrors.CodeUpstream {
		t.Fatalf("Code = %q, want %q", out.Code, pkgerrors.CodeUpstream)
	}
	if !out.IsTransient {
		t.Fatalf("IsTransient = false, want true (保守側)")
	}
}

func TestMapAMAPIError_Nil_ReturnsNil(t *testing.T) {
	if out := mapAMAPIError(nil); out != nil {
		t.Fatalf("mapAMAPIError(nil) = %v, want nil", out)
	}
}

func TestClassifyCause_429Or5xxOrNetwork(t *testing.T) {
	cases := []struct {
		name   string
		input  error
		expect string
	}{
		{"429", &googleapi.Error{Code: 429}, "429"},
		{"500", &googleapi.Error{Code: 500}, "5xx"},
		{"503", &googleapi.Error{Code: 503}, "5xx"},
		{"network", stdErrors.New("dial tcp: refused"), "network"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := classifyCause(c.input)
			if got != c.expect {
				t.Fatalf("classifyCause(%v) = %q, want %q", c.input, got, c.expect)
			}
		})
	}
}

// ---- Retry behaviour（httptest.Server を使った integration） ----

// retryServer は最初の N 回 status を返し、N+1 回目から OK を返すテスト用 server。
// 戻り値の atomic.Int32 から実際の呼び出し回数を観測できる。
func retryServer(t *testing.T, failures int, failStatus int, okBody string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	calls := &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if int(n) <= failures {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(failStatus)
			_, _ = w.Write([]byte(fmt.Sprintf(`{"error":{"code":%d,"message":"transient"}}`, failStatus)))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(okBody))
	}))
	t.Cleanup(srv.Close)
	return srv, calls
}

func TestDoWithRetry_RecoversAfter429(t *testing.T) {
	// Arrange: 2 回 429 を返した後で OK
	srv, calls := retryServer(t, 2, http.StatusTooManyRequests, `{"url":"https://signup","name":"signupUrls/abc"}`)
	c, log := newTestClient(t, srv, time.Microsecond)

	// Act
	url, name, err := c.CreateSignupURL(context.Background())

	// Assert: 3 回目で成功（再試行 = 2 回）
	if err != nil {
		t.Fatalf("CreateSignupURL after 2x 429 = %v, want nil", err)
	}
	if url != "https://signup" || name != "signupUrls/abc" {
		t.Fatalf("got url=%q name=%q", url, name)
	}
	if calls.Load() != 3 {
		t.Fatalf("server received %d calls, want 3", calls.Load())
	}
	// 再試行の Warn ログが 2 回以上
	if log.warnCalls.Load() < 2 {
		t.Fatalf("warn calls = %d, want >= 2 (retry warnings)", log.warnCalls.Load())
	}
}

func TestDoWithRetry_GivesUpAfter4Attempts(t *testing.T) {
	// Arrange: 常に 503 を返す server。defaultMaxRetries=3 + 初回 = 4 試行
	srv, calls := retryServer(t, 100, http.StatusServiceUnavailable, `{}`)
	c, _ := newTestClient(t, srv, time.Microsecond)

	// Act
	_, _, err := c.CreateSignupURL(context.Background())

	// Assert: 4 試行（= 初回 + 3 再試行）で失敗、再試行可能エラー
	if err == nil {
		t.Fatalf("expected error after retry exhaustion, got nil")
	}
	var de *pkgerrors.Error
	if !stdErrors.As(err, &de) {
		t.Fatalf("error is not *pkgerrors.Error: %T", err)
	}
	if de.Code != pkgerrors.CodeUpstream {
		t.Fatalf("Code = %q, want %q", de.Code, pkgerrors.CodeUpstream)
	}
	if !de.IsTransient {
		t.Fatalf("IsTransient = false, want true (再試行枯渇は呼び出し側に再試行可能として通知)")
	}
	if calls.Load() != int32(defaultMaxRetries+1) {
		t.Fatalf("server received %d calls, want %d", calls.Load(), defaultMaxRetries+1)
	}
}

func TestDoWithRetry_DoesNotRetryOn4xx(t *testing.T) {
	// Arrange: 403 を 1 回返す
	srv, calls := retryServer(t, 100, http.StatusForbidden, `{}`)
	c, _ := newTestClient(t, srv, time.Microsecond)

	// Act
	_, _, err := c.CreateSignupURL(context.Background())

	// Assert: 即時失敗（再試行なし）
	if err == nil {
		t.Fatalf("expected error for 403, got nil")
	}
	var de *pkgerrors.Error
	if !stdErrors.As(err, &de) {
		t.Fatalf("error is not *pkgerrors.Error: %T", err)
	}
	if de.Code != pkgerrors.CodeForbidden {
		t.Fatalf("Code = %q, want %q", de.Code, pkgerrors.CodeForbidden)
	}
	if de.IsTransient {
		t.Fatalf("IsTransient = true, want false (4xx は再試行不可)")
	}
	if calls.Load() != 1 {
		t.Fatalf("server received %d calls, want 1 (no retries for 4xx)", calls.Load())
	}
}

func TestDoWithRetry_RespectsContextCancel(t *testing.T) {
	// Arrange: 常に 503。Sleep を context cancel する Options に差し替え
	srv, calls := retryServer(t, 100, http.StatusServiceUnavailable, `{}`)
	httpClient := &http.Client{Transport: &endpointRewriter{
		base:    srv.URL,
		wrapped: srv.Client().Transport,
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cfg := config.Config{GoogleApplicationCredentials: "x.json"}
	cli, err := NewClient(context.Background(), cfg, &fakeLogger{}, &Options{
		HTTPClient:  httpClient,
		MaxRetries:  defaultMaxRetries,
		BaseBackoff: time.Second,
		Sleep: func(ctx context.Context, d time.Duration) error {
			cancel()
			return ctx.Err()
		},
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// Act
	_, _, err = cli.CreateSignupURL(ctx)

	// Assert
	if err == nil {
		t.Fatalf("expected error after cancel, got nil")
	}
	var de *pkgerrors.Error
	if !stdErrors.As(err, &de) {
		t.Fatalf("error is not *pkgerrors.Error: %T", err)
	}
	if de.Code != pkgerrors.CodeUnavailable {
		t.Fatalf("Code = %q, want %q", de.Code, pkgerrors.CodeUnavailable)
	}
	if calls.Load() != 1 {
		t.Fatalf("server received %d calls, want 1 (cancel during backoff)", calls.Load())
	}
}

func TestDoWithRetry_NetworkError_IsRetriedThenExhausted(t *testing.T) {
	// Arrange: server を建てた直後に閉じることで connection refused を発生させる
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	addr := srv.Listener.Addr().String()
	srv.Close()
	conn, dialErr := net.DialTimeout("tcp", addr, 100*time.Millisecond)
	if dialErr == nil {
		_ = conn.Close()
		t.Skip("server unexpectedly still reachable, skipping network failure test")
	}

	cfg := config.Config{GoogleApplicationCredentials: "x.json"}
	httpClient := &http.Client{
		Transport: &endpointRewriter{base: "http://" + addr},
		Timeout:   500 * time.Millisecond,
	}
	cli, err := NewClient(context.Background(), cfg, &fakeLogger{}, &Options{
		HTTPClient:  httpClient,
		MaxRetries:  1,
		BaseBackoff: time.Microsecond,
		Sleep: func(ctx context.Context, d time.Duration) error {
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewClient failed: %v", err)
	}

	// Act
	_, _, err = cli.CreateSignupURL(context.Background())

	// Assert: network 失敗 = transient + CodeUnavailable
	if err == nil {
		t.Fatalf("expected error for unreachable backend, got nil")
	}
	var de *pkgerrors.Error
	if !stdErrors.As(err, &de) {
		t.Fatalf("error is not *pkgerrors.Error: %T", err)
	}
	if de.Code != pkgerrors.CodeUnavailable {
		t.Fatalf("Code = %q, want %q", de.Code, pkgerrors.CodeUnavailable)
	}
	if !de.IsTransient {
		t.Fatalf("IsTransient = false, want true")
	}
}

// ---- contextAwareSleep ----

func TestContextAwareSleep_RespectsCancel(t *testing.T) {
	// Arrange
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Act
	err := contextAwareSleep(ctx, 10*time.Second)

	// Assert: 即時 cancel
	if err == nil {
		t.Fatalf("expected ctx.Err(), got nil")
	}
}

func TestContextAwareSleep_ZeroDuration_NoSleep(t *testing.T) {
	// Act
	start := time.Now()
	err := contextAwareSleep(context.Background(), 0)
	dur := time.Since(start)

	// Assert
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if dur > 100*time.Millisecond {
		t.Fatalf("duration = %v, want immediate", dur)
	}
}
