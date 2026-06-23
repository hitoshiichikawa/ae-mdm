package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	internalerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
)

// TestRequireSuperAdmin_NoTenantContext_Returns401 は requirements.md Req 5.4 / 5.5 と
// 本タスク詳細項目「TenantContext 未確立 → 401」契約に対応する。
//
// 本テストは admin_middleware.go 単体の挙動を verify する。実運用では
// TenantContextMiddleware が先に 401 を返すが、admin middleware 単独でも fail-closed
// で動作することを確認する（防御的多層化）。
func TestRequireSuperAdmin_NoTenantContext_Returns401(t *testing.T) {
	// Arrange
	log := &fakeLogger{}
	nextCalled := false
	handler := RequireSuperAdmin(log)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/x", nil)

	// Act
	handler.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401", rec.Code)
	}
	if nextCalled {
		t.Errorf("TenantContext 不在で next が呼ばれてはならない")
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	if body.Code != string(internalerrors.CodeUnauthenticated) {
		t.Errorf("body.Code = %q; want %q", body.Code, internalerrors.CodeUnauthenticated)
	}
}

// TestRequireSuperAdmin_TenantContextWithoutSuperAdmin_Returns403 はテスト (e) 対応。
// TenantContext は put 済みだが IsSuperAdmin=false の場合に 403 を返すことを確認する。
func TestRequireSuperAdmin_TenantContextWithoutSuperAdmin_Returns403(t *testing.T) {
	// Arrange
	log := &fakeLogger{}
	nextCalled := false
	handler := RequireSuperAdmin(log)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/x", nil)
	tc := db.TenantContext{
		TenantID:     uuid.New(),
		AdminUserID:  uuid.New(),
		Roles:        []string{"TenantAdmin"},
		IsSuperAdmin: false,
	}
	req = req.WithContext(db.WithTenantContext(context.Background(), tc))

	// Act
	handler.ServeHTTP(rec, req)

	// Assert
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403", rec.Code)
	}
	if nextCalled {
		t.Errorf("IsSuperAdmin=false で next が呼ばれてはならない")
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("json decode: %v", err)
	}
	if body.Code != string(internalerrors.CodeForbidden) {
		t.Errorf("body.Code = %q; want %q", body.Code, internalerrors.CodeForbidden)
	}
}

// TestRequireSuperAdmin_SuperAdmin_PassesToNext はテスト (f) 対応。
// IsSuperAdmin=true で next handler に到達することを確認する。
func TestRequireSuperAdmin_SuperAdmin_PassesToNext(t *testing.T) {
	// Arrange
	log := &fakeLogger{}
	nextCalled := false
	handler := RequireSuperAdmin(log)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusAccepted)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/x", nil)
	tc := db.TenantContext{
		TenantID:     uuid.Nil, // SuperAdmin cross-tenant 操作時の典型値
		AdminUserID:  uuid.New(),
		Roles:        []string{"SuperAdmin"},
		IsSuperAdmin: true,
	}
	req = req.WithContext(db.WithTenantContext(context.Background(), tc))

	// Act
	handler.ServeHTTP(rec, req)

	// Assert
	if !nextCalled {
		t.Fatalf("IsSuperAdmin=true で next が呼ばれること")
	}
	if rec.Code != http.StatusAccepted {
		t.Errorf("status = %d; want 202", rec.Code)
	}
}
