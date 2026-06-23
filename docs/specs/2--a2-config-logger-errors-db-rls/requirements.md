# Requirements Document

## Introduction

本要件は ae-mdm（Android Enterprise EMM SaaS）MVP のプラットフォーム共通基盤（A2）を確立する
ためのものである。Umbrella Issue #24（`docs/specs/24-android-enterprise-emm-mvp/`）の Task 2.1〜
2.3 を 1 Issue に統合し、各ドメイン（テナント / 認証認可 / エンロール / ポリシー / デバイス /
コマンド / アプリ配信 / 通知）の実装で共通利用される「設定（config）・構造化ログ（logger）・
独自エラー型（errors）・DB 接続プール + Tenant Context 強制（platform/db）・HTTP サブルータ群
（platform/httpserver）・マイグレーション + RLS + audit_logs の append-only 化」を提供する。

本基盤の最重要責務は umbrella の Req 1.4 / 1.5（テナント分離）と NFR 2.1（テナント間の参照・
操作禁止の恒常維持）および NFR 4.3（監査ログの改竄・削除不可）を、アプリ層フィルタと
PostgreSQL Row-Level Security の二重防御で物理的に担保することにある。テナント識別子を伴わない
DB アクセスが偶発的に到達した場合でも、panic ガードによりリクエスト処理を停止し、誤って
他テナントの行を読み書きする事故を防ぐ。各ドメインの handler / service の実装は本 Issue の
対象外であり、後続 Issue（A3 以降）で行う。

## 関連

- Depends on: #1
- Parent: #24

## Requirements

### Requirement 1: 設定（config）の共通基盤

**Objective:** As a 開発者, I want 全プロセス（api / worker / CLI）が環境変数を一元的に読み込み、必須値の欠落を起動時に検出できる共通基盤, so that 後続 Issue が config を経由するだけで動作環境（DB / OIDC / Pub/Sub / AMAPI / 監査ログ保持期間 / 同期遅延閾値）を取得できる

#### Acceptance Criteria

1. The EMM Platform shall 環境変数からアプリケーション設定値を構造体として読み込む共通モジュールを提供する
2. The EMM Platform shall 設定値として少なくとも DB 接続情報・OIDC クライアント情報・Pub/Sub 接続情報・AMAPI 認証情報・セッション秘密鍵・監査ログ保持期間・同期遅延閾値を読み込む
3. If 必須環境変数が未設定または不正なフォーマットであるとき, the EMM Platform shall プロセス起動を fail-fast で中断し、どの環境変数が問題かを判別可能なエラーメッセージを出力する
4. While アプリケーションが稼働中であるとき, the EMM Platform shall 設定値を不変（読み取り専用）として扱い、ランタイムでの上書きを許可しない
5. The EMM Platform shall 設定モジュールが `api` プロセス・`worker` プロセス・補助 CLI（admin-seed 等）から共通に import 可能である

### Requirement 2: 構造化ログ（logger）の共通基盤

**Objective:** As a 運用者, I want 全プロセスが構造化ログを発行し、テナント ID・リクエスト ID・メッセージ ID 等の追跡キーが field として常時付与される, so that マルチテナント環境下で特定のリクエストや通知の処理経路を後追いできる

#### Acceptance Criteria

1. The EMM Platform shall アプリケーション全体で利用する構造化ログのファクトリを提供する
2. The EMM Platform shall ログ出力に `tenant_id` / `request_id` / `message_id` を field として付与するためのヘルパを提供する
3. When ハンドラまたはワーカーがエラーを観測したとき, the EMM Platform shall エラー原因（cause）を含む構造化ログを WARN または ERROR レベルで記録する
4. The EMM Platform shall ログのレベル・出力先・出力フォーマット（JSON 等）を環境変数から設定可能にする
5. While ログ出力が行われているとき, the EMM Platform shall 機密情報（OIDC トークン本体・サービスアカウント鍵・セッション cookie 値）を平文でログに出力しない

### Requirement 3: 独自エラー型（errors）の共通基盤

**Objective:** As a 開発者, I want ドメインエラーを HTTP ステータスや Pub/Sub ack/nack に一貫して写像できる独自 Error 型, so that 各ドメインの handler / worker が同じ表現でエラーを返し、最外層で一元的に応答を生成できる

#### Acceptance Criteria

1. The EMM Platform shall 機械可読なコード（`Code`）・利用者向けメッセージ（`Message`）・wrap 元（`Cause`）を保持する独自 Error 型を提供する
2. The EMM Platform shall 独自 Error 型が標準的なエラーラッピング操作（`errors.Is` / `errors.As` 互換）を満たす
3. When 独自 Error が HTTP ハンドラの最外層に到達したとき, the EMM Platform shall 当該 Error の属性に基づき HTTP ステータスコードを決定する
4. When 独自 Error がワーカーの最外層に到達したとき, the EMM Platform shall 当該 Error が再試行可能か否かに基づき ack / nack の判定を決定する
5. If 独自 Error 型でない予期しないエラーが最外層に到達したとき, the EMM Platform shall 当該エラーを 5xx 相当として扱い、原因を構造化ログに記録する

### Requirement 4: DB 接続プールと Tenant Context の強制

**Objective:** As a 開発者, I want 全 DB アクセスがトランザクション境界内で `SET LOCAL app.tenant_id`（および SuperAdmin の場合 `app.is_superadmin`）を必ず発行する仕組み, so that RLS と組み合わせて他テナント行への偶発的な読み書きを物理的に防げる

#### Acceptance Criteria

1. The EMM Platform shall 環境変数の設定値から PostgreSQL の接続プールを構築する
2. The EMM Platform shall DB トランザクション境界の開始から終了（commit / rollback）までを単一のヘルパ呼び出しで管理可能にする
3. When ハンドラまたはワーカーが DB トランザクションを開始したとき, the EMM Platform shall 当該トランザクション内で `SET LOCAL app.tenant_id` を当該リクエストのテナント識別子で発行する
4. Where 当該リクエストが SuperAdmin による cross-tenant 操作であるとき, the EMM Platform shall 当該トランザクション内で `SET LOCAL app.is_superadmin = true` を併せて発行する
5. If テナントコンテキストを保持しないリクエスト経路から DB トランザクションが開始されようとしたとき, the EMM Platform shall 当該トランザクションを開始せず panic 相当のガードで処理を停止し、構造化ログに当該事象を記録する
6. The EMM Platform shall DB アクセスに利用する SQL を型安全な手段で生成可能にする設定ファイルを提供する

### Requirement 5: Tenant Context Middleware と HTTP サブルータ群

**Objective:** As a 開発者, I want 認証済みリクエストから抽出したテナントコンテキストを後続のハンドラと DB トランザクションに伝播し、テナント系（`/api`）と運用系（`/api/admin`）のルート群を分離して mount できる, so that 顧客企業向け API と SaaS 運用者向け API を同一プロセス上で認可分離して提供できる

#### Acceptance Criteria

1. The EMM Platform shall HTTP リクエスト処理経路に挿入するテナントコンテキストミドルウェアを提供する
2. When 認証済みリクエストが当該ミドルウェアを通過したとき, the EMM Platform shall リクエストコンテキストに当該リクエストのテナント識別子・管理者識別子・ロール・SuperAdmin 判定を格納する
3. The EMM Platform shall HTTP ルータを単一プロセス上で `/api` と `/api/admin` の 2 つのサブルータ群として mount する
4. Where リクエストパスが `/api/admin` 配下に属するとき, the EMM Platform shall 当該リクエストに対して SuperAdmin 必須ガードをミドルウェアチェーンに挟む
5. If `/api/admin` 配下に SuperAdmin ロールを持たない管理者からのリクエストが到達したとき, the EMM Platform shall 当該リクエストを 403 で拒否し、対象リソースの存在を露出しない
6. The EMM Platform shall HTTP リクエスト処理経路に recover ミドルウェア・request_id 付与ミドルウェア・構造化アクセスログミドルウェアを登録する

### Requirement 6: マイグレーション骨格と全テーブルへの RLS 適用

**Objective:** As a 運用者, I want umbrella の Logical Data Model に定義された全テーブルを再現可能な順序で作成・撤回でき、テナント識別子を持つテーブルすべてに RLS を有効化できるマイグレーション群, so that 開発・テスト・本番のいずれの環境でも同一のスキーマと分離ポリシーが適用される

#### Acceptance Criteria

1. The EMM Platform shall umbrella spec の Logical Data Model に列挙された全テーブル（`tenants` / `admin_users` / `admin_role_assignments` / `sessions` / `enrollment_tokens` / `policies` / `devices` / `device_commands` / `tenant_apps` / `audit_logs` / `notification_dedupe` / `unassigned_notifications`）を作成するマイグレーションを提供する
2. The EMM Platform shall 各マイグレーションについて適用（up）と撤回（down）の対応ファイルを提供する
3. The EMM Platform shall テナント識別子カラムを持つすべてのテーブルに対して `ENABLE ROW LEVEL SECURITY` を有効化し、`app.tenant_id` または `app.is_superadmin` に基づく分離ポリシーを定義するマイグレーションを提供する
4. While アプリケーション用 DB ロールで接続しているとき, the EMM Platform shall テナント識別子カラムを持つ他テナント行を SELECT / UPDATE / DELETE のいずれにおいても返却・更新・削除しない
5. The EMM Platform shall DDL 実行用ロールとアプリケーション用ロールを別ロールとして分離する

### Requirement 7: audit_logs の append-only 強制

**Objective:** As a 監査・コンプライアンス担当, I want アプリケーション経由で監査ログレコードが改竄・削除できない構成, so that 監査ログを「記録後は不変」として扱える（NFR 4.3）

#### Acceptance Criteria

1. The EMM Platform shall `audit_logs` テーブルに INSERT のみを許可し UPDATE / DELETE を拒否する DB 構成を適用するマイグレーションを提供する
2. If アプリケーション用 DB ロールから `audit_logs` への UPDATE または DELETE が試行されたとき, the EMM Platform shall 当該操作を DB レベルで拒否する
3. The EMM Platform shall `audit_logs` の SELECT に対しても、テナント識別子による分離ポリシーまたは SuperAdmin による全テナント横断アクセスポリシーを適用する
4. When `audit_logs` への INSERT 経路を提供するマイグレーションが適用されたとき, the EMM Platform shall 当該テーブルに対する RLS を `FORCE ROW LEVEL SECURITY` として強制し、テーブル所有者であっても分離ポリシーを回避できない構成にする

## Non-Functional Requirements

### NFR 1: テナント分離の二重防御

1. The EMM Platform shall アプリケーション層の `tenant_id` フィルタと PostgreSQL の RLS を両方有効化した状態で稼働する（一方のみで稼働してはならない）
2. While 任意のテナント A の管理者として認証済みのセッションが稼働中であるとき, the EMM Platform shall テナント B に属する全テーブル（`tenants` を除く）の行を、当該セッションからのいかなる SQL 操作によっても返却・更新・削除しない

### NFR 2: マイグレーションの再現性と可逆性

1. The EMM Platform shall マイグレーションを順序付き ID で管理し、同一の up シーケンスを 2 回目以降に再適用してもスキーマが変化しない（冪等な適用）
2. The EMM Platform shall すべての up マイグレーションに対応する down マイグレーションを提供し、テスト環境で down → up を 1 サイクル実行してもスキーマが破損しない

### NFR 3: 起動時 fail-fast

1. If config・DB 接続・マイグレーション・OIDC discovery いずれかの初期化に失敗したとき, the EMM Platform shall プロセスを exit code 非 0 で停止し、原因が判別可能なエラーログを出力する
2. The EMM Platform shall リクエスト処理を開始する前に、必要なすべての外部依存（DB 接続・必要な GUC が利用可能であること）が初期化済みであることを確認する

### NFR 4: 互換性と将来移行容易性

1. The EMM Platform shall config・logger・errors・db・httpserver の各モジュールを `api` プロセスと `worker` プロセスの双方から共通に import 可能な配置とする
2. The EMM Platform shall ローカル状態（ファイルシステム上の永続データ）を持たず、設定はすべて環境変数経由で注入されることで、AWS Fargate 等のコンテナ実行環境に移行可能な構成を維持する

## Out of Scope

- 各ドメインの handler / service / repository 実装（テナント / 認証認可 / エンロール / ポリシー / デバイス / コマンド / アプリ配信 / 通知の各 service ロジック、いずれも後続 Issue で実装）
- 初期 SuperAdmin の seed CLI 実装（umbrella tasks.md 14.2 で別途扱う）
- フロントエンド（tenant-console / admin-console）の実装
- AMAPI Client の薄いラッパ実装（umbrella tasks.md 4.1）
- OIDC Verifier / Session Manager の実装（umbrella tasks.md 3.1）
- RBAC Authorizer の許可マトリクス実装（umbrella tasks.md 3.2）。本 Issue では `/api/admin` 配下に挟む SuperAdmin ガードのフック点のみを提供する
- Pub/Sub クライアントの実装（umbrella tasks.md 6.1）
- 性能チューニング（インデックス追加・接続プールサイズの最適化）。MVP の妥当な初期値を採用する

## Open Questions

なし
