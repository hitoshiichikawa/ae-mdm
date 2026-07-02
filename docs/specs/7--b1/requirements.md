# Requirements Document

## Introduction

ae-mdm（Android Enterprise EMM SaaS）MVP において、業務端末を QR コードで管理下に置くための
**端末エンロール（B1）** を定義する。エンロールが成立しないと EMM の起点（ポリシー配信・
コマンド・状態可視化）が一切成立しないため、本機能は MVP の基盤である。本 Issue の責務は
(1) Fully Managed / Dedicated(Kiosk) モードのエンロールメントトークン発行と QR 表示用データ生成、
(2) AMAPI からの ENROLLMENT 通知を契機とした発行元テナントの端末インベントリ登録、
(3) 期限切れ・使用済みトークン・テナント不一致などの異常系の適切な処理と可視化、
(4) 発行の監査ログ記録、にある。本要件は umbrella #24 の Requirement 3（端末エンロール）および
Requirement 8.2 / NFR 1.2 を、Enrollment という実装単位に focus して切り出したものである。

## 関連

- Depends on: #34 #38 #5 #39
- Parent: #24
- Related: #40

> 補足: #34（A4a AMAPI Client 共有ラッパ）が提供する `CreateEnrollmentToken` を用いてトークンを
> 発行し、AMAPI への低レベル呼び出し（認証・再試行・エラーマッピング）は当該ラッパに委ねる。
> #38（A4b Tenant Service）が保持する enterprise 識別子とテナントの対応関係を、通知処理時の
> テナント特定の前提として利用する。#39（A6b Notification Dispatcher）が enterprise_name →
> tenant_id の逆引き・未割当退避・重複排除・種別別 dispatch を担い、本 Issue が所有するのは
> ENROLLMENT 種別のドメインハンドラの実体である。#5（A5 Audit Service）の記録 IF に発行イベントを
> 渡す。#40（B2b Policy Service）は Dedicated で紐づける Kiosk ポリシーの提供元であり参照関係にある
> （非ブロッキング）。

## Requirements

### Requirement 1: エンロールメントトークンの発行（モード別・QR データ生成）

**Objective:** As a TenantAdmin または Operator, I want Fully Managed / Dedicated(Kiosk) モードのエンロールメントトークンを発行し QR 表示用データを得られる, so that 業務端末を最小限の手順で管理下に置ける

#### Acceptance Criteria

1. When 管理者が FULLY_MANAGED モードを指定してトークン発行を要求したとき, the Enrollment Service shall 個人利用不可（personal usage 不許可）のエンロールメントトークンを AMAPI 経由で生成し、対応する QR コード表示用データを返す
2. When 管理者が DEDICATED モードを指定してトークン発行を要求したとき, the Enrollment Service shall 個人利用不可かつ Kiosk ポリシーを紐づけたエンロールメントトークンを AMAPI 経由で生成し、対応する QR コード表示用データを返す
3. When トークンを発行したとき, the Enrollment Service shall 発行テナント識別子（tenant_id）と発行管理者を内部メタデータ（additionalData）として当該トークンに付与する
4. If トークン発行要求のモードが未指定または FULLY_MANAGED / DEDICATED 以外の値であるとき, the Enrollment Service shall トークンを生成せず、不正なモード指定として呼び出し側にエラーを返す
5. If DEDICATED モードでありながら紐づける Kiosk ポリシーが未指定または自テナントに存在しないとき, the Enrollment Service shall トークンを生成せず、当該ポリシーの未指定・未検出として呼び出し側にエラーを返す
6. If AMAPI がトークン生成を再試行不可なエラーとして返したとき, the Enrollment Service shall 当該トークンを発行済みとして永続化せず、当該エラーを呼び出し側に伝達する

### Requirement 2: トークン発行の認可・テナント境界

**Objective:** As a セキュリティ運用者, I want トークン発行を許可ロールと自テナント境界に限定する, so that 権限のない管理者や越境操作による不正な端末登録を防げる

#### Acceptance Criteria

1. Where 発行要求元のロールが TenantAdmin または Operator であるとき, the Enrollment Service shall トークン発行を許可する
2. If 発行要求元のロールが Viewer であるとき, the Enrollment Service shall トークン発行を権限不足として拒否する
3. If 発行要求元が自テナント以外を対象にトークン発行を試みたとき, the Enrollment Service shall 当該操作を拒否し、他テナントのリソースの存在を露出しない

### Requirement 3: ENROLLMENT 通知による端末インベントリ登録

**Objective:** As a SaaS 運営者および TenantAdmin, I want エンロール完了通知を契機に端末が発行元テナントの端末インベントリへ登録される, so that 管理対象端末の現況を通知駆動で把握できる

#### Acceptance Criteria

1. When ENROLLMENT 通知を受信し、additionalData の tenant_id が当該通知の enterprise から特定したテナントと一致したとき, the Enrollment Notification Handler shall 当該端末を発行元テナントの端末インベントリに登録または更新する
2. If ENROLLMENT 通知の additionalData の tenant_id が当該通知の enterprise から特定したテナントと一致しないとき, the Enrollment Notification Handler shall 当該通知を破棄せず「未割当」状態で記録し、いずれのテナントの端末インベントリも更新しない
3. If ENROLLMENT 通知に additionalData の tenant_id が欠落しているとき, the Enrollment Notification Handler shall 当該通知を「未割当」状態で記録し、いずれのテナントの端末インベントリも更新しない
4. When 同一端末についての ENROLLMENT 通知を複数回受信したとき, the Enrollment Notification Handler shall 当該端末を重複登録せず、既存端末レコードの更新として冪等に処理する
5. When Android 10 未満であることにより AMAPI または ADP がエンロールを拒否した結果が観測されたとき, the Enrollment Notification Handler shall 当該端末をコンプライアンス状態「サポート対象外」として端末インベントリに残す
6. If ENROLLMENT 通知の処理が一時的（再試行で回復しうる）に失敗したとき, the Enrollment Notification Handler shall 当該通知を処理完了扱いにせず、再処理対象として保持する

### Requirement 4: 期限切れ・使用済みトークンのエラー伝達

**Objective:** As a TenantAdmin または Operator, I want 無効なトークンでのエンロール試行がエラーとして伝わり端末が登録されない, so that 期限切れ・使い回しによる不正なエンロールを検知できる

#### Acceptance Criteria

1. If エンロールメントトークンが有効期限切れの状態でエンロールが試行されたとき, the Enrollment Service shall 当該端末を端末インベントリに登録せず、期限切れである旨を管理者が把握できる形で伝達する
2. If エンロールメントトークンが使用済み（既に消費済み）の状態でエンロールが試行されたとき, the Enrollment Service shall 当該端末を端末インベントリに登録せず、使用済みである旨を管理者が把握できる形で伝達する

### Requirement 5: トークン発行の監査ログ記録

**Objective:** As a 監査担当者, I want トークン発行が監査ログに記録される, so that 誰がいつどのモードのトークンを発行したかを事後追跡できる

#### Acceptance Criteria

1. When エンロールメントトークンを発行したとき, the Enrollment Service shall 発行者・テナント・モード（Fully Managed か Dedicated か）・有効期限・結果を監査ログ記録の対象として渡す
2. The Enrollment Service shall 監査ログに渡す詳細に、エンロールメントトークンの秘密値（AMAPI が返す Value）および QR コードの秘密値を含めない

### Requirement 6: エンロールフローの検証（結合テスト）

**Objective:** As a 開発者, I want トークン発行から端末登録までの主要動線が結合テストで検証される, so that 発行・登録・未割当退避の回帰を防げる

#### Acceptance Criteria

1. The Enrollment Service shall トークン発行後に模擬 ENROLLMENT 通知を処理すると当該端末が発行元テナントの端末インベントリに登録されることを、結合テストで検証可能にする
2. The Enrollment Notification Handler shall additionalData の tenant_id と enterprise から特定したテナントが不一致の通知が「未割当」状態で記録されることを、結合テストで検証可能にする
3. The Enrollment Notification Handler shall 同一端末についての ENROLLMENT 通知を複数回処理しても端末が重複登録されないことを、結合テストで検証可能にする

## Non-Functional Requirements

### NFR 1: プラットフォーム互換性

1. The Enrollment Service shall Android 10 以上を実行する端末のエンロール（トークン発行および端末インベントリ登録）をサポートする

### NFR 2: テナント分離

1. The Enrollment Notification Handler shall テナント A 宛の ENROLLMENT 通知の処理によってテナント B の端末インベントリを更新しない状態を恒常的に保つ
2. While ENROLLMENT 通知のテナントが一意に特定できていないとき, the Enrollment Notification Handler shall いかなるテナントの端末インベントリも更新しない

### NFR 3: 秘密情報の非記録

1. The Enrollment Service shall エンロールメントトークンの秘密値（AMAPI が返す Value）および QR コードの秘密値を、構造化ログ・監査ログ・永続化レコードのいずれにも保存しない

### NFR 4: 可観測性

1. The Enrollment Notification Handler shall 未割当退避・サポート対象外記録・処理失敗の各イベントについて、運用者が対象端末・enterprise・原因を事後追跡できる構造化ログを出力する

## Traceability（umbrella #24 との対応）

| 本 Issue | umbrella #24 の対応 |
|---|---|
| Req 1.1 | Req 3.1 |
| Req 1.2 | Req 3.2 |
| Req 1.3 | Req 3.3 |
| Req 1.4 / 1.5 / 1.6 | Req 3.1 / 3.2 の異常系・境界（新規細分化） |
| Req 2.1 / 2.2 | Req 3 Objective / Req 2.4（Viewer 拒否） |
| Req 2.3 | Req 1.5 / NFR 2.1（テナント分離） |
| Req 3.1 | Req 3.4 / Req 8.2 |
| Req 3.2 | Req 3.5 / NFR 2.2 |
| Req 3.3 | Req 3.5 |
| Req 3.4 | Req 8.2（登録または更新の冪等性） |
| Req 3.5 | NFR 1.2 / Req 8.2（サポート対象外） |
| Req 3.6 | Req 8.7（再処理保持） |
| Req 4.1 / 4.2 | Req 3.6 |
| Req 5.1 | Req 3.7 |
| Req 5.2 / NFR 3.1 | 機密値の非記録（#34 NFR / CLAUDE.md 機密情報方針） |
| NFR 1.1 | NFR 1.1 |
| NFR 2.1 / 2.2 | NFR 2.1 / 2.2 |

## Out of Scope

- エンロール UI（モード選択・QR コード描画・エラー表示画面）— #15 および tenant-console
  `features/enroll` Issue（umbrella #24 tasks 12.3）の範囲。本 Issue は QR 表示用データを返すのみで、
  QR 画像の生成・描画は行わない
- zero-touch enrollment / Knox Mobile Enrollment / NFC プロビジョニング（MVP 対象外 / umbrella Out of Scope）
- STATUS_REPORT / COMMAND 種別の通知処理 — Device Service / Command Service の各 Issue の範囲。
  本 Issue は ENROLLMENT 種別のみを所有する
- Pub/Sub クライアント・worker エントリ・通知の重複排除・enterprise_name → tenant_id 逆引き・
  未割当退避キューそのもの — #35 / #39 で実装済み。本ハンドラは Notification Dispatcher から
  dispatch される実体として振る舞う
- AMAPI Client 共有ラッパ（`CreateEnrollmentToken` の認証・再試行・エラーマッピング）— #34 で
  実装済み。本サービスは当該 IF を呼び出すのみ
- 監査ログストア・append-only 強制・保持期間・閲覧 UI — #5 および基盤 Issue の範囲。本サービスは
  記録対象イベントを Audit Service に渡すのみ
- RBAC 許可マトリクスの定義・OIDC 認証・セッション発行・DB Row-Level Security ポリシー — #33 / #37
  の範囲。本サービスはテナント分離・認可の前提としてこれらに依拠する
- Kiosk ポリシー本体の作成・更新・検証 — #40（Policy Service）/ #36（Policy Validator）の範囲。
  本サービスは既存の Kiosk ポリシーをトークンに紐づけるのみ
- `enrollment_tokens` / `devices` テーブルの新規マイグレーション作成（既存スキーマ 0004 / 0006 を
  消費する前提）

## 用語

| 用語 | 意味 |
|---|---|
| エンロール | Android 端末を AMAPI 管理下に置き、発行元テナントに紐づける手続き |
| Fully Managed | 端末全体を管理する完全管理モード（個人利用不可） |
| Dedicated(Kiosk) | 単一/限定用途に固定する専用端末モード（個人利用不可・Kiosk ポリシー紐付け） |
| エンロールメントトークン | エンロールを許可する一時的なトークン。QR に埋め込まれる |
| QR 表示用データ | QR コードを描画するための元データ（画像描画は UI の責務） |
| additionalData | トークンに付与する内部メタデータ。tenant_id と発行管理者を含む |
| ENROLLMENT 通知 | エンロール完了を伝える AMAPI からの Pub/Sub 通知 |
| enterprise | AMAPI がテナントごとに払い出す Enterprise 識別子（テナント特定の鍵） |
| 未割当 | additionalData の tenant_id とテナントが一致・特定できない通知の退避状態 |
| サポート対象外 | Android 10 未満などで管理対象にできない端末のコンプライアンス状態 |

## 確認事項 / 未決事項

> 注: Issue #7 の既存コメントは triage bot 生成の自動コメントのみで、人間による決定事項の回答は
> 存在しない。以下は Issue 本文・umbrella spec からは一意に確定できない論点であり、推測で確定せず
> 設計・PR レビューで人間判断を仰ぐ。

- **期限切れ・使用済みトークンのエラー伝達経路（Requirement 4）**: umbrella Req 3.6 は「AMAPI からの
  エラーを管理者画面に伝達」と記すが、無効トークンでのエンロール試行は端末側で AMAPI に拒否され、
  成功時のような ENROLLMENT 通知が届かない可能性がある。管理者が期限切れ・使用済みを把握する経路が
  (a) トークン自身の状態表示（backend が保持する expires_at / 消費状態に基づく）か、
  (b) 失敗イベント通知の受信か、が未確定。本要件では observable な結果（登録しない・旨を伝達）で
  記述し、伝達 UX は #15 の範囲・伝達元は design で確定したい
- **「使用済み（消費済み）」の判定源**: 既存 `enrollment_tokens`（migration 0004）に消費状態を追跡する
  列が存在するかが本要件確定時点で不明。トークンの one-time 消費を backend 側で追跡するか、AMAPI 側の
  状態に委ねるかが未確定。Requirement 4.2 は「使用済みの状態でエンロールが試行されたとき」の observable
  な挙動のみを定義した
- **サポート対象外（Android 10 未満）の観測契機（Requirement 3.5）**: umbrella tasks 7.2 は
  「Android 10 未満で AMAPI/ADP に拒否された場合の応答を受けて」記録するとするが、拒否がどの通知
  （ENROLLMENT か STATUS_REPORT か）・どの応答で観測されるかが未確定。本要件では「拒否結果が観測された
  とき」の observable な挙動（サポート対象外として残す）を記述し、観測契機は design で確定したい
- **QR 秘密値の非永続化と再表示不可（NFR 3.1）**: 秘密値（AMAPI が返す Value）を永続化しない規約
  （#34 由来）に従うと、一度発行した QR を後から再表示できない。MVP でエンロール QR は「発行時に一度だけ
  表示」する UX で問題ないか（再表示要件の有無）を確認したい。本要件では非永続化を NFR として確定し、
  再表示の要否は #15 側の論点として送る
- **Operator による発行の許可範囲**: umbrella Req 3 Objective は Operator を含むが、Req 2.5（Operator の
  権限定義）にはエンロールトークン発行が明記されていない。本要件では Issue 本文の指示に従い Operator に
  発行を許可（Requirement 2.1）したが、Req 2.5 との整合を人間に確認したい
