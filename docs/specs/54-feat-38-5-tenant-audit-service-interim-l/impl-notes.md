# Implementation Notes — #54 tenant 監査イベントの監査ログ Service 配線

## 概要

tenant ドメインの監査イベント（作成 / bind / 無効化）を、interim の構造化ログ記録器
（`tenant.LoggerRecorder`）ではなく既存の監査ログ Service（`internal/audit`）へ配線する
アダプタを新規実装し、`cmd/api/main.go` の DI を差し替えた。

## 実装した型・パッケージ

- 新規パッケージ `internal/tenantaudit`（domain-glue）
  - `recorder.go`: `Recorder` 型（`tenant.EventRecorder` を満たす）/ `NewRecorder(audit.Service)` /
    写像 helper（`mapEventType` / `mapResult` / `buildDetail`）/ EventType 定数
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

- `tenantRecorder := tenant.NewLoggerRecorder(log)` → `tenantRecorder := tenantaudit.NewRecorder(auditSvc)`
  （main.go:236 付近。`auditSvc` は同 main.go:211 で構築済みのものを再利用 / Req 5.3 / NFR 1）。
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
| NFR 2.1（観測性） | 既存 audit Service の構造化ログ | 既存 `internal/audit` テスト pass |

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

STATUS: complete
