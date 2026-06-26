package oidc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	coreoidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// Console は OIDC ID トークンの aud から判別したコンソール種別。
//
// requirements.md Req 1.3 / 6.1 / 6.4 / design.md「OIDC Verifier」節と整合。
type Console string

const (
	// ConsoleTenant はテナント管理者向け（tenant-console）の OIDC client。
	ConsoleTenant Console = "tenant-console"
	// ConsoleAdmin は SaaS 運用者向け（admin-console）の OIDC client。
	ConsoleAdmin Console = "admin-console"
)

// Claims は ID トークンから抽出する検証済みクレーム。
//
// raw JWT は含めない（requirements.md Req 1.11 / NFR 4.2）。auth.Service は本 struct を
// 受け取って `Claims.MatchedConsole` と handler の expected console / `Claims.Nonce` と
// `StatePayload.OIDCNonce` を照合する責務を負う。
type Claims struct {
	// Subject は OIDC `sub` クレーム。issuer スコープで一意なので、auth.Repository での
	// admin_users 解決には Issuer とセットで使う（design.md「Auth Repository」節）。
	Subject string
	// Email は `email` クレーム。OPTIONAL（IdP 側で未提供なら空文字）。
	Email string
	// Groups は `groups` クレーム。RBAC 解釈は後続 Issue で実装する。
	Groups []string
	// Issuer は `iss` クレーム。検証時に Verifier の expected issuer と一致確認済み。
	Issuer string
	// MatchedConsole は aud 検証で確定したコンソール種別（必ず 1 つ）。
	MatchedConsole Console
	// Nonce は OIDC `nonce` クレーム（OPTIONAL）。Verifier 自身は nonce 一致確認を行わず、
	// 値が来ていればそのまま populate する。一致確認は Service 層の責務
	// （RFC OIDC Core 1.0 §3.1.2.7 / design.md「OIDC nonce binding」節）。
	Nonce string
}

// Verifier は OIDC ID トークン検証の単一エントリポイント。
//
// 実装は goroutine-safe（construction 後の状態は読み取り専用）。
type Verifier interface {
	// VerifyIDToken は raw ID トークンを検証し、Claims を返す。
	// 検証失敗時は *errors.Error を返す（Code は CodeUnauthenticated、failure_kind は
	// Cause チェーンに含める）。raw JWT は戻り値・error にも含めない。
	VerifyIDToken(ctx context.Context, rawIDToken string) (Claims, error)
	// TenantEndpoint は tenant-console 用 oauth2.Endpoint を返す。
	// 戻り値の AuthStyle は AuthStyleInHeader に明示上書き済み（client_secret_basic 固定）。
	TenantEndpoint() oauth2.Endpoint
	// AdminEndpoint は admin-console 用 oauth2.Endpoint を返す。
	// 戻り値の AuthStyle は AuthStyleInHeader に明示上書き済み（client_secret_basic 固定）。
	AdminEndpoint() oauth2.Endpoint
}

// failureKind は構造化ログの failure_kind field 値を表す sentinel error 型。
//
// Cause チェーンに含めることで Service / Handler が `errors.As` で識別し、Logger.Warn の
// 構造化 field 値として surface する（NFR 4.1 / design.md「failure_kind ログフィールド一覧」）。
type failureKind string

func (f failureKind) Error() string { return string(f) }

const (
	// FailureKindInvalidSig は ID トークンの署名検証失敗（Req 1.7）。
	FailureKindInvalidSig failureKind = "invalid_sig"
	// FailureKindInvalidIss は iss クレームが期待値と不一致（Req 1.8）。
	FailureKindInvalidIss failureKind = "invalid_iss"
	// FailureKindInvalidAud は aud クレームが tenant / admin のいずれにも一致しない（Req 1.4）。
	FailureKindInvalidAud failureKind = "invalid_aud"
	// FailureKindAudAmbiguous は aud に tenant / admin の両方を含む（Req 1.5）。
	FailureKindAudAmbiguous failureKind = "aud_ambiguous"
	// FailureKindTokenExpired は exp クレームが検証時刻以前（Req 1.6）。
	FailureKindTokenExpired failureKind = "token_expired"
	// FailureKindInvalidKid は対応する公開鍵が JWKS に無い（Req 1.10）。
	FailureKindInvalidKid failureKind = "invalid_kid"
	// FailureKindOIDCDiscovery は起動時 discovery / JWKS fetch 失敗（NFR 3.2）。
	FailureKindOIDCDiscovery failureKind = "oidc_discovery"
)

// verifierImpl は Verifier interface の実装。
//
// tenant-console / admin-console 各々の Provider + IDTokenVerifier を保持し、
// 構築後は読み取り専用（goroutine-safe）。
type verifierImpl struct {
	tenant consoleVerifier
	admin  consoleVerifier
}

// consoleVerifier は 1 console 分の OIDC verifier 構成を束ねる。
type consoleVerifier struct {
	console  Console
	issuer   string
	clientID string
	endpoint oauth2.Endpoint
	verifier *coreoidc.IDTokenVerifier
}

// NewVerifier は config から tenant-console / admin-console 双方の Verifier を構築する。
//
// 起動時に両 issuer の OIDC discovery（`.well-known/openid-configuration`）を実行し、
// 失敗時は `*errors.Error{Code: CodeUnavailable}` を返す（NFR 3.2 fail-closed bootstrap）。
//
// 構築された Verifier は goroutine-safe。返却後の状態は読み取り専用で、tenant / admin の
// Provider / IDTokenVerifier / oauth2.Endpoint をそれぞれ保持する。
func NewVerifier(ctx context.Context, cfg config.Config) (Verifier, error) {
	tenant, err := buildConsoleVerifier(ctx, ConsoleTenant, cfg.OIDCTenantIssuerURL, cfg.OIDCTenantClientID)
	if err != nil {
		return nil, err
	}
	admin, err := buildConsoleVerifier(ctx, ConsoleAdmin, cfg.OIDCAdminIssuerURL, cfg.OIDCAdminClientID)
	if err != nil {
		return nil, err
	}
	return &verifierImpl{tenant: tenant, admin: admin}, nil
}

// buildConsoleVerifier は 1 console 分の Provider + IDTokenVerifier + oauth2.Endpoint を
// 構築する。go-oidc 内蔵 aud 検証は SkipClientIDCheck=true で明示的に無効化し、aud は
// 本 package の VerifyIDToken で排他一致検証を行う。
//
// discovery（`coreoidc.NewProvider`）に加え、jwks_uri の prefetch（HTTP GET + 最低限の
// JSON shape 検証）も bootstrap で実施する。jwks_uri が unreachable / 不正形式の場合は
// 起動失敗として `*errors.Error{Code: CodeUnavailable, failure_kind: oidc_discovery}` を
// 返す（NFR 3.2 fail-closed bootstrap / tasks.md L228-230 / design.md L295-296）。
// 内蔵 RemoteKeySet は遅延 fetch なので初回 callback まで JWKS 異常が検知されない。本 prefetch
// が二次の起動ゲートとなる。
func buildConsoleVerifier(ctx context.Context, console Console, issuerURL, clientID string) (consoleVerifier, error) {
	provider, err := coreoidc.NewProvider(ctx, issuerURL)
	if err != nil {
		// NFR 3.2 / design.md「OIDC Verifier」 invariants。
		// 機密値（client_secret 等）は wrap message に含めない（design.md NFR 1.1）。
		return consoleVerifier{}, pkgerrors.Wrap(
			pkgerrors.CodeUnavailable,
			fmt.Sprintf("oidc discovery failed (console=%s, issuer=%s)", console, issuerURL),
			joinFailureKind(err, FailureKindOIDCDiscovery),
		)
	}
	if err := prefetchJWKS(ctx, provider, console, issuerURL); err != nil {
		return consoleVerifier{}, err
	}
	// ClientID="" + SkipClientIDCheck=true で go-oidc 内蔵 aud 検証を切る。
	// 両方明示しないと go-oidc v3 が "invalid configuration" で reject する。
	idtv := provider.Verifier(&coreoidc.Config{
		ClientID:          "",
		SkipClientIDCheck: true,
	})
	endpoint := provider.Endpoint()
	// AuthStyle を InHeader に明示上書き（client_secret_basic 固定。
	// AuthStyleAutoDetect の非決定的フォールバックを避ける / design.md Technology Stack
	// 「Authentication」行 + 確認事項 6 と整合）。
	endpoint.AuthStyle = oauth2.AuthStyleInHeader
	return consoleVerifier{
		console:  console,
		issuer:   issuerURL,
		clientID: clientID,
		endpoint: endpoint,
		verifier: idtv,
	}, nil
}

// prefetchJWKS は discovery document から `jwks_uri` を抽出し、HTTP GET で取得して
// 最低限の JSON shape（`keys` 配列が存在する）を検証する。
//
// 失敗時は `*errors.Error{Code: CodeUnavailable, failure_kind: oidc_discovery}` を返し、
// 起動失敗として上位に伝搬する。レスポンスボディはサイズ上限 1 MiB で読み切る
// （故障した IdP が巨大レスポンスを返すケースの defense）。
func prefetchJWKS(ctx context.Context, provider *coreoidc.Provider, console Console, issuerURL string) error {
	var discovery struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := provider.Claims(&discovery); err != nil {
		return pkgerrors.Wrap(
			pkgerrors.CodeUnavailable,
			fmt.Sprintf("oidc discovery jwks_uri extract failed (console=%s, issuer=%s)", console, issuerURL),
			joinFailureKind(err, FailureKindOIDCDiscovery),
		)
	}
	if discovery.JWKSURI == "" {
		return pkgerrors.Wrap(
			pkgerrors.CodeUnavailable,
			fmt.Sprintf("oidc discovery jwks_uri missing (console=%s, issuer=%s)", console, issuerURL),
			FailureKindOIDCDiscovery,
		)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discovery.JWKSURI, nil)
	if err != nil {
		return pkgerrors.Wrap(
			pkgerrors.CodeUnavailable,
			fmt.Sprintf("oidc jwks request build failed (console=%s, jwks_uri=%s)", console, discovery.JWKSURI),
			joinFailureKind(err, FailureKindOIDCDiscovery),
		)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return pkgerrors.Wrap(
			pkgerrors.CodeUnavailable,
			fmt.Sprintf("oidc jwks fetch failed (console=%s, jwks_uri=%s)", console, discovery.JWKSURI),
			joinFailureKind(err, FailureKindOIDCDiscovery),
		)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return pkgerrors.Wrap(
			pkgerrors.CodeUnavailable,
			fmt.Sprintf("oidc jwks fetch non-2xx (console=%s, jwks_uri=%s, status=%d)", console, discovery.JWKSURI, resp.StatusCode),
			FailureKindOIDCDiscovery,
		)
	}
	// 1 MiB 上限で読み切る。本物の JWKS は通常 < 10 KB。
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return pkgerrors.Wrap(
			pkgerrors.CodeUnavailable,
			fmt.Sprintf("oidc jwks read failed (console=%s, jwks_uri=%s)", console, discovery.JWKSURI),
			joinFailureKind(err, FailureKindOIDCDiscovery),
		)
	}
	var jwks struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := json.Unmarshal(body, &jwks); err != nil {
		return pkgerrors.Wrap(
			pkgerrors.CodeUnavailable,
			fmt.Sprintf("oidc jwks parse failed (console=%s, jwks_uri=%s)", console, discovery.JWKSURI),
			joinFailureKind(err, FailureKindOIDCDiscovery),
		)
	}
	if len(jwks.Keys) == 0 {
		return pkgerrors.Wrap(
			pkgerrors.CodeUnavailable,
			fmt.Sprintf("oidc jwks empty keys array (console=%s, jwks_uri=%s)", console, discovery.JWKSURI),
			FailureKindOIDCDiscovery,
		)
	}
	return nil
}

// VerifyIDToken は raw ID トークンを検証し、Claims を返す。
//
//  1. coreoidc が iss / 署名 / exp を検証（aud は SkipClientIDCheck=true で本 package が
//     後段で検証）
//  2. aud 配列を本 package が判定: tenant / admin のいずれか **排他一致** を強制
//  3. iss クレームに合致する console（tenant or admin）を選んで検証する
//
// 検証失敗時は `*errors.Error{Code: CodeUnauthenticated}` を返し、Cause チェーンに
// failure_kind sentinel error を含める。raw JWT は戻り値・error にも一切含めない
// （Req 1.11 / NFR 1.1）。
func (v *verifierImpl) VerifyIDToken(ctx context.Context, rawIDToken string) (Claims, error) {
	// tenant / admin で issuer URL が同一になる構成（同一 IdP 内の 2 client、
	// 例: 1 Keycloak realm 内に tenant-console / admin-console の 2 client を登録）と、
	// 別 issuer URL になる構成の両方をサポートする必要がある。
	//
	// 戦略: 両 verifier の Verify を順に試し、最初に成功した方の IDToken を採用する。
	// aud の判定は finalize 側で client_id 排他一致として行うため、どちらの verifier が
	// 成功したかは「sig / iss / exp が通った」ことを示すだけで、matched console の確定には
	// 関与しない（finalize 内で aud から console を決定する）。
	tenantTok, tenantErr := v.tenant.verifier.Verify(ctx, rawIDToken)
	if tenantErr == nil {
		return v.finalize(tenantTok)
	}
	adminTok, adminErr := v.admin.verifier.Verify(ctx, rawIDToken)
	if adminErr == nil {
		return v.finalize(adminTok)
	}
	// 両方失敗 → 失敗種別を分類して返す。
	return Claims{}, classifyVerifyError(tenantErr, adminErr, v.tenant.issuer, v.admin.issuer)
}

// finalize は coreoidc.IDToken が手に入った段階で aud 排他一致を検証し、Claims を返す。
//
// tenant / admin の client_id のうち aud 配列に含まれる数を数え、排他的に 1 つだけ
// 含まれる場合のみ MatchedConsole を確定する（Req 1.4 / 1.5）。
func (v *verifierImpl) finalize(tok *coreoidc.IDToken) (Claims, error) {
	// aud 排他一致: tenant / admin の 2 値のうち、token の Audience に含まれる数を数える。
	hasTenant := containsString(tok.Audience, v.tenant.clientID)
	hasAdmin := containsString(tok.Audience, v.admin.clientID)
	var matched Console
	switch {
	case hasTenant && hasAdmin:
		return Claims{}, unauthenticated(FailureKindAudAmbiguous,
			"id token aud contains both tenant and admin client ids")
	case !hasTenant && !hasAdmin:
		return Claims{}, unauthenticated(FailureKindInvalidAud,
			"id token aud does not match any console")
	case hasTenant:
		matched = ConsoleTenant
	case hasAdmin:
		matched = ConsoleAdmin
	}

	// 追加 claims を extract（email / groups）。Verify が一度 unmarshal している
	// payload を再 unmarshal するため Claims() を呼ぶ。
	var extra struct {
		Email  string   `json:"email"`
		Groups []string `json:"groups"`
	}
	if err := tok.Claims(&extra); err != nil {
		// claims unmarshal 失敗は実装側 invariant 違反に近いが fail-closed で 401。
		// raw JWT を error に含めない（NFR 4.2）。
		return Claims{}, unauthenticated(FailureKindInvalidSig,
			fmt.Sprintf("oidc claims extract failed: %v", err))
	}

	return Claims{
		Subject:        tok.Subject,
		Email:          extra.Email,
		Groups:         extra.Groups,
		Issuer:         tok.Issuer,
		MatchedConsole: matched,
		Nonce:          tok.Nonce,
	}, nil
}

// classifyVerifyError は coreoidc.Verify の error を failure_kind に分類する。
//
// 両 console で失敗したケース（tenantErr != nil && adminErr != nil）を扱う前提。
// 優先度: token_expired > invalid_sig (kid 不在を含む) > invalid_iss。
func classifyVerifyError(tenantErr, adminErr error, tenantIssuer, adminIssuer string) error {
	// 1. exp 切れは coreoidc が *TokenExpiredError を返す → token_expired。
	var expiredErr *coreoidc.TokenExpiredError
	if errors.As(tenantErr, &expiredErr) || errors.As(adminErr, &expiredErr) {
		return unauthenticated(FailureKindTokenExpired,
			"id token expired")
	}
	// 2. iss 不一致は coreoidc.Verify が `oidc: id token issued by a different provider` を
	//    返す（プレフィックス一致で判定）。
	if isIssuerMismatch(tenantErr) && isIssuerMismatch(adminErr) {
		return unauthenticated(FailureKindInvalidIss,
			fmt.Sprintf("id token iss does not match any expected issuer (tenant=%s, admin=%s)",
				tenantIssuer, adminIssuer))
	}
	// 3. 署名検証失敗 / kid 不在は `failed to verify signature` プレフィックスで返る。
	//    kid 不在は JWKS が「matching key なし」を返した場合に同じ経路に乗る。
	//    どちらか片方が signature 失敗で、もう片方が iss 不一致のケース（issuer が片方しか
	//    マッチしないが対応する keyset に kid が無い等）も signature 失敗扱いとする。
	if isSignatureFailure(tenantErr) || isSignatureFailure(adminErr) {
		// kid 不在の判定は signature failure 経路の中で error message からは分離が難しい。
		// go-jose が `square/go-jose: error in cryptographic primitive` 等を返す。
		// invalid_kid を明示するため、kid 不在エラー文言（"unable to find matching key" /
		// "no matching key" 等）が含まれていれば invalid_kid に分類する。
		if isKidNotFound(tenantErr) || isKidNotFound(adminErr) {
			return unauthenticated(FailureKindInvalidKid,
				"id token kid does not match any key in JWKS")
		}
		return unauthenticated(FailureKindInvalidSig,
			"id token signature verification failed")
	}
	// 4. それ以外 → fail-closed で invalid_sig 扱い。
	return unauthenticated(FailureKindInvalidSig,
		"id token verification failed")
}

// TenantEndpoint は tenant-console 用 oauth2.Endpoint を返す。
func (v *verifierImpl) TenantEndpoint() oauth2.Endpoint { return v.tenant.endpoint }

// AdminEndpoint は admin-console 用 oauth2.Endpoint を返す。
func (v *verifierImpl) AdminEndpoint() oauth2.Endpoint { return v.admin.endpoint }

// unauthenticated は failure_kind を Cause に含む CodeUnauthenticated エラーを構築する。
//
// raw JWT / client secret / MAC 鍵を含めないよう、呼び出し側で message を組み立てる責務を持つ。
func unauthenticated(kind failureKind, message string) *pkgerrors.Error {
	return pkgerrors.Wrap(pkgerrors.CodeUnauthenticated, message, kind)
}

// joinFailureKind は wrap 元 error の上に failure_kind sentinel を貼り付けた error を返す。
// errors.Is(err, FailureKindXxx) で識別できるようにする。
func joinFailureKind(err error, kind failureKind) error {
	if err == nil {
		return kind
	}
	return fmt.Errorf("%w: %w", kind, err)
}

// containsString は slice に target が含まれるかを返す。
func containsString(s []string, target string) bool {
	for _, v := range s {
		if v == target {
			return true
		}
	}
	return false
}

// isIssuerMismatch は coreoidc.Verify が iss 不一致で返した error を判定する。
//
// coreoidc の verify.go は `oidc: id token issued by a different provider` を返す。
func isIssuerMismatch(err error) bool {
	if err == nil {
		return false
	}
	return containsSubstring(err.Error(), "issued by a different provider")
}

// isSignatureFailure は coreoidc.Verify が署名検証で返した error を判定する。
func isSignatureFailure(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return containsSubstring(msg, "failed to verify signature") ||
		containsSubstring(msg, "malformed jwt") ||
		containsSubstring(msg, "id token not signed")
}

// isKidNotFound は go-oidc / go-jose が「matching key なし」を返した error を判定する。
//
// go-oidc/v3 RemoteKeySet が JWKS 上に該当 kid を見つけられない場合、エラーメッセージに
// "failed to verify id token signature" / `no keys matches` / `unable to find matching key`
// 等が含まれる。本判定は文字列マッチで「kid 不在」を best-effort に分類する。
func isKidNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return containsSubstring(msg, "no keys matches") ||
		containsSubstring(msg, "unable to find matching key") ||
		containsSubstring(msg, "no matching key")
}

// containsSubstring は strings.Contains の薄いラッパ（依存方向を errors / config / logger
// 以外に広げないため自前で持つ）。
func containsSubstring(s, substr string) bool {
	if substr == "" {
		return true
	}
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
