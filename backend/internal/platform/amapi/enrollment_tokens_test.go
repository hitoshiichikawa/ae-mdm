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

func TestCreateEnrollmentToken_OK_NormalizesResponse(t *testing.T) {
	// Arrange
	srv, _, got := recordingServer(t, http.StatusOK, func(req receivedRequest) string {
		return `{
		  "name":"enterprises/X/enrollmentTokens/t1",
		  "value":"super-secret-token",
		  "qrCode":"{\"foo\":1}",
		  "expirationTimestamp":"2025-06-30T00:00:00Z"
		}`
	})
	c, _ := newTestClient(t, srv, time.Microsecond)

	// Act
	tok, err := c.CreateEnrollmentToken(context.Background(), "enterprises/X", EnrollmentTokenRequest{
		PolicyName:         "enterprises/X/policies/default",
		Duration:           "3600s",
		AdditionalData:     "ou=engineering",
		AllowPersonalUsage: "PERSONAL_USAGE_DISALLOWED",
	})

	// Assert
	if err != nil {
		t.Fatalf("CreateEnrollmentToken = %v", err)
	}
	if tok.Name != "enterprises/X/enrollmentTokens/t1" {
		t.Fatalf("Name = %q", tok.Name)
	}
	if tok.Value != "super-secret-token" {
		t.Fatalf("Value = %q", tok.Value)
	}
	if tok.QRCode == "" {
		t.Fatalf("QRCode should be propagated, got empty")
	}
	if tok.ExpirationTime != "2025-06-30T00:00:00Z" {
		t.Fatalf("ExpirationTime = %q", tok.ExpirationTime)
	}
	if !strings.Contains((*got)[0].Path, "/v1/enterprises/X/enrollmentTokens") {
		t.Fatalf("path = %q", (*got)[0].Path)
	}
	if !strings.Contains((*got)[0].Body, `"policyName":"enterprises/X/policies/default"`) {
		t.Fatalf("body should include policyName: %q", (*got)[0].Body)
	}
	if !strings.Contains((*got)[0].Body, `"additionalData":"ou=engineering"`) {
		t.Fatalf("body should include additionalData: %q", (*got)[0].Body)
	}
}

func TestCreateEnrollmentToken_EmptyEnterpriseName_ReturnsInvalidRequest(t *testing.T) {
	// Arrange
	srv, calls, _ := recordingServer(t, http.StatusOK, func(req receivedRequest) string {
		t.Errorf("should not call AMAPI")
		return `{}`
	})
	c, _ := newTestClient(t, srv, time.Microsecond)

	// Act
	_, err := c.CreateEnrollmentToken(context.Background(), "", EnrollmentTokenRequest{})

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

func TestCreateEnrollmentToken_400_ReturnsInvalidRequest(t *testing.T) {
	// Arrange
	srv, calls, _ := recordingServer(t, http.StatusBadRequest, func(req receivedRequest) string {
		return `{"error":{"code":400,"message":"invalid duration"}}`
	})
	c, _ := newTestClient(t, srv, time.Microsecond)

	// Act
	_, err := c.CreateEnrollmentToken(context.Background(), "enterprises/X", EnrollmentTokenRequest{
		Duration: "not-a-duration",
	})

	// Assert
	if err == nil {
		t.Fatalf("expected error")
	}
	var de *pkgerrors.Error
	if !stdErrors.As(err, &de) {
		t.Fatalf("error is not *pkgerrors.Error: %T", err)
	}
	if de.Code != pkgerrors.CodeInvalidRequest {
		t.Fatalf("Code = %q, want %q", de.Code, pkgerrors.CodeInvalidRequest)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1 (no retries for 4xx)", calls.Load())
	}
}
