# Implementation Plan

> 本 Issue（#33）は ae-mdm の認証基盤（A3a: OIDC Verifier + Session 管理）を提供する。
> A2（Issue #2）の共通基盤（config / logger / errors / db / httpserver）の上に、
> `internal/platform/oidc/verifier.go` と `internal/auth/{types,state,session,repository,
> service,handler,middleware}.go` を新規追加し、`/api/auth/login` / `/api/auth/callback` /
> `/api/auth/logout` の 6 エンドポイント（tenant 系 3 + admin 系 3）を提供する。RBAC は
> 本 Issue のスコープ外（後続 Issue #?? の umbrella tasks 3.2）で、`AuthClaims.Roles` の
> クレーム伝播までを担う。
>
> 並列実行可能なタスクには `(P)` を付け、`_Boundary:_` で担当 Components を明示する。

- [ ] 1. config / migration / 共通公開化（後段の前提整備）
- [ ] 1.1 Config に session / state timeout を追加 (P)
  - `backend/internal/config/config.go` に `SessionIdleTimeout time.Duration`（default 30m）/
    `SessionAbsoluteTimeout time.Duration`（default 8h）/ `StateCookieTTL time.Duration`
    （default 10m）/ `StateMACSecret string`（required, len >= 32 bytes）を追加
  - `backend/internal/config/env.go` の既存パーサパターンに合わせて `duration` パーサ
    helper（または既存があれば再利用）と string（min length）バリデーションを追加
  - `backend/internal/config/config_test.go` に (a) default 値適用（3 つの duration が
    既定値）、(b) STATE_MAC_SECRET 未設定 / 32 文字未満で `*errors.Error{Code:
    config_invalid}`、(c) duration 不正フォーマットで `*errors.Error{Code: config_invalid}`、
    (d) 正常系で値が読み込まれることのテストを追加
  - `.env.example` に `SESSION_IDLE_TIMEOUT=30m` / `SESSION_ABSOLUTE_TIMEOUT=8h` /
    `STATE_COOKIE_TTL=10m` / `STATE_MAC_SECRET=<REPLACE_ME_GENERATE_32_BYTES_OF_RANDOM_HEX>`
    の 4 行を追加（管理者セッション暗号化用秘密鍵セクション直下）
  - _Requirements: NFR 2.1, NFR 2.2_
  - _Boundary: Config_
- [ ] 1.2 sessions テーブル拡張マイグレーション (P)
  - `backend/db/migrations/0013_extend_sessions.up.sql` を新規追加。`ALTER TABLE sessions ADD
    COLUMN last_seen_at timestamptz NOT NULL DEFAULT now()` → `UPDATE sessions SET last_seen_at
    = idle_at` → `ALTER TABLE sessions DROP COLUMN idle_at` → `ALTER TABLE sessions ADD
    COLUMN revoked_at timestamptz NULL` → `ALTER TABLE sessions ADD COLUMN console text NOT
    NULL DEFAULT 'tenant-console' CHECK (console IN ('tenant-console','admin-console'))` →
    `ALTER TABLE sessions ALTER COLUMN console DROP DEFAULT`
  - `backend/db/migrations/0013_extend_sessions.down.sql` を新規追加。対称な DROP / ADD
    （`idle_at` 復元、`revoked_at` / `console` DROP）。`idle_at` の値復元は `last_seen_at`
    からの UPDATE で対応
  - 既存 RLS ポリシー（`tenant_isolation_sessions`）は本マイグレーションで再定義しない
    （A2 で配置済みのまま、列追加のみ）
  - 結合テストは task 6 で `migrations_reversible_test.go` の対象に含まれる前提（本 task では
    手動で `make migrate-up && make migrate-down && make migrate-up` の整合性を文書化する
    のみ）
  - _Requirements: 3.7, 3.9, 4.1, 4.2, 4.3, 4.8, 5.1, 6.3_
  - _Boundary: Migrations_
- [ ] 1.3 httpserver の authClaims 関連シンボル公開化 (P)
  - `backend/internal/platform/httpserver/middleware.go` の private シンボルを以下に rename:
    `authClaims` → `AuthClaims`（フィールド構成は不変）、`withAuthClaims` →
    `WithAuthClaims`、`authClaimsFromContext` → `AuthClaimsFromContext`、
    `authClaimsCtxKey` は internal のまま維持
  - 同 package 内の test 呼び出し箇所（`middleware_test.go` / `admin_middleware_test.go` /
    `server_test.go`）を rename に合わせて修正
  - 既存挙動（default deny / 401 / 403 chain）は不変であること（test の期待値は変えない）
  - 後続 task（auth.Middleware）が `httpserver.WithAuthClaims(ctx, AuthClaims{...})` を
    package 外から呼び出せるようになる
  - _Requirements: 5.3, 5.4_
  - _Boundary: HTTPServer_
  - _Depends: なし（A2 完了済みのため独立）_
- [ ] 1.4 logger redaction allowlist 拡張 + ユニットテスト (P)
  - `backend/internal/logger/redact.go` の機密キー allowlist に以下 4 件を追加:
    `state_mac_secret` / `client_secret` / `state_cookie` / `session_cookie`
    （A2 既存 allowlist の `session_secret` / `id_token` / `access_token` / `refresh_token` /
    `cookie` / `google_application_credentials` / `sa_json` / `private_key` / `password` は
    そのまま保持）
  - `backend/internal/logger/logger_test.go` に追加 4 件の各 key が `***` に置換されること、
    任意の field 名サブストリング一致で redaction が発火することの単体テストを追加
  - これにより、後続 task で auth.Service / oidc.Verifier / auth.Middleware が構造化ログに
    `session_cookie=raw` のような field を **誤って**出した場合でも値が `***` に置換され、
    NFR 1.1 / NFR 4.2 / Req 1.11 / Req 3.6 の「生値・MAC 鍵をログに残さない」要件が
    多層防御として担保される（一次防御は各呼び出し側が hash / prefix のみ field 化する責務）
  - _Requirements: 1.11, 3.6, NFR 1.1, NFR 4.2_
  - _Boundary: Logger_
  - _Depends: なし（A2 完了済みのため独立）_

- [ ] 2. OIDC Verifier（JWKS キャッシュ + 検証）
- [ ] 2.1 oidc.Verifier 実装と単体テスト
  - `backend/internal/platform/oidc/verifier.go` を新規追加。`coreos/go-oidc/v3` の
    `oidc.NewProvider` + `oidc.NewRemoteKeySet` を tenant / admin の 2 issuer 分構築し、
    `Verifier` interface（`VerifyIDToken(ctx, raw) (Claims, error)`）を実装。aud 検証は
    本パッケージで明示実装（tenant / admin のいずれか **排他一致**を強制 / Req 1.4 / 1.5）
  - `Claims` 型に `Subject` / `Email` / `Groups` / `Issuer` / `MatchedConsole Console` を
    持たせる。raw JWT は **含めない**（Req 1.11）
  - 起動時 helper `NewVerifier(ctx, cfg config.Config) (Verifier, error)` を提供。
    discovery / JWKS prefetch 失敗時は `*errors.Error{Code: CodeUnavailable, failure_kind:
    oidc_discovery}` を返す（NFR 3.2）。後段（task 6.3）の bootstrap が `oauth2.Config` の
    Endpoint を構築できるよう、`TenantEndpoint() oauth2.Endpoint` / `AdminEndpoint()
    oauth2.Endpoint` を `Verifier` interface に併せて提供する
  - `backend/internal/platform/oidc/doc.go` を追加し、`internal/platform/oidc` は他 internal
    package（errors / config / logger 以外）を import しない（cycle 回避）旨と `auth` domain
    からのみ呼ばれる旨を godoc に記載
  - `backend/internal/platform/oidc/verifier_test.go` を新規追加。`httptest.NewServer` で
    OIDC discovery + JWKS endpoint を mock し、テスト用 RSA private key で署名した ID
    トークンを生成して以下を検証: (a) 正常系で aud=tenant-console を MatchedConsole に返す、
    (b) aud=admin-console も同様、(c) 署名検証失敗（鍵差替）で 401 + `failure_kind=
    invalid_sig`、(d) iss 不一致で `failure_kind=invalid_iss`、(e) aud 不一致で
    `failure_kind=invalid_aud`、(f) aud 配列に tenant+admin 同居で `failure_kind=
    aud_ambiguous`（Req 1.5）、(g) exp 切れで `failure_kind=token_expired`、(h) kid 不在で
    `failure_kind=invalid_kid`、(i) kid rotation（JWKS endpoint レスポンスを差し替え）後の
    再検証で成功復帰
  - _Requirements: 1.1, 1.2, 1.3, 1.4, 1.5, 1.6, 1.7, 1.8, 1.9, 1.10, 1.11, 6.1, 6.4, NFR 3.2, NFR 4.1_
  - _Boundary: OIDCVerifier_
  - _Depends: 1.1_

- [ ] 3. auth domain: 型 + state cookie + session cookie helpers
- [ ] 3.1 auth.types + state cookie helper + 単体テスト (P)
  - `backend/internal/auth/types.go` を新規追加。`Identity`（AdminUserID / OIDCSubject /
    Email / TenantID / Roles / IsSuperAdmin）と `Session`（TokenHash / AdminUserID / Console /
    IssuedAt / LastSeenAt / ExpiresAt / RevokedAt *time.Time）を定義
  - `backend/internal/auth/clock.go` を新規追加。`Clock interface { Now() time.Time }` と
    `SystemClock` 実装。Service / Middleware の DI で利用
  - `backend/internal/auth/state.go` を新規追加。`StatePayload`（Nonce / Console /
    RedirectKey / IssuedAt）と `Sign(payload, secret) (cookieValue string, err error)` /
    `Verify(cookieValue, queryState, secret, ttl, now) (StatePayload, error)` /
    `CookieAttributes(ttl) http.Cookie` を実装。MAC は HMAC-SHA256、cookie 値フォーマットは
    `base64url(payload) + "." + base64url(MAC)`、比較は `subtle.ConstantTimeCompare`
  - `backend/internal/auth/state_test.go` を新規追加。(a) Sign → Verify 往復、(b) MAC tamper
    で `*errors.Error{Code: CodeUnauthenticated, failure_kind: state_invalid}`、(c) TTL
    超過で `failure_kind: state_expired`、(d) nonce 改竄で `failure_kind: state_invalid`、
    (e) queryState と cookie state 不一致で `failure_kind: state_mismatch`、(f) cookie 不在
    （空文字）で `failure_kind: state_invalid`
  - `backend/internal/auth/doc.go` を新規追加。`internal/auth` の依存方向ルール（platform/oidc
    と platform/db / platform/httpserver / logger / errors / config のみ import 可）を記載
  - _Requirements: 2.1, 2.2, 2.3, 2.4, 2.5, 2.6, 2.7, 2.9, NFR 4.1_
  - _Boundary: AuthTypes, StateCookie, AuthClock_
  - _Depends: 1.1_
- [ ] 3.2 auth.session helper + 単体テスト (P)
  - `backend/internal/auth/session.go` を新規追加。`New() (rawToken string, err error)`
    （`crypto/rand.Read` で 32 byte → base64url no-padding）、`HashToken(raw) string`
    （SHA-256 hex）、`CookieAttributes() http.Cookie`（`__Host-ae_mdm_session` /
    HttpOnly / Secure / SameSite=Lax / Path=/）、`ExpireCookieAttributes() http.Cookie`
    （失効・logout 時の削除 cookie / Max-Age=0）、`HashPrefix(hash) string`（先頭 8 文字 /
    Req 3.8）を実装
  - `backend/internal/auth/session_test.go` を新規追加。(a) `New()` が 32 byte 相当の
    base64url 文字列を返す（長さ・文字種）、(b) `New()` を 100 回呼んで重複なし
    （乱数性）、(c) `HashToken` が SHA-256 hex（64 文字）を返す、(d) `HashToken` の冪等性、
    (e) `CookieAttributes` の Name / Secure / HttpOnly / SameSite / Path 値、(f)
    `ExpireCookieAttributes` の Max-Age=0 確認、(g) `HashPrefix` が 8 文字
  - _Requirements: 3.2, 3.3, 3.4, 3.5, 3.7, 3.8, 4.7, NFR 1.2_
  - _Boundary: SessionCookie_
  - _Depends: 1.1_

- [ ] 4. auth.Repository（sessions / admin_users CRUD）
- [ ] 4.1 Repository 実装 + integration テスト
  - `backend/internal/auth/repository.go` を新規追加。`Repository` interface（`UpsertAdminUser`
    / `Create` / `Get` / `Touch` / `Revoke`）と `pgxpool.Pool` ベースの実装を提供
  - すべての CRUD は `db.BeginTxFunc` 経由で実行する。`Get` / `Touch` / `Revoke` /
    `UpsertAdminUser` は **`SuperAdmin context`**（`db.WithTenantContext(ctx,
    db.TenantContext{IsSuperAdmin: true})` で前置）で呼ばれる前提（A2 design.md「sessions の
    認証 lookup 経路」散文と整合 / 確立 lookup は本 Issue では Service が wrap）
  - `Get(ctx, tokenHash)` は 0 行で `*errors.Error{Code: CodeUnauthenticated, failure_kind:
    session_tamper}` を返す（Req 5.4）。Session struct + Identity struct を join 取得（join 元
    は `sessions ↔ admin_users`、Identity.Roles は `admin_role_assignments` から groups 風に
    集約 / `IsSuperAdmin` は role に 'SuperAdmin' を含むかで判定）
  - `Touch` は `UPDATE sessions SET last_seen_at = $1 WHERE token_hash = $2`（expires_at 不変 /
    Req 4.8）
  - `Revoke` は `UPDATE sessions SET revoked_at = $1 WHERE token_hash = $2 AND revoked_at
    IS NULL`（冪等性 / Req 5.1）
  - `UpsertAdminUser(ctx, sub, email, console)` は `admin_users` に対する `INSERT ... ON
    CONFLICT (oidc_subject) DO UPDATE SET email = EXCLUDED.email RETURNING id, tenant_id` を
    SuperAdmin context で実行
  - `backend/test/integration/auth_repository_test.go` を新規追加。`docker compose up -d
    postgres` 前提（DATABASE_URL 未設定で skip）。シナリオ: (a) UpsertAdminUser の初回 INSERT
    と 2 回目 UPDATE、(b) Create → Get で hash 一致時に Session+Identity が返る、(c) Get で
    hash 不一致時に 0 行 → `session_tamper`、(d) Touch 後の last_seen_at 更新と expires_at
    不変、(e) Revoke 後の revoked_at セット、(f) Revoke 冪等性
  - _Requirements: 3.7, 3.9, 4.3, 4.6, 4.8, 5.1, 5.3, 5.4, 6.3_
  - _Boundary: AuthRepository_
  - _Depends: 1.2, 1.3, 3.1, 3.2_

- [ ] 5. auth.Service（4 ユースケース）+ auth.Handler（HTTP 6 endpoints）
- [ ] 5.1 Service 実装 + 単体テスト
  - `backend/internal/auth/service.go` を新規追加。`Service` interface（`BeginLogin` /
    `HandleCallback` / `LookupAndRefresh` / `Logout`）を提供
  - `BeginLogin(ctx, console, returnTo)` — `return_to` の検証（同一オリジン内相対 URL のみ
    許容 / 確認事項 3）、`StatePayload` 構築 → `state.Sign` → IdP 認可エンドポイント URL を
    `oauth2.Config.AuthCodeURL(state, oauth2.SetAuthURLParam(...))` で構築 → `(redirectURL,
    stateCookie, nil)` を返す
  - `HandleCallback(ctx, console, code, queryState, rawStateCookie)` — `state.Verify` →
    失敗時は対応する `failure_kind` で 401 → `oauth2.Config.Exchange(ctx, code)` で token
    取得（5xx は `*errors.Error{Code: CodeUpstream, failure_kind: upstream_oidc_token}`） →
    `verifier.VerifyIDToken` → `Claims.MatchedConsole != console` なら `failure_kind:
    invalid_aud` で拒否（Req 6.2 のクライアント分離強制） → `repo.UpsertAdminUser` →
    `session.New()` → `repo.Create` → `(rawSessionToken, sessionCookie, nil)`。state cookie の
    削除は Handler が行う（本 Service は session cookie のみ返す）
  - `LookupAndRefresh(ctx, rawSessionToken, now)` — `session.HashToken` → `repo.Get` → 失効
    判定の順序 **absolute → revoked → idle**（Req 4.5 が最強拘束）→ 失効時は `repo.Revoke` を
    発行（冪等 / Req 4.6）+ 対応する `failure_kind` 401 → 有効なら `repo.Touch(hash, now)` +
    `(Identity, Session, nil)`
  - `Logout(ctx, rawSessionToken)` — `HashToken` → `repo.Revoke`（既に revoked でも no-op）
  - `Service` 構築 helper `NewService(cfg, verifier, repo, oauth2Configs map[oidc.Console]
    *oauth2.Config, clock, log)`。`oauth2Configs` は tenant / admin 別 client_id /
    redirect_uri / endpoints
  - `backend/internal/auth/service_test.go` を新規追加。fake `oidc.Verifier` / fake
    `Repository` / fake `Clock` / fake `oauth2` token endpoint（httptest.NewServer）を使い:
    (a) BeginLogin の return_to validate（相対 OK / host 指定 400）、(b) HandleCallback の
    正常系で Session 作成 + cookie 返却、(c) state mismatch / state expired / state invalid /
    各 OIDC 失敗種別の伝播、(d) **`Claims.MatchedConsole` と handler の expected console
    不一致で `invalid_aud`**（Req 6.2 のテスト）、(e) LookupAndRefresh の境界値（idle 29:59 /
    30:00 / 30:01、absolute 7:59:59 / 8:00:00 / 8:00:01、revoked_at != nil → session_revoked）、
    (f) Logout で Revoke 1 回呼ばれる、2 回目 Logout は no-op
  - _Requirements: 2.1, 2.5, 2.6, 2.7, 2.8, 2.9, 3.1, 3.9, 4.3, 4.4, 4.5, 4.6, 5.1, 5.3, 5.4, 6.2, NFR 3.1, NFR 4.1_
  - _Boundary: AuthService_
  - _Depends: 2.1, 3.1, 3.2, 4.1_
- [ ] 5.2 Handler 実装 + httptest 単体テスト
  - `backend/internal/auth/handler.go` を新規追加。`Handler` struct と `Mount(r chi.Router,
    consolePrefix string, console oidc.Console)` を提供。`/login`（GET）/ `/callback`（GET）/
    `/logout`（POST）の 3 ルートを `r.Get("/login", h.login(console))` のように console を
    closure で固定して登録
  - login ハンドラ: `return_to` クエリ取得 → `service.BeginLogin` → `Set-Cookie:
    state_cookie` + `302 Found` redirect。エラー時 `errors.WriteHTTP`
  - callback ハンドラ: `code` / `state` クエリ取得 → cookie から state cookie 取得 →
    `service.HandleCallback` → `Set-Cookie: session_cookie` + `Set-Cookie: state_expire` +
    `302 Found` redirect to return_to（HandleCallback が返す StatePayload.RedirectKey の
    一致確認は service 内で完結）
  - logout ハンドラ: cookie から session token 取得 → `service.Logout` → `Set-Cookie:
    session_expire` + `204 No Content`
  - `backend/internal/auth/handler_test.go` を新規追加。`httptest.NewRecorder` + chi router
    で fake Service を差し込み、6 endpoint（tenant 系 3 + admin 系 3）が以下を返すことを検証:
    (a) `/api/auth/login`: 302 + state cookie / `return_to=//evil.example` で 400、
    (b) `/api/auth/callback`: 302 + session cookie + state cookie 削除、
    (c) `/api/auth/callback`: state mismatch で 401 + cookie 削除、
    (d) `/api/auth/logout`: 204 + session cookie 削除、cookie 不在で 401
  - _Requirements: 2.1, 2.2, 2.5, 2.8, 3.1, 5.2, 6.2_
  - _Boundary: AuthHandler_
  - _Depends: 5.1_

- [ ] 6. auth.Middleware + bootstrap 配線 + integration テスト
- [ ] 6.1 Middleware 実装と単体テスト
  - `backend/internal/auth/middleware.go` を新規追加。`NewMiddleware(svc Service, log
    logger.Logger, clock Clock) func(http.Handler) http.Handler` を提供
  - 動作: (1) `__Host-ae_mdm_session` cookie 取得（不在は default deny で 401 + cookie 削除）
    → (2) `service.LookupAndRefresh(ctx, raw, clock.Now())` → 失効時は `Set-Cookie: expire` +
    `errors.WriteHTTP(401)` → (3) 成功時は `httpserver.AuthClaims{TenantID: identity.TenantID,
    AdminUserID: identity.AdminUserID, Roles: identity.Roles, IsSuperAdmin: identity.
    IsSuperAdmin}` を `httpserver.WithAuthClaims(ctx, ...)` で ctx に注入 → next.ServeHTTP
  - panic / DB error 等の想定外例外は fail-closed で 401（NFR 3.1）。Cause は ERROR ログ
  - `backend/internal/auth/middleware_test.go` を新規追加。fake Service + chi route で
    (a) cookie 不在で 401 + cookie 削除、(b) LookupAndRefresh が `session_idle` を返したら
    401 + cookie 削除、(c) `session_expired` / `session_revoked` / `session_tamper` も同様、
    (d) 成功時に `httpserver.AuthClaimsFromContext` で AuthClaims が取り出せる + next 到達、
    (e) fake Service が panic した場合 fail-closed で 401（recover チェーンは httpserver 側
    Recoverer に委ねる前提で本 middleware は panic を握りつぶさず 500 にする経路でも可、
    本 task ではどちらでも仕様適合）
  - _Requirements: 3.7, 4.3, 4.4, 4.5, 4.6, 4.7, 5.3, 5.4, NFR 3.1, NFR 4.1_
  - _Boundary: AuthMiddleware_
  - _Depends: 5.1, 1.3_
- [ ] 6.2 httpserver.NewServer に auth middleware + auth エンドポイントを配線
  - `backend/internal/platform/httpserver/server.go` の `NewServer` シグネチャを変更。
    `authMW func(http.Handler) http.Handler`（auth.Middleware の戻り値）と `authMount
    func(r chi.Router, consolePrefix string)`（auth.Handler の mount 関数）を追加引数として
    受け取る。`/api/auth` を root router 直下に Mount（TenantContextMiddleware の **外側** /
    認証未確立の段階で到達するため）、`/api/admin/auth` も root router 直下に Mount
  - `apiRouter` / `adminRouter` の `Use(...)` チェーンに `authMW` を `TenantContextMiddleware`
    の **前段**として挿入。これにより auth middleware が session cookie を lookup して
    AuthClaims を注入 → TenantContextMiddleware が AuthClaims から TenantContext を確立 →
    domain handler に到達する流れが成立する
  - 引数の nil 許容（test 用 fixture が auth 未配線で `NewServer` を呼ぶ既存テストを壊さない
    ため）: `authMW == nil` の場合は A2 既存挙動（default deny 401）を維持。
    `authMount == nil` の場合は auth エンドポイントを Mount しない
  - `server_test.go` を追加変更: (a) authMW 配線時の `/api/...` が AuthClaims 注入後に
    TenantContext 確立を経て 200 を返す経路（test stub handler 経由）、(b) authMount 配線時
    に `/api/auth/login` が auth.Handler に到達する（既存 401 default deny ではなく 302 を
    返す）
  - _Requirements: 5.3, 5.4_
  - _Boundary: HTTPServer_
  - _Depends: 6.1, 5.2_
- [ ] 6.3 cmd/api bootstrap に OIDC Verifier / Auth 配線追加
  - `backend/cmd/api/main.go` を編集。`config.Load()` の後に `oidc.NewVerifier(ctx, cfg)` を
    呼び（失敗時は exit 1 / NFR 3.2）、`auth.NewRepository(pool)` → `auth.NewService(cfg,
    verifier, repo, oauth2Configs, clock, log)` → `auth.NewMiddleware(svc, log, clock)` を
    構築 → `httpserver.NewServer(cfg, log, pool, authMW, authMount)` に注入
  - `oauth2Configs` の構築は `cmd/api/main.go` 内で `map[oidc.Console]*oauth2.Config{
    oidc.ConsoleTenant: { ClientID: cfg.OIDCTenantClientID, RedirectURL: cfg.
    OIDCTenantRedirectURL, Endpoint: verifier.TenantEndpoint(), ... }, ... }` のように
    （Endpoint 取得は task 2.1 で `Verifier` interface に追加する helper を使う）
  - bootstrap 失敗時の exit ハンドリング（既存 A2 パターンに揃える: ERROR ログ + os.Exit(1)）
  - `backend/cmd/api/main_test.go` に bootstrap smoke test がある場合は更新（auth 配線が
    増えても起動可能であること）。実 IdP 到達は test しない（mock 不要 / 本 task では bootstrap
    のコード経路カバーのみ）
  - _Requirements: NFR 3.1, NFR 3.2_
  - _Boundary: cmd-api_
  - _Depends: 6.2_
- [ ] 6.4 結合テスト（auth 全フロー）
  - `backend/test/integration/auth_login_callback_test.go` を新規追加。`docker compose up -d
    postgres` 前提 + テスト用 RSA private key で OIDC IdP を `httptest.NewServer` で mock
    （discovery / JWKS / token endpoint を提供）。シナリオ: (a) `GET /api/auth/login` で 302 +
    state cookie 発行、(b) `GET /api/auth/callback` で session 作成・sessions テーブルに 1 行・
    cookie に hash でない生値・永続ストアには hash のみ・state cookie が削除（Req 2.8 /
    3.1–3.9 / NFR 1.2）、(c) state cookie 改竄で 401、(d) ID トークン aud 不一致で 401
  - `backend/test/integration/auth_session_lookup_test.go` を新規追加。Create Session 後に
    test 用 stub handler を `/api/devices` 相当に mount し、(a) cookie 提示で 200 +
    AuthClaims が ctx に到達、(b) idle 31 分後の再アクセスで 401 + revoked_at 更新、(c)
    absolute 8h+1s 後の再アクセスで 401（Req 4.3–4.7）
  - `backend/test/integration/auth_logout_revoke_test.go` を新規追加。Create Session → `POST
    /api/auth/logout` → 同 cookie 再提示で 401（Req 5.1 / 5.3）、改竄 cookie（hash 不一致）でも
    401（Req 5.4）
  - DATABASE_URL 未設定 / 必要 binary 不在時は各 test が自身で `t.Skip` する設計（A2 既存
    パターンに揃える）
  - _Requirements: 1.1, 1.3, 1.4, 2.1, 2.5, 2.8, 3.1, 3.6, 3.7, 4.3, 4.4, 4.5, 4.6, 4.7, 5.1, 5.3, 5.4, 6.2, NFR 1.2, NFR 4.1_
  - _Boundary: AuthService, AuthHandler, AuthMiddleware, AuthRepository, OIDCVerifier, HTTPServer_
  - _Depends: 6.3_

- [ ] 7. ドキュメント更新（runbook / impl-notes）
- [ ] 7.1 runbook / impl-notes の認証配線手順を追記
  - `docs/runbook/local-dev.md`（A2 で新規追加済み）に「OIDC 認証フロー検証手順」節を追加。
    Keycloak realm export（`infra/keycloak/realm-export.json`、umbrella task 1.2 で作成
    済み前提）の tenant-console / admin-console 2 client が必要であることを明記し、未配置の
    場合は本 Issue 範囲では IdP mock を使うか、umbrella task 1.2 完了を待つ旨を記載
  - `STATE_MAC_SECRET` の生成手順（`openssl rand -hex 32`）と、`.env.example` の置換手順を
    記載
  - `docs/specs/33--a3a-oidc-verifier-session/impl-notes.md` を新規追加し、(a) `coreos/go-oidc`
    を indirect → direct 依存に昇格する `go.mod` / `go.sum` 更新が必要、(b) Keycloak realm
    export の 2 client 定義への依存、(c) 確認事項 1–6 のうち本 Issue 実装時点で未解消のもの、
    (d) `internal/depspin/depspin.go` 内 `coreos-go-oidc` の blank import を本 Issue で
    削除（A2 task 5.2 で残置されていれば）、を箇条書きで記載
  - _Requirements: NFR 2.1_
  - _Boundary: Documentation_
  - _Depends: 6.3_

## Verify

本 spec の実装後、watcher（stage-a-verify gate）が再実行すべき verify コマンドを構造化ブロックで
宣言する。OIDC 検証 / state cookie / session 永続化 / middleware の全 task は `go test ./...` で
ユニットテストおよび `_test.go` の整合性が担保される。integration test（`backend/test/integration/
auth_*.go`）は DATABASE_URL 接続が確立できない環境で `t.Skip` する設計のため、DB 不在環境でも
verify は false-fail しない。

<!-- stage-a-verify -->
```sh
cd backend && go build ./... && go vet ./... && go test ./...
```
