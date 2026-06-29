# Requirements Document

## Introduction

ae-mdm（Android Enterprise EMM SaaS）MVP において、テナント管理者（TenantAdmin）が作成・更新した
Android Management ポリシーを実端末へ届けるための **Policy ドメイン Service** を定義する。本サービスは
ポリシーの作成・更新（upsert）、検証（Policy Validator / #36 の呼び出し）、AMAPI への反映、ポリシー
snapshot の保持、端末へのポリシー割当、各変更の監査ログ記録を担う。検証ロジック内部（5 領域の妥当性
判定 / #36 で実装済み）とポリシー編集 UI（5 領域タブ / tenant-console Issue）は本サービスのスコープ外で
あり、本要件は umbrella #24 の Requirement 4（ポリシー管理）および同 design.md「Policy Service」節を、
Policy Service という実装単位に focus して切り出したものである。

## 関連

- Depends on: #34 #36
- Parent: #24

> 補足: #36（B2a Policy Validator）が提供する `policy.Validate(PolicyInput) ValidationResult` を
> 保存前の検証ゲートとして呼び出す。Raw JSON → ドメイン入力型（PolicyInput）への変換は Policy Service の
> 責務である（#36 doc.go に明記）。#34（A4a AMAPI Client 共有ラッパ）が提供する `UpsertPolicy` /
> `GetPolicy` 等のテナント分離済み操作 IF に依存し、AMAPI への低レベル呼び出し（認証・再試行・
> エラーマッピング）は当該ラッパに委ねる。

## Requirements

### Requirement 1: ポリシーの作成・更新（upsert）と AMAPI 反映・snapshot 保持

**Objective:** As a TenantAdmin, I want 自テナントのポリシーを作成・更新できる, so that テナント内の端末を一貫した管理ルール下に置くための設定を確定できる

#### Acceptance Criteria

1. When TenantAdmin が新規ポリシーを作成して保存を要求し、検証を通過したとき, the Policy Service shall 当該ポリシーを自テナントの enterprise 配下に AMAPI 経由で upsert する
2. When TenantAdmin が既存ポリシーの更新を要求し、検証を通過したとき, the Policy Service shall 更新後の内容を AMAPI に反映し、当該ポリシーが割り当て済みの端末へ新しい内容が配信される前提を満たす
3. When ポリシーの AMAPI 反映が成功したとき, the Policy Service shall 当該ポリシーの AMAPI ポリシー名と本体 JSON snapshot を自テナント識別子に紐づけて永続化する
4. If ポリシーの AMAPI 反映が再試行不可なエラーとして返されたとき, the Policy Service shall snapshot を当該失敗内容で確定保存せず、当該エラーを呼び出し側に伝達する
5. While 同一ポリシーに対する更新が連続して行われているとき, the Policy Service shall 永続化した snapshot を最新の反映済み内容と一致させ、AMAPI 反映前の中間状態を確定 snapshot として残さない

### Requirement 2: 保存前の検証ゲート（上限超過・必須項目不正の拒否）

**Objective:** As a TenantAdmin, I want 不正なポリシーが保存・反映される前に弾かれ、不正項目が提示される, so that 端末へ不正な設定が配信される事故を防ぎ、修正箇所を把握できる

#### Acceptance Criteria

1. When TenantAdmin がポリシーの作成または更新を要求したとき, the Policy Service shall AMAPI 反映および snapshot 保存の前に Policy Validator による検証を実行する
2. If 1 ポリシーあたりのアプリ件数が上限（3,000 件）を超えているとき, the Policy Service shall 当該ポリシーの保存・反映を拒否し、上限超過である旨を呼び出し側に提示する
3. If パスワード最小桁数・Kiosk 指定アプリのパッケージ名などの必須項目が不正な値であるとき, the Policy Service shall 当該ポリシーの保存・反映を拒否し、不正項目を呼び出し側に提示する
4. When 検証で複数の不正項目が検出されたとき, the Policy Service shall 検出されたすべての不正項目を呼び出し側に提示する
5. If 検証で 1 件以上の不正項目が検出されたとき, the Policy Service shall AMAPI への反映および snapshot の保存を一切行わない

### Requirement 3: ポリシーの端末割当

**Objective:** As a TenantAdmin, I want 作成済みポリシーを自テナントの端末に割り当てられる, so that 対象端末を当該ポリシーの管理下に置ける

#### Acceptance Criteria

1. When TenantAdmin が自テナントの端末に対して自テナントのポリシーを割り当てたとき, the Policy Service shall 当該端末の適用対象ポリシーを当該ポリシーとして確定する
2. If 割当要求で指定されたポリシーが自テナントに存在しないとき, the Policy Service shall 当該割当を拒否し、未検出として呼び出し側にエラーを返す
3. If 割当要求で指定された端末が自テナントに存在しないとき, the Policy Service shall 当該割当を拒否し、未検出として呼び出し側にエラーを返す
4. While あるポリシーが端末に割り当て済みであるとき, the Policy Service shall 当該ポリシーの更新内容が当該端末への配信対象として扱われる前提を満たす

### Requirement 4: テナント分離（他テナントのポリシー・端末への操作拒否）

**Objective:** As a セキュリティ運用者, I want ポリシーの参照・更新・割当が自テナント境界に閉じる, so that 他テナントのポリシー・端末への越境操作を防げる

#### Acceptance Criteria

1. If TenantAdmin が他テナントのポリシーに対する更新を要求したとき, the Policy Service shall 当該操作を拒否する
2. If TenantAdmin が他テナントのポリシーを割当対象として指定したとき, the Policy Service shall 当該割当を拒否する
3. If TenantAdmin が他テナントの端末を割当先として指定したとき, the Policy Service shall 当該割当を拒否する
4. The Policy Service shall ポリシーの一覧・参照に対して、自テナントに属するポリシーのみを返す
5. If 他テナントのポリシーまたは端末に対する操作が拒否されたとき, the Policy Service shall レスポンスに当該リソースの存在有無を区別可能な形で露出しない

### Requirement 5: ポリシー変更の監査ログ記録

**Objective:** As a 監査担当者, I want ポリシーの作成・更新・削除が監査ログに記録される, so that 誰がいつどのポリシーを変更したかを事後追跡できる

#### Acceptance Criteria

1. When ポリシーが作成されたとき, the Policy Service shall 当該作成イベント（実行者・テナント識別子・ポリシー識別子・結果）を監査ログ記録の対象として渡す
2. When ポリシーが更新されたとき, the Policy Service shall 当該更新イベント（実行者・テナント識別子・ポリシー識別子・結果）を監査ログ記録の対象として渡す
3. When ポリシーが削除されたとき, the Policy Service shall 当該削除イベント（実行者・テナント識別子・ポリシー識別子・結果）を監査ログ記録の対象として渡す
4. The Policy Service shall 監査ログに渡す詳細に、ID トークン・セッション cookie の生値・サービスアカウント資格情報などの機密値を含めない

## Non-Functional Requirements

### NFR 1: プラットフォーム互換性

1. The Policy Service shall Android 10 以上を実行する端末へのポリシー配信を前提として、AMAPI へのポリシー反映と端末割当をサポートする

### NFR 2: 型安全な永続化と AMAPI 呼び出しの共有経路

1. The Policy Service shall ポリシー snapshot の永続化を、列ごとに型付けされた値の読み書きとして行い、型不整合を実行時エラーまで遅延させない
2. The Policy Service shall AMAPI へのポリシー反映を共有 AMAPI クライアントラッパ経由でのみ行い、ポリシー反映のために AMAPI 認証・再試行・エラーマッピングを独自に再実装しない

### NFR 3: 監査・可観測性

1. The Policy Service shall ポリシーの作成・更新・削除・割当のうち拒否された操作について、原因分析に必要な属性（実行者・対象テナント・対象リソース・拒否理由）を構造化ログとして記録する
2. The Policy Service shall ログ出力にポリシー本体 JSON の機密パラメータ・サービスアカウント資格情報・OAuth トークンの生値を含めない

## Out of Scope

- Policy Validator（5 領域の検証ロジック内部）の実装 — #36（B2a Policy Validator）で実装済み。本サービスは
  保存前に `policy.Validate(PolicyInput)` を呼び出すのみで、検証規則自体は追加・変更しない
- ポリシー編集 UI（アプリ / パスワード / セキュリティ / システム更新 / Kiosk の 5 領域タブ）—
  tenant-console `features/policies` Issue（umbrella #24）の範囲。umbrella Req 4.2（5 領域の設定項目提供）は
  UI 側の責務であり本サービスのスコープ外
- AMAPI Client 共有ラッパ自体の実装（`UpsertPolicy` / `GetPolicy` の認証・再試行・エラーマッピング）—
  #34（A4a AMAPI Client）で実装済み。本サービスは当該 IF を呼び出すのみ
- 監査ログテーブルへの永続化ロジック自体 — #5（A5 Audit Service）で実装済み。本サービスは記録対象イベントを
  Audit Service に渡すのみ
- RBAC 許可マトリクス・OIDC 認証・セッション発行・DB Row-Level Security ポリシーの定義 — 各基盤 Issue
  （#33 / #37 / umbrella #24 tasks 2.3）の範囲。本サービスはテナント分離の前提としてこれらに依拠する
- 端末コンプライアンス状態・適用状態（`appliedState` 等）の可視化、STATUS_REPORT 通知に基づくポリシー
  適用結果の反映 — Device Service / Notification Handler Issue の範囲
- Managed Google Play アプリ承認カタログとアプリのポリシーへの紐付け（FORCE_INSTALLED / AVAILABLE）—
  App Service Issue（umbrella #24 Req 7）の範囲
- ポリシー削除に伴う割当済み端末のカスケード処理（割当解除・代替ポリシー再割当）の詳細仕様

## 確認事項 / 未決事項

> 注: Issue #40 の既存コメントは Path Overlap Checker / dispatch 等の自動 bot コメントのみで、人間による
> 決定事項の回答は存在しない。以下は設計・スコープ境界に関わる論点であり、推測で確定せず PR レビューで
> 人間判断を仰ぐ。

- **端末割当（Requirement 3）の AMAPI 反映経路**: umbrella #24 design.md では端末へのポリシー割当
  エンドポイントは `PUT /api/devices/{id}/policy`（Device Service 所管 / 別 Issue・未実装）として描かれ、
  かつ既存 AMAPI Client 共有ラッパには device → policy を patch する操作 IF が存在しない
  （`GetDevice` / `ListDevices` / `IssueCommand` のみ）。本 Issue の「割当」が
  (a) 端末側の適用対象ポリシーの DB 更新（`devices.applied_policy_id` の確定）までか、
  (b) AMAPI への device patch までを含むか、
  (c) 割当エンドポイントを Policy Service 側に持つか Device Service 側に委ねるか、
  が未確定。本要件では Requirement 3 を「端末の適用対象ポリシーを確定する」抽象レベルで記述し、AMAPI への
  device patch 経路の有無は確定していない。実装単位の境界を design 段階で確定したい
- **永続化方式**: Issue #40 の技術制約は「sqlc で型安全な生 SQL」と記載されるが、既に実装済みの
  ドメイン Service（Tenant Service 等）は sqlc を導入せず手書きの raw SQL（トランザクション境界を
  明示する方式）を採用している。Policy Service で sqlc を新規導入するか、既存実装の慣習を踏襲して
  手書き raw SQL とするかの整合方針が未確定。本要件では NFR 2.1 を「型安全な永続化」という observable な
  性質として記述するに留め、sqlc 採否は design / 実装判断に委ねる
- **ポリシー削除の競合制御**: umbrella #24 design.md の API Contract では `DELETE /api/policies/{id}` が
  409（Conflict）を返し得る。割当済み端末が存在するポリシーの削除を拒否（409）するか、割当解除を伴って
  許可するかは未確定。本要件では Requirement 5.3 で削除イベントの監査記録のみを定義し、削除可否の競合
  条件は Out of Scope（カスケード処理の詳細仕様）に寄せた。MVP での削除可否ポリシーを確認したい
- **必須項目の網羅範囲**: Requirement 2.3 では「パスワード最小桁数・Kiosk 指定アプリのパッケージ名など」を
  例示したが、検証対象となる必須項目・範囲・enum 許容値の網羅は #36 Validator の実装に従う。Policy Service
  側で Raw JSON → PolicyInput への変換時に追加の必須性チェックを課すか否か（変換不能を不正入力として
  扱う粒度）が未確定で、design で変換契約を確定したい
