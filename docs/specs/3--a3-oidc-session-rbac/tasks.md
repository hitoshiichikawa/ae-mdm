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
  - `backend/internal/config/config.go` に `SessionIdleMinutes int`（default 30）、
    `SessionAbsoluteHours int`（default 8）、`OIDCTenantPostLoginAllowedPrefixes []string`、
    `OIDCAdminPostLoginAllowedPrefixes []string` を追加
  - `backend/internal/config/env.go` に `SESSION_IDLE_MINUTES` / `SESSION_ABSOLUTE_HOURS` の
    `intWithDefault` 呼び出しを追加（int 解析失敗で `CodeConfigInvalid`、欠落時は default）。
    `OIDC_TENANT_POST_LOGIN_ALLOWED_PREFIXES` / `OIDC_ADMIN_POST_LOGIN_ALLOWED_PREFIXES` は
    カンマ区切り `[]string` パースで読み込み、空 / 未指定なら対応する `OIDCTenantRedirectURL` /
    `OIDCAdminRedirectURL` の `Scheme://Host/` 部を default として 1 要素 slice にセット
  - `.env.example` に同 4 行を追加（コメントで idle / absolute timeout と open redirect 防止
    allowlist の意味を記載）
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
    の default 適用と int 解析失敗の test ケースを追加。`OIDC_*_POST_LOGIN_ALLOWED_PREFIXES`
    の (a) カンマ区切り複数値、(b) 空 → RedirectURL ホスト派生 default の 2 ケースを追加。
    `errors_test.go` に `CodeOIDCInvalid` / `CodeUnknownRole` が HTTP 401 にマップされる test と、
    403 / 404 body が `{code, message, request_id}` のみで対象 ID を含まないことの regression test
    （Req 6.2 / 6.3）を追加。`logger_test.go` に新規 allowlist 3 件の redaction test を追加
  - _Requirements: 1.5, 2.5, 6.2, 6.3, NFR 5.1_
  - _Boundary: Config, Errors, Logger, Migration0013_

- [ ] 2. OIDC Verifier + VerifierSet + Token Exchange + Role Mapping
  - **Audience 型の置き場**: `Audience` 型と `AudienceTenant` / `AudienceAdmin` 定数は
    `backend/internal/platform/authz/roles.go` に置く（task 3 で実装）。oidc に置くと
    role_mapping.go が authz を import する関係と合わせて `oidc ↔ authz` の import cycle に
    なるため
  - `backend/internal/platform/oidc/verifier.go` に `Claims` struct（Subject / Email / Groups /
    Roles / TenantID / Issuer / Audience (`authz.Audience`) / ExpiresAt）と `Verifier` interface
    （`Verify(ctx, rawIDToken, expectedAud authz.Audience) (Claims, error)` /
    `ExchangeCode(ctx, code, redirectURL, codeVerifier string) (rawIDToken, error)` /
    `AuthorizeURL(state, codeChallenge string) string`）を定義。**Claims.Roles** は標準
    `roles` claim を、**Claims.Groups** は Keycloak `groups` claim を保持
  - `verifier_set.go` に `VerifierSet` 構造体と `NewVerifierSet(ctx, cfg) (*VerifierSet, error)`
    + `For(aud authz.Audience) Verifier` を実装。tenant / admin の 2 OIDC Provider に対し
    `coreos/go-oidc/v3` の `oidc.NewProvider` + `oidc.NewVerifier` を構築（OIDC discovery 失敗時は
    fail-fast）。`oauth2.Config` を内部に保持し ExchangeCode / AuthorizeURL を委譲
  - `role_mapping.go` に `MapClaimsToRoles(c Claims) []authz.Role` を実装。**優先順位**: (1)
    `c.Roles` が非空なら `roles` claim を写像、(2) なければ `c.Groups` を写像（Req 4.2 は
    「groups または roles claim」を求めるため、どちらの IdP にも対応する）。Keycloak の
    `/SuperAdmin` / `/TenantAdmin` / `/Operator` / `/Viewer`（groups）と素の `SuperAdmin` /
    `TenantAdmin` / `Operator` / `Viewer`（roles）の両方をサポート。未知のみで空 slice 返却
    → Req 4.12 で session 発行拒否のシグナル。authz package への依存方向は OK
    （oidc → authz を許す。逆方向は禁止）
  - `token_exchange.go` に `ExchangeCode` の本体（`oauth2.Config.Exchange` で `code_verifier`
    を `oauth2.SetAuthURLParam("code_verifier", codeVerifier)` 経由で送出 + 戻り
    `id_token` 抽出）
  - 検証失敗時は `*errors.Error{Code: CodeOIDCInvalid}`、Cause に `invalid_signature` /
    `invalid_issuer` / `invalid_audience` / `token_expired` の判別文字列を含める
  - **同 task 内テスト**: `verifier_test.go` で `httptest` ベースの mock OIDC server
    （discovery + JWKS + token endpoint）を立て、(a) 正常系（aud 一致）/ (b) 署名不正 /
    (c) iss 不一致 / (d) aud 不一致（tenant-console token を expectedAud=admin-console で
    reject）/ (e) exp 切れ の 5 分岐を網羅。`role_mapping_test.go` で (i) Keycloak 形式 groups
    のみ / (ii) 標準 roles のみ / (iii) 両方混在 / (iv) 未知のみ / (v) 部分的に未知 の
    5 ケースで MapClaimsToRoles を verify
  - _Requirements: 2.1, 2.2, 2.3, 2.4, 2.5, 2.6, 2.7, 2.8, 2.9, 4.2, NFR 2.1, NFR 2.2_
  - _Boundary: OIDCVerifier, VerifierSet, RoleMapping, TokenExchanger_
  - _Depends: 1, 3_  # Audience 型が task 3 で authz package に定義されるため

- [ ] 3. RBAC Authorizer + PermissionsMatrix + Require chi middleware + Audience 型
  - `backend/internal/platform/authz/roles.go` に `Role` 型と `SuperAdmin` / `TenantAdmin` /
    `Operator` / `Viewer` 定数（Req 4.1）に加え、**`Audience` 型と `AudienceTenant` /
    `AudienceAdmin` 定数**を同 file に置く（oidc / auth / httpserver からの import cycle を
    避けるため最下層 authz package に集約）
  - `actions.go` に `Action` 型（`read` / `create` / `update` / `delete` / `command:lock` /
    `command:reboot` / `command:wipe`）と `ResourceType` 型（`tenant` / `admin_user` /
    `policy` / `device` / `command` / `app` / `audit_log` / `enrollment_token` /
    `audit_log_cross_tenant`）と、**`IsTenantScoped(r ResourceType) bool`**（tenant /
    audit_log_cross_tenant は false、その他は true を返すヘルパ）
  - `permissions.go` に `Matrix map[Role]map[Action]map[ResourceType]bool` を `init()` で
    構築（4 × 7 × 9 = 252 セルすべて bool 明示。partial fill 禁止 = fail-closed）。
    **SuperAdmin 行は requirements.md AC 4.11 と整合**: device / policy / command (lock/reboot/wipe)
    / admin_user / audit_log の各 action は **すべて許可**（cross-tenant 経路は Authorize の
    audience guard で制御）。Authorizer interface + `DefaultAuthorizer` 実装
    （`Authorize(p Principal, action, resource, targetTenantID) error` /
    `PermissionsFor(role) map[Action]map[ResourceType]bool`）。Authorize の判定順序（design.md
    Authorize 節と整合）:
    1. Matrix 引きで 1 role でも true が引ければ pass、なければ 403
    2. `IsTenantScoped(resource) && !p.IsSuperAdmin` の場合: `targetTenantID == uuid.Nil` で
       **403**（fail-closed、空入力許可不可）。`targetTenantID != p.TenantID` でも 403
    3. `p.IsSuperAdmin && targetTenantID != uuid.Nil && targetTenantID != p.TenantID`（実質
       cross-tenant 経路）の場合: **`p.Audience == authz.AudienceAdmin` を AND チェック**。
       不一致なら 403（Req 5.8。aud=tenant-console + SuperAdmin token の cross-tenant 越境を阻止）
    4. すべて pass → nil
  - `middleware.go` に `Require(authz Authorizer, action Action, resource ResourceType)
    func(http.Handler) http.Handler` を実装。chi middleware として最外層から
    `principalFromCtx` 関数 var で principal 取得 → Authorize → 失敗時
    `*errors.Error{Code: CodeForbidden}` で 403。403 応答 body は `errors.WriteHTTP` 経由で
    `{code, message, request_id}` のみを返し、対象 resource_id / path を含めない
    （Req 6.1 / 6.2 / 6.3）
  - 注意: 本 task の `principalFromCtx` injection point（パッケージレベル変数）を用意し、
    task 4 で `auth` package の `init` から書き込む（authz → auth の direct import を避ける）
  - **同 task 内テスト**: `permissions_test.go` で 4 role × 7 action × 9 resource の 252 セル
    全網羅の表駆動テスト。SuperAdmin の device read / policy create-update-delete /
    command:lock|reboot|wipe / admin_user manage / audit_log_cross_tenant 許可 を explicit
    ケースとして追加（requirements.md AC 4.11 整合）。`Operator` の `command:wipe` 拒否、
    `Viewer` の write/command 全拒否、`TenantAdmin` の cross-tenant 拒否、`SuperAdmin` の
    `audit_log_cross_tenant` 許可を explicit ケースとして追加（Req 4.5〜4.11, 5.8）。
    **追加ケース**: (a) TenantAdmin / Operator / Viewer が `targetTenantID=uuid.Nil` で
    tenant-scoped resource を要求 → 403、(b) SuperAdmin が aud=tenant-console で
    `targetTenantID != p.TenantID` の cross-tenant 要求 → 403（Req 5.8）。
    `middleware_test.go` で chi middleware の 403 / 200 経路を verify。403 body に対象
    resource_id / path が含まれないことの assertion を含める（Req 6.1 / 6.2 / 6.3）
  - _Requirements: 4.1, 4.3, 4.4, 4.5, 4.6, 4.7, 4.8, 4.9, 4.10, 4.11, 5.2, 5.4, 5.8, 6.1, 6.2, 6.3, NFR 4.1, NFR 4.2_
  - _Boundary: Authorizer, PermissionsMatrix, RequireMiddleware, Audience_
  - _Depends: 1_

- [ ] 4. AuthRepository + SessionManager + Principal
  - `backend/internal/auth/principal.go` に `Principal` struct（AdminUserID / TenantID /
    Roles `[]authz.Role` / IsSuperAdmin / Audience `authz.Audience`）と
    `PrincipalFromContext(ctx) (Principal, error)` / `WithPrincipal(ctx, p) context.Context` を実装。
    同 file の `init()` で `authz.principalFromCtx`（task 3 で用意した injection point）に登録
    し、`authz.Require` から本 package の Principal を取得可能にする
  - `repository.go` に `AuthRepository` interface（UpsertAdminUser / ReplaceRoles /
    InsertSession / FindSessionByHash / UpdateSessionIdle / DeleteSession /
    FindRolesForAdminUser）と `pgxRepository` 実装。全 method は `pgx.Tx` を引数で受ける
    （pool 直接アクセス禁止）。`sessions.aud` カラムを INSERT / SELECT に含める
  - `session_token.go` に opaque token 生成（`crypto/rand` で 32B → `base64.RawURLEncoding`）と
    sha256 ハッシュヘルパ
  - `session_manager.go` に `SessionManager` interface（`Issue(ctx, tx pgx.Tx, adminUserID,
    aud authz.Audience) (*http.Cookie, error)` / `Validate(ctx, rawToken) (Principal, error)` /
    `Revoke(ctx, rawToken) error`）と `DefaultSessionManager` 実装:
    - **Issue: pgx.Tx を引数で受け、呼び出し側 (AuthService.HandleCallback) の outer tx に
      join する**（旧 design では Issue 内部で別 tx を開いていたため、未 commit な
      admin_users 行を新規 tx から FK 参照できず原子性が崩れる事故があった。本 spec で解消）。
      `InsertSession(tx, SessionRow{IdleAt: now+IdleMinutes, ExpiresAt: now+AbsoluteHours,
      Audience: aud, ...})` を呼び、`http.Cookie{Name:"ae_session", Value: rawToken,
      HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode, Path:"/"}` を返す
      （NFR 1.1 / 1.2 / 1.3）。**idle_at / expires_at は deadline timestamp として保存**
    - Validate: SessionManager 内で `BeginTxFunc(WithTenantContext(ctx, IsSuperAdmin=true),
      pool, fn)` で短い独立 tx を開き、その中で `FindSessionByHash` を実行。判定式は
      **`now > expires_at`**（absolute 失効） または **`now > idle_at`**（idle 失効、idle_at
      自体が deadline）で `DeleteSession` + 401。成功時は **`UpdateSessionIdle(hash,
      now+IdleMinutes)`**（deadline を再延長）→ `FindRolesForAdminUser` → Principal 組み立て
      （Audience は sessions.aud を継承）
    - Revoke: 内部で短い独立 tx を開き `DeleteSession`
    - NewSessionManager(repo, pool, idleMinutes, absoluteHours) で pool を保持する
  - cookie 値の生値は logger に出力しない（NFR 5.1）。Validate 失敗 log には sha256 hash の
    先頭 8 文字のみを `session_hash_prefix` field で出す
  - **同 task 内テスト**: `session_token_test.go` で base64 長さ 43 文字 / hash の決定性 verify。
    `session_manager_test.go` で in-memory mock repository を使い:
    - Issue は呼び出し側から渡された mock tx を使う（別 tx を開かないこと）の verify
    - Issue 後の cookie 属性（HttpOnly / Secure / SameSite=Lax / Path=/）全 verify
      （Req 3.2〜3.4 / NFR 1.1〜1.3）
    - sessions 行の `idle_at = now+IdleMin` / `expires_at = now+AbsoluteHours` で INSERT
      されることを mock で verify
    - Validate の idle 境界（29 / 30 / 31 分の 3 ケース、NFR 3.1）— **`now > idle_at` の
      判定**（30 分目で fail、31 分目で fail、29 分目で pass）
    - Validate の absolute 境界（7h59m / 8h / 8h1m、NFR 3.2）
    - Validate 成功時に `UpdateSessionIdle(hash, now+IdleMin)` が呼ばれる（idle_at が
      `now+IdleMin` に refresh される）ことを mock で verify（Req 3.5）
    - Revoke 後の Validate が `CodeUnauthenticated` を返す（NFR 3.3 / Req 3.9）
  - _Requirements: 2.5, 3.1, 3.2, 3.3, 3.4, 3.5, 3.6, 3.7, 3.8, 3.9, NFR 1.1, NFR 1.2, NFR 1.3, NFR 3.1, NFR 3.2, NFR 3.3, NFR 5.1_
  - _Boundary: AuthRepository, SessionManager, Principal_
  - _Depends: 1, 3_

- [ ] 5. AuthService + AuthHandler + audit emitter IF
  - `backend/internal/auth/audit_emitter.go` に `RecorderInterface` interface
    （`Record(ctx, tx pgx.Tx, ev RoleChangeEvent) error` — outer tx に join するため
    `pgx.Tx` を引数で受ける）と `NoopRecorder` 実装、`RoleChangeEvent` struct
    （ActorID / TargetAdminUserID / FromRoles `[]authz.Role` / ToRoles `[]authz.Role` /
    OccurredAt / Source `"login_sync" | "admin_change"`）を定義
  - `service.go` に `Service` interface（`HandleCallback(ctx, code, aud authz.Audience,
    redirectURL, codeVerifier, postLoginRedirect string) (*http.Cookie, postLoginURL, error)` /
    `ChangeRole(ctx, target, newRole) error` / `Logout(ctx, rawToken) error`）と実装。
    HandleCallback:
    1. `Verifier.ExchangeCode(code, redirectURL, codeVerifier)` → rawIDToken
    2. `Verifier.Verify(rawIDToken, expectedAud)` → Claims（失敗時 `CodeOIDCInvalid`）
    3. `oidc.MapClaimsToRoles(Claims)` → []authz.Role（空ならば `CodeUnknownRole`、Req 4.12）
    4. **Tenant ID 解決**（design.md「Tenant ID resolution rule」と整合）:
       - `Claims.TenantID` が UUID parse 成功 → そのまま採用
       - aud=admin-console かつ roles に SuperAdmin → `uuid.Nil`
       - 上記いずれでもない → `CodeOIDCInvalid` (Cause: `missing_tenant_id`) で 401
    5. **post_login_redirect の allowlist 検証**: 受け取った `postLoginRedirect` が
       config の `OIDC{Tenant,Admin}PostLoginAllowedPrefixes` のいずれかに `HasPrefix` で
       マッチしなければ 400 (`invalid_redirect`)。空 / 未指定なら default landing page に
       fallback
    6. `BeginTxFunc(WithTenantContext(ctx, db.TenantContext{IsSuperAdmin:true}), pool, fn)`
       の **outer tx 内**で:
       - `UpsertAdminUser(tx, oidc_sub, email, resolvedTenantID)` で id 確保
       - `FindRolesForAdminUser(tx, adminUserID)` で既存 roles を取得
       - 新 roles ↔ 既存 roles の差分を計算
       - `ReplaceRoles(tx, adminUserID, newRoles, resolvedTenantID)` で同期
       - **差分があれば** `Recorder.Record(tx, RoleChangeEvent{Source:"login_sync", FromRoles:
         existing, ToRoles: new, ActorID: adminUserID（self-sync）})` を **同一 tx 内**で呼ぶ
         （Req 7.1 / 7.3 / login-time role sync の監査ログ漏れ防止）。Record が err を返したら
         rollback + 500（login 全体を未確定として扱う）
       - `SessionManager.Issue(ctx, tx, adminUserID, expectedAud)` で cookie 発行（tx 引数渡し）
    7. cookie を返し、validated post_login_redirect を URL として返す
  - ChangeRole: tx 内で `admin_role_assignments` UPDATE → `Recorder.Record(tx,
    RoleChangeEvent{Source:"admin_change", ActorID: principal.AdminUserID})` を同一 tx 内で
    呼ぶ。err なら rollback + `CodeBusinessRule` 422（Req 7.3）
  - `handler.go` に `Routes(svc Service, sessMgr SessionManager, aud authz.Audience,
    allowedRedirectPrefixes []string) chi.Router` を実装（aud と allowlist は呼び出し側が
    tenant / admin で個別指定する設計に統一）。endpoints:
    - GET /login: **PKCE code_verifier / code_challenge を backend で生成**（`crypto/rand` で
      32B → base64url、SHA-256 で challenge）。state nonce 生成。state cookie 発行
      （HMAC-SHA256(SessionSecret, state||code_verifier) で MAC、cookie value に
      `<state>|<code_verifier>` を MAC 付き encoding、HttpOnly + Secure + SameSite=Lax +
      5 分 Max-Age）→ Verifier.AuthorizeURL(state, code_challenge) で 302
    - GET /callback: state cookie から state / code_verifier 取り出し + MAC 検証 →
      `svc.HandleCallback(code, aud, redirectURL, codeVerifier, query.post_login_redirect)` →
      cookie 設定 + 302 to validated post_login_redirect
    - POST /logout: cookie から rawToken 取得 → svc.Logout → Set-Cookie Max-Age=-1 + 204
    - GET /session: PrincipalFromContext から SessionInfo JSON 応答（admin_user_id + roles +
      tenant_id + aud。GET /session 経路は SessionAuthMiddleware が事前に Principal を
      authClaims に注入する想定のため、`routers.API` 配下に別 mount するか、Routes 内で
      cookie 検証を自前で行う方式は task 6 で確定）
  - **同 task 内テスト**: `service_test.go` で mock OIDCVerifier / mock AuthRepository /
    mock SessionManager / mock Recorder で:
    - HandleCallback 正常系（aud=tenant-console と aud=admin-console の 2 経路、tenant_id
      claim 経路）
    - SuperAdmin + aud=admin-console + tenant_id 空 claim → tenant_id=Nil で session 発行
    - tenant_id 解決失敗 → 401 (`missing_tenant_id`)、SessionManager.Issue 未呼び出し
    - post_login_redirect allowlist 外 → 400 (`invalid_redirect`)、Issue 未呼び出し
    - OIDC verify 失敗時 SessionManager.Issue が呼ばれないこと（NFR 2.1 / Req 1.7 / 2.6〜2.9）
    - MapClaimsToRoles 結果が empty で `CodeUnknownRole`（Req 4.12）
    - **role 差分発生時に Recorder.Record(Source="login_sync") が outer tx の同一 tx 引数で
      呼ばれること**を mock で verify（Req 7.1 ログイン時 role 同期の監査確認）
    - Recorder.Record が err → rollback で ReplaceRoles も無効化される（mock side-effect 確認）
    - **SessionManager.Issue が AuthService の outer tx と同じ pgx.Tx インスタンスを受け取る
      ことを mock で verify**（旧 design の nested tx 問題が再発しないことの regression test）
    - ChangeRole で Recorder.Record が err を返すと admin_role_assignments の UPDATE が
      rollback され、`CodeBusinessRule` 422 が返ること（Req 7.3）
    - Logout 経路で SessionManager.Revoke が呼ばれること
  - `handler_test.go` で /login の state + code_verifier cookie 発行 + Location ヘッダ
    （code_challenge query 含む）、/callback の state / code_verifier 検証 / Set-Cookie /
    302 リダイレクト、/callback で post_login_redirect=allowlist 外 → 400 を `httptest`
    ベースで verify。/logout の 204 + Set-Cookie Max-Age=-1
  - _Requirements: 1.1, 1.2, 1.3, 1.4, 1.5, 1.6, 1.7, 2.6, 2.7, 2.8, 2.9, 4.12, 7.1, 7.2, 7.3, NFR 2.1, NFR 5.1_
  - _Boundary: AuthService, AuthHandler, AuditEmitterIF_
  - _Depends: 2, 4_

- [ ] 6. httpserver 拡張（SessionAuthMiddleware + RequireSuperAdmin aud チェック）+ cmd/api 配線
  - `backend/internal/platform/httpserver/middleware.go` の既存 `authClaims` struct に
    `Audience string` field を追加（既存 test との互換は zero value=""）
  - `session_auth.go`（新規）に **`type AuthClaims struct`**（primitive 型のみ: TenantID
    uuid.UUID / AdminUserID uuid.UUID / Roles []string / IsSuperAdmin bool / Audience string。
    export type）と **`type SessionValidatorFunc func(ctx context.Context, rawToken string)
    (AuthClaims, error)`** を export し、`SessionAuthMiddleware(validate SessionValidatorFunc,
    log logger.Logger) func(http.Handler) http.Handler` を実装。`auth.SessionManager` を
    直接 receive しない理由は、SessionManager.Validate が `auth.Principal`（authz.Role /
    authz.Audience を含む）を返すため、`httpserver` package が `auth` / `authz` を import
    せざるを得なくなる（旧 design の structural-typing IF では Validate のタプル型が
    SessionManager.Validate（戻り `(Principal, error)`）と型不一致を起こしコンパイル不能に
    なる事故があった。本 spec の adapter 関数化で解消）。動作:
    - cookie `ae_session` が無ければ **authClaims 未注入のまま next.ServeHTTP に進む**
      （401 で chain 終端しない）。後段の `TenantContextMiddleware` が default-deny で
      authClaims 不在を 401 に倒すため、`/api` / `/api/admin` 配下は結果的に 401 になる。
      `/api/auth/*` / `/api/admin/auth/*` は `Routers.APIAuth` / `Routers.AdminAuth` 経由で
      mount され本 middleware の chain に乗らないため、cookie 不在でログイン経路に到達できる
    - cookie あり → `validate(ctx, raw)` → 失敗時は Set-Cookie Max-Age=-1 + 401 で chain 終端
    - 成功時 `withAuthClaims(ctx, authClaims{TenantID, AdminUserID, Roles, IsSuperAdmin,
      Audience})` を ctx に注入し next.ServeHTTP
  - `admin_middleware.go` の `RequireSuperAdmin` を改修:
    - `authClaimsFromContext` で authClaims を取得（不在なら 401）
    - `claims.Audience != "admin-console"` なら 403（**新規追加**、Req 5.4）
    - `!claims.IsSuperAdmin` なら 403（既存）
    - すべて pass なら next。body には対象リソース ID / path を含めない（Req 5.7 / 6.1〜6.3）
    - 認可拒否時の構造化ログには `reason` / `actor_id` / `path` を field 化し、`resource_id`
      は載せない（SuperAdmin 操作時のみ resource_id を log に含める、NFR 5.2）
  - `server.go` の `Routers` struct に **`APIAuth chi.Router` と `AdminAuth chi.Router`** の
    2 fields を追加（API 系 / Admin 系の auth-route 専用、SessionAuthMiddleware /
    TenantContextMiddleware / RequireSuperAdmin の全対象外）。
    `NewServer(cfg, log, pool, validator SessionValidatorFunc) (*http.Server, Routers, error)` に
    signature 変更。`validator=nil` 許容（nil なら SessionAuthMiddleware を Use せず A2 互換動作）。
    chi 配線:
    - root: `apiRouter := chi.NewRouter()`、`r.Mount("/api", apiRouter)`
    - APIAuth: `apiAuthRouter := chi.NewRouter()`（middleware 一切無し）、
      `apiRouter.Mount("/auth", apiAuthRouter)` → `Routers.APIAuth = apiAuthRouter`
    - AdminAuth: `adminAuthRouter := chi.NewRouter()`（middleware 一切無し）、
      `apiRouter.Mount("/admin/auth", adminAuthRouter)` → `Routers.AdminAuth = adminAuthRouter`
    - API: `apiBaseRouter.Use(SessionAuthMiddleware(validator, log))` +
      `apiBaseRouter.Use(TenantContextMiddleware)` を Use し、`apiRouter.Mount("/", apiBaseRouter)`
      → `Routers.API = apiBaseRouter`
    - Admin: 同 middleware chain + `RequireSuperAdmin` を加え `apiRouter.Mount("/admin",
      adminBaseRouter)` → `Routers.Admin = adminBaseRouter`
    - **path 解決順序**: chi は具体的 prefix が抽象的 prefix より先にマッチするため、
      `/api/auth` および `/api/admin/auth` が APIAuth / AdminAuth に解決され
      SessionAuthMiddleware 等を経由しないことを server_test.go の spy で verify
  - `backend/cmd/api/main.go` の bootstrap に以下を配線:
    - `authRepo := auth.NewRepository(pool)`
    - `verifierSet, err := oidc.NewVerifierSet(ctx, cfg)`（err は fail-fast）
    - `sessMgr := auth.NewSessionManager(authRepo, pool, cfg.SessionIdleMinutes, cfg.SessionAbsoluteHours)`
    - `authSvc := auth.NewService(verifierSet, authRepo, sessMgr, auth.NoopRecorder{}, pool)`
    - **adapter 関数**: `validator := func(ctx context.Context, raw string)
      (httpserver.AuthClaims, error) { p, err := sessMgr.Validate(ctx, raw); if err != nil
      { return httpserver.AuthClaims{}, err }; return httpserver.AuthClaims{TenantID: p.TenantID,
      AdminUserID: p.AdminUserID, Roles: rolesToStrings(p.Roles), IsSuperAdmin: p.IsSuperAdmin,
      Audience: string(p.Audience)}, nil }`（auth.Principal → httpserver.AuthClaims の
      primitive 化アダプタ。`httpserver` が `auth` / `authz` を import しないための層）
    - `srv, routers, _ := httpserver.NewServer(cfg, log, pool, validator)`
    - `routers.APIAuth.Mount("/", auth.Routes(authSvc, sessMgr, authz.AudienceTenant,
      cfg.OIDCTenantPostLoginAllowedPrefixes))`
    - `routers.AdminAuth.Mount("/", auth.Routes(authSvc, sessMgr, authz.AudienceAdmin,
      cfg.OIDCAdminPostLoginAllowedPrefixes))`
  - **同 task 内テスト**:
    - `session_auth_test.go`: cookie 不在で素通し（next.ServeHTTP が呼ばれ status 200）/
      cookie + validate 成功で authClaims 注入 / validate 失敗で 401 + Set-Cookie Max-Age=-1
      （Req 3.8 / 3.9 / NFR 3.3）
    - `admin_middleware_test.go` 拡張: aud=tenant-console + IsSuperAdmin=true の 403 ケース
      追加（Req 5.4）、aud=admin-console + IsSuperAdmin=true の 200 ケース、aud="" の 401 ケース
      （authClaims 不在）、403 **body** に `resource_id` / `path` が含まれず **log** に
      `path` は含まれるが `resource_id` は含まれない（NFR 5.2 / Req 5.7 整合）の assert
    - `server_test.go` 拡張: `NewServer(..., nil)` で A2 互換動作（既存 test 不変）、
      `NewServer(..., mockValidator)` で:
      - `/api/auth/login` が SessionAuthMiddleware / TenantContextMiddleware を経由せず到達
        （cookie 無しで通る、Req 1.1）
      - `/api/admin/auth/login` も同じく cookie 無しで到達（SuperAdmin ガード対象外、Req 1.3）
      - `/api/admin/<mock>` は SuperAdmin ガード適用（Req 5.1）
      - mockValidator が `/api/auth/login` 経路で呼ばれないことを spy で verify
  - _Requirements: 1.1, 1.3, 3.5, 3.8, 3.9, 5.1, 5.2, 5.3, 5.4, 5.5, 5.6, 5.7, 6.1, 6.2, 6.3, NFR 3.3, NFR 5.2_
  - _Boundary: SessionAuthMiddleware, RequireSuperAdmin, HTTPServer, AuthClaims, SessionValidatorFunc_
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
