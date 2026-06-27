package audit

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	internalerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/authz"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/httpserver"
)

// ---- Test doubles ----

// fakeService は Service interface を満たす test double。List で受信した Filter / ctx を記録し、
// 任意の結果・エラーを返せる。Record は本 handler テストでは使わないため最小実装にする。
type fakeService struct {
	listCalls    int
	listedFilter Filter
	listedCtx    context.Context
	listOut      []Event
	listErr      error
}

func (s *fakeService) Record(_ context.Context, _ Event) error { return nil }

func (s *fakeService) List(ctx context.Context, f Filter) ([]Event, error) {
	s.listCalls++
	s.listedFilter = f
	s.listedCtx = ctx
	return s.listOut, s.listErr
}

// fakeLogger は logger.Logger interface を満たすテスト用 spy。WARN 呼び出しを観測する。
type fakeLogger struct {
	warnCalls []logCall
}

type logCall struct {
	msg    string
	fields []any
}

func (f *fakeLogger) Debug(_ string, _ ...any) {}
func (f *fakeLogger) Info(_ string, _ ...any)  {}
func (f *fakeLogger) Warn(msg string, fields ...any) {
	f.warnCalls = append(f.warnCalls, logCall{msg: msg, fields: fields})
}
func (f *fakeLogger) Error(_ string, _ ...any)     {}
func (f *fakeLogger) With(_ ...any) logger.Logger  { return f }
func (f *fakeLogger) Sync() error                  { return nil }

// fieldValue は fields []any（"key", value, ... の列）から key の値を取り出す helper。
func fieldValue(fields []any, key string) (any, bool) {
	for i := 0; i+1 < len(fields); i += 2 {
		if k, ok := fields[i].(string); ok && k == key {
			return fields[i+1], true
		}
	}
	return nil, false
}

// assertWarnFailureKind は WARN 呼び出し列に failure_kind=want が含まれることを検証する helper。
func assertWarnFailureKind(t *testing.T, log *fakeLogger, want string) {
	t.Helper()
	for _, call := range log.warnCalls {
		if v, ok := fieldValue(call.fields, "failure_kind"); ok {
			if got, ok := v.(string); ok && got == want {
				return
			}
		}
	}
	t.Errorf("WARN ログに failure_kind=%q が存在しない; warnCalls=%+v", want, log.warnCalls)
}

// newTenantClaims は指定 role の tenant-console claims を作る helper。
func newTenantClaims(tenantID uuid.UUID, roles ...string) httpserver.AuthClaims {
	return httpserver.AuthClaims{
		TenantID:          tenantID,
		AdminUserID:       uuid.New(),
		Roles:             roles,
		Console:           "tenant-console",
		SessionHashPrefix: "abc12345",
	}
}

// doRequest は Handler を本番同様に親 chi router へ `Mount("/audit-logs", h)` した上で
// GET /audit-logs?<rawQuery> を実行する helper。
//
// cmd/api の `routers.API.Mount("/audit-logs", handler)` 配線（design.md Modified Files）を
// 忠実に再現することで、chi の prefix strip 後に handler 内の root route（`/`）が成立する経路を
// 検証する。claimsOrNil が nil の場合は claims を ctx に注入しない（未認証経路の検証用）。
func doRequest(t *testing.T, h *Handler, rawQuery string, claimsOrNil *httpserver.AuthClaims) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	router.Mount("/audit-logs", h)

	rec := httptest.NewRecorder()
	target := "/audit-logs"
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if claimsOrNil != nil {
		req = req.WithContext(httpserver.WithAuthClaims(req.Context(), *claimsOrNil))
	}
	router.ServeHTTP(rec, req)
	return rec
}

// decodeDTOs は 200 応答 body を []AuditLogDTO に decode する helper。
func decodeDTOs(t *testing.T, rec *httptest.ResponseRecorder) []AuditLogDTO {
	t.Helper()
	var dtos []AuditLogDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dtos); err != nil {
		t.Fatalf("decode DTOs: %v body=%q", err, rec.Body.String())
	}
	return dtos
}

// decodeErrCode は 4xx/5xx 応答 body の code を取り出す helper。
func decodeErrCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v body=%q", err, rec.Body.String())
	}
	return body.Code
}

// ---- (a) TenantAdmin で own-tenant Filter（TenantID 未設定）で 200 + JSON 配列（Req 2.1 / 4.6） ----

func TestHandler_List_TenantAdmin_OwnTenant_Returns200(t *testing.T) {
	// Arrange
	tenantID := uuid.New()
	svc := &fakeService{listOut: []Event{
		{ID: uuid.New(), TenantID: tenantID, ActorID: uuid.New(), EventType: "policy_change", Result: ResultSuccess, OccurredAt: fixedNow},
	}}
	log := &fakeLogger{}
	h := NewHandler(svc, authz.New(), log)
	claims := newTenantClaims(tenantID, "TenantAdmin")

	// Act
	rec := doRequest(t, h, "", &claims)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 body=%q", rec.Code, rec.Body.String())
	}
	if svc.listCalls != 1 {
		t.Fatalf("svc.List 呼び出し回数 = %d; want 1", svc.listCalls)
	}
	if svc.listedFilter.TenantID != nil {
		t.Errorf("own-tenant 経路で Filter.TenantID が設定されている（RLS に委ねるべき）: %v", svc.listedFilter.TenantID)
	}
	dtos := decodeDTOs(t, rec)
	if len(dtos) != 1 {
		t.Fatalf("DTO 件数 = %d; want 1", len(dtos))
	}
	if dtos[0].EventType != "policy_change" {
		t.Errorf("DTO.EventType = %q; want policy_change", dtos[0].EventType)
	}
}

// ---- (b) Operator claims で 403（Req 4.1） ----

func TestHandler_List_Operator_Returns403(t *testing.T) {
	// Arrange
	tenantID := uuid.New()
	svc := &fakeService{}
	log := &fakeLogger{}
	h := NewHandler(svc, authz.New(), log)
	claims := newTenantClaims(tenantID, "Operator")

	// Act
	rec := doRequest(t, h, "", &claims)

	// Assert
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403", rec.Code)
	}
	if svc.listCalls != 0 {
		t.Errorf("403 で svc.List が呼ばれてはならない（呼び出し回数=%d）", svc.listCalls)
	}
	if code := decodeErrCode(t, rec); code != string(internalerrors.CodeForbidden) {
		t.Errorf("body.Code = %q; want %q", code, internalerrors.CodeForbidden)
	}
}

// ---- (c) Viewer claims で 403（Req 4.5） ----

func TestHandler_List_Viewer_Returns403(t *testing.T) {
	// Arrange
	tenantID := uuid.New()
	svc := &fakeService{}
	log := &fakeLogger{}
	h := NewHandler(svc, authz.New(), log)
	claims := newTenantClaims(tenantID, "Viewer")

	// Act
	rec := doRequest(t, h, "", &claims)

	// Assert
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d; want 403", rec.Code)
	}
	if svc.listCalls != 0 {
		t.Errorf("403 で svc.List が呼ばれてはならない（呼び出し回数=%d）", svc.listCalls)
	}
	if code := decodeErrCode(t, rec); code != string(internalerrors.CodeForbidden) {
		t.Errorf("body.Code = %q; want %q", code, internalerrors.CodeForbidden)
	}
}

// ---- (d) claims 不在で 401（Req 4.2） ----

func TestHandler_List_NoClaims_Returns401(t *testing.T) {
	// Arrange
	svc := &fakeService{}
	log := &fakeLogger{}
	h := NewHandler(svc, authz.New(), log)

	// Act（claims を注入しない）
	rec := doRequest(t, h, "", nil)

	// Assert
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401", rec.Code)
	}
	if svc.listCalls != 0 {
		t.Errorf("401 で svc.List が呼ばれてはならない（呼び出し回数=%d）", svc.listCalls)
	}
	if code := decodeErrCode(t, rec); code != string(internalerrors.CodeUnauthenticated) {
		t.Errorf("body.Code = %q; want %q", code, internalerrors.CodeUnauthenticated)
	}
}

// ---- (e) event_type / actor_id / resource_id / from / to が Filter に正しく写像（Req 2.2〜2.5） ----

func TestHandler_List_MapsAllFilterFields(t *testing.T) {
	// Arrange
	tenantID := uuid.New()
	actorID := uuid.New()
	svc := &fakeService{listOut: []Event{}}
	log := &fakeLogger{}
	h := NewHandler(svc, authz.New(), log)
	claims := newTenantClaims(tenantID, "TenantAdmin")
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	rawQuery := "event_type=role_change&actor_id=" + actorID.String() +
		"&resource_id=res-42&from=" + from.Format(time.RFC3339) + "&to=" + to.Format(time.RFC3339)

	// Act
	rec := doRequest(t, h, rawQuery, &claims)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 body=%q", rec.Code, rec.Body.String())
	}
	got := svc.listedFilter
	if got.EventType != "role_change" {
		t.Errorf("Filter.EventType = %q; want role_change", got.EventType)
	}
	if got.ActorID != actorID.String() {
		t.Errorf("Filter.ActorID = %q; want %q", got.ActorID, actorID.String())
	}
	if got.ResourceID != "res-42" {
		t.Errorf("Filter.ResourceID = %q; want res-42", got.ResourceID)
	}
	if got.From == nil || !got.From.Equal(from) {
		t.Errorf("Filter.From = %v; want %v", got.From, from)
	}
	if got.To == nil || !got.To.Equal(to) {
		t.Errorf("Filter.To = %v; want %v", got.To, to)
	}
}

// ---- (f) from のみ / to のみ指定が Filter に反映される（Req 2.6 / 2.7） ----

func TestHandler_List_FromOnly_ReflectedInFilter(t *testing.T) {
	// Arrange
	tenantID := uuid.New()
	svc := &fakeService{listOut: []Event{}}
	h := NewHandler(svc, authz.New(), &fakeLogger{})
	claims := newTenantClaims(tenantID, "TenantAdmin")
	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	// Act
	rec := doRequest(t, h, "from="+from.Format(time.RFC3339), &claims)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	if svc.listedFilter.From == nil || !svc.listedFilter.From.Equal(from) {
		t.Errorf("Filter.From = %v; want %v", svc.listedFilter.From, from)
	}
	if svc.listedFilter.To != nil {
		t.Errorf("to 未指定なのに Filter.To が設定されている: %v", svc.listedFilter.To)
	}
}

func TestHandler_List_ToOnly_ReflectedInFilter(t *testing.T) {
	// Arrange
	tenantID := uuid.New()
	svc := &fakeService{listOut: []Event{}}
	h := NewHandler(svc, authz.New(), &fakeLogger{})
	claims := newTenantClaims(tenantID, "TenantAdmin")
	to := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	// Act
	rec := doRequest(t, h, "to="+to.Format(time.RFC3339), &claims)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	if svc.listedFilter.To == nil || !svc.listedFilter.To.Equal(to) {
		t.Errorf("Filter.To = %v; want %v", svc.listedFilter.To, to)
	}
	if svc.listedFilter.From != nil {
		t.Errorf("from 未指定なのに Filter.From が設定されている: %v", svc.listedFilter.From)
	}
}

// ---- (g) from/to 非 RFC3339・actor_id 非 uuid で 400（failure_kind=parse_invalid / Req 2.8 と区別） ----

func TestHandler_List_InvalidQuery_Returns400(t *testing.T) {
	tenantID := uuid.New()
	claims := newTenantClaims(tenantID, "TenantAdmin")

	cases := []struct {
		name     string
		rawQuery string
	}{
		{name: "from 非 RFC3339", rawQuery: "from=not-a-date"},
		{name: "to 非 RFC3339", rawQuery: "to=2026-13-99"},
		{name: "actor_id 非 uuid", rawQuery: "actor_id=not-a-uuid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			svc := &fakeService{}
			log := &fakeLogger{}
			h := NewHandler(svc, authz.New(), log)

			// Act
			rec := doRequest(t, h, tc.rawQuery, &claims)

			// Assert
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400 body=%q", rec.Code, rec.Body.String())
			}
			if svc.listCalls != 0 {
				t.Errorf("400 で svc.List が呼ばれてはならない（呼び出し回数=%d）", svc.listCalls)
			}
			if code := decodeErrCode(t, rec); code != string(internalerrors.CodeInvalidRequest) {
				t.Errorf("body.Code = %q; want %q", code, internalerrors.CodeInvalidRequest)
			}
			assertWarnFailureKind(t, log, FailureKindParseInvalid)
		})
	}
}

// ---- (h) fake Service が空 slice を返したら 200 + [] （Req 2.8） ----

func TestHandler_List_EmptyResult_Returns200EmptyArray(t *testing.T) {
	// Arrange
	tenantID := uuid.New()
	svc := &fakeService{listOut: []Event{}}
	h := NewHandler(svc, authz.New(), &fakeLogger{})
	claims := newTenantClaims(tenantID, "TenantAdmin")

	// Act
	rec := doRequest(t, h, "", &claims)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	// JSON は null ではなく空配列 [] であること（Req 2.8）。
	if got := rec.Body.String(); got != "[]\n" {
		t.Errorf("body = %q; want %q", got, "[]\n")
	}
	dtos := decodeDTOs(t, rec)
	if len(dtos) != 0 {
		t.Errorf("DTO 件数 = %d; want 0", len(dtos))
	}
}
