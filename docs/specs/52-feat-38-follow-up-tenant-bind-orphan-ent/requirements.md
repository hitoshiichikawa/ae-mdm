# Requirements Document

## Introduction

親 Issue #38（A4b Tenant ドメイン Service）で実装した Enterprise バインドの楽観的競合制御には、
PR #51 のコードレビューで 2 つの高優先度の弱点が指摘された。本 follow-up は当該 2 点を hardening
する。第 1 は「並行 bind / bind 中の無効化で競合に敗れた要求が 409 を返した時点で、既に AMAPI に
作成済みの Enterprise が DB のどのテナント行にも紐付かない orphan として残る」問題。第 2 は
「`signup_url_name` が永続化されず bind リクエスト body から渡されるため、発行元テナントへの束縛が
担保されない」問題。いずれも #37 ガードにより admin-console + SuperAdmin に限定済みであり、
無認可越境ではなく同一 SuperAdmin の運用ミスや並行性によるリソース不整合を防ぐ hardening である。
本要件は #38 の requirements / NFR を一部更新・上書きする（後述 Traceability 参照）。

## 関連

- Parent: #38
- Related: #51

> 補足: 本 Issue は親 #38 の確定 spec（`docs/specs/38--a4b-tenant-service/requirements.md` /
> `design.md`）が定義した Bind 処理フローと状態機械を強化対象とする。PR #51（#38 の実装 PR）の
> レビュー指摘から派生した。

## Requirements

### Requirement 1: バインド予約状態による orphan Enterprise の防止

**Objective:** As a SaaS 運用者（SuperAdmin）, I want 並行 bind や bind 中の無効化で競合が起きても AMAPI 上に未紐付けの Enterprise が残らない, so that 競合に敗れた要求が AMAPI リソースを無駄に作成・放置することを防げる

#### Acceptance Criteria

1. When SuperAdmin が「バインド未完了」状態のテナントに対して Enterprise バインドを要求したとき, the Tenant Service shall 当該テナントの状態を「バインド予約中（binding）」へ原子的に遷移させ、当該遷移に成功した要求のみを Enterprise 作成に進める
2. When 同一テナントに対して複数の Enterprise バインド要求が並行して到達したとき, the Tenant Service shall 「バインド未完了」から「バインド予約中」への遷移に成功した 1 要求のみを勝者とし、それ以外の要求は Enterprise 作成を実行する前に競合として呼び出し側にエラーを返す
3. If テナントが既に「バインド予約中」状態であるときに新たなバインド要求が到達したとき, the Tenant Service shall 当該要求について Enterprise を新規作成せず、競合として呼び出し側にエラーを返す
4. When 「バインド予約中」への遷移に成功した勝者要求の Enterprise 作成が成功したとき, the Tenant Service shall 当該テナントの状態を「バインド予約中」から「バインド済み（bound）」へ遷移させ、作成された enterprise 識別子を保存する
5. If 「バインド予約中」への遷移に成功した勝者要求の Enterprise 作成が失敗したとき, the Tenant Service shall 当該テナントを enterprise 識別子未保存の状態に保ち、後続の再バインドが可能な状態へ回復させる
6. While テナントが「バインド予約中」状態であるとき, the Tenant Service shall 当該テナントに対する無効化操作の前提状態判定を、予約中要求と無効化要求のいずれか一方のみが確定するよう競合制御する
7. When Enterprise バインドの成否（予約失敗・Enterprise 作成失敗・確定成功）が確定したとき, the Tenant Service shall 当該バインドイベント（実行者・テナント識別子・結果）を監査ログ記録の対象とする

### Requirement 2: 中断したバインド予約の回収

**Objective:** As a SaaS 運用者（SuperAdmin）, I want クラッシュや AMAPI タイムアウトで「バインド予約中」のまま中断した行が放置されず回収される, so that 予約状態のテナントが恒久的にバインド不能・無効化不能のまま塩漬けになることを防げる

#### Acceptance Criteria

1. If テナントが「バインド予約中」状態のまま完了・失敗のいずれにも確定せず中断したとき, the Tenant Service shall 当該テナントを再びバインド可能または無効化可能な状態へ回収する手段を提供する
2. When 「バインド予約中」のまま中断したテナントに対して再度バインドが要求されたとき, the Tenant Service shall 既存の予約を冪等に扱い、enterprise 識別子が二重に作成・紐付けされない形でバインドを進行または完了させる
3. While テナントが「バインド予約中」状態であるとき, the Tenant Service shall 当該テナントを「バインド済み」とはみなさず、業務操作（端末・ポリシー等の AMAPI 操作）の前提として未バインドと判定する
4. When バインド予約の回収が行われたとき, the Tenant Service shall 当該回収イベント（対象テナント識別子・回収前状態・結果）を監査ログまたは構造化ログの記録対象とする

### Requirement 3: signup_url_name の発行元テナントへの束縛

**Objective:** As a SaaS 運用者（SuperAdmin）, I want signup_url_name が発行元テナントに束縛され、他テナントの値で bind できない, so that 同一 SuperAdmin の運用ミスにより別テナントのサインアップ識別子を取り違えてバインドする事故を防げる

#### Acceptance Criteria

1. When テナント作成時にサインアップ URL とその識別子（signup_url_name）が発行されたとき, the Tenant Service shall 当該 signup_url_name を発行元テナントのレコードに永続化する
2. While テナントに signup_url_name が永続化されているとき, the Tenant Service shall 当該テナントのバインドにおいて永続化済みの signup_url_name を発行元テナントに束縛された正本として扱う
3. If 発行元テナント以外の signup_url_name を用いてバインドが要求されたとき, the Tenant Service shall 当該バインドを fail-closed で拒否し、Enterprise を作成しない
4. If バインド対象テナントに signup_url_name が永続化されていないとき, the Tenant Service shall 当該バインドを fail-closed で拒否し、Enterprise を作成しない

### Requirement 4: 状態機械の 4 値化に伴う遷移整合

**Objective:** As a SaaS 運営者, I want テナント状態が binding を含む 4 値で常に整合した遷移のみを許す, so that 予約状態を含む拡張後も部分遷移や未定義遷移による不整合行が生まれない

#### Acceptance Criteria

1. The Tenant Service shall テナント状態を pending_bind / binding / bound / disabled の 4 値のいずれか 1 つに常に保持する
2. If 定義されていない状態遷移（例: bound から binding への遷移、disabled から binding への遷移）が要求されたとき, the Tenant Service shall 当該遷移を実行せず、不正な状態遷移として呼び出し側にエラーを返す
3. If Enterprise バインド処理が「バインド予約中」遷移後の任意の時点で失敗したとき, the Tenant Service shall enterprise 識別子を保存せず、「バインド済み」へ部分的に遷移した状態を残さない
4. When テナント一覧または個別詳細が参照されたとき, the Tenant Service shall 当該テナントの状態を pending_bind / binding / bound / disabled のいずれかとして返す

## Non-Functional Requirements

### NFR 1: 状態遷移の整合性（#38 NFR 1.1 の更新）

1. The Tenant Service shall テナント状態を pending_bind / binding / bound / disabled の 4 値のいずれか 1 つに常に保持する（本 NFR は #38 NFR 1.1 の 3 値定義を 4 値へ更新・上書きする）
2. The Tenant Service shall enterprise 識別子を「バインド済み」状態のテナントでのみ非空として保持し、pending_bind / binding / disabled では enterprise 識別子を未確定として扱う

### NFR 2: 並行性・原子性

1. When 同一テナントに対する複数のバインド要求・無効化要求が並行して到達したとき, the Tenant Service shall いずれか 1 要求のみが状態遷移を確定し、残りの要求を競合として拒否する（lost update を発生させない）
2. While 勝者要求が Enterprise 作成のための外部 I/O を待機しているとき, the Tenant Service shall 当該待機が他テナントへの操作受付を阻害しない形でバインドを処理する

### NFR 3: 可観測性

1. The Tenant Service shall 「バインド予約中」のまま中断したテナント・回収されたテナントの発生を、運用者が観測可能な構造化ログまたは指標として記録する
2. The Tenant Service shall ログ出力にサインアップ URL の秘密パラメータ・signup_url_name の機密該当部分・サービスアカウント資格情報・OAuth トークンの生値を含めない

## Out of Scope

- AMAPI Client 共有ラッパ自体の実装（`CreateSignupURL` / `CreateEnterprise` 等の低レベル呼び出し）—
  #34（A4a）で実装済み。本 follow-up は当該 IF のオーケストレーション方法のみを変更する
- admin-console のテナント管理 UI（一覧・作成・bind・無効化画面）— admin-console Issue の範囲。
  signup_url_name を bind リクエスト body から外すか検証に留めるかで UI の入力契約が変わり得るが、
  本要件では「発行元テナントに束縛される」という挙動のみを規定し、UI 変更は扱わない
- RBAC 許可マトリクスおよび `/api/admin/*` ガード自体 — #37（A3b）で実装済み。全操作は引き続き
  当該ガード配下（admin-console + SuperAdmin）に限定される前提を踏襲する
- 監査ログテーブル（`audit_logs`）への永続化ロジック自体 — Audit Service Issue の範囲。本サービスは
  記録対象イベント（回収イベントを含む）を記録ポートに渡すのみ
- AMAPI 側に既に作成済みで DB に紐付かない既存 orphan Enterprise の遡及的な削除・棚卸し（compensation
  による外部リソースの能動的削除）— 本 follow-up は「新たな orphan を構造的に作らない」ことを範囲とし、
  AMAPI 上の既存 orphan の能動的クリーンアップは扱わない（確認事項参照）
- テナント無効化の二段階確認の入力契約・再有効化フロー — #38 の Open Questions のままで、本 follow-up
  では変更しない
- バインド予約状態を実現する具体的な実装方式（状態列の追加方法・ロック方式・回収を sweep で行うか再 bind
  冪等化で行うか・signup_url_name を bind API body から外すか検証に留めるか）— design.md（Architect の
  領分）で確定する

## Open Questions

- **回収方式の選択（reconciliation sweep か 再 bind 冪等化か）**: 「バインド予約中」のまま中断した行の
  回収を、定期的な reconciliation sweep で行うか、当該行への再 bind を冪等化することで行うか、または
  両方を併用するかは Architect の設計判断に委ねる。本要件（Requirement 2）は「回収手段が存在し、
  二重作成・二重紐付けを起こさない」という観測可能な挙動のみを規定する
- **signup_url_name の bind API 契約の変更範囲**: 永続化値を bind 時に「使う」（リクエスト body から
  signup_url_name を外す）か、リクエスト body の値と永続化値の「一致を検証する」（body は残し不一致を
  拒否する）かは、bind API 契約の変更範囲に関わるため Architect 判断に委ねる。本要件（Requirement 3）は
  「発行元テナントに束縛され、他テナントの値で bind できない」という挙動のみを規定する
- **AMAPI 上の既存 orphan Enterprise への対処**: 本 follow-up 以前に発生済みの、AMAPI 上に存在し DB に
  紐付かない Enterprise の能動的削除・棚卸しを本 Issue の範囲に含めるか、別途運用タスク／別 Issue とするか
  の方針確認が必要（現状は Out of Scope と解釈し、新規 orphan の防止に限定している）
- **中断検出のしきい値**: 「バインド予約中のまま中断した」と判定するための経過時間しきい値（例: 予約から
  N 分経過した binding 行を中断とみなす）を要件として固定すべきか、運用設定に委ねるかが未確定。本要件は
  しきい値の具体値を規定せず、回収手段の存在のみを求めている

## Traceability（#38 spec との関係）

本 follow-up が親 #38 の確定 spec を更新・上書きする箇所を明示する。

| 本要件 ID | #38 spec への影響 |
|---|---|
| Requirement 1 / Requirement 4 / NFR 1.1 | #38 NFR 1.1（状態 3 値 pending_bind / bound / disabled）を **4 値**（+ binding）へ更新・上書きする |
| Requirement 1 | #38 Requirement 2（Enterprise バインド）の処理フローを「pending_bind → 条件付き UpdateBound」から「pending_bind → binding 予約 → 勝者のみ CreateEnterprise → binding → bound」へ強化する（#38 Req 2.1〜2.5 / NFR 1.3 を補強） |
| Requirement 2 | #38 spec に存在しなかった「中断した予約の回収」要件を新規追加する |
| Requirement 3 | #38 Requirement 1（テナント作成）に signup_url_name の永続化を追加し、#38 Requirement 2（バインド）に発行元束縛検証を追加する |
| NFR 2 | #38 design.md の並行性前提を要件レベルで明文化し、原子性・非ブロッキングの観測可能要件を追加する |
| NFR 3 | #38 NFR 2（監査・可観測性）に回収イベントの可観測性を追加する |
