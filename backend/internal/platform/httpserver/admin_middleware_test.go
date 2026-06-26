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

// TestRequireSuperAdmin_NoTenantContext_Returns401 は Issue #33 当初実装の挙動を
// 回帰耐性で固定する（本ファイル後半の RequireAdminConsoleAndSuperAdmin が canonical で
// あり、本テストは backward compatibility のため）。
//
// 本テストは admin_middleware.go の RequireSuperAdmin 単体の挙動を verify する。
// 実運用では TenantContextMiddleware が先に 401 を返すが、admin middleware 単独でも
// fail-closed で動作することを確認する（防御的多層化）。
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

// TestRequireSuperAdmin_TenantContextWithoutSuperAdmin_Returns403 は backward compat。
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

// TestRequireSuperAdmin_SuperAdmin_PassesToNext は backward compat。
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

// ----------------------------------------------------------------------------
// Issue #37 (A3b): RequireAdminConsoleAndSuperAdmin canonical テスト群
// ----------------------------------------------------------------------------

// newAdminConsoleClaims は admin-console aud + SuperAdmin の正常 claims を作る helper。
func newAdminConsoleClaims() AuthClaims {
	return AuthClaims{
		TenantID:     uuid.Nil, // SuperAdmin の cross-tenant 文脈
		AdminUserID:  uuid.New(),
		Roles:        []string{"SuperAdmin"},
		IsSuperAdmin: true,
		Console:      "admin-console",
	}
}

// runAdminGuard は test helper。RequireAdminConsoleAndSuperAdmin を組み立てて HTTP を呼ぶ。
func runAdminGuard(t *testing.T, claimsOrNil *AuthClaims) (*httptest.ResponseRecorder, *fakeLogger, bool) {
	t.Helper()
	log := &fakeLogger{}
	nextCalled := false
	handler := RequireAdminConsoleAndSuperAdmin(log)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/tenants/abcdef", nil)
	req.Header.Set("X-Request-ID", "test-req-id-123")
	// RequestID middleware を挟まずに直接 ctx へ request_id を入れる
	req = req.WithContext(context.WithValue(req.Context(), requestIDCtxKey{}, "test-req-id-123"))
	if claimsOrNil != nil {
		req = req.WithContext(WithAuthClaims(req.Context(), *claimsOrNil))
	}
	handler.ServeHTTP(rec, req)
	return rec, log, nextCalled
}

// TestRequireAdminConsoleAndSuperAdmin_NoSession_Returns401 は requirements.md Req 2.6 /
// 6.3 と integrate する。AuthClaims が ctx に無い → 401。
func TestRequireAdminConsoleAndSuperAdmin_NoSession_Returns401(t *testing.T) {
	rec, log, nextCalled := runAdminGuard(t, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401", rec.Code)
	}
	if nextCalled {
		t.Errorf("session 不在で next が呼ばれてはならない")
	}
	// body には対象リソースのパス情報を含まない（Req 2.7 / 3.6）
	bodyStr := rec.Body.String()
	if containsTenantPath(bodyStr) {
		t.Errorf("body に対象リソース path が露出している: %q", bodyStr)
	}
	// 構造化ログに authz_deny_reason=session_missing が含まれること
	assertLogFieldExists(t, log, "authz_deny_reason", authzDenyReasonSessionMissing)
	assertLogFieldExists(t, log, "request_id", "test-req-id-123")
}

// TestRequireAdminConsoleAndSuperAdmin_TenantConsole_Returns403 は Req 2.4 / 2.7 を verify する。
// admin-console 以外（tenant-console）の audience は SuperAdmin role を持っていても 403。
func TestRequireAdminConsoleAndSuperAdmin_TenantConsole_Returns403(t *testing.T) {
	claims := newAdminConsoleClaims()
	claims.Console = "tenant-console"
	rec, log, nextCalled := runAdminGuard(t, &claims)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403", rec.Code)
	}
	if nextCalled {
		t.Errorf("tenant-console aud で next が呼ばれてはならない")
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
	// 構造化ログに audience_mismatch が含まれること
	assertLogFieldExists(t, log, "authz_deny_reason", authzDenyReasonAudienceMismatch)
	assertLogFieldExists(t, log, "console", "tenant-console")
	assertLogFieldExists(t, log, "actor_id", claims.AdminUserID.String())
}

// TestRequireAdminConsoleAndSuperAdmin_AdminConsole_NotSuperAdmin_Returns403 は Req 2.5 / 2.7
// を verify する。admin-console aud でも非 SuperAdmin は 403。
func TestRequireAdminConsoleAndSuperAdmin_AdminConsole_NotSuperAdmin_Returns403(t *testing.T) {
	claims := newAdminConsoleClaims()
	claims.IsSuperAdmin = false
	claims.Roles = []string{"TenantAdmin"}
	rec, log, nextCalled := runAdminGuard(t, &claims)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403", rec.Code)
	}
	if nextCalled {
		t.Errorf("非 SuperAdmin で next が呼ばれてはならない")
	}
	assertLogFieldExists(t, log, "authz_deny_reason", authzDenyReasonSuperAdminNotPresent)
	assertLogFieldExists(t, log, "console", "admin-console")
}

// TestRequireAdminConsoleAndSuperAdmin_Success_PassesToNext は Req 2.3 を verify する。
func TestRequireAdminConsoleAndSuperAdmin_Success_PassesToNext(t *testing.T) {
	claims := newAdminConsoleClaims()
	rec, log, nextCalled := runAdminGuard(t, &claims)
	if !nextCalled {
		t.Fatalf("admin-console + SuperAdmin で next が呼ばれるべき; status=%d body=%q",
			rec.Code, rec.Body.String())
	}
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d; want 200", rec.Code)
	}
	if len(log.warnCalls) != 0 {
		t.Errorf("成功経路で WARN ログを出してはならない: %v", log.warnCalls)
	}
}

// TestRequireAdminConsoleAndSuperAdmin_EmptyConsole_Returns403 は AuthClaims.Console が
// zero value（空文字 / legacy claims）の場合に audience_mismatch で 403 になることを verify する。
// 旧 auth middleware（Console を埋めない実装）からの後方互換セキュリティ保証。
func TestRequireAdminConsoleAndSuperAdmin_EmptyConsole_Returns403(t *testing.T) {
	claims := newAdminConsoleClaims()
	claims.Console = ""
	rec, log, nextCalled := runAdminGuard(t, &claims)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403 (legacy claims で console 空文字)", rec.Code)
	}
	if nextCalled {
		t.Errorf("Console=\"\" で next が呼ばれてはならない")
	}
	assertLogFieldExists(t, log, "authz_deny_reason", authzDenyReasonAudienceMismatch)
}

// containsTenantPath は body に対象リソースの ID 部分（"abcdef" 等）が露出していないかを
// 簡易チェックする helper（Req 2.7 / 3.6: body にリソース ID を含めない）。
func containsTenantPath(body string) bool {
	// 本テストは fixed path /api/admin/tenants/abcdef を使う。body 中に "abcdef" が
	// 出現する場合は識別子の露出と判定する。
	for i := 0; i+5 < len(body); i++ {
		if body[i:i+6] == "abcdef" {
			return true
		}
	}
	return false
}

// assertLogFieldExists は fakeLogger の WARN 呼び出し列の中に (key, want) ペアが含まれるかを
// チェックする helper。
func assertLogFieldExists(t *testing.T, log *fakeLogger, key, want string) {
	t.Helper()
	for _, call := range log.warnCalls {
		if v, ok := fieldValue(call.fields, key); ok {
			if got, ok := v.(string); ok && got == want {
				return
			}
		}
	}
	t.Errorf("WARN ログに field %q=%q が存在しない; warnCalls=%+v", key, want, log.warnCalls)
}
