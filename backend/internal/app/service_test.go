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

// ---- Fake Repository（カタログ参照の委譲を検証 / Req 2.1。Upsert / ApprovedPackages は task 2 では未使用） ----

type fakeAppRepository struct {
	mu        sync.Mutex
	listCalls int

	listRows []TenantAppRow
	listErr  error
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

// Upsert / ApprovedPackages は Repository interface を満たすためのスタブ（task 3 / 4 で検証する）。
func (r *fakeAppRepository) Upsert(_ context.Context, _ uuid.UUID, _ []SyncApp) (int, error) {
	return 0, nil
}

func (r *fakeAppRepository) ApprovedPackages(_ context.Context, _ uuid.UUID, _ []string) (map[string]struct{}, error) {
	return make(map[string]struct{}), nil
}

func (r *fakeAppRepository) listCallCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.listCalls
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
	resolver *fakeResolver
	log      *fakeLogger
}

func newAppServiceHarness() *appServiceHarness {
	repo := &fakeAppRepository{}
	client := &fakeWebTokenClient{token: amapi.WebToken{Name: "enterprises/LC0123456789/webTokens/wt1", Value: testWebTokenValue}}
	resolver := &fakeResolver{name: testEnterpriseName}
	log := &fakeLogger{}
	svc := NewService(repo, client, resolver, log)
	return &appServiceHarness{
		svc:      svc,
		repo:     repo,
		amapi:    client,
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
