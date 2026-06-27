# Requirements Document

## Introduction

ae-mdm は AMAPI からの ENROLLMENT / STATUS_REPORT / COMMAND 通知を Cloud Pub/Sub の pull
subscription で受信し、端末インベントリ・コマンド状態を通知駆動で最新化する（design Requirement
8.1 / 8.5、NFR 3.1）。本 Issue #35（tasks 6.1）はその土台となる **Pub/Sub クライアント基盤** に
スコープを限定する。具体的には、emulator / 本番 GCP を切り替える Pub/Sub client の構築、pull
subscription の受信ループ、受信メッセージの ack / nack 抽象化、dead-letter topic への送出抽象、
および worker プロセスへの subscriber 配線（graceful shutdown を含む）を担う。通知の dispatch・
冪等処理・未割当退避・各ドメインの通知ハンドラ本体は別 Issue #36（tasks 6.2）に委ねる。

## Requirements

### Requirement 1: Pub/Sub クライアントの構築と接続先切替

**Objective:** As a worker プロセス, I want emulator / 本番 GCP を切り替えて Pub/Sub client を初期化したい, so that ローカル開発と本番運用を同一コードパスで動かせる

#### Acceptance Criteria

1. The PubSubClient shall 設定された Pub/Sub プロジェクト識別子を用いて Pub/Sub client を構築する
2. Where `PUBSUB_EMULATOR_HOST` が設定されているとき, the PubSubClient shall 当該 emulator エンドポイントに接続する
3. If `PUBSUB_EMULATOR_HOST` が未設定（空文字）であるとき, the PubSubClient shall 本番 GCP の Pub/Sub に接続する
4. If Pub/Sub client の構築に失敗したとき, the PubSubClient shall 構造化エラーを返し、部分初期化されたリソースを残さない
5. When PubSubClient のクローズが要求されたとき, the PubSubClient shall 保持している接続・内部リソースを解放する
6. If 既にクローズ済みの PubSubClient に対して再度クローズが要求されたとき, the PubSubClient shall 追加のエラーを発生させずに正常終了する

### Requirement 2: pull subscription での通知受信

**Objective:** As a worker プロセス, I want pull subscription で通知メッセージを受信したい, so that AMAPI からの状態変化をポーリングなしで取り込める

#### Acceptance Criteria

1. The PubSubSubscriber shall 設定された subscription 識別子に対して pull 方式で通知メッセージを受信する
2. When pull subscription で通知メッセージを受信したとき, the PubSubSubscriber shall 当該メッセージを注入された handler 抽象に引き渡す
3. The PubSubSubscriber shall メッセージ受信時に当該メッセージの識別子を構造化ログのフィールドとして記録する
4. If 設定された subscription が Pub/Sub 上に存在しないとき, the PubSubSubscriber shall 構造化エラーを返して受信ループを開始しない
5. When 空 payload のメッセージを受信したとき, the PubSubSubscriber shall 当該メッセージを破棄せず handler 抽象に引き渡す

### Requirement 3: 受信メッセージの ack / nack 抽象化

**Objective:** As a worker プロセス, I want handler の処理結果に応じてメッセージを ack / nack したい, so that 一時的失敗時に再配信され、恒常的失敗時に再配信ループに陥らない

#### Acceptance Criteria

1. When handler 抽象がメッセージを正常に処理したとき, the PubSubSubscriber shall 当該メッセージを ack する
2. When handler 抽象がメッセージの処理でエラーを返したとき, the PubSubSubscriber shall 当該エラーに対する ack / nack 判定を既存の `errors.ShouldAck` を経由して決定する
3. If handler 抽象が transient なエラーを返したとき, the PubSubSubscriber shall 当該メッセージを nack し、再配信の対象として保持する
4. If handler 抽象が恒常的（非 transient）なエラーを返したとき, the PubSubSubscriber shall 当該メッセージを ack し、無限の再配信を発生させない
5. When メッセージを ack または nack したとき, the PubSubSubscriber shall 当該結果と対象メッセージ識別子を構造化ログに記録する

### Requirement 4: 同時処理上限（MaxOutstandingMessages）の設定

**Objective:** As a 運用者, I want 同時処理メッセージ数の上限を設定したい, so that worker のリソース消費と並行度を制御できる

#### Acceptance Criteria

1. The PubSubSubscriber shall 同時に未確定（outstanding）状態で保持するメッセージ数の上限を設定値として受け付ける
2. While 未確定メッセージ数が設定された上限に達しているとき, the PubSubSubscriber shall 新規メッセージの追加 pull を抑制する
3. If 同時処理上限として 1 未満の値が指定されたとき, the PubSubSubscriber shall 当該設定を不正として拒否し、構造化エラーを返す

### Requirement 5: dead-letter topic への送出抽象

**Objective:** As a worker プロセス, I want 処理できないメッセージを dead-letter topic に送出したい, so that 喪失せず運用者が事後追跡できる（design Requirement 8.6 の土台）

#### Acceptance Criteria

1. The PubSubSubscriber shall メッセージを dead-letter topic に送出する抽象 IF を提供する
2. When dead-letter 送出 IF が呼び出されたとき, the PubSubSubscriber shall 対象メッセージの payload と識別子を設定された dead-letter topic に publish する
3. When dead-letter topic への送出に成功したとき, the PubSubSubscriber shall 送出結果と対象メッセージ識別子を構造化ログに記録する
4. If dead-letter topic への送出に失敗したとき, the PubSubSubscriber shall 構造化エラーを返し、当該メッセージを ack しない

### Requirement 6: worker プロセスへの subscriber 配線と graceful shutdown

**Objective:** As a 運用者, I want `make up` で worker が起動し、停止シグナルで安全に停止してほしい, so that 通知反映 SLO（NFR 3.1）の前提となる常駐基盤が成立する

#### Acceptance Criteria

1. When worker プロセスが起動したとき, the Worker shall 設定読み込みとロガー初期化の後に PubSubSubscriber を起動する
2. The Worker shall PubSubSubscriber に対して通知処理の handler 抽象を依存注入として配線する
3. When `make up` により worker サービスが起動されたとき, the Worker shall プロセスを常駐させ、healthz 応答を継続する
4. When worker プロセスが SIGINT または SIGTERM を受信したとき, the Worker shall PubSubSubscriber の受信を停止し、処理中メッセージを規定の猶予時間内で確定させてからプロセスを終了する
5. If PubSubSubscriber の起動に失敗したとき, the Worker shall 構造化 ERROR ログを出力し、非ゼロ終了コードでプロセスを終了する（fail-fast）

## Non-Functional Requirements

### NFR 1: 接続性とリトライ（可観測性・回復性）

1. If 起動時に Pub/Sub 接続先（emulator または本番 GCP）へ到達できないとき, the Worker shall 構造化 ERROR ログを出力した上で非ゼロ終了コードで終了する（silent fail しない）
2. When 受信・ack・nack・dead-letter 送出の各操作でエラーが発生したとき, the PubSubSubscriber shall 当該操作・対象メッセージ識別子・原因を含む構造化ログを 1 件出力する

### NFR 2: graceful shutdown のタイムアウト

1. When SIGINT または SIGTERM を受信したとき, the Worker shall graceful shutdown を 5 秒以内（既存 worker scaffold の `shutdownTimeout` と整合する規定猶予時間内）に完了する
2. If graceful shutdown が規定の猶予時間内に完了しなかったとき, the Worker shall 構造化 ERROR ログを出力し、非ゼロ終了コードで終了する

### NFR 3: 後方互換（既存 worker scaffold との整合）

1. The Worker shall 既存の `-healthcheck` サブコマンドと :8090 `/healthz` 常駐の挙動を維持したまま、PubSubSubscriber を追加配線する

## Out of Scope

- 通知の Dispatcher（受信ループから Envelope へのパース、通知種別ごとの handler 振り分け）— Issue #36（tasks 6.2）
- 冪等処理（`notification_dedupe` テーブルベースの MessageID 重複排除）— Issue #36
- 未割当退避（`unassigned_notifications` への退避、enterprise_name → tenant_id 解決失敗時の処理）— Issue #36
- enterprise → tenant 解決および `SET LOCAL app.tenant_id` の tx 注入 — Issue #36
- 各ドメインの通知ハンドラ本体（ENROLLMENT / STATUS_REPORT / COMMAND の DB 反映）— Issue #36（tasks 7.x / device / command ドメイン）
- 通知の送信元検証・署名・整合性確認（verifier）— Issue #36
- admin-console 向けの未割当退避キュー閲覧 API — Issue #36
- 通知欠落・同期遅延に対する管理者向けアラート — design Out of Scope（24-android-enterprise-emm-mvp）

## 確認事項 / 未決定事項

- **dead-letter topic の設定源**: dead-letter topic を指す設定が既存 config に未定義である
  （`backend/internal/config/config.go` の Pub/Sub セクションは `PubSubProjectID` /
  `PubSubTopic` / `PubSubSubscription` / `PubSubEmulatorHost` のみ）。新規 env var
  `PUBSUB_DEAD_LETTER_TOPIC` を追加するか、`<PubSubTopic>-deadletter` 等の命名規約で導出するか、
  あるいは subscription 側の deadLetterPolicy に委ねて本 Issue では publish せず subscription
  設定のみとするか、いずれを採るかが未確定（Requirement 5 の前提）。
- **handler 抽象の境界形**: PubSubSubscriber が受け取る通知処理 handler を interface とするか
  関数型（`func(ctx, message) error` 等）とするか、また #36 の Dispatcher 注入までの暫定 handler
  をどう扱うか（no-op handler を配線するか、Dispatcher が無い間は subscriber 起動自体を行わない
  か）が未確定（Requirement 6.2 の前提）。design.md の領分だが、暫定 handler の有無は本 Issue の
  受入観点に影響するため明示。
- **emulator の topic / subscription 自動作成**: `make up` 起動時に emulator 上の topic /
  subscription（および dead-letter topic）を worker 側で自動作成するか、別途 seed 手順
  （Makefile target / init コンテナ等）に委ねるかが未確定（Requirement 2.4 の「subscription
  不在時の挙動」および Requirement 6.3 の `make up` 起動要件と関係する）。

## トレーサビリティ

各 Requirement / NFR を design（24-android-enterprise-emm-mvp）の Requirement ID および
tasks 6.1 の Boundary（PubSubClient / PubSubSubscriber）へ対応付ける。

| 本 Issue の要件 | design Requirement | tasks 6.1 Boundary |
|---|---|---|
| Requirement 1（client 構築・接続先切替・クローズ） | 8.1, 8.5 | PubSubClient |
| Requirement 2（pull 受信） | 8.1, 8.5 | PubSubSubscriber |
| Requirement 3（ack / nack 抽象） | 8.1, 8.7（土台） | PubSubSubscriber |
| Requirement 4（MaxOutstandingMessages） | 8.1, NFR 3.1 | PubSubSubscriber |
| Requirement 5（dead-letter 送出抽象） | 8.6（土台） | PubSubSubscriber |
| Requirement 6（worker 配線・graceful shutdown） | 8.1, NFR 3.1 | PubSubSubscriber |
| NFR 1（接続性・可観測性） | NFR 3.1 | PubSubClient, PubSubSubscriber |
| NFR 2（shutdown タイムアウト） | NFR 3.1 | PubSubSubscriber |
| NFR 3（既存 scaffold 整合） | 8.1 | PubSubSubscriber |

> Note: design Requirement 8.6（検証失敗時の破棄 + ログ保持）と 8.7（一時失敗時の再処理保持）の
> 本体は Issue #36 のスコープ。本 Issue #35 は dead-letter 送出抽象（5）と ack / nack 抽象（3）の
> 「土台」のみを担い、verifier / dedupe / dispatch ロジックは含まない。

## 関連

- Depends on: #2
- Parent: #24
- Related: #36
