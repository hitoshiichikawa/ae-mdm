package tenantaudit

import (
	"context"
	stderrors "errors"
	"testing"

	"github.com/google/uuid"

	"github.com/hitoshiichikawa/ae-mdm/internal/audit"
	"github.com/hitoshiichikawa/ae-mdm/internal/tenant"
)

// ---- Fake audit.Service ----

// spyAuditService は audit.Service の test double（in-memory spy）。
//
// Record で受け取った audit.Event を捕捉し、任意のエラーを返せる。List は本アダプタが
// 呼ばないため未使用（interface 充足のためのスタブ）。永続化という外部副作用のみを fake 化し、
// アダプタ自身の純粋な写像ロジックはモックしない（テスト規約: 自分の純粋ロジックはモックしない）。
type spyAuditService struct {
	recordCalls   int
	recordedEvent audit.Event
	recordErr     error
}

func (s *spyAuditService) Record(_ context.Context, ev audit.Event) error {
	s.recordCalls++
	s.recordedEvent = ev
	return s.recordErr
}

func (s *spyAuditService) List(_ context.Context, _ audit.Filter) ([]audit.Event, error) {
	return nil, nil
}

// 静的型アサーション: spyAuditService が audit.Service を満たすこと。
var _ audit.Service = (*spyAuditService)(nil)

// 静的型アサーション: *Recorder が tenant.EventRecorder を満たすこと（Req 5.1）。
var _ tenant.EventRecorder = (*Recorder)(nil)

// ---- 写像の正常系（Req 2.1〜2.6 / 2.8）----

func TestRecorder_Record_OperationMapping(t *testing.T) {
	tests := []struct {
		name          string
		op            tenant.Operation
		result        tenant.Result
		wantEventType audit.EventType
		wantResult    audit.ResultType
	}{
		{
			name:          "create 操作が成功のとき tenant_create / success へ写像される",
			op:            tenant.OperationCreate,
			result:        tenant.ResultSuccess,
			wantEventType: EventTypeCreate,
			wantResult:    audit.ResultSuccess,
		},
		{
			name:          "bind 操作が成功のとき tenant_bind / success へ写像される",
			op:            tenant.OperationBind,
			result:        tenant.ResultSuccess,
			wantEventType: EventTypeBind,
			wantResult:    audit.ResultSuccess,
		},
		{
			name:          "disable 操作が失敗のとき tenant_disable / failure へ写像される",
			op:            tenant.OperationDisable,
			result:        tenant.ResultFailure,
			wantEventType: EventTypeDisable,
			wantResult:    audit.ResultFailure,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange (Req 2.1 / 2.2 / 2.3 / 2.4〜2.6 / 2.8)
			spy := &spyAuditService{}
			rec := NewRecorder(spy)
			actor := uuid.New()
			tenantID := uuid.New()
			ev := tenant.Event{
				Actor:     actor,
				TenantID:  tenantID,
				Operation: tt.op,
				Result:    tt.result,
			}

			// Act
			err := rec.Record(context.Background(), ev)

			// Assert
			if err != nil {
				t.Fatalf("Record() 予期しないエラー: %v", err)
			}
			if spy.recordCalls != 1 {
				t.Fatalf("audit.Service.Record 呼び出し回数 = %d, want 1", spy.recordCalls)
			}
			got := spy.recordedEvent
			if got.EventType != tt.wantEventType {
				t.Errorf("EventType = %q, want %q", got.EventType, tt.wantEventType)
			}
			if got.Result != tt.wantResult {
				t.Errorf("Result = %q, want %q", got.Result, tt.wantResult)
			}
			if got.ActorID != actor {
				t.Errorf("ActorID = %v, want %v (Req 2.1)", got.ActorID, actor)
			}
			if got.TenantID != tenantID {
				t.Errorf("TenantID = %v, want %v (Req 2.2)", got.TenantID, tenantID)
			}
			if got.ResourceID != tenantID.String() {
				t.Errorf("ResourceID = %q, want %q (Req 2.8)", got.ResourceID, tenantID.String())
			}
		})
	}
}

// ---- 結果区分の成功写像（Req 2.3 正常系の片側）----

func TestRecorder_Record_ResultSuccessMapping(t *testing.T) {
	// Arrange (Req 2.3)
	spy := &spyAuditService{}
	rec := NewRecorder(spy)
	ev := tenant.Event{
		Actor:     uuid.New(),
		TenantID:  uuid.New(),
		Operation: tenant.OperationCreate,
		Result:    tenant.ResultSuccess,
	}

	// Act
	if err := rec.Record(context.Background(), ev); err != nil {
		t.Fatalf("Record() 予期しないエラー: %v", err)
	}

	// Assert
	if spy.recordedEvent.Result != audit.ResultSuccess {
		t.Errorf("Result = %q, want %q", spy.recordedEvent.Result, audit.ResultSuccess)
	}
}

// ---- ID / OccurredAt の非設定委譲（Req 2.9）----

func TestRecorder_Record_DelegatesIDAndOccurredAt(t *testing.T) {
	// Arrange (Req 2.9): アダプタは ID / OccurredAt を自前確定せず zero で渡す。
	spy := &spyAuditService{}
	rec := NewRecorder(spy)
	ev := tenant.Event{
		Actor:     uuid.New(),
		TenantID:  uuid.New(),
		Operation: tenant.OperationCreate,
		Result:    tenant.ResultSuccess,
	}

	// Act
	if err := rec.Record(context.Background(), ev); err != nil {
		t.Fatalf("Record() 予期しないエラー: %v", err)
	}

	// Assert
	got := spy.recordedEvent
	if got.ID != uuid.Nil {
		t.Errorf("ID = %v, want uuid.Nil（採番は audit.Service の責務 / Req 2.9）", got.ID)
	}
	if !got.OccurredAt.IsZero() {
		t.Errorf("OccurredAt = %v, want zero（clock 補完は audit.Service の責務 / Req 2.9）", got.OccurredAt)
	}
}

// ---- fail-closed（Req 3.1 / 3.2）----

func TestRecorder_Record_PropagatesPersistError(t *testing.T) {
	// Arrange (Req 3.1 / 3.2): audit.Service が永続化エラーを返す。
	wantErr := stderrors.New("persist failed")
	spy := &spyAuditService{recordErr: wantErr}
	rec := NewRecorder(spy)
	ev := tenant.Event{
		Actor:     uuid.New(),
		TenantID:  uuid.New(),
		Operation: tenant.OperationCreate,
		Result:    tenant.ResultSuccess,
	}

	// Act
	err := rec.Record(context.Background(), ev)

	// Assert: アダプタ境界で握りつぶさず、そのまま伝播する。
	if !stderrors.Is(err, wantErr) {
		t.Errorf("Record() err = %v, want %v（アダプタ境界で握りつぶさない / Req 3.1 / 3.2）", err, wantErr)
	}
}

// ---- Detail: 非機密フィールドのみ載る（Req 4.1 / 4.2）----

func TestRecorder_Record_DetailCarriesNonSensitiveFields(t *testing.T) {
	// Arrange (Req 4.2): 二段階確認完了 + 拒否理由を持つ failure イベント。
	spy := &spyAuditService{}
	rec := NewRecorder(spy)
	ev := tenant.Event{
		Actor:                 uuid.New(),
		TenantID:              uuid.New(),
		Operation:             tenant.OperationDisable,
		Result:                tenant.ResultFailure,
		ConfirmationCompleted: true,
		DenyReason:            "two-step confirmation is required",
	}

	// Act
	if err := rec.Record(context.Background(), ev); err != nil {
		t.Fatalf("Record() 予期しないエラー: %v", err)
	}

	// Assert: 非機密フィールドが機械可読な鍵名で載る。
	detail := spy.recordedEvent.Detail
	if got, ok := detail["confirmation_completed"].(bool); !ok || got != true {
		t.Errorf("detail[confirmation_completed] = %v, want true", detail["confirmation_completed"])
	}
	if got, ok := detail["deny_reason"].(string); !ok || got != "two-step confirmation is required" {
		t.Errorf("detail[deny_reason] = %v, want 拒否理由文字列", detail["deny_reason"])
	}
}

// ---- Detail: 機密に相当する値が載らない（Req 4.1 / 4.3）----

func TestRecorder_Record_DetailHasNoSensitiveKeys(t *testing.T) {
	// Arrange (Req 4.1 / 4.3): tenant.Event は機密フィールドを構造的に持たない。
	spy := &spyAuditService{}
	rec := NewRecorder(spy)
	ev := tenant.Event{
		Actor:                 uuid.New(),
		TenantID:              uuid.New(),
		Operation:             tenant.OperationCreate,
		Result:                tenant.ResultSuccess,
		ConfirmationCompleted: true,
		DenyReason:            "some reason",
	}

	// Act
	if err := rec.Record(context.Background(), ev); err != nil {
		t.Fatalf("Record() 予期しないエラー: %v", err)
	}

	// Assert: Detail の鍵は非機密の許可リストに限定される。
	allowed := map[string]bool{
		"confirmation_completed": true,
		"deny_reason":            true,
	}
	for key := range spy.recordedEvent.Detail {
		if !allowed[key] {
			t.Errorf("detail に想定外の鍵 %q が載っている（機密漏洩の疑い / Req 4.1）", key)
		}
	}
}

// ---- Detail: 載せ得る非機密値が無い場合の扱い（Req 4.4）----

func TestRecorder_Record_DenyReasonEmpty_OmitsDenyReasonKey(t *testing.T) {
	// Arrange (Req 4.4): DenyReason 空のときは deny_reason 鍵を載せない。
	spy := &spyAuditService{}
	rec := NewRecorder(spy)
	ev := tenant.Event{
		Actor:                 uuid.New(),
		TenantID:              uuid.New(),
		Operation:             tenant.OperationCreate,
		Result:                tenant.ResultSuccess,
		ConfirmationCompleted: false,
		DenyReason:            "",
	}

	// Act
	if err := rec.Record(context.Background(), ev); err != nil {
		t.Fatalf("Record() 予期しないエラー: %v", err)
	}

	// Assert: deny_reason 鍵は存在しない。
	if _, ok := spy.recordedEvent.Detail["deny_reason"]; ok {
		t.Errorf("DenyReason が空なのに deny_reason 鍵が載っている（Req 4.4）")
	}
}

// ---- 結果区分の境界防御: 未知 / zero 値は failure へ倒す（Req 2.3 異常系）----

func TestRecorder_Record_UnknownResult_MapsToFailure(t *testing.T) {
	// Arrange (境界防御): tenant.Result は string 型のため zero 値 / 未定義値を取り得る。
	// これらを成功扱いにすると、本当は失敗した操作が success として監査記録される危険があるため、
	// 明示的な success 以外はすべて failure へ倒すことを検証する。
	cases := []struct {
		name   string
		result tenant.Result
	}{
		{name: "zero 値の Result は failure へ倒れる", result: tenant.Result("")},
		{name: "未知の Result 文字列は failure へ倒れる", result: tenant.Result("partial")},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			spy := &spyAuditService{}
			rec := NewRecorder(spy)
			ev := tenant.Event{
				Actor:     uuid.New(),
				TenantID:  uuid.New(),
				Operation: tenant.OperationCreate,
				Result:    c.result,
			}

			// Act
			if err := rec.Record(context.Background(), ev); err != nil {
				t.Fatalf("Record() 予期しないエラー: %v", err)
			}

			// Assert: 未知 / zero 値は success と誤認せず failure へ写像する。
			if spy.recordedEvent.Result != audit.ResultFailure {
				t.Errorf("Result = %q, want %q（未知値を成功扱いにしない境界防御 / Req 2.3）",
					spy.recordedEvent.Result, audit.ResultFailure)
			}
		})
	}
}

// ---- 未知 Operation（Req 2.7 異常系）----

func TestRecorder_Record_UnknownOperation_MapsToIdentifiableType(t *testing.T) {
	// Arrange (Req 2.7): 定義外 Operation。
	spy := &spyAuditService{}
	rec := NewRecorder(spy)
	ev := tenant.Event{
		Actor:     uuid.New(),
		TenantID:  uuid.New(),
		Operation: tenant.Operation("teleport"), // 定義外
		Result:    tenant.ResultSuccess,
	}

	// Act
	if err := rec.Record(context.Background(), ev); err != nil {
		t.Fatalf("Record() 予期しないエラー: %v", err)
	}

	// Assert: 無音破棄せず、識別可能な未知種別へ写像し、委譲する。
	if spy.recordCalls != 1 {
		t.Fatalf("未知 Operation が無音破棄された（Record 呼び出し回数 = %d, want 1 / Req 2.7）", spy.recordCalls)
	}
	if spy.recordedEvent.EventType != EventTypeUnknown {
		t.Errorf("EventType = %q, want %q（未知種別へ写像 / Req 2.7）", spy.recordedEvent.EventType, EventTypeUnknown)
	}
}

// ---- 既知 EventType 定数値の固定（NFR 1.2）----

func TestEventTypeConstants_FixedStringValues(t *testing.T) {
	// Arrange / Act / Assert (NFR 1.2): 絞り込み条件として一貫利用できる固定値を保証。
	cases := []struct {
		got  audit.EventType
		want string
	}{
		{EventTypeCreate, "tenant_create"},
		{EventTypeBind, "tenant_bind"},
		{EventTypeDisable, "tenant_disable"},
	}
	for _, c := range cases {
		if string(c.got) != c.want {
			t.Errorf("EventType 定数値 = %q, want %q（NFR 1.2 固定値）", c.got, c.want)
		}
	}
}
