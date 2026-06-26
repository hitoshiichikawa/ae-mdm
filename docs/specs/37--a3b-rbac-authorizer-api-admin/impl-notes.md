# Implementation Notes — Issue #37 (A3b: 認可基盤 RBAC Authorizer + /api/admin ガード)

本ファイルは Issue #37 の単一実装ループにおける実装方針 / 重要判断 / 残存課題 /
確認事項を記録する。`docs/specs/37--a3b-rbac-authorizer-api-admin/requirements.md` は
PM 確定済みのため書き換えず、矛盾点・解釈の判断は本ファイルの「確認事項」節に
記載する。

## 実装サマリ

新規パッケージ `backend/internal/platform/authz/` を作成し、4 ロール × action × resource
の表駆動 RBAC Authorizer を実装した。さらに `backend/internal/platform/httpserver/admin_middleware.go`
に新規 middleware `RequireAdminConsoleAndSuperAdmin` を追加し、`/api/admin/*` 配下に
**admin-console aud かつ SuperAdmin** の 2 条件 AND ガードを固定で挟む。`AuthClaims` に
Console フィールドを追加し、`auth.NewMiddleware` で session.Console を転記する経路を
確立した。

## ファイル / モジュール配置

- `backend/internal/platform/authz/doc.go`: パッケージ doc + 依存方向ルール（errors のみ）
- `backend/internal/platform/authz/roles.go`: Role 型 / 4 ロール定数 / known role 判定
- `backend/internal/platform/authz/permissions.go`: Action / Resource 定数 +
  `permissionMatrix` 単一定数テーブル + known action/resource 判定 + `isAllowedByMatrix`
- `backend/internal/platform/authz/authz.go`: Audience 型 / DenyReason enum / Request /
  Decision 型 / Authorizer 構造体 + `Authorize` メソッド
- `backend/internal/platform/authz/authz_test.go`: 表駆動テスト + 各 fail-closed 経路の
  単体テスト
- `backend/internal/platform/httpserver/admin_middleware.go`: 既存 `RequireSuperAdmin`
  を温存しつつ、新規 `RequireAdminConsoleAndSuperAdmin` を追加（audience + SuperAdmin
  の 2 条件 AND ガード + 構造化 WARN ログ）
- `backend/internal/platform/httpserver/admin_middleware_test.go`: 既存 3 テスト温存 +
  新規 5 テストを追加（NoSession / TenantConsole / NotSuperAdmin / Success / EmptyConsole）
- `backend/internal/platform/httpserver/middleware.go`: `AuthClaims` 構造体に
  `Console string` フィールドを追加（後方互換 / zero value は audience_mismatch 経路で
  fail-closed）
- `backend/internal/platform/httpserver/server.go`: admin chain の middleware を
  `RequireSuperAdmin` から `RequireAdminConsoleAndSuperAdmin` に切り替え
- `backend/internal/platform/httpserver/server_test.go`: 既存テスト fixture に
  Console: "admin-console" を追加（admin chain 切替後の挙動を回帰耐性で固定）
- `backend/internal/auth/middleware.go`: Service.LookupAndRefresh の戻り値 Session.Console
  を `AuthClaims.Console` に転記

## 公開 API

- `authz.New() *Authorizer`: Authorizer 構築（状態を持たないため引数なし）
- `authz.Authorizer.Authorize(req Request) Decision`: 1 回の許可判定
- `authz.Request{Roles, SessionTenantID, Audience, Action, Resource, TargetTenantID}`:
  入力契約（Req 5.1）
- `authz.Decision{Allowed, DenyReason}`: 返り値（Req 5.2）
- `authz.DenyReason` enum: 7 区分（Req 5.3 + 未知 fail-closed 2 区分）
  - `DenyReasonRoleNotPermitted` / `DenyReasonCrossTenant` /
    `DenyReasonAudienceMismatch` / `DenyReasonTargetTenantInvalid` /
    `DenyReasonUnknownRole` / `DenyReasonUnknownAction` / `DenyReasonUnknownResource`
- `httpserver.RequireAdminConsoleAndSuperAdmin(log) func(http.Handler) http.Handler`:
  `/api/admin/*` 配下の 2 条件 AND ガード middleware

## 重要な実装判断

### 1. 許可マトリクスの構造選択（map[triple]struct{}）

`permissionMatrix` は `map[permissionKey]struct{}{}` 形式で宣言した。

理由:
- O(1) lookup（NFR 3.1）
- 「allow と宣言した entry のみが allow / 未登録は fail-closed deny」というセマンティクス
  が 1 行で表現できる（明示的 deny 列を持つ必要がない）
- 4 ロール × 全 action × 全 resource を 2 次元配列にすると nil 値の扱いが煩雑になる
- Go の map literal で declarative に並べられるため、design.md Permission Matrix との
  対応を目視確認しやすい

代替案として `map[Role]map[Action][]Resource` のような入れ子構造も検討したが、lookup
パスが複雑化し未知 role/action/resource の判定経路が分散するため採用しなかった。

### 2. DenyReason の enum 設計（7 区分）

requirements.md Req 5.3 は 5 区分を要求（role / cross-tenant / audience / target /
unknown role/action/resource）。本実装では **未知 fail-closed を 3 サブ区分に細分**して
合計 7 区分を distinct な enum 値として定義した:

- `DenyReasonUnknownRole`（Req 1.5）
- `DenyReasonUnknownAction`（Req 1.6 / action 側）
- `DenyReasonUnknownResource`（Req 1.6 / resource 側）

理由: 監査ログでの根本原因解析を容易にし、攻撃検知の精度を上げる（Req 7.2「拒否理由を
構造化フィールドとして含む」の運用価値最大化）。Req 5.3 は「区別可能であること」を
要求しており、細分は要件を満たす方向で逸脱しない。

### 3. cross-tenant 判定と matrix 判定の順序

Authorize は以下の順序で fail-closed を成立させる:

1. unknown audience → DenyReasonAudienceMismatch
2. unknown action → DenyReasonUnknownAction
3. unknown resource → DenyReasonUnknownResource
4. targetTenantID 不正書式 / uuid.Nil → DenyReasonTargetTenantInvalid（Req 4.4 で
   SuperAdmin / admin-console であっても拒否）
5. cross-tenant 判定: audience != admin-console → DenyReasonCrossTenant
6. cross-tenant 判定: SuperAdmin 不在 → DenyReasonCrossTenant
7. matrix lookup（複数 role 兼務時は OR 評価で最も寛容なロールの結果 / Req 5.5）
8. matrix で全 deny: 1 つ以上 known role があれば DenyReasonRoleNotPermitted、
   全 unknown なら DenyReasonUnknownRole

cross-tenant 通過後も matrix 評価に落ちる（admin-console + SuperAdmin が WIPE を発行
することは matrix で deny / design.md Permission Matrix と整合）。

### 4. AuthClaims.Console は string 型（oidc.Console ではなく）

`httpserver.AuthClaims.Console` は `oidc.Console` 型ではなく `string` 型として宣言した。

理由:
- `httpserver` パッケージは `oidc` パッケージへの直接依存を持たない設計（既存 doc.go の
  依存方向ルール）
- AuthClaims は HTTP middleware 層の context 注入用 DTO であり、ドメイン型を持ち込む
  必要がない
- 比較は文字列同値で十分（admin_middleware.go の `adminConsoleAudience` 定数と直接比較）

auth.NewMiddleware は `string(session.Console)` で型変換して転記する。

### 5. authz.Audience も独立型として再定義

`internal/platform/authz/authz.go` の `Audience` 型は `oidc.Console` と同値域だが
独立に宣言した（doc.go の依存方向ルール / authz は errors のみ import）。呼び出し側
（admin middleware / 各 domain handler）は `authz.Audience(claims.Console)` で
型変換して `authz.Request.Audience` に渡す。

### 6. server.go の admin chain の middleware 切替

既存 `RequireSuperAdmin`（IsSuperAdmin のみ判定）から新規 `RequireAdminConsoleAndSuperAdmin`
（audience + SuperAdmin の 2 条件 AND）に切り替えた。`RequireSuperAdmin` 自体は
backward compat のため `admin_middleware.go` に残置するが、本 chain には使用しない。

理由: requirements.md Req 2.4 は「audience != admin-console の場合 403」を要求する。
既存 `RequireSuperAdmin` は IsSuperAdmin だけを判定しており、tenant-console aud で
発行された SuperAdmin セッションが /api/admin/* を通過してしまう（Req 2.4 違反）。

### 7. ログ field 命名（authz_deny_reason）

`/api/admin/*` ガードが構造化 WARN ログに出す field 命名は本 Issue 独自の自然な命名
（`authz_deny_reason`）を採用した（Open Questions の指針 (4)）。Issue #33 の
`failure_kind` / `console` / `session_hash_prefix` は session 検証層の field 名で、
本 Issue の認可層（後段）と意味カテゴリが異なるため、別 field 名を取る方が運用者の
事後追跡で混乱しない。`console` / `actor_id` / `request_id` / `path` / `method`
は Issue #33 既存命名を踏襲する。

なお、本 ガード middleware は session_hash_prefix を出さない（auth middleware が
既に session_hash_prefix 付きの WARN を出している場合は重複しないため）。

## テスト構成

### 単体テスト（authz）

`backend/internal/platform/authz/authz_test.go`:

- `TestAuthorize_Matrix_AllRoles_PrimaryActions`: 4 ロール × 主要 action × 主要 resource
  全 35 ケースの表駆動テスト（Req 1.1〜1.4 / 1.7 / NFR 2.2 / Req 5.5）
- `TestAuthorize_UnknownRole_FailsClosed`: 未知 role が fail-closed（Req 1.5）
- `TestAuthorize_UnknownAction_FailsClosed`: 未知 action が fail-closed（Req 1.6）
- `TestAuthorize_UnknownResource_FailsClosed`: 未知 resource が fail-closed（Req 1.6）
- `TestAuthorize_UnknownAudience_FailsClosed`: 未知 audience が fail-closed（Req 6.5）
- `TestAuthorize_CrossTenant_AdminConsoleSuperAdmin_Allowed`: cross-tenant + admin-console
  + SuperAdmin で allow（Req 3.3）
- `TestAuthorize_CrossTenant_NotAdminConsole_Denied`: cross-tenant + tenant-console で
  cross_tenant 拒否（Req 3.4）
- `TestAuthorize_CrossTenant_NotSuperAdmin_Denied`: cross-tenant + admin-console + 非
  SuperAdmin で cross_tenant 拒否（Req 3.5）
- `TestAuthorize_TargetTenantID_EmptyOrInvalid_FailsClosed`: 空文字 / 不正書式 / partial
  UUID / uuid.Nil が target_tenant_invalid で fail-closed（Req 4.1〜4.3）
- `TestAuthorize_TargetTenantID_FailClosed_PreemptsRoleAndAudience`: SuperAdmin /
  admin-console でも target 空文字なら拒否（Req 4.4）
- `TestAuthorize_MultiRole_TakesMostPermissive`: Operator + Viewer 兼務時に Operator
  経由で command:lock が allow（Req 5.5）
- `TestAuthorize_MultiRole_UnknownPlusKnown_KnownWins`: 未知 + 既知 role の組合せで
  既知 role の結果が採用される（Req 1.5 と 5.5 の境界）
- `TestAuthorize_DenyReasonsAreDistinct`: 7 拒否理由 enum が distinct（Req 5.3）
- `TestAuthorize_Deterministic_NoExternalIO`: 100 回呼んでも同一結果（NFR 1.2）
- `TestAuthorize_SameTenant_NoSuperAdmin_StillAllowedByMatrix`: same-tenant の通常経路
  （Req 3.2）

### 結合テスト（admin_middleware）

`backend/internal/platform/httpserver/admin_middleware_test.go`:

- 既存 `TestRequireSuperAdmin_*` 3 件: backward compat として温存
- 新規 `TestRequireAdminConsoleAndSuperAdmin_*` 5 件: NoSession（Req 2.6） /
  TenantConsole（Req 2.4） / NotSuperAdmin（Req 2.5） / Success（Req 2.3） /
  EmptyConsole（Req 6.5 / legacy claims 後方互換セキュリティ）

### 既存テストへの影響

- `backend/internal/platform/httpserver/server_test.go`: admin chain 切替により
  `TestServer_Admin*` 2 件で SuperAdmin claims に `Console: "admin-console"` を追加
- `backend/test/integration/http_subrouter_mount_test.go`: `RequireSuperAdmin`
  単体挙動の test であり影響なし
- `backend/test/integration/auth_e2e_helpers_test.go`: 独自 `stack.adminMW` を使用
  しており影響なし

## 既存実装（#33）との接合点

- **Session 構造体**: `auth.Session` の `Console oidc.Console` フィールドを認可基盤
  入口の audience として参照する。Session 構造体自体は変えない（NFR 4.1）
- **auth.NewMiddleware**: Service.LookupAndRefresh の戻り値 `(Identity, Session, error)`
  から `session.Console` を取り出して `httpserver.AuthClaims.Console` に転記する
- **httpserver.AuthClaims**: 認可基盤入口の入力契約。Console フィールド追加（後方互換）
- **middleware chain への挿入**: `/api/admin/*` の chain は
  `authMWAdmin` → `TenantContextMiddleware` → `RequireAdminConsoleAndSuperAdmin` の順
  （server.go）。`/api/*`（テナント系）は本 Issue の対象外で変更しない

## AC Traceability

| Req | テスト / 実装箇所 |
|---|---|
| 1.1 | authz_test.go `TestAuthorize_Matrix_AllRoles_PrimaryActions` + `permissionMatrix` 単一テーブル |
| 1.2 | 同上 + `Authorizer.Authorize` の matrix lookup |
| 1.3 / 1.4 | 同上（allow / deny ケース両方を網羅） |
| 1.5 | `TestAuthorize_UnknownRole_FailsClosed` + `isKnownRole` |
| 1.6 | `TestAuthorize_UnknownAction_FailsClosed` / `TestAuthorize_UnknownResource_FailsClosed` + `isKnownAction` / `isKnownResource` |
| 1.7 | `TestAuthorize_Matrix_AllRoles_PrimaryActions` の各ケース（design.md Permission Matrix と整合） |
| 2.1 / 2.2 | `RequireAdminConsoleAndSuperAdmin` の 2 段判定 |
| 2.3 | `TestRequireAdminConsoleAndSuperAdmin_Success_PassesToNext` |
| 2.4 | `TestRequireAdminConsoleAndSuperAdmin_TenantConsole_Returns403` |
| 2.5 | `TestRequireAdminConsoleAndSuperAdmin_AdminConsole_NotSuperAdmin_Returns403` |
| 2.6 | `TestRequireAdminConsoleAndSuperAdmin_NoSession_Returns401` |
| 2.7 | 同上テストの body 検査 + response body に対象リソース ID を含めない実装 |
| 3.1 | `authz.Request` 構造体（Roles / SessionTenantID / Audience / Action / Resource / TargetTenantID） |
| 3.2 | `TestAuthorize_SameTenant_NoSuperAdmin_StillAllowedByMatrix` |
| 3.3 | `TestAuthorize_CrossTenant_AdminConsoleSuperAdmin_Allowed` |
| 3.4 | `TestAuthorize_CrossTenant_NotAdminConsole_Denied` |
| 3.5 | `TestAuthorize_CrossTenant_NotSuperAdmin_Denied` |
| 3.6 | `TestRequireAdminConsoleAndSuperAdmin_NoSession_Returns401` の body 検査（containsTenantPath） |
| 4.1〜4.3 | `TestAuthorize_TargetTenantID_EmptyOrInvalid_FailsClosed`（empty / malformed / partial-uuid / uuid-nil の 4 sub case） |
| 4.4 | `TestAuthorize_TargetTenantID_FailClosed_PreemptsRoleAndAudience` |
| 4.5 | DenyReasonTargetTenantInvalid enum が他と distinct であることを `TestAuthorize_DenyReasonsAreDistinct` で verify |
| 5.1 | `authz.Request` 6 フィールド宣言 |
| 5.2 | `authz.Decision{Allowed, DenyReason}` |
| 5.3 | `TestAuthorize_DenyReasonsAreDistinct`（7 区分の distinct verify） |
| 5.4 | `TestAuthorize_Deterministic_NoExternalIO` |
| 5.5 | `TestAuthorize_MultiRole_TakesMostPermissive` / `TestAuthorize_MultiRole_UnknownPlusKnown_KnownWins` |
| 6.1 | `RequireAdminConsoleAndSuperAdmin` が `AuthClaimsFromContext` 経由で claims を取得 |
| 6.2 | 同 middleware の Console / IsSuperAdmin / AdminUserID 参照経路 |
| 6.3 | `TestRequireAdminConsoleAndSuperAdmin_NoSession_Returns401` |
| 6.4 | 複数 role 兼務時の `TestAuthorize_MultiRole_TakesMostPermissive` |
| 6.5 | `TestAuthorize_UnknownAudience_FailsClosed` |
| 7.1 | `RequireAdminConsoleAndSuperAdmin` が拒否経路で `log.Warn` を呼ぶ |
| 7.2 | `TestRequireAdminConsoleAndSuperAdmin_*` の `assertLogFieldExists` で authz_deny_reason 検査 |
| 7.3 | 本 ガード middleware は session_hash_prefix を出さない（重複回避 / Issue #33 が既に出す） |
| 7.4 | `assertLogFieldExists(t, log, "actor_id", ...)` で AdminUserID field を verify |
| 7.5 | `assertLogFieldExists(t, log, "request_id", ...)` で request_id field を verify |
| NFR 1.1 | 各 fail-closed テスト |
| NFR 1.2 | `TestAuthorize_Deterministic_NoExternalIO` + Authorizer 構造体に状態なし |
| NFR 2.1 | `permissionMatrix` 単一テーブル |
| NFR 2.2 | `TestAuthorize_Matrix_AllRoles_PrimaryActions` の全組合せ網羅 |
| NFR 3.1 | `Authorizer.Authorize` は map index のみで完了 |
| NFR 3.2 | `RequireAdminConsoleAndSuperAdmin` は AuthClaimsFromContext + log のみで I/O なし |
| NFR 4.1 | Session 構造体は変更しない（auth.Session の意味づけ不変） |
| NFR 4.2 | `oidc.Console` 値「tenant-console」「admin-console」をそのまま使う |

## 確認事項（Open Questions と本実装で出てきた追加質問）

requirements.md の Open Questions 4 件と、実装中に出てきた追加 1 件を以下に列挙する。
PM / Architect / 人間レビュワーへの差し戻しが必要な事項。

### 1. admin_users CRUD の実装範囲

requirements.md Open Question 1（admin_users CRUD 対象範囲）に関して、本 Issue では
以下の解釈で実装した:

- **対応 HTTP ルート / handler は実装しない**（本 Issue の Out of Scope）
- **`permissionMatrix` 上には `ResourceAdminUser` エントリを宣言**（SuperAdmin / TenantAdmin
  の 2 ロールが create/update/delete/read を持つ。同テナント内判定は cross-tenant
  ロジックで担保）

後続 Issue で「TenantAdmin が同テナント内 admin_users のみ操作可」を実装する際に
本テーブル宣言を流用する想定。

### 2. Roles vs IsSuperAdmin の参照経路

requirements.md Open Question 2 に関して、本 Issue では以下の方針で実装した:

- **Authorizer.Authorize の入力は `Roles []string`**（canonical）
- **`/api/admin/*` ガード（RequireAdminConsoleAndSuperAdmin）は AuthClaims.IsSuperAdmin
  を参照**（既存 `auth.Identity.IsSuperAdmin` の派生フラグ経由）

理由: `RequireAdminConsoleAndSuperAdmin` は middleware 層で 1 回判定するだけなので、
IsSuperAdmin 派生フラグ参照が経済的（Roles の全要素を走査する必要がない）。一方で
domain handler が個別 action × resource を判定する場合は `Roles` を canonical 入力と
する Authorizer.Authorize を呼ぶ。両者の意味は等価（IsSuperAdmin はある時点で Roles の
集約から導出される派生フラグ）であり、矛盾しない。

### 3. umbrella req 番号体系

requirements.md Open Question 3 に関して、本 Issue では番号引用せずテキスト意味のみ
参照した（PM 確認事項として既に明記済み）。umbrella 起票者側の番号体系是正は本 Issue
範囲外。

### 4. ログ field 命名

requirements.md Open Question 4 に関して、本 Issue では Issue #33 既存規約
（`failure_kind` / `console` / `session_hash_prefix`）と本 Issue 独自命名
（`authz_deny_reason`）の使い分けを採用した（上記「重要な実装判断 #7」参照）。

### 5. （追加質問）AuthClaims に Console を追加する設計判断

本 Issue 実装で `httpserver.AuthClaims` に `Console string` フィールドを追加した
（既存 4 フィールド: TenantID / AdminUserID / Roles / IsSuperAdmin に加えて 1 つ追加）。
Issue #33 で確立された AuthClaims の構造を拡張する形になる。

- 追加内容: Console フィールド（string 型 / "tenant-console" / "admin-console" / 空文字）
- 後方互換: 空文字は legacy claims を表し、RequireAdminConsoleAndSuperAdmin が
  audience_mismatch で 403 fail-closed に倒す
- 影響範囲: 既存 server_test.go の SuperAdmin 系テスト 2 件に Console: "admin-console"
  を明示追加

NFR 4.1（既存 Session 構造体の意味づけを変更しない）は守れている（Session 構造体は
不変 / 追加は AuthClaims 側のみ）。本拡張で問題ないか PM / Architect / 人間レビュワー
に確認したい。

## 検証実行結果

実行コマンド: `cd backend && go build ./... && go vet ./... && go test ./... -count=1`

- `go build ./...`: PASS
- `go vet ./...`: PASS
- `go test ./... -count=1`: 全 package PASS
  - `internal/platform/authz` 0.003s（新規）
  - `internal/platform/httpserver` 0.006s（既存 + 新規テスト）
  - `internal/auth` 0.013s（middleware.go の Console 転記反映後も既存テスト全 pass）
  - `cmd/api` 0.003s
  - `internal/config` / `internal/errors` / `internal/logger` / `internal/platform/db` /
    `internal/platform/oidc` / `test/integration` 全 PASS

STATUS: complete
