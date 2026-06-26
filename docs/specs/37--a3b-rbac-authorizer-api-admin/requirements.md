# Requirements Document

## Introduction

本要件は ae-mdm（Android Enterprise EMM SaaS）MVP の **認可基盤（A3b）** を定義する。
Umbrella Issue #24 の Requirement 2.3〜2.7 / 4.10（他テナント越境拒否）/ 5.8 / 6.1（fail-closed）
および design.md「Authorization (RBAC) Service」節の許可マトリクスを実装単位の要件として
切り出し、4 ロール（SuperAdmin / TenantAdmin / Operator / Viewer）× action × resource の
表駆動許可マトリクスを実現し、運用系ルート `/api/admin/*` を **admin-console audience かつ
SuperAdmin** の 2 条件 AND ガードで保護する責務を担う。

本基盤は前提として Issue #33（A3a）の OIDC Verifier + Session 管理が確立する Session
（`AdminUserID` / `TenantID` / `Roles` / `IsSuperAdmin` / `Console` 等の属性）を入力契約とする。
本 Issue は「ある認証済みリクエストが、role / target tenant / audience の組合せに対して、
要求された action × resource を実行してよいか」を判定する Authorizer と、`/api/admin/*` の
ルート群に固定で挟まる SuperAdmin ガードの 2 つを範囲とする。OIDC 検証・セッション発行・
画面側のロール出し分け・管理者ユーザー管理 UI 等は本 Issue のスコープ外とする。

## 関連

- Depends on: #33
- Parent: #24

## Requirements

### Requirement 1: 表駆動の許可マトリクス（4 ロール × action × resource）

**Objective:** As a 認可基盤の利用側（HTTP ハンドラ / domain Service）, I want 4 ロールごとの
許可関係を単一の宣言的なテーブルから判定できる, so that 各ハンドラに permission check を散在
させずに整合的な RBAC を実装できる

#### Acceptance Criteria

1. The Authorizer shall SuperAdmin / TenantAdmin / Operator / Viewer の 4 ロールに対して、
   action × resource の許可関係を **単一の定数テーブル（表駆動）** として保持する
2. The Authorizer shall 許可テーブルにロール・action・resource を入力すると、ブール相当の
   allow / deny を一意に返す
3. When 認証済みリクエストの role に対して action × resource の組合せが許可テーブルで
   allow と宣言されているとき, the Authorizer shall 当該操作を許可する
4. When 認証済みリクエストの role に対して action × resource の組合せが許可テーブルで
   deny と宣言されているとき, the Authorizer shall 当該操作を拒否する
5. If 認証済みリクエストの role が許可テーブルに定義されていない未知の値であるとき,
   the Authorizer shall 当該操作を拒否する（fail-closed）
6. If 認証済みリクエストの action または resource が許可テーブルに定義されていない未知の
   値であるとき, the Authorizer shall 当該操作を拒否する（fail-closed）
7. The Authorizer shall Umbrella Issue #24 design.md「Authorization (RBAC) Service」節の
   許可マトリクス（policy:create / update / delete = TenantAdmin のみ、command:wipe =
   TenantAdmin のみ、command:lock / reboot = TenantAdmin と Operator、tenant:create / delete =
   SuperAdmin のみ、audit_log:read cross-tenant = SuperAdmin のみ 等）と一致する許可関係を
   保持する

### Requirement 2: `/api/admin/*` ガード（admin-console audience かつ SuperAdmin の 2 条件 AND）

**Objective:** As a セキュリティ運用者, I want 運用系ルート `/api/admin/*` が「admin-console
コンソール由来のセッション」かつ「SuperAdmin ロール」の 2 条件を AND で満たすリクエストのみを
通過させる, so that tenant-console 経由で取得したセッションや非 SuperAdmin の昇格試行による
運用系操作を物理的に防げる

#### Acceptance Criteria

1. The `/api/admin/*` ガード shall リクエストの認証済みセッションに紐付くコンソール種別
   （audience）が `admin-console` であるかを判定する
2. The `/api/admin/*` ガード shall リクエストの認証済みセッションの role が SuperAdmin で
   あるかを判定する
3. When `/api/admin/*` 配下のルートに到達したリクエストの audience が `admin-console` かつ
   role が SuperAdmin であるとき, the `/api/admin/*` ガード shall 当該リクエストを後続の
   ハンドラへ通過させる
4. If `/api/admin/*` 配下のルートに到達したリクエストの audience が `admin-console` 以外
   （`tenant-console` 等）であるとき, the `/api/admin/*` ガード shall 当該リクエストを 403 で
   拒否する
5. If `/api/admin/*` 配下のルートに到達したリクエストの role が SuperAdmin 以外であるとき,
   the `/api/admin/*` ガード shall 当該リクエストを 403 で拒否する
6. If `/api/admin/*` 配下のルートに到達したリクエストに認証済みセッションが確立されていない
   とき, the `/api/admin/*` ガード shall 当該リクエストを 401 で拒否する
7. If `/api/admin/*` ガードが対象リソースを拒否したとき, the `/api/admin/*` ガード shall
   レスポンス body に対象リソースの識別子・存在有無を露出しない（Umbrella Req 1.5 / 5.5
   と整合）

### Requirement 3: cross-tenant 操作の許可判定（targetTenantID の評価）

**Objective:** As a SaaS 運営者, I want 認証済みリクエストの所属テナントと異なる target tenant
に対する操作（cross-tenant 操作）が、admin-console 由来の SuperAdmin に限って許可される,
so that 他テナント越境による情報漏洩・権限昇格を防げる

#### Acceptance Criteria

1. The Authorizer shall 入力として認証済みセッションの role / 所属 tenant / audience に加え、
   操作対象の target tenant 識別子（targetTenantID）を受け取る
2. When 認証済みセッションの所属 tenant と targetTenantID が一致する（同テナント内操作）
   とき, the Authorizer shall Requirement 1 の許可マトリクスの判定結果に従って許可可否を
   決定する
3. When 認証済みセッションの所属 tenant と targetTenantID が一致しない（cross-tenant 操作）
   とき, the Authorizer shall 当該操作を audience が `admin-console` かつ role が SuperAdmin
   の場合に限り許可する
4. If cross-tenant 操作のリクエストにおいて audience が `admin-console` 以外であるとき,
   the Authorizer shall 当該操作を拒否する
5. If cross-tenant 操作のリクエストにおいて role が SuperAdmin 以外であるとき,
   the Authorizer shall 当該操作を拒否する
6. When Authorizer が cross-tenant 操作を拒否したとき, the Authorizer shall 対象リソースの
   存在有無を呼び出し側に区別可能な形で返却しない（Umbrella Req 1.5 / 5.5 と整合）

### Requirement 4: targetTenantID の fail-closed 検証

**Objective:** As a セキュリティ運用者, I want targetTenantID が空・null・未指定など曖昧な
入力に対して、Authorizer が安全側（拒否）に倒れる, so that 入力欠落・改竄・ロジック誤りに
起因する越境操作の見落としを防げる

#### Acceptance Criteria

1. If Authorizer に渡された targetTenantID が空文字または null であるとき,
   the Authorizer shall 当該操作を拒否する（fail-closed）
2. If Authorizer に渡された targetTenantID が呼び出し側 API の入力契約で必須であるにも
   関わらず未指定であるとき, the Authorizer shall 当該操作を拒否する（fail-closed）
3. If Authorizer に渡された targetTenantID が UUID 等の所定書式として不正であるとき,
   the Authorizer shall 当該操作を拒否する（fail-closed）
4. While targetTenantID が fail-closed として拒否されているとき, the Authorizer shall
   ロールや audience が SuperAdmin / admin-console であっても操作を許可しない
5. The Authorizer shall targetTenantID の fail-closed 拒否を、許可マトリクスでの deny
   判定とは区別可能な拒否理由として呼び出し側に返却する

### Requirement 5: Authorizer の入力契約と返り値

**Objective:** As a Authorizer の呼び出し側（HTTP ハンドラ / middleware / domain Service）,
I want Authorizer の入出力契約が明示されている, so that ハンドラ内に permission check を
散在させずに整合的な呼び出しが書ける

#### Acceptance Criteria

1. The Authorizer shall 1 回の許可判定の入力として、ロール（または役割集合）・所属
   テナント識別子・audience（コンソール種別）・action・resource・targetTenantID の 6 種を
   受け取る
2. The Authorizer shall 1 回の許可判定の返り値として、allow / deny の判定結果と、
   deny の場合は拒否理由を区別可能な形式で返却する
3. The Authorizer shall 拒否理由として少なくとも以下を区別可能にする: 「role による deny」
   「cross-tenant 越境による deny」「audience 不一致による deny」「targetTenantID の
   fail-closed 拒否」「未知の role / action / resource による fail-closed 拒否」
4. The Authorizer shall 同一入力に対して常に同一の判定結果を返す（外部状態に依存しない決定論的
   判定）
5. The Authorizer shall 入力の役割集合が複数のロールを含む場合、いずれかのロールで allow
   となる action × resource を allow と判定する（最も寛容なロールの結果を採用する）

### Requirement 6: 既存 Session（Issue #33）との結合

**Objective:** As a HTTP middleware 設計者, I want Authorizer と `/api/admin/*` ガードが、
Issue #33 で確立された Session 構造から role / audience / tenant 情報を取り出して判定できる,
so that 認証層と認可層の責務分離を保ったまま結合できる

#### Acceptance Criteria

1. The `/api/admin/*` ガード shall Issue #33 で確立された認証済みセッションが request
   context に注入されている前提で動作する
2. When `/api/admin/*` ガードが認可判定を行うとき, the `/api/admin/*` ガード shall Session
   から audience（コンソール種別）・role（Roles / IsSuperAdmin）・所属テナント識別子を取り出す
3. If 認証済みセッションが request context に確立されていないとき, the `/api/admin/*` ガード
   shall 当該リクエストを 401 で拒否し、後続の認可判定を行わない
4. The Authorizer shall Session に紐付くロールが複数（例: TenantAdmin と Operator を兼務）
   であっても、Requirement 5.5 の決定論で判定する
5. The Authorizer shall Session の audience 値 `tenant-console` / `admin-console` のいずれかを
   入力としてそのまま受け取り、未知の audience 値を fail-closed で拒否する

### Requirement 7: 監査・観測性（denied アクセスのログ）

**Objective:** As a 運用者, I want 拒否された認可判定が、原因分析に必要な属性とともに構造化
ログに記録される, so that 越境攻撃・設定誤りの兆候を事後追跡できる

#### Acceptance Criteria

1. When Authorizer または `/api/admin/*` ガードが認可判定で deny を返したとき, the 認可
   基盤 shall 拒否事象を構造化ログとして WARN レベルで記録する
2. The 拒否事象ログ shall role（または role 集合）・audience・action・resource・targetTenantID・
   拒否理由（Requirement 5.3 で定義した区分）の各属性を構造化フィールドとして含む
3. The 拒否事象ログ shall 対象セッションの識別子の生値を含めず、Issue #33 で確立された
   短縮ハッシュ表現（session_hash_prefix 相当）のみを含む
4. The 拒否事象ログ shall 認可判定の対象 admin 識別子（AdminUserID）を構造化フィールドと
   して含む
5. The 拒否事象ログ shall リクエストの相関 ID（request_id）を構造化フィールドとして含む

## Non-Functional Requirements

### NFR 1: fail-closed と決定論性

1. If 認可判定に必要な入力（role / audience / targetTenantID / action / resource）の
   いずれかが未指定・未知値・想定外の例外で取得できないとき, the 認可基盤 shall 当該リクエストを
   拒否し、認証成功・認可成功として扱わない
2. The Authorizer shall 同一入力に対して同一の判定結果を返し、判定中に外部 I/O（DB アクセス・
   ネットワーク呼び出し）を発生させない

### NFR 2: 表駆動マトリクスの保守性

1. The Authorizer shall 4 ロール × 全 action × 全 resource の許可関係を、コード差分のみで
   追加・変更可能な単一の宣言テーブルから読み取る
2. The Authorizer shall 許可マトリクスの内容を、自動テストで 4 ロール × 主要 action × 主要
   resource の全組合せで検証可能な形式で保持する

### NFR 3: 性能

1. The Authorizer shall 1 回の許可判定を、許可マトリクスに対する定数時間（O(1)）の参照で
   完了する
2. The `/api/admin/*` ガード shall per-request の副作用として、許可テーブル参照・Session
   読み出し・構造化ログ出力以外の I/O を発生させない

### NFR 4: 既存実装との互換性

1. The 認可基盤 shall Issue #33（A3a）で確立された Session 構造体（AdminUserID / TenantID /
   Roles / IsSuperAdmin / Console）の意味づけを変更せずに利用する
2. The 認可基盤 shall Umbrella Issue #24 design.md で `tenant-console` / `admin-console` の 2
   値として宣言された audience の語彙をそのまま使用し、独自の audience 値を導入しない

## Out of Scope

- OIDC ID トークン検証・OIDC 認可コードフロー・state 保護・セッション cookie 発行
  （Issue #33 で完了済み）
- 画面側のロール出し分け（tenant-console / admin-console UI 側での非活性化・非表示制御）
- 管理者ユーザー管理（admin_users / admin_role_assignments の作成・更新・削除）の UI および
  API
- ロール割り当ての永続化レイヤ（admin_role_assignments テーブル定義および永続化操作）
- Authorizer による DB Row-Level Security の発火制御（DB 側 RLS は Issue #2 で確立済みで、
  本 Issue ではアプリ層の RBAC のみを範囲とする）
- 監査ログテーブル（`audit_logs`）への永続化（本 Issue の Requirement 7 は構造化ログへの
  記録までで、`audit_logs` テーブル INSERT は umbrella Req 2.9 / NFR 4.2 の後続 Issue 範囲）
- WIPE 等の不可逆操作に対する二段階確認 UI（Umbrella NFR 4.1 の後続 Issue 範囲）

## Open Questions

- Umbrella Issue #24 design.md「Authorization (RBAC) Service」節の許可マトリクスでは
  `admin_user: create / update_role / delete` が「SuperAdmin = yes (any)」「TenantAdmin =
  yes (own tenant)」と記載されているが、本 Issue では同 row を「TenantAdmin は自テナント
  内 admin_users のみ操作可」と解釈してよいか、それとも admin_users 管理は本 Issue の対象外
  （後続 Issue で扱う）か、確認したい
- Issue #33 の Session には `Roles []string` と派生フラグ `IsSuperAdmin bool` の両方が存在
  するが、本 Issue の Authorizer は `Roles` を canonical な入力として用いる前提で要件化した。
  ガード側で `IsSuperAdmin` を direct に参照する経路を残すか、`Roles` 経由に一本化するかは
  Architect / 設計判断に委ねる
- Issue #37 本文が言及する Umbrella Req 番号（2.3〜2.7 / 4.10 / 5.8 / 6.1）のうち、
  Umbrella requirements.md 側に 4.10 / 5.8 / 6.1 という番号が直接存在しない（NFR 2.1 / 2.3
  に相当する記述が散在）。本 Issue ではテキスト上の意味（fail-closed 越境拒否・Authorizer
  の audience 判定）を読み取って要件化したが、umbrella 起票者側で番号体系の整合をとる必要が
  あるかを確認したい
- 拒否事象ログ（Requirement 7）の出力フォーマット（field key 名）について、Issue #33 の
  `failure_kind` / `console` / `session_hash_prefix` 等の既存命名規約と揃えるべきか、本 Issue
  独自の命名（例: `authz_deny_reason`）を採用するかを確認したい
- Authorizer の Cross-tenant 判定（Requirement 3）において、API ルートとして `/api/admin/*`
  以外（例: tenant-console 経由の `/api/...` ルート）から targetTenantID が指定された場合の
  挙動は、本 Issue では「cross-tenant 越境として拒否」と解釈した。tenant-console 経由で
  targetTenantID を許容する API は MVP 時点で存在しない想定だが、明示確認したい
