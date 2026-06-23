# Implementation Plan

> 本 Issue（#3 / A3）は ae-mdm の認証認可基盤（OIDC verify / opaque session + cookie /
> 4 ロール表駆動 RBAC / `/api/admin` ガード）を提供する。A2（#2）で確立済みの
> `internal/config` / `internal/logger` / `internal/errors` / `internal/platform/db` /
> `internal/platform/httpserver` を **再利用**し、本 Issue では認証認可ドメイン固有の
> 新規 package（`internal/platform/oidc` / `internal/platform/authz` / `internal/auth`）と
> 既存 httpserver への minor 改修（authClaims 拡張 / SessionAuthMiddleware 追加 /
> RequireSuperAdmin の aud チェック追加）を行う。
>
> 全タスクは `_Requirements:_` の AC に対応するテスト追加を **同 task 内**に含む。
> 並列実行可能タスクには `(P)` を付与し `_Boundary:_` を明示する。

- [ ] 1. 共通基盤の拡張（config / errors / migration 0013）
  - `backend/internal/config/config.go` に `SessionIdleMinutes int`（default 30）と
    `SessionAbsoluteHours int`（default 8）を追加
  - `backend/internal/config/env.go` に `SESSION_IDLE_MINUTES` / `SESSION_ABSOLUTE_HOURS` の
    `intWithDefault` 呼び出しを追加（int 解析失敗で `CodeConfigInvalid`、欠落時は default）
  - `.env.example` に同 2 行を追加（コメントで idle / absolute timeout の意味を記載）
  - `backend/internal/errors/codes.go` に `CodeOIDCInvalid Code = "oidc_invalid"` と
    `CodeUnknownRole Code = "unknown_role"` を追加し、`defaultHTTPStatus(code)` の switch に
    両方とも 401（`http.StatusUnauthorized`）を返すケースを追加
  - `backend/internal/errors/http_mapping.go` の `WriteHTTP` 応答 body フォーマットを
    確認し、403 / 404 応答が `{code, message, request_id}` のみで対象リソース ID / path を
    含まないことを再確認（Req 6.2 / 6.3。A2 で既に統一されているはずなので本タスクでは
    **regression test 追加のみ**）
  - `backend/internal/logger/redact.go` の機密 allowlist に `client_secret` / `session_token`
    / `code_verifier` を追加（NFR 5.1）
  - `backend/db/migrations/0013_add_sessions_aud.up.sql` /
    `0013_add_sessions_aud.down.sql` を新規追加。up は `ALTER TABLE sessions ADD COLUMN
    aud text NOT NULL DEFAULT 'tenant-console' CHECK (aud IN ('tenant-console',
    'admin-console'))` → `ALTER TABLE sessions ALTER COLUMN aud DROP DEFAULT` の 2 段、down は
    `ALTER TABLE sessions DROP COLUMN aud`
  - **同 task 内テスト**: `config_test.go` に `SESSION_IDLE_MINUTES=30` / `SESSION_ABSOLUTE_HOURS=8`
    の default 適用と int 解析失敗の test ケースを追加。`errors_test.go` に
    `CodeOIDCInvalid` / `CodeUnknownRole` が HTTP 401 にマップされる test と、403 / 404
    body が `{code, message, request_id}` のみで対象 ID を含まないことの regression test
    （Req 6.2 / 6.3）を追加。`logger_test.go` に新規 allowlist 3 件の redaction test を追加
  - _Requirements: 2.5, 6.2, 6.3, NFR 5.1_
  - _Boundary: Config, Errors, Logger, Migration0013_

- [ ] 2. OIDC Verifier + VerifierSet + Token Exchange + Role Mapping (P)
  - `backend/internal/platform/oidc/verifier.go` に `Audience` 型（`tenant-console` /
    `admin-console` 定数）、`Claims` struct（Subject/Email/Groups/Issuer/Audience/ExpiresAt）、
    `Verifier` interface（`Verify(ctx, rawIDToken, expectedAud) (Claims, error)` /
    `ExchangeCode(ctx, code, redirectURL) (rawIDToken, error)` / `AuthorizeURL(state,
    codeChallenge) string`）を定義
  - `verifier_set.go` に `VerifierSet` 構造体と `NewVerifierSet(ctx, cfg) (*VerifierSet, error)`
    + `For(aud) Verifier` を実装。tenant / admin の 2 OIDC Provider に対し `coreos/go-oidc/v3`
    の `oidc.NewProvider` + `oidc.NewVerifier` を構築（OIDC discovery 失敗時は fail-fast）。
    `oauth2.Config` を内部に保持し ExchangeCode / AuthorizeURL を委譲
  - `role_mapping.go` に `MapRoles(groups []string) []authz.Role` を実装（Keycloak の
    `/SuperAdmin` / `/TenantAdmin` / `/Operator` / `/Viewer` を写像。未知のみで空 slice 返却
    → Req 4.12 で session 発行拒否のシグナル）。authz package への依存方向は OK
    （oidc → authz を許す。逆方向は禁止）
  - `token_exchange.go` に `ExchangeCode` の本体（`oauth2.Config.Exchange` + 戻り
    `id_token` 抽出）
  - 検証失敗時は `*errors.Error{Code: CodeOIDCInvalid}`、Cause に `invalid_signature` /
    `invalid_issuer` / `invalid_audience` / `token_expired` の判別文字列を含める
  - **同 task 内テスト**: `verifier_test.go` で `httptest` ベースの mock OIDC server
    （discovery + JWKS + token endpoint）を立て、(a) 正常系（aud 一致）/(b) 署名不正 /
    (c) iss 不一致 / (d) aud 不一致（tenant-console token を expectedAud=admin-console で
    reject）/ (e) exp 切れ の 5 分岐を網羅。`role_mapping_test.go` で Keycloak 形式 groups
    の写像（正常 / 未知のみ / 混在）テスト
  - _Requirements: 2.1, 2.2, 2.3, 2.4, 2.5, 2.6, 2.7, 2.8, 2.9, 4.2, NFR 2.1, NFR 2.2_
  - _Boundary: OIDCVerifier, VerifierSet, RoleMapping, TokenExchanger_
  - _Depends: 1_

- [ ] 3. RBAC Authorizer + PermissionsMatrix + Require chi middleware (P)
  - `backend/internal/platform/authz/roles.go` に `Role` 型と `SuperAdmin` / `TenantAdmin` /
    `Operator` / `Viewer` 定数（Req 4.1）
  - `actions.go` に `Action` 型（`read` / `create` / `update` / `delete` / `command:lock` /
    `command:reboot` / `command:wipe`）と `ResourceType` 型（`tenant` / `admin_user` /
    `policy` / `device` / `command` / `app` / `audit_log` / `enrollment_token` /
    `audit_log_cross_tenant`）
  - `permissions.go` に `Matrix map[Role]map[Action]map[ResourceType]bool` を `init()` で
    構築（4 × 7 × 9 = 252 セルすべて bool 明示。partial fill 禁止 = fail-closed）。
    Authorizer interface + `DefaultAuthorizer` 実装（`Authorize(p Principal, action,
    resource, targetTenantID) error` / `PermissionsFor(role) map[Action]map[ResourceType]bool`）。
    TenantAdmin / Operator / Viewer は role-level の Matrix bool が true でも
    `targetTenantID != p.TenantID && targetTenantID != uuid.Nil` で false に倒す。SuperAdmin
    は cross-tenant 許可（Req 4.10 / 5.8）
  - `middleware.go` に `Require(authz Authorizer, action Action, resource ResourceType)
    func(http.Handler) http.Handler` を実装。chi middleware として最外層から `auth.PrincipalFromContext`
    で principal 取得 → Authorize → 失敗時 `*errors.Error{Code: CodeForbidden}` で 403。
    403 応答 body は `errors.WriteHTTP` 経由で `{code, message, request_id}` のみを返し、
    対象 resource_id / path を含めない（Req 6.1 / 6.2 / 6.3）
  - 注意: 本 task の `auth.PrincipalFromContext` は task 4 で実装される。本 task では
    `authz/middleware.go` 内に `principalFromCtx func(context.Context) (Principal, error)` の
    differ injection point（パッケージレベル変数）を用意し、task 4 で `auth` package が
    init で書き込む形にする
  - **同 task 内テスト**: `permissions_test.go` で 4 role × 7 action × 9 resource の 252 セル
    全網羅の表駆動テスト。`Operator` の `command:wipe` 拒否、`Viewer` の write/command 全拒否、
    `TenantAdmin` の cross-tenant 拒否、`SuperAdmin` の `audit_log_cross_tenant` 許可を
    explicit ケースとして追加（Req 4.5〜4.11, 5.8）。`middleware_test.go` で chi middleware
    の 403 / 200 経路を verify。403 body に対象 resource_id / path が含まれないことの
    assertion を含める（Req 6.1 / 6.2 / 6.3）
  - _Requirements: 4.1, 4.3, 4.4, 4.5, 4.6, 4.7, 4.8, 4.9, 4.10, 4.11, 5.8, 6.1, 6.2, 6.3, NFR 4.1, NFR 4.2_
  - _Boundary: Authorizer, PermissionsMatrix, RequireMiddleware_
  - _Depends: 1_

- [ ] 4. AuthRepository + SessionManager + Principal
  - `backend/internal/auth/principal.go` に `Principal` struct（AdminUserID / TenantID /
    Roles / IsSuperAdmin / Audience）と `PrincipalFromContext(ctx) (Principal, error)` /
    `WithPrincipal(ctx, p) context.Context` を実装。同 file の `init()` で
    `authz.PrincipalFromContextSetter`（task 3 で用意した injection point）に登録
  - `repository.go` に `AuthRepository` interface（UpsertAdminUser / ReplaceRoles /
    InsertSession / FindSessionByHash / UpdateSessionIdle / DeleteSession /
    FindRolesForAdminUser）と `pgxRepository` 実装。全 method は `pgx.Tx` を引数で受ける
    （pool 直接アクセス禁止）。`sessions.aud` カラムを INSERT / SELECT に含める
  - `session_token.go` に opaque token 生成（`crypto/rand` で 32B → `base64.RawURLEncoding`）と
    sha256 ハッシュヘルパ
  - `session_manager.go` に `SessionManager` interface（`Issue(ctx, adminUserID, aud) (*http.Cookie,
    error)` / `Validate(ctx, rawToken) (Principal, error)` / `Revoke(ctx, rawToken) error`）
    と `DefaultSessionManager` 実装:
    - Issue: tx 内 `app.is_superadmin=true` 文脈で `InsertSession`（idle_at=now+IdleMin,
      expires_at=now+AbsoluteHours, aud）→ `http.Cookie{Name:"ae_session",
      Value: rawToken, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, Path:"/"}`
      を返す（NFR 1.1 / 1.2 / 1.3）
    - Validate: tx 内 `app.is_superadmin=true` 文脈で `FindSessionByHash` →
      `now > expires_at` で `DeleteSession` + 401 / `now > idle_at + IdleMin` で
      `DeleteSession` + 401 / 成功時 `UpdateSessionIdle(hash, now)` → `FindRolesForAdminUser`
      → Principal 組み立て（Audience は sessions.aud を継承）
    - Revoke: tx 内で `DeleteSession`
  - cookie 値の生値は logger に出力しない（NFR 5.1）。Validate 失敗 log には sha256 hash の
    先頭 8 文字のみを `session_hash_prefix` field で出す
  - **同 task 内テスト**: `session_token_test.go` で base64 長さ 43 文字 / hash の決定性 verify。
    `session_manager_test.go` で in-memory mock repository を使い:
    - Issue 後の cookie 属性（HttpOnly / Secure / SameSite=Lax / Path=/）全 verify（Req 3.2〜3.4 /
      NFR 1.1〜1.3）
    - Validate の idle 境界（29 / 30 / 31 分の 3 ケース、NFR 3.1）
    - Validate の absolute 境界（7h59m / 8h / 8h1m、NFR 3.2）
    - Revoke 後の Validate が `CodeUnauthenticated` を返す（NFR 3.3 / Req 3.9）
    - Validate 内で idle_at が UPDATE されることを mock で verify（Req 3.5）
  - _Requirements: 2.5, 3.1, 3.2, 3.3, 3.4, 3.5, 3.6, 3.7, 3.8, 3.9, NFR 1.1, NFR 1.2, NFR 1.3, NFR 3.1, NFR 3.2, NFR 3.3, NFR 5.1_
  - _Boundary: AuthRepository, SessionManager, Principal_
  - _Depends: 1, 3_

- [ ] 5. AuthService + AuthHandler + audit emitter IF
  - `backend/internal/auth/audit_emitter.go` に `RecorderInterface` interface
    （`Record(ctx, RoleChangeEvent) error`）と `NoopRecorder` 実装、`RoleChangeEvent` struct
    （ActorID / TargetAdminUserID / FromRole / ToRole / OccurredAt）を定義
  - `service.go` に `Service` interface（`HandleCallback(ctx, code, aud, redirectURL)
    (*http.Cookie, postLoginURL, error)` / `ChangeRole(ctx, target, newRole) error` /
    `Logout(ctx, rawToken) error`）と実装。HandleCallback:
    1. `Verifier.ExchangeCode` → rawIDToken
    2. `Verifier.Verify(rawIDToken, expectedAud)` → Claims（失敗時 `CodeOIDCInvalid`）
    3. `oidc.MapRoles(Claims.Groups)` → []Role（空ならば `CodeUnknownRole`、Req 4.12）
    4. `BeginTxFunc(WithTenantContext(ctx, db.TenantContext{IsSuperAdmin:true}), pool, fn)`
       内で `UpsertAdminUser` → `ReplaceRoles` → `SessionManager.Issue(adminUserID, aud)` →
       cookie を返す
  - ChangeRole: tx 内で `admin_role_assignments` UPDATE → `Recorder.Record(role_change)` →
    err なら rollback + `CodeBusinessRule` 422（Req 7.3）
  - `handler.go` に `Routes(svc Service, sessMgr SessionManager, aud oidc.Audience,
    postLoginRedirect string) chi.Router` を実装（aud と redirect は呼び出し側が tenant /
    admin で個別指定する設計に統一）。endpoints:
    - GET /login: state cookie 発行（HMAC-SHA256(SessionSecret, nonce) で MAC、HttpOnly +
      Secure + SameSite=Lax + 5 分 Max-Age）→ Verifier.AuthorizeURL で 302
    - GET /callback: state cookie 検証 → svc.HandleCallback → cookie 設定 + 302 to
      post_login_redirect
    - POST /logout: cookie から rawToken 取得 → svc.Logout → Set-Cookie Max-Age=-1 + 204
    - GET /session: PrincipalFromContext から SessionInfo JSON 応答（email は session ではなく
      admin_users から JOIN 取得。本 task では admin_user_id + roles + tenant_id + aud の
      JSON を返すだけで十分）
  - **同 task 内テスト**: `service_test.go` で mock OIDCVerifier / mock AuthRepository /
    mock SessionManager / mock Recorder で:
    - HandleCallback 正常系（aud=tenant-console と aud=admin-console の 2 経路）
    - OIDC verify 失敗時 SessionManager.Issue が呼ばれないこと（NFR 2.1 / Req 1.7 / 2.6〜2.9）
    - MapRoles 結果が empty で `CodeUnknownRole` （Req 4.12）
    - ChangeRole で Recorder.Record が err を返すと admin_role_assignments の UPDATE が
      rollback され、`CodeBusinessRule` 422 が返ること（Req 7.3）
    - Logout 経路で SessionManager.Revoke が呼ばれること
  - `handler_test.go` で /login の state cookie 発行 + Location ヘッダ、/callback の state
    検証 / Set-Cookie / 302 リダイレクト、/logout の 204 + Set-Cookie Max-Age=-1 を `httptest`
    ベースで verify
  - _Requirements: 1.1, 1.2, 1.3, 1.4, 1.5, 1.6, 1.7, 2.6, 2.7, 2.8, 2.9, 4.12, 7.1, 7.2, 7.3, NFR 2.1_
  - _Boundary: AuthService, AuthHandler, AuditEmitterIF_
  - _Depends: 2, 4_

- [ ] 6. httpserver 拡張（SessionAuthMiddleware + RequireSuperAdmin aud チェック）+ cmd/api 配線
  - `backend/internal/platform/httpserver/middleware.go` の既存 `authClaims` struct に
    `Audience string` field を追加（既存 test との互換は zero value=""）
  - `session_auth.go`（新規）に `SessionAuthMiddleware(sessMgr SessionManagerIF, log
    logger.Logger) func(http.Handler) http.Handler` を実装。`SessionManagerIF` は
    `interface { Validate(ctx, rawToken) (authClaims-relevant fields, error) }` 形の structural
    typing 用 interface を httpserver 内で再宣言（`auth.SessionManager` を直接 import せず、
    `Validate(ctx, raw) (tenantID uuid.UUID, adminUserID uuid.UUID, roles []string,
    isSuperAdmin bool, audience string, error)` で受ける軽量 IF）。動作:
    - cookie `ae_session` が無ければ default-deny で next.ServeHTTP（後段の
      TenantContextMiddleware が 401 を返す。`/api/auth/*` 配下は cookie 不在でも通すための
      設計）
    - cookie あり → `Validate` → 失敗時は Set-Cookie Max-Age=-1 + 401 で chain 終端
    - 成功時 `withAuthClaims(ctx, authClaims{TenantID, AdminUserID, Roles, IsSuperAdmin,
      Audience})` を ctx に注入し next.ServeHTTP
  - `admin_middleware.go` の `RequireSuperAdmin` を改修:
    - `authClaimsFromContext` で authClaims を取得（不在なら 401）
    - `claims.Audience != "admin-console"` なら 403（**新規追加**、Req 5.4）
    - `!claims.IsSuperAdmin` なら 403（既存）
    - すべて pass なら next。body には対象リソース ID / path を含めない（Req 5.7 / 6.1〜6.3）
    - 認可拒否時の構造化ログには `reason` / `actor_id` / `path` を field 化し、`resource_id`
      は載せない（SuperAdmin 操作時のみ resource_id を log に含める、NFR 5.2）
  - `server.go` の `Routers` struct に `AdminAuth chi.Router` を追加し、
    `NewServer(cfg, log, pool, sessMgr SessionManagerIF) (*http.Server, Routers, error)` に
    signature 変更。`sessMgr=nil` 許容（nil なら SessionAuthMiddleware を Use せず A2 互換動作）。
    `/api/admin/*` 配下の SuperAdmin ガード適用範囲から `/api/admin/auth/*` を除外する
    （`apiRouter.Mount("/admin/auth", ...)` 用の別 router を `Routers.AdminAuth` で公開、
    既存 `Routers.Admin` の chain に SuperAdmin ガードを残しつつ、auth 経路は SuperAdmin 不要で
    通せるようにする）。**`Routers.API` / `Routers.Admin` の chain の先頭** に
    `SessionAuthMiddleware(sessMgr, log)` を追加
  - `backend/cmd/api/main.go` の bootstrap に以下を配線:
    - `authRepo := auth.NewRepository(pool)`
    - `verifierSet, err := oidc.NewVerifierSet(ctx, cfg)` （err は fail-fast）
    - `sessMgr := auth.NewSessionManager(authRepo, cfg.SessionIdleMinutes, cfg.SessionAbsoluteHours, pool)`
    - `authSvc := auth.NewService(verifierSet, authRepo, sessMgr, auth.NoopRecorder{}, pool)`
    - `srv, routers, _ := httpserver.NewServer(cfg, log, pool, sessMgr)`
    - `routers.API.Mount("/auth", auth.Routes(authSvc, sessMgr, oidc.AudienceTenant,
      cfg.OIDCTenantRedirectURL))`
    - `routers.AdminAuth.Mount("/auth", auth.Routes(authSvc, sessMgr, oidc.AudienceAdmin,
      cfg.OIDCAdminRedirectURL))`
  - **同 task 内テスト**:
    - `session_auth_test.go`: cookie 不在で素通し / cookie + Validate 成功で authClaims 注入 /
      Validate 失敗で 401 + Set-Cookie Max-Age=-1（Req 3.8 / 3.9 / NFR 3.3）
    - `admin_middleware_test.go` 拡張: aud=tenant-console + IsSuperAdmin=true の 403 ケース
      追加（Req 5.4）、aud=admin-console + IsSuperAdmin=true の 200 ケース、aud="" の 401 ケース
      （authClaims 不在）、403 ログに `resource_id` が含まれないこと（NFR 5.2）の assert
    - `server_test.go` 拡張: `NewServer(..., nil)` で A2 互換動作（既存 test 不変）、
      `NewServer(..., mockSessMgr)` で `/api/auth/login` が SuperAdmin ガード対象外で通過、
      `/api/admin/auth/login` も SuperAdmin ガード対象外で通過（Req 1.1 / 1.3）、
      `/api/admin/<mock>` は SuperAdmin ガード適用（Req 5.1）
  - _Requirements: 1.1, 1.3, 3.5, 3.8, 3.9, 5.1, 5.2, 5.3, 5.4, 5.5, 5.6, 5.7, 6.1, 6.2, 6.3, NFR 3.3, NFR 5.2_
  - _Boundary: SessionAuthMiddleware, RequireSuperAdmin, HTTPServer_
  - _Depends: 4, 5_

- [ ] 7. 結合テスト（OIDC callback E2E + admin route authz + role change rollback）
  - `backend/test/integration/auth_oidc_session_test.go` を新規追加。`httptest` で
    JWKS endpoint + token endpoint をホストする OIDC stub を立て、`docker compose up -d
    postgres` の DB に対して migrations 0001-0013 を適用した状態で:
    - (a) GET /api/auth/login → 302 to OIDC stub authorize URL（client_id=tenant-console）
    - (b) callback URL を直接叩く（state cookie を pre-set）→ Set-Cookie ae_session + 302
    - (c) 当該 cookie で `/api/<mock test route>` に GET → 200（authClaims が注入されている）
    - (d) idle 31 分相当の時刻進行（`SessionManager` を test 用の `now()` 差替えで testクロック
      化）→ 同 cookie で 401（NFR 3.1）
    - (e) absolute 8h1m 経過 → 401（NFR 3.2）
    - (f) POST /api/auth/logout → 204 + Set-Cookie Max-Age=-1 → 同 cookie 再使用で 401
      （NFR 3.3）
    - (g) aud=admin-console 経路で同様のフローを `/api/admin/auth/login`〜 で実行
  - `backend/test/integration/authz_admin_route_test.go` を新規追加:
    - (a) aud=tenant-console + role=SuperAdmin の session で `/api/admin/<mock>` → 403
      （Req 5.4。aud mismatch）
    - (b) aud=admin-console + role=SuperAdmin の session で同 endpoint → 200
    - (c) aud=admin-console + role=Viewer の session で同 endpoint → 403（Req 5.5）
    - (d) 未認証 cookie 無しで同 endpoint → 401（Req 5.6）
    - (e) `/api/admin/auth/login` は cookie 無しでも 302（SuperAdmin ガード対象外、Req 1.3）
  - `backend/test/integration/auth_role_change_test.go` を新規追加:
    - (a) AuthService.ChangeRole で正常系: `admin_role_assignments` が更新され、Recorder
      （test fake）が `RoleChangeEvent` を 1 件受信
    - (b) Recorder が err を返した場合: `admin_role_assignments` の UPDATE が rollback され、
      422 で `CodeBusinessRule` を受信（Req 7.3）
  - DB 接続が確立できない CI 環境では各 integration test が `t.Skip` する（A2 と同じ慣習）
  - _Requirements: 1.1, 1.3, 1.4, 1.5, 1.6, 1.7, 3.5, 3.6, 3.7, 3.8, 3.9, 5.1, 5.4, 5.5, 5.6, 7.1, 7.3, NFR 3.1, NFR 3.2, NFR 3.3_
  - _Boundary: AuthHandler, SessionManager, RequireSuperAdmin, AuthService_
  - _Depends: 6_

## Verify

本 spec の実装後、watcher（stage-a-verify gate）が再実行すべき verify コマンドを構造化
ブロックで宣言する。A2 と同じく integration test は `DATABASE_URL` 接続が必要だが、未接続の
環境では各 integration test が自身で `t.Skip` する設計のため、DB 不在環境でも verify は
false-fail しない。

<!-- stage-a-verify -->
```sh
cd backend && go build ./... && go vet ./... && go test ./...
```
