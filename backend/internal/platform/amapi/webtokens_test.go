package amapi

import (
	"context"
	stdErrors "errors"
	"net/http"
	"strings"
	"testing"
	"time"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

func TestCreateWebToken_OK_ReturnsValue(t *testing.T) {
	// Arrange
	srv, _, got := recordingServer(t, http.StatusOK, func(req receivedRequest) string {
		return `{"name":"enterprises/X/webTokens/w1","value":"super-secret-webtoken"}`
	})
	c, _ := newTestClient(t, srv, time.Microsecond)

	// Act
	w, err := c.CreateWebToken(context.Background(), "enterprises/X", "https://admin.example.com")

	// Assert
	if err != nil {
		t.Fatalf("CreateWebToken = %v", err)
	}
	if w.Name != "enterprises/X/webTokens/w1" {
		t.Fatalf("Name = %q", w.Name)
	}
	if w.Value != "super-secret-webtoken" {
		t.Fatalf("Value = %q", w.Value)
	}
	if !strings.Contains((*got)[0].Path, "/v1/enterprises/X/webTokens") {
		t.Fatalf("path = %q", (*got)[0].Path)
	}
	if !strings.Contains((*got)[0].Body, `"parentFrameUrl":"https://admin.example.com"`) {
		t.Fatalf("body should include parentFrameUrl: %q", (*got)[0].Body)
	}
}

func TestCreateWebToken_EmptyEnterpriseName_ReturnsInvalidRequest(t *testing.T) {
	// Arrange
	srv, calls, _ := recordingServer(t, http.StatusOK, func(req receivedRequest) string {
		t.Errorf("should not call AMAPI")
		return `{}`
	})
	c, _ := newTestClient(t, srv, time.Microsecond)

	// Act
	_, err := c.CreateWebToken(context.Background(), "", "https://admin")

	// Assert
	if err == nil {
		t.Fatalf("expected error")
	}
	var de *pkgerrors.Error
	if !stdErrors.As(err, &de) || de.Code != pkgerrors.CodeInvalidRequest {
		t.Fatalf("want CodeInvalidRequest, got %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("calls = %d, want 0", calls.Load())
	}
}

func TestCreateWebToken_EmptyParentFrameURL_ReturnsInvalidRequest(t *testing.T) {
	// Arrange
	srv, calls, _ := recordingServer(t, http.StatusOK, func(req receivedRequest) string {
		t.Errorf("should not call AMAPI")
		return `{}`
	})
	c, _ := newTestClient(t, srv, time.Microsecond)

	// Act
	_, err := c.CreateWebToken(context.Background(), "enterprises/X", "")

	// Assert
	if err == nil {
		t.Fatalf("expected error")
	}
	var de *pkgerrors.Error
	if !stdErrors.As(err, &de) || de.Code != pkgerrors.CodeInvalidRequest {
		t.Fatalf("want CodeInvalidRequest, got %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("calls = %d, want 0", calls.Load())
	}
}
