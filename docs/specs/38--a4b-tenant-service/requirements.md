# Requirements Document

## Introduction

ae-mdm（Android Enterprise EMM SaaS）MVP のマルチテナント基盤として、SaaS 運用者（SuperAdmin）が
admin-console から顧客企業ごとのテナントを作成し、当該テナント専用の Android Enterprise を
バインドし、テナントの状態（バインド未完了 / バインド済み / 無効化）を管理する **Tenant ドメイン
Service** を定義する。Tenant 文脈が確定しないと各機能（エンロール・ポリシー・端末・コマンド・
アプリ・監査）のテナント分離が成立しないため、本サービスはマルチテナント全機能の前提となる。
本要件は umbrella #24 の Requirement 1（テナント管理 / Enterprise バインド）および同 design.md
「Domain Layer / Tenant Service」節を実装単位の要件として切り出したものであり、Enterprise
バインドのオーケストレーションロジックとテナント状態管理に範囲を限定する。AMAPI への低レベル
呼び出し自体（#34 で実装済み）と admin-console のテナント管理 UI（別 Issue）は本サービスの
スコープ外とする。

## 関連

- Depends on: #34 #37
- Parent: #24

> 補足: #34（A4a AMAPI Client 共有ラッパ）が提供する `CreateSignupURL` / `CreateEnterprise`
> 等のテナント分離済み操作 IF に依存する。#37（A3b RBAC Authorizer + `/api/admin` ガード）が
> 提供する「admin-console audience かつ SuperAdmin」の 2 条件 AND ガードに依存し、本サービスの
> 全エンドポイントは当該ガード配下に配置される。

## Requirements

### Requirement 1: テナント作成

**Objective:** As a SuperAdmin, I want admin-console から新規テナントを作成できる, so that 顧客企業ごとに独立したテナント環境の起点を用意できる

#### Acceptance Criteria

1. When SuperAdmin が新規テナント作成を要求したとき, the Tenant Service shall テナントレコードを生成し、その状態を「バインド未完了（pending_bind）」として保存する
2. When テナントレコードを生成したとき, the Tenant Service shall 当該テナント専用の Enterprise バインドを開始するためのサインアップ URL を生成して呼び出し側に返す
3. If テナント作成要求にテナント名が空または欠落しているとき, the Tenant Service shall 当該要求を実行せず、不正入力として呼び出し側にエラーを返す
4. If サインアップ URL の生成が AMAPI Client から再試行不可なエラーとして返されたとき, the Tenant Service shall テナントを「バインド未完了」状態のまま保持し、当該エラーを呼び出し側に伝達する
5. When テナント作成が完了したとき, the Tenant Service shall 当該作成イベント（実行者・テナント識別子・結果）を監査ログ記録の対象とする

### Requirement 2: Enterprise バインド

**Objective:** As a SuperAdmin, I want 作成したテナントに Android Enterprise をバインドできる, so that 当該テナントに対する AMAPI 操作（端末・ポリシー等）を実行可能にできる

#### Acceptance Criteria

1. When SuperAdmin が「バインド未完了」状態のテナントに対して Enterprise バインドを要求したとき, the Tenant Service shall AMAPI Client 経由で Enterprise を作成し、返却された enterprise 識別子を当該テナントに保存する
2. When Enterprise バインドが成功したとき, the Tenant Service shall 当該テナントの状態を「バインド済み（bound）」へ遷移させる
3. While テナントが「バインド済み」状態であるとき, the Tenant Service shall 以後の当該テナント向け AMAPI 呼び出しで保存済みの enterprise 識別子を用いる前提を満たすために、当該識別子を呼び出し側が参照可能な形で保持する
4. If Enterprise の作成またはバインドが AMAPI Client から失敗として返されたとき, the Tenant Service shall 当該テナントを「バインド未完了」状態に保ち、enterprise 識別子を保存しない
5. If 既に「バインド済み」状態のテナントに対して重複してバインドが要求されたとき, the Tenant Service shall 新たな Enterprise を作成せず、競合として呼び出し側にエラーを返す
6. If 「無効化（disabled）」状態のテナントに対してバインドが要求されたとき, the Tenant Service shall 当該操作を拒否する
7. When Enterprise バインドの成否が確定したとき, the Tenant Service shall 当該バインドイベント（実行者・テナント識別子・結果）を監査ログ記録の対象とする

### Requirement 3: テナント無効化

**Objective:** As a SuperAdmin, I want テナントを無効化できる, so that 解約・運用停止したテナントに対する業務操作を停止しつつ監査証跡を保持できる

#### Acceptance Criteria

1. When SuperAdmin がテナント無効化を要求し、二段階確認が完了したとき, the Tenant Service shall 当該テナントの状態を「無効化（disabled）」へ遷移させる
2. If テナント無効化要求に対して二段階確認が完了していないとき, the Tenant Service shall 当該テナントを無効化せず、確認未完了として呼び出し側にエラーを返す
3. While テナントが「無効化」状態であるとき, the Tenant Service shall 当該テナントに対する作成・バインド・更新系の操作を拒否する
4. If 既に「無効化」状態のテナントに対して再度の無効化が要求されたとき, the Tenant Service shall 当該操作を競合として呼び出し側にエラーを返す
5. When テナント無効化が完了したとき, the Tenant Service shall 当該無効化イベント（実行者・テナント識別子・確認完了・結果）を監査ログ記録の対象とする

### Requirement 4: テナント状態の参照

**Objective:** As a SuperAdmin, I want テナントの一覧と個別状態を参照できる, so that 各テナントのバインド進捗・有効性を把握できる

#### Acceptance Criteria

1. When SuperAdmin がテナント一覧を要求したとき, the Tenant Service shall 全テナントの識別子・名称・状態（pending_bind / bound / disabled）を返す
2. When SuperAdmin が個別テナントの詳細を要求したとき, the Tenant Service shall 当該テナントの識別子・名称・状態・enterprise 識別子（バインド済みの場合）を返す
3. If 存在しないテナント識別子で詳細が要求されたとき, the Tenant Service shall 当該テナントが存在しない旨を「未検出」として呼び出し側に返す
4. While 登録済みのテナントが 1 件も存在しないとき, the Tenant Service shall テナント一覧として空のリストを返す

### Requirement 5: テナント状態に基づく業務操作の前提ガード

**Objective:** As a SaaS 運営者, I want バインド未完了・無効化のテナントに対する業務操作（端末・ポリシー等）が前提状態で拒否される, so that enterprise 識別子未確定や運用停止中のテナントへの不正な AMAPI 操作を防げる

#### Acceptance Criteria

1. While テナントが「バインド未完了」状態であるとき, the Tenant Service shall 当該テナントを業務操作（端末・ポリシー等の AMAPI 操作）の前提として「未バインド」と判定可能にする
2. If 「バインド未完了」状態のテナント文脈で enterprise 識別子を要求する操作が呼び出されたとき, the Tenant Service shall enterprise 識別子を返さず、バインド未完了として呼び出し側にエラーを返す
3. If 「無効化」状態のテナント文脈で業務操作のための enterprise 識別子が要求されたとき, the Tenant Service shall 当該テナントが無効である旨を呼び出し側に伝達し、業務操作を許容しない

### Requirement 6: SuperAdmin 専用アクセスとテナント分離

**Objective:** As a セキュリティ運用者, I want テナント管理操作が admin-console 由来の SuperAdmin に限定され、テナント分離が担保される, so that 非 SuperAdmin や tenant-console 経由のセッションによる運用系操作・他テナント越境を防げる

#### Acceptance Criteria

1. The Tenant Service shall すべてのテナント管理操作（作成・バインド・無効化・一覧・詳細）を `/api/admin` ルート群配下のエンドポイントとして提供する
2. If テナント管理操作のリクエストの audience が `admin-console` 以外（`tenant-console` 等）であるとき, the `/api/admin` ガード shall 当該リクエストを 403 で拒否する
3. If テナント管理操作のリクエストの role が SuperAdmin 以外であるとき, the `/api/admin` ガード shall 当該リクエストを 403 で拒否する
4. If テナント管理操作のリクエストに認証済みセッションが確立されていないとき, the `/api/admin` ガード shall 当該リクエストを 401 で拒否する
5. If テナント管理操作が拒否されたとき, the Tenant Service shall レスポンス body に対象テナントの存在有無を区別可能な形で露出しない

## Non-Functional Requirements

### NFR 1: 状態遷移の整合性

1. The Tenant Service shall テナント状態を pending_bind / bound / disabled の 3 値のいずれか 1 つに常に保持する
2. If 定義されていない状態遷移（例: disabled から bound への遷移）が要求されたとき, the Tenant Service shall 当該遷移を実行せず、不正な状態遷移として呼び出し側にエラーを返す
3. If Enterprise バインド処理が途中で失敗したとき, the Tenant Service shall テナントを enterprise 識別子未保存かつ「バインド未完了」状態に保ち、bound へ部分的に遷移した状態を残さない

### NFR 2: 監査・可観測性

1. The Tenant Service shall テナントの作成・バインド・無効化の各操作について、実行者識別子・対象テナント識別子・操作種別・結果（成功 / 失敗）を監査ログ記録の対象として渡す
2. The Tenant Service shall 拒否された操作（権限不足・不正状態遷移・確認未完了）について、原因分析に必要な属性（実行者・対象テナント・拒否理由）を構造化ログとして記録する
3. The Tenant Service shall ログ出力にサインアップ URL の秘密パラメータ・サービスアカウント資格情報・OAuth トークンの生値を含めない

### NFR 3: ステートレス性

1. The Tenant Service shall テナント状態・enterprise 識別子をプロセスローカルなメモリではなく永続ストアに保持し、複数プロセス・再起動をまたいで同一の判定結果を返す（12-factor / ステートレス）

## Out of Scope

- AMAPI Client 共有ラッパ自体の実装（`CreateSignupURL` / `CreateEnterprise` 等の低レベル呼び出し・
  認証・再試行・エラーマッピング）— #34（A4a AMAPI Client）で実装済み。本サービスは当該 IF を
  オーケストレーションするのみ
- admin-console のテナント管理 UI（一覧・作成・bind・無効化画面）— admin-console Issue
  （umbrella #24 tasks 13.2）の範囲
- RBAC 許可マトリクスおよび `/api/admin/*` ガード自体の実装 — #37（A3b）で実装済み。本サービスは
  当該ガード配下に配置されるのみ
- OIDC 認証・セッション発行・admin_users / admin_role_assignments の管理 — #33（A3a）/ 別 Issue
- DB Row-Level Security ポリシーの定義・有効化 — umbrella #24 tasks 2.3（基盤 Issue）の範囲。
  本サービスはテナント分離の前提として RLS / Authz に依拠する
- 監査ログテーブル（`audit_logs`）への永続化ロジック自体 — Audit Service Issue（umbrella #24
  tasks 5.1）の範囲。本サービスは記録対象イベントを Audit Service に渡すのみ
- テナント削除に伴う配下リソース（端末・ポリシー・トークン等）の論理削除・カスケード処理 —
  各ドメイン Issue の範囲（本サービスはテナント状態を disabled に遷移させるまで）
- テナント管理者のセルフサインアップ（顧客自身による Enterprise 作成フロー）— umbrella #24
  Open Questions 1 の確定により MVP では SuperAdmin 代行運用に限定

## Open Questions

- **テナント無効化の不可逆性**: umbrella #24 design.md の状態機械は `disabled` を終端
  （`Disabled --> [*]`）として描いている。無効化を不可逆（disabled から bound への復帰なし）と
  するか、再有効化フローを将来用意するかが未確定。本要件では NFR 1.2 で disabled→bound 遷移を
  不正として扱ったが、再有効化要件の要否を確認したい
- **二段階確認の入力契約**: テナント無効化（Requirement 3）の二段階確認について、確認トークン
  方式・確認入力（テナント名再入力等）方式のいずれを採るかは design.md / 実装判断に委ねる。
  本要件では「二段階確認が完了していること」を前提条件として記述するに留めた
- **enterprise 識別子の保持先と参照経路**: Requirement 2.3 / 5.2 で「保存済み enterprise 識別子を
  呼び出し側が参照可能にする」と記述したが、他ドメインが Tenant Service の公開 IF 経由で参照するか、
  共有ストアを直接読むかは design.md（Architect の領分）に委ねる
- **テナント一覧のページング**: Requirement 4.1 のテナント一覧について、MVP 想定テナント数では
  全件返却で足りる想定だが、件数増加時のページング要否は確認の余地がある
- **二段階確認とバインド操作の関係**: umbrella NFR 4.1 は「不可逆操作」に二段階確認を要求する。
  本要件では無効化（Requirement 3）にのみ二段階確認を課したが、Enterprise バインド（Requirement 2）
  にも確認を課すべきかは未確定（バインドは原則として不可逆ではないため対象外と解釈した）

## Traceability

本要件と Issue #38 受入基準候補 / umbrella #24 Requirement の対応表。

| 本要件 ID | Issue #38 受入基準候補 | umbrella #24 対応 Req / NFR |
|---|---|---|
| Requirement 1（テナント作成） | テナント作成ができる（Req 1） | 1.1（テナント生成 + サインアップ URL）、1.3（失敗時 pending_bind 保持） |
| Requirement 2（Enterprise バインド） | Enterprise bind ができる（Req 1） | 1.1（Enterprise 作成・バインド）、1.2（enterprise 識別子保存）、1.3（失敗時 pending_bind） |
| Requirement 3（テナント無効化） | 無効化ができる（Req 1） | NFR 4.1（不可逆操作の二段階確認）、1.4（テナント分離保持）、2.9（操作の監査記録） |
| Requirement 4（テナント状態の参照） | テナント作成・bind・無効化の確認 | 1.1〜1.3（状態 pending_bind / bound / disabled の可視化） |
| Requirement 5（業務操作の前提ガード） | （暗黙）bind 未完了で業務操作不可 | 1.3（バインド未完了テナントは端末・ポリシー操作を一切受け付けない）、1.2（enterprise 識別子利用） |
| Requirement 6（SuperAdmin 専用・テナント分離） | `/api/admin/tenants` は SuperAdmin 専用（403 ガード）、テナント分離が結合テストで検証される | 2.7（SuperAdmin の全テナント横断管理）、9.5（admin-console の特権）、1.4 / 1.5（テナント分離・存在露出なし）、NFR 2.1 |
| NFR 1（状態遷移の整合性） | 無効化・bind の冪等性 / 二重 bind 防止 | 1.1〜1.3（状態機械 pending_bind / bound / disabled） |
| NFR 2（監査・可観測性） | （暗黙）操作の追跡可能性 | 2.9（操作の監査記録）、NFR 4.2（監査ログ記録項目） |
| NFR 3（ステートレス性） | 12-factor / ステートレス（制約） | umbrella design.md「AWS Fargate 移行容易性」 |
