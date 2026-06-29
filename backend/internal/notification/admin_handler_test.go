package notification

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
)

// ---- Test doubles ----

// fakeUnassignedQueue は UnassignedQueue interface を満たす test double。List で受信した Filter /
// ctx を記録し、任意の結果・エラーを返せる。Enqueue は本 handler テストでは使わないため最小実装。
type fakeUnassignedQueue struct {
	listCalls    int
	listedFilter Filter
	listedCtx    context.Context
	listOut      []UnassignedNotification
	listErr      error
}

func (q *fakeUnassignedQueue) Enqueue(_ context.Context, _ Envelope) error { return nil }

func (q *fakeUnassignedQueue) List(ctx context.Context, f Filter) ([]UnassignedNotification, error) {
	q.listCalls++
	q.listedFilter = f
	q.listedCtx = ctx
	return q.listOut, q.listErr
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
func (f *fakeLogger) Error(_ string, _ ...any)    {}
func (f *fakeLogger) With(_ ...any) logger.Logger { return f }
func (f *fakeLogger) Sync() error                 { return nil }

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

// doAdminRequest は AdminHandler を本番同様に親 chi router へ
// `Mount("/notifications/unassigned", h)` した上で GET /notifications/unassigned?<rawQuery> を
// 実行する helper。
//
// cmd/api の `routers.Admin.Mount("/notifications/unassigned", h)` 配線（実 path
// `/api/admin/notifications/unassigned`）を忠実に再現することで、chi の prefix strip 後に
// handler 内の root route（`/`）が成立する経路を検証する。
func doAdminRequest(t *testing.T, h *AdminHandler, rawQuery string) *httptest.ResponseRecorder {
	t.Helper()
	router := chi.NewRouter()
	router.Mount("/notifications/unassigned", h)

	rec := httptest.NewRecorder()
	target := "/notifications/unassigned"
	if rawQuery != "" {
		target += "?" + rawQuery
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	router.ServeHTTP(rec, req)
	return rec
}

// decodeDTOs は 200 応答 body を []UnassignedNotificationDTO に decode する helper。
func decodeDTOs(t *testing.T, rec *httptest.ResponseRecorder) []UnassignedNotificationDTO {
	t.Helper()
	var dtos []UnassignedNotificationDTO
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

// ---- (a) from / to / type が Filter に正しく写像される（Req 4.2） ----

func TestAdminHandler_List_MapsAllFilterFields(t *testing.T) {
	// Arrange
	queue := &fakeUnassignedQueue{listOut: []UnassignedNotification{}}
	h := NewAdminHandler(queue, &fakeLogger{})
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	rawQuery := "from=" + from.Format(time.RFC3339) +
		"&to=" + to.Format(time.RFC3339) + "&type=ENROLLMENT"

	// Act
	rec := doAdminRequest(t, h, rawQuery)

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 body=%q", rec.Code, rec.Body.String())
	}
	if queue.listCalls != 1 {
		t.Fatalf("queue.List 呼び出し回数 = %d; want 1", queue.listCalls)
	}
	got := queue.listedFilter
	if got.From == nil || !got.From.Equal(from) {
		t.Errorf("Filter.From = %v; want %v", got.From, from)
	}
	if got.To == nil || !got.To.Equal(to) {
		t.Errorf("Filter.To = %v; want %v", got.To, to)
	}
	if got.Type != "ENROLLMENT" {
		t.Errorf("Filter.Type = %q; want ENROLLMENT", got.Type)
	}
}

// ---- (b) from のみ / to のみ / type のみ指定が Filter に反映される（Req 4.2 / 境界・部分指定） ----

func TestAdminHandler_List_PartialFilters_ReflectedInFilter(t *testing.T) {
	from := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name     string
		rawQuery string
		wantFrom *time.Time
		wantTo   *time.Time
		wantType string
	}{
		{name: "from のみ", rawQuery: "from=" + from.Format(time.RFC3339), wantFrom: &from},
		{name: "to のみ", rawQuery: "to=" + to.Format(time.RFC3339), wantTo: &to},
		{name: "type のみ", rawQuery: "type=COMMAND", wantType: "COMMAND"},
		{name: "未指定（全件）", rawQuery: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			queue := &fakeUnassignedQueue{listOut: []UnassignedNotification{}}
			h := NewAdminHandler(queue, &fakeLogger{})

			// Act
			rec := doAdminRequest(t, h, tc.rawQuery)

			// Assert
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d; want 200 body=%q", rec.Code, rec.Body.String())
			}
			got := queue.listedFilter
			if tc.wantFrom == nil {
				if got.From != nil {
					t.Errorf("from 未指定なのに Filter.From が設定されている: %v", got.From)
				}
			} else if got.From == nil || !got.From.Equal(*tc.wantFrom) {
				t.Errorf("Filter.From = %v; want %v", got.From, *tc.wantFrom)
			}
			if tc.wantTo == nil {
				if got.To != nil {
					t.Errorf("to 未指定なのに Filter.To が設定されている: %v", got.To)
				}
			} else if got.To == nil || !got.To.Equal(*tc.wantTo) {
				t.Errorf("Filter.To = %v; want %v", got.To, *tc.wantTo)
			}
			if got.Type != tc.wantType {
				t.Errorf("Filter.Type = %q; want %q", got.Type, tc.wantType)
			}
		})
	}
}

// ---- (c) 不正 from / to（非 RFC3339）/ type（許可外）で 400 + 不正項目提示（Req 4.5 / 異常系） ----

func TestAdminHandler_List_InvalidQuery_Returns400WithInvalidItem(t *testing.T) {
	cases := []struct {
		name     string
		rawQuery string
		wantMsg  string // 不正項目を提示する固定文言（Req 4.5）
	}{
		{name: "from 非 RFC3339", rawQuery: "from=not-a-date", wantMsg: "invalid from"},
		{name: "to 非 RFC3339", rawQuery: "to=2026-13-99", wantMsg: "invalid to"},
		{name: "type 許可外", rawQuery: "type=USAGE_LOGS_UPLOADED", wantMsg: "invalid type"},
		{name: "type 空白混入", rawQuery: "type=enrollment", wantMsg: "invalid type"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			queue := &fakeUnassignedQueue{}
			log := &fakeLogger{}
			h := NewAdminHandler(queue, log)

			// Act
			rec := doAdminRequest(t, h, tc.rawQuery)

			// Assert
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400 body=%q", rec.Code, rec.Body.String())
			}
			if queue.listCalls != 0 {
				t.Errorf("400 で queue.List が呼ばれてはならない（呼び出し回数=%d）", queue.listCalls)
			}
			if code := decodeErrCode(t, rec); code != string(internalerrors.CodeInvalidRequest) {
				t.Errorf("body.Code = %q; want %q", code, internalerrors.CodeInvalidRequest)
			}
			if msg := decodeErrMessage(t, rec); msg != tc.wantMsg {
				t.Errorf("body.message = %q; want %q（不正項目提示 / Req 4.5）", msg, tc.wantMsg)
			}
			assertWarnFailureKind(t, log, failureKindParseInvalid)
		})
	}
}

// ---- (d) queue が空 slice を返したら 200 + [] （Req 4.1 / 空入力） ----

func TestAdminHandler_List_EmptyResult_Returns200EmptyArray(t *testing.T) {
	// Arrange
	queue := &fakeUnassignedQueue{listOut: []UnassignedNotification{}}
	h := NewAdminHandler(queue, &fakeLogger{})

	// Act
	rec := doAdminRequest(t, h, "")

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	// JSON は null ではなく空配列 [] であること（Req 4.1）。
	if got := rec.Body.String(); got != "[]\n" {
		t.Errorf("body = %q; want %q", got, "[]\n")
	}
	dtos := decodeDTOs(t, rec)
	if len(dtos) != 0 {
		t.Errorf("DTO 件数 = %d; want 0", len(dtos))
	}
}

// ---- (e) queue が退避通知を返したら 200 + DTO 配列（Req 4.1 / 正常系） ----

func TestAdminHandler_List_ReturnsNotifications(t *testing.T) {
	// Arrange
	id := uuid.New()
	received := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	queue := &fakeUnassignedQueue{listOut: []UnassignedNotification{
		{
			ID:               id,
			MessageID:        "msg-1",
			NotificationType: Enrollment,
			EnterpriseName:   "enterprises/LC0123",
			Payload:          []byte(`{"k":"v"}`),
			ReceivedAt:       received,
		},
	}}
	h := NewAdminHandler(queue, &fakeLogger{})

	// Act
	rec := doAdminRequest(t, h, "")

	// Assert
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 body=%q", rec.Code, rec.Body.String())
	}
	dtos := decodeDTOs(t, rec)
	if len(dtos) != 1 {
		t.Fatalf("DTO 件数 = %d; want 1", len(dtos))
	}
	got := dtos[0]
	if got.ID != id.String() {
		t.Errorf("DTO.ID = %q; want %q", got.ID, id.String())
	}
	if got.MessageID != "msg-1" {
		t.Errorf("DTO.MessageID = %q; want msg-1", got.MessageID)
	}
	if got.NotificationType != "ENROLLMENT" {
		t.Errorf("DTO.NotificationType = %q; want ENROLLMENT", got.NotificationType)
	}
	if got.EnterpriseName != "enterprises/LC0123" {
		t.Errorf("DTO.EnterpriseName = %q; want enterprises/LC0123", got.EnterpriseName)
	}
	if got.ReceivedAt != received.Format(time.RFC3339) {
		t.Errorf("DTO.ReceivedAt = %q; want %q", got.ReceivedAt, received.Format(time.RFC3339))
	}
	if string(got.Payload) != `{"k":"v"}` {
		t.Errorf("DTO.Payload = %q; want %q", string(got.Payload), `{"k":"v"}`)
	}
}

// ---- (f) queue.List が DB エラーを返したら 503 + failure_kind=query_error（NFR 3.1 / 異常系） ----

func TestAdminHandler_List_QueueError_Returns503(t *testing.T) {
	// Arrange: queue が SELECT / scan の DB 失敗（CodeUnavailable）を返す。
	queue := &fakeUnassignedQueue{listErr: internalerrors.New(internalerrors.CodeUnavailable, "db down")}
	log := &fakeLogger{}
	h := NewAdminHandler(queue, log)

	// Act
	rec := doAdminRequest(t, h, "")

	// Assert
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503 body=%q", rec.Code, rec.Body.String())
	}
	if queue.listCalls != 1 {
		t.Errorf("queue.List 呼び出し回数 = %d; want 1（filter parse は通過し List で失敗する経路）", queue.listCalls)
	}
	if code := decodeErrCode(t, rec); code != string(internalerrors.CodeUnavailable) {
		t.Errorf("body.Code = %q; want %q", code, internalerrors.CodeUnavailable)
	}
	assertWarnFailureKind(t, log, failureKindQueryError)
}

// decodeErrMessage は 4xx/5xx 応答 body の message を取り出す helper（不正項目提示の検証用 / Req 4.5）。
func decodeErrMessage(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v body=%q", err, rec.Body.String())
	}
	return body.Message
}
