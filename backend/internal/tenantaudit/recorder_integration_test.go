package tenantaudit

import (
	"context"
	stderrors "errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/hitoshiichikawa/ae-mdm/internal/audit"
	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	"github.com/hitoshiichikawa/ae-mdm/internal/tenant"
)

// 本ファイルは Recorder を **実 audit.Service**（spy ではなく `audit.NewService` の本番実装）へ
// 配線し、DB 境界（audit.Repository）のみを fake 化した結合寄りのテスト。
//
// 本リポジトリには実 PostgreSQL を起動する結合テスト基盤が存在しない（`internal/platform/db`
// の DB 系テストも `fakeTx` mock、`internal/audit/repository_test.go` は純粋関数 buildSelectQuery
// のみを検証）。そのため `audit_logs` の FK / RLS / jsonb 永続化そのものを通す結合テストは
// 本 PR スコープ（tenant 監査配線 = 利用・配線のみ）外であり別途基盤導入を要する。
// 代替として、自作ロジックである **audit.Service を実体のまま**通し（テスト規約: 自分の純粋
// ロジックはモックしない）、ID 採番・clock 補完・永続化失敗の伝播という Service 契約まで含めた
// 写像 → 委譲の経路を検証する。これにより spy だけでは確認できなかった main.go 配線相当の
// 経路（Req 1.1 / 1.3 / 2.x / 2.9 / 3.1 / 3.2）を unit-level で押さえる。

// fakeAuditRepo は audit.Repository の test double。DB 境界（audit_logs への INSERT / SELECT）
// のみを fake 化し、Insert で受け取った Event を捕捉して任意のエラーを返せる。
type fakeAuditRepo struct {
	inserts   []audit.Event
	insertErr error
}

func (r *fakeAuditRepo) Insert(_ context.Context, ev audit.Event) error {
	r.inserts = append(r.inserts, ev)
	return r.insertErr
}

func (r *fakeAuditRepo) Select(_ context.Context, _ audit.Filter, _ time.Time) ([]audit.Event, error) {
	return nil, nil
}

// 静的型アサーション: fakeAuditRepo が audit.Repository を満たすこと。
var _ audit.Repository = (*fakeAuditRepo)(nil)

// fixedClock は audit.Clock の決定的 test double（採番後の OccurredAt を確定値で検証するため）。
type fixedClock struct {
	now time.Time
}

func (c fixedClock) Now() time.Time { return c.now }

// 静的型アサーション: fixedClock が audit.Clock を満たすこと。
var _ audit.Clock = fixedClock{}

// ---- 実 audit.Service 経由の写像 → 永続化境界委譲（Req 1.1 / 2.1〜2.3 / 2.8 / 2.9）----

func TestRecorder_ThroughRealAuditService_PersistsMappedEvent(t *testing.T) {
	// Arrange: 実 audit.Service（本番実装）+ fake Repository へ配線する。
	// cfg は Record 経路で未使用（保持期間は List のみ参照）のため zero 値で足りる。
	repo := &fakeAuditRepo{}
	clk := fixedClock{now: time.Date(2026, 6, 29, 9, 0, 0, 0, time.UTC)}
	svc := audit.NewService(config.Config{}, repo, clk, nil)
	rec := NewRecorder(svc)

	actor := uuid.New()
	tenantID := uuid.New()
	ev := tenant.Event{
		Actor:     actor,
		TenantID:  tenantID,
		Operation: tenant.OperationCreate,
		Result:    tenant.ResultSuccess,
	}

	// Act
	if err := rec.Record(context.Background(), ev); err != nil {
		t.Fatalf("Record() 予期しないエラー: %v", err)
	}

	// Assert: Repository.Insert へ写像済み Event が 1 件だけ到達する（配線 + 委譲）。
	if len(repo.inserts) != 1 {
		t.Fatalf("Repository.Insert 到達件数 = %d, want 1（実 Service 経由の委譲）", len(repo.inserts))
	}
	got := repo.inserts[0]

	// 写像内容（Req 2.1 / 2.2 / 2.3 / 2.8 + 種別）。
	if got.EventType != EventTypeCreate {
		t.Errorf("EventType = %q, want %q", got.EventType, EventTypeCreate)
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
	if got.Result != audit.ResultSuccess {
		t.Errorf("Result = %q, want %q (Req 2.3)", got.Result, audit.ResultSuccess)
	}

	// アダプタは ID / OccurredAt を zero で渡し、実 Service が採番・clock 補完する（Req 2.9）。
	if got.ID == uuid.Nil {
		t.Errorf("ID = uuid.Nil, want 採番済み（audit.Service が補完 / Req 2.9）")
	}
	if !got.OccurredAt.Equal(clk.now) {
		t.Errorf("OccurredAt = %v, want %v（audit.Service が clock 補完 / Req 2.9）", got.OccurredAt, clk.now)
	}
}

// ---- 実 audit.Service 経由でも fail-closed が成立する（Req 3.1 / 3.2）----

func TestRecorder_ThroughRealAuditService_PropagatesPersistError(t *testing.T) {
	// Arrange (Req 3.1 / 3.2): Repository が永続化エラーを返す。
	// `audit_logs.tenant_id` の FK 違反など、存在しないテナントに対する失敗監査の永続化失敗を
	// 模した経路。実 Service は warnPersistFailure 後にエラーを return し、成功扱いしない。
	wantErr := stderrors.New("audit_logs insert rejected")
	repo := &fakeAuditRepo{insertErr: wantErr}
	clk := fixedClock{now: time.Date(2026, 6, 29, 9, 0, 0, 0, time.UTC)}
	svc := audit.NewService(config.Config{}, repo, clk, nil)
	rec := NewRecorder(svc)

	ev := tenant.Event{
		Actor:     uuid.New(),
		TenantID:  uuid.New(),
		Operation: tenant.OperationBind,
		Result:    tenant.ResultFailure,
	}

	// Act
	err := rec.Record(context.Background(), ev)

	// Assert: 実 Service → アダプタ境界のいずれでも握りつぶさず、呼び出し側へ伝播する。
	if !stderrors.Is(err, wantErr) {
		t.Errorf("Record() err = %v, want %v（実 Service 経由でも fail-closed / Req 3.1 / 3.2）", err, wantErr)
	}
}
