package tenant

import (
	"context"
	stderrors "errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap/zapcore"

	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/amapi"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
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
	// fields は zap.Field（単一引数 / logger.ActorID・TenantID 等）と素の key/value ペア
	// （"deny_reason", reason 等）が混在する。両方を map 化して field 検証に使えるようにする
	// （actor_id を含む zap.Field を捨てると NFR 2.2 の実行者記録を検証できないため / #51 round5）。
	for i := 0; i < len(fields); {
		switch fv := fields[i].(type) {
		case zapcore.Field:
			f[fv.Key] = zapFieldValue(fv)
			i++
		case string:
			if i+1 < len(fields) {
				f[fv] = fields[i+1]
			}
			i += 2
		default:
			i++
		}
	}
	l.entries = append(l.entries, fakeLogEntry{Level: level, Msg: msg, Fields: f})
}

// zapFieldValue は zapcore.Field から検証に使う値を取り出す。logger.ActorID / TenantID は
// zap.String（StringType）であり、値は String フィールドに入る。それ以外の型は Interface を返す。
func zapFieldValue(fld zapcore.Field) any {
	if fld.Type == zapcore.StringType {
		return fld.String
	}
	return fld.Interface
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

// warnDenyActor は WARN + deny_reason を持つ最初のエントリの actor_id field 値を返す（NFR 2.2 /
// #51 round5）。拒否ログに実行者が記録されているか（uuid.Nil でないか）を検証するために用いる。
func (l *fakeLogger) warnDenyActor() (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.entries {
		if e.Level != "warn" {
			continue
		}
		if _, ok := e.Fields["deny_reason"]; !ok {
			continue
		}
		v, ok := e.Fields["actor_id"]
		if !ok {
			return "", false
		}
		s, _ := v.(string)
		return s, true
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
	insert         int
	get            int
	list           int
	updateBound    int
	updateDisabled int
}

type fakeRepository struct {
	mu sync.Mutex

	// 設定された挙動
	insertErr error
	getRow    TenantRow
	getErr    error
	listRows  []TenantRow
	listErr   error

	// UpdateBound / UpdateDisabled の制御（affected 行数・error）。
	updateBoundAffected    int64
	updateBoundErr         error
	updateDisabledAffected int64
	updateDisabledErr      error

	// 記録された入力
	calls                fakeRepoCalls
	lastInsertRow        TenantRow
	lastGetID            uuid.UUID
	lastUpdateBoundID    uuid.UUID
	lastUpdateBoundName  string
	lastUpdateDisabledID uuid.UUID
	lastDisabledActor    uuid.UUID
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

// UpdateBound は条件付き UPDATE（WHERE status='pending_bind'）を模擬する。affected 行数 /
// error を設定可能にし、呼出回数・入力（id / enterprise_name）を記録する（task 5 Bind 検証用）。
func (r *fakeRepository) UpdateBound(_ context.Context, id uuid.UUID, enterpriseName string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls.updateBound++
	r.lastUpdateBoundID = id
	r.lastUpdateBoundName = enterpriseName
	if r.updateBoundErr != nil {
		return 0, r.updateBoundErr
	}
	return r.updateBoundAffected, nil
}

// UpdateDisabled は条件付き UPDATE（WHERE status!='disabled'）を模擬する。affected 行数 /
// error を設定可能にし、呼出回数・入力（id / actor）を記録する（task 5 Disable 検証用）。
func (r *fakeRepository) UpdateDisabled(_ context.Context, id uuid.UUID, actor uuid.UUID) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls.updateDisabled++
	r.lastUpdateDisabledID = id
	r.lastDisabledActor = actor
	if r.updateDisabledErr != nil {
		return 0, r.updateDisabledErr
	}
	return r.updateDisabledAffected, nil
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
		// 空 name の拒否も create の失敗監査として記録する（Req 1.5 / NFR 2.1）。
		events := h.recorder.recorded()
		if len(events) != 1 || events[0].Operation != OperationCreate || events[0].Result != ResultFailure {
			t.Errorf("expected a create/failure audit event on empty name, got %+v", events)
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

	t.Run("CreateSignupURL が失敗したとき tenant を pending_bind のまま保持しエラーを伝達し Record(create,failure) を行う（Req 1.4）", func(t *testing.T) {
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
		// Req 1.4: URL 生成失敗時もテナントは pending_bind として既に永続化済みであること。
		if h.repo.calls.insert != 1 {
			t.Errorf("Insert must be called once so the tenant remains pending_bind on signup url failure (Req 1.4), got %d", h.repo.calls.insert)
		}
		if h.repo.lastInsertRow.Status != StatusPendingBind {
			t.Errorf("inserted row status must be pending_bind, got %s", h.repo.lastInsertRow.Status)
		}
		events := h.recorder.recorded()
		if len(events) != 1 {
			t.Fatalf("expected exactly 1 audit event, got %d", len(events))
		}
		if events[0].Operation != OperationCreate || events[0].Result != ResultFailure {
			t.Errorf("expected create/failure event, got %s/%s", events[0].Operation, events[0].Result)
		}
	})

	t.Run("CreateSignupURL が err=nil で空の signup_url を返すとき 502 を返し pending_bind を保つ（Req 1.2 / 1.4）", func(t *testing.T) {
		// Arrange: AMAPI が成功扱い（err=nil）で空の URL を返す異常応答を模擬する。
		h := newServiceHarness()
		h.stub.OnCreateSignupURL = func(_ context.Context) (string, string, error) {
			return "   ", testSignupURLName, nil
		}

		// Act
		_, _, err := h.svc.Create(context.Background(), uuid.New(), CreateInput{Name: testTenantNameValue})

		// Assert
		if got := codeOf(t, err); got != pkgerrors.CodeUpstream {
			t.Fatalf("expected CodeUpstream on empty signup url, got %s", got)
		}
		// テナントは pending_bind として永続化済みであること（Req 1.4）。
		if h.repo.calls.insert != 1 {
			t.Errorf("Insert must be called once, got %d", h.repo.calls.insert)
		}
		events := h.recorder.recorded()
		if len(events) != 1 || events[0].Result != ResultFailure {
			t.Errorf("expected a create/failure audit event, got %+v", events)
		}
	})

	t.Run("CreateSignupURL が err=nil で空の signup_url_name を返すとき 502 を返す（Req 2.1 の bind 入力前提）", func(t *testing.T) {
		// Arrange: URL は返るが後続 bind に必要な signup_url_name が空の異常応答を模擬する。
		h := newServiceHarness()
		h.stub.OnCreateSignupURL = func(_ context.Context) (string, string, error) {
			return testSignupURLSecret, "  ", nil
		}

		// Act
		_, _, err := h.svc.Create(context.Background(), uuid.New(), CreateInput{Name: testTenantNameValue})

		// Assert
		if got := codeOf(t, err); got != pkgerrors.CodeUpstream {
			t.Fatalf("expected CodeUpstream on empty signup_url_name, got %s", got)
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

// superAdminCtx は SuperAdmin TenantContext を確立した ctx を返す。EnterpriseNameForTenant の
// テナント分離ガードは fail-closed（TenantContext 未確立は拒否）であり、status 別の挙動を検証する
// サブテストは admin 内部経路を模す SuperAdmin context 上で実行する。
func superAdminCtx() context.Context {
	return db.WithTenantContext(context.Background(), db.TenantContext{TenantID: uuid.Nil, IsSuperAdmin: true})
}

func TestService_EnterpriseNameForTenant(t *testing.T) {
	t.Run("bound のとき enterprise_name と nil を返す", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		id := uuid.New()
		h.repo.getRow = TenantRow{ID: id, Name: testTenantNameValue, Status: StatusBound, EnterpriseName: testEnterpriseName}

		// Act
		name, err := h.svc.EnterpriseNameForTenant(superAdminCtx(), id)

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
		name, err := h.svc.EnterpriseNameForTenant(superAdminCtx(), id)

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
		_, err := h.svc.EnterpriseNameForTenant(superAdminCtx(), id)

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
		id := uuid.New()

		// Act: SuperAdmin context + 一致 id で越境ガードを通し、Get 不在の写像を検証する。
		_, err := h.svc.EnterpriseNameForTenant(superAdminCtx(), id)

		// Assert
		if got := codeOf(t, err); got != pkgerrors.CodeNotFound {
			t.Fatalf("expected CodeNotFound, got %s", got)
		}
	})

	t.Run("pending_bind 拒否で ErrNotBound sentinel を返す", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		id := uuid.New()
		h.repo.getRow = TenantRow{ID: id, Status: StatusPendingBind}

		// Act
		_, err := h.svc.EnterpriseNameForTenant(superAdminCtx(), id)

		// Assert
		if !stderrors.Is(err, ErrNotBound) {
			t.Errorf("expected ErrNotBound sentinel, got %v", err)
		}
	})

	t.Run("TenantContext 未確立のとき fail-closed で ErrTenantNotFound を返し repo.Get を呼ばない（テナント分離 / #51 round4）", func(t *testing.T) {
		// Arrange: 呼び出し側が TenantContext を確立し損ねた経路を模す（context.Background）。
		h := newServiceHarness()
		id := uuid.New()
		h.repo.getRow = TenantRow{ID: id, Name: testTenantNameValue, Status: StatusBound, EnterpriseName: testEnterpriseName}

		// Act
		name, err := h.svc.EnterpriseNameForTenant(context.Background(), id)

		// Assert: SuperAdmin 昇格に委ねて任意 tenant の enterprise_name を返さず fail-closed で拒否。
		if !stderrors.Is(err, ErrTenantNotFound) {
			t.Fatalf("expected ErrTenantNotFound when TenantContext is missing, got %v", err)
		}
		if name != "" {
			t.Errorf("expected empty enterprise name on fail-closed denial, got %q", name)
		}
		if h.repo.calls.get != 0 {
			t.Errorf("repo.Get must not be called when TenantContext is missing (no existence leak), got %d", h.repo.calls.get)
		}
		if _, ok := h.log.warnWithDenyReason(); !ok {
			t.Errorf("expected a WARN log entry with deny_reason field (NFR 2.2)")
		}
	})

	t.Run("bound だが enterprise_name が空のとき fail-closed で拒否する（データ不整合防御 / #51 round4）", func(t *testing.T) {
		// Arrange: DDL は bound 行の enterprise_name 非空を強制しないため、空の bound 行を模す。
		h := newServiceHarness()
		id := uuid.New()
		h.repo.getRow = TenantRow{ID: id, Name: testTenantNameValue, Status: StatusBound, EnterpriseName: ""}

		// Act
		name, err := h.svc.EnterpriseNameForTenant(superAdminCtx(), id)

		// Assert: 空の識別子を下流へ渡さず CodeBusinessRule（ErrInvalidState）で拒否する。
		if got := codeOf(t, err); got != pkgerrors.CodeBusinessRule {
			t.Fatalf("expected CodeBusinessRule on empty enterprise name, got %s", got)
		}
		if !stderrors.Is(err, ErrInvalidState) {
			t.Errorf("expected ErrInvalidState sentinel, got %v", err)
		}
		if name != "" {
			t.Errorf("expected empty enterprise name on rejection, got %q", name)
		}
		if _, ok := h.log.warnWithDenyReason(); !ok {
			t.Errorf("expected a WARN log entry with deny_reason field (NFR 2.2)")
		}
	})

	t.Run("tenant-scoped 呼び出しが自テナント以外の id を要求したとき ErrTenantNotFound で拒否し repo.Get を呼ばない（テナント分離）", func(t *testing.T) {
		// Arrange: 呼び出し元は tenant-scoped（IsSuperAdmin=false, TenantID=caller）。
		h := newServiceHarness()
		caller := uuid.New()
		other := uuid.New()
		ctx := db.WithTenantContext(context.Background(), db.TenantContext{TenantID: caller, IsSuperAdmin: false})

		// Act: 自テナント（caller）以外の other を要求する。
		_, err := h.svc.EnterpriseNameForTenant(ctx, other)

		// Assert
		if !stderrors.Is(err, ErrTenantNotFound) {
			t.Fatalf("expected ErrTenantNotFound on cross-tenant access, got %v", err)
		}
		if h.repo.calls.get != 0 {
			t.Errorf("repo.Get must not be called on cross-tenant access (no existence leak), got %d", h.repo.calls.get)
		}
		if _, ok := h.log.warnWithDenyReason(); !ok {
			t.Errorf("expected a WARN log entry with deny_reason field (NFR 2.2)")
		}
	})

	t.Run("tenant-scoped 呼び出しが自テナントの id を要求したとき enterprise_name を返す（テナント分離）", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		caller := uuid.New()
		h.repo.getRow = TenantRow{ID: caller, Name: testTenantNameValue, Status: StatusBound, EnterpriseName: testEnterpriseName}
		ctx := db.WithTenantContext(context.Background(), db.TenantContext{TenantID: caller, IsSuperAdmin: false})

		// Act
		name, err := h.svc.EnterpriseNameForTenant(ctx, caller)

		// Assert
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if name != testEnterpriseName {
			t.Errorf("expected %q for own tenant, got %q", testEnterpriseName, name)
		}
	})

	t.Run("前提ガード拒否の構造化ログに実行者（actor_id）を記録する（NFR 2.2 / #51 round5）", func(t *testing.T) {
		// Arrange: tenant-scoped 自テナント呼び出しが pending_bind を要求する経路（status 別の
		// 前提ガード拒否）。TenantContext に実行者 admin の id を確立する。
		h := newServiceHarness()
		caller := uuid.New()
		adminID := uuid.New()
		h.repo.getRow = TenantRow{ID: caller, Name: testTenantNameValue, Status: StatusPendingBind}
		ctx := db.WithTenantContext(context.Background(), db.TenantContext{
			TenantID:     caller,
			AdminUserID:  adminID,
			IsSuperAdmin: false,
		})

		// Act
		_, err := h.svc.EnterpriseNameForTenant(ctx, caller)

		// Assert: 拒否ログの actor_id が確立済み TenantContext の AdminUserID であり、uuid.Nil で
		// 取りこぼされていないこと（実行者・対象テナント・拒否理由を満たす / NFR 2.2）。
		if !stderrors.Is(err, ErrNotBound) {
			t.Fatalf("expected ErrNotBound, got %v", err)
		}
		got, ok := h.log.warnDenyActor()
		if !ok {
			t.Fatalf("expected a WARN deny log entry carrying actor_id (NFR 2.2)")
		}
		if got != adminID.String() {
			t.Errorf("deny log actor_id must be the calling admin %q, got %q", adminID.String(), got)
		}
		if got == uuid.Nil.String() {
			t.Errorf("deny log actor_id must not be uuid.Nil once TenantContext is established")
		}
	})

	t.Run("SuperAdmin context は任意 id を参照できる（admin 経路はテナント分離ガード対象外）", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		target := uuid.New()
		h.repo.getRow = TenantRow{ID: target, Name: testTenantNameValue, Status: StatusBound, EnterpriseName: testEnterpriseName}
		ctx := db.WithTenantContext(context.Background(), db.TenantContext{TenantID: uuid.Nil, IsSuperAdmin: true})

		// Act
		name, err := h.svc.EnterpriseNameForTenant(ctx, target)

		// Assert
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if name != testEnterpriseName {
			t.Errorf("expected %q for SuperAdmin cross-tenant read, got %q", testEnterpriseName, name)
		}
	})
}

// ===== Bind =====

func TestService_Bind(t *testing.T) {
	t.Run("pending_bind から CreateEnterprise 成功 + UpdateBound(affected=1) のとき bound と enterprise_name を返し Record(bind,success) を行う", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		actor := uuid.New()
		id := uuid.New()
		h.repo.getRow = TenantRow{ID: id, Name: testTenantNameValue, Status: StatusPendingBind}
		h.repo.updateBoundAffected = 1
		h.stub.OnCreateEnterprise = func(_ context.Context, signupURLName, projectID string) (string, error) {
			if signupURLName != testSignupURLName {
				t.Errorf("expected signup url name %q, got %q", testSignupURLName, signupURLName)
			}
			if projectID != "test-project" {
				t.Errorf("expected project id from config, got %q", projectID)
			}
			return testEnterpriseName, nil
		}

		// Act
		view, err := h.svc.Bind(context.Background(), actor, id, BindInput{SignupURLName: testSignupURLName})

		// Assert
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if view.Status != StatusBound {
			t.Errorf("expected status bound, got %s", view.Status)
		}
		if view.EnterpriseName != testEnterpriseName {
			t.Errorf("expected enterprise name %q, got %q", testEnterpriseName, view.EnterpriseName)
		}
		if h.repo.calls.updateBound != 1 {
			t.Fatalf("expected UpdateBound to be called once, got %d", h.repo.calls.updateBound)
		}
		if h.repo.lastUpdateBoundName != testEnterpriseName {
			t.Errorf("UpdateBound must receive the created enterprise name, got %q", h.repo.lastUpdateBoundName)
		}
		events := h.recorder.recorded()
		if len(events) != 1 {
			t.Fatalf("expected exactly 1 audit event, got %d", len(events))
		}
		if events[0].Operation != OperationBind || events[0].Result != ResultSuccess {
			t.Errorf("expected bind/success event, got %s/%s", events[0].Operation, events[0].Result)
		}
		if events[0].Actor != actor || events[0].TenantID != id {
			t.Errorf("event must carry actor %s and tenant id %s, got %s / %s", actor, id, events[0].Actor, events[0].TenantID)
		}
	})

	t.Run("CreateEnterprise が失敗したとき bound へ進まず UpdateBound 未呼出でエラー伝達し Record(bind,failure) を行う", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		id := uuid.New()
		h.repo.getRow = TenantRow{ID: id, Name: testTenantNameValue, Status: StatusPendingBind}
		amapiErr := pkgerrors.New(pkgerrors.CodeUpstream, "amapi create enterprise failed")
		h.stub.OnCreateEnterprise = func(_ context.Context, _, _ string) (string, error) {
			return "", amapiErr
		}

		// Act
		_, err := h.svc.Bind(context.Background(), uuid.New(), id, BindInput{SignupURLName: testSignupURLName})

		// Assert
		if !stderrors.Is(err, amapiErr) {
			t.Fatalf("expected the amapi error to be propagated, got %v", err)
		}
		if h.repo.calls.updateBound != 0 {
			t.Errorf("UpdateBound must not be called when CreateEnterprise fails (NFR 1.3), got %d", h.repo.calls.updateBound)
		}
		events := h.recorder.recorded()
		if len(events) != 1 || events[0].Operation != OperationBind || events[0].Result != ResultFailure {
			t.Errorf("expected a bind/failure audit event, got %+v", events)
		}
	})

	t.Run("bound 状態への再 bind のとき 409 を返し CreateEnterprise を呼ばない（新規 Enterprise を作らない）", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		id := uuid.New()
		h.repo.getRow = TenantRow{ID: id, Name: testTenantNameValue, Status: StatusBound, EnterpriseName: testEnterpriseName}

		// Act
		_, err := h.svc.Bind(context.Background(), uuid.New(), id, BindInput{SignupURLName: testSignupURLName})

		// Assert
		if got := codeOf(t, err); got != pkgerrors.CodeConflict {
			t.Fatalf("expected CodeConflict, got %s", got)
		}
		if h.stub.CallCount("CreateEnterprise") != 0 {
			t.Errorf("CreateEnterprise must not be called on re-bind of a bound tenant, got %d", h.stub.CallCount("CreateEnterprise"))
		}
		if h.repo.calls.updateBound != 0 {
			t.Errorf("UpdateBound must not be called on re-bind, got %d", h.repo.calls.updateBound)
		}
		if _, ok := h.log.warnWithDenyReason(); !ok {
			t.Errorf("expected a WARN log entry with deny_reason field (NFR 2.2)")
		}
	})

	t.Run("disabled 状態への bind のとき 422 を返し CreateEnterprise を呼ばない", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		id := uuid.New()
		h.repo.getRow = TenantRow{ID: id, Name: testTenantNameValue, Status: StatusDisabled}

		// Act
		_, err := h.svc.Bind(context.Background(), uuid.New(), id, BindInput{SignupURLName: testSignupURLName})

		// Assert
		if got := codeOf(t, err); got != pkgerrors.CodeBusinessRule {
			t.Fatalf("expected CodeBusinessRule, got %s", got)
		}
		if h.stub.CallCount("CreateEnterprise") != 0 {
			t.Errorf("CreateEnterprise must not be called on bind of a disabled tenant, got %d", h.stub.CallCount("CreateEnterprise"))
		}
		if _, ok := h.log.warnWithDenyReason(); !ok {
			t.Errorf("expected a WARN log entry with deny_reason field (NFR 2.2)")
		}
	})

	t.Run("UpdateBound が affected=0 を返すとき競合として 409 を返す（楽観的競合制御）", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		id := uuid.New()
		h.repo.getRow = TenantRow{ID: id, Name: testTenantNameValue, Status: StatusPendingBind}
		h.repo.updateBoundAffected = 0
		h.stub.OnCreateEnterprise = func(_ context.Context, _, _ string) (string, error) {
			return testEnterpriseName, nil
		}

		// Act
		_, err := h.svc.Bind(context.Background(), uuid.New(), id, BindInput{SignupURLName: testSignupURLName})

		// Assert
		if got := codeOf(t, err); got != pkgerrors.CodeConflict {
			t.Fatalf("expected CodeConflict on affected=0, got %s", got)
		}
		if h.repo.calls.updateBound != 1 {
			t.Errorf("expected UpdateBound to be called once, got %d", h.repo.calls.updateBound)
		}
		events := h.recorder.recorded()
		if len(events) != 1 || events[0].Result != ResultFailure {
			t.Errorf("expected a bind/failure audit event on conflict, got %+v", events)
		}
	})

	t.Run("UpdateBound が CodeConflict（23505 写像）を返すときその error を伝達する", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		id := uuid.New()
		h.repo.getRow = TenantRow{ID: id, Name: testTenantNameValue, Status: StatusPendingBind}
		conflictErr := pkgerrors.New(pkgerrors.CodeConflict, "enterprise name already bound to another tenant")
		h.repo.updateBoundErr = conflictErr
		h.stub.OnCreateEnterprise = func(_ context.Context, _, _ string) (string, error) {
			return testEnterpriseName, nil
		}

		// Act
		_, err := h.svc.Bind(context.Background(), uuid.New(), id, BindInput{SignupURLName: testSignupURLName})

		// Assert
		if !stderrors.Is(err, conflictErr) {
			t.Fatalf("expected the repository conflict error to be propagated, got %v", err)
		}
	})

	t.Run("不在 id のとき CodeNotFound を返し CreateEnterprise を呼ばない（異常系）", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		h.repo.getErr = pkgerrors.Wrap(pkgerrors.CodeNotFound, "tenant not found", nil)

		// Act
		_, err := h.svc.Bind(context.Background(), uuid.New(), uuid.New(), BindInput{SignupURLName: testSignupURLName})

		// Assert
		if got := codeOf(t, err); got != pkgerrors.CodeNotFound {
			t.Fatalf("expected CodeNotFound, got %s", got)
		}
		if h.stub.CallCount("CreateEnterprise") != 0 {
			t.Errorf("CreateEnterprise must not be called when tenant is absent, got %d", h.stub.CallCount("CreateEnterprise"))
		}
		// 取得失敗（不在）も bind の失敗監査として記録する（Req 2.7 / NFR 2.1）。
		events := h.recorder.recorded()
		if len(events) != 1 || events[0].Operation != OperationBind || events[0].Result != ResultFailure {
			t.Errorf("expected a bind/failure audit event on lookup failure, got %+v", events)
		}
	})

	t.Run("定義外 status のとき fail-closed で 422 を返す（NFR 1.2）", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		id := uuid.New()
		h.repo.getRow = TenantRow{ID: id, Name: testTenantNameValue, Status: Status("weird")}

		// Act
		_, err := h.svc.Bind(context.Background(), uuid.New(), id, BindInput{SignupURLName: testSignupURLName})

		// Assert
		if got := codeOf(t, err); got != pkgerrors.CodeBusinessRule {
			t.Fatalf("expected CodeBusinessRule on undefined state, got %s", got)
		}
		if h.stub.CallCount("CreateEnterprise") != 0 {
			t.Errorf("CreateEnterprise must not be called on undefined state, got %d", h.stub.CallCount("CreateEnterprise"))
		}
	})

	t.Run("成功時の enterprise_name / signup url name が監査ログへ漏れない（NFR 2.3）", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		id := uuid.New()
		h.repo.getRow = TenantRow{ID: id, Name: testTenantNameValue, Status: StatusPendingBind}
		h.repo.updateBoundAffected = 1
		h.stub.OnCreateEnterprise = func(_ context.Context, _, _ string) (string, error) {
			return testEnterpriseName, nil
		}

		// Act
		_, _ = h.svc.Bind(context.Background(), uuid.New(), id, BindInput{SignupURLName: testSignupURLName})

		// Assert: Event は機密値フィールドを持たないため enterprise_name / signup url name は記録されない。
		for _, ev := range h.recorder.recorded() {
			if ev.DenyReason == testEnterpriseName || ev.DenyReason == testSignupURLName {
				t.Errorf("sensitive value leaked into audit event deny_reason")
			}
		}
		if h.log.containsSecret(testEnterpriseName) || h.log.containsSecret(testSignupURLName) {
			t.Errorf("sensitive value leaked into structured log")
		}
	})

	t.Run("signup_url_name が空白 trim 後に空のとき 400 を返し Get・CreateEnterprise を呼ばない（入力検証）", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		id := uuid.New()
		h.repo.getRow = TenantRow{ID: id, Name: testTenantNameValue, Status: StatusPendingBind}

		// Act
		_, err := h.svc.Bind(context.Background(), uuid.New(), id, BindInput{SignupURLName: "   "})

		// Assert
		if got := codeOf(t, err); got != pkgerrors.CodeInvalidRequest {
			t.Fatalf("expected CodeInvalidRequest on empty signup_url_name, got %s", got)
		}
		if h.repo.calls.get != 0 {
			t.Errorf("Get must not be called when input is invalid, got %d", h.repo.calls.get)
		}
		if h.stub.CallCount("CreateEnterprise") != 0 {
			t.Errorf("CreateEnterprise must not be called when input is invalid, got %d", h.stub.CallCount("CreateEnterprise"))
		}
		if _, ok := h.log.warnWithDenyReason(); !ok {
			t.Errorf("expected a WARN log entry with deny_reason field (NFR 2.2)")
		}
	})

	t.Run("CreateEnterprise が空の enterprise_name を返すとき 502 を返し UpdateBound を呼ばず pending_bind を保つ（NFR 1.3）", func(t *testing.T) {
		// Arrange: AMAPI が成功扱い（err=nil）で空の name を返す異常応答を模擬する。
		h := newServiceHarness()
		id := uuid.New()
		h.repo.getRow = TenantRow{ID: id, Name: testTenantNameValue, Status: StatusPendingBind}
		h.stub.OnCreateEnterprise = func(_ context.Context, _, _ string) (string, error) {
			return "  ", nil
		}

		// Act
		_, err := h.svc.Bind(context.Background(), uuid.New(), id, BindInput{SignupURLName: testSignupURLName})

		// Assert
		if got := codeOf(t, err); got != pkgerrors.CodeUpstream {
			t.Fatalf("expected CodeUpstream on empty enterprise name, got %s", got)
		}
		if h.repo.calls.updateBound != 0 {
			t.Errorf("UpdateBound must not be called when enterprise name is empty, got %d", h.repo.calls.updateBound)
		}
		events := h.recorder.recorded()
		if len(events) != 1 || events[0].Result != ResultFailure {
			t.Errorf("expected a bind/failure audit event, got %+v", events)
		}
	})
}

// ===== Disable =====

func TestService_Disable(t *testing.T) {
	t.Run("確認テキストが name と一致 + UpdateDisabled(affected=1) のとき disabled へ遷移し Record(disable,success,confirmed=true) を行う", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		actor := uuid.New()
		id := uuid.New()
		h.repo.getRow = TenantRow{ID: id, Name: testTenantNameValue, Status: StatusBound, EnterpriseName: testEnterpriseName}
		h.repo.updateDisabledAffected = 1

		// Act
		view, err := h.svc.Disable(context.Background(), actor, id, DisableInput{Confirmation: testTenantNameValue})

		// Assert
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if view.Status != StatusDisabled {
			t.Errorf("expected status disabled, got %s", view.Status)
		}
		if view.EnterpriseName != "" {
			t.Errorf("disabled view must not expose enterprise_name, got %q", view.EnterpriseName)
		}
		if h.repo.calls.updateDisabled != 1 {
			t.Fatalf("expected UpdateDisabled to be called once, got %d", h.repo.calls.updateDisabled)
		}
		if h.repo.lastDisabledActor != actor {
			t.Errorf("UpdateDisabled must receive the actor %s, got %s", actor, h.repo.lastDisabledActor)
		}
		events := h.recorder.recorded()
		if len(events) != 1 {
			t.Fatalf("expected exactly 1 audit event, got %d", len(events))
		}
		if events[0].Operation != OperationDisable || events[0].Result != ResultSuccess {
			t.Errorf("expected disable/success event, got %s/%s", events[0].Operation, events[0].Result)
		}
		if !events[0].ConfirmationCompleted {
			t.Errorf("expected ConfirmationCompleted=true on a confirmed disable")
		}
	})

	t.Run("確認テキストが name と不一致のとき 422 を返し UpdateDisabled 未呼出で拒否ログを出す（Req 3.2）", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		id := uuid.New()
		h.repo.getRow = TenantRow{ID: id, Name: testTenantNameValue, Status: StatusBound, EnterpriseName: testEnterpriseName}

		// Act
		_, err := h.svc.Disable(context.Background(), uuid.New(), id, DisableInput{Confirmation: "Wrong Name"})

		// Assert
		if got := codeOf(t, err); got != pkgerrors.CodeBusinessRule {
			t.Fatalf("expected CodeBusinessRule, got %s", got)
		}
		if !stderrors.Is(err, ErrConfirmationRequired) {
			t.Errorf("expected ErrConfirmationRequired sentinel, got %v", err)
		}
		if h.repo.calls.updateDisabled != 0 {
			t.Errorf("UpdateDisabled must not be called when confirmation mismatches, got %d", h.repo.calls.updateDisabled)
		}
		if _, ok := h.log.warnWithDenyReason(); !ok {
			t.Errorf("expected a WARN log entry with deny_reason field (NFR 2.2)")
		}
	})

	t.Run("確認テキストが空文字のとき 422 を返す（境界値 / 空入力）", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		id := uuid.New()
		h.repo.getRow = TenantRow{ID: id, Name: testTenantNameValue, Status: StatusBound}

		// Act
		_, err := h.svc.Disable(context.Background(), uuid.New(), id, DisableInput{Confirmation: ""})

		// Assert
		if got := codeOf(t, err); got != pkgerrors.CodeBusinessRule {
			t.Fatalf("expected CodeBusinessRule on empty confirmation, got %s", got)
		}
		if h.repo.calls.updateDisabled != 0 {
			t.Errorf("UpdateDisabled must not be called on empty confirmation, got %d", h.repo.calls.updateDisabled)
		}
	})

	t.Run("既に disabled のテナントのとき 409 を返し UpdateDisabled 未呼出で拒否ログを出す（二重無効化 / Req 3.4）", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		id := uuid.New()
		h.repo.getRow = TenantRow{ID: id, Name: testTenantNameValue, Status: StatusDisabled}

		// Act
		_, err := h.svc.Disable(context.Background(), uuid.New(), id, DisableInput{Confirmation: testTenantNameValue})

		// Assert
		if got := codeOf(t, err); got != pkgerrors.CodeConflict {
			t.Fatalf("expected CodeConflict, got %s", got)
		}
		if h.repo.calls.updateDisabled != 0 {
			t.Errorf("UpdateDisabled must not be called when tenant is already disabled, got %d", h.repo.calls.updateDisabled)
		}
		if _, ok := h.log.warnWithDenyReason(); !ok {
			t.Errorf("expected a WARN log entry with deny_reason field (NFR 2.2)")
		}
	})

	t.Run("UpdateDisabled が affected=0 を返すとき二重無効化競合として 409 を返す（Req 3.4）", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		id := uuid.New()
		h.repo.getRow = TenantRow{ID: id, Name: testTenantNameValue, Status: StatusBound}
		h.repo.updateDisabledAffected = 0

		// Act
		_, err := h.svc.Disable(context.Background(), uuid.New(), id, DisableInput{Confirmation: testTenantNameValue})

		// Assert
		if got := codeOf(t, err); got != pkgerrors.CodeConflict {
			t.Fatalf("expected CodeConflict on affected=0, got %s", got)
		}
		if h.repo.calls.updateDisabled != 1 {
			t.Errorf("expected UpdateDisabled to be called once, got %d", h.repo.calls.updateDisabled)
		}
		events := h.recorder.recorded()
		if len(events) != 1 || events[0].Result != ResultFailure {
			t.Errorf("expected a disable/failure audit event on conflict, got %+v", events)
		}
	})

	t.Run("不在 id のとき CodeNotFound を返し UpdateDisabled 未呼出（異常系）", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		h.repo.getErr = pkgerrors.Wrap(pkgerrors.CodeNotFound, "tenant not found", nil)

		// Act
		_, err := h.svc.Disable(context.Background(), uuid.New(), uuid.New(), DisableInput{Confirmation: testTenantNameValue})

		// Assert
		if got := codeOf(t, err); got != pkgerrors.CodeNotFound {
			t.Fatalf("expected CodeNotFound, got %s", got)
		}
		if h.repo.calls.updateDisabled != 0 {
			t.Errorf("UpdateDisabled must not be called when tenant is absent, got %d", h.repo.calls.updateDisabled)
		}
		// 取得失敗（不在）も disable の失敗監査として記録する（Req 3.5 / NFR 2.1）。
		events := h.recorder.recorded()
		if len(events) != 1 || events[0].Operation != OperationDisable || events[0].Result != ResultFailure {
			t.Errorf("expected a disable/failure audit event on lookup failure, got %+v", events)
		}
	})

	t.Run("pending_bind テナントの確認一致のとき disabled へ遷移できる（PendingBind→Disabled 遷移）", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		id := uuid.New()
		h.repo.getRow = TenantRow{ID: id, Name: testTenantNameValue, Status: StatusPendingBind}
		h.repo.updateDisabledAffected = 1

		// Act
		view, err := h.svc.Disable(context.Background(), uuid.New(), id, DisableInput{Confirmation: testTenantNameValue})

		// Assert
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if view.Status != StatusDisabled {
			t.Errorf("expected status disabled, got %s", view.Status)
		}
	})

	t.Run("定義外 status のとき fail-closed で 422 を返し UpdateDisabled 未呼出（NFR 1.2 / #51 round4）", func(t *testing.T) {
		// Arrange: 確認テキストは一致させ、未定義 status のみが拒否要因であることを切り出す。
		h := newServiceHarness()
		id := uuid.New()
		h.repo.getRow = TenantRow{ID: id, Name: testTenantNameValue, Status: Status("weird")}
		h.repo.updateDisabledAffected = 1

		// Act
		_, err := h.svc.Disable(context.Background(), uuid.New(), id, DisableInput{Confirmation: testTenantNameValue})

		// Assert: disabled へ遷移させず CodeBusinessRule（ErrInvalidState）で拒否する。
		if got := codeOf(t, err); got != pkgerrors.CodeBusinessRule {
			t.Fatalf("expected CodeBusinessRule on undefined state, got %s", got)
		}
		if !stderrors.Is(err, ErrInvalidState) {
			t.Errorf("expected ErrInvalidState sentinel, got %v", err)
		}
		if h.repo.calls.updateDisabled != 0 {
			t.Errorf("UpdateDisabled must not be called on undefined state, got %d", h.repo.calls.updateDisabled)
		}
		if _, ok := h.log.warnWithDenyReason(); !ok {
			t.Errorf("expected a WARN log entry with deny_reason field (NFR 2.2)")
		}
	})
}
