package tenantaudit

import (
	"context"
	stderrors "errors"
	"testing"

	"github.com/google/uuid"

	"github.com/hitoshiichikawa/ae-mdm/internal/audit"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
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
		{
			// #52 Req 2.4: RecoverStaleBindings が発火する recover 監査イベントを、
			// tenant_unknown ではなく識別可能な tenant_recover へ写像することを保証する。
			name:          "recover 操作が成功のとき tenant_recover / success へ写像される",
			op:            tenant.OperationRecover,
			result:        tenant.ResultSuccess,
			wantEventType: EventTypeRecover,
			wantResult:    audit.ResultSuccess,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange (Req 2.1 / 2.2 / 2.3 / 2.4〜2.6 / 2.8)
			spy := &spyAuditService{}
			rec := NewRecorder(spy, nil)
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
	rec := NewRecorder(spy, nil)
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
	rec := NewRecorder(spy, nil)
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
	rec := NewRecorder(spy, nil)
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
	rec := NewRecorder(spy, nil)
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
	rec := NewRecorder(spy, nil)
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
	rec := NewRecorder(spy, nil)
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
			rec := NewRecorder(spy, nil)
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
	rec := NewRecorder(spy, nil)
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

// ---- 観測ログ: 永続化成功時に結果を識別可能な構造化ログを出す（NFR 2.1）----

// spyLogger は logger.Logger の test double。Info / Warn の呼び出し回数とフィールドを捕捉し、
// 観測ログ（NFR 2.1）の有無・レベル・フィールドを検証する。永続化という外部副作用は audit.Service
// 側で fake 化済みのため、ここではログ出力の副作用のみを捕捉する。
type spyLogger struct {
	infoCalls int
	warnCalls int
	lastMsg   string
	lastFlds  []any
}

func (l *spyLogger) Info(msg string, fields ...any) {
	l.infoCalls++
	l.lastMsg = msg
	l.lastFlds = fields
}

func (l *spyLogger) Warn(msg string, fields ...any) {
	l.warnCalls++
	l.lastMsg = msg
	l.lastFlds = fields
}

func (l *spyLogger) Debug(_ string, _ ...any)    {}
func (l *spyLogger) Error(_ string, _ ...any)    {}
func (l *spyLogger) With(_ ...any) logger.Logger { return l }
func (l *spyLogger) Sync() error                 { return nil }

// 静的型アサーション: spyLogger が logger.Logger を満たすこと。
var _ logger.Logger = (*spyLogger)(nil)

// hasStringField は構造化フィールド列に指定文字列（key 名 / 値）が含まれるかを返す。
// fields は plain な key/value ペアと zap.Field（actor_id / tenant_id）が混在するため、
// 文字列要素の有無を走査する（厳密なペア解析は行わない）。
func hasStringField(fields []any, s string) bool {
	for _, f := range fields {
		if str, ok := f.(string); ok && str == s {
			return true
		}
	}
	return false
}

func TestRecorder_Record_LogsOutcomeOnPersistSuccess(t *testing.T) {
	tests := []struct {
		name          string
		result        tenant.Result
		denyReason    string
		wantInfoCalls int
		wantWarnCalls int
		wantResultStr string
	}{
		{
			name:          "操作成功が永続化されたとき Info で結果 success を識別可能に出す",
			result:        tenant.ResultSuccess,
			denyReason:    "",
			wantInfoCalls: 1,
			wantWarnCalls: 0,
			wantResultStr: string(tenant.ResultSuccess),
		},
		{
			name:          "拒否（操作失敗）が永続化されたとき Warn で結果 failure を識別可能に出す",
			result:        tenant.ResultFailure,
			denyReason:    "two-step confirmation is required",
			wantInfoCalls: 0,
			wantWarnCalls: 1,
			wantResultStr: string(tenant.ResultFailure),
		},
		{
			// 境界防御の観測整合: zero 値の Result は mapResult が audit row 上 failure へ倒すため、
			// 観測ログも raw な result="" / Info ではなく、永続化結果に揃えた failure / Warn で出す
			// （NFR 2.1 の結果識別と fail-closed の観測性 / 監査証跡とログの乖離防止）。
			name:          "zero 値の Result が永続化されたとき audit row と整合して Warn で failure を出す",
			result:        tenant.Result(""),
			denyReason:    "",
			wantInfoCalls: 0,
			wantWarnCalls: 1,
			wantResultStr: string(audit.ResultFailure),
		},
		{
			// 同上: 未知の Result 文字列も mapResult が failure へ倒すため、観測ログも failure / Warn。
			name:          "未知の Result 文字列が永続化されたとき audit row と整合して Warn で failure を出す",
			result:        tenant.Result("partial"),
			denyReason:    "",
			wantInfoCalls: 0,
			wantWarnCalls: 1,
			wantResultStr: string(audit.ResultFailure),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange (NFR 2.1): 永続化が成功する spy と観測ログ spy を配線する。
			spy := &spyAuditService{}
			log := &spyLogger{}
			rec := NewRecorder(spy, log)
			ev := tenant.Event{
				Actor:                 uuid.New(),
				TenantID:              uuid.New(),
				Operation:             tenant.OperationDisable,
				Result:                tt.result,
				ConfirmationCompleted: true,
				DenyReason:            tt.denyReason,
			}

			// Act
			if err := rec.Record(context.Background(), ev); err != nil {
				t.Fatalf("Record() 予期しないエラー: %v", err)
			}

			// Assert: 永続化成功時に結果に応じた level で観測ログが 1 件出る（NFR 2.1）。
			if log.infoCalls != tt.wantInfoCalls {
				t.Errorf("Info 呼び出し回数 = %d, want %d（NFR 2.1 成功経路の観測ログ）", log.infoCalls, tt.wantInfoCalls)
			}
			if log.warnCalls != tt.wantWarnCalls {
				t.Errorf("Warn 呼び出し回数 = %d, want %d（NFR 2.1 / 結果区分で level 分岐）", log.warnCalls, tt.wantWarnCalls)
			}
			// 結果が構造化ログで識別可能であること（result 値が載る / NFR 2.1）。
			if !hasStringField(log.lastFlds, tt.wantResultStr) {
				t.Errorf("観測ログに result=%q が載らない: fields=%v（NFR 2.1）", tt.wantResultStr, log.lastFlds)
			}
		})
	}
}

// ---- 観測ログ: 拒否理由など非機密フィールドのみが載り、機密鍵は載らない（NFR 2.1 / 2.3）----

func TestRecorder_Record_PersistSuccessLog_CarriesOnlyNonSensitiveFields(t *testing.T) {
	// Arrange (NFR 2.3): 二段階確認完了 + 拒否理由を持つ failure イベントが永続化成功する。
	spy := &spyAuditService{}
	log := &spyLogger{}
	rec := NewRecorder(spy, log)
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

	// Assert: 非機密フィールド（confirmation_completed / deny_reason）が載る。
	if !hasStringField(log.lastFlds, "confirmation_completed") {
		t.Errorf("観測ログに confirmation_completed が載らない: fields=%v（NFR 2.1）", log.lastFlds)
	}
	if !hasStringField(log.lastFlds, "deny_reason") {
		t.Errorf("拒否イベントの観測ログに deny_reason が載らない: fields=%v（NFR 2.1 / 2.2）", log.lastFlds)
	}
}

// ---- 観測ログ: 永続化失敗時は重複出力しない（失敗ログは audit.Service の責務 / NFR 2.1）----

func TestRecorder_Record_PersistError_DoesNotEmitSuccessLog(t *testing.T) {
	// Arrange (NFR 2.1): 永続化が失敗する。失敗経路の構造化ログ（failure_kind=persist_error）は
	// audit.Service が担うため、アダプタは成功経路ログを重複出力しない。
	spy := &spyAuditService{recordErr: stderrors.New("persist failed")}
	log := &spyLogger{}
	rec := NewRecorder(spy, log)
	ev := tenant.Event{
		Actor:     uuid.New(),
		TenantID:  uuid.New(),
		Operation: tenant.OperationBind,
		Result:    tenant.ResultFailure,
	}

	// Act
	if err := rec.Record(context.Background(), ev); err == nil {
		t.Fatalf("Record() は永続化失敗を伝播するはず（fail-closed / Req 3.1 / 3.2）")
	}

	// Assert: 永続化失敗時にアダプタは観測ログ（成功経路ログ）を出さない（二重出力回避）。
	if log.infoCalls != 0 || log.warnCalls != 0 {
		t.Errorf("永続化失敗時にアダプタが観測ログを出した: Info=%d Warn=%d, want 0/0（失敗ログは audit.Service の責務）",
			log.infoCalls, log.warnCalls)
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
		{EventTypeRecover, "tenant_recover"},
	}
	for _, c := range cases {
		if string(c.got) != c.want {
			t.Errorf("EventType 定数値 = %q, want %q（NFR 1.2 固定値）", c.got, c.want)
		}
	}
}
