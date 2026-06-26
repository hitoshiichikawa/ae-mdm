# Requirements Document

## Introduction

Android Enterprise EMM MVP（umbrella #24）における各ドメイン（Tenant / Policy / Device /
Command / Enrollment / App / Notification）はすべて Android Management API（以下 AMAPI）を
呼び出す。各ドメインが個別に低レベルの AMAPI 呼び出し・認証・再試行・エラー解釈を抱えると、
テナント分離の物理担保（enterprise 識別子の取り違え防止）・再試行戦略・エラーマッピングが
散逸し、運用品質が劣化する。本 Issue（A4a）は、自社 GCP プロジェクトのサービスアカウントで
AMAPI を呼ぶ **共有の薄いラッパ** を提供し、各ドメインがビジネスロジックに専念できる状態を
作ることを目的とする。本ラッパは EMM-bound 方式（自社 GCP のサービスアカウントで全テナントの
Enterprise を操作）を前提とする。本 Issue のスコープは共有ラッパ自体の提供に限定し、Tenant
ドメインのビジネスロジックや各ドメインからの利用は別 Issue で扱う。

## 関連

- Depends on: #2
- Parent: #24

## Requirements

### Requirement 1: AMAPI 操作の集約と公開 IF

**Objective:** As a 各ドメインサービスの実装者, I want AMAPI 呼び出しを 1 つの共有ラッパに集約した型安全な操作 IF, so that 各ドメインが低レベルの REST / SDK 呼び出しを抱えずにビジネスロジックに専念できる

#### Acceptance Criteria

1. The AMAPI Client Wrapper shall サインアップ URL の生成（`CreateSignupURL`）操作をラッパ経由で提供する
2. The AMAPI Client Wrapper shall Enterprise の作成（`CreateEnterprise`）と取得（`GetEnterprise`）操作をラッパ経由で提供する
3. The AMAPI Client Wrapper shall ポリシーの upsert（`UpsertPolicy`）と取得（`GetPolicy`）操作をラッパ経由で提供する
4. The AMAPI Client Wrapper shall 端末の一覧取得（`ListDevices`）と詳細取得（`GetDevice`）操作をラッパ経由で提供する
5. The AMAPI Client Wrapper shall 端末コマンドの発行（`IssueCommand`）操作をラッパ経由で提供する
6. The AMAPI Client Wrapper shall エンロールメントトークンの発行（`CreateEnrollmentToken`）操作をラッパ経由で提供する
7. The AMAPI Client Wrapper shall WebToken の発行（`CreateWebToken`）操作をラッパ経由で提供する
8. The AMAPI Client Wrapper shall 上記の各操作を、AMAPI のレスポンスをそのまま透過せずに本ラッパの型へ正規化した値として返す
9. While 各ドメインサービスが AMAPI を呼び出している間, the AMAPI Client Wrapper shall AMAPI への呼び出しが本ラッパを経由しない経路（各ドメインからの SDK 直接呼び出し）を許容しない

### Requirement 2: テナント分離の物理担保（enterprise 識別子の必須化）

**Objective:** As a SaaS 運営者（SuperAdmin）, I want すべての AMAPI 呼び出しが enterprise 識別子を引数として必須にすること, so that 実装時の取り違えによる他テナントの Enterprise リソース誤操作を物理的に防げる

#### Acceptance Criteria

1. The AMAPI Client Wrapper shall enterprise 識別子（`enterpriseName`）を必要とする全操作（ポリシー / 端末 / コマンド / エンロールメントトークン / WebToken / Enterprise 取得）の引数として `enterpriseName` を必須化する
2. Where 当該操作が Enterprise 確定前のフロー（`CreateSignupURL` および `CreateEnterprise`）に該当するとき, the AMAPI Client Wrapper shall `enterpriseName` 引数を要求しない例外として扱う
3. If 呼び出し側が `enterpriseName` を空値または未指定として AMAPI Client Wrapper のテナント分離対象操作を呼び出したとき, the AMAPI Client Wrapper shall 当該操作を実行せず、不正引数として呼び出し側にエラーを返す
4. The AMAPI Client Wrapper shall 受領した `enterpriseName` を AMAPI への HTTP / SDK 呼び出しのリソースパスにそのまま反映し、内部で別テナントの識別子に置換しない

### Requirement 3: サービスアカウント認証と機密情報の取扱い

**Objective:** As a SaaS 運営者, I want AMAPI 認証を自社 GCP の単一サービスアカウントに集約し、秘密情報をコード内に埋め込まない, so that 認証情報の漏洩リスクと管理コストを最小化できる

#### Acceptance Criteria

1. The AMAPI Client Wrapper shall 自社 GCP プロジェクトのサービスアカウント資格情報を用いて AMAPI を呼び出す（EMM-bound 方式）
2. The AMAPI Client Wrapper shall サービスアカウント資格情報を環境変数から注入された経路でのみ読み込む
3. If サービスアカウント資格情報が未設定または読み込めない状態でラッパが操作呼び出しを受けたとき, the AMAPI Client Wrapper shall 当該操作を実行せず、設定不備として呼び出し側にエラーを返す
4. The AMAPI Client Wrapper shall API Key を含む秘密値をソースコード・コミット履歴・ログ出力のいずれにも埋め込まない
5. The AMAPI Client Wrapper shall 取得した OAuth アクセストークンをプロセス内でキャッシュし、有効期限に基づいて再取得する

### Requirement 4: エラーマッピング（4xx / 5xx の意味的分類）

**Objective:** As a 各ドメインサービスの実装者, I want AMAPI からのエラーが「呼び出し側の問題」と「再試行可能な一時障害」に意味的に分類された型で返ること, so that 各ドメインが HTTP ステータスコードを直接解釈せずに適切な応答を返せる

#### Acceptance Criteria

1. When AMAPI が 4xx 系のレスポンスを返したとき, the AMAPI Client Wrapper shall 当該レスポンスを「呼び出し側に起因するドメインエラー」として再試行不可なエラー型に正規化して返す
2. When AMAPI が 5xx 系のレスポンスを返したとき, the AMAPI Client Wrapper shall 当該レスポンスを「再試行可能な一時障害」として再試行可能なエラー型に正規化して返す
3. When AMAPI が認可不足（403 相当）を返したとき, the AMAPI Client Wrapper shall 呼び出し側が「権限不足」として識別可能なエラーとして返す
4. When AMAPI が対象リソース不在（404 相当）を返したとき, the AMAPI Client Wrapper shall 呼び出し側が「未検出」として識別可能なエラーとして返す
5. When AMAPI が競合（409 相当）を返したとき, the AMAPI Client Wrapper shall 呼び出し側が「競合」として識別可能なエラーとして返す
6. When AMAPI への呼び出しがネットワーク到達不能・タイムアウト等で失敗したとき, the AMAPI Client Wrapper shall 当該失敗を「再試行可能な一時障害」として正規化して返す
7. The AMAPI Client Wrapper shall すべてのエラーに対して、原因となった AMAPI 応答の識別情報（HTTP ステータス相当および AMAPI が返したエラー要約）を上位レイヤがログ出力で参照できる形で保持する

### Requirement 5: 一時障害に対する再試行戦略

**Objective:** As a 各ドメインサービスの実装者, I want 一時的な障害（429 / 5xx）に対する再試行戦略がラッパに内包されている, so that 各ドメインが個別に exponential backoff を実装せずに済む

#### Acceptance Criteria

1. When AMAPI が 429（レート制限）または 5xx 系のレスポンスを返したとき, the AMAPI Client Wrapper shall exponential backoff に基づき同一操作を最大 3 回まで再試行する
2. If 最大 3 回の再試行を経ても 429 / 5xx が解消されないとき, the AMAPI Client Wrapper shall 再試行を打ち切り、「再試行可能な一時障害」として呼び出し側にエラーを返す
3. When AMAPI が 4xx 系（401 / 403 / 404 / 409 等）のレスポンスを返したとき, the AMAPI Client Wrapper shall 当該操作を再試行せず、直ちに「呼び出し側に起因するドメインエラー」として呼び出し側に返す
4. If 呼び出し側のコンテキストがキャンセル（タイムアウトまたは明示的キャンセル）されたとき, the AMAPI Client Wrapper shall 進行中の再試行を中断し、キャンセル理由を保持したエラーとして呼び出し側に返す

### Requirement 6: 試験容易性（mock 用 IF の切り出し）

**Objective:** As a 各ドメインサービスの実装者, I want 各ドメインの単体テストで AMAPI Client Wrapper をモック差し替え可能な IF として扱える, so that 実 AMAPI を呼ばずに各ドメインのビジネスロジックを単体テストできる

#### Acceptance Criteria

1. The AMAPI Client Wrapper shall 公開 IF として呼び出し側が差し替え可能な抽象（interface）を提供する
2. The AMAPI Client Wrapper shall テスト用途で利用可能な stub 実装を本ラッパと同一のパッケージ境界内で提供する
3. While 各ドメインの単体テストが AMAPI Client Wrapper の stub 実装を利用しているとき, the AMAPI Client Wrapper shall 実 AMAPI への HTTP / SDK 呼び出しを発生させない

## Non-Functional Requirements

### NFR 1: 可観測性

1. The AMAPI Client Wrapper shall すべての AMAPI 呼び出しについて、操作種別・対象 `enterpriseName`・所要時間・最終結果（成功 / ドメインエラー / 一時障害）をログとして出力する
2. The AMAPI Client Wrapper shall ログ出力時にサービスアカウント秘密値・OAuth アクセストークン・WebToken の値そのものを記録しない
3. The AMAPI Client Wrapper shall 再試行が発生した場合、各再試行の試行回数と原因種別（429 / 5xx / ネットワーク失敗）をログとして識別可能にする

### NFR 2: セキュリティ

1. The AMAPI Client Wrapper shall 認証資格情報を環境変数または環境変数で示されたパス経由の資格情報ファイルからのみ取得し、リポジトリ内の固定パス・ハードコード値からは取得しない
2. The AMAPI Client Wrapper shall HTTPS 以外の経路で AMAPI を呼び出さない

### NFR 3: 互換性・後方互換

1. The AMAPI Client Wrapper shall 既存の共通基盤（設定読み込み・ロガー・エラー型）に対する破壊的変更を行わず、既存の公開 API を変更しない

## Out of Scope

- Tenant ドメインのビジネスロジック（テナントレコードの作成・状態遷移・SuperAdmin による
  Enterprise バインドフローのハンドラ実装）— 別 Issue（Tenant Service / umbrella #24 tasks 4.2）
- 各ドメイン（Policy / Device / Command / Enrollment / App）からの本ラッパ利用 — 各ドメインの
  実装 Issue（umbrella #24 tasks 5〜）
- AMAPI からの Pub/Sub 通知（ENROLLMENT / STATUS / COMMAND）の受信ハンドラ — 別 Issue
- 監査ログへの記録ロジック（記録要否の判断・記録項目の組み立て）— 監査ログサービス Issue
- AMAPI Client Wrapper をフロントエンド（admin-console / tenant-console）から直接呼ぶ経路 —
  全フロントエンド呼び出しは backend ドメインサービス経由のみ
- 1 ポリシーあたりのアプリ上限（3,000 件）など、ドメイン側のビジネスルール検証 — 当該検証は
  呼び出し側ドメインが担う（本ラッパは AMAPI の応答に従う）

## Open Questions

- なし

