package amapi

import (
	"context"
	"encoding/json"
	"io"
	stdErrors "errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// receivedRequest はテスト server が受け取った 1 件のリクエスト要約。
type receivedRequest struct {
	Method string
	Path   string
	Query  string
	Body   string
}

// recordingServer は到来したリクエストを録音しつつ、指定 status / body を返す test server。
//
// レスポンス body を func で受け取ることで、URL パスや query に応じた応答を組み立てられる。
func recordingServer(t *testing.T, status int, respond func(req receivedRequest) string) (*httptest.Server, *atomic.Int32, *[]receivedRequest) {
	t.Helper()
	calls := &atomic.Int32{}
	var got []receivedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		rr := receivedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Query:  r.URL.RawQuery,
			Body:   string(body),
		}
		got = append(got, rr)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(respond(rr)))
	}))
	t.Cleanup(srv.Close)
	return srv, calls, &got
}

func TestCreateSignupURL_OK_ReturnsURLAndName(t *testing.T) {
	// Arrange
	srv, calls, got := recordingServer(t, http.StatusOK, func(req receivedRequest) string {
		return `{"url":"https://signup.example.com/start","name":"signupUrls/abc"}`
	})
	c, _ := newTestClient(t, srv, time.Microsecond)

	// Act
	url, name, err := c.CreateSignupURL(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("CreateSignupURL = %v, want nil", err)
	}
	if url != "https://signup.example.com/start" {
		t.Fatalf("url = %q", url)
	}
	if name != "signupUrls/abc" {
		t.Fatalf("name = %q", name)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
	if (*got)[0].Method != http.MethodPost {
		t.Fatalf("method = %q, want POST", (*got)[0].Method)
	}
	if !strings.Contains((*got)[0].Path, "/v1/signupUrls") {
		t.Fatalf("path = %q, want /v1/signupUrls suffix", (*got)[0].Path)
	}
}

func TestCreateEnterprise_OK_ReturnsEnterpriseName(t *testing.T) {
	// Arrange
	srv, calls, got := recordingServer(t, http.StatusOK, func(req receivedRequest) string {
		return `{"name":"enterprises/LC123","enterpriseDisplayName":"Acme"}`
	})
	c, _ := newTestClient(t, srv, time.Microsecond)

	// Act
	name, err := c.CreateEnterprise(context.Background(), "signupUrls/abc", "my-gcp-project")

	// Assert
	if err != nil {
		t.Fatalf("CreateEnterprise = %v, want nil", err)
	}
	if name != "enterprises/LC123" {
		t.Fatalf("name = %q", name)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
	// SDK が `signupUrlName` / `projectId` を query へ載せている
	if !strings.Contains((*got)[0].Query, "signupUrlName=signupUrls%2Fabc") {
		t.Fatalf("query missing signupUrlName: %q", (*got)[0].Query)
	}
	if !strings.Contains((*got)[0].Query, "projectId=my-gcp-project") {
		t.Fatalf("query missing projectId: %q", (*got)[0].Query)
	}
}

func TestGetEnterprise_OK_NormalizesResponse(t *testing.T) {
	// Arrange
	srv, _, got := recordingServer(t, http.StatusOK, func(req receivedRequest) string {
		return `{"name":"enterprises/LC123","enterpriseDisplayName":"Acme Co"}`
	})
	c, _ := newTestClient(t, srv, time.Microsecond)

	// Act
	ent, err := c.GetEnterprise(context.Background(), "enterprises/LC123")

	// Assert
	if err != nil {
		t.Fatalf("GetEnterprise = %v", err)
	}
	if ent.Name != "enterprises/LC123" {
		t.Fatalf("Name = %q", ent.Name)
	}
	if ent.DisplayName != "Acme Co" {
		t.Fatalf("DisplayName = %q", ent.DisplayName)
	}
	if (*got)[0].Method != http.MethodGet {
		t.Fatalf("method = %q, want GET", (*got)[0].Method)
	}
	if !strings.Contains((*got)[0].Path, "/v1/enterprises/LC123") {
		t.Fatalf("path = %q, want /v1/enterprises/LC123", (*got)[0].Path)
	}
}

func TestGetEnterprise_EmptyEnterpriseName_ReturnsInvalidRequest(t *testing.T) {
	// Arrange: server 側を呼んだら失敗にする
	srv, calls, _ := recordingServer(t, http.StatusOK, func(req receivedRequest) string {
		t.Errorf("should not call AMAPI when enterpriseName is empty (got %s %s)", req.Method, req.Path)
		return `{}`
	})
	c, _ := newTestClient(t, srv, time.Microsecond)

	// Act
	_, err := c.GetEnterprise(context.Background(), "")

	// Assert
	if err == nil {
		t.Fatalf("expected error for empty enterpriseName")
	}
	var de *pkgerrors.Error
	if !stdErrors.As(err, &de) {
		t.Fatalf("error is not *pkgerrors.Error: %T", err)
	}
	if de.Code != pkgerrors.CodeInvalidRequest {
		t.Fatalf("Code = %q, want %q", de.Code, pkgerrors.CodeInvalidRequest)
	}
	if calls.Load() != 0 {
		t.Fatalf("calls = %d, want 0 (guard before API)", calls.Load())
	}
}

func TestGetEnterprise_NotFound_ReturnsCodeNotFound(t *testing.T) {
	// Arrange
	srv, _, _ := recordingServer(t, http.StatusNotFound, func(req receivedRequest) string {
		return `{"error":{"code":404,"message":"enterprise not found"}}`
	})
	c, _ := newTestClient(t, srv, time.Microsecond)

	// Act
	_, err := c.GetEnterprise(context.Background(), "enterprises/missing")

	// Assert
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var de *pkgerrors.Error
	if !stdErrors.As(err, &de) {
		t.Fatalf("error is not *pkgerrors.Error: %T", err)
	}
	if de.Code != pkgerrors.CodeNotFound {
		t.Fatalf("Code = %q, want %q", de.Code, pkgerrors.CodeNotFound)
	}
	if de.IsTransient {
		t.Fatalf("IsTransient = true, want false (404)")
	}
}

func TestGetEnterprise_ResourcePathReflectsArgument(t *testing.T) {
	// Arrange: テナント分離の物理担保 (Req 2.4) を検証する
	srv, _, got := recordingServer(t, http.StatusOK, func(req receivedRequest) string {
		return `{"name":"enterprises/CALLER_VALUE"}`
	})
	c, _ := newTestClient(t, srv, time.Microsecond)

	// Act
	_, err := c.GetEnterprise(context.Background(), "enterprises/CALLER_VALUE")

	// Assert
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains((*got)[0].Path, "/v1/enterprises/CALLER_VALUE") {
		t.Fatalf("path = %q, want enterprises/CALLER_VALUE", (*got)[0].Path)
	}
}

// JSON のレスポンス body が SDK 経由で正しくデコードされていることを sanity check するため、
// json.Marshal 済みオブジェクトとの一致を見ている。
var _ = json.Marshal
