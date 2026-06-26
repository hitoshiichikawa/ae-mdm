package amapi

import (
	"context"
	stdErrors "errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

func TestListDevices_PagesConcatenated(t *testing.T) {
	// Arrange: 1 ページ目に nextPageToken、2 ページ目に空 token
	calls := &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			_, _ = w.Write([]byte(`{"devices":[{"name":"enterprises/X/devices/A","appliedState":"ACTIVE"}],"nextPageToken":"tok2"}`))
		case 2:
			_, _ = w.Write([]byte(`{"devices":[{"name":"enterprises/X/devices/B","appliedState":"PROVISIONING"}]}`))
		default:
			t.Errorf("unexpected page call #%d", n)
		}
	}))
	defer srv.Close()
	c, _ := newTestClient(t, srv, time.Microsecond)

	// Act
	devices, err := c.ListDevices(context.Background(), "enterprises/X")

	// Assert
	if err != nil {
		t.Fatalf("ListDevices = %v", err)
	}
	if len(devices) != 2 {
		t.Fatalf("len = %d, want 2", len(devices))
	}
	if devices[0].Name != "enterprises/X/devices/A" || devices[0].State != "ACTIVE" {
		t.Fatalf("device[0] = %+v", devices[0])
	}
	if devices[1].Name != "enterprises/X/devices/B" || devices[1].State != "PROVISIONING" {
		t.Fatalf("device[1] = %+v", devices[1])
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

func TestListDevices_EmptyEnterpriseName_ReturnsInvalidRequest(t *testing.T) {
	// Arrange
	srv, calls, _ := recordingServer(t, http.StatusOK, func(req receivedRequest) string {
		t.Errorf("should not call AMAPI")
		return `{}`
	})
	c, _ := newTestClient(t, srv, time.Microsecond)

	// Act
	_, err := c.ListDevices(context.Background(), "")

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

func TestGetDevice_OK_NormalizesResponse(t *testing.T) {
	// Arrange
	srv, _, got := recordingServer(t, http.StatusOK, func(req receivedRequest) string {
		return `{"name":"enterprises/X/devices/D1","appliedState":"ACTIVE","appliedPolicyName":"enterprises/X/policies/default","lastStatusReportTime":"2025-06-01T00:00:00Z"}`
	})
	c, _ := newTestClient(t, srv, time.Microsecond)

	// Act
	d, err := c.GetDevice(context.Background(), "enterprises/X", "D1")

	// Assert
	if err != nil {
		t.Fatalf("GetDevice = %v", err)
	}
	if d.Name != "enterprises/X/devices/D1" {
		t.Fatalf("Name = %q", d.Name)
	}
	if d.State != "ACTIVE" {
		t.Fatalf("State = %q", d.State)
	}
	if d.PolicyName != "enterprises/X/policies/default" {
		t.Fatalf("PolicyName = %q", d.PolicyName)
	}
	if d.LastStatusReportTime != "2025-06-01T00:00:00Z" {
		t.Fatalf("LastStatusReportTime = %q", d.LastStatusReportTime)
	}
	if !strings.Contains((*got)[0].Path, "/v1/enterprises/X/devices/D1") {
		t.Fatalf("path = %q", (*got)[0].Path)
	}
}

func TestGetDevice_EmptyDeviceID_ReturnsInvalidRequest(t *testing.T) {
	// Arrange
	srv, _, _ := recordingServer(t, http.StatusOK, func(req receivedRequest) string {
		return `{}`
	})
	c, _ := newTestClient(t, srv, time.Microsecond)

	// Act
	_, err := c.GetDevice(context.Background(), "enterprises/X", "")

	// Assert
	if err == nil {
		t.Fatalf("expected error")
	}
	var de *pkgerrors.Error
	if !stdErrors.As(err, &de) || de.Code != pkgerrors.CodeInvalidRequest {
		t.Fatalf("want CodeInvalidRequest, got %v", err)
	}
}

func TestIssueCommand_OK_ReturnsOperationName(t *testing.T) {
	// Arrange
	srv, _, got := recordingServer(t, http.StatusOK, func(req receivedRequest) string {
		return `{"name":"enterprises/X/devices/D1/operations/op-1","done":false}`
	})
	c, _ := newTestClient(t, srv, time.Microsecond)

	// Act
	commandID, err := c.IssueCommand(context.Background(), "enterprises/X", "D1", CommandRequest{
		Type:     "LOCK",
		Duration: "60s",
	})

	// Assert
	if err != nil {
		t.Fatalf("IssueCommand = %v", err)
	}
	if commandID != "enterprises/X/devices/D1/operations/op-1" {
		t.Fatalf("commandID = %q", commandID)
	}
	if !strings.Contains((*got)[0].Path, "/v1/enterprises/X/devices/D1:issueCommand") {
		t.Fatalf("path = %q", (*got)[0].Path)
	}
	if !strings.Contains((*got)[0].Body, `"type":"LOCK"`) {
		t.Fatalf("body should include type=LOCK: %q", (*got)[0].Body)
	}
	if !strings.Contains((*got)[0].Body, `"duration":"60s"`) {
		t.Fatalf("body should include duration=60s: %q", (*got)[0].Body)
	}
}

func TestIssueCommand_EmptyType_ReturnsInvalidRequest(t *testing.T) {
	// Arrange
	srv, calls, _ := recordingServer(t, http.StatusOK, func(req receivedRequest) string {
		t.Errorf("should not call AMAPI when Type is empty")
		return `{}`
	})
	c, _ := newTestClient(t, srv, time.Microsecond)

	// Act
	_, err := c.IssueCommand(context.Background(), "enterprises/X", "D1", CommandRequest{Type: ""})

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

func TestIssueCommand_409Conflict_NotRetried(t *testing.T) {
	// Arrange
	srv, calls, _ := recordingServer(t, http.StatusConflict, func(req receivedRequest) string {
		return `{"error":{"code":409,"message":"device busy"}}`
	})
	c, _ := newTestClient(t, srv, time.Microsecond)

	// Act
	_, err := c.IssueCommand(context.Background(), "enterprises/X", "D1", CommandRequest{Type: "LOCK"})

	// Assert
	if err == nil {
		t.Fatalf("expected error")
	}
	var de *pkgerrors.Error
	if !stdErrors.As(err, &de) {
		t.Fatalf("error is not *pkgerrors.Error: %T", err)
	}
	if de.Code != pkgerrors.CodeConflict {
		t.Fatalf("Code = %q, want %q", de.Code, pkgerrors.CodeConflict)
	}
	if de.IsTransient {
		t.Fatalf("IsTransient = true, want false")
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
}
