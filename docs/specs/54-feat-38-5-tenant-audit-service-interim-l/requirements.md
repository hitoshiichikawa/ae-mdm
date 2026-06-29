# Requirements Document

## Introduction

PR #51（#38 / A4b Tenant Service）を、A5 監査ログ Service（#5 / PR #50）を含む統合先へ merge した結果、
Tenant Service の監査記録は interim 実装（構造化ログ出力のみの記録器）に配線されたままで、同一バイナリに
同居した監査ログ Service（`audit_logs` への append-only 永続化）へ繋がっていない。このため tenant の
作成 / bind / 無効化イベントが `/api/admin/audit-logs` に永続化されず、ログ出力だけで失われている。
これは退行ではなく、#38 design が「interim 記録器は暫定 binding、監査ログ Service の記録器への差し替えは
将来 main の責務」と明記した「先送りされた統合」であり、監査ログ Service の着地で初めて統合が可能かつ
必要になった。本要件は、tenant 監査イベントを監査ログ Service へ配線するアダプタの振る舞いを、機密非漏洩・
fail-closed の射程・イベント写像規則を含めて確定する。

## 関連

- Parent: #38
- Related: #5

## Requirements

### Requirement 1: tenant 監査イベントの監査ログ Service への配線

**Objective:** As a SaaS 運営者, I want tenant の作成 / bind / 無効化イベントが監査ログ Service 経由で永続化される, so that テナント操作の監査証跡が `/api/admin/audit-logs` で閲覧でき、ログ出力だけで失われない

#### Acceptance Criteria

1. When tenant の作成 / bind / 無効化が成功したとき, the Tenant 監査配線 shall 当該イベントを監査ログ Service へ記録要求として渡し、`audit_logs` へ追記させる
2. When tenant の作成 / bind / 無効化が失敗（拒否を含む）したとき, the Tenant 監査配線 shall 当該イベントを結果区分「失敗」として監査ログ Service へ記録要求として渡す
3. When tenant 監査イベントが監査ログ Service 経由で永続化されたのち, the Tenant 監査配線 shall 当該イベントを `/api/admin/audit-logs`（全テナント横断閲覧経路）で閲覧可能な状態にする
4. The Tenant 監査配線 shall tenant 経路の監査記録先を interim の構造化ログ出力のみの記録器ではなく監査ログ Service へ向ける

### Requirement 2: tenant イベントから監査イベントへの写像

**Objective:** As a 監査記録を残すドメイン, I want tenant のイベント表現を監査イベント表現へ過不足なく写像できる, so that 異なる型をまたいでも実行者・対象テナント・操作種別・結果が一貫して記録される

#### Acceptance Criteria

1. The Tenant 監査配線 shall tenant イベントの実行者識別子を監査イベントの実行者識別子へ写像する
2. The Tenant 監査配線 shall tenant イベントの対象テナント識別子を監査イベントのテナント識別子へ写像する
3. The Tenant 監査配線 shall tenant イベントの操作結果（成功 / 失敗）を監査イベントの結果区分（成功 / 失敗）へ写像する
4. When tenant イベントの操作種別が「作成」であるとき, the Tenant 監査配線 shall 監査イベント種別を `tenant_create` として写像する
5. When tenant イベントの操作種別が「bind」であるとき, the Tenant 監査配線 shall 監査イベント種別を `tenant_bind` として写像する
6. When tenant イベントの操作種別が「無効化」であるとき, the Tenant 監査配線 shall 監査イベント種別を `tenant_disable` として写像する
7. If tenant イベントの操作種別が定義済みの 3 種別（作成 / bind / 無効化）のいずれにも一致しないとき, the Tenant 監査配線 shall 当該イベントを未知種別として識別可能なイベント種別へ写像し、無音で破棄しない
8. The Tenant 監査配線 shall 対象リソース識別子として対象テナント識別子の文字列表現を監査イベントへ写像する
9. While 監査イベントのレコード識別子および発生時刻が未設定（採番前 / 現在時刻補完前）であるとき, the Tenant 監査配線 shall これらを自前で確定させず監査ログ Service の採番・現在時刻補完に委ねる

### Requirement 3: fail-closed の射程（永続化失敗の伝播）

**Objective:** As a コンプライアンス担当者, I want 監査ログ Service の fail-closed 挙動（永続化失敗を成功扱いしない）が tenant 経路でも有効になる, so that 監査永続化に失敗したイベントが「記録済み」と誤認されない

#### Acceptance Criteria

1. If 監査ログ Service が tenant 監査イベントの永続化エラーを返したとき, the Tenant 監査配線 shall 当該エラーをアダプタ境界で握りつぶさず呼び出し側（tenant Service の監査記録 helper）へ伝播する
2. If 監査ログ Service が tenant 監査イベントの永続化エラーを返したとき, the Tenant 監査配線 shall 当該記録を成功として扱わない
3. While tenant Service の監査記録 helper が現状の error 握りつぶし契約（永続化失敗時に観測ログのみ残しユースケース本体のエラー経路を上書きしない）を維持しているとき, the Tenant 監査配線 shall 当該 helper の error-handling 契約を本統合で変更しない
4. When 監査ログ Service の永続化失敗が観測されたとき, the 監査ログ Service shall 失敗種別を識別可能な構造化ログ（`failure_kind=persist_error`）を記録する

### Requirement 4: 機密値の非漏洩（一次防御の維持）

**Objective:** As a セキュリティ運用者, I want 機密値が監査イベントの詳細（detail）に漏れない, so that サインアップ URL token・SA 資格情報・OAuth トークン生値が監査ログへ surface しない

#### Acceptance Criteria

1. The Tenant 監査配線 shall 機密値（サインアップ URL の秘密パラメータ・サービスアカウント資格情報・OAuth トークンの生値）を監査イベントの詳細へ載せない
2. Where tenant イベントが非機密フィールド（二段階確認の完了有無・拒否理由）を保持しているとき, the Tenant 監査配線 shall これらを監査イベントの詳細へ機械可読な鍵名で写像してよい
3. The Tenant 監査配線 shall tenant イベントが機密値フィールドを構造的に一切保持しないという一次防御の前提（#38 NFR 2.3）に依拠し、機密値を新たに detail へ持ち込む経路を作らない
4. If 写像により監査イベントの詳細へ載せ得る非機密フィールドが存在しないとき, the Tenant 監査配線 shall 詳細を未設定（空）として監査イベントへ渡す

### Requirement 5: アダプタの責務境界

**Objective:** As a 保守担当者, I want tenant イベントを監査ログ Service へ橋渡しするアダプタの責務が単一に保たれる, so that 変換・委譲以外のロジックが混入せず将来の差し替えが容易になる

#### Acceptance Criteria

1. The Tenant 監査配線 shall tenant Service が利用する監査記録ポートの契約を満たす形で監査ログ Service への記録要求に適合する
2. The Tenant 監査配線 shall 自身の責務を「tenant イベントから監査イベントへの変換」と「監査ログ Service への委譲」の 2 点に限定し、再試行・抑制・フィルタリング等の追加判断を持たない
3. When アプリケーション起動時の依存配線が行われるとき, the 配線処理 shall tenant Service へ interim の構造化ログ記録器ではなくアダプタ経由の監査ログ Service を注入する

## Non-Functional Requirements

### NFR 1: 既存挙動の互換維持

1. The Tenant 監査配線 shall 本統合の対象外である tenant ユースケース本体（作成 / bind / 無効化の HTTP 応答契約）の成功・失敗判定を変更しない
2. The Tenant 監査配線 shall 監査イベント種別の文字列値を tenant 監査閲覧の絞り込み条件（`tenant_create` / `tenant_bind` / `tenant_disable`）として一貫して利用可能な固定値に保つ

### NFR 2: 観測性

1. When tenant 監査イベントの永続化が成功または失敗したとき, the 監査経路 shall 機密値を含まない構造化ログで結果を識別可能にする

## Out of Scope

- tenant ユースケース本体（作成 / bind / 無効化）の HTTP 応答を、監査永続化失敗時に失敗へ転じさせる
  変更（= 論点 A の選択肢 (b)）。本要件では fail-closed の射程をアダプタ境界での error 伝播
  （Req 3.1 / 3.2）に限定し、ユースケース本体の error-handling 契約変更は扱わない（根拠は Open Questions 参照）
- `audit_logs` への永続化ロジック・スキーマ・append-only 強制・保持期間設定（#5 / A2 / #33 / #37 で実装済み。本統合は利用・配線のみ）
- AMAPI Client・tenant Service のユースケースロジック・RBAC ガード自体の実装（#34 / #38 / #37 の範囲）
- 監査ログ閲覧 UI・ページネーション方式・エクスポート（#18 / #21 および design.md の領分）
- interim の構造化ログ記録器実装の削除可否の判断（本統合では配線を監査ログ Service へ切り替えることのみを扱い、interim 実装コードの撤去・残置は別途判断）

## Open Questions

- **論点 A（fail-closed の射程）は本要件で選択肢 (a)（アダプタ境界での error 伝播）に確定した。**
  根拠: (1) tenant Service の監査記録 helper は「永続化失敗時に観測ログのみ残しユースケース本体の
  エラー経路を上書きしない」挙動を、`auth.revokeOnExpire` と同方針の意図的契約として明記している
  （`internal/tenant/service.go:431-452`）。(2) #38 design は本配線を「将来 main の責務」と位置付けており、
  ユースケース本体の error 契約変更は本統合のスコープに含まれない。(3) 監査ログ Service の fail-closed
  （#5 Req 1.6「呼び出し側へエラーを返し成功扱いしない」）は、アダプタが自身の境界で error を再度
  握りつぶさない限り満たされる。ユースケース本体が伝播後の error をどう扱うか（現状は握りつぶし）は
  tenant Service の既存・別管理の契約である。**選択肢 (b)（HTTP 応答まで失敗させる）を採るべきという
  経営/コンプラ判断がある場合は、tenant Service の error-handling 契約変更を伴う別 Issue として切り出す
  必要があるため、人間レビューでの確認を残す。**
- 監査イベント種別の正規語彙（`tenant_create` / `tenant_bind` / `tenant_disable`）は本統合で確定した
  （監査ドキュメントの canonical 例 `tenant_create` に整合）。他ドメインのイベント種別との衝突回避方針は
  各ドメイン Issue 側の責務であり、本要件の対象外。

## Traceability

| 本要件 | 由来 |
|---|---|
| Requirement 1〜5 全体 | Issue #54（先送りされた統合の実体化） / Parent #38 / Related #5 |
| Requirement 1（配線・閲覧可能化） | Issue 受入基準「tenant の作成 / bind / 無効化が `/api/admin/audit-logs` で閲覧可能になる」 |
| Requirement 2（イベント写像 / 種別マッピング / ResourceID） | Issue 統合作業「tenant イベント → 監査イベントの写像」「種別 → 監査イベント種別の対応付け」 / 既存監査イベント契約 |
| Requirement 3（fail-closed の射程） | #5 Req 1.6（fail-closed） / 既存 tenant 監査記録 helper の握りつぶし契約（`internal/tenant/service.go:431-452`） |
| Requirement 4（機密非漏洩） | #38 NFR 2.3（機密値フィールドを構造的に持たない一次防御） / #5 Req 1.7（sanitize は呼び出し側責務） |
| Requirement 5（アダプタ責務境界） | Issue 統合作業「アダプタ」/ tenant 監査記録ポートと監査ログ Service の型不一致の橋渡し |
| NFR 1（互換維持） | #38 design「interim binding → 将来 main の責務」/ 退行ではない統合という位置付け |
