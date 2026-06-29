# Implementation Notes — #54 tenant 監査イベントの監査ログ Service 配線

## 概要

tenant ドメインの監査イベント（作成 / bind / 無効化）を、interim の構造化ログ記録器
（`tenant.LoggerRecorder`）ではなく既存の監査ログ Service（`internal/audit`）へ配線する
アダプタを新規実装し、`cmd/api/main.go` の DI を差し替えた。

## 実装した型・パッケージ

- 新規パッケージ `internal/tenantaudit`（domain-glue）
  - `recorder.go`: `Recorder` 型（`tenant.EventRecorder` を満たす）/ `NewRecorder(audit.Service, logger.Logger)` /
    写像 helper（`mapEventType` / `mapResult` / `buildDetail`）/ EventType 定数 / 成功経路の観測ログ（`logPersisted`）
  - `recorder_test.go`: AC と 1:1 のユニットテスト（spy audit.Service で永続化のみ fake 化）
- 配置理由: tenant / audit 双方を import するため core 2 パッケージへ相互 import を持ち込まず、
  `package main` だと単体テスト不能になるため独立パッケージにした（Req 5 / #38 design と整合）。

## 写像表（Req 2）

| tenant.Event | audit.Event | 備考 |
|---|---|---|
| `Actor` | `ActorID` | Req 2.1 |
| `TenantID` | `TenantID` | Req 2.2 |
| `TenantID.String()` | `ResourceID` | Req 2.8 |
| `Result`(success/failure) | `Result`(ResultSuccess/ResultFailure) | Req 2.3 |
| `Operation=create` | `EventType="tenant_create"` | Req 2.4 |
| `Operation=bind` | `EventType="tenant_bind"` | Req 2.5 |
| `Operation=disable` | `EventType="tenant_disable"` | Req 2.6 |
| 未知 `Operation` | `EventType="tenant_unknown"` | Req 2.7（無音破棄しない） |
| (未設定) | `ID` / `OccurredAt` = zero | Req 2.9（audit.Service が採番・clock 補完） |
| `ConfirmationCompleted` | `Detail["confirmation_completed"]` | Req 4.2 |
| `DenyReason`(非空時のみ) | `Detail["deny_reason"]` | Req 4.2 / 4.4 |

機密値は tenant.Event が構造的に保持しないため Detail へ持ち込む経路を作らない（Req 4.1 / 4.3）。

## 配線差分（cmd/api/main.go）

- `tenantRecorder := tenant.NewLoggerRecorder(log)` → `tenantRecorder := tenantaudit.NewRecorder(auditSvc, log)`
  （main.go:239 付近。`auditSvc` は同 main.go:212 で構築済みのものを再利用、`log` は bootstrap 済み logger を
  成功経路の観測ログ用に渡す / Req 5.3 / NFR 1 / NFR 2.1）。
- section (8) のコメントを「audit Service 未実装のため interim logger」→「audit Service へ
  配線済み」の実態に更新（#54 Req 1 / 5.3）。
- import に `internal/tenantaudit` を追加。
- `tenant.LoggerRecorder` / `tenant/audit_log.go` は削除せず配線から外すのみ（Out of Scope）。

## fail-closed の射程（Req 3）

- アダプタは `audit.Service.Record` の error を**そのまま return**し、境界で握りつぶさない
  （Req 3.1 / 3.2）。
- `internal/tenant/service.go:431-452` の `record()` helper（永続化失敗を観測ログのみ残し
  ユースケース本体へ伝播しない契約）は**変更していない**（Req 3.3 / NFR 1.1 / Out of Scope）。
  伝播後の error をどう扱うかは tenant Service の既存契約のまま。
- 永続化失敗時の `failure_kind=persist_error` 構造化ログ（Req 3.4）は既存 audit Service 側
  （`service.go:warnPersistFailure`）が担う。本アダプタは追加判断を持たない（Req 5.2）。

## AC Traceability（要件 → 実装 / テスト）

| AC | 実装箇所 | テスト |
|---|---|---|
| Req 1.1〜1.4（配線・閲覧可能化） | `cmd/api/main.go`（auditSvc 注入） / `Recorder.Record` | `main` build + `TestRecorder_Record_OperationMapping` |
| Req 2.1（actor 写像） | `recorder.go` `Record` ActorID | `TestRecorder_Record_OperationMapping`（ActorID 検証） |
| Req 2.2（tenant 写像） | `recorder.go` `Record` TenantID | 同上（TenantID 検証） |
| Req 2.3（結果区分写像） | `recorder.go` `mapResult` | `TestRecorder_Record_OperationMapping` / `..._ResultSuccessMapping` |
| Req 2.4〜2.6（種別写像） | `recorder.go` `mapEventType` | `TestRecorder_Record_OperationMapping` |
| Req 2.7（未知種別） | `recorder.go` `mapEventType` default | `TestRecorder_Record_UnknownOperation_MapsToIdentifiableType` |
| Req 2.8（ResourceID） | `recorder.go` `Record` ResourceID | `TestRecorder_Record_OperationMapping`（ResourceID 検証） |
| Req 2.9（ID/OccurredAt 委譲） | `recorder.go` `Record`（zero 据置） | `TestRecorder_Record_DelegatesIDAndOccurredAt` |
| Req 3.1 / 3.2（error 伝播） | `recorder.go` `Record` return | `TestRecorder_Record_PropagatesPersistError` |
| Req 3.3（helper 契約不変） | `service.go:431-452` 未変更 | 既存 `internal/tenant` テスト pass |
| Req 3.4（persist_error ログ） | 既存 audit `warnPersistFailure` | 既存 `internal/audit` テスト pass |
| Req 4.1 / 4.3（機密非漏洩） | `recorder.go` `buildDetail` | `TestRecorder_Record_DetailHasNoSensitiveKeys` |
| Req 4.2（非機密フィールド写像） | `recorder.go` `buildDetail` | `TestRecorder_Record_DetailCarriesNonSensitiveFields` |
| Req 4.4（非機密値なし時 空） | `recorder.go` `buildDetail`（deny_reason 省略） | `TestRecorder_Record_DenyReasonEmpty_OmitsDenyReasonKey` |
| Req 5.1（ポート契約充足） | `Recorder` が `tenant.EventRecorder` 実装 | `var _ tenant.EventRecorder` 静的アサーション |
| Req 5.2（責務限定） | `recorder.go`（変換＋委譲のみ） | コードレビュー / 再試行・抑制ロジック不在 |
| Req 5.3（配線注入） | `cmd/api/main.go` | `cmd/api` build pass |
| NFR 1.1（ユースケース本体不変） | `service.go` 未変更 | 既存 `internal/tenant` テスト pass |
| NFR 1.2（種別固定値） | `recorder.go` EventType 定数 | `TestEventTypeConstants_FixedStringValues` |
| NFR 2.1（観測性） | 永続化成功時は `recorder.go` `logPersisted`（結果は永続化値 `mapResult` に揃える）/ 失敗時は既存 audit Service `warnPersistFailure` | `TestRecorder_Record_LogsOutcomeOnPersistSuccess`（unknown/zero→failure/Warn 含む） / 既存 `internal/audit` テスト pass |

## 検証結果

| コマンド | 結果 |
|---|---|
| `go build ./...` | PASS（exit 0） |
| `go test ./...` | PASS（全パッケージ ok。tenantaudit 含む全層 green） |
| `gofmt -l`（新規/変更ファイル） | clean（`recorder.go` / `recorder_test.go` / `main.go` 差分なし） |
| `go vet ./...` | PASS（警告なし） |

注: `gofmt -l .` はリポジトリ全体で複数の**既存**ファイル（`internal/audit/handler_test.go`
等）を列挙するが、いずれも本 Issue で触れていない凍結済みコードであり、本実装の変更対象外。
本 Issue で新規追加/変更した 3 ファイルは gofmt clean。

Red→Green 確認: `mapEventType` の default 分岐を一時的に壊すと
`TestRecorder_Record_UnknownOperation_MapsToIdentifiableType` が fail することを観測してから
復旧した（テストが観点を実際に守ることを確認済み）。

## 確認事項（後段 Reviewer / 人間への申し送り）

1. **論点 A(b) の別 Issue 化提案**: requirements が確定したとおり、本実装は fail-closed の射程を
   アダプタ境界での error 伝播（選択肢 a）に限定した。tenant ユースケース本体の HTTP 応答を
   監査永続化失敗時に失敗へ転じさせる（選択肢 b）は、`tenant/service.go:431-452` の `record()`
   helper（error 握りつぶし契約）変更を伴うため別 Issue が必要。経営/コンプラ判断が入る場合は
   切り出しを推奨。
2. **interim LoggerRecorder の撤去判断**: `internal/tenant/audit_log.go`（および
   `audit_log_test.go`）は Out of Scope のため削除せず配線から外すのみとした。現状
   `tenant.NewLoggerRecorder` は本番経路から参照されなくなった（テストのみ参照）。撤去可否は
   別途判断が必要（撤去する場合は `tenant` パッケージから当該ファイルとテストを除去）。
3. **未知 Operation の写像値 `tenant_unknown` の妥当性**: Req 2.7 は「識別可能な種別へ写像」と
   のみ規定し正規語彙を確定していない。元の Operation 文字列を含める案（例
   `tenant_unknown:<op>`）も検討したが、EventType を監査閲覧の固定絞り込み値とする NFR 1.2 と
   整合させるため、固定値 `tenant_unknown` を採用した。元文字列を保持したい運用要件があれば
   Detail へ載せる拡張を別途検討可能（現状は載せていない）。
4. **Detail の常時 confirmation_completed 付与**: 現実装では `buildDetail` が常に
   `confirmation_completed` を載せるため Detail は実質常に非 nil（create/bind でも
   `confirmation_completed=false` が載る）。Req 4.4 の「載せ得る非機密値が無い場合は空」は
   将来 tenant.Event の構造変更で confirmation が外れた場合に備えた防御として実装したが、
   現状の tenant.Event 構造では発火しない。confirmation を無効化操作以外で省略すべきかは
   tenant ドメイン側の監査詳細仕様の論点であり、本 Issue のスコープ外（既存 LoggerRecorder も
   常時出力していたため互換維持）。

## Iteration ラウンド 1（PR #56 レビュー対応）

レビュー（codex）4 件のうち裁定で legitimate とされた 3 件に対応した（残り 1 件は excessive 裁定）。

- **[low] `mapResult` の境界防御（対応済み / 修正 commit）**: `mapResult` を fail-closed 化した。
  従来は `tenant.ResultFailure` 以外をすべて `ResultSuccess` に倒していたが、`tenant.Result` は
  string 型で zero 値 / 未定義値を表現可能なため、明示的な `tenant.ResultSuccess` のみを成功と
  写像し、それ以外（failure / zero / 未知値）はすべて `audit.ResultFailure` へ倒す allow-list 方式に
  変更（未知値を成功と誤認しない安全側）。`TestRecorder_Record_UnknownResult_MapsToFailure` を追加。
  既存 2 値（success/failure）の写像挙動は不変。
- **[medium] 実 audit.Service 経由の結合寄りテスト（対応済み / 修正 commit）**: spy だけでは
  カバーできなかった「実 `audit.NewService` 経由の写像 → 採番 / clock 補完 / 永続化失敗伝播」を
  `recorder_integration_test.go` で追加検証（DB 境界 `audit.Repository` のみ fake 化）。
  実 PostgreSQL を起動する結合テスト基盤が本リポジトリに存在しない（DB 系テストは `fakeTx` mock、
  `audit/repository_test.go` は純粋関数のみ）ため、`audit_logs` の FK / RLS / jsonb 永続化そのものを
  通す結合テストは本 PR スコープ（利用・配線のみ）外であり別途基盤導入を要する。
- **[high] 存在しない tenant_id の失敗監査が FK で永続化されない件（要件/設計ギャップとして申し送り）**:
  下記「確認事項 5」を参照。本 PR スコープ内では修正できないため commit せず、別 Issue 化を提案する。
- **[medium] design.md / tasks.md 不在（裁定 excessive / 対応なし）**: 自動裁定で excessive と
  判定された（設計書不在はプロセス観察であり AC 違反やコード欠陥ではない / Architect は条件付き起動で
  interim 実装の設計書不在は許容範囲）。加えて本モードは impl PR iteration であり spec 文書の新規
  作成・書き換えは禁止のため対応しない。

確認事項に以下を追記:

5. **[要件/設計ギャップ] `audit_logs.tenant_id` の FK と「存在しないテナントへの失敗監査」の衝突**:
   `0009_create_audit_logs.up.sql` の `tenant_id uuid REFERENCES tenants(id)` により、`tenants` に
   存在しない非 nil の tenant_id を持つ行は INSERT が FK で拒否される。tenant Service の失敗経路の
   うち、bind / disable の lookup failure（`service.go:217-221` / `295-299`）や bind の入力検証失敗
   （`service.go:207-213`）は、URL path 由来の**存在しない tenant id** を載せた失敗 Event を記録要求
   するため、`audit_logs` への永続化が FK で失敗する（→ `audit.Service` が error を返し、アダプタは
   伝播するが、`record()` helper が握りつぶすため当該失敗監査は `/api/admin/audit-logs` に残らない）。
   - 注: 作成失敗（name 空 / `service.go:147-148`）は `tenant_id=uuid.Nil` で記録され、audit Repository が
     `uuid.Nil → NULL` 写像するため FK 違反にならない（NULL は許容）。問題は **非 nil かつ非実在**の id。
   - 本 PR スコープ内で修正不能な理由: (a) スキーマ（FK）変更は本 Issue の Out of Scope（`audit_logs`
     のスキーマは #5 / #33 / #37 の領分。本統合は利用・配線のみ）。(b) Req 2.2 はアダプタに
     tenant_id 写像を義務付けており、失敗時に一律 `tenant_id=NULL` へ倒すと Req 2.2 と矛盾する。
     (c) アダプタは「変換 + 委譲」のみに責務を限定され（Req 5.2）、テナント存在確認のための DB
     アクセスや retry/fallback 判断を持てない（DB ハンドルも持たない）。
   - 提案（別 Issue 化）: いずれも要件/設計判断を伴うため人間レビューに委ねる。
     (i) `audit_logs.tenant_id` の FK を緩和し「存在しないテナントへの操作試行」も監査可能にする、
     または (ii) 失敗監査では `tenant_id=NULL` + `resource_id=<試行 id>`（resource_id は FK なしの
     text 列で既に試行 id を保持済み）で記録する規則を要件として確定する。どちらも本アダプタ単独では
     決められないため、Req 1.2 / 2.2 と `audit_logs` スキーマ責務をまたぐ別 Issue を推奨。

ITERATION-1 STATUS: complete

## Iteration ラウンド 3（PR #56 レビュー対応）

レビュー（codex）6 件すべてが裁定で legitimate とされた。対応内訳:

- **[medium] `logPersisted` の結果区分が audit row と乖離（対応済み / 修正 commit）**: `logPersisted` が raw な
  `e.Result` を載せ、`e.Result == tenant.ResultFailure` のみ Warn 判定していたため、unknown / zero 値が
  `mapResult` の fail-closed により audit row 上は failure なのにログ上は Info / `result=""` となり観測ログと
  監査証跡が乖離していた。`logPersisted` を **永続化値（`mapResult` 後）に揃える**よう修正し、result 値と
  Warn / Info の level 判定の双方を audit row と一致させた（NFR 2.1 / fail-closed の観測性）。
  `TestRecorder_Record_LogsOutcomeOnPersistSuccess` に zero 値 / 未知値ケース（→ failure / Warn）を追加し
  Red→Green を確認。既存の success / failure ケースの挙動は不変。
- **[high] 存在しない tenant_id の失敗監査が FK で永続化されない件のテスト固定（テスト再焦点化）**: 根本対処は
  本 Issue スコープ外（下記「確認事項 5」のとおり (a) `audit_logs` FK スキーマ=#5 領分 / (b) lookup failure 時の
  tenant id 写像=#38 領分 / (c) Req 2.2・Req 5.2 によりアダプタ単独では不可）で round-1 から不変。round-2 指摘は
  結合テストが `List(tenant_bind) want 0` を断言し「失敗監査が残らない」状態を**望ましい挙動として固定**していた点。
  当該テストを `TestTenantAuditWiring_PersistErrorIsPropagated` に改名し、**fail-closed の error 伝播（Req 3.1 / 3.2）を
  実 DB の FK 違反経路で検証する**目的へ再焦点化。gap を「期待結果」として固定する `List want 0` 断言を撤去した
  （AC に紐付かない断言の除去であり、テストを弱めて pass させる行為ではない）。gap 自体は確認事項 5 で別 Issue 化を継続提案。
- **[medium] design.md / tasks.md 不在（対応なし / 返信で boundary 説明）**: 本 Issue は Triage で `needs_architect:false`
  と判定された単一実装パスのため Architect 段（設計 PR ゲート）を経ておらず design.md / tasks.md が存在しない。これらは
  Architect が**別の設計 PR**（人間レビュー済み）で作成する成果物であり、Developer が impl PR で新規作成・書き換えする
  ことは agent 連携ルール・本 iteration モードの双方で禁止。requirements ⇄ 実装 / テストの追跡は本ファイル
  「AC Traceability」表で提供済み。design/tasks が必要なら再 triage（`needs_architect:true`）または別 Issue を推奨。
- **[low] impl-notes / review-notes の `NewRecorder` シグネチャ齟齬（対応済み / docs 修正）**: impl-notes.md:12 / 38 と
  review-notes.md:18 の `NewRecorder(audit.Service)` / `NewRecorder(auditSvc)` を、実コード `NewRecorder(auditSvc, log)`
  （`logger.Logger` 第 2 引数）に合わせて修正した。

ITERATION-3 STATUS: complete

## Iteration ラウンド 4（PR #56 レビュー対応）

レビュー（codex）4 件のうち裁定で legitimate 3 件 / excessive 1 件。対応内訳:

- **[medium] 失敗監査の閲覧可能性が未実証（対応済み / 修正 commit）**: round-4 指摘の核心は、既存の
  失敗経路テスト `TestTenantAuditWiring_PersistErrorIsPropagated` が**存在しない** tenant_id で FK 違反を
  誘発し error 伝播のみを確認するため、「失敗監査が最終的に `/api/admin/audit-logs` で閲覧可能か」を
  実証できていなかった点。tenant Service の失敗経路の大半は **実在テナントへの拒否**（lookup 成功後の
  `already_bound` / `state_invalid` / `bind_conflict` 等。`service.go:230-273`）であり、これらは FK を満たし
  永続化・閲覧できる。`TestTenantAuditWiring_FailureEventForExistingTenantPersistsAndIsListable` を新規追加し、
  実在テナントへの失敗（拒否）監査が `Result=failure` のまま永続化 → List で閲覧可能になることを実 DB
  （`DATABASE_URL` 設定時）で固定した（Req 1.2 / 1.3 / 2.3 / 4.2 を結合経路で補完）。これにより、FK で
  残らないのは **非実在 tenant_id を載せた失敗監査に限る**ことが切り分けられる（下記 high 項参照）。
- **[high] 存在しない tenant_id の失敗監査が FK で永続化されない件（要件/設計ギャップとして申し送り継続）**:
  round-1 / round-3 から不変で本 PR スコープ内では修正不能（「確認事項 5」のとおり (a) `audit_logs` FK
  スキーマ=#5 領分 / (b) lookup failure 時の tenant id 写像=#38 領分 / (c) Req 2.2・Req 5.2 によりアダプタ
  単独では不可）。round-4 の medium 対応で「閲覧可能な失敗監査」が実証されたことにより、本 high が指す
  gap は **非実在 tenant への操作試行に限定される**ことが明確化された。別 Issue 化提案を継続。
- **[medium] design.md / tasks.md 不在（裁定 excessive / 対応なし・返信で boundary 説明）**: 本 Issue は
  Triage で `needs_architect:false` の単一実装パスのため Architect 段（設計 PR ゲート）を経ておらず
  design.md / tasks.md が存在しない。これらは Architect が別の設計 PR（人間レビュー済み）で作成する成果物で
  あり、Developer が impl PR で新規作成することは agent 連携ルール・本 iteration モードの双方で禁止
  （1 PR = design or impl）。requirements ⇄ 実装 / テストの追跡は本ファイル「AC Traceability」表で提供済み。
- **[low] review-notes.md の HEAD commit / Reviewed Scope が陳腐化（対応なし・返信で説明）**: review-notes.md は
  Reviewer エージェントの round-1 スナップショット（`round=1` / HEAD `2042ca4` 明記）であり、その後の
  iteration 修正 commit（5a4868f / 057fb63 / 5601312 / 本 round）はすべて当該レビュー後に積まれたため陳腐化は
  iteration フロー上不可避。Reviewer 成果物の rewrite は Developer の責務外（point-in-time のレビュー記録を
  事後改変するのは不適切）。最新状態の追跡は本 impl-notes の AC Traceability 表が担い、再レビューが要れば
  Reviewer の round-2 起動が正道。

ITERATION-4 STATUS: complete

STATUS: complete
