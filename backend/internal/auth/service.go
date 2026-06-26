package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	stderrors "errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"

	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/oidc"
)

// Service は認証ユースケースの集約 interface。
//
// design.md「Auth Service」節および tasks.md task 5.1 と整合。HTTP / cookie I/O は Handler、
// tx / DB I/O は Repository、ID トークン検証は Verifier に委譲し、本 interface はそれらを
// 組み合わせる純粋なドメインロジック（state cookie 発行・検証 / nonce 照合 / セッション
// 失効判定 / logout）に専念する。
type Service interface {
	// BeginLogin は OIDC 認可リクエスト URL と state cookie 属性を構築する。
	//
	// 戻り値:
	//   - redirectURL: IdP 認可エンドポイント URL（state / nonce パラメータ込み）
	//   - stateCookie: Value 設定済みの http.Cookie（呼び出し側がそのまま Set-Cookie する）
	//   - err: returnTo open redirect 候補（400） / CSPRNG 失敗（500）
	BeginLogin(ctx context.Context, console oidc.Console, returnTo string) (redirectURL string, stateCookie http.Cookie, err error)

	// HandleCallback は OIDC callback リクエストを処理し、新規セッションを発行する。
	//
	// 戻り値:
	//   - rawSessionToken: cookie value に乗せる opaque session token（生値）
	//   - sessionCookie: Value 設定済みの http.Cookie（呼び出し側がそのまま Set-Cookie する）
	//   - returnTo: callback 成功後の遷移先 URL（StatePayload.ReturnTo 由来）
	//   - err: state_invalid / state_expired / state_mismatch / state_console_mismatch /
	//     state_replay / upstream_oidc_token / invalid_aud / nonce_mismatch /
	//     admin_user_not_provisioned / csprng_failure 等
	HandleCallback(ctx context.Context, console oidc.Console, code, queryState, rawStateCookie string) (rawSessionToken string, sessionCookie http.Cookie, returnTo string, err error)

	// LookupAndRefresh は session cookie を lookup し、console 照合 + 失効判定 + last_seen_at
	// 更新を行う。失効時は repository.Revoke を呼んで永続ストア側も失効状態にする（Req 4.6）。
	//
	// 失効判定順序: console_mismatch → session_expired → session_revoked → session_idle
	LookupAndRefresh(ctx context.Context, rawSessionToken string, expectedConsole oidc.Console, now time.Time) (Identity, Session, error)

	// Logout は session を revoke する（既に revoked でも no-op）。
	Logout(ctx context.Context, rawSessionToken string) error
}

// TokenGenerator は opaque session token の生成を抽象化する DI 境界。
//
// 本番は `session.New` を直接渡す。テストでは fake fn を渡して CSPRNG 失敗
// シナリオ（Req 3.5 / NFR 3.1 の fail-closed）を観測する。
type TokenGenerator func() (rawToken string, err error)

// service は Service interface の本番実装。
type service struct {
	cfg           config.Config
	verifier      oidc.Verifier
	repo          Repository
	oauth2Configs map[oidc.Console]*oauth2.Config
	clock         Clock
	tokenGen      TokenGenerator
	log           logger.Logger
}

// NewService は本番用 Service を構築する。
//
// oauth2Configs は tenant / admin で別 ClientID / ClientSecret / RedirectURL / Endpoint /
// Scopes を保持する。`Scopes` には必ず `goidc.ScopeOpenID`（= "openid"）を含めること
// （`openid` scope が無いと IdP は authorization code フローで `id_token` を発行しない）。
//
// tokenGen は session 生 token 生成器。本番では `session.New` を渡し、テストでは fake を
// 渡せる構造（design.md「Auth Service」節 / task 5.1 詳細項目と整合）。
func NewService(
	cfg config.Config,
	verifier oidc.Verifier,
	repo Repository,
	oauth2Configs map[oidc.Console]*oauth2.Config,
	clock Clock,
	tokenGen TokenGenerator,
	log logger.Logger,
) Service {
	return &service{
		cfg:           cfg,
		verifier:      verifier,
		repo:          repo,
		oauth2Configs: oauth2Configs,
		clock:         clock,
		tokenGen:      tokenGen,
		log:           log,
	}
}

// BeginLogin は Service.BeginLogin の実装。
func (s *service) BeginLogin(ctx context.Context, console oidc.Console, returnTo string) (string, http.Cookie, error) {
	// 1. returnTo の正規化と validate（open redirect 防止 / design.md 確認事項 3）
	normalizedReturnTo, err := normalizeReturnTo(returnTo)
	if err != nil {
		s.log.Warn("auth failure",
			"failure_kind", string(FailureKindReturnToInvalid),
			"console", string(console),
		)
		return "", http.Cookie{}, err
	}

	// 2. Nonce / OIDCNonce を `crypto/rand` で **独立に** 16 byte ずつ生成する。
	//    同値を使い回さない（OAuth `state` と OIDC `nonce` は別パラメータ / RFC OIDC Core
	//    1.0 §3.1.2.1 / design.md StatePayload definition）。
	nonce, err := generateNonce()
	if err != nil {
		s.log.Warn("auth failure",
			"failure_kind", string(FailureKindCSPRNGFailure),
			"console", string(console),
		)
		return "", http.Cookie{}, pkgerrors.Wrap(
			pkgerrors.CodeInternal,
			"state nonce generation failed",
			joinFailureKind(err, FailureKindCSPRNGFailure),
		)
	}
	oidcNonce, err := generateNonce()
	if err != nil {
		s.log.Warn("auth failure",
			"failure_kind", string(FailureKindCSPRNGFailure),
			"console", string(console),
		)
		return "", http.Cookie{}, pkgerrors.Wrap(
			pkgerrors.CodeInternal,
			"oidc nonce generation failed",
			joinFailureKind(err, FailureKindCSPRNGFailure),
		)
	}

	// 3. StatePayload を構築して MAC 署名された cookie 値文字列を生成
	now := s.clock.Now()
	payload := StatePayload{
		Nonce:     nonce,
		OIDCNonce: oidcNonce,
		Console:   console,
		ReturnTo:  normalizedReturnTo,
		IssuedAt:  now,
	}
	cookieValue, err := Sign(payload, []byte(s.cfg.StateMACSecret))
	if err != nil {
		return "", http.Cookie{}, err
	}

	// 4. IdP 認可エンドポイント URL を構築。
	//    `state` クエリには Nonce、`nonce` クエリには OIDCNonce を載せる（別パラメータ）。
	oauthCfg, ok := s.oauth2Configs[console]
	if !ok || oauthCfg == nil {
		// 構成不備（cmd/api bootstrap で必ず両 console を登録する責務）。
		return "", http.Cookie{}, pkgerrors.New(
			pkgerrors.CodeInternal,
			"oauth2 config missing for console",
		)
	}
	redirectURL := oauthCfg.AuthCodeURL(
		payload.Nonce,
		oauth2.SetAuthURLParam("nonce", payload.OIDCNonce),
	)

	// 5. state cookie を組み立て（Value を設定）
	cookie := CookieAttributes(s.cfg.StateCookieTTL)
	cookie.Value = cookieValue

	return redirectURL, cookie, nil
}

// HandleCallback は Service.HandleCallback の実装。
func (s *service) HandleCallback(ctx context.Context, console oidc.Console, code, queryState, rawStateCookie string) (string, http.Cookie, string, error) {
	// 1. state cookie / queryState の MAC + TTL + Nonce 検証
	payload, err := Verify(rawStateCookie, queryState, []byte(s.cfg.StateMACSecret), s.cfg.StateCookieTTL, s.clock.Now())
	if err != nil {
		s.logWarnFailure(err, console)
		return "", http.Cookie{}, "", err
	}

	// 1a. payload.Console と handler の console を照合（cross-console state 混同 reject）
	if payload.Console != console {
		err := pkgerrors.Wrap(
			pkgerrors.CodeUnauthenticated,
			"state console mismatch",
			FailureKindStateConsoleMismatch,
		)
		s.logWarnFailure(err, console)
		return "", http.Cookie{}, "", err
	}

	// 1b. state nonce を一度限り消費（PRIMARY KEY UNIQUE 違反 → state_replay）。
	//     **token 交換より前**に実行することで replay を IdP token endpoint に届かせない。
	expiresAt := payload.IssuedAt.Add(s.cfg.StateCookieTTL)
	if err := s.repo.ConsumeStateNonce(ctx, payload.Nonce, console, expiresAt); err != nil {
		s.logWarnFailure(err, console)
		return "", http.Cookie{}, "", err
	}

	// 2. code → token 交換
	oauthCfg, ok := s.oauth2Configs[console]
	if !ok || oauthCfg == nil {
		return "", http.Cookie{}, "", pkgerrors.New(
			pkgerrors.CodeInternal,
			"oauth2 config missing for console",
		)
	}
	token, err := oauthCfg.Exchange(ctx, code)
	if err != nil {
		wrapped := pkgerrors.Wrap(
			pkgerrors.CodeUpstream,
			"oidc token exchange failed",
			joinFailureKind(err, FailureKindUpstreamOIDCToken),
		)
		s.logWarnFailure(wrapped, console)
		return "", http.Cookie{}, "", wrapped
	}

	// 3. id_token を安全に取り出す（直接 type assertion は panic するため two-value form を使う）
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		err := pkgerrors.Wrap(
			pkgerrors.CodeUpstream,
			"oidc id_token missing from token response",
			FailureKindUpstreamOIDCToken,
		)
		s.logWarnFailure(err, console)
		return "", http.Cookie{}, "", err
	}

	// 4. id_token 検証
	claims, err := s.verifier.VerifyIDToken(ctx, rawIDToken)
	if err != nil {
		s.logWarnFailure(err, console)
		return "", http.Cookie{}, "", err
	}

	// 4a. Claims.MatchedConsole と handler の console を照合（Req 6.2 クライアント分離強制）
	if claims.MatchedConsole != console {
		err := pkgerrors.Wrap(
			pkgerrors.CodeUnauthenticated,
			"id token aud does not match handler console",
			FailureKindInvalidAud,
		)
		s.logWarnFailure(err, console)
		return "", http.Cookie{}, "", err
	}

	// 4b. Claims.Nonce と payload.OIDCNonce を constant-time 比較
	//     （authorization code injection 防止 / OIDC Core 1.0 §3.1.2.7）
	if subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(payload.OIDCNonce)) != 1 {
		err := pkgerrors.Wrap(
			pkgerrors.CodeUnauthenticated,
			"id token nonce does not match state cookie oidc nonce",
			FailureKindNonceMismatch,
		)
		s.logWarnFailure(err, console)
		return "", http.Cookie{}, "", err
	}

	// 5. admin_users 解決（事前 provisioning 必須 / 0 行は 403）
	identity, err := s.repo.ResolveAdminUser(ctx, claims.Issuer, claims.Subject, claims.Email, console)
	if err != nil {
		s.logWarnFailure(err, console)
		return "", http.Cookie{}, "", err
	}

	// 6. session token 生成（CSPRNG 失敗は 500 / fail-closed）
	rawSessionToken, err := s.tokenGen()
	if err != nil {
		wrapped := pkgerrors.Wrap(
			pkgerrors.CodeInternal,
			"session token generation failed",
			joinFailureKind(err, FailureKindCSPRNGFailure),
		)
		s.logWarnFailure(wrapped, console)
		return "", http.Cookie{}, "", wrapped
	}
	tokenHash := HashToken(rawSessionToken)

	// 7. session を永続化
	now := s.clock.Now()
	sess := Session{
		TokenHash:   tokenHash,
		AdminUserID: identity.AdminUserID,
		Console:     console,
		IssuedAt:    now,
		LastSeenAt:  now,
		ExpiresAt:   now.Add(s.cfg.SessionAbsoluteTimeout),
		RevokedAt:   nil,
	}
	if err := s.repo.Create(ctx, sess); err != nil {
		s.logWarnFailure(err, console)
		return "", http.Cookie{}, "", err
	}

	// 8. session cookie を組み立て（Value を設定）
	cookie := SessionCookieAttributes(s.cfg.SessionAbsoluteTimeout)
	cookie.Value = rawSessionToken

	return rawSessionToken, cookie, payload.ReturnTo, nil
}

// LookupAndRefresh は Service.LookupAndRefresh の実装。
func (s *service) LookupAndRefresh(ctx context.Context, rawSessionToken string, expectedConsole oidc.Console, now time.Time) (Identity, Session, error) {
	if rawSessionToken == "" {
		err := pkgerrors.Wrap(
			pkgerrors.CodeUnauthenticated,
			"session cookie missing",
			FailureKindSessionTamper,
		)
		s.logWarnSession(err, expectedConsole, "")
		return Identity{}, Session{}, err
	}
	tokenHash := HashToken(rawSessionToken)

	sess, identity, err := s.repo.Get(ctx, tokenHash)
	if err != nil {
		s.logWarnSession(err, expectedConsole, tokenHash)
		return Identity{}, Session{}, err
	}

	// 失効判定順序: 1) console_mismatch → 2) session_expired → 3) session_revoked → 4) session_idle
	if sess.Console != expectedConsole {
		err := pkgerrors.Wrap(
			pkgerrors.CodeUnauthenticated,
			"session console does not match expected console",
			FailureKindConsoleMismatch,
		)
		s.revokeOnExpire(ctx, tokenHash, now)
		s.logWarnSession(err, expectedConsole, tokenHash)
		return Identity{}, Session{}, err
	}
	if now.After(sess.ExpiresAt) {
		err := pkgerrors.Wrap(
			pkgerrors.CodeUnauthenticated,
			"session absolute expiry exceeded",
			FailureKindSessionExpired,
		)
		s.revokeOnExpire(ctx, tokenHash, now)
		s.logWarnSession(err, expectedConsole, tokenHash)
		return Identity{}, Session{}, err
	}
	if sess.RevokedAt != nil {
		err := pkgerrors.Wrap(
			pkgerrors.CodeUnauthenticated,
			"session revoked",
			FailureKindSessionRevoked,
		)
		s.logWarnSession(err, expectedConsole, tokenHash)
		return Identity{}, Session{}, err
	}
	if now.Sub(sess.LastSeenAt) > s.cfg.SessionIdleTimeout {
		err := pkgerrors.Wrap(
			pkgerrors.CodeUnauthenticated,
			"session idle timeout exceeded",
			FailureKindSessionIdle,
		)
		s.revokeOnExpire(ctx, tokenHash, now)
		s.logWarnSession(err, expectedConsole, tokenHash)
		return Identity{}, Session{}, err
	}

	// 有効 → last_seen_at を更新
	if err := s.repo.Touch(ctx, tokenHash, now); err != nil {
		s.logWarnSession(err, expectedConsole, tokenHash)
		return Identity{}, Session{}, err
	}
	sess.LastSeenAt = now
	return identity, sess, nil
}

// Logout は Service.Logout の実装。
func (s *service) Logout(ctx context.Context, rawSessionToken string) error {
	if rawSessionToken == "" {
		// 入力不在は no-op（既に logout 済み / cookie 不在）。Handler が cookie 削除属性を
		// 発行することで終端する。
		return nil
	}
	tokenHash := HashToken(rawSessionToken)
	return s.repo.Revoke(ctx, tokenHash, s.clock.Now())
}

// revokeOnExpire は失効検出時に repo.Revoke を冪等に呼ぶ helper（Req 4.6）。
//
// Revoke のエラーは failure_kind 経路を上書きしないよう、ログのみに残して戻り値には
// 反映しない（呼び出し側は元の failure_kind error を伝播する）。
func (s *service) revokeOnExpire(ctx context.Context, tokenHash string, now time.Time) {
	if err := s.repo.Revoke(ctx, tokenHash, now); err != nil {
		// 失敗パスでの追加 Revoke エラーは観測のみ（NFR 3.1 fail-closed の優先）。
		s.log.Warn("session revoke on expire failed",
			"session_hash_prefix", HashPrefix(tokenHash),
		)
	}
}

// logWarnFailure は state / OIDC 検証経路の失敗を `failure_kind` field 付きで WARN ログに出力する。
//
// NFR 4.1 の実装責務。`failure_kind` を Cause チェーンから抽出して field に出す
// （Cause に乗せるだけでは観測性 AC を満たせない / tasks.md 5.1 詳細項目）。
func (s *service) logWarnFailure(err error, console oidc.Console) {
	kind := extractFailureKind(err)
	s.log.Warn("auth failure",
		"failure_kind", kind,
		"console", string(console),
	)
}

// logWarnSession は session lookup 経路の失敗を `failure_kind` + `session_hash_prefix` 付きで
// WARN ログに出力する。生 hash は出さず先頭 8 文字のみ（Req 3.8）。
func (s *service) logWarnSession(err error, console oidc.Console, tokenHash string) {
	kind := extractFailureKind(err)
	fields := []any{
		"failure_kind", kind,
		"console", string(console),
	}
	if tokenHash != "" {
		fields = append(fields, "session_hash_prefix", HashPrefix(tokenHash))
	}
	s.log.Warn("auth failure", fields...)
}

// extractFailureKind は err の Cause チェーンから failure_kind 文字列値を抽出する。
//
// 見つからない場合は空文字を返す（Logger 側で field 不在として扱う）。
// auth package の failureKind は `errors.As` で直接識別できる。oidc package の failureKind は
// unexported type なので型一致は使えないが、`Error()` の戻り値が string ベースの sentinel
// 値なので Cause チェーンを走査して既知の文字列リストと突き合わせる（NFR 4.1 の二系統対応）。
func extractFailureKind(err error) string {
	if err == nil {
		return ""
	}
	// 1. auth package の failureKind を優先的に確認
	var kind failureKind
	if stderrors.As(err, &kind) {
		return string(kind)
	}
	// 2. oidc package の failure_kind 文字列を Cause チェーンから拾う（best-effort）
	knownOIDC := []string{
		"invalid_sig",
		"invalid_iss",
		"invalid_aud",
		"aud_ambiguous",
		"token_expired",
		"invalid_kid",
		"oidc_discovery",
	}
	for cur := err; cur != nil; cur = stderrors.Unwrap(cur) {
		msg := cur.Error()
		for _, k := range knownOIDC {
			if msg == k {
				return k
			}
		}
	}
	return ""
}

// normalizeReturnTo は returnTo クエリパラメータを正規化・validate する。
//
// design.md 確認事項 3 採用案: 同一オリジン内の相対 URL のみ許容。空文字は default `/`。
// `http://` / `https://` / `//` を含む host 指定はすべて 400 で reject（open redirect 防止）。
//
//   - 空文字 → "/"（default）
//   - "/" で始まり、かつ "//" で始まらない → 受理（同一オリジン相対パス）
//   - それ以外（"http://" / "https://" / "//evil.example" / "javascript:" / 相対パス
//     "dashboard"）→ 400 reject
func normalizeReturnTo(returnTo string) (string, error) {
	if returnTo == "" {
		return "/", nil
	}
	// "//" で始まる scheme-relative URL は host 指定可能なので reject
	if strings.HasPrefix(returnTo, "//") {
		return "", pkgerrors.Wrap(
			pkgerrors.CodeInvalidRequest,
			"return_to must be a same-origin relative path",
			FailureKindReturnToInvalid,
		)
	}
	// "/" で始まらない（http:// 含む absolute URL や相対パス）→ reject
	if !strings.HasPrefix(returnTo, "/") {
		return "", pkgerrors.Wrap(
			pkgerrors.CodeInvalidRequest,
			"return_to must be a same-origin relative path",
			FailureKindReturnToInvalid,
		)
	}
	// url.Parse で host が空であることを念のため確認（"/dashboard" は host=""）
	u, parseErr := url.Parse(returnTo)
	if parseErr != nil {
		return "", pkgerrors.Wrap(
			pkgerrors.CodeInvalidRequest,
			"return_to is not a valid URL",
			FailureKindReturnToInvalid,
		)
	}
	if u.Host != "" || u.Scheme != "" {
		return "", pkgerrors.Wrap(
			pkgerrors.CodeInvalidRequest,
			"return_to must not contain host or scheme",
			FailureKindReturnToInvalid,
		)
	}
	return returnTo, nil
}

// generateNonce は 16 byte の暗号学的乱数を base64url no-padding で文字列化して返す。
//
// OAuth `state` と OIDC `nonce` の両方に同じ generator を使うが、呼び出し側で **別個に
// 呼ぶ**ことで独立した値を得る（同値を使い回さない / RFC OIDC Core 1.0 §3.1.2.1）。
//
// 失敗時は `crypto/rand.Read` の error をそのまま返す（Service 側で wrap）。
func generateNonce() (string, error) {
	const nonceBytes = 16
	buf := make([]byte, nonceBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// joinFailureKind は wrap 元 error の上に failureKind sentinel を貼り付けた error を返す。
//
// errors.Is(err, FailureKindXxx) で識別できるようにする。
func joinFailureKind(err error, kind failureKind) error {
	if err == nil {
		return kind
	}
	return fmt.Errorf("%w: %w", kind, err)
}
