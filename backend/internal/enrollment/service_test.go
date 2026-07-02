package enrollment

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap/zapcore"

	"github.com/hitoshiichikawa/ae-mdm/internal/audit"
	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/amapi"
)

// ---- 機密値漏洩 assertion 用の固定文字列（Req 5.2 / NFR 3.1 観点） ----
//
// AMAPI が返す Value / QRCode は秘密値であり、監査 Detail / 構造化ログ / snapshot に一切
// surface してはならない。TokenView（HTTP 応答一度きり）のみが返却経路である。
const (
	testEnterpriseName = "enterprises/LC0123456789"
	testTokenName      = "enterprises/LC0123456789/enrollmentTokens/TID-1"
	testTokenValue     = "DO-NOT-LEAK-TOKEN-VALUE-abc123"
	testQRCodeData     = "DO-NOT-LEAK-QR-CODE-xyz789"
	testAMAPIPolicyID  = "policy-uuid-string-1234"
	testExpiration     = "2026-07-02T13:00:00Z"
)

// ---- Fake TokenRepository（snapshot 永続化 / 一覧 / 呼び出し有無を検証） ----

type fakeTokenRepository struct {
	mu sync.Mutex

	inserted  []TokenRow
	insertErr error

	listRows []TokenRow
	listErr  error
}

func (r *fakeTokenRepository) Insert(_ context.Context, row TokenRow) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	// 呼び出し試行を記録してから error を返す（insertCount は「試行回数」を表す）。
	r.inserted = append(r.inserted, row)
	return r.insertErr
}

func (r *fakeTokenRepository) List(_ context.Context, _ uuid.UUID) ([]TokenRow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.listErr != nil {
		return nil, r.listErr
	}
	out := make([]TokenRow, len(r.listRows))
	copy(out, r.listRows)
	return out, nil
}

func (r *fakeTokenRepository) insertCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.inserted)
}

// ---- Fake enterpriseResolver（enterprise_name 解決 / Req 2.3 前提） ----

type fakeEnterpriseResolver struct {
	name string
	err  error
}

func (r *fakeEnterpriseResolver) EnterpriseNameForTenant(_ context.Context, _ uuid.UUID) (string, error) {
	if r.err != nil {
		return "", r.err
	}
	return r.name, nil
}

// ---- Fake policyChecker（DEDICATED policy 検証 / Req 1.5） ----

type fakePolicyChecker struct {
	mu      sync.Mutex
	calls   []policyCheckCall
	amapiID string
	err     error
}

type policyCheckCall struct {
	TenantID uuid.UUID
	PolicyID uuid.UUID
}

func (c *fakePolicyChecker) ResolveOwnedPolicy(_ context.Context, tenantID, policyID uuid.UUID) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, policyCheckCall{TenantID: tenantID, PolicyID: policyID})
	if c.err != nil {
		return "", c.err
	}
	return c.amapiID, nil
}

func (c *fakePolicyChecker) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
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

// ---- Fake Logger（logDeny / logInconsistency の field 検証 / 機密値漏洩検証 / NFR 3.1） ----

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

func (l *fakeLogger) snapshot() []fakeLogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]fakeLogEntry, len(l.entries))
	copy(out, l.entries)
	return out
}

// zapFieldValue は zapcore.Field から値を取り出す（string / int / interface を扱う）。
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

// ---- Fake Clock（ListTokens の status 派生を決定的にする） ----

type fakeClock struct {
	now time.Time
}

func (c fakeClock) Now() time.Time { return c.now }

// ---- Test harness ----

type serviceHarness struct {
	svc      Service
	repo     *fakeTokenRepository
	client   *amapi.StubClient
	recorder *fakeRecorder
	resolver *fakeEnterpriseResolver
	policies *fakePolicyChecker
	log      *fakeLogger
}

// fixedNow は ListTokens テストで active / expired 境界を決定的にするための固定現在時刻。
var fixedNow = time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)

func newServiceHarness() *serviceHarness {
	repo := &fakeTokenRepository{}
	client := &amapi.StubClient{}
	recorder := &fakeRecorder{}
	resolver := &fakeEnterpriseResolver{name: testEnterpriseName}
	policies := &fakePolicyChecker{amapiID: testAMAPIPolicyID}
	log := &fakeLogger{}
	svc := NewService(repo, client, recorder, resolver, policies, fakeClock{now: fixedNow}, log)
	return &serviceHarness{
		svc:      svc,
		repo:     repo,
		client:   client,
		recorder: recorder,
		resolver: resolver,
		policies: policies,
		log:      log,
	}
}

// stubIssuedToken は AMAPI が正常にトークンを払い出す hook を設定する（秘密値 + RFC3339 expiration）。
func (h *serviceHarness) stubIssuedToken(expiration string) {
	h.client.OnCreateEnrollmentToken = func(_ context.Context, _ string, _ amapi.EnrollmentTokenRequest) (amapi.EnrollmentToken, error) {
		return amapi.EnrollmentToken{
			Name:           testTokenName,
			Value:          testTokenValue,
			QRCode:         testQRCodeData,
			ExpirationTime: expiration,
		}, nil
	}
}

// lastEnrollmentRequest は StubClient の呼び出し履歴から直近の CreateEnrollmentToken 引数を取り出す。
func lastEnrollmentRequest(t *testing.T, c *amapi.StubClient) amapi.EnrollmentTokenRequest {
	t.Helper()
	for i := len(c.Calls) - 1; i >= 0; i-- {
		if c.Calls[i].Method != "CreateEnrollmentToken" {
			continue
		}
		req, ok := c.Calls[i].Args[0].(amapi.EnrollmentTokenRequest)
		if !ok {
			t.Fatalf("unexpected CreateEnrollmentToken arg type %T", c.Calls[i].Args[0])
		}
		return req
	}
	t.Fatal("CreateEnrollmentToken was not called")
	return amapi.EnrollmentTokenRequest{}
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

// ===== Req 1.1 / 1.3: FULLY_MANAGED 発行 =====

func TestService_IssueToken_FullyManaged_UsesDisallowedPersonalUsage(t *testing.T) {
	// Arrange
	h := newServiceHarness()
	h.stubIssuedToken(testExpiration)
	actor, tenantID := uuid.New(), uuid.New()

	// Act
	_, err := h.svc.IssueToken(context.Background(), actor, tenantID, IssueRequest{Mode: ModeFullyManaged})

	// Assert: 個人利用不可固定で AMAPI が 1 回呼ばれ、PolicyName は未設定（FULLY_MANAGED / Req 1.1）。
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := h.client.CallCount("CreateEnrollmentToken"); got != 1 {
		t.Fatalf("expected CreateEnrollmentToken called once, got %d", got)
	}
	req := lastEnrollmentRequest(t, h.client)
	if req.AllowPersonalUsage != "PERSONAL_USAGE_DISALLOWED" {
		t.Errorf("expected AllowPersonalUsage=PERSONAL_USAGE_DISALLOWED, got %q", req.AllowPersonalUsage)
	}
	if req.PolicyName != "" {
		t.Errorf("expected empty PolicyName for FULLY_MANAGED, got %q", req.PolicyName)
	}
}

func TestService_IssueToken_FullyManaged_EmbedsTenantIssuerModeInAdditionalData(t *testing.T) {
	// Arrange
	h := newServiceHarness()
	h.stubIssuedToken(testExpiration)
	actor, tenantID := uuid.New(), uuid.New()

	// Act
	_, err := h.svc.IssueToken(context.Background(), actor, tenantID, IssueRequest{Mode: ModeFullyManaged})

	// Assert: additionalData に tenant_id / issued_by / mode が含まれる（Req 1.3）。
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	req := lastEnrollmentRequest(t, h.client)
	var ad map[string]string
	if uerr := json.Unmarshal([]byte(req.AdditionalData), &ad); uerr != nil {
		t.Fatalf("additionalData is not valid JSON: %v (%q)", uerr, req.AdditionalData)
	}
	if ad["tenant_id"] != tenantID.String() {
		t.Errorf("expected tenant_id=%s, got %q", tenantID, ad["tenant_id"])
	}
	if ad["issued_by"] != actor.String() {
		t.Errorf("expected issued_by=%s, got %q", actor, ad["issued_by"])
	}
	if ad["mode"] != string(ModeFullyManaged) {
		t.Errorf("expected mode=%s, got %q", ModeFullyManaged, ad["mode"])
	}
}

func TestService_IssueToken_ReturnsSecretQRDataInViewOnly(t *testing.T) {
	// Arrange
	h := newServiceHarness()
	h.stubIssuedToken(testExpiration)
	actor, tenantID := uuid.New(), uuid.New()

	// Act
	view, err := h.svc.IssueToken(context.Background(), actor, tenantID, IssueRequest{Mode: ModeFullyManaged})

	// Assert: TokenView が秘密値（Value / QRCodeData）と parse 済み expires_at を一度だけ返す（Req 1.1 / NFR 3.1）。
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if view.Value != testTokenValue {
		t.Errorf("expected Value=%q, got %q", testTokenValue, view.Value)
	}
	if view.QRCodeData != testQRCodeData {
		t.Errorf("expected QRCodeData=%q, got %q", testQRCodeData, view.QRCodeData)
	}
	wantExp, _ := time.Parse(time.RFC3339, testExpiration)
	if !view.ExpiresAt.Equal(wantExp) {
		t.Errorf("expected ExpiresAt=%v, got %v", wantExp, view.ExpiresAt)
	}
	if view.ID == uuid.Nil {
		t.Error("expected non-nil token ID in view")
	}
}

// ===== Req 1.2: DEDICATED 発行で PolicyName に amapi policy id を設定 =====

func TestService_IssueToken_Dedicated_SetsResolvedPolicyName(t *testing.T) {
	// Arrange
	h := newServiceHarness()
	h.stubIssuedToken(testExpiration)
	actor, tenantID := uuid.New(), uuid.New()
	policyID := uuid.New()

	// Act
	_, err := h.svc.IssueToken(context.Background(), actor, tenantID, IssueRequest{Mode: ModeDedicated, PolicyID: &policyID})

	// Assert: policyChecker が呼ばれ、解決した amapi policy id が PolicyName に設定される（Req 1.2）。
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if h.policies.callCount() != 1 {
		t.Fatalf("expected ResolveOwnedPolicy called once, got %d", h.policies.callCount())
	}
	req := lastEnrollmentRequest(t, h.client)
	if req.PolicyName != testAMAPIPolicyID {
		t.Errorf("expected PolicyName=%q, got %q", testAMAPIPolicyID, req.PolicyName)
	}
	if req.AllowPersonalUsage != "PERSONAL_USAGE_DISALLOWED" {
		t.Errorf("expected AllowPersonalUsage=PERSONAL_USAGE_DISALLOWED for DEDICATED, got %q", req.AllowPersonalUsage)
	}
}

// ===== Req 1.4: 不正/未指定モード → 生成せずエラー・AMAPI 非呼出 =====

func TestService_IssueToken_InvalidMode_ReturnsErrorWithoutCallingAMAPI(t *testing.T) {
	cases := map[string]Mode{
		"未指定（空文字）": Mode(""),
		"未知の値":     Mode("byod"),
	}
	for name, mode := range cases {
		t.Run(name, func(t *testing.T) {
			// Arrange
			h := newServiceHarness()
			h.stubIssuedToken(testExpiration)
			actor, tenantID := uuid.New(), uuid.New()

			// Act
			_, err := h.svc.IssueToken(context.Background(), actor, tenantID, IssueRequest{Mode: mode})

			// Assert: ErrInvalidMode(400) を返し、AMAPI も snapshot も呼ばない（Req 1.4）。
			if !stderrors.Is(err, ErrInvalidMode) {
				t.Fatalf("expected ErrInvalidMode, got %v", err)
			}
			if got := h.client.CallCount("CreateEnrollmentToken"); got != 0 {
				t.Errorf("expected AMAPI not called, got %d calls", got)
			}
			if h.repo.insertCount() != 0 {
				t.Errorf("expected Insert not called, got %d calls", h.repo.insertCount())
			}
		})
	}
}

// ===== Req 1.5: DEDICATED で policy 未指定/不在 → 発行せずエラー・AMAPI 非呼出 =====

func TestService_IssueToken_DedicatedWithoutPolicy_ReturnsErrRequiredWithoutCallingAMAPI(t *testing.T) {
	nilPolicy := uuid.Nil
	cases := map[string]*uuid.UUID{
		"policy_id 欠落（nil）":    nil,
		"policy_id が uuid.Nil": &nilPolicy,
	}
	for name, pid := range cases {
		t.Run(name, func(t *testing.T) {
			// Arrange
			h := newServiceHarness()
			h.stubIssuedToken(testExpiration)
			actor, tenantID := uuid.New(), uuid.New()

			// Act
			_, err := h.svc.IssueToken(context.Background(), actor, tenantID, IssueRequest{Mode: ModeDedicated, PolicyID: pid})

			// Assert: ErrPolicyRequired を返し、policyChecker / AMAPI / Insert を呼ばない（Req 1.5）。
			if !stderrors.Is(err, ErrPolicyRequired) {
				t.Fatalf("expected ErrPolicyRequired, got %v", err)
			}
			if h.policies.callCount() != 0 {
				t.Errorf("expected ResolveOwnedPolicy not called, got %d", h.policies.callCount())
			}
			if got := h.client.CallCount("CreateEnrollmentToken"); got != 0 {
				t.Errorf("expected AMAPI not called, got %d calls", got)
			}
			if h.repo.insertCount() != 0 {
				t.Errorf("expected Insert not called, got %d calls", h.repo.insertCount())
			}
		})
	}
}

func TestService_IssueToken_DedicatedPolicyNotFound_PropagatesWithoutCallingAMAPI(t *testing.T) {
	// Arrange: policyChecker が存在差非露出の NotFound を返す（自テナント不在 / 越境 / Req 1.5 / 2.3）。
	h := newServiceHarness()
	h.stubIssuedToken(testExpiration)
	notFound := pkgerrors.New(pkgerrors.CodeNotFound, "policy not found")
	h.policies.err = notFound
	actor, tenantID := uuid.New(), uuid.New()
	policyID := uuid.New()

	// Act
	_, err := h.svc.IssueToken(context.Background(), actor, tenantID, IssueRequest{Mode: ModeDedicated, PolicyID: &policyID})

	// Assert: NotFound をそのまま伝達し、AMAPI / Insert を呼ばない（Req 1.5）。
	if !stderrors.Is(err, notFound) {
		t.Fatalf("expected propagated NotFound, got %v", err)
	}
	if codeOf(t, err) != pkgerrors.CodeNotFound {
		t.Errorf("expected CodeNotFound, got %s", codeOf(t, err))
	}
	if got := h.client.CallCount("CreateEnrollmentToken"); got != 0 {
		t.Errorf("expected AMAPI not called, got %d calls", got)
	}
	if h.repo.insertCount() != 0 {
		t.Errorf("expected Insert not called, got %d calls", h.repo.insertCount())
	}
}

// ===== Req 1.6 / 5.1: AMAPI エラー → snapshot 非永続化 + failure 監査 =====

func TestService_IssueToken_AMAPIError_SkipsInsertAndRecordsFailureAudit(t *testing.T) {
	// Arrange: AMAPI が再試行不可エラー（4xx 相当）を返す。
	h := newServiceHarness()
	amapiErr := pkgerrors.New(pkgerrors.CodeInvalidRequest, "amapi rejected enrollment token")
	h.client.OnCreateEnrollmentToken = func(_ context.Context, _ string, _ amapi.EnrollmentTokenRequest) (amapi.EnrollmentToken, error) {
		return amapi.EnrollmentToken{}, amapiErr
	}
	actor, tenantID := uuid.New(), uuid.New()

	// Act
	_, err := h.svc.IssueToken(context.Background(), actor, tenantID, IssueRequest{Mode: ModeFullyManaged})

	// Assert: AMAPI は呼ばれるが snapshot は永続化されず、失敗監査が記録される（Req 1.6 / 5.1）。
	if !stderrors.Is(err, amapiErr) {
		t.Fatalf("expected propagated AMAPI error, got %v", err)
	}
	if got := h.client.CallCount("CreateEnrollmentToken"); got != 1 {
		t.Fatalf("expected AMAPI called once, got %d", got)
	}
	if h.repo.insertCount() != 0 {
		t.Errorf("expected Insert not called on AMAPI error, got %d", h.repo.insertCount())
	}
	evs := h.recorder.recorded()
	if len(evs) != 1 {
		t.Fatalf("expected 1 failure audit event, got %d", len(evs))
	}
	if evs[0].Result != audit.ResultFailure {
		t.Errorf("expected ResultFailure, got %s", evs[0].Result)
	}
	if evs[0].EventType != eventTypeEnrollmentTokenIssue {
		t.Errorf("expected event type %s, got %s", eventTypeEnrollmentTokenIssue, evs[0].EventType)
	}
}

// ===== Req 5.1: 発行成功時の監査内容（発行者 / テナント / モード / 有効期限 / 結果） =====

func TestService_IssueToken_Success_RecordsAuditWithSafeFields(t *testing.T) {
	// Arrange
	h := newServiceHarness()
	h.stubIssuedToken(testExpiration)
	actor, tenantID := uuid.New(), uuid.New()

	// Act
	view, err := h.svc.IssueToken(context.Background(), actor, tenantID, IssueRequest{Mode: ModeFullyManaged})

	// Assert: 成功監査 1 件が安全 field（mode / expires_at / result）で記録される（Req 5.1）。
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	evs := h.recorder.recorded()
	if len(evs) != 1 {
		t.Fatalf("expected 1 audit event, got %d", len(evs))
	}
	ev := evs[0]
	if ev.Result != audit.ResultSuccess {
		t.Errorf("expected ResultSuccess, got %s", ev.Result)
	}
	if ev.ActorID != actor || ev.TenantID != tenantID {
		t.Errorf("expected actor=%s tenant=%s, got actor=%s tenant=%s", actor, tenantID, ev.ActorID, ev.TenantID)
	}
	if ev.ResourceID != view.ID.String() {
		t.Errorf("expected ResourceID=%s, got %s", view.ID, ev.ResourceID)
	}
	if ev.Detail["mode"] != string(ModeFullyManaged) {
		t.Errorf("expected detail.mode=%s, got %v", ModeFullyManaged, ev.Detail["mode"])
	}
	if ev.Detail["result"] != string(audit.ResultSuccess) {
		t.Errorf("expected detail.result=success, got %v", ev.Detail["result"])
	}
	if _, ok := ev.Detail["expires_at"]; !ok {
		t.Error("expected detail.expires_at to be present")
	}
}

// ===== Req 5.2 / NFR 3.1: 監査 Detail / 構造化ログに秘密値（Value / QRCode）を混入させない =====

func TestService_IssueToken_DoesNotLeakSecretToAuditOrLogs(t *testing.T) {
	assertNoSecretLeak := func(t *testing.T, h *serviceHarness) {
		t.Helper()
		// 監査 Detail に秘密値が混入しない（Detail を JSON 直列化して走査 / Req 5.2）。
		for _, ev := range h.recorder.recorded() {
			raw, _ := json.Marshal(ev.Detail)
			if strings.Contains(string(raw), testTokenValue) || strings.Contains(string(raw), testQRCodeData) {
				t.Errorf("secret value leaked into audit detail: %s", raw)
			}
		}
		// 構造化ログの field に秘密値が混入しない（NFR 3.1）。
		for _, e := range h.log.snapshot() {
			for k, v := range e.Fields {
				if s, ok := v.(string); ok && (strings.Contains(s, testTokenValue) || strings.Contains(s, testQRCodeData)) {
					t.Errorf("secret value leaked into log field %q of %q: %s", k, e.Msg, s)
				}
			}
		}
	}

	t.Run("発行成功経路で秘密値を漏らさない", func(t *testing.T) {
		// Arrange
		h := newServiceHarness()
		h.stubIssuedToken(testExpiration)
		actor, tenantID := uuid.New(), uuid.New()

		// Act
		_, err := h.svc.IssueToken(context.Background(), actor, tenantID, IssueRequest{Mode: ModeFullyManaged})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Assert
		assertNoSecretLeak(t, h)
	})

	t.Run("snapshot 永続化失敗の inconsistency ログでも秘密値を漏らさない", func(t *testing.T) {
		// Arrange: AMAPI 発行成功後に Insert が失敗し、inconsistency ERROR ログを出す経路。
		h := newServiceHarness()
		h.stubIssuedToken(testExpiration)
		h.repo.insertErr = pkgerrors.New(pkgerrors.CodeUnavailable, "db unavailable")
		actor, tenantID := uuid.New(), uuid.New()

		// Act
		_, err := h.svc.IssueToken(context.Background(), actor, tenantID, IssueRequest{Mode: ModeFullyManaged})

		// Assert: ErrTokenPersist(503) を伝達しつつ秘密値を漏らさない（Req 1.6 / NFR 3.1）。
		if !stderrors.Is(err, ErrTokenPersist) {
			t.Fatalf("expected ErrTokenPersist, got %v", err)
		}
		assertNoSecretLeak(t, h)
	})
}

// ===== Req 4.1: ListTokens が expires_at 由来 status（active / expired）を返す =====

func TestService_ListTokens_DerivesStatusFromExpiresAt(t *testing.T) {
	// Arrange: fixedNow を基準に、未来 expiry（active）と過去 expiry（expired）の 2 行を用意する。
	h := newServiceHarness()
	tenantID := uuid.New()
	activeID, expiredID := uuid.New(), uuid.New()
	h.repo.listRows = []TokenRow{
		{ID: activeID, TenantID: tenantID, Mode: ModeFullyManaged, ExpiresAt: fixedNow.Add(time.Hour)},
		{ID: expiredID, TenantID: tenantID, Mode: ModeDedicated, ExpiresAt: fixedNow.Add(-time.Hour)},
	}

	// Act
	got, err := h.svc.ListTokens(context.Background(), tenantID)

	// Assert
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 summaries, got %d", len(got))
	}
	statusByID := map[uuid.UUID]string{got[0].ID: got[0].Status, got[1].ID: got[1].Status}
	if statusByID[activeID] != StatusActive {
		t.Errorf("expected active status for future expiry, got %q", statusByID[activeID])
	}
	if statusByID[expiredID] != StatusExpired {
		t.Errorf("expected expired status for past expiry, got %q", statusByID[expiredID])
	}
}

func TestService_ListTokens_RepositoryError_Propagates(t *testing.T) {
	// Arrange
	h := newServiceHarness()
	h.repo.listErr = pkgerrors.New(pkgerrors.CodeUnavailable, "db unavailable")

	// Act
	_, err := h.svc.ListTokens(context.Background(), uuid.New())

	// Assert: Repository の CodeUnavailable をそのまま伝達する。
	if codeOf(t, err) != pkgerrors.CodeUnavailable {
		t.Errorf("expected CodeUnavailable, got %v", err)
	}
}
