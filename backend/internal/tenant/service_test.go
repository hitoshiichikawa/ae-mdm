package tenant

import (
	"context"
	stderrors "errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/amapi"
)

// ---- 機密値漏洩 assertion 用の固定文字列（NFR 2.2 / 2.3 観点） ----
const (
	testSignupURLSecret = "https://enterprise.google.com/signup?token=DO-NOT-LEAK"
	testSignupURLName   = "signupUrls/abc123"
	testEnterpriseName  = "enterprises/LC0123456789"
	testTenantNameValue = "Acme Corp"
)

// ---- Fake Logger（拒否経路の構造化ログ field 検証 / NFR 2.2） ----

type fakeLogger struct {
	mu      sync.Mutex
	entries []fakeLogEntry
}

type fakeLogEntry struct {
	Level  string
	Msg    string
	Fields map[string]any
}

func (l *fakeLogger) record(level, msg string, fields ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	f := map[string]any{}
	for i := 0; i+1 < len(fields); i += 2 {
		// logger.ActorID / logger.TenantID は zap.Field を返すため、key/value ペアの
		// string key のみを map 化する（zap.Field 直接渡しは構造化済みなので本 fake では
		// "deny_reason" 等の素の key/value ペアの検証に用いる）。
		k, ok := fields[i].(string)
		if !ok {
			continue
		}
		f[k] = fields[i+1]
	}
	l.entries = append(l.entries, fakeLogEntry{Level: level, Msg: msg, Fields: f})
}

func (l *fakeLogger) Debug(msg string, fields ...any) { l.record("debug", msg, fields...) }
func (l *fakeLogger) Info(msg string, fields ...any)  { l.record("info", msg, fields...) }
func (l *fakeLogger) Warn(msg string, fields ...any)  { l.record("warn", msg, fields...) }
func (l *fakeLogger) Error(msg string, fields ...any) { l.record("error", msg, fields...) }
func (l *fakeLogger) With(_ ...any) logger.Logger     { return l }
func (l *fakeLogger) Sync() error                     { return nil }

// warnWithDenyReason は WARN レベルかつ deny_reason field を持つエントリの有無を返す（NFR 2.2）。
func (l *fakeLogger) warnWithDenyReason() (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.entries {
		if e.Level != "warn" {
			continue
		}
		if v, ok := e.Fields["deny_reason"]; ok {
			s, _ := v.(string)
			return s, true
		}
	}
	return "", false
}

// containsSecret は記録された全エントリの key/value 文字列に機密値が混入していないか検査する。
func (l *fakeLogger) containsSecret(secret string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.entries {
		for _, v := range e.Fields {
			if s, ok := v.(string); ok && s == secret {
				return true
			}
		}
	}
	return false
}

// ---- Fake Repository ----

type fakeRepoCalls struct {
	insert int
	get    int
	list   int
}

type fakeRepository struct {
	mu sync.Mutex

	// 設定された挙動
	insertErr error
	getRow    TenantRow
	getErr    error
	listRows  []TenantRow
	listErr   error

	// 記録された入力
	calls         fakeRepoCalls
	lastInsertRow TenantRow
	lastGetID     uuid.UUID
}

func (r *fakeRepository) Insert(_ context.Context, t TenantRow) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls.insert++
	r.lastInsertRow = t
	return r.insertErr
}

func (r *fakeRepository) Get(_ context.Context, id uuid.UUID) (TenantRow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls.get++
	r.lastGetID = id
	if r.getErr != nil {
		return TenantRow{}, r.getErr
	}
	return r.getRow, nil
}

func (r *fakeRepository) List(_ context.Context) ([]TenantRow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls.list++
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.listRows, nil
}

// UpdateBound / UpdateDisabled は task 4 のスコープ外（Bind/Disable は task 5）。interface を
// 満たすためのスタブ。本 task のテストでは呼び出されない。
func (r *fakeRepository) UpdateBound(_ context.Context, _ uuid.UUID, _ string) (int64, error) {
	return 0, nil
}

func (r *fakeRepository) UpdateDisabled(_ context.Context, _ uuid.UUID, _ uuid.UUID) (int64, error) {
	return 0, nil
}

// ---- Fake EventRecorder ----

type fakeRecorder struct {
	mu     sync.Mutex
	events []Event
	err    error
}

func (r *fakeRecorder) Record(_ context.Context, e Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return r.err
}

func (r *fakeRecorder) recorded() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Event, len(r.events))
	copy(out, r.events)
	return out
}

// ---- Test harness ----

type serviceHarness struct {
	svc      Service
	repo     *fakeRepository
	stub     *amapi.StubClient
	recorder *fakeRecorder
	log      *fakeLogger
}

func newServiceHarness() *serviceHarness {
	repo := &fakeRepository{}
	stub := &amapi.StubClient{}
	recorder := &fakeRecorder{}
	log := &fakeLogger{}
	svc := NewService(repo, stub, recorder, config.Config{AMAPIProjectID: "test-project"}, log)
	return &serviceHarness{svc: svc, repo: repo, stub: stub, recorder: recorder, log: log}
}

// codeOf は err から *errors.Error の Code を抽出する（errors.As 経由）。
func codeOf(t *testing.T, err error) pkgerrors.Code {
	t.Helper()
	var de *pkgerrors.Error
	if !stderrors.As(err, &de) {
		t.Fatalf("expected *errors.Error, got %T (%v)", err, err)
	}
	return de.Code
}

// ===== Create =====

func TestService_Create(t *testing.T) {
	t.Run("name が空白 trim 後に空のとき CodeInvalidRequest を返し永続化・AMAPI 呼び出しをしない", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		actor := uuid.New()

		// Act
		_, _, err := h.svc.Create(context.Background(), actor, CreateInput{Name: "   "})

		// Assert
		if got := codeOf(t, err); got != pkgerrors.CodeInvalidRequest {
			t.Fatalf("expected CodeInvalidRequest, got %s", got)
		}
		if h.stub.CallCount("CreateSignupURL") != 0 {
			t.Errorf("CreateSignupURL must not be called on empty name, got %d", h.stub.CallCount("CreateSignupURL"))
		}
		if h.repo.calls.insert != 0 {
			t.Errorf("Insert must not be called on empty name, got %d", h.repo.calls.insert)
		}
	})

	t.Run("name が空のとき拒否経路の構造化ログ field（deny_reason）を出す", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()

		// Act
		_, _, _ = h.svc.Create(context.Background(), uuid.New(), CreateInput{Name: ""})

		// Assert
		reason, ok := h.log.warnWithDenyReason()
		if !ok {
			t.Fatalf("expected a WARN log entry with deny_reason field")
		}
		if reason == "" {
			t.Errorf("deny_reason must be non-empty")
		}
	})

	t.Run("正常入力のとき pending_bind + signup_url を返し Insert と Record(create,success) を行う", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		actor := uuid.New()
		h.stub.OnCreateSignupURL = func(_ context.Context) (string, string, error) {
			return testSignupURLSecret, testSignupURLName, nil
		}

		// Act
		view, su, err := h.svc.Create(context.Background(), actor, CreateInput{Name: testTenantNameValue})

		// Assert
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if view.Status != StatusPendingBind {
			t.Errorf("expected status pending_bind, got %s", view.Status)
		}
		if view.Name != testTenantNameValue {
			t.Errorf("expected name %q, got %q", testTenantNameValue, view.Name)
		}
		if su.URL != testSignupURLSecret {
			t.Errorf("expected signup url to be returned, got %q", su.URL)
		}
		if su.Name != testSignupURLName {
			t.Errorf("expected signup url name %q, got %q", testSignupURLName, su.Name)
		}
		if h.repo.calls.insert != 1 {
			t.Fatalf("expected Insert to be called once, got %d", h.repo.calls.insert)
		}
		if h.repo.lastInsertRow.Status != StatusPendingBind {
			t.Errorf("inserted row status must be pending_bind, got %s", h.repo.lastInsertRow.Status)
		}
		if h.repo.lastInsertRow.ID != view.ID {
			t.Errorf("inserted row id %s must match returned view id %s", h.repo.lastInsertRow.ID, view.ID)
		}
		events := h.recorder.recorded()
		if len(events) != 1 {
			t.Fatalf("expected exactly 1 audit event, got %d", len(events))
		}
		ev := events[0]
		if ev.Operation != OperationCreate || ev.Result != ResultSuccess {
			t.Errorf("expected create/success event, got %s/%s", ev.Operation, ev.Result)
		}
		if ev.Actor != actor || ev.TenantID != view.ID {
			t.Errorf("event must carry actor %s and tenant id %s, got %s / %s", actor, view.ID, ev.Actor, ev.TenantID)
		}
	})

	t.Run("name 前後の空白は trim されて保存される（境界値）", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		h.stub.OnCreateSignupURL = func(_ context.Context) (string, string, error) {
			return testSignupURLSecret, testSignupURLName, nil
		}

		// Act
		view, _, err := h.svc.Create(context.Background(), uuid.New(), CreateInput{Name: "  " + testTenantNameValue + "  "})

		// Assert
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if view.Name != testTenantNameValue {
			t.Errorf("expected trimmed name %q, got %q", testTenantNameValue, view.Name)
		}
	})

	t.Run("CreateSignupURL が非 transient error のとき永続化せずエラーを伝達し Record(create,failure) を行う", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		actor := uuid.New()
		amapiErr := pkgerrors.New(pkgerrors.CodeUpstream, "amapi rejected the request")
		h.stub.OnCreateSignupURL = func(_ context.Context) (string, string, error) {
			return "", "", amapiErr
		}

		// Act
		_, _, err := h.svc.Create(context.Background(), actor, CreateInput{Name: testTenantNameValue})

		// Assert
		if !stderrors.Is(err, amapiErr) {
			t.Fatalf("expected the amapi error to be propagated, got %v", err)
		}
		if h.repo.calls.insert != 0 {
			t.Errorf("Insert must not be called when signup url creation fails, got %d", h.repo.calls.insert)
		}
		events := h.recorder.recorded()
		if len(events) != 1 {
			t.Fatalf("expected exactly 1 audit event, got %d", len(events))
		}
		if events[0].Operation != OperationCreate || events[0].Result != ResultFailure {
			t.Errorf("expected create/failure event, got %s/%s", events[0].Operation, events[0].Result)
		}
	})

	t.Run("Insert が失敗したときエラーを伝達し Record(create,failure) を行う", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		h.stub.OnCreateSignupURL = func(_ context.Context) (string, string, error) {
			return testSignupURLSecret, testSignupURLName, nil
		}
		insertErr := pkgerrors.New(pkgerrors.CodeUnavailable, "db unavailable")
		h.repo.insertErr = insertErr

		// Act
		_, _, err := h.svc.Create(context.Background(), uuid.New(), CreateInput{Name: testTenantNameValue})

		// Assert
		if !stderrors.Is(err, insertErr) {
			t.Fatalf("expected the insert error to be propagated, got %v", err)
		}
		events := h.recorder.recorded()
		if len(events) != 1 || events[0].Result != ResultFailure {
			t.Errorf("expected a create/failure audit event, got %+v", events)
		}
	})

	t.Run("成功時の signup url 秘密値が監査ログへ漏れない（NFR 2.3）", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		h.stub.OnCreateSignupURL = func(_ context.Context) (string, string, error) {
			return testSignupURLSecret, testSignupURLName, nil
		}

		// Act
		_, _, _ = h.svc.Create(context.Background(), uuid.New(), CreateInput{Name: testTenantNameValue})

		// Assert: Event は機密値フィールドを持たないため signup url は記録されない。
		for _, ev := range h.recorder.recorded() {
			if ev.DenyReason == testSignupURLSecret {
				t.Errorf("signup url secret leaked into audit event")
			}
		}
		if h.log.containsSecret(testSignupURLSecret) {
			t.Errorf("signup url secret leaked into structured log")
		}
	})
}

// ===== Get =====

func TestService_Get(t *testing.T) {
	t.Run("存在するテナントのとき TenantView を返す", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		id := uuid.New()
		h.repo.getRow = TenantRow{ID: id, Name: testTenantNameValue, Status: StatusBound, EnterpriseName: testEnterpriseName}

		// Act
		view, err := h.svc.Get(context.Background(), id)

		// Assert
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if view.ID != id || view.Status != StatusBound || view.EnterpriseName != testEnterpriseName {
			t.Errorf("unexpected view: %+v", view)
		}
	})

	t.Run("存在しないテナント id のとき CodeNotFound を返す", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		h.repo.getErr = pkgerrors.Wrap(pkgerrors.CodeNotFound, "tenant not found", nil)

		// Act
		_, err := h.svc.Get(context.Background(), uuid.New())

		// Assert
		if got := codeOf(t, err); got != pkgerrors.CodeNotFound {
			t.Fatalf("expected CodeNotFound, got %s", got)
		}
	})
}

// ===== List =====

func TestService_List(t *testing.T) {
	t.Run("複数テナントのとき TenantView の slice を返す", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		h.repo.listRows = []TenantRow{
			{ID: uuid.New(), Name: "A", Status: StatusPendingBind},
			{ID: uuid.New(), Name: "B", Status: StatusBound, EnterpriseName: testEnterpriseName},
		}

		// Act
		views, err := h.svc.List(context.Background())

		// Assert
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(views) != 2 {
			t.Fatalf("expected 2 views, got %d", len(views))
		}
		if views[1].EnterpriseName != testEnterpriseName {
			t.Errorf("expected enterprise name on bound tenant, got %q", views[1].EnterpriseName)
		}
	})

	t.Run("登録 0 件のとき空 slice（非 nil）を返す（境界値）", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		h.repo.listRows = []TenantRow{}

		// Act
		views, err := h.svc.List(context.Background())

		// Assert
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if views == nil {
			t.Fatalf("expected non-nil empty slice")
		}
		if len(views) != 0 {
			t.Errorf("expected empty slice, got %d", len(views))
		}
	})

	t.Run("Repository が失敗したときエラーを伝達する（異常系）", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		h.repo.listErr = pkgerrors.New(pkgerrors.CodeUnavailable, "db unavailable")

		// Act
		_, err := h.svc.List(context.Background())

		// Assert
		if got := codeOf(t, err); got != pkgerrors.CodeUnavailable {
			t.Fatalf("expected CodeUnavailable, got %s", got)
		}
	})
}

// ===== EnterpriseNameForTenant =====

func TestService_EnterpriseNameForTenant(t *testing.T) {
	t.Run("bound のとき enterprise_name と nil を返す", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		id := uuid.New()
		h.repo.getRow = TenantRow{ID: id, Name: testTenantNameValue, Status: StatusBound, EnterpriseName: testEnterpriseName}

		// Act
		name, err := h.svc.EnterpriseNameForTenant(context.Background(), id)

		// Assert
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if name != testEnterpriseName {
			t.Errorf("expected %q, got %q", testEnterpriseName, name)
		}
	})

	t.Run("pending_bind のとき CodeBusinessRule（未バインド）を返し構造化ログを出す", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		id := uuid.New()
		h.repo.getRow = TenantRow{ID: id, Name: testTenantNameValue, Status: StatusPendingBind}

		// Act
		name, err := h.svc.EnterpriseNameForTenant(context.Background(), id)

		// Assert
		if got := codeOf(t, err); got != pkgerrors.CodeBusinessRule {
			t.Fatalf("expected CodeBusinessRule, got %s", got)
		}
		if name != "" {
			t.Errorf("expected empty enterprise name on rejection, got %q", name)
		}
		if _, ok := h.log.warnWithDenyReason(); !ok {
			t.Errorf("expected a WARN log entry with deny_reason field (NFR 2.2)")
		}
	})

	t.Run("disabled のとき CodeBusinessRule（無効化）を返し構造化ログを出す", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		id := uuid.New()
		h.repo.getRow = TenantRow{ID: id, Name: testTenantNameValue, Status: StatusDisabled}

		// Act
		_, err := h.svc.EnterpriseNameForTenant(context.Background(), id)

		// Assert
		if got := codeOf(t, err); got != pkgerrors.CodeBusinessRule {
			t.Fatalf("expected CodeBusinessRule, got %s", got)
		}
		if !stderrors.Is(err, ErrTenantDisabled) {
			t.Errorf("expected ErrTenantDisabled sentinel, got %v", err)
		}
		if _, ok := h.log.warnWithDenyReason(); !ok {
			t.Errorf("expected a WARN log entry with deny_reason field (NFR 2.2)")
		}
	})

	t.Run("不在のとき CodeNotFound を返す（異常系）", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		h.repo.getErr = pkgerrors.Wrap(pkgerrors.CodeNotFound, "tenant not found", nil)

		// Act
		_, err := h.svc.EnterpriseNameForTenant(context.Background(), uuid.New())

		// Assert
		if got := codeOf(t, err); got != pkgerrors.CodeNotFound {
			t.Fatalf("expected CodeNotFound, got %s", got)
		}
	})

	t.Run("pending_bind 拒否で ErrNotBound sentinel を返す", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		h.repo.getRow = TenantRow{ID: uuid.New(), Status: StatusPendingBind}

		// Act
		_, err := h.svc.EnterpriseNameForTenant(context.Background(), uuid.New())

		// Assert
		if !stderrors.Is(err, ErrNotBound) {
			t.Errorf("expected ErrNotBound sentinel, got %v", err)
		}
	})
}
