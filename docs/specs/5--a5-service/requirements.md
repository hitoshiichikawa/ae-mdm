# Requirements Document

## Introduction

本要件は ae-mdm（Android Enterprise EMM SaaS）MVP の **監査ログ Service（A5）** を確立するためのものである。
破壊的操作・権限変更などの重要操作を改竄不能に追跡できないと、SaaS としてのガバナンス・コンプライアンス
要件を満たせない。多くのドメイン（テナント・認可・ポリシー・コマンド・エンロール等）が、自身の重要操作を
監査記録へ残すために本 Service の記録 IF に依存する。

本 Issue の責務は、(1) 各ドメイン Service から呼び出される **append-only な記録 IF（Record）の提供**、
(2) tenant-console 経由の **自テナント監査ログ閲覧**（TenantAdmin 以上）と admin-console 経由の
**全テナント横断閲覧**（SuperAdmin）の 2 経路の提供、(3) **保持期間（既定 180 日、設定可能）に基づく
読取対象範囲の制限**、にある。監査ログストア・append-only 強制（記録後の更新・削除拒否）・INSERT-only
書込権限・保持期間設定は **A2 / #33 / #37 で実装済み**であり、本 Service はこれらを正しく利用・連携する
ことを責務とする（ストアスキーマや権限定義の新規作成は本 Issue のスコープ外）。各重要イベントの実発火
（どのドメインがいつ Record を呼ぶか）は各ドメイン Issue 側の責務であり、本 Issue は記録 IF の提供までを担う。

## 関連

- Depends on: #33
- Parent: #24
- Related: #18 #21

## Requirements

### Requirement 1: 監査イベント記録 IF（append-only な書込）

**Objective:** As a 監査記録を残したいドメイン Service, I want 型付き監査イベントを append-only で記録できる単一の記録 IF, so that 重要操作の履歴を一貫した形で改竄不能に蓄積できる

#### Acceptance Criteria

1. When ドメイン Service が監査イベントの記録を要求したとき, the Audit Service shall 当該イベントを実行者・テナント・イベント種別・対象リソース・詳細・結果・発生時刻とともに監査ログへ 1 レコードとして追記する
2. When SuperAdmin による全テナント横断操作（特定テナントに帰属しない操作）の記録を要求したとき, the Audit Service shall テナント識別子を未設定（NULL 相当）として当該イベントを追記する
3. When 監査イベントの結果が成功であるとき, the Audit Service shall 当該イベントを結果区分「成功」として追記する
4. When 監査イベントの結果が失敗であるとき, the Audit Service shall 当該イベントを結果区分「失敗」として追記する
5. The Audit Service shall 記録済みの監査ログレコードに対する更新（update）および削除（delete）の操作 IF を一切公開しない
6. If 監査ログへの追記が永続化エラーで失敗したとき, the Audit Service shall 呼び出し側へエラーを返し、当該書込を成功として扱わない
7. The Audit Service shall 監査イベント詳細（detail）に ID トークン本体・セッション cookie の生値・パスワード等の機密値そのものを格納しない

### Requirement 2: 自テナント監査ログ閲覧（tenant-console 経路）

**Objective:** As a TenantAdmin, I want 自テナントの監査ログを時系列で閲覧し、操作種別・実行者・対象リソース・期間で絞り込みたい, so that 自社テナント内の重要操作の追跡と説明責任を果たせる

#### Acceptance Criteria

1. When TenantAdmin 以上のロールを持つ管理者が自テナント監査ログ閲覧経路に要求を行ったとき, the Audit Service shall 当該管理者の所属テナントに帰属する監査ログのみを発生時刻の降順で返す
2. When 自テナント監査ログ閲覧要求にイベント種別の絞り込み条件が指定されたとき, the Audit Service shall 当該イベント種別に一致する監査ログのみに絞り込んで返す
3. When 自テナント監査ログ閲覧要求に実行者の絞り込み条件が指定されたとき, the Audit Service shall 当該実行者に一致する監査ログのみに絞り込んで返す
4. When 自テナント監査ログ閲覧要求に対象リソースの絞り込み条件が指定されたとき, the Audit Service shall 当該対象リソースに一致する監査ログのみに絞り込んで返す
5. When 自テナント監査ログ閲覧要求に期間（開始時刻 from / 終了時刻 to）が指定されたとき, the Audit Service shall 発生時刻が当該期間に含まれる監査ログのみに絞り込んで返す
6. Where 自テナント監査ログ閲覧要求に開始時刻 from のみが指定されたとき, the Audit Service shall 発生時刻が from 以降の監査ログのみを返す
7. Where 自テナント監査ログ閲覧要求に終了時刻 to のみが指定されたとき, the Audit Service shall 発生時刻が to 以前の監査ログのみを返す
8. When 絞り込み条件に一致する監査ログが 1 件も存在しないとき, the Audit Service shall 空の結果を正常応答として返す
9. The Audit Service shall 自テナント監査ログ閲覧経路において、他テナントに帰属する監査ログおよびテナント未設定（NULL）の監査ログを結果に含めない

### Requirement 3: 全テナント横断監査ログ閲覧（admin-console 経路）

**Objective:** As a SuperAdmin, I want 全テナント横断の監査ログを閲覧し、テナント・操作種別・実行者・対象リソース・期間で絞り込みたい, so that SaaS 運営者として全テナントのガバナンス状況とインシデントを横断的に追跡できる

#### Acceptance Criteria

1. When SuperAdmin が全テナント横断監査ログ閲覧経路に要求を行ったとき, the Audit Service shall 全テナントの監査ログおよびテナント未設定（NULL）の監査ログを発生時刻の降順で返す
2. When 横断監査ログ閲覧要求に特定テナントの絞り込み条件が指定されたとき, the Audit Service shall 当該テナントに帰属する監査ログのみに絞り込んで返す
3. When 横断監査ログ閲覧要求にイベント種別・実行者・対象リソース・期間のいずれかの絞り込み条件が指定されたとき, the Audit Service shall 当該条件に一致する監査ログのみに絞り込んで返す
4. When 横断監査ログ閲覧要求の絞り込み条件に一致する監査ログが 1 件も存在しないとき, the Audit Service shall 空の結果を正常応答として返す
5. Where 横断監査ログ閲覧要求にテナント絞り込み条件が指定されないとき, the Audit Service shall 全テナントおよびテナント未設定（NULL）の監査ログを対象とする

### Requirement 4: 監査ログ閲覧の認可（RBAC / テナント分離）

**Objective:** As a セキュリティ運用者, I want 監査ログの閲覧が役割とテナント境界に厳密に従う, so that 権限を持たない管理者や他テナントの管理者が監査ログを覗き見できない

#### Acceptance Criteria

1. While 自テナント監査ログ閲覧経路にアクセスする管理者のロールが Operator または Viewer であるとき, the Audit Service shall 当該要求を権限不足として拒否する
2. If 認証されていない要求が監査ログ閲覧経路に到達したとき, the Audit Service shall 当該要求を認証エラーとして拒否し、監査ログを返さない
3. If TenantAdmin が自身の所属テナント以外のテナントの監査ログを閲覧しようとしたとき, the Audit Service shall 当該要求を拒否し、当該テナントの監査ログの存在自体を露出しない
4. While 全テナント横断監査ログ閲覧経路にアクセスする管理者のロールが SuperAdmin 以外であるとき, the Audit Service shall 当該要求を権限不足として拒否する
5. Where 管理者のロールが Viewer であるとき, the Audit Service shall 自テナント監査ログ閲覧経路・全テナント横断監査ログ閲覧経路のいずれにおいても監査ログの閲覧を拒否する
6. The Audit Service shall 監査ログ閲覧の認可判定を、A2 / #37 で確立済みの許可マトリクス（audit_log read own-tenant = SuperAdmin・TenantAdmin / cross-tenant = SuperAdmin のみ）に基づいて行う

### Requirement 5: 保持期間に基づく読取範囲の制限

**Objective:** As a SaaS 運営者, I want 監査ログの閲覧が設定された保持期間に基づいて行われる, so that 保持ポリシーと一致した範囲のみが参照され、契約・規制要件に整合する

#### Acceptance Criteria

1. The Audit Service shall 監査ログの読取対象範囲を、設定された保持期間（既定 180 日、設定値で変更可能）に基づいて制限する
2. While 監査ログ閲覧要求が行われているとき, the Audit Service shall 発生時刻が保持期間の起点（現在時刻 − 保持期間）より前の監査ログを結果に含めない
3. When 閲覧要求の期間条件（from / to）が保持期間の起点より前を含むとき, the Audit Service shall 保持期間の起点以降のレコードのみを返し、保持期間外のレコードを除外する
4. Where 保持期間が設定値で変更されているとき, the Audit Service shall 変更後の保持期間に基づいて読取対象範囲を決定する

### Requirement 6: 記録後の不変性（改竄・削除不可）の連携検証

**Objective:** As a コンプライアンス担当者, I want 記録済みの監査ログが管理者・アプリケーションのいずれからも改竄・削除できない, so that 監査証跡の完全性を証明できる

#### Acceptance Criteria

1. The Audit Service shall 記録済み監査ログを、アプリケーション経路から更新・削除できない状態に保つ
2. If アプリケーションが利用する書込権限で監査ログレコードの更新が試行されたとき, the 監査ログストア shall 当該更新を拒否する
3. If アプリケーションが利用する書込権限で監査ログレコードの削除が試行されたとき, the 監査ログストア shall 当該削除を拒否する
4. While SuperAdmin の文脈で全テナント横断操作の監査ログを追記しているとき, the 監査ログストア shall テナント未設定（NULL）の追記を許可しつつ、当該レコードの事後の更新・削除を拒否する

## Non-Functional Requirements

### NFR 1: 保持期間と保持容量

1. The Audit Service shall 監査ログを 180 日以上保持できる構成で動作し、保持期間を設定値（既定 180 日）で変更可能にする
2. While 監査ログの追記が継続しているとき, the 監査ログストア shall 保持期間内の監査ログレコードを欠損なく保持する

### NFR 2: 記録の完全性とテナント分離

1. The Audit Service shall 任意のテナント A の管理者が、自テナント監査ログ閲覧経路でテナント B の監査ログおよびテナント未設定（NULL）の監査ログを参照できない状態を恒常的に保つ
2. The Audit Service shall 通常テナント文脈での監査ログ追記時に、追記対象のテナント識別子が要求元テナントと一致しないレコードの混入を拒否する

### NFR 3: 機密値の非格納と観測性

1. The Audit Service shall 監査ログレコードおよび構造化ログに、ID トークン本体・セッション cookie の生値・パスワード等の機密値の平文を含めない
2. When 監査ログの追記または閲覧が認可・永続化エラーで失敗したとき, the Audit Service shall 失敗種別（権限不足 / テナント不一致 / 永続化エラー等）を識別可能な構造化ログを記録する

## Out of Scope

- 監査ログ閲覧 UI（tenant-console / admin-console の画面実装。#18 / #21 で扱う）
- 各ドメインにおける監査イベントの **実発火**（テナント作成・削除、ロール変更、トークン発行、ポリシー変更、WIPE・全コマンド発行等。umbrella Req 2.9 / 3.7 / 4.8 / 6.8 / 6.9 に対応する各ドメイン Issue 側で Record を呼び出す）
- 監査ログストアのスキーマ・append-only 強制（記録後の更新・削除拒否）・INSERT-only 書込権限・保持期間設定の **新規作成**（A2 / #33 / #37 で実装済みであり、本 Issue は利用・連携のみ）
- 保持期間を超えた監査ログの **物理削除・アーカイブ・パージのバッチ処理**（本 Issue は読取範囲の制限のみを扱い、物理 purge は別途検討）
- 監査ログのエクスポート（CSV / JSON ダウンロード）・外部 SIEM 連携・長期アーカイブストレージへの転送
- 監査ログの改竄検知のための暗号学的ハッシュチェーン・署名等の追加的完全性保証（ストアレベルの append-only 強制を超える施策）
- 監査ログ閲覧結果のページネーション方式の詳細（offset / cursor 等の方式選定は design.md の領分）

## 未解決事項 / 確認事項

- 監査ログ閲覧結果の **件数上限・ページネーション** の要否と方式が Issue 本文・umbrella requirements に明記されていない。横断閲覧では結果が大量になり得るため MVP での上限・ページング有無の方針を確認したい（本要件では方式詳細を Out of Scope とし、design.md に委ねた）。
- umbrella requirements の Open Question にあった「Viewer が監査ログを閲覧可能か」は、本 Issue で **TenantAdmin 以上に確定**（Issue 受入基準 + Permission Matrix が根拠。Requirement 4.1 / 4.5 に反映済み）。umbrella requirements 側の Open Question の是正（クローズ）は umbrella 起票者 / PjM の判断を要する。
- 保持期間（umbrella NFR 4.2 で「180 日以上」を仮置き）は、想定顧客の契約・規制要件に応じて見直しの余地がある（umbrella Open Question と同根）。本要件では設定の既定 180 日・設定可能を前提とした。
- 監査イベント種別（event_type）の語彙の **正規セット**（tenant_create / role_change / policy_change / command_wipe / token_issue 等）の確定範囲は、各ドメイン Issue の発火実装に依存する。本 Issue は任意の型付きイベントを受け付ける記録 IF の提供までを責務とし、語彙の網羅確定は各ドメイン Issue 側で行う想定。
