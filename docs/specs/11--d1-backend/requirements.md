# Requirements Document

## Introduction

ae-mdm（Android Enterprise EMM SaaS）MVP において、テナント管理者（TenantAdmin）が業務アプリを
Managed Google Play 上で承認し、自テナントの端末群へ配信するための **App ドメイン backend** を定義する。
本 backend は (1) Managed Google Play 承認 UI を iframe 表示するための webToken 発行、(2) 承認結果を
自テナントのアプリカタログ（`tenant_apps`）へ反映する同期、(3) 承認済みアプリカタログの参照、
(4) ポリシーへのアプリ紐付け時に「承認済みカタログ内のアプリのみを受け付ける」不変条件の担保を担う。
本要件は umbrella #24 の Requirement 7（アプリ配信）および同 design.md「App Service」節を、App backend
という実装単位に focus して切り出したものであり、フロントエンド UI と installType の AMAPI 実反映は
本 backend のスコープ外とする（後述 Out of Scope）。

## 関連

- Depends on: #34 #40
- Parent: #24

> 補足: #34（A4a AMAPI Client 共有ラッパ）が提供する `CreateWebToken` 等のテナント分離済み操作 IF に
> 依存し、Managed Google Play / AMAPI への低レベル呼び出し（認証・再試行・エラーマッピング）は当該
> ラッパへ委ねる。installType（FORCE_INSTALLED / AVAILABLE）の AMAPI ポリシー `applications[]` への
> 実反映は #40（B2b Policy Service）の責務であり、本 backend はポリシー編集時の承認済み不変条件のみを
> 担保する。umbrella #24 Requirement 7.5（端末ごとのインストール済みアプリ表示）は Device Service の
> 責務であり本要件には含めない。

## Requirements

### Requirement 1: Managed Google Play iframe 表示用 webToken の発行

**Objective:** As a TenantAdmin, I want Managed Google Play の承認 UI を iframe 表示するための webToken を発行できる, so that 自テナントの文脈で業務アプリの承認画面を開ける

> Traceability: umbrella #24 Req 7.1（承認フローの起点となる iframe 表示）

#### Acceptance Criteria

1. When TenantAdmin が parent_frame_url を指定して iframe 表示用 webToken の発行を要求したとき, the App Service shall 自テナントの enterprise に紐づく webToken（トークン値と有効期限）を発行して返す
2. If parent_frame_url が欠落または空文字で送信されたとき, the App Service shall webToken を発行せず、入力不正として当該要求を拒否する
3. If webToken の発行要求元テナントが Enterprise バインド未完了状態であるとき, the App Service shall webToken を発行せず、当該テナントが未バインドである旨のエラーを返す

### Requirement 2: 承認済みアプリカタログの参照

**Objective:** As a TenantAdmin, I want 自テナントの承認済みアプリカタログを一覧参照できる, so that どのアプリが端末配信の対象候補になっているかを把握できる

> Traceability: umbrella #24 Req 7.1（承認結果がカタログに反映されている状態の参照）

#### Acceptance Criteria

1. When TenantAdmin が自テナントの承認済みアプリカタログ一覧を要求したとき, the App Service shall 自テナントに紐づく承認済みアプリ（パッケージ名・タイトル・アイコン・承認時刻）の一覧を返す
2. While 自テナントの承認済みアプリが 1 件も存在しないとき, the App Service shall 空の一覧を正常応答として返す
3. The App Service shall 承認済みアプリカタログ一覧に他テナントのアプリを一切含めない

### Requirement 3: Managed Google Play 承認結果のカタログ同期

**Objective:** As a TenantAdmin, I want Managed Google Play 上での承認結果を自テナントのアプリカタログに反映できる, so that 承認したアプリを端末配信の対象カタログとして扱える

> Traceability: umbrella #24 Req 7.1（承認結果の自テナントカタログへの反映）

#### Acceptance Criteria

1. When TenantAdmin が承認結果の同期を要求したとき, the App Service shall Managed Google Play 上で自テナントが承認済みのアプリを取得し、自テナントのアプリカタログへ反映する
2. When 同期対象アプリのパッケージ名が自テナントのカタログに既に存在するとき, the App Service shall 重複レコードを作成せず、当該アプリのカタログ情報を更新する
3. When 同期が正常に完了したとき, the App Service shall 反映件数と同期時刻を呼び出し側に返す
4. While 自テナントの承認済みアプリが 1 件も存在しない状態で同期が要求されたとき, the App Service shall カタログを空のまま保ち、反映件数 0 を正常応答として返す
5. If 同期の要求元テナントが Enterprise バインド未完了状態であるとき, the App Service shall 同期を行わず、当該テナントが未バインドである旨のエラーを返す
6. If Managed Google Play からの承認結果の取得が上流エラーで失敗したとき, the App Service shall アプリカタログを更新せず、当該エラーを呼び出し側に伝達する

### Requirement 4: テナント分離（他テナントのアプリへの越境操作拒否）

**Objective:** As a セキュリティ運用者, I want アプリカタログの参照・同期・操作が自テナント境界に閉じる, so that 他テナントの承認済みアプリへの越境操作を防げる

> Traceability: umbrella #24 Req 7.4 / NFR 2.1

#### Acceptance Criteria

1. If TenantAdmin が他テナントの承認済みアプリリストを参照・同期・操作しようとしたとき, the App Service shall 当該操作を拒否する
2. If 他テナントのアプリに対する操作が拒否されたとき, the App Service shall レスポンスに当該アプリの存在有無を区別可能な形で露出しない

### Requirement 5: ポリシー紐付け時の承認済みアプリ不変条件

**Objective:** As a TenantAdmin, I want ポリシーへ紐付けられるアプリが承認済みカタログ内のものに限定される, so that 未承認アプリが誤って端末へ配信される事故を防げる

> Traceability: umbrella #24 Req 7.2 / 7.3 のうち App backend が担う不変条件部分（installType の AMAPI 実反映は #40 Policy Service。後述 Out of Scope）

#### Acceptance Criteria

1. When 承認済みアプリをポリシーへ紐付ける操作（強制インストール（FORCE_INSTALLED）または任意インストール（AVAILABLE）として指定）が行われるとき, the App Service shall 対象アプリが自テナントの承認済みカタログに存在することを検証する
2. If 自テナントの承認済みカタログに存在しないアプリをポリシーへ紐付けようとしたとき, the App Service shall 当該紐付けを拒否する

## Non-Functional Requirements

### NFR 1: セキュリティ（秘密値の非露出・テナント分離の恒常性）

1. The App Service shall iframe 埋め込み用 webToken の値（短命トークン）を構造化ログおよび監査ログに平文で出力しない
2. The App Service shall 任意のテナントの管理者が他テナントの承認済みアプリカタログを参照・同期・操作できない状態を恒常的に保つ

### NFR 2: AMAPI 呼び出しの共有経路

1. The App Service shall Managed Google Play / AMAPI への呼び出し（webToken 発行・承認カタログ取得）を共有 AMAPI クライアントラッパ経由でのみ行い、AMAPI 認証・再試行・エラーマッピングを独自に再実装しない

### NFR 3: 監査・可観測性

1. The App Service shall アプリカタログの同期実行を、実行者・テナント識別子・反映件数・結果とともに監査ログ記録の対象として渡す
2. The App Service shall 構造化ログにサービスアカウント資格情報・OAuth トークンの生値を含めない

## Out of Scope

- フロントエンド（tenant-console `features/apps`）のアプリ配信 UI — Managed Play iframe の埋め込み枠・カタログ表示・ポリシー割当 UI（#17 の範囲）
- Managed Google Play iframe 内部 UI の再実装 — Google 提供であり再デザイン・再実装しない（本 backend は iframe 表示に必要な webToken を発行するのみ）
- installType（FORCE_INSTALLED / AVAILABLE）の AMAPI ポリシー `applications[]` への実反映 — #40（B2b Policy Service）の責務。本 backend は承認済み不変条件（Requirement 5）の担保のみを負う
- 端末ごとのインストール済み管理対象アプリ一覧の表示（umbrella #24 Req 7.5）— Device Service の責務
- 限定公開／プライベートアプリの自社開発フローおよび Web アプリ（PWA ショートカット）配信 — MVP 対象外
- AMAPI Client 共有ラッパ自体の実装（`CreateWebToken` 等の認証・再試行・エラーマッピング）— #34（A4a AMAPI Client）で実装済み。本 backend は当該 IF を呼び出すのみ
- 監査ログテーブルへの永続化ロジック自体 — #5（A5 Audit Service）で実装済み。本 backend は記録対象イベントを Audit Service に渡すのみ
- RBAC 許可マトリクス・OIDC 認証・セッション発行・DB Row-Level Security ポリシーの定義 — 各基盤 Issue（#33 / #37 / umbrella #24 tasks 2.3）の範囲。本 backend はテナント分離の前提としてこれらに依拠する

## Open Questions

> 注: Issue #11 の既存コメントは Triage の edit_paths 自動生成コメントと Claude 処理開始通知の 2 件のみで、
> 人間による決定事項の回答は存在しない。以下は設計・スコープ境界に関わる論点であり、推測で確定せず
> Architect / PR レビューで人間判断を仰ぐ。

- **同期のカタログ取得経路（Requirement 3.1）**: 現状の共有 AMAPI クライアント（#34）には承認済みアプリカタログの一覧取得メソッドが存在せず（`CreateWebToken` のみ）、`POST /api/apps/sync` がどこから承認結果を取得して `tenant_apps` に反映するか（フロントエンドが Play iframe から選択済み package を渡す／AMAPI に list メソッドを追加する／別経路 等）が未確定。本要件では Requirement 3.1 を「Managed Google Play 上で自テナントが承認済みのアプリを取得し反映する」抽象レベルで記述し、取得経路は design 段階で確定したい
- **`POST /api/apps/sync` のリクエスト形状**: body 有無・package リスト受領の要否は上記の取得経路に依存するため要件では確定しない。umbrella #24 design.md の暫定 API Contract は「Request: —（body なし）→ {synced_at, count}」だが、取得経路の確定に伴い変わり得る
- **installType 反映の責務境界（Requirement 5）**: 7.2 / 7.3 の installType 反映が App backend に新規エンドポイントを要するか、既存 Policy Service（#40）の `applications[]` 編集経路 + App Service の承認済みチェックで充足されるかが未確定。本要件では App backend が担う部分を「承認済みアプリ限定の不変条件」として Requirement 5 に切り出し、AMAPI 実反映は Out of Scope に寄せた。不変条件の enforcement 位置（App Service が Policy Service から呼ばれる／Policy Service 内で App カタログを参照する 等）は design で確定したい
- **アプリカタログ同期の監査記録（NFR 3.1）**: umbrella #24 Requirement 7 は app 承認・同期の監査記録を明示していない。umbrella design.md は App Service の `audit.Service` 依存を Important として挙げるため本要件で NFR 3.1 を仮置きしたが、記録対象イベントの粒度（同期実行単位／個別アプリ追加単位）と要否は要確認
