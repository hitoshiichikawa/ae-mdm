package main

import (
	"context"
	"reflect"
	"testing"

	goidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/hitoshiichikawa/ae-mdm/internal/audit"
	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/oidc"
	"github.com/hitoshiichikawa/ae-mdm/internal/tenant"
	"github.com/hitoshiichikawa/ae-mdm/internal/tenantaudit"
)

// TestHealthcheckURL_RespectsHTTPListenAddr は HTTP_LISTEN_ADDR env からの port 抽出と
// loopback URL 構築の挙動を確認する（PR #31 round-1 / round-2 / round-3 review 由来）。
//
// listen addr が 0.0.0.0 / :port / 完全な host:port のいずれでも、healthcheck は
// 127.0.0.1:<port>/healthz を叩く（コンテナ内 healthcheck の前提）。
func TestHealthcheckURL_RespectsHTTPListenAddr(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{name: "default (空文字)", input: "", want: "http://127.0.0.1:8080/healthz"},
		{name: ":9090 → port 上書き", input: ":9090", want: "http://127.0.0.1:9090/healthz"},
		{name: "0.0.0.0:8080", input: "0.0.0.0:8080", want: "http://127.0.0.1:8080/healthz"},
		{name: "127.0.0.1:9000", input: "127.0.0.1:9000", want: "http://127.0.0.1:9000/healthz"},
		{name: "解析不能 → fallback", input: "garbage", want: "http://127.0.0.1:8080/healthz"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			got := healthcheckURL(c.input)
			if got != c.want {
				t.Errorf("healthcheckURL(%q) = %q; want %q", c.input, got, c.want)
			}
		})
	}
}

// fakeVerifier は task 6.3 の buildOAuth2Configs を独立に検証するための oidc.Verifier 実装。
// tenant / admin 別の AuthStyleInHeader 固定 Endpoint を返し、VerifyIDToken は使わない。
type fakeVerifier struct {
	tenantEndpoint oauth2.Endpoint
	adminEndpoint  oauth2.Endpoint
}

func (f *fakeVerifier) VerifyIDToken(_ context.Context, _ string) (oidc.Claims, error) {
	return oidc.Claims{}, nil
}
func (f *fakeVerifier) TenantEndpoint() oauth2.Endpoint { return f.tenantEndpoint }
func (f *fakeVerifier) AdminEndpoint() oauth2.Endpoint  { return f.adminEndpoint }

// TestBuildOAuth2Configs は task 6.3 で追加した buildOAuth2Configs helper が、
// tenant / admin それぞれの ClientID / ClientSecret / RedirectURL / Scopes / Endpoint を
// Config と Verifier から決定論的に取り出すことを回帰的に守る。
//
// 検証観点（tasks.md 6.3 詳細項目 / impl-notes Task 5.1 の Scopes 必須要件と整合）:
//   - 戻り map は ConsoleTenant / ConsoleAdmin の 2 key のみを持つ
//   - Scopes に goidc.ScopeOpenID（"openid"）/ "email" / "profile" を含む
//     （openid 不在だと IdP が id_token を発行せず、後段 token.Extra で 502 に化ける）
//   - 各 oauth2.Config の Endpoint は Verifier の TenantEndpoint() / AdminEndpoint() を
//     そのまま採用（AuthStyle == AuthStyleInHeader / client_secret_basic 固定の契約は
//     verifier_test.go の TestVerifier_EndpointAuthStyle_IsInHeader が一次的に守るので、
//     本 test では Endpoint が「verifier から取り出した値」と byte-equal で一致することのみ
//     assert する）
func TestBuildOAuth2Configs(t *testing.T) {
	tenantEndpoint := oauth2.Endpoint{
		AuthURL:   "https://idp.tenant.example/auth",
		TokenURL:  "https://idp.tenant.example/token",
		AuthStyle: oauth2.AuthStyleInHeader,
	}
	adminEndpoint := oauth2.Endpoint{
		AuthURL:   "https://idp.admin.example/auth",
		TokenURL:  "https://idp.admin.example/token",
		AuthStyle: oauth2.AuthStyleInHeader,
	}
	v := &fakeVerifier{tenantEndpoint: tenantEndpoint, adminEndpoint: adminEndpoint}

	cfg := config.Config{
		OIDCTenantClientID:     "tenant-client-id",
		OIDCTenantClientSecret: "tenant-client-secret",
		OIDCTenantRedirectURL:  "http://localhost:8080/api/auth/callback",
		OIDCAdminClientID:      "admin-client-id",
		OIDCAdminClientSecret:  "admin-client-secret",
		OIDCAdminRedirectURL:   "http://localhost:8080/api/admin/auth/callback",
	}

	got := buildOAuth2Configs(cfg, v)

	if len(got) != 2 {
		t.Fatalf("buildOAuth2Configs: got %d entries; want 2", len(got))
	}

	wantScopes := []string{goidc.ScopeOpenID, "email", "profile"}

	tenant, ok := got[oidc.ConsoleTenant]
	if !ok {
		t.Fatalf("buildOAuth2Configs: missing tenant entry")
	}
	if tenant.ClientID != cfg.OIDCTenantClientID {
		t.Errorf("tenant.ClientID = %q; want %q", tenant.ClientID, cfg.OIDCTenantClientID)
	}
	if tenant.ClientSecret != cfg.OIDCTenantClientSecret {
		t.Errorf("tenant.ClientSecret mismatch")
	}
	if tenant.RedirectURL != cfg.OIDCTenantRedirectURL {
		t.Errorf("tenant.RedirectURL = %q; want %q", tenant.RedirectURL, cfg.OIDCTenantRedirectURL)
	}
	if !reflect.DeepEqual(tenant.Scopes, wantScopes) {
		t.Errorf("tenant.Scopes = %v; want %v", tenant.Scopes, wantScopes)
	}
	if tenant.Endpoint != tenantEndpoint {
		t.Errorf("tenant.Endpoint = %+v; want %+v", tenant.Endpoint, tenantEndpoint)
	}

	admin, ok := got[oidc.ConsoleAdmin]
	if !ok {
		t.Fatalf("buildOAuth2Configs: missing admin entry")
	}
	if admin.ClientID != cfg.OIDCAdminClientID {
		t.Errorf("admin.ClientID = %q; want %q", admin.ClientID, cfg.OIDCAdminClientID)
	}
	if admin.ClientSecret != cfg.OIDCAdminClientSecret {
		t.Errorf("admin.ClientSecret mismatch")
	}
	if admin.RedirectURL != cfg.OIDCAdminRedirectURL {
		t.Errorf("admin.RedirectURL = %q; want %q", admin.RedirectURL, cfg.OIDCAdminRedirectURL)
	}
	if !reflect.DeepEqual(admin.Scopes, wantScopes) {
		t.Errorf("admin.Scopes = %v; want %v", admin.Scopes, wantScopes)
	}
	if admin.Endpoint != adminEndpoint {
		t.Errorf("admin.Endpoint = %+v; want %+v", admin.Endpoint, adminEndpoint)
	}
}

// TestBuildTenantRecorder_WiresAuditServiceAdapterNotInterimLogger は、main の本番 DI が tenant
// ドメインの監査記録ポートに **監査ログ Service へ委譲する tenantaudit アダプタ**を配線し、interim の
// 構造化ログ記録器（tenant.LoggerRecorder）へ退行していないことを型レベルに回帰検知する
// （#54 Req 1.4 / 5.3）。
//
// PR #56 round-5 review 指摘（#2）への対応: 既存のアダプタ単体テスト（internal/tenantaudit/*_test.go）は
// `tenantaudit.NewRecorder` を直接生成して写像・委譲契約を検証するが、main wiring が誤って
// `tenant.NewLoggerRecorder` へ戻っても失敗しない。main が実際に呼ぶ buildTenantRecorder の戻り値型を
// assert することで、本番配線の退行（interim logger への巻き戻し）を捕捉する。
func TestBuildTenantRecorder_WiresAuditServiceAdapterNotInterimLogger(t *testing.T) {
	// Arrange: 本番 DI（main.go runBootstrap）と同じ引数で構築する。
	// repo は本 test で Record/List を呼ばないため nil で足り、NewService は依存を保持するだけ。
	auditSvc := audit.NewService(config.Config{}, nil, audit.SystemClock{}, nil)

	// Act: main が実際に呼ぶ wiring helper を通して recorder を構築する。
	rec := buildTenantRecorder(auditSvc, nil)

	// Assert: 監査ログ Service へ委譲する tenantaudit アダプタであること（interim logger でないこと）。
	if _, ok := rec.(*tenantaudit.Recorder); !ok {
		t.Fatalf("buildTenantRecorder の戻り値型 = %T, want *tenantaudit.Recorder（"+
			"interim tenant.LoggerRecorder へ退行した疑い / #54 Req 1.4 / 5.3）", rec)
	}
	// interim の構造化ログ記録器へ巻き戻っていないことを明示的に否定する。
	if _, isLogger := rec.(*tenant.LoggerRecorder); isLogger {
		t.Fatalf("buildTenantRecorder が interim tenant.LoggerRecorder を返した（" +
			"監査ログ Service への配線が退行 / #54 Req 1.4 / 5.3）")
	}
}
