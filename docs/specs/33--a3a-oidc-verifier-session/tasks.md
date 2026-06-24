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
- [ ] 1.1 Config に session timeout / state / OIDC client secret を追加 (P)
  - `backend/internal/config/config.go` に以下を追加:
    - `SessionIdleTimeout time.Duration`（env `SESSION_IDLE_TIMEOUT`, default `30m`）
    - `SessionAbsoluteTimeout time.Duration`（env `SESSION_ABSOLUTE_TIMEOUT`, default `8h`）
    - `StateCookieTTL time.Duration`（env `STATE_COOKIE_TTL`, default `10m`, **validation: `0 < ttl <= 10m`**。Req 2.4「10 分以内に有効期限が切れる」を上限制約として満たすため、設定値が `0` / 負の duration / `10m` を超える値のいずれかなら起動時 `*errors.Error{Code: config_invalid}` で reject する）
    - `StateMACSecret string`（env `STATE_MAC_SECRET`, required, len >= 32 bytes）
    - `OIDCTenantClientSecret string`（env `OIDC_TENANT_CLIENT_SECRET`, required, confidential
      client + `client_secret_basic` 認証用 / design.md Req 6.2 と確認事項 6 で確定）
    - `OIDCAdminClientSecret string`（env `OIDC_ADMIN_CLIENT_SECRET`, required, 同上）
  - `backend/internal/config/env.go` の既存パーサパターンに合わせて `duration` パーサ
    helper（または既存があれば再利用）と string（min length / required）バリデーションを追加。
    client secret は **空文字を許可しない**（required string）
  - `backend/internal/config/config_test.go` に以下を追加:
    - (a) default 値適用（3 つの duration が既定値）
    - (b) `STATE_MAC_SECRET` 未設定 / 32 文字未満で `*errors.Error{Code: config_invalid}`
    - (c) `OIDC_TENANT_CLIENT_SECRET` / `OIDC_ADMIN_CLIENT_SECRET` 未設定（空文字）で
      `*errors.Error{Code: config_invalid}`
    - (d) duration 不正フォーマットで `*errors.Error{Code: config_invalid}`
    - (e) 正常系で全 6 値が読み込まれること
    - (f) `STATE_COOKIE_TTL=0s` / `STATE_COOKIE_TTL=-1m` / `STATE_COOKIE_TTL=11m` のいずれも
      `*errors.Error{Code: config_invalid}`（境界値: `STATE_COOKIE_TTL=10m` は受理 /
      `STATE_COOKIE_TTL=10m1s` は reject）
  - `.env.example` に以下 6 行を追加（管理者セッション暗号化用秘密鍵セクション直下）:
    - `SESSION_IDLE_TIMEOUT=30m`
    - `SESSION_ABSOLUTE_TIMEOUT=8h`
    - `STATE_COOKIE_TTL=10m`
    - `STATE_MAC_SECRET=<REPLACE_ME_GENERATE_32_BYTES_OF_RANDOM_HEX>`
    - `OIDC_TENANT_CLIENT_SECRET=<REPLACE_ME_FROM_KEYCLOAK_TENANT_CLIENT>`
    - `OIDC_ADMIN_CLIENT_SECRET=<REPLACE_ME_FROM_KEYCLOAK_ADMIN_CLIENT>`
  - _Requirements: 6.1, 6.2, 6.4, NFR 1.1, NFR 2.1, NFR 2.2_
  - _Boundary: Config_
- [ ] 1.2 sessions / admin_users テーブル拡張マイグレーション (P)
  - `backend/db/migrations/0013_extend_sessions.up.sql` を新規追加。`ALTER TABLE sessions ADD
    COLUMN last_seen_at timestamptz NOT NULL DEFAULT now()` → `UPDATE sessions SET last_seen_at
    = idle_at` → `ALTER TABLE sessions DROP COLUMN idle_at` → `ALTER TABLE sessions ADD
    COLUMN revoked_at timestamptz NULL` → `ALTER TABLE sessions ADD COLUMN console text NOT
    NULL DEFAULT 'tenant-console' CHECK (console IN ('tenant-console','admin-console'))` →
    `ALTER TABLE sessions ALTER COLUMN console DROP DEFAULT`
  - `backend/db/migrations/0013_extend_sessions.down.sql` を新規追加。対称な DROP / ADD
    （`idle_at` 復元、`revoked_at` / `console` DROP）。`idle_at` の値復元は `last_seen_at`
    からの UPDATE で対応
  - **`backend/db/migrations/0014_admin_users_add_oidc_issuer.up.sql` を新規追加**:
    `ALTER TABLE admin_users ADD COLUMN oidc_issuer text NOT NULL DEFAULT ''` →
    `UPDATE admin_users SET oidc_issuer = '<TENANT_ISSUER_URL_PLACEHOLDER>'`（既存行は A2 時点で
    空のはずだが念のため backfill）→ `ALTER TABLE admin_users ALTER COLUMN oidc_issuer DROP
    DEFAULT` → `ALTER TABLE admin_users DROP CONSTRAINT admin_users_oidc_subject_key`（既存の
    `oidc_subject UNIQUE` を解除）→ `ALTER TABLE admin_users ADD CONSTRAINT
    admin_users_oidc_issuer_subject_key UNIQUE (oidc_issuer, oidc_subject)`。**理由**: OIDC `sub`
    は issuer スコープでのみ一意のため、tenant / admin issuer を分けられる本設計では
    `(oidc_issuer, oidc_subject)` の組で admin_users を解決する必要がある（`sub` 単独の
    UNIQUE 制約は別 issuer の同 sub と衝突する偽陽性を生む）
  - 対応 down: `0014_admin_users_add_oidc_issuer.down.sql` で対称の DROP CONSTRAINT + ADD
    CONSTRAINT（`oidc_subject UNIQUE` 復元）+ `DROP COLUMN oidc_issuer`
  - 既存 RLS ポリシー（`tenant_isolation_sessions`）は本マイグレーションで再定義しない
    （A2 で配置済みのまま、列追加のみ）
  - **既存コード / fixture / test の `sessions.idle_at` 参照を確認・置換**:
    `grep -rn "idle_at" backend/ test/` を実行し、A2 で `sessions.idle_at` を参照している
    repository / fixture / integration test / SQL を列挙する。検出された全箇所を
    `last_seen_at` に置換し、置換漏れがあると本 migration 後に既存テストが壊れる
    （SQLState 42703: column "idle_at" does not exist）。置換対象には少なくとも (a)
    `backend/internal/platform/db/` 配下の RLS / sessions テスト、(b)
    `backend/test/integration/` 配下の sessions 系 helper、(c) A2 で配置された
    `sessions_test.go` 等のユニットテストが含まれる想定
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
    `Verifier` interface（`VerifyIDToken(ctx, raw) (Claims, error)`）を実装。
    **go-oidc 内蔵 aud 検証は `provider.Verifier(&oidc.Config{ClientID: "", SkipClientIDCheck:
    true})` で明示的に切る**（`ClientID == "" && !SkipClientIDCheck` だと go-oidc v3 が
    `invalid configuration` で reject するため、両方を明示する必要がある / design.md
    「OIDC Verifier」節と整合）。aud 検証は本パッケージで明示実装（tenant / admin のいずれか
    **排他一致**を強制 / Req 1.4 / 1.5）
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
  - `backend/internal/auth/state.go` を新規追加。`StatePayload`（**`Nonce` / `Console` /
    `ReturnTo` / `IssuedAt`**。design.md と整合 / Req 2.9 で MAC 保護下に置く戻り先 URL の
    生値）と `Sign(payload, secret) (cookieValue string, err error)` /
    `Verify(cookieValue, queryState, secret, ttl, now) (StatePayload, error)` /
    `CookieAttributes(ttl) http.Cookie` を実装。MAC は HMAC-SHA256、cookie 値フォーマットは
    `base64url(json(payload)) + "." + base64url(MAC)`、比較は `subtle.ConstantTimeCompare`。
    cookie 名は **`__Host-ae_mdm_state`** 固定（design.md と整合）
  - `backend/internal/auth/state_test.go` を新規追加。
    - (a) Sign → Verify 往復で `StatePayload.ReturnTo` が cookie 経由で復元される
    - (b) MAC tamper で `*errors.Error{Code: CodeUnauthenticated, failure_kind: state_invalid}`
    - (c) TTL 超過で `failure_kind: state_expired`
    - (d) `Nonce` 改竄で `failure_kind: state_invalid`
    - (e) queryState と cookie 内 Nonce 不一致で `failure_kind: state_mismatch`
    - (f) cookie 不在（空文字）で `failure_kind: state_invalid`
    - **(g) `CookieAttributes(ttl)` の属性検証**（Req 2.3 / 2.4 のテスト直接対応）:
      `Name == "__Host-ae_mdm_state"` / `HttpOnly == true` / `Secure == true` /
      `SameSite == http.SameSiteLaxMode` / `Path == "/"` / `MaxAge == int(ttl/time.Second)`
      （`cfg.StateCookieTTL` から導出 / 10 分以下）
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
  - `backend/internal/auth/repository.go` を新規追加。`Repository` interface（**`ResolveAdminUser(ctx, issuer, subject, email, console)`**
    / `Create` / `Get` / `Touch` / `Revoke`）と `pgxpool.Pool` ベースの実装を提供
  - すべての CRUD は `db.BeginTxFunc` 経由で実行する。`Get` / `Touch` / `Revoke` /
    `ResolveAdminUser` は **`SuperAdmin context`**（`db.WithTenantContext(ctx,
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
  - **`ResolveAdminUser(ctx, issuer, sub, email, console)`** は事前 provisioning **必須** の
    read-modify-write を以下の手順で実行する（**新規 INSERT は行わない** / design.md と整合）:
    1. SuperAdmin context で `SELECT id, tenant_id FROM admin_users WHERE oidc_issuer = $1 AND oidc_subject = $2 FOR UPDATE`
       （OIDC の `sub` は **issuer スコープ**でのみ一意のため、tenant / admin issuer を
       分けられる本設計では `(issuer, subject)` の組で一意解決する。`subject` 単独で解決すると
       別 issuer のユーザーへ誤解決する。`admin_users` テーブルに `oidc_issuer text NOT NULL`
       カラムが存在しない場合は A2 既存スキーマを拡張する migration（後述 task 1.2 / 1.2a の
       いずれかに統合）が必要 — 詳細は impl-notes.md「OIDC issuer 識別子の永続化方針」節で
       PM 確認）
    2. 0 行なら `*errors.Error{Code: CodeForbidden, failure_kind:
       admin_user_not_provisioned}` を返す（HTTP 403 に Service が マッピング）
    3. 1 行なら `UPDATE admin_users SET email = $1 WHERE id = $2`（IdP 側で email が変わった
       場合に追従。`admin_users.id` / `tenant_id` / `oidc_issuer` / `oidc_subject` は不変）
    4. `admin_role_assignments` を join して `Identity.Roles` / `IsSuperAdmin` を集約し、
       `Identity{AdminUserID, OIDCSubject:sub, Email, TenantID, Roles, IsSuperAdmin}` を返す
  - `backend/test/integration/auth_repository_test.go` を新規追加。`docker compose up -d
    postgres` 前提（DATABASE_URL 未設定で skip）。シナリオ:
    - (a) `ResolveAdminUser` 未 provisioning 時 403 `admin_user_not_provisioned`（事前 seed
      なしで OIDC subject 提示 → reject）
    - (b) `ResolveAdminUser` provisioned 既存行で Identity が tenant_id / Roles 込みで返る
    - (c) `ResolveAdminUser` で IdP 側 email 変更が反映される（事前 seed → resolve → 行内 email
      が新値、admin_users.id / tenant_id は不変）
    - (d) Create → Get で hash 一致時に Session+Identity が返る
    - (e) Get で hash 不一致時に 0 行 → `session_tamper`
    - (f) Touch 後の last_seen_at 更新と expires_at 不変
    - (g) Revoke 後の revoked_at セット
    - (h) Revoke 冪等性
  - _Requirements: 3.7, 3.9, 4.3, 4.6, 4.8, 5.1, 5.3, 5.4, 6.3_
  - _Boundary: AuthRepository_
  - _Depends: 1.2, 1.3, 3.1, 3.2_

- [ ] 5. auth.Service（4 ユースケース）+ auth.Handler（HTTP 6 endpoints）
- [ ] 5.1 Service 実装 + 単体テスト
  - `backend/internal/auth/service.go` を新規追加。`Service` interface（`BeginLogin` /
    `HandleCallback` / `LookupAndRefresh` / `Logout`）を提供
  - `BeginLogin(ctx, console, returnTo) (redirectURL string, stateCookie http.Cookie, err error)` —
    `returnTo` 正規化と検証（**空文字の場合は default `/` を採用**して callback 成功時に空の
    `Location` ヘッダが出ないようにする / Req 2.5。空でない場合は同一オリジン内相対 URL のみ
    許容、`http://` `https://` `//` を含む host 指定は 400 / 確認事項 3）→ `StatePayload{Nonce,
    Console: console, ReturnTo: returnTo, IssuedAt: clock.Now()}` 構築 → `state.Sign` で
    cookie 値生成 → `state.CookieAttributes(cfg.StateCookieTTL)` に cookie 値をセット →
    IdP 認可エンドポイント URL を `oauth2.Config.AuthCodeURL(payload.Nonce)` で構築
    （`state` クエリパラメータには `payload.Nonce` を載せる / cookie 内 Nonce との
    constant-time 一致確認に使う）→ `(redirectURL, stateCookie, nil)` を返す
  - `HandleCallback(ctx, console, code, queryState, rawStateCookie) (rawSessionToken string,
    sessionCookie http.Cookie, returnTo string, err error)`:
    1. `state.Verify(rawStateCookie, queryState, cfg.StateMACSecret, cfg.StateCookieTTL,
       clock.Now())` → 成功時に `StatePayload` を取り出して `returnTo := payload.ReturnTo`。
       失敗時は対応する `failure_kind`（`state_invalid` / `state_expired` /
       `state_mismatch` / `state_replay`）で 401
    1a. **`payload.Console == console` 検証**: state cookie 内の `StatePayload.Console` が
       当該 callback の console（引数 `console`）と一致しない場合は `failure_kind:
       state_console_mismatch` で 401 + 即時 state cookie 削除（Max-Age=0）。tenant login で
       発行した state を admin callback に提示する cross-console state 混同を拒否する
       （Req 2.9 / 6.2 の物理分離強制）。本検証は state.Verify の MAC / TTL 検証を通過した
       直後に Service 層で行う（state.go の `Verify` 戻り値 `StatePayload.Console` を Service が
       明示的に照合する）
    2. `oauth2.Config.Exchange(ctx, code)` で token 取得（5xx は `*errors.Error{Code:
       CodeUpstream, failure_kind: upstream_oidc_token}`）
    3. **token endpoint レスポンスから `id_token` を安全に取り出す**:
       `rawIDToken, ok := token.Extra("id_token").(string)` で取り出し、`!ok || rawIDToken == ""`
       なら `*errors.Error{Code: CodeUpstream, failure_kind: upstream_oidc_token}` で 502 を返す
       （直接 type assertion `token.Extra("id_token").(string)` は token endpoint が `id_token` を
       返さない / 文字列でない場合に panic するため禁止 / NFR 3.1 の fail-closed と整合）。
       `verifier.VerifyIDToken(ctx, rawIDToken)` → `Claims.MatchedConsole != console` なら
       `failure_kind: invalid_aud` で拒否（Req 6.2 のクライアント分離強制）
    4. `repo.ResolveAdminUser(ctx, claims.Issuer, claims.Subject, claims.Email, console)` →
       `admin_user_not_provisioned` で 403 を伝播（OIDC の `sub` は issuer スコープで一意のため、
       tenant / admin で issuer を分けられる本設計では **`issuer` + `subject` の組**で
       admin_users を解決する必要がある。`subject` 単独で解決すると別 issuer のユーザーへ誤解決
       するリスクがある / 後述 task 4.1 の Repository signature 変更と整合）
    5. `rawSessionToken, _ := session.New()`、`tokenHash := session.HashToken(rawSessionToken)`
    6. `now := clock.Now()` を 1 度取得し、`repo.Create(ctx, Session{TokenHash: tokenHash,
       AdminUserID: identity.AdminUserID, Console: console, IssuedAt: now, LastSeenAt: now,
       ExpiresAt: now.Add(cfg.SessionAbsoluteTimeout), RevokedAt: nil})` で 1 行 INSERT
       （Req 4.1 / 4.2 / 4.8 / NFR 2.1 を満たす）
    7. `sessionCookie := session.CookieAttributes()` の Value に `rawSessionToken` を設定
       （`MaxAge` は `cfg.SessionAbsoluteTimeout` 由来）
    8. `(rawSessionToken, sessionCookie, returnTo, nil)` を返す（state cookie の **削除**
       は Handler が `session.ExpireCookieAttributes()` 相当を発行することで実施）
  - `LookupAndRefresh(ctx, rawSessionToken string, expectedConsole oidc.Console, now time.Time)
    (Identity, Session, error)` — `session.HashToken` → `repo.Get` → **失効判定の順序**
    `1) Session.Console != expectedConsole → console_mismatch（Req 6.2 / 6.3 の Auth Middleware
    側 console 分離強制）` → `2) now > Session.ExpiresAt → session_expired（absolute / Req 4.5）`
    → `3) Session.RevokedAt != nil → session_revoked（Req 5.3）` → `4) now -
    Session.LastSeenAt > cfg.SessionIdleTimeout → session_idle（Req 4.4）`。失効時は
    `repo.Revoke(ctx, hash, now)`（冪等 / Req 4.6）+ 対応する `failure_kind` で
    `*errors.Error{Code: CodeUnauthenticated, Cause: kind}` を返す。有効なら
    `repo.Touch(ctx, hash, now)` + `(Identity, Session, nil)`
  - `Logout(ctx, rawSessionToken)` — `HashToken` → `repo.Revoke`（既に revoked でも no-op）
  - **`failure_kind` ログ field 出力の実装責務**: `BeginLogin` / `HandleCallback` /
    `LookupAndRefresh` の各失敗パスで `log.Warn("auth failure", "failure_kind", kind,
    "console", string(console), "session_hash_prefix", session.HashPrefix(hash))` の形式で
    **明示的に** field を出すこと（NFR 4.1 を実装で満たす / cause チェーンに乗せるだけでは
    観測性 AC を満たせない）。A2 `internal/logger` の `Logger.Warn(msg string, fields ...any)`
    は `"key", value` 可変長ペア形式を受け付ける（`logger.String` のような helper は存在
    しない / `logger.TenantID` / `RequestID` / `MessageID` / `ActorID` / `Err` のみが
    field helper / 詳細は `backend/internal/logger/logger.go` の `toZapFields`）。
    `session_hash_prefix` は session lookup 経路でのみ追加（state / OIDC 検証経路では unset）
  - `Service` 構築 helper `NewService(cfg, verifier, repo, oauth2Configs map[oidc.Console]
    *oauth2.Config, clock, log)`。`oauth2Configs` は tenant / admin 別 `ClientID` /
    `ClientSecret`（`cfg.OIDCTenantClientSecret` / `cfg.OIDCAdminClientSecret` を **必ず**
    設定）/ `RedirectURL` / `Endpoint`
  - `backend/internal/auth/service_test.go` を新規追加。fake `oidc.Verifier` / fake
    `Repository` / fake `Clock` / fake `oauth2` token endpoint（httptest.NewServer）を使い:
    - (a) `BeginLogin` の return_to validate（相対 OK / `//evil.example` 含む host 指定で 400）
    - (b) `HandleCallback` 正常系で Session 作成 + cookie 返却 + `returnTo` が
      `StatePayload.ReturnTo` 由来
    - (c) state mismatch / state expired / state invalid / 各 OIDC 失敗種別の伝播
    - (c2) **`StatePayload.Console` と handler の console 不一致で `state_console_mismatch`**
      （tenant login で発行した state を admin callback に提示する経路 / Req 2.9 / 6.2 の
      cross-console state 混同 reject）
    - (d) **`Claims.MatchedConsole` と handler の expected console 不一致で `invalid_aud`**
      （Req 6.2 のテスト）
    - (e) `ResolveAdminUser` が `admin_user_not_provisioned` を返したら Service が 403 で
      伝播し、session 作成に到達しないこと
    - (f) `LookupAndRefresh` の境界値（idle 29:59 / 30:00 / 30:01、absolute 7:59:59 /
      8:00:00 / 8:00:01、revoked_at != nil → session_revoked、Console mismatch →
      console_mismatch（Req 6.2 / 6.3 の Middleware 側分離強制テスト））
    - (g) `Logout` で Revoke 1 回呼ばれる、2 回目 Logout は no-op
    - (h) 各失敗パスで `log.Warn` に `failure_kind` field が **明示的に**渡されている
      （fake logger で field を assert）
  - _Requirements: 2.1, 2.5, 2.6, 2.7, 2.8, 2.9, 3.1, 3.9, 4.1, 4.2, 4.3, 4.4, 4.5, 4.6, 4.8, 5.1, 5.3, 5.4, 6.2, 6.3, NFR 3.1, NFR 4.1_
  - _Boundary: AuthService_
  - _Depends: 2.1, 3.1, 3.2, 4.1_
- [ ] 5.2 Handler 実装 + httptest 単体テスト
  - `backend/internal/auth/handler.go` を新規追加。`Handler` struct と `Mount(r chi.Router,
    consolePrefix string, console oidc.Console)` を提供。`/login`（GET）/ `/callback`（GET）/
    `/logout`（POST）の 3 ルートを `r.Get("/login", h.login(console))` のように console を
    closure で固定して登録
  - login ハンドラ: `return_to` クエリ取得 → `service.BeginLogin(ctx, console, returnTo)` →
    `Set-Cookie: state_cookie` + `302 Found` `Location: redirectURL`。エラー時 `errors.WriteHTTP`
  - callback ハンドラ: `code` / `state` クエリ取得 → cookie から state cookie 取得 →
    `rawSessionToken, sessionCookie, returnTo, err := service.HandleCallback(ctx, console,
    code, queryState, rawStateCookie)` → 成功時は **`Set-Cookie: session_cookie`**（生値が
    Value）+ **`Set-Cookie: state cookie 削除`**（`Max-Age=0; Path=/` の `__Host-ae_mdm_state`）+
    `302 Found` `Location: returnTo`（**Service が戻す `returnTo` は `StatePayload.ReturnTo`
    由来 / MAC 保護されているので tamper されない / それでも Handler は念のため Location
    値に対する `chi.URLParam` 等での host 解析を行わず生値 string をそのまま `Location` に
    乗せる**）
  - logout ハンドラ: cookie から session token 取得 → `service.Logout(ctx, rawSessionToken)` →
    `Set-Cookie: session_expire`（`session.ExpireCookieAttributes()`）+ `204 No Content`
  - `backend/internal/auth/handler_test.go` を新規追加。`httptest.NewRecorder` + chi router
    で fake Service を差し込み、6 endpoint（tenant 系 3 + admin 系 3）が以下を返すことを検証:
    - (a) `/api/auth/login`: 302 + state cookie / `return_to=//evil.example` で 400
    - (b) `/api/auth/callback`: 302 + session cookie + state cookie 削除 + Location が fake
      Service の戻す `returnTo` 値と一致
    - (c) `/api/auth/callback`: state mismatch で 401 + cookie 削除
    - (d) `/api/auth/logout`: 204 + session cookie 削除、cookie 不在で 401
  - _Requirements: 2.1, 2.2, 2.5, 2.8, 3.1, 5.2, 6.2_
  - _Boundary: AuthHandler_
  - _Depends: 5.1_

- [ ] 6. auth.Middleware + bootstrap 配線 + integration テスト
- [ ] 6.1 Middleware 実装と単体テスト
  - `backend/internal/auth/middleware.go` を新規追加。**`NewMiddleware(svc Service,
    expectedConsole oidc.Console, log logger.Logger, clock Clock) func(http.Handler)
    http.Handler`** を提供。`expectedConsole` は tenant 系 / admin 系で **別インスタンス**を
    構築するために必須（design.md「Auth Middleware」節および Req 6.2 / 6.3 で要求）
  - 動作:
    1. `__Host-ae_mdm_session` cookie 取得（不在は default deny で 401 + cookie 削除 /
       `failure_kind: session_tamper` でログ）
    2. `service.LookupAndRefresh(ctx, raw, expectedConsole, clock.Now())` を呼ぶ
       （**`expectedConsole` を引数で伝搬する**ことで、Service 側で console 照合 →
       absolute → revoked → idle の順で失効判定し、`Session.Console != expectedConsole`
       なら `failure_kind: console_mismatch` で 401 を返す。これにより tenant 系 cookie が
       admin route に提示された場合に **即座に拒否**できる / Req 6.2 / 6.3）
    3. 失効時は `Set-Cookie: expire` + `errors.WriteHTTP(401)` + `log.Warn` に
       `failure_kind` を含む構造化 field を出す（NFR 4.1）
    4. 成功時は `httpserver.AuthClaims{TenantID: identity.TenantID, AdminUserID:
       identity.AdminUserID, Roles: identity.Roles, IsSuperAdmin: identity.IsSuperAdmin}`
       を `httpserver.WithAuthClaims(ctx, ...)` で ctx に注入 → `next.ServeHTTP`
  - panic / DB error 等の想定外例外は fail-closed で 401（NFR 3.1）。Cause は ERROR ログ
  - `backend/internal/auth/middleware_test.go` を新規追加。fake Service + chi route で:
    - (a) cookie 不在で 401 + cookie 削除
    - (b) LookupAndRefresh が `session_idle` を返したら 401 + cookie 削除
    - (c) `session_expired` / `session_revoked` / `session_tamper` も同様
    - (d) **`expectedConsole=ConsoleAdmin` の middleware に tenant 用 session が提示された
      ケース（fake Service が `console_mismatch` を返す）で 401 + cookie 削除 + `log.Warn`
      の `failure_kind=console_mismatch` field**（Req 6.2 / 6.3 のテスト直接対応）
    - (e) 成功時に `httpserver.AuthClaimsFromContext` で AuthClaims が取り出せる + next 到達
    - (f) fake Service が panic した場合 fail-closed で 401（recover チェーンは httpserver 側
      Recoverer に委ねる前提で本 middleware は panic を握りつぶさず 500 にする経路でも可、
      本 task ではどちらでも仕様適合）
  - _Requirements: 3.7, 4.3, 4.4, 4.5, 4.6, 4.7, 5.3, 5.4, 6.2, 6.3, NFR 3.1, NFR 4.1_
  - _Boundary: AuthMiddleware_
  - _Depends: 5.1, 1.3_
- [ ] 6.2 httpserver.NewServer に auth middleware + auth エンドポイントを配線
  - `backend/internal/platform/httpserver/server.go` の `NewServer` シグネチャを変更。以下の
    追加引数を受け取る:
    - **`authMWTenant func(http.Handler) http.Handler`**（`expectedConsole=ConsoleTenant`
      で構築された tenant 用 auth.Middleware の戻り値）
    - **`authMWAdmin func(http.Handler) http.Handler`**（`expectedConsole=ConsoleAdmin`
      で構築された admin 用 auth.Middleware の戻り値）
    - `authMount func(r chi.Router, consolePrefix string, console oidc.Console)`
      （auth.Handler の mount 関数。`/api/auth` を `ConsoleTenant`、`/api/admin/auth` を
      `ConsoleAdmin` で 2 回呼ばれる）
  - `/api/auth` を root router 直下に `authMount(r, "/api/auth", ConsoleTenant)` で Mount
    （TenantContextMiddleware の **外側** / 認証未確立の段階で到達するため）、
    `/api/admin/auth` も root router 直下に `authMount(r, "/api/admin/auth", ConsoleAdmin)`
    で Mount
  - `apiRouter`（`/api`）の `Use(...)` チェーンには **`authMWTenant`** を、`adminRouter`
    （`/api/admin`）の `Use(...)` チェーンには **`authMWAdmin`** を、それぞれ
    `TenantContextMiddleware` の前段として挿入する。これにより漏洩した tenant 系 session が
    admin route に提示された場合に `authMWAdmin` 側で `console_mismatch` で即拒否される
    （Req 6.2 / 6.3 の物理的分離強制 / design.md「Auth Middleware」節）
  - 引数の nil 許容（test 用 fixture が auth 未配線で `NewServer` を呼ぶ既存テストを壊さない
    ため）: `authMWTenant` / `authMWAdmin` のいずれかが nil なら A2 既存挙動（default deny
    401）を維持。`authMount == nil` の場合は auth エンドポイントを Mount しない
  - `server_test.go` を追加変更:
    - (a) `authMWTenant` 配線時の `/api/...` が AuthClaims 注入後に TenantContext 確立を経て
      200 を返す経路（test stub handler 経由）
    - (b) `authMount` 配線時に `/api/auth/login` が auth.Handler に到達する（既存 401
      default deny ではなく 302 を返す）
    - (c) `authMWAdmin` 配線時に **tenant 用 session cookie**（fake Service で
      `console_mismatch` を返すよう設定）を `/api/admin/...` に提示すると 401 + cookie 削除
      （`/api/...` への提示は通る経路と対比して、cross-console reject が成立することを assert）
  - _Requirements: 5.3, 5.4, 6.2, 6.3_
  - _Boundary: HTTPServer_
  - _Depends: 6.1, 5.2_
- [ ] 6.3 cmd/api bootstrap に OIDC Verifier / Auth 配線追加
  - `backend/cmd/api/main.go` を編集。`config.Load()` の後に `oidc.NewVerifier(ctx, cfg)` を
    呼び（失敗時は exit 1 / NFR 3.2）、`auth.NewRepository(pool)` → `auth.NewService(cfg,
    verifier, repo, oauth2Configs, clock, log)` → **`authMWTenant :=
    auth.NewMiddleware(svc, oidc.ConsoleTenant, log, clock)`** + **`authMWAdmin :=
    auth.NewMiddleware(svc, oidc.ConsoleAdmin, log, clock)`** を構築 →
    `httpserver.NewServer(cfg, log, pool, authMWTenant, authMWAdmin, authMount)` に注入
  - `oauth2Configs` の構築は `cmd/api/main.go` 内で `map[oidc.Console]*oauth2.Config{
    oidc.ConsoleTenant: { ClientID: cfg.OIDCTenantClientID, **ClientSecret:
    cfg.OIDCTenantClientSecret**, RedirectURL: cfg.OIDCTenantRedirectURL, Endpoint:
    verifier.TenantEndpoint() }, oidc.ConsoleAdmin: { ClientID: cfg.OIDCAdminClientID,
    **ClientSecret: cfg.OIDCAdminClientSecret**, RedirectURL: cfg.OIDCAdminRedirectURL,
    Endpoint: verifier.AdminEndpoint() } }` のように構築
    （Endpoint 取得は task 2.1 で `Verifier` interface に追加する helper を使う / Endpoint の
    auth style は default の `client_secret_basic`）
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
    （discovery / JWKS / token endpoint を提供）。シナリオ:
    - (a) `GET /api/auth/login?return_to=/dashboard` で 302 + state cookie 発行 + Location
      に IdP 認可エンドポイント
    - (b) `GET /api/auth/callback` で session 作成・sessions テーブルに 1 行・cookie に hash
      でない生値・永続ストアには hash のみ・state cookie が削除（Req 2.8 / 3.1–3.9 /
      NFR 1.2）+ Location が `return_to=/dashboard` 由来
    - (c) state cookie 改竄で 401 + `failure_kind=state_invalid` ログ
    - (d) ID トークン aud 不一致で 401 + `failure_kind=invalid_aud` ログ
    - (e) **未 provisioning な OIDC (issuer, subject) 組** で callback → 403 +
      `failure_kind=admin_user_not_provisioned`（admin_users の事前 provisioning 必須経路 /
      新規 INSERT されないことを assert。本ケースは requirements.md の特定 AC ではなく、
      design.md「Auth Repository」節の `ResolveAdminUser` 仕様および「確認事項 8 admin_users /
      admin_role_assignments の事前 provisioning」由来）
  - `backend/test/integration/auth_session_lookup_test.go` を新規追加。Create Session 後に
    test 用 stub handler を `/api/devices` 相当に mount し:
    - (a) cookie 提示で 200 + AuthClaims が ctx に到達
    - (b) idle 31 分後の再アクセスで 401 + revoked_at 更新
    - (c) absolute 8h+1s 後の再アクセスで 401（Req 4.3–4.7）
    - (d) **tenant 用 session cookie を `/api/admin/...` に提示すると 401 +
      `failure_kind=console_mismatch` + cookie 削除**（Req 6.2 / 6.3 の cross-console
      reject 経路。stub admin handler を別途 mount）
  - `backend/test/integration/auth_logout_revoke_test.go` を新規追加。Create Session → `POST
    /api/auth/logout` → 同 cookie 再提示で 401（Req 5.1 / 5.3）、改竄 cookie（hash 不一致）でも
    401（Req 5.4）
  - DATABASE_URL 未設定 / 必要 binary 不在時は各 test が自身で `t.Skip` する設計（A2 既存
    パターンに揃える）
  - _Requirements: 1.1, 1.3, 1.4, 2.1, 2.5, 2.8, 3.1, 3.6, 3.7, 4.3, 4.4, 4.5, 4.6, 4.7, 5.1, 5.3, 5.4, 6.2, 6.3, NFR 1.2, NFR 4.1_
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
