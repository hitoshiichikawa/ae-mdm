package app

import (
	"context"
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

// ---- テスト固定値 ----
//
// testWebTokenValue は iframe 埋め込み用 webToken の秘匿値を模す。成功経路でこの値が構造化ログに
// surface しないこと（NFR 1.1）を検証する。
const (
	testEnterpriseName = "enterprises/LC0123456789"
	testParentFrameURL = "https://console.example.com/apps"
	testWebTokenValue  = "SECRET-WEBTOKEN-VALUE-DO-NOT-LEAK-abc123"
)

// ---- Fake webTokenClient（webToken 発行 / 呼び出し有無と引数を検証 / Req 1.1 / NFR 2.1） ----

type fakeWebTokenClient struct {
	mu    sync.Mutex
	calls []webTokenCall

	token amapi.WebToken
	err   error
}

type webTokenCall struct {
	EnterpriseName string
	ParentFrameURL string
}

func (c *fakeWebTokenClient) CreateWebToken(_ context.Context, enterpriseName, parentFrameURL string) (amapi.WebToken, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, webTokenCall{EnterpriseName: enterpriseName, ParentFrameURL: parentFrameURL})
	if c.err != nil {
		return amapi.WebToken{}, c.err
	}
	return c.token, nil
}

func (c *fakeWebTokenClient) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

func (c *fakeWebTokenClient) lastCall() (webTokenCall, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.calls) == 0 {
		return webTokenCall{}, false
	}
	return c.calls[len(c.calls)-1], true
}

// ---- Fake enterpriseResolver（enterprise_name 解決 / bind gate / Req 1.3） ----

type fakeResolver struct {
	mu    sync.Mutex
	calls int

	name string
	err  error
}

func (r *fakeResolver) EnterpriseNameForTenant(_ context.Context, _ uuid.UUID) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.err != nil {
		return "", r.err
	}
	return r.name, nil
}

func (r *fakeResolver) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// ---- Fake Repository（カタログ参照 Req 2.1 / 同期 Upsert Req 3.x / 承認済み ApprovedPackages Req 5.x の委譲を検証） ----

type fakeAppRepository struct {
	mu        sync.Mutex
	listCalls int

	listRows []TenantAppRow
	listErr  error

	// Upsert の注入設定と呼び出し捕捉（task 3 / Req 3.1 / 3.4 / 3.6）。
	upsertCalls int
	upsertApps  []SyncApp
	upsertCount int
	upsertErr   error

	// ApprovedPackages の注入設定と呼び出し捕捉（task 4 / Req 5.1 / 5.2）。
	approvedCalls    int
	approvedPackages []string
	approvedSet      map[string]struct{}
	approvedErr      error
}

func (r *fakeAppRepository) List(_ context.Context, _ uuid.UUID) ([]TenantAppRow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.listCalls++
	if r.listErr != nil {
		return nil, r.listErr
	}
	// 実 Repository は 0 件でも非 nil 空 slice を返す契約のため、それに倣う。
	out := make([]TenantAppRow, len(r.listRows))
	copy(out, r.listRows)
	return out, nil
}

// Upsert は注入された upsertCount / upsertErr を返し、渡された apps を捕捉する（Req 3.1 / 3.6）。
func (r *fakeAppRepository) Upsert(_ context.Context, _ uuid.UUID, apps []SyncApp) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.upsertCalls++
	r.upsertApps = append([]SyncApp(nil), apps...)
	if r.upsertErr != nil {
		return 0, r.upsertErr
	}
	return r.upsertCount, nil
}

// ApprovedPackages は注入された approvedSet / approvedErr を返し、渡された packageNames と呼び出し
// 回数を捕捉する（Req 5.1 / 5.2）。呼び出し回数は空入力で Repository へ触れないことの検証に用いる。
func (r *fakeAppRepository) ApprovedPackages(_ context.Context, _ uuid.UUID, packageNames []string) (map[string]struct{}, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.approvedCalls++
	r.approvedPackages = append([]string(nil), packageNames...)
	if r.approvedErr != nil {
		return nil, r.approvedErr
	}
	// 実 Repository は 0 件でも非 nil 空 map を返す契約のため、それに倣って注入集合を複製して返す。
	out := make(map[string]struct{}, len(r.approvedSet))
	for pkg := range r.approvedSet {
		out[pkg] = struct{}{}
	}
	return out, nil
}

func (r *fakeAppRepository) listCallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.listCalls
}

func (r *fakeAppRepository) upsertCallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.upsertCalls
}

func (r *fakeAppRepository) upsertLastApps() []SyncApp {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]SyncApp, len(r.upsertApps))
	copy(out, r.upsertApps)
	return out
}

func (r *fakeAppRepository) approvedCallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.approvedCalls
}

func (r *fakeAppRepository) approvedLastPackages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.approvedPackages))
	copy(out, r.approvedPackages)
	return out
}

// ---- Fake eventRecorder（同期監査の呼び出しと Detail を捕捉 / NFR 3.1 / 3.2） ----

type fakeEventRecorder struct {
	mu     sync.Mutex
	events []audit.Event
	err    error
}

func (r *fakeEventRecorder) Record(_ context.Context, ev audit.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return r.err
}

func (r *fakeEventRecorder) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

func (r *fakeEventRecorder) last() (audit.Event, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == 0 {
		return audit.Event{}, false
	}
	return r.events[len(r.events)-1], true
}

// detailLeaks は捕捉した全監査イベントの Detail 値（string 化）に secret 文字列が含まれるかを返す
// （NFR 3.2 の機密値非露出検証用）。
func (r *fakeEventRecorder) detailLeaks(secret string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		for _, v := range e.Detail {
			if s, ok := v.(string); ok && strings.Contains(s, secret) {
				return true
			}
		}
	}
	return false
}

// ---- Fake Logger（構造化ログを捕捉し秘匿値漏洩を検証 / NFR 1.1 / policy.fakeLogger と同型） ----

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

// zapFieldValue は zapcore.Field から値を取り出す（string field を主に扱う / 漏洩検証用）。
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

// leaks は捕捉した全ログエントリ（msg + 全 field 値）のいずれかに secret 文字列が含まれるかを返す
// （NFR 1.1 の webToken.Value 漏洩検証用）。
func (l *fakeLogger) leaks(secret string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.entries {
		if strings.Contains(e.Msg, secret) {
			return true
		}
		for _, v := range e.Fields {
			if s, ok := v.(string); ok && strings.Contains(s, secret) {
				return true
			}
		}
	}
	return false
}

// ---- Test harness ----

type appServiceHarness struct {
	svc      Service
	repo     *fakeAppRepository
	amapi    *fakeWebTokenClient
	recorder *fakeEventRecorder
	resolver *fakeResolver
	log      *fakeLogger
}

func newAppServiceHarness() *appServiceHarness {
	repo := &fakeAppRepository{}
	client := &fakeWebTokenClient{token: amapi.WebToken{Name: "enterprises/LC0123456789/webTokens/wt1", Value: testWebTokenValue}}
	recorder := &fakeEventRecorder{}
	resolver := &fakeResolver{name: testEnterpriseName}
	log := &fakeLogger{}
	svc := NewService(repo, client, recorder, resolver, log)
	return &appServiceHarness{
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

// ===== CreatePlayToken: 正常発行 / 入力検証 / bind gate / AMAPI 伝達（Req 1.1 / 1.2 / 1.3 / NFR 2.1） =====

func TestService_CreatePlayToken(t *testing.T) {
	t.Run("正常発行のとき CreateWebToken の Value を PlayTokenView で返す", func(t *testing.T) {
		// Arrange
		h := newAppServiceHarness()
		actor, tenantID := uuid.New(), uuid.New()

		// Act
		view, err := h.svc.CreatePlayToken(context.Background(), actor, tenantID, PlayTokenRequest{ParentFrameURL: testParentFrameURL})

		// Assert: Value が一致し、enterprise 解決 → webToken 発行の順で呼ばれる（Req 1.1）。
		if err != nil {
			t.Fatalf("expected success, got %v", err)
		}
		if view.Value != testWebTokenValue {
			t.Errorf("expected view value %q, got %q", testWebTokenValue, view.Value)
		}
		if h.resolver.callCount() != 1 {
			t.Errorf("expected enterprise resolver called once, got %d", h.resolver.callCount())
		}
		call, ok := h.amapi.lastCall()
		if !ok {
			t.Fatal("expected a CreateWebToken call")
		}
		if call.EnterpriseName != testEnterpriseName {
			t.Errorf("expected enterprise name %q passed to CreateWebToken, got %q", testEnterpriseName, call.EnterpriseName)
		}
		if call.ParentFrameURL != testParentFrameURL {
			t.Errorf("expected parent_frame_url %q passed to CreateWebToken, got %q", testParentFrameURL, call.ParentFrameURL)
		}
	})

	t.Run("parent_frame_url が空のとき 400 を返し enterprise 解決も webToken 発行も呼ばない", func(t *testing.T) {
		// Arrange
		h := newAppServiceHarness()
		actor, tenantID := uuid.New(), uuid.New()

		// Act
		_, err := h.svc.CreatePlayToken(context.Background(), actor, tenantID, PlayTokenRequest{ParentFrameURL: ""})

		// Assert: 空入力は 400 で早期 return し、resolver / AMAPI を一切呼ばない（Req 1.2）。
		if got := codeOf(t, err); got != pkgerrors.CodeInvalidRequest {
			t.Fatalf("expected CodeInvalidRequest (400) for empty parent_frame_url, got %s", got)
		}
		if h.resolver.callCount() != 0 {
			t.Errorf("expected enterprise resolver NOT called, got %d", h.resolver.callCount())
		}
		if h.amapi.callCount() != 0 {
			t.Errorf("expected CreateWebToken NOT called, got %d", h.amapi.callCount())
		}
	})

	t.Run("parent_frame_url が空白のみのとき 400 を返す", func(t *testing.T) {
		// Arrange: 境界値（whitespace-only）。
		h := newAppServiceHarness()
		actor, tenantID := uuid.New(), uuid.New()

		// Act
		_, err := h.svc.CreatePlayToken(context.Background(), actor, tenantID, PlayTokenRequest{ParentFrameURL: "   "})

		// Assert（Req 1.2）。
		if got := codeOf(t, err); got != pkgerrors.CodeInvalidRequest {
			t.Fatalf("expected CodeInvalidRequest (400) for whitespace-only parent_frame_url, got %s", got)
		}
		if h.resolver.callCount() != 0 || h.amapi.callCount() != 0 {
			t.Errorf("expected no resolver / AMAPI calls, got resolver=%d amapi=%d", h.resolver.callCount(), h.amapi.callCount())
		}
	})

	t.Run("未バインドテナントのとき resolver の error を伝達し webToken 発行を呼ばない", func(t *testing.T) {
		// Arrange: resolver が未バインド（CodeBusinessRule / 422）を返す。
		h := newAppServiceHarness()
		actor, tenantID := uuid.New(), uuid.New()
		resolveErr := pkgerrors.New(pkgerrors.CodeBusinessRule, "tenant is not bound to an enterprise")
		h.resolver.err = resolveErr

		// Act
		_, err := h.svc.CreatePlayToken(context.Background(), actor, tenantID, PlayTokenRequest{ParentFrameURL: testParentFrameURL})

		// Assert: resolver error がそのまま伝達され、AMAPI は呼ばれない（Req 1.3）。
		if !stderrors.Is(err, resolveErr) {
			t.Errorf("expected resolver error propagated, got %v", err)
		}
		if got := codeOf(t, err); got != pkgerrors.CodeBusinessRule {
			t.Errorf("expected CodeBusinessRule (422) propagated, got %s", got)
		}
		if h.amapi.callCount() != 0 {
			t.Errorf("expected CreateWebToken NOT called when tenant is unbound, got %d", h.amapi.callCount())
		}
	})

	t.Run("AMAPI が error を返すとき正規化済み error をそのまま伝達する", func(t *testing.T) {
		// Arrange: AMAPI が上流エラー（CodeUpstream / 502）を返す（#34 正規化済み）。
		h := newAppServiceHarness()
		actor, tenantID := uuid.New(), uuid.New()
		amapiErr := pkgerrors.New(pkgerrors.CodeUpstream, "amapi web token upstream failure")
		h.amapi.err = amapiErr

		// Act
		_, err := h.svc.CreatePlayToken(context.Background(), actor, tenantID, PlayTokenRequest{ParentFrameURL: testParentFrameURL})

		// Assert: AMAPI 由来 error を再分類せずそのまま伝達する（NFR 2.1）。
		if !stderrors.Is(err, amapiErr) {
			t.Errorf("expected AMAPI error propagated as-is, got %v", err)
		}
		if got := codeOf(t, err); got != pkgerrors.CodeUpstream {
			t.Errorf("expected CodeUpstream (502) propagated, got %s", got)
		}
		if h.amapi.callCount() != 1 {
			t.Errorf("expected CreateWebToken called once, got %d", h.amapi.callCount())
		}
	})
}

// ===== CreatePlayToken: webToken の Value を構造化ログに出さない（NFR 1.1） =====

func TestService_CreatePlayToken_NoTokenLeak(t *testing.T) {
	t.Run("成功時に webToken の Value を構造化ログへ出力しない", func(t *testing.T) {
		// Arrange: 秘匿値を含む webToken を発行する。
		h := newAppServiceHarness()
		actor, tenantID := uuid.New(), uuid.New()

		// Act
		view, err := h.svc.CreatePlayToken(context.Background(), actor, tenantID, PlayTokenRequest{ParentFrameURL: testParentFrameURL})

		// Assert: Value は PlayTokenView にのみ現れ、いかなるログエントリにも surface しない（NFR 1.1）。
		if err != nil {
			t.Fatalf("expected success, got %v", err)
		}
		if view.Value != testWebTokenValue {
			t.Fatalf("expected view value %q, got %q", testWebTokenValue, view.Value)
		}
		if h.log.leaks(testWebTokenValue) {
			t.Errorf("webToken value leaked into structured logs: %+v", h.log.snapshot())
		}
	})
}

// ===== ListApps: カタログ参照の委譲 / row→view 写像 / 空 slice / error 伝達（Req 2.1 / 2.2） =====

func TestService_ListApps(t *testing.T) {
	t.Run("Repository.List の結果を TenantAppView へ写像して返す", func(t *testing.T) {
		// Arrange: 2 件（icon あり / icon NULL の双方で写像を検証）。
		h := newAppServiceHarness()
		tenantID := uuid.New()
		approvedAt := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
		icon := strptr("https://cdn.example.com/icon.png")
		h.repo.listRows = []TenantAppRow{
			{ID: uuid.New(), TenantID: tenantID, PackageName: "com.example.a", Title: "App A", IconURL: icon, ApprovedAt: approvedAt},
			{ID: uuid.New(), TenantID: tenantID, PackageName: "com.example.b", Title: "App B", IconURL: nil, ApprovedAt: approvedAt},
		}

		// Act
		views, err := h.svc.ListApps(context.Background(), tenantID)

		// Assert: package_name / title / icon_url / approved_at が正しく写像される（Req 2.1）。
		if err != nil {
			t.Fatalf("expected success, got %v", err)
		}
		if h.repo.listCallCount() != 1 {
			t.Errorf("expected Repository.List called once, got %d", h.repo.listCallCount())
		}
		if len(views) != 2 {
			t.Fatalf("expected 2 views, got %d", len(views))
		}
		if views[0].PackageName != "com.example.a" || views[0].Title != "App A" {
			t.Errorf("unexpected view[0]: %+v", views[0])
		}
		if views[0].IconURL == nil || *views[0].IconURL != "https://cdn.example.com/icon.png" {
			t.Errorf("expected view[0] icon_url passed through, got %v", views[0].IconURL)
		}
		if !views[0].ApprovedAt.Equal(approvedAt) {
			t.Errorf("expected view[0] approved_at %v, got %v", approvedAt, views[0].ApprovedAt)
		}
		if views[1].IconURL != nil {
			t.Errorf("expected view[1] icon_url to stay null, got %v", *views[1].IconURL)
		}
	})

	t.Run("0 件のとき非 nil の空 slice を返す", func(t *testing.T) {
		// Arrange: 境界値（承認済みアプリ 0 件 / Req 2.2）。
		h := newAppServiceHarness()
		tenantID := uuid.New()
		h.repo.listRows = nil

		// Act
		views, err := h.svc.ListApps(context.Background(), tenantID)

		// Assert: nil ではなく空 slice（Handler が `[]` をシリアライズできる / Req 2.2）。
		if err != nil {
			t.Fatalf("expected success, got %v", err)
		}
		if views == nil {
			t.Fatal("expected non-nil empty slice, got nil")
		}
		if len(views) != 0 {
			t.Errorf("expected 0 views, got %d", len(views))
		}
	})

	t.Run("Repository が error を返すとき伝達する", func(t *testing.T) {
		// Arrange: DB 不通（CodeUnavailable / 503）。
		h := newAppServiceHarness()
		tenantID := uuid.New()
		listErr := pkgerrors.New(pkgerrors.CodeUnavailable, "tenant_apps list query failed")
		h.repo.listErr = listErr

		// Act
		_, err := h.svc.ListApps(context.Background(), tenantID)

		// Assert（Req 2.1 異常系）。
		if !stderrors.Is(err, listErr) {
			t.Errorf("expected list error propagated, got %v", err)
		}
		if got := codeOf(t, err); got != pkgerrors.CodeUnavailable {
			t.Errorf("expected CodeUnavailable (503) propagated, got %s", got)
		}
	})
}

// ===== SyncApps: bind gate / アトミック upsert / 反映件数・同期時刻 / 同期監査（Req 3.1 / 3.3 / 3.4 / 3.5 / 3.6 / NFR 3.1 / 3.2） =====

// syncAppsFixture は正常系で用いる 2 件の SyncApp（icon あり / icon なしの双方で写像経路を通す）。
func syncAppsFixture() []SyncApp {
	return []SyncApp{
		{PackageName: "com.example.a", Title: "App A", IconURL: strptr("https://cdn.example.com/a.png")},
		{PackageName: "com.example.b", Title: "App B", IconURL: nil},
	}
}

func TestService_SyncApps(t *testing.T) {
	t.Run("正常同期のとき反映件数と非 zero の同期時刻を返し Upsert へ委譲する", func(t *testing.T) {
		// Arrange: bound テナント + Repository が 2 件反映を返す。
		h := newAppServiceHarness()
		actor, tenantID := uuid.New(), uuid.New()
		apps := syncAppsFixture()
		h.repo.upsertCount = 2

		// Act
		result, err := h.svc.SyncApps(context.Background(), actor, tenantID, SyncRequest{Apps: apps})

		// Assert: 反映件数を返し（Req 3.1 / 3.3）、SyncedAt は非 zero、Upsert が渡された apps で 1 度呼ばれる。
		if err != nil {
			t.Fatalf("expected success, got %v", err)
		}
		if result.Count != 2 {
			t.Errorf("expected count 2, got %d", result.Count)
		}
		if result.SyncedAt.IsZero() {
			t.Errorf("expected non-zero synced_at, got zero time")
		}
		if h.repo.upsertCallCount() != 1 {
			t.Fatalf("expected Repository.Upsert called once, got %d", h.repo.upsertCallCount())
		}
		if got := h.repo.upsertLastApps(); len(got) != 2 || got[0].PackageName != "com.example.a" {
			t.Errorf("expected apps relayed to Upsert, got %+v", got)
		}
	})

	t.Run("正常同期のとき成功監査（app_sync / result=success / count）を記録する", func(t *testing.T) {
		// Arrange（Req 3.3 / NFR 3.1）。
		h := newAppServiceHarness()
		actor, tenantID := uuid.New(), uuid.New()
		h.repo.upsertCount = 3

		// Act
		_, err := h.svc.SyncApps(context.Background(), actor, tenantID, SyncRequest{Apps: syncAppsFixture()})

		// Assert: 監査が 1 件、event_type=app_sync / result=success / Detail.count=反映件数。
		if err != nil {
			t.Fatalf("expected success, got %v", err)
		}
		if h.recorder.callCount() != 1 {
			t.Fatalf("expected 1 audit record, got %d", h.recorder.callCount())
		}
		ev, _ := h.recorder.last()
		if ev.EventType != "app_sync" {
			t.Errorf("expected event_type app_sync, got %s", ev.EventType)
		}
		if ev.Result != audit.ResultSuccess {
			t.Errorf("expected result success, got %s", ev.Result)
		}
		if ev.TenantID != tenantID || ev.ActorID != actor {
			t.Errorf("expected tenant/actor recorded, got tenant=%s actor=%s", ev.TenantID, ev.ActorID)
		}
		if ev.Detail["count"] != 3 {
			t.Errorf("expected Detail.count=3, got %v", ev.Detail["count"])
		}
		if ev.Detail["result"] != string(audit.ResultSuccess) {
			t.Errorf("expected Detail.result=success, got %v", ev.Detail["result"])
		}
	})

	t.Run("空リスト同期のとき count 0 を正常応答として返し成功監査を記録する", func(t *testing.T) {
		// Arrange: 境界値（承認済みアプリ 0 件 / Req 3.4）。
		h := newAppServiceHarness()
		actor, tenantID := uuid.New(), uuid.New()
		h.repo.upsertCount = 0

		// Act
		result, err := h.svc.SyncApps(context.Background(), actor, tenantID, SyncRequest{Apps: nil})

		// Assert: count 0 の正常応答、SyncedAt 非 zero、成功監査 count 0（Req 3.4 / NFR 3.1）。
		if err != nil {
			t.Fatalf("expected success for empty sync, got %v", err)
		}
		if result.Count != 0 {
			t.Errorf("expected count 0 for empty sync, got %d", result.Count)
		}
		if result.SyncedAt.IsZero() {
			t.Errorf("expected non-zero synced_at, got zero time")
		}
		ev, ok := h.recorder.last()
		if !ok {
			t.Fatal("expected success audit even for empty sync")
		}
		if ev.Result != audit.ResultSuccess || ev.Detail["count"] != 0 {
			t.Errorf("expected success audit with count 0, got result=%s count=%v", ev.Result, ev.Detail["count"])
		}
	})

	t.Run("未バインドテナントのとき Upsert を呼ばず error を伝達し失敗監査を記録する", func(t *testing.T) {
		// Arrange: resolver が未バインド（CodeBusinessRule / 422）を返す（Req 3.5）。
		h := newAppServiceHarness()
		actor, tenantID := uuid.New(), uuid.New()
		bindErr := pkgerrors.New(pkgerrors.CodeBusinessRule, "tenant is not bound to an enterprise")
		h.resolver.err = bindErr

		// Act
		_, err := h.svc.SyncApps(context.Background(), actor, tenantID, SyncRequest{Apps: syncAppsFixture()})

		// Assert: error 伝達（422）、Upsert 未呼び出し、失敗監査 count 0（Req 3.5 / NFR 3.1）。
		if !stderrors.Is(err, bindErr) {
			t.Errorf("expected bind error propagated, got %v", err)
		}
		if got := codeOf(t, err); got != pkgerrors.CodeBusinessRule {
			t.Errorf("expected CodeBusinessRule (422) propagated, got %s", got)
		}
		if h.repo.upsertCallCount() != 0 {
			t.Errorf("expected Repository.Upsert NOT called for unbound tenant, got %d", h.repo.upsertCallCount())
		}
		ev, ok := h.recorder.last()
		if !ok {
			t.Fatal("expected failure audit for unbound tenant")
		}
		if ev.Result != audit.ResultFailure || ev.Detail["count"] != 0 {
			t.Errorf("expected failure audit with count 0, got result=%s count=%v", ev.Result, ev.Detail["count"])
		}
	})

	t.Run("package_name が空のとき 400 を返し Upsert を呼ばず失敗監査を記録する", func(t *testing.T) {
		// Arrange: 境界値（空白のみ package_name）。
		h := newAppServiceHarness()
		actor, tenantID := uuid.New(), uuid.New()
		apps := []SyncApp{{PackageName: "   ", Title: "App A"}}

		// Act
		_, err := h.svc.SyncApps(context.Background(), actor, tenantID, SyncRequest{Apps: apps})

		// Assert: 400、Upsert 未呼び出し、失敗監査（Req 3.1 入力ガード / NFR 3.1）。
		if got := codeOf(t, err); got != pkgerrors.CodeInvalidRequest {
			t.Fatalf("expected CodeInvalidRequest (400) for empty package_name, got %s", got)
		}
		if h.repo.upsertCallCount() != 0 {
			t.Errorf("expected Repository.Upsert NOT called for invalid input, got %d", h.repo.upsertCallCount())
		}
		if ev, ok := h.recorder.last(); !ok || ev.Result != audit.ResultFailure {
			t.Errorf("expected failure audit for invalid input, got %+v (recorded=%t)", ev, ok)
		}
	})

	t.Run("title が空のとき 400 を返し Upsert を呼ばない", func(t *testing.T) {
		// Arrange: 境界値（空 title / tenant_apps.title は NOT NULL）。
		h := newAppServiceHarness()
		actor, tenantID := uuid.New(), uuid.New()
		apps := []SyncApp{{PackageName: "com.example.a", Title: ""}}

		// Act
		_, err := h.svc.SyncApps(context.Background(), actor, tenantID, SyncRequest{Apps: apps})

		// Assert: 400、Upsert 未呼び出し。
		if got := codeOf(t, err); got != pkgerrors.CodeInvalidRequest {
			t.Fatalf("expected CodeInvalidRequest (400) for empty title, got %s", got)
		}
		if h.repo.upsertCallCount() != 0 {
			t.Errorf("expected Repository.Upsert NOT called for empty title, got %d", h.repo.upsertCallCount())
		}
	})

	t.Run("Repository が error を返すとき count 0 で伝達し失敗監査を記録する", func(t *testing.T) {
		// Arrange: DB 不通（CodeUnavailable / 503）で部分反映を残さない（Req 3.6）。
		h := newAppServiceHarness()
		actor, tenantID := uuid.New(), uuid.New()
		upErr := pkgerrors.New(pkgerrors.CodeUnavailable, "tenant_apps upsert failed")
		h.repo.upsertErr = upErr

		// Act
		result, err := h.svc.SyncApps(context.Background(), actor, tenantID, SyncRequest{Apps: syncAppsFixture()})

		// Assert: error 伝達（503）、count 0、失敗監査 count 0（Req 3.6 / NFR 3.1）。
		if !stderrors.Is(err, upErr) {
			t.Errorf("expected upsert error propagated, got %v", err)
		}
		if got := codeOf(t, err); got != pkgerrors.CodeUnavailable {
			t.Errorf("expected CodeUnavailable (503) propagated, got %s", got)
		}
		if result.Count != 0 {
			t.Errorf("expected count 0 on upsert failure, got %d", result.Count)
		}
		ev, ok := h.recorder.last()
		if !ok {
			t.Fatal("expected failure audit on upsert failure")
		}
		if ev.Result != audit.ResultFailure || ev.Detail["count"] != 0 {
			t.Errorf("expected failure audit with count 0, got result=%s count=%v", ev.Result, ev.Detail["count"])
		}
	})

	t.Run("監査 Record が失敗しても同期成功結果を覆さず WARN に留める", func(t *testing.T) {
		// Arrange: Record が error を返すが成功パス（NFR 3.1）。
		h := newAppServiceHarness()
		actor, tenantID := uuid.New(), uuid.New()
		h.repo.upsertCount = 2
		h.recorder.err = pkgerrors.New(pkgerrors.CodeUnavailable, "audit sink unavailable")

		// Act
		result, err := h.svc.SyncApps(context.Background(), actor, tenantID, SyncRequest{Apps: syncAppsFixture()})

		// Assert: ユースケース結果は成功のまま（count 反映）、Record は呼ばれ、WARN ログが出る。
		if err != nil {
			t.Fatalf("expected sync success not overturned by audit failure, got %v", err)
		}
		if result.Count != 2 {
			t.Errorf("expected count 2 despite audit failure, got %d", result.Count)
		}
		if h.recorder.callCount() != 1 {
			t.Errorf("expected Record attempted once, got %d", h.recorder.callCount())
		}
		if !hasWarn(h.log, "app audit record failed") {
			t.Errorf("expected WARN log on audit record failure, got %+v", h.log.snapshot())
		}
	})

	t.Run("同期監査 Detail に enterprise_name 等の機密値を含めない", func(t *testing.T) {
		// Arrange: bound テナント（enterprise_name = testEnterpriseName）で成功同期する（NFR 3.2）。
		h := newAppServiceHarness()
		actor, tenantID := uuid.New(), uuid.New()
		h.repo.upsertCount = 2

		// Act
		_, err := h.svc.SyncApps(context.Background(), actor, tenantID, SyncRequest{Apps: syncAppsFixture()})

		// Assert: Detail の key は安全 field（count / result）のみで、enterprise_name を値に含めない。
		if err != nil {
			t.Fatalf("expected success, got %v", err)
		}
		ev, ok := h.recorder.last()
		if !ok {
			t.Fatal("expected an audit record")
		}
		if len(ev.Detail) != 2 {
			t.Errorf("expected Detail to carry only count/result, got %+v", ev.Detail)
		}
		if _, hasCount := ev.Detail["count"]; !hasCount {
			t.Errorf("expected Detail.count present, got %+v", ev.Detail)
		}
		if _, hasResult := ev.Detail["result"]; !hasResult {
			t.Errorf("expected Detail.result present, got %+v", ev.Detail)
		}
		if h.recorder.detailLeaks(testEnterpriseName) {
			t.Errorf("enterprise_name leaked into audit Detail: %+v", ev.Detail)
		}
	})
}

// ===== CheckAppsApproved: 承認済みアプリ read seam（全承認 / 未承認 / 越境 / 空入力 / DB error）（Req 5.1 / 5.2） =====

func TestService_CheckAppsApproved(t *testing.T) {
	t.Run("全 package が承認済みのとき nil を返し ApprovedPackages へ委譲する", func(t *testing.T) {
		// Arrange: 要求 package が全て自テナントの承認済み集合に含まれる（Req 5.1）。
		h := newAppServiceHarness()
		tenantID := uuid.New()
		packages := []string{"com.example.a", "com.example.b"}
		h.repo.approvedSet = map[string]struct{}{"com.example.a": {}, "com.example.b": {}}

		// Act
		err := h.svc.CheckAppsApproved(context.Background(), tenantID, packages)

		// Assert: nil を返し、Repository へ委譲し、渡した packageNames が捕捉される（Req 5.1）。
		if err != nil {
			t.Fatalf("expected nil for all-approved packages, got %v", err)
		}
		if h.repo.approvedCallCount() != 1 {
			t.Fatalf("expected Repository.ApprovedPackages called once, got %d", h.repo.approvedCallCount())
		}
		got := h.repo.approvedLastPackages()
		if len(got) != 2 || got[0] != "com.example.a" || got[1] != "com.example.b" {
			t.Errorf("expected packageNames relayed to ApprovedPackages, got %+v", got)
		}
	})

	t.Run("1 件が未承認のとき ErrAppNotApproved(422) を返す", func(t *testing.T) {
		// Arrange: 2 件中 1 件のみ承認済み（Req 5.2 異常系）。
		h := newAppServiceHarness()
		tenantID := uuid.New()
		packages := []string{"com.example.a", "com.example.b"}
		h.repo.approvedSet = map[string]struct{}{"com.example.a": {}}

		// Act
		err := h.svc.CheckAppsApproved(context.Background(), tenantID, packages)

		// Assert: sentinel ErrAppNotApproved（Code=CodeBusinessRule / 422）を返す（Req 5.2）。
		if !stderrors.Is(err, ErrAppNotApproved) {
			t.Errorf("expected ErrAppNotApproved for unapproved package, got %v", err)
		}
		if got := codeOf(t, err); got != pkgerrors.CodeBusinessRule {
			t.Errorf("expected CodeBusinessRule (422), got %s", got)
		}
	})

	t.Run("越境 package（自テナント承認済み集合に含まれない）のとき未承認扱いで拒否する", func(t *testing.T) {
		// Arrange: 越境 package は tenant-scoped Repository の承認済み集合に現れない（RLS / Req 5.2 / Req 4.x）。
		h := newAppServiceHarness()
		tenantID := uuid.New()
		packages := []string{"com.other-tenant.app"}
		h.repo.approvedSet = map[string]struct{}{} // 他テナント承認分は自テナント集合に含まれない

		// Act
		err := h.svc.CheckAppsApproved(context.Background(), tenantID, packages)

		// Assert: 越境 package は未承認扱いで ErrAppNotApproved（422）。存在有無を露出しない（Req 4.2）。
		if !stderrors.Is(err, ErrAppNotApproved) {
			t.Errorf("expected cross-tenant package treated as unapproved, got %v", err)
		}
		if got := codeOf(t, err); got != pkgerrors.CodeBusinessRule {
			t.Errorf("expected CodeBusinessRule (422), got %s", got)
		}
	})

	t.Run("空入力のとき nil を返し ApprovedPackages を呼ばない", func(t *testing.T) {
		// Arrange: 境界値（nil / 空 slice は検証対象なし）。
		cases := []struct {
			name     string
			packages []string
		}{
			{name: "nil", packages: nil},
			{name: "空 slice", packages: []string{}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				h := newAppServiceHarness()
				tenantID := uuid.New()

				// Act
				err := h.svc.CheckAppsApproved(context.Background(), tenantID, tc.packages)

				// Assert: nil を返し、Repository を一切呼ばない（不要な DB 往復を避ける）。
				if err != nil {
					t.Fatalf("expected nil for empty input, got %v", err)
				}
				if h.repo.approvedCallCount() != 0 {
					t.Errorf("expected Repository.ApprovedPackages NOT called for empty input, got %d", h.repo.approvedCallCount())
				}
			})
		}
	})

	t.Run("Repository が error を返すとき ErrAppNotApproved へ握り潰さず伝達する", func(t *testing.T) {
		// Arrange: DB 不通（CodeUnavailable / 503）。承認判定不能を 422 に化けさせない（Req 5.1 異常系）。
		h := newAppServiceHarness()
		tenantID := uuid.New()
		approvedErr := pkgerrors.New(pkgerrors.CodeUnavailable, "tenant_apps approved query failed")
		h.repo.approvedErr = approvedErr

		// Act
		err := h.svc.CheckAppsApproved(context.Background(), tenantID, []string{"com.example.a"})

		// Assert: Repository error をそのまま伝達し 503 を保持する。
		if !stderrors.Is(err, approvedErr) {
			t.Errorf("expected repository error propagated, got %v", err)
		}
		if got := codeOf(t, err); got != pkgerrors.CodeUnavailable {
			t.Errorf("expected CodeUnavailable (503) propagated, not swallowed to 422, got %s", got)
		}
	})
}

// hasWarn は fakeLogger の捕捉ログに指定 msg の WARN エントリが存在するかを返す。
func hasWarn(l *fakeLogger, msg string) bool {
	for _, e := range l.snapshot() {
		if e.Level == "warn" && e.Msg == msg {
			return true
		}
	}
	return false
}
