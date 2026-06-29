package policy

import (
	"context"
	stderrors "errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap/zapcore"

	"github.com/hitoshiichikawa/ae-mdm/internal/audit"
	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/amapi"
)

// ---- 機密値漏洩 assertion 用の固定文字列（Req 5.4 / NFR 3.2 観点） ----
//
// secret value は raw body の機密パラメータを模す。監査 Detail / logDeny / ValidationError の
// Message にこの値が surface しないことを検証する。
const (
	testEnterpriseName = "enterprises/LC0123456789"
	testPolicyName     = "Production Policy"
	testSecretValue    = "DO-NOT-LEAK-SECRET-PASSWORD-12345"
)

// ---- Fake upsertClient（AMAPI 反映 / 呼び出し有無と引数を検証） ----

type fakeUpsertClient struct {
	mu    sync.Mutex
	calls []upsertCall
	err   error
}

type upsertCall struct {
	EnterpriseName string
	PolicyName     string
	Body           amapi.PolicyBody
}

func (c *fakeUpsertClient) UpsertPolicy(_ context.Context, enterpriseName, policyName string, body amapi.PolicyBody) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, upsertCall{EnterpriseName: enterpriseName, PolicyName: policyName, Body: body})
	return c.err
}

func (c *fakeUpsertClient) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

func (c *fakeUpsertClient) lastCall() (upsertCall, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.calls) == 0 {
		return upsertCall{}, false
	}
	return c.calls[len(c.calls)-1], true
}

// ---- Fake Repository（snapshot 永続化 / 既存行確認 / 呼び出し有無を検証） ----

type fakeRepository struct {
	mu sync.Mutex

	inserted []PolicyRow
	updated  []PolicyRow

	insertErr error
	updateErr error
	updateAff int64

	getRow PolicyRow
	getErr error

	// List 用（task 4.1）
	listRows []PolicyRow
	listErr  error

	// Delete 用（task 4.1）
	deleteCalls []deleteCall
	deleteAff   int64
	deleteErr   error

	// Assign 用（task 4.1）
	assignCalls []assignCall
	assignAff   int64
	assignErr   error
}

type deleteCall struct {
	TenantID uuid.UUID
	PolicyID uuid.UUID
}

type assignCall struct {
	TenantID uuid.UUID
	DeviceID uuid.UUID
	PolicyID uuid.UUID
}

func (r *fakeRepository) Insert(_ context.Context, row PolicyRow) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.inserted = append(r.inserted, row)
	return r.insertErr
}

func (r *fakeRepository) Update(_ context.Context, row PolicyRow) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updated = append(r.updated, row)
	if r.updateErr != nil {
		return 0, r.updateErr
	}
	return r.updateAff, nil
}

func (r *fakeRepository) Get(_ context.Context, _ uuid.UUID, _ uuid.UUID) (PolicyRow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.getErr != nil {
		return PolicyRow{}, r.getErr
	}
	return r.getRow, nil
}

func (r *fakeRepository) List(_ context.Context, _ uuid.UUID) ([]PolicyRow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.listErr != nil {
		return nil, r.listErr
	}
	// Repository 実装は 0 件でも非 nil 空 slice を返す契約のため、それに倣う。
	out := make([]PolicyRow, len(r.listRows))
	copy(out, r.listRows)
	return out, nil
}

func (r *fakeRepository) Delete(_ context.Context, tenantID uuid.UUID, policyID uuid.UUID) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.deleteCalls = append(r.deleteCalls, deleteCall{TenantID: tenantID, PolicyID: policyID})
	if r.deleteErr != nil {
		return 0, r.deleteErr
	}
	return r.deleteAff, nil
}

func (r *fakeRepository) AssignPolicyToDevice(_ context.Context, tenantID, deviceID, policyID uuid.UUID) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.assignCalls = append(r.assignCalls, assignCall{TenantID: tenantID, DeviceID: deviceID, PolicyID: policyID})
	if r.assignErr != nil {
		return 0, r.assignErr
	}
	return r.assignAff, nil
}

func (r *fakeRepository) insertCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.inserted)
}

func (r *fakeRepository) updateCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.updated)
}

func (r *fakeRepository) deleteCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.deleteCalls)
}

func (r *fakeRepository) lastAssignCall() (assignCall, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.assignCalls) == 0 {
		return assignCall{}, false
	}
	return r.assignCalls[len(r.assignCalls)-1], true
}

// ---- Fake eventRecorder（監査記録の有無 / 内容を検証 / Req 5.x） ----

type fakeRecorder struct {
	mu     sync.Mutex
	events []audit.Event
	err    error
}

func (r *fakeRecorder) Record(_ context.Context, ev audit.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return r.err
}

func (r *fakeRecorder) recorded() []audit.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]audit.Event, len(r.events))
	copy(out, r.events)
	return out
}

// ---- Fake enterpriseResolver（enterprise_name 解決 / Req 1.1） ----

type fakeResolver struct {
	name string
	err  error
}

func (r *fakeResolver) EnterpriseNameForTenant(_ context.Context, _ uuid.UUID) (string, error) {
	if r.err != nil {
		return "", r.err
	}
	return r.name, nil
}

// ---- Fake Logger（logDeny の field 検証 / 機密値漏洩検証 / NFR 3.1 / 3.2） ----

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

func (l *fakeLogger) Debug(msg string, fields ...any) { l.record("debug", msg, fields...) }
func (l *fakeLogger) Info(msg string, fields ...any)  { l.record("info", msg, fields...) }
func (l *fakeLogger) Warn(msg string, fields ...any)  { l.record("warn", msg, fields...) }
func (l *fakeLogger) Error(msg string, fields ...any) { l.record("error", msg, fields...) }
func (l *fakeLogger) With(_ ...any) logger.Logger     { return l }
func (l *fakeLogger) Sync() error                     { return nil }

// zapFieldValue は zapcore.Field から値を取り出す（string field を主に扱う / logDeny 検証用）。
func zapFieldValue(f zapcore.Field) any {
	if f.String != "" {
		return f.String
	}
	if f.Integer != 0 {
		return f.Integer
	}
	if f.Interface != nil {
		return f.Interface
	}
	return nil
}

func (l *fakeLogger) snapshot() []fakeLogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]fakeLogEntry, len(l.entries))
	copy(out, l.entries)
	return out
}

// ---- Test harness ----

type serviceHarness struct {
	svc      Service
	repo     *fakeRepository
	amapi    *fakeUpsertClient
	recorder *fakeRecorder
	resolver *fakeResolver
	log      *fakeLogger
}

func newServiceHarness() *serviceHarness {
	repo := &fakeRepository{}
	client := &fakeUpsertClient{}
	recorder := &fakeRecorder{}
	resolver := &fakeResolver{name: testEnterpriseName}
	log := &fakeLogger{}
	svc := NewService(repo, client, recorder, resolver, log)
	return &serviceHarness{
		svc:      svc,
		repo:     repo,
		amapi:    client,
		recorder: recorder,
		resolver: resolver,
		log:      log,
	}
}

// codeOf は err から *errors.Error の Code を抽出する（errors.As 経由）。
func codeOf(t *testing.T, err error) pkgerrors.Code {
	t.Helper()
	var de *pkgerrors.Error
	if !stderrors.As(err, &de) {
		t.Fatalf("expected *errors.Error in chain, got %T (%v)", err, err)
	}
	return de.Code
}

// validBody は検証を通過する最小限の正常 raw body を返す（app 1 件 + 有効 enum 値）。
func validBody() map[string]any {
	return map[string]any{
		keyApplications: []any{
			map[string]any{keyPackageName: "com.example.app", keyInstallType: "FORCE_INSTALLED"},
		},
		keyEncryptionPolicy: "ENABLED_WITH_PASSWORD",
	}
}

// ===== Create: 検証失敗時に AMAPI / Repository を呼ばない（Req 2.5） =====

func TestService_Create(t *testing.T) {
	t.Run("検証失敗のとき AMAPI も Repository も呼ばずに早期 return する", func(t *testing.T) {
		// Arrange: encryptionPolicy が許容値外（invalid field）の body。
		h := newServiceHarness()
		actor, tenantID := uuid.New(), uuid.New()
		body := map[string]any{keyEncryptionPolicy: "INVALID_ENUM_VALUE"}

		// Act
		_, err := h.svc.Create(context.Background(), actor, tenantID, PolicyRequest{Name: testPolicyName, Body: body})

		// Assert: AMAPI / Repository が一切呼ばれない（Req 2.5）。
		if err == nil {
			t.Fatal("expected validation error, got nil")
		}
		if h.amapi.callCount() != 0 {
			t.Errorf("expected AMAPI not called, got %d calls", h.amapi.callCount())
		}
		if h.repo.insertCount() != 0 {
			t.Errorf("expected Repository.Insert not called, got %d calls", h.repo.insertCount())
		}
	})

	t.Run("検証失敗のとき失敗監査を ResultFailure で記録する", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		actor, tenantID := uuid.New(), uuid.New()
		body := map[string]any{keyEncryptionPolicy: "INVALID_ENUM_VALUE"}

		// Act
		_, _ = h.svc.Create(context.Background(), actor, tenantID, PolicyRequest{Name: testPolicyName, Body: body})

		// Assert: 失敗経路でも監査 Record が呼ばれる（Req 5.1）。
		evs := h.recorder.recorded()
		if len(evs) != 1 {
			t.Fatalf("expected 1 audit event, got %d", len(evs))
		}
		if evs[0].Result != audit.ResultFailure {
			t.Errorf("expected ResultFailure, got %s", evs[0].Result)
		}
		if evs[0].EventType != eventTypePolicyCreate {
			t.Errorf("expected event type policy_create, got %s", evs[0].EventType)
		}
	})

	t.Run("AMAPI が再試行不可エラーを返すとき snapshot を永続化せずエラーを伝達する", func(t *testing.T) {
		// Arrange: AMAPI が 4xx 相当（非 transient）を返す。
		h := newServiceHarness()
		actor, tenantID := uuid.New(), uuid.New()
		amapiErr := pkgerrors.New(pkgerrors.CodeUpstream, "amapi rejected policy")
		h.amapi.err = amapiErr

		// Act
		_, err := h.svc.Create(context.Background(), actor, tenantID, PolicyRequest{Name: testPolicyName, Body: validBody()})

		// Assert: AMAPI は呼ばれるが snapshot は永続化されない（Req 1.4）。
		if h.amapi.callCount() != 1 {
			t.Fatalf("expected AMAPI called once, got %d", h.amapi.callCount())
		}
		if h.repo.insertCount() != 0 {
			t.Errorf("expected Repository.Insert NOT called on AMAPI failure, got %d", h.repo.insertCount())
		}
		if !stderrors.Is(err, amapiErr) {
			t.Errorf("expected AMAPI error to be propagated, got %v", err)
		}
	})

	t.Run("成功時に AMAPI 反映後 snapshot 永続化 + 成功監査を記録する", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		actor, tenantID := uuid.New(), uuid.New()
		body := validBody()

		// Act
		view, err := h.svc.Create(context.Background(), actor, tenantID, PolicyRequest{Name: testPolicyName, Body: body})

		// Assert: AMAPI → snapshot → 監査の順で成功する（Req 1.3 / 5.1）。
		if err != nil {
			t.Fatalf("expected success, got %v", err)
		}
		if h.amapi.callCount() != 1 {
			t.Fatalf("expected AMAPI called once, got %d", h.amapi.callCount())
		}
		if h.repo.insertCount() != 1 {
			t.Fatalf("expected Repository.Insert called once, got %d", h.repo.insertCount())
		}
		evs := h.recorder.recorded()
		if len(evs) != 1 || evs[0].Result != audit.ResultSuccess {
			t.Fatalf("expected 1 success audit event, got %+v", evs)
		}
		if view.Name != testPolicyName {
			t.Errorf("expected view name %q, got %q", testPolicyName, view.Name)
		}
	})

	t.Run("成功時に UpsertPolicy へ短い policyId を渡し enterpriseName を二重連結しない", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		actor, tenantID := uuid.New(), uuid.New()

		// Act
		_, err := h.svc.Create(context.Background(), actor, tenantID, PolicyRequest{Name: testPolicyName, Body: validBody()})

		// Assert: policyName 引数は短い policyId（enterpriseName を含まない）。
		if err != nil {
			t.Fatalf("expected success, got %v", err)
		}
		call, ok := h.amapi.lastCall()
		if !ok {
			t.Fatal("expected an AMAPI call")
		}
		if call.EnterpriseName != testEnterpriseName {
			t.Errorf("expected enterprise name %q, got %q", testEnterpriseName, call.EnterpriseName)
		}
		if strings.Contains(call.PolicyName, "/policies/") || strings.HasPrefix(call.PolicyName, "enterprises/") {
			t.Errorf("policyName must be a short policyId, got %q", call.PolicyName)
		}
		// PolicyBody.Name には full path が入る。
		if !strings.HasPrefix(call.Body.Name, testEnterpriseName+"/policies/") {
			t.Errorf("expected PolicyBody.Name to be full path, got %q", call.Body.Name)
		}
	})

	t.Run("enterprise 解決が失敗するとき AMAPI も Repository も呼ばずエラーを伝達する", func(t *testing.T) {
		// Arrange: tenant が未 bind 等で error を返す。
		h := newServiceHarness()
		actor, tenantID := uuid.New(), uuid.New()
		resolveErr := pkgerrors.New(pkgerrors.CodeBusinessRule, "tenant is not bound to an enterprise")
		h.resolver.err = resolveErr

		// Act
		_, err := h.svc.Create(context.Background(), actor, tenantID, PolicyRequest{Name: testPolicyName, Body: validBody()})

		// Assert
		if !stderrors.Is(err, resolveErr) {
			t.Errorf("expected resolve error propagated, got %v", err)
		}
		if h.amapi.callCount() != 0 || h.repo.insertCount() != 0 {
			t.Errorf("expected no AMAPI / Repository calls, got amapi=%d insert=%d", h.amapi.callCount(), h.repo.insertCount())
		}
	})
}

// ===== Create: 上限超過 / 複数不正 / top-level Code（Req 2.2 / 2.4） =====

func TestService_Create_Validation(t *testing.T) {
	t.Run("アプリ 3000 件超のとき 422 で BusinessRule 全件提示する", func(t *testing.T) {
		// Arrange: 3001 件のアプリ（上限超過 = KindBusinessRule のみ）。
		h := newServiceHarness()
		actor, tenantID := uuid.New(), uuid.New()
		apps := make([]any, MaxAppCount+1)
		for i := range apps {
			apps[i] = map[string]any{keyPackageName: "com.example.app", keyInstallType: "AVAILABLE"}
		}
		body := map[string]any{keyApplications: apps}

		// Act
		_, err := h.svc.Create(context.Background(), actor, tenantID, PolicyRequest{Name: testPolicyName, Body: body})

		// Assert: top-level Code は 422（全件 BusinessRule）。
		if got := codeOf(t, err); got != pkgerrors.CodeBusinessRule {
			t.Fatalf("expected CodeBusinessRule (422), got %s", got)
		}
		var vferr *ValidationFailedError
		if !stderrors.As(err, &vferr) {
			t.Fatalf("expected *ValidationFailedError, got %T", err)
		}
		if len(vferr.Errors) == 0 {
			t.Fatal("expected at least 1 validation error")
		}
		// 上限超過は BusinessRule（Req 2.2）。
		foundBusinessRule := false
		for _, e := range vferr.Errors {
			if e.Kind == KindBusinessRule {
				foundBusinessRule = true
			}
		}
		if !foundBusinessRule {
			t.Error("expected a KindBusinessRule error for app count overflow")
		}
		// AMAPI / Repository は呼ばれない（Req 2.5）。
		if h.amapi.callCount() != 0 || h.repo.insertCount() != 0 {
			t.Errorf("expected no AMAPI / Repository calls, got amapi=%d insert=%d", h.amapi.callCount(), h.repo.insertCount())
		}
	})

	t.Run("複数領域の不正が混在するとき全件提示し invalid_field 混在で top-level Code は 400", func(t *testing.T) {
		// Arrange: 上限超過(BusinessRule) + encryptionPolicy 不正(invalid_field) + 型不整合(invalid_field) を混在。
		h := newServiceHarness()
		actor, tenantID := uuid.New(), uuid.New()
		apps := make([]any, MaxAppCount+1)
		for i := range apps {
			apps[i] = map[string]any{keyPackageName: "com.example.app", keyInstallType: "AVAILABLE"}
		}
		body := map[string]any{
			keyApplications:     apps,            // 上限超過 → BusinessRule
			keyEncryptionPolicy: "INVALID_ENUM",  // enum 外 → invalid_field
			keySystemUpdate:     "not-an-object", // 型不整合 → mapper invalid_field
		}

		// Act
		_, err := h.svc.Create(context.Background(), actor, tenantID, PolicyRequest{Name: testPolicyName, Body: body})

		// Assert: invalid_field 混在 → top-level 400（Req 2.4 + top-level Code 規則）。
		if got := codeOf(t, err); got != pkgerrors.CodeInvalidRequest {
			t.Fatalf("expected CodeInvalidRequest (400) when invalid_field is mixed in, got %s", got)
		}
		var vferr *ValidationFailedError
		if !stderrors.As(err, &vferr) {
			t.Fatalf("expected *ValidationFailedError, got %T", err)
		}
		// 全件提示: BusinessRule と InvalidField の双方が含まれる（mapper 変換不能 + Validator 不正の結合）。
		var hasBusinessRule, hasInvalidField bool
		for _, e := range vferr.Errors {
			switch e.Kind {
			case KindBusinessRule:
				hasBusinessRule = true
			case KindInvalidField:
				hasInvalidField = true
			}
		}
		if !hasBusinessRule || !hasInvalidField {
			t.Errorf("expected both BusinessRule and InvalidField errors (all-errors presentation), got %+v", vferr.Errors)
		}
	})
}

// ===== 機密値漏洩検証（Req 5.4 / NFR 3.2） =====

func TestService_NoSecretLeak(t *testing.T) {
	t.Run("検証失敗時の監査 Detail / logDeny に raw body の機密値を載せない", func(t *testing.T) {
		// Arrange: 機密値を含む不正 body（encryptionPolicy が型不整合 → invalid field）。
		h := newServiceHarness()
		actor, tenantID := uuid.New(), uuid.New()
		body := map[string]any{
			keyEncryptionPolicy: testSecretValue, // 文字列だが許容 enum 外 → 検証失敗
			keyPasswordPolicies: []any{
				map[string]any{keyPwdMinimumLength: testSecretValue}, // 型不整合 → invalid field
			},
		}

		// Act
		_, _ = h.svc.Create(context.Background(), actor, tenantID, PolicyRequest{Name: testPolicyName, Body: body})

		// Assert: 監査 Detail に機密値が含まれない（Req 5.4）。
		for _, ev := range h.recorder.recorded() {
			assertNoSecretInDetail(t, ev.Detail)
		}
		// logDeny の field に機密値が含まれない（NFR 3.2）。
		for _, e := range h.log.snapshot() {
			for k, v := range e.Fields {
				if s, ok := v.(string); ok && strings.Contains(s, testSecretValue) {
					t.Errorf("secret value leaked into log field %q: %q", k, s)
				}
			}
		}
	})

	t.Run("成功時の監査 Detail に raw body の機密値を載せない", func(t *testing.T) {
		// Arrange: 検証は通過するが機密フィールドを含む body。
		h := newServiceHarness()
		actor, tenantID := uuid.New(), uuid.New()
		body := validBody()
		body["customSecretField"] = testSecretValue // pass-through される機密値

		// Act
		_, err := h.svc.Create(context.Background(), actor, tenantID, PolicyRequest{Name: testPolicyName, Body: body})

		// Assert
		if err != nil {
			t.Fatalf("expected success, got %v", err)
		}
		for _, ev := range h.recorder.recorded() {
			assertNoSecretInDetail(t, ev.Detail)
		}
		// AMAPI へは pass-through で機密値が渡る（これは正常 / 監査やログに出ないことだけを検証）。
		call, _ := h.amapi.lastCall()
		if call.Body.Raw["customSecretField"] != testSecretValue {
			t.Error("expected secret to pass through to AMAPI body (pass-through contract)")
		}
	})
}

// assertNoSecretInDetail は監査 Detail（再帰的に）に testSecretValue が含まれないことを検証する。
func assertNoSecretInDetail(t *testing.T, detail map[string]any) {
	t.Helper()
	for k, v := range detail {
		if s, ok := v.(string); ok && strings.Contains(s, testSecretValue) {
			t.Errorf("secret value leaked into audit detail field %q: %q", k, s)
		}
		if m, ok := v.(map[string]any); ok {
			assertNoSecretInDetail(t, m)
		}
	}
}

// ===== Update: 検証ゲート / 既存行確認 / AMAPI 失敗 / 成功 =====

func TestService_Update(t *testing.T) {
	existingRow := func(tenantID, policyID uuid.UUID) PolicyRow {
		return PolicyRow{
			ID:              policyID,
			TenantID:        tenantID,
			Name:            "old name",
			AMAPIPolicyName: testEnterpriseName + "/policies/" + policyID.String(),
			Body:            map[string]any{"old": true},
			Version:         3,
		}
	}

	t.Run("検証失敗のとき既存行確認も AMAPI も Repository も呼ばない", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		actor, tenantID, policyID := uuid.New(), uuid.New(), uuid.New()
		h.repo.getRow = existingRow(tenantID, policyID)
		body := map[string]any{keyEncryptionPolicy: "INVALID_ENUM"}

		// Act
		_, err := h.svc.Update(context.Background(), actor, tenantID, policyID, PolicyRequest{Name: testPolicyName, Body: body})

		// Assert: 検証が最初のゲートであり、失敗時は何も呼ばない（Req 2.5）。
		if err == nil {
			t.Fatal("expected validation error")
		}
		if h.amapi.callCount() != 0 || h.repo.updateCount() != 0 {
			t.Errorf("expected no AMAPI / Update calls, got amapi=%d update=%d", h.amapi.callCount(), h.repo.updateCount())
		}
	})

	t.Run("対象が自テナント不在のとき NotFound を伝達し AMAPI を呼ばない", func(t *testing.T) {
		// Arrange: Get が NotFound を返す（RLS 0 行 / 他テナント越境）。
		h := newServiceHarness()
		actor, tenantID, policyID := uuid.New(), uuid.New(), uuid.New()
		h.repo.getErr = ErrPolicyNotFound

		// Act
		_, err := h.svc.Update(context.Background(), actor, tenantID, policyID, PolicyRequest{Name: testPolicyName, Body: validBody()})

		// Assert（Req 4.1 / 4.4 / 4.5）。
		if got := codeOf(t, err); got != pkgerrors.CodeNotFound {
			t.Fatalf("expected CodeNotFound, got %s", got)
		}
		if h.amapi.callCount() != 0 {
			t.Errorf("expected AMAPI not called when target not found, got %d", h.amapi.callCount())
		}
	})

	t.Run("AMAPI が再試行不可エラーを返すとき snapshot を永続化しない", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		actor, tenantID, policyID := uuid.New(), uuid.New(), uuid.New()
		h.repo.getRow = existingRow(tenantID, policyID)
		amapiErr := pkgerrors.New(pkgerrors.CodeUpstream, "amapi rejected")
		h.amapi.err = amapiErr

		// Act
		_, err := h.svc.Update(context.Background(), actor, tenantID, policyID, PolicyRequest{Name: testPolicyName, Body: validBody()})

		// Assert（Req 1.4 / 1.5）。
		if h.repo.updateCount() != 0 {
			t.Errorf("expected Repository.Update NOT called on AMAPI failure, got %d", h.repo.updateCount())
		}
		if !stderrors.Is(err, amapiErr) {
			t.Errorf("expected AMAPI error propagated, got %v", err)
		}
	})

	t.Run("成功時に AMAPI 反映後 snapshot 更新 + 成功監査を記録し既存 version を据え置く", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		actor, tenantID, policyID := uuid.New(), uuid.New(), uuid.New()
		existing := existingRow(tenantID, policyID)
		h.repo.getRow = existing
		h.repo.updateAff = 1

		// Act
		view, err := h.svc.Update(context.Background(), actor, tenantID, policyID, PolicyRequest{Name: testPolicyName, Body: validBody()})

		// Assert（Req 1.2 / 1.5 / 5.2）。
		if err != nil {
			t.Fatalf("expected success, got %v", err)
		}
		if h.amapi.callCount() != 1 || h.repo.updateCount() != 1 {
			t.Fatalf("expected 1 AMAPI + 1 Update call, got amapi=%d update=%d", h.amapi.callCount(), h.repo.updateCount())
		}
		if view.Version != existing.Version {
			t.Errorf("expected version preserved as %d, got %d", existing.Version, view.Version)
		}
		evs := h.recorder.recorded()
		if len(evs) != 1 || evs[0].Result != audit.ResultSuccess || evs[0].EventType != eventTypePolicyUpdate {
			t.Fatalf("expected 1 success policy_update audit event, got %+v", evs)
		}
		// UpsertPolicy へ既存 amapi_policy_name の末尾 policyId を渡す（enterpriseName 二重連結なし）。
		call, _ := h.amapi.lastCall()
		if call.PolicyName != policyID.String() {
			t.Errorf("expected short policyId %q, got %q", policyID.String(), call.PolicyName)
		}
	})

	t.Run("更新で affected=0 のとき NotFound を返す", func(t *testing.T) {
		// Arrange: Get は成功するが、反映後の Update が 0 行（競合削除等）。
		h := newServiceHarness()
		actor, tenantID, policyID := uuid.New(), uuid.New(), uuid.New()
		h.repo.getRow = existingRow(tenantID, policyID)
		h.repo.updateAff = 0

		// Act
		_, err := h.svc.Update(context.Background(), actor, tenantID, policyID, PolicyRequest{Name: testPolicyName, Body: validBody()})

		// Assert（Req 4.4 / 4.5）。
		if got := codeOf(t, err); got != pkgerrors.CodeNotFound {
			t.Fatalf("expected CodeNotFound on affected=0, got %s", got)
		}
	})
}

// ===== Get: 自テナント参照 / 不在は NotFound / read のため監査なし（Req 4.4 / 4.5） =====

func TestService_Get(t *testing.T) {
	t.Run("自テナントの policy 詳細を row から充填して返す", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		tenantID, policyID := uuid.New(), uuid.New()
		h.repo.getRow = PolicyRow{
			ID:              policyID,
			TenantID:        tenantID,
			Name:            testPolicyName,
			AMAPIPolicyName: testEnterpriseName + "/policies/" + policyID.String(),
			Body:            map[string]any{"foo": "bar"},
			Version:         7,
		}

		// Act
		view, err := h.svc.Get(context.Background(), tenantID, policyID)

		// Assert: row の field（id / name / body / version）が view に充填される（Req 4.4）。
		if err != nil {
			t.Fatalf("expected success, got %v", err)
		}
		if view.ID != policyID || view.Name != testPolicyName || view.Version != 7 {
			t.Errorf("unexpected view: %+v", view)
		}
		if view.Body["foo"] != "bar" {
			t.Errorf("expected body passed through, got %+v", view.Body)
		}
	})

	t.Run("不在 policy の Get は存在差を露出しない NotFound を伝達する", func(t *testing.T) {
		// Arrange: Repository が ErrPolicyNotFound（RLS 0 行 / 他テナント越境）を返す。
		h := newServiceHarness()
		tenantID, policyID := uuid.New(), uuid.New()
		h.repo.getErr = ErrPolicyNotFound

		// Act
		_, err := h.svc.Get(context.Background(), tenantID, policyID)

		// Assert（Req 4.4 / 4.5）。
		if got := codeOf(t, err); got != pkgerrors.CodeNotFound {
			t.Fatalf("expected CodeNotFound, got %s", got)
		}
	})

	t.Run("read 操作のため監査記録を行わない", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		tenantID, policyID := uuid.New(), uuid.New()
		h.repo.getRow = PolicyRow{ID: policyID, TenantID: tenantID, Name: testPolicyName}

		// Act
		_, _ = h.svc.Get(context.Background(), tenantID, policyID)

		// Assert: 参照操作は監査対象外（Req 5.x は作成・更新・削除・割当のみ）。
		if evs := h.recorder.recorded(); len(evs) != 0 {
			t.Errorf("expected no audit events for read, got %d", len(evs))
		}
	})
}

// ===== List: 自テナント一覧 / 空 slice / read のため監査なし（Req 4.4） =====

func TestService_List(t *testing.T) {
	t.Run("自テナントの policy 一覧を summary へ写像して返す", func(t *testing.T) {
		// Arrange: 2 件の自テナント行（Body は summary に載らない）。
		h := newServiceHarness()
		tenantID := uuid.New()
		id1, id2 := uuid.New(), uuid.New()
		h.repo.listRows = []PolicyRow{
			{ID: id1, TenantID: tenantID, Name: "policy A", Version: 1, Body: map[string]any{"secret": testSecretValue}},
			{ID: id2, TenantID: tenantID, Name: "policy B", Version: 2, Body: map[string]any{"secret": testSecretValue}},
		}

		// Act
		summaries, err := h.svc.List(context.Background(), tenantID)

		// Assert（Req 4.4）。
		if err != nil {
			t.Fatalf("expected success, got %v", err)
		}
		if len(summaries) != 2 {
			t.Fatalf("expected 2 summaries, got %d", len(summaries))
		}
		if summaries[0].ID != id1 || summaries[0].Name != "policy A" || summaries[0].Version != 1 {
			t.Errorf("unexpected summary[0]: %+v", summaries[0])
		}
	})

	t.Run("0 件のとき非 nil の空 slice を返す", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		tenantID := uuid.New()
		h.repo.listRows = nil

		// Act
		summaries, err := h.svc.List(context.Background(), tenantID)

		// Assert: nil ではなく空 slice（Handler が `[]` をシリアライズできる）。
		if err != nil {
			t.Fatalf("expected success, got %v", err)
		}
		if summaries == nil {
			t.Fatal("expected non-nil empty slice, got nil")
		}
		if len(summaries) != 0 {
			t.Errorf("expected 0 summaries, got %d", len(summaries))
		}
	})

	t.Run("read 操作のため監査記録を行わない", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		tenantID := uuid.New()
		h.repo.listRows = []PolicyRow{{ID: uuid.New(), TenantID: tenantID, Name: "policy A"}}

		// Act
		_, _ = h.svc.List(context.Background(), tenantID)

		// Assert
		if evs := h.recorder.recorded(); len(evs) != 0 {
			t.Errorf("expected no audit events for list, got %d", len(evs))
		}
	})

	t.Run("Repository が error を返すとき伝達する", func(t *testing.T) {
		// Arrange: DB 不通等。
		h := newServiceHarness()
		tenantID := uuid.New()
		listErr := pkgerrors.New(pkgerrors.CodeUnavailable, "db unavailable")
		h.repo.listErr = listErr

		// Act
		_, err := h.svc.List(context.Background(), tenantID)

		// Assert
		if !stderrors.Is(err, listErr) {
			t.Errorf("expected list error propagated, got %v", err)
		}
	})
}

// ===== Delete: 成功監査 / 不在は NotFound / 割当済み Conflict 伝達 + 失敗監査（Req 5.3 / 4.4 / 4.5） =====

func TestService_Delete(t *testing.T) {
	t.Run("削除成功時に Repository.Delete を委譲し成功監査を記録する", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		actor, tenantID, policyID := uuid.New(), uuid.New(), uuid.New()
		h.repo.deleteAff = 1

		// Act
		err := h.svc.Delete(context.Background(), actor, tenantID, policyID)

		// Assert: 委譲 + 成功監査（Req 5.3）。
		if err != nil {
			t.Fatalf("expected success, got %v", err)
		}
		if h.repo.deleteCount() != 1 {
			t.Fatalf("expected Repository.Delete called once, got %d", h.repo.deleteCount())
		}
		evs := h.recorder.recorded()
		if len(evs) != 1 || evs[0].Result != audit.ResultSuccess || evs[0].EventType != eventTypePolicyDelete {
			t.Fatalf("expected 1 success policy_delete audit event, got %+v", evs)
		}
		if evs[0].ResourceID != policyID.String() {
			t.Errorf("expected ResourceID=policy_id, got %q", evs[0].ResourceID)
		}
	})

	t.Run("自テナント不在（affected=0）のとき NotFound を返し失敗監査を記録する", func(t *testing.T) {
		// Arrange: WHERE tenant_id で 0 行（不在 / 他テナント越境）。
		h := newServiceHarness()
		actor, tenantID, policyID := uuid.New(), uuid.New(), uuid.New()
		h.repo.deleteAff = 0

		// Act
		err := h.svc.Delete(context.Background(), actor, tenantID, policyID)

		// Assert（Req 4.4 / 4.5）。
		if got := codeOf(t, err); got != pkgerrors.CodeNotFound {
			t.Fatalf("expected CodeNotFound on affected=0, got %s", got)
		}
		evs := h.recorder.recorded()
		if len(evs) != 1 || evs[0].Result != audit.ResultFailure || evs[0].EventType != eventTypePolicyDelete {
			t.Fatalf("expected 1 failure policy_delete audit event, got %+v", evs)
		}
	})

	t.Run("割当済み端末ありの削除（Conflict）を 409 で伝達し失敗監査を記録する", func(t *testing.T) {
		// Arrange: Repository が ErrDeleteConflict（FK 違反 / 409）を返す。
		h := newServiceHarness()
		actor, tenantID, policyID := uuid.New(), uuid.New(), uuid.New()
		h.repo.deleteErr = ErrDeleteConflict

		// Act
		err := h.svc.Delete(context.Background(), actor, tenantID, policyID)

		// Assert: 409 を伝達（Req 5.3 / design 確認事項 3）。
		if got := codeOf(t, err); got != pkgerrors.CodeConflict {
			t.Fatalf("expected CodeConflict (409), got %s", got)
		}
		if !stderrors.Is(err, ErrDeleteConflict) {
			t.Errorf("expected ErrDeleteConflict propagated, got %v", err)
		}
		// 削除失敗も監査対象（Req 5.3）。
		evs := h.recorder.recorded()
		if len(evs) != 1 || evs[0].Result != audit.ResultFailure || evs[0].EventType != eventTypePolicyDelete {
			t.Fatalf("expected 1 failure policy_delete audit event, got %+v", evs)
		}
	})

	t.Run("監査 Detail に機密値を載せない", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		actor, tenantID, policyID := uuid.New(), uuid.New(), uuid.New()
		h.repo.deleteAff = 1

		// Act
		_ = h.svc.Delete(context.Background(), actor, tenantID, policyID)

		// Assert: Detail に testSecretValue が含まれない（Req 5.4）。Delete は raw body を持たないが、
		// 安全 field のみ（policy_id / result）が載ることを担保する。
		for _, ev := range h.recorder.recorded() {
			assertNoSecretInDetail(t, ev.Detail)
			if _, ok := ev.Detail["policy_id"]; !ok {
				t.Error("expected policy_id in delete audit detail")
			}
		}
	})
}

// ===== Assign: 成功で applied_policy_id 確定 / 他テナント不在は NotFound（Req 3.x / 4.2 / 4.3） =====

func TestService_Assign(t *testing.T) {
	t.Run("割当成功時に AssignPolicyToDevice へ委譲し成功監査を記録する", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		actor, tenantID, deviceID, policyID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
		h.repo.assignAff = 1

		// Act
		err := h.svc.Assign(context.Background(), actor, tenantID, deviceID, policyID)

		// Assert: 委譲（tenantID / deviceID / policyID）+ 成功監査（Req 3.1）。
		if err != nil {
			t.Fatalf("expected success, got %v", err)
		}
		call, ok := h.repo.lastAssignCall()
		if !ok || call.TenantID != tenantID || call.DeviceID != deviceID || call.PolicyID != policyID {
			t.Fatalf("unexpected assign call: %+v ok=%v", call, ok)
		}
		evs := h.recorder.recorded()
		if len(evs) != 1 || evs[0].Result != audit.ResultSuccess || evs[0].EventType != eventTypePolicyAssign {
			t.Fatalf("expected 1 success policy_assign audit event, got %+v", evs)
		}
	})

	t.Run("割当成功時に AMAPI を呼ばない（DB 更新までに限定）", func(t *testing.T) {
		// Arrange: 割当は AMAPI device patch を行わない（design 確認事項 1）。
		h := newServiceHarness()
		actor, tenantID, deviceID, policyID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
		h.repo.assignAff = 1

		// Act
		_ = h.svc.Assign(context.Background(), actor, tenantID, deviceID, policyID)

		// Assert
		if h.amapi.callCount() != 0 {
			t.Errorf("expected AMAPI not called on assign, got %d", h.amapi.callCount())
		}
	})

	t.Run("他テナント device 指定（affected=0）のとき NotFound を返し失敗監査を記録する", func(t *testing.T) {
		// Arrange: WHERE id AND tenant_id で 0 行 → (0, nil)（Req 3.3 / 4.3）。
		h := newServiceHarness()
		actor, tenantID, deviceID, policyID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
		h.repo.assignAff = 0
		h.repo.assignErr = nil

		// Act
		err := h.svc.Assign(context.Background(), actor, tenantID, deviceID, policyID)

		// Assert（Req 3.3 / 4.3 / 4.5）。
		if got := codeOf(t, err); got != pkgerrors.CodeNotFound {
			t.Fatalf("expected CodeNotFound on affected=0, got %s", got)
		}
		evs := h.recorder.recorded()
		if len(evs) != 1 || evs[0].Result != audit.ResultFailure || evs[0].EventType != eventTypePolicyAssign {
			t.Fatalf("expected 1 failure policy_assign audit event, got %+v", evs)
		}
	})

	t.Run("他テナント policy 指定（複合 FK 違反）のとき NotFound を伝達し失敗監査を記録する", func(t *testing.T) {
		// Arrange: Repository が ErrPolicyNotFound（FK 違反 / 存在差非露出）を返す（Req 3.2 / 4.2）。
		h := newServiceHarness()
		actor, tenantID, deviceID, policyID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
		h.repo.assignErr = ErrPolicyNotFound

		// Act
		err := h.svc.Assign(context.Background(), actor, tenantID, deviceID, policyID)

		// Assert（Req 3.2 / 4.2 / 4.5）。
		if got := codeOf(t, err); got != pkgerrors.CodeNotFound {
			t.Fatalf("expected CodeNotFound on FK violation, got %s", got)
		}
		if !stderrors.Is(err, ErrPolicyNotFound) {
			t.Errorf("expected ErrPolicyNotFound propagated, got %v", err)
		}
		evs := h.recorder.recorded()
		if len(evs) != 1 || evs[0].Result != audit.ResultFailure {
			t.Fatalf("expected 1 failure audit event, got %+v", evs)
		}
	})

	t.Run("割当監査 Detail に device_id を載せ機密値を載せない", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		actor, tenantID, deviceID, policyID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
		h.repo.assignAff = 1

		// Act
		_ = h.svc.Assign(context.Background(), actor, tenantID, deviceID, policyID)

		// Assert: Detail に device_id / policy_id が載り、機密値は載らない（Req 5.4）。
		evs := h.recorder.recorded()
		if len(evs) != 1 {
			t.Fatalf("expected 1 audit event, got %d", len(evs))
		}
		assertNoSecretInDetail(t, evs[0].Detail)
		if evs[0].Detail["device_id"] != deviceID.String() {
			t.Errorf("expected device_id in assign detail, got %+v", evs[0].Detail)
		}
		if evs[0].Detail["policy_id"] != policyID.String() {
			t.Errorf("expected policy_id in assign detail, got %+v", evs[0].Detail)
		}
	})
}

// ===== ValidationFailedError の top-level Code 規則（Req 2.2 / 2.3 / 2.4） =====

func TestNewValidationFailedError_TopLevelCode(t *testing.T) {
	t.Run("全件 BusinessRule のとき top-level Code は 422", func(t *testing.T) {
		// Arrange
		errs := []ValidationError{
			{Domain: DomainApp, Field: "AppCount", Kind: KindBusinessRule, Message: "too many apps"},
		}

		// Act
		got := newValidationFailedError(errs)

		// Assert
		if got.Code() != pkgerrors.CodeBusinessRule {
			t.Errorf("expected CodeBusinessRule, got %s", got.Code())
		}
	})

	t.Run("invalid_field が 1 件でも混在するとき top-level Code は 400", func(t *testing.T) {
		// Arrange
		errs := []ValidationError{
			{Domain: DomainApp, Field: "AppCount", Kind: KindBusinessRule, Message: "too many apps"},
			{Domain: DomainSecurity, Field: "EncryptionPolicy", Kind: KindInvalidField, Message: "invalid enum"},
		}

		// Act
		got := newValidationFailedError(errs)

		// Assert
		if got.Code() != pkgerrors.CodeInvalidRequest {
			t.Errorf("expected CodeInvalidRequest, got %s", got.Code())
		}
	})

	t.Run("Error 文字列に raw body 生値を含めない", func(t *testing.T) {
		// Arrange
		errs := []ValidationError{
			{Domain: DomainSecurity, Field: "EncryptionPolicy", Kind: KindInvalidField, Message: "invalid enum"},
		}

		// Act
		got := newValidationFailedError(errs)

		// Assert: error string に secret が混入しない（Message には機密値を載せない契約）。
		if strings.Contains(got.Error(), testSecretValue) {
			t.Errorf("error string leaked secret: %q", got.Error())
		}
	})
}
