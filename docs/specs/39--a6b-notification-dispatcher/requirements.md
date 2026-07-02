# Requirements Document

## Introduction

AMAPI（Android Management API）は端末のエンロール・状態変化・コマンド完了を Pub/Sub 経由で
at-least-once セマンティクスで通知する。同一通知が複数回到達したり、一時的な処理失敗で再配信
されることが前提であり、無防備に処理すると端末インベントリやコマンド状態の二重更新・取りこぼし
が発生する。本 Notification Dispatcher は、受信した通知を冪等に処理し、通知種別ごとのドメイン
ハンドラへ振り分け、テナントを特定できない通知を退避する基盤を提供する。これは umbrella spec
（`docs/specs/24-android-enterprise-emm-mvp/`）の Requirement 8（監視・通知）および NFR 2.2 /
2.3 / 3.1 の dispatch 機構スライスであり、各ドメインハンドラの実体・Pub/Sub クライアント基盤は
本 Issue のスコープ外とする。

## 関連

- Parent: #24
- Depends on: #35

## Requirements

### Requirement 1: 通知の冪等処理（重複排除）

**Objective:** As a SaaS 運営者, I want at-least-once で到達する同一通知が 1 回だけ処理される, so that 端末インベントリ・コマンド状態の二重更新を防げる

#### Acceptance Criteria

1. When 未処理の通知を受信したとき, the Notification Dispatcher shall 当該通知の MessageID を `notification_dedupe` に記録した上で後続の処理（種別別 dispatch）を実行する
2. When 既に処理済みの MessageID を持つ通知を再受信したとき, the Notification Dispatcher shall 種別別 dispatch を実行せず、当該通知を即座に ack 相当として処理完了扱いにする
3. While 同一 MessageID の通知が並行して複数同時に到達しているとき, the Notification Dispatcher shall いずれか 1 件のみが種別別 dispatch を実行するよう重複排除を直列化する
4. If 重複排除記録の永続化に失敗したとき, the Notification Dispatcher shall 当該通知を処理完了扱いにせず、再処理対象として保持する

### Requirement 2: 通知種別ごとの dispatch

**Objective:** As a SaaS 運営者, I want 通知が種別ごとに対応するドメインハンドラへ振り分けられる, so that エンロール・状態・コマンドの各更新が正しいハンドラで処理される

#### Acceptance Criteria

1. When ENROLLMENT 種別の通知を受信したとき, the Notification Dispatcher shall 当該通知を ENROLLMENT 用ドメインハンドラへ振り分ける
2. When STATUS_REPORT 種別の通知を受信したとき, the Notification Dispatcher shall 当該通知を STATUS_REPORT 用ドメインハンドラへ振り分ける
3. When COMMAND 種別の通知を受信したとき, the Notification Dispatcher shall 当該通知を COMMAND 用ドメインハンドラへ振り分ける
4. If 受信した通知の種別が未対応または未登録（対応するドメインハンドラが存在しない）であるとき, the Notification Dispatcher shall 当該通知を取りこぼさず、運用者が事後追跡できる形でログを残した上で処理完了扱いにする
5. If 受信した通知の検証（送信元・整合性）に失敗したとき, the Notification Dispatcher shall 当該通知を破棄し、運用者が事後追跡できる形でログを残す
6. If 受信した通知のペイロードが空または種別を判定できない形式であるとき, the Notification Dispatcher shall 種別別 dispatch を実行せず、運用者が事後追跡できる形でログを残した上で当該通知を破棄する

### Requirement 3: テナント未割当通知の退避

**Objective:** As a SaaS 運営者, I want テナントを特定できない通知が退避され他テナントを更新しない, so that 誤配信や設定漏れによる越境更新を防ぎつつ取りこぼしも防げる

#### Acceptance Criteria

1. When 通知の `enterprise_name` から `tenant_id` が一意に解決できたとき, the Notification Dispatcher shall 当該テナントのコンテキストを確立した上で種別別 dispatch を実行する
2. If 通知の `enterprise_name` から `tenant_id` を一意に特定できないまたは特定不能であるとき, the Notification Dispatcher shall 当該通知を `unassigned_notifications` に退避し、種別別 dispatch を実行しない
3. When 通知を `unassigned_notifications` に退避したとき, the Notification Dispatcher shall 当該通知を処理完了扱い（ack 相当）にし、再配信ループに戻さない
4. If 通知の `enterprise_name` が空または欠落しているとき, the Notification Dispatcher shall 当該通知を `unassigned_notifications` に退避し、いずれのテナントのリソースも更新しない
5. The Notification Dispatcher shall テナント未割当通知の退避において、いずれのテナントのリソースも更新しない

### Requirement 4: 退避キュー閲覧 API（admin-console 向け）

**Objective:** As a SuperAdmin, I want 退避された未割当通知を一覧・絞り込みで閲覧できる, so that 誤配信や設定漏れを検知し対処できる

#### Acceptance Criteria

1. When SuperAdmin が退避キュー閲覧 API（`GET /api/admin/notifications/unassigned`）を要求したとき, the Notification Dispatcher shall 退避済みの未割当通知の一覧を返す
2. When SuperAdmin が `from` / `to` / `type` の絞り込み条件を指定して退避キュー閲覧 API を要求したとき, the Notification Dispatcher shall 指定条件に合致する未割当通知のみを抽出して返す
3. If 認証されていない要求が退避キュー閲覧 API に到達したとき, the Notification Dispatcher shall 401 相当の認証エラーを返す
4. If SuperAdmin 以外のロールが退避キュー閲覧 API にアクセスを試みたとき, the Notification Dispatcher shall 403 相当の権限拒否を返す
5. If 絞り込み条件（`from` / `to` / `type`）に不正な値が指定されたとき, the Notification Dispatcher shall 当該要求を拒否し、不正項目を要求元に提示する

### Requirement 5: リトライと再処理保持

**Objective:** As a SaaS 運営者, I want 一時的な処理失敗が再処理対象として保持される, so that 通知が永久に喪失することがない

#### Acceptance Criteria

1. If 通知の種別別 dispatch が一時的（再試行で回復しうる）に失敗したとき, the Notification Dispatcher shall 当該通知を処理完了扱いにせず、再処理対象として保持する
2. When 通知の処理が成功裏に完了したとき, the Notification Dispatcher shall 当該通知を処理完了扱い（ack 相当）にし、再配信ループに戻さない
3. While ある通知が再処理対象として保持されているとき, the Notification Dispatcher shall 当該通知を喪失させず、後続の再配信で再処理可能な状態を保つ

### Requirement 6: dispatch 経路の検証

**Objective:** As a 開発者, I want dispatch 経路が結合テストで検証される, so that 冪等処理・退避・振り分けの主要動線が回帰なく保たれる

#### Acceptance Criteria

1. The Notification Dispatcher shall 同一 MessageID の通知を 2 回受信したとき種別別 dispatch が 1 回のみ実行されることを、結合テストで検証可能にする
2. The Notification Dispatcher shall `enterprise_name` から `tenant_id` を解決できない通知が `unassigned_notifications` に退避されることを、結合テストで検証可能にする
3. The Notification Dispatcher shall ENROLLMENT / STATUS_REPORT / COMMAND の各種別が対応するドメインハンドラへ振り分けられることを、結合テストで検証可能にする
4. The Notification Dispatcher shall 退避済み通知が退避キュー閲覧 API（`GET /api/admin/notifications/unassigned`）から取得できることを、結合テストで検証可能にする

## Non-Functional Requirements

### NFR 1: 通知駆動の即時性

1. When 通知を受信したとき, the Notification Dispatcher shall 受信時点から 60 秒以内に、対応するドメインハンドラへの dispatch を完了する

### NFR 2: テナント分離

1. The Notification Dispatcher shall テナント A 宛の通知の処理によってテナント B のリソースを更新しない状態を恒常的に保つ
2. While 通知の `tenant_id` が一意に特定できていないとき, the Notification Dispatcher shall いかなるテナントのリソースも更新しない

### NFR 3: 可観測性

1. The Notification Dispatcher shall 破棄・退避・重複排除・dispatch 失敗の各イベントについて、運用者が MessageID 単位で事後追跡できる構造化ログを出力する

## Out of Scope

- Pub/Sub クライアント・worker エントリ（別 Issue #35。実装済み）
- 各通知ハンドラの実体（エンロール / デバイス / コマンドの各 Issue）
- `notification_dedupe` / `unassigned_notifications` テーブルの新規マイグレーション作成（既存スキーマ `0010` / `0011` を消費する前提）
- 退避済み通知の再処理・再投入（リドライブ）操作 UI / API
- dead-letter キューそのものの構成・運用方針（再配信回数上限の具体値含む）

## 用語

| 用語 | 意味 |
|---|---|
| at-least-once | 同一通知が 1 回以上配信される（重複・再配信が起こりうる）配信保証 |
| 冪等処理 | 同一通知を複数回受信しても結果が 1 回処理と同じになる性質 |
| MessageID | Pub/Sub が各メッセージに付与する一意識別子。重複排除の鍵 |
| `enterprise_name` | AMAPI が払い出す Enterprise 識別子。テナント特定の鍵 |
| 未割当通知 | `enterprise_name` から `tenant_id` を一意に特定できない通知 |
| dispatch | 通知種別に応じて対応するドメインハンドラへ振り分けること |
| ドメインハンドラ | ENROLLMENT / STATUS_REPORT / COMMAND の各種別を処理する本体（本 Issue 外） |
| ack / nack | 通知の処理完了（ack）/ 再配信要求（nack）を示す Pub/Sub への応答 |

## Open Questions

- なし（本 Issue は umbrella spec で設計確定済みのスライスであり、Issue 本文・既存ドキュメント・確認済みの既存実装事実で要件は確定している）
