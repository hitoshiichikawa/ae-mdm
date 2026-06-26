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

func TestUpsertPolicy_OK_PatchesResource(t *testing.T) {
	// Arrange
	srv, calls, got := recordingServer(t, http.StatusOK, func(req receivedRequest) string {
		return `{"name":"enterprises/X/policies/default","version":"42"}`
	})
	c, _ := newTestClient(t, srv, time.Microsecond)

	// Act
	err := c.UpsertPolicy(context.Background(), "enterprises/X", "default", PolicyBody{
		Raw: map[string]any{
			"cameraDisabled": true,
		},
	})

	// Assert
	if err != nil {
		t.Fatalf("UpsertPolicy = %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d", calls.Load())
	}
	if (*got)[0].Method != http.MethodPatch {
		t.Fatalf("method = %q, want PATCH", (*got)[0].Method)
	}
	if !strings.Contains((*got)[0].Path, "/v1/enterprises/X/policies/default") {
		t.Fatalf("path = %q", (*got)[0].Path)
	}
	if !strings.Contains((*got)[0].Body, `"cameraDisabled":true`) {
		t.Fatalf("body should include cameraDisabled=true: %q", (*got)[0].Body)
	}
}

func TestUpsertPolicy_EmptyEnterpriseName_ReturnsInvalidRequest(t *testing.T) {
	// Arrange
	srv, calls, _ := recordingServer(t, http.StatusOK, func(req receivedRequest) string {
		t.Errorf("should not call AMAPI; got %s %s", req.Method, req.Path)
		return `{}`
	})
	c, _ := newTestClient(t, srv, time.Microsecond)

	// Act
	err := c.UpsertPolicy(context.Background(), "", "default", PolicyBody{})

	// Assert
	if err == nil {
		t.Fatalf("expected error")
	}
	var de *pkgerrors.Error
	if !stdErrors.As(err, &de) || de.Code != pkgerrors.CodeInvalidRequest {
		t.Fatalf("want CodeInvalidRequest, got %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("AMAPI was called %d times, want 0", calls.Load())
	}
}

func TestUpsertPolicy_EmptyPolicyName_ReturnsInvalidRequest(t *testing.T) {
	// Arrange
	srv, _, _ := recordingServer(t, http.StatusOK, func(req receivedRequest) string {
		return `{}`
	})
	c, _ := newTestClient(t, srv, time.Microsecond)

	// Act
	err := c.UpsertPolicy(context.Background(), "enterprises/X", "", PolicyBody{})

	// Assert
	if err == nil {
		t.Fatalf("expected error for empty policyName")
	}
	var de *pkgerrors.Error
	if !stdErrors.As(err, &de) || de.Code != pkgerrors.CodeInvalidRequest {
		t.Fatalf("want CodeInvalidRequest, got %v", err)
	}
}

func TestGetPolicy_OK_NormalizesResponse(t *testing.T) {
	// Arrange
	srv, _, _ := recordingServer(t, http.StatusOK, func(req receivedRequest) string {
		return `{"name":"enterprises/X/policies/default","version":"7","cameraDisabled":true}`
	})
	c, _ := newTestClient(t, srv, time.Microsecond)

	// Act
	body, err := c.GetPolicy(context.Background(), "enterprises/X", "default")

	// Assert
	if err != nil {
		t.Fatalf("GetPolicy = %v", err)
	}
	if body.Name != "enterprises/X/policies/default" {
		t.Fatalf("Name = %q", body.Name)
	}
	if body.Version != 7 {
		t.Fatalf("Version = %d, want 7", body.Version)
	}
	if v, ok := body.Raw["cameraDisabled"].(bool); !ok || !v {
		t.Fatalf("Raw[cameraDisabled] = %#v, want true", body.Raw["cameraDisabled"])
	}
}

func TestUpsertPolicy_ZeroValueFieldsAreSent(t *testing.T) {
	// Arrange: raw map に false / 空配列 / 0 等の zero value を含めても AMAPI patch 本文に
	// 必ず出ることを確認する（Google API Go client の omitempty を ForceSendFields で打ち消す）。
	srv, _, got := recordingServer(t, http.StatusOK, func(req receivedRequest) string {
		return `{"name":"enterprises/X/policies/default","version":"1"}`
	})
	c, _ := newTestClient(t, srv, time.Microsecond)

	// Act
	err := c.UpsertPolicy(context.Background(), "enterprises/X", "default", PolicyBody{
		Raw: map[string]any{
			"cameraDisabled":      false,
			"installAppsDisabled": false,
			"applications":        []any{},
		},
	})

	// Assert
	if err != nil {
		t.Fatalf("UpsertPolicy = %v", err)
	}
	body := (*got)[0].Body
	if !strings.Contains(body, `"cameraDisabled":false`) {
		t.Fatalf("body must include cameraDisabled=false: %q", body)
	}
	if !strings.Contains(body, `"installAppsDisabled":false`) {
		t.Fatalf("body must include installAppsDisabled=false: %q", body)
	}
	if !strings.Contains(body, `"applications":[]`) {
		t.Fatalf("body must include empty applications array: %q", body)
	}
}

func TestUpsertPolicy_NestedZeroValueFieldsAreSent(t *testing.T) {
	// Arrange: 入れ子の zero value も ForceSendFields の再帰伝搬で送信される
	// （AMAPI Policy の入れ子: passwordRequirements 等）。
	srv, _, got := recordingServer(t, http.StatusOK, func(req receivedRequest) string {
		return `{"name":"enterprises/X/policies/default","version":"1"}`
	})
	c, _ := newTestClient(t, srv, time.Microsecond)

	// Act
	err := c.UpsertPolicy(context.Background(), "enterprises/X", "default", PolicyBody{
		Raw: map[string]any{
			"passwordRequirements": map[string]any{
				"passwordMinimumLength": float64(0),
			},
		},
	})

	// Assert
	if err != nil {
		t.Fatalf("UpsertPolicy = %v", err)
	}
	body := (*got)[0].Body
	if !strings.Contains(body, `"passwordMinimumLength":0`) {
		t.Fatalf("body must include nested zero-value field: %q", body)
	}
}

func TestGetPolicy_503_RetriedAndExhausted(t *testing.T) {
	// Arrange
	srv, calls := retryServer(t, 100, http.StatusServiceUnavailable, `{}`)
	c, _ := newTestClient(t, srv, time.Microsecond)

	// Act
	_, err := c.GetPolicy(context.Background(), "enterprises/X", "default")

	// Assert
	if err == nil {
		t.Fatalf("expected error")
	}
	var de *pkgerrors.Error
	if !stdErrors.As(err, &de) {
		t.Fatalf("error is not *pkgerrors.Error: %T", err)
	}
	if de.Code != pkgerrors.CodeUpstream || !de.IsTransient {
		t.Fatalf("want CodeUpstream+transient, got %+v", de)
	}
	if calls.Load() != int32(defaultMaxRetries+1) {
		t.Fatalf("calls = %d, want %d", calls.Load(), defaultMaxRetries+1)
	}
}
