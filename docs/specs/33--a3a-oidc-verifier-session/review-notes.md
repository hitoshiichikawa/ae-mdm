# Review Notes

<!-- idd-claude:review round=1 model=claude-sonnet-4-5 timestamp=2026-06-26T00:00:00Z -->

## Reviewed Scope

- Branch: claude/issue-33-impl--a3a-oidc-verifier-session
- HEAD commit: f0f5c9287c4056b078c846b3f73f68c1f88c51a1
- Compared to: develop..HEAD（Stage B HEAD 全体レビュー / 全 task 完了後の独立レビュー）
- Feature Flag Protocol: opt-out（CLAUDE.md 宣言値）→ flag 観点細目は適用しない

## Verified Requirements

### Requirement 1 — OIDC ID トークン検証

- 1.1（JWKS 公開鍵で署名検証） — `internal/platform/oidc/verifier.go` `VerifyIDToken` + `TestVerifyIDToken_TenantAudience_Success` / `..._AdminAudience_Success` で署名検証が成立することを確認
- 1.2（iss 完全一致） — `expectedIssuer` 比較 + `TestVerifyIDToken_IssuerMismatch_RejectsAsInvalidIss`
- 1.3（aud で console 判別） — `Claims.MatchedConsole` + tenant/admin 成功テスト
- 1.4（aud 不一致拒否） — `TestVerifyIDToken_AudienceMismatch_RejectsAsInvalidAud`
- 1.5（aud 配列で両方含む場合は曖昧として拒否） — `TestVerifyIDToken_AudienceArrayHasBoth_RejectsAsAmbiguous`
- 1.6（exp 検証） — `TestVerifyIDToken_Expired_RejectsAsTokenExpired`
- 1.7（署名検証失敗で拒否） — `TestVerifyIDToken_SignatureFailure_RejectsAsInvalidSig`
- 1.8（iss 不一致拒否） — 1.2 と同テスト
- 1.9（kid rotation で JWKS 再取得） — `TestVerifyIDToken_KidRotation_RefetchesJWKS`（jwksHits atomic counter で再 fetch 確認）
- 1.10（kid 不在で拒否） — `TestVerifyIDToken_KidNotFound_RejectsAsInvalidKid`
- 1.11（raw JWT 非ログ・非永続） — `Claims` 型に raw JWT を含めない設計 + `TestVerifyIDToken_NoSensitiveValueInErrorMessage`

### Requirement 2 — state CSRF / replay 防止

- 2.1（推測困難な state を redirect URL に付与） — `Service.BeginLogin` + `oauth2.AuthCodeURL` で state 付与 + `TestBeginLogin_ValidReturnTo_ReturnsRedirectAndCookie`
- 2.2（MAC 保護 cookie） — `auth/state.go` `Sign` HMAC-SHA256 + `TestStateCookie_SignVerify_RoundTripRestoresPayload`
- 2.3（HttpOnly / Secure / SameSite=Lax） — `TestStateCookie_CookieAttributes_ReturnsExpectedAttributes`
- 2.4（10 分以内 TTL） — `cfg.StateCookieTTL` validation `1s..10m` + `config_test.go` 境界値テスト
- 2.5（callback で state MAC 一致確認） — `Service.HandleCallback` step 1 で `state.Verify` 呼出 + `TestHandleCallback_StateMismatch_*`
- 2.6（cookie 不在/期限切れ/MAC 失敗で拒否） — `TestStateCookie_EmptyCookieValue_FailsAsStateInvalid` / `TestStateCookie_TTLExpired_FailsAsStateExpired` / `TestStateCookie_MACTamper_FailsAsStateInvalid`
- 2.7（queryState/cookie 不一致拒否） — `TestStateCookie_QueryStateMismatch_FailsAsStateMismatch`（subtle.ConstantTimeCompare 経路）
- 2.8（callback で state cookie 即時無効化） — `state.ExpireCookieAttributes()` `MaxAge=-1` + `TestStateCookie_ExpireCookieAttributes_ReturnsDeletionCookie` + Handler の callback 成功/失敗/欠落判定の 3 経路全てで発行
- 2.9（別ブラウザ/別 redirect への state 流用拒否） — `__Host-` prefix + SameSite=Lax + `StatePayload.Console` 照合（`state_console_mismatch`） + OIDC nonce binding（`nonce_mismatch`） + `state_nonces` テーブル PK UNIQUE 制約による一度限り消費（`state_replay`） + `TestAuthRepository_ConsumeStateNonce_Replay` + `TestAuthCallback_StateReplay_SecondAttemptReturns401AndDeletesStateCookie`

### Requirement 3 — セッション cookie 発行と属性

- 3.1（成功時 session 識別子発行 + cookie 返却） — `Service.HandleCallback` step 5–8 + `TestHandleCallback_HappyPath_CreatesSessionAndReturnsCookie` + e2e `TestAuthCallback_HappyPath_CreatesSessionAndRedirects`
- 3.2 / 3.3 / 3.4（HttpOnly / Secure / SameSite=Lax） — `TestSession_SessionCookieAttributes_ReturnsExpectedAttributes`
- 3.5（crypto/rand 32 byte base64url） — `auth/session.go` `New()` + `TestSession_New_ReturnsBase64URLOf32Bytes` + `TestSession_New_100CallsProduceUniqueTokens`
- 3.6（生値を log/store に残さない） — Repository は hash のみ INSERT + `TestService_LogFailureKindOnFailure` + `TestMiddleware_FailurePaths_DoNotLeakSensitiveValues`
- 3.7（SHA-256 ハッシュ格納） — `auth/session.go` `HashToken` + `TestSession_HashToken_ReturnsSHA256Hex` + Repository.Create が `TokenHash` を INSERT
- 3.8（hash 短縮識別子のみログ） — `HashPrefix(hash)` 先頭 8 文字 + `TestSession_HashPrefix_Returns8Chars` + middleware/service で `session_hash_prefix` field 使用
- 3.9（aud で識別した console 種別を session に紐付） — `sessions.console` 列（migration 0013） + `TestAuthRepository_Create_Get_Touch_Revoke` で Console 永続を確認

### Requirement 4 — idle / 絶対タイムアウト

- 4.1（last_seen_at 記録） — `Service.HandleCallback` step 6 で `LastSeenAt: now` + happy path テスト
- 4.2（absolute expires_at = issued + 8h） — 同 step で `ExpiresAt: now.Add(cfg.SessionAbsoluteTimeout)`
- 4.3（リクエスト時 last_seen_at 更新） — `Repository.Touch` + middleware が `LookupAndRefresh` 経由で更新 + integration test
- 4.4（idle 30 分超失効） — `TestLookupAndRefresh_IdleBoundaries`（29:59 / 30:00 / 30:01 境界） + e2e `TestAuthLookup_IdleExceeded_Returns401AndRevokes`
- 4.5（absolute 8h 超失効） — `TestLookupAndRefresh_AbsoluteBoundaries`（7:59:59 / 8:00:00 / 8:00:01） + e2e `TestAuthLookup_AbsoluteExceeded_Returns401`
- 4.6（失効時永続ストアも失効状態） — `revokeOnExpire` で `repo.Revoke` 冪等呼出 + e2e で `revoked_at` セット確認
- 4.7（失効時 cookie 削除） — `TestMiddleware_SessionIdle_Returns401AndExpiresCookie` + middleware の deny 経路
- 4.8（Touch は absolute 不変） — `Repository.Touch` は `UPDATE sessions SET last_seen_at = $1` のみ（expires_at 不変） + integration test で確認

### Requirement 5 — セッション失効・ログアウト

- 5.1（logout で永続ストア失効） — `Service.Logout` → `Repository.Revoke` + `TestLogout_CallsRevokeOnce` + e2e `TestAuthLogout_RevokesSession_AndRePresentationReturns401`
- 5.2（logout で cookie 削除） — Handler.logout が `SessionExpireCookieAttributes()` 返却 + `TestLogout_TenantWithSessionCookie_Returns204AndExpiresSessionCookie`
- 5.3（logout 後同 cookie 再提示で拒否） — e2e の同 cookie 再提示 401 assertion
- 5.4（改竄 cookie 拒否） — `TestAuthRepository_Get_HashMismatch_SessionTamper` + e2e `TestAuthLookup_TamperedCookie_Returns401SessionTamper`

### Requirement 6 — 2 console OIDC クライアントの分離

- 6.1（tenant/admin 2 client 独立保持） — Verifier が 2 issuer/aud 分の Provider 構築 + `Config.Load()` 最終段で `OIDCTenantClientID != OIDCAdminClientID` / `Secret` / `RedirectURL` fail-fast 検査 + `config_test.go` の同値拒否テスト群
- 6.2（callback の path-based クライアント確定） — `Handler.Mount(r, "/api/auth", ConsoleTenant)` / `Handler.Mount(r, "/api/admin/auth", ConsoleAdmin)` の 2 度呼びで closure 固定 + `TestMount_RegistersAllSixEndpoints` + middleware/service の `expectedConsole` 引数で console 漏洩拒否
- 6.3（session に console 種別紐付） — `sessions.console` 列 + `Service.LookupAndRefresh` 内 console 照合 + `TestLookupAndRefresh_ConsoleMismatch_Returns401` + e2e `TestAuthLookup_TenantSessionOnAdminRoute_Returns401WithConsoleMismatch`
- 6.4（tenant/admin 信頼 aud を異なる値で設定可） — Config に独立フィールド + redirect URL 同値拒否 + `TestLoad_OIDCRedirectURL_SameAcrossConsoles_Rejected` 等

### Non-Functional Requirements

- NFR 1.1（機密値を平文ログに残さない） — Logger redaction allowlist 4 件追加 + `TestRedactFields_AuthAllowlistAdditions` + Service/Handler/Middleware/Verifier の `_SensitiveValuesNotEmbedded` 系テストで error wrap / response body / log field の機密非含有を assert
- NFR 1.2（SHA-256 ハッシュで格納 + 比較） — Repository は hash 列で lookup + `TestAuthRepository_Get_HashMismatch_SessionTamper`
- NFR 2.1（idle/absolute/state TTL env 可変） — `SESSION_IDLE_TIMEOUT` / `SESSION_ABSOLUTE_TIMEOUT` / `STATE_COOKIE_TTL` env + 境界値検証テスト群 + `.env.example` / runbook 6.2 節で運用手順明文化
- NFR 2.2（runtime 上書き不可） — Config 値型で immutable（A2 既存性質を継承）
- NFR 3.1（例外時 fail-closed 401） — Middleware の deny / Service の Wrap で `CodeUnauthenticated` + `TestMiddleware_ServicePanic_NextNotInvoked`
- NFR 3.2（起動時 OIDC discovery 失敗で起動失敗） — `cmd/api/main.go` で `oidc.NewVerifier` err → log.Error + return 1 + `TestNewVerifier_DiscoveryFailure_ReturnsUnavailable`
- NFR 4.1（失敗種別構造化ログ） — `failure_kind` field 出力 + `TestService_LogFailureKindOnFailure` + e2e で 9 種 failure_kind（state_invalid / invalid_aud / admin_user_not_provisioned / state_replay / session_idle / session_expired / console_mismatch / session_revoked / session_tamper）の captured log assertion
- NFR 4.2（session 生値 / ID token / state MAC 鍵を log に含めない） — `TestMiddleware_FailurePaths_DoNotLeakSensitiveValues` 等で response body と log field の双方で機密値非含有を assert

## Boundary 違反チェック

tasks.md の `_Boundary:_` 列挙コンポーネント（Config / Migrations / HTTPServer / Logger / OIDCVerifier / AuthTypes / StateCookie / AuthClock / SessionCookie / AuthRepository / AuthService / AuthHandler / AuthMiddleware / cmd-api / Documentation）に対応するファイルパスのみ変更されている。`backend/internal/depspin/depspin.go` の `coreos-go-oidc` blank import 削除は task 7.1 詳細項目 (d) で明示宣言されたスコープ内変更。`backend/test/integration/db_tenant_isolation_test.go` / `helpers_test.go` / `http_subrouter_mount_test.go` の小幅修正は migration 0013/0014 に伴う既存 fixture の `oidc_issuer` カラム追加・`idle_at` → `last_seen_at` 置換・`AuthClaims` rename 追随等で、task 1.2 / 1.3 詳細項目に明示記載済み。境界外への変更は検出されなかった。

## テスト実行確認

impl-notes.md の各 `### Task X.Y — Verify 実行結果` 節で以下を確認:

- `cd backend && go build ./...`: 全 task で PASS
- `cd backend && go vet ./...`: 全 task で PASS
- `cd backend && go test ./...`: 全 task で PASS
- DB-backed verify: task 4.1 で 8 関数 PASS（auth_repository_test） + task 6.4 で 12 関数 PASS（auth_login_callback + auth_session_lookup + auth_logout_revoke） / 合計 20 関数 + 既存 integration test 全件 PASS

## Findings

なし

## Summary

requirements.md の全 numeric ID（Req 1.1〜1.11 / 2.1〜2.9 / 3.1〜3.9 / 4.1〜4.8 / 5.1〜5.4 / 6.1〜6.4 および NFR 1.1〜4.2）について、実装またはテストでのカバレッジを確認した。境界違反・missing test・AC 未カバーは検出されず、DB-backed integration verify（20 関数 PASS）の実行結果も impl-notes.md に適切に記録されている。

RESULT: approve
