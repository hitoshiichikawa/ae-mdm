# 実装ノート（Issue #9 [C1] デバイス）

## Implementation Notes

### Task 1

- **採用方針**: schema-only migration を追加。`devices` に STATUS_REPORT 報告値 `applied_policy_name text`（nullable）列と、コンプライアンス分類フィルタ一覧の性能担保用に複合 index `idx_devices_tenant_compliance ON devices(tenant_id, compliance_status)` を追加した。
- **重要な判断**:
  - tasks.md / design.md は migration 番号を `0018` と記載するが、実ツリーの `0018` は既に Issue #52（`0018_tenant_binding_state_and_signup_url`）が使用済みのため **番号のみ 0019 に採番**した（context-map.md NOTE の「実コードベースと矛盾する場合の記録」方針、design.md Risks「migration 番号衝突」に従う）。ファイル名サフィックス `devices_applied_policy_name_and_compliance_index` は tasks.md 記載どおり維持。
  - `applied_policy_name` は STATUS_REPORT が報告する適用中ポリシー名の**報告値**であり、既存 `applied_policy_id`（割当 intent の複合 FK / #40）とは**別概念**。FK / 名前解決に依存せず faithful に保持するため nullable text 列とした。
  - 既存 RLS `tenant_isolation_devices`（0011）は列追加で継承されるため新規 RLS policy は不要。既存 `0006`（devices 本体）は非破壊、`IF NOT EXISTS`/`IF EXISTS` で冪等化。down は up の逆順（index DROP → column DROP）で reversible。
  - 本 task の `_Requirements_partial:_` に全 AC（7.1 / 2.1 / NFR 1.1）が列挙されており、振る舞い・性能テストは後続 task（7.1→task 6 結合テスト / 2.1→task 4・7 / NFR 1.1→task 8.1 の EXPLAIN）へ deferred のため、**本 task 内の Go テスト追加は不要**。
- **残存課題**: 後続 task（4 / 6 / 7 / 8.1 等）が参照する migration ファイル名は `0019_...`（tasks.md 記載の `0018` ではない）。後続 merge 順次第で再々採番があり得る。

### Task 2

- **採用方針**: 型定義・骨格 task。`internal/device/` を新規追加し、DTO / enum / Clock / sentinel error を design.md「Data Models > DTO 形状」と Components の各 interface シグネチャどおりに定義。Repository / Service / StatusApplier / Handler の interface 定義・実装は後続 task（3〜8）へ委譲。
- **重要な判断**:
  - `DeviceRow` は migration 0006 + 0019（`applied_policy_name`）の `devices` 列に対応させ、nullable 列（`AppliedPolicyID` / `AppliedPolicyName` / `LastStatusAt`）は pointer、jsonb 列は `json.RawMessage` で scan する前提（policy.PolicyRow 手本）。DB 層型（`DeviceRow` / `TenantComplianceCount` / `StatusApplyInput`）には JSON tag を付与せず、API 応答型（`DeviceSummary` / `DeviceDetail` / `TenantOverview`）にのみ snake_case JSON tag を付与（policy.PolicyView 慣習）。
  - read/write を別型に分離し、`Service`（read）に write メソッドを持たせない設計を doc.go に明記して Req 7.3 を型担保。write 経路は StatusApplier のみ。依存方向は `device → notification`（逆方向 import なし）。
  - enum 値（`ComplianceStatus` 4 値 / `DeviceMode` 2 値）は migration 0006 の DB ENUM 文字列と厳密一致。`unsupported` は read の第 4 分類として定義するが StatusApplier は書き込まない（Open Q）。
  - `TenantComplianceCount` は `AggregateOverview` の flat 集計 1 行（GROUP BY tenant_id, compliance_status）、`TenantOverview` は tenant 単位に畳み込んだ 4 分類 0 埋め応答型として別型化。
- **残存課題**: 後続 task 3 以降で Repository / Service / StatusApplier / Handler の interface と実装を追加する際、本 types.go の DTO 型を参照する。`ListFilter` のページング既定（page=1 / page_size=50 / 上限 200 clamp）は Handler（task 7）が補完する契約であり、型側では既定値を持たせていない（zero-value は Handler が解釈）。

### Task 3

- **採用方針**: `repository.go` を `policy.Repository` と同構造で実装（`rowScanner` / `scanDeviceRow` / `mapGetError` / `wrapSentinel` / `BeginTxFunc` 昇格なし / `RowsAffected()` 返却）。tenant-scoped は ambient TenantContext のまま RLS に分離を委ね、`AggregateOverview` のみ SuperAdmin ctx を信頼。
- **重要な判断**:
  - jsonb ↔ `json.RawMessage` は本 codebase に前例が無いため、`notification.scanUnassigned` の「jsonb→`[]byte` 中間変数→変換」手本に倣い、**scan は `[]byte` 経由**（`json.RawMessage(bytes)`）、**bind は `[]byte`（nil→NULL）+ `::jsonb` cast** とした。pgx v5.6.0 の named-slice-type scan / enum-OID encode の不確実性を回避し runtime で確実に動く形にした（notification の `[]byte`→jsonb bind 前例で裏付け）。enum は text 生ラベルを `string` 経由 scan、bind は `*string`（nil→NULL）+ `::compliance_status`/`::device_mode` cast（index を潰さないよう列は bare 保持）。
  - `ListByTenant` は動的 WHERE をプレースホルダ番号を動的採番して組む。sync フィルタは Service.isSyncDelayed と同意味論（strict `<` / NULL は非遅延）。ページング安定化のため `ORDER BY enrolled_at ASC, id ASC` を必須付与。既定値補完は Handler 責務のため受領値をそのまま使用。
  - `UpdateFromStatusReport` は design 記載どおり `WHERE amapi_device_name=$1`（(tenant_id, amapi_device_name) UNIQUE + RLS で高々 1 行）。全カラム `COALESCE($n, col)` で欠落=既存値保持。affected=0（未登録端末）は error に倒さず `(0, nil)`。
- **残存課題**: enum/jsonb の実 DB bind/scan 検証は `test/integration/device_repository_test.go`（+ `seedDevice` の enum/jsonb cast INSERT）が担保するが、当環境は `DATABASE_URL` 未設定で **t.Skip**（compile のみ確認）。CI / DB 有り環境で実行し bind/scan の runtime 妥当性を最終確認されたい。NFR 1.1（index 妥当性 EXPLAIN）は task 8.1 の deferred。

### Task 4

- **採用方針**: `service.go` に read 系 `Service`（`List`/`Get`/`Overview` の 3 メソッド）を実装。DB 非依存の fake `Repository`/`Clock` で列挙全 AC を網羅する単体テストを近傍（`service_test.go`）に追加した。
- **重要な判断**:
  - コンストラクタは `NewService(repo, clock, syncDelayThresholdHours int)` とし、config 型全体ではなく閾値 int を注入（Service を config から独立させ fake clock + 任意閾値で境界テストしやすくする / Req 4.2）。閾値は `time.Duration(hours)*time.Hour` へ換算し、`syncCutoff = Clock.Now() - 閾値` を List/Get で 1 回算出して filter・per-row `SyncDelayed` に共用（Repository.ListByTenant と同一 cutoff を渡す）。
  - `isSyncDelayed` は `lastStatusAt != nil && lastStatusAt.Before(cutoff)`（strict `<` / 閾値ちょうど・NULL は非遅延 / design.md 設計判断・Req 4.1・4.3）。Repository.ListByTenant の SQL 意味論（`last_status_at < cutoff` / NULL 非遅延）と一致。
  - jsonb 空属性は `rawOrDefault`（`len==0` 判定）で HW/SW→`{}`、非準拠理由/installed_apps→`[]` を補填し null を返さない（Req 2.5）。`applied_policy_name` NULL→`""`。`compliance_status` は stored 値をそのまま返却（再判定しない / Req 3.1・3.3）。`Get` は Repository が写像済みの `ErrDeviceNotFound` をそのまま伝達。
  - `Overview` は flat `[]TenantComplianceCount` を slice + index map で畳み込み、tenant 出現順（Repository の tenant_id 順）を保ちつつ `Breakdown` を 4 分類 0 埋め初期化してから加算。`DeviceCount` は当該 tenant の count 合計。全体 0 件は非 nil 空 slice（Req 6.1・6.4）。`Service` interface に write メソッドを持たせず Req 7.3 を型担保。
- **残存課題**: なし（Handler/AdminHandler は task 7/8、`Service.Overview` の tenant_id 絞り込み Req 6.3 は Repository/AdminHandler 側で担保）。

### Task 5

- **採用方針**: `notification/status_handler.go` に STATUS_REPORT 本体処理を実装。`DeviceStatusWriter` port・`DeviceStatusReport` 値オブジェクト（任意フィールドを pointer / `*json.RawMessage` で保持）・`StatusHandler`（`NotificationHandler` 実装）・nil-safe な `NewStatusHandler` を追加し、DB 非依存の spy `DeviceStatusWriter` で全 AC を網羅する単体テストを近傍（`status_handler_test.go`）に追加した。
- **重要な判断**:
  - design.md L386 は値オブジェクトを `StatusReport` と表記するが、同 package `notification` には既に STATUS_REPORT 種別を表す `NotificationType` 定数 `StatusReport`（`types.go` L22 / Issue #39）が存在し **型名が衝突**する（build error `StatusReport redeclared`）。spec 本文は書き換えず、型名のみ `DeviceStatusReport` へ改名して衝突回避した（method 名 `ApplyStatusReport` は別名前空間で非衝突のため維持）。後続 task 6 の StatusApplier は `notification.DeviceStatusReport` を実装対象とする（下記「確認事項」参照）。
  - `Handle` は verifier.go と同型の防御的破棄 ack（空 payload → malformed JSON → `DeviceName` 空 の順で `discardError(...)` を return）。`discardError` / `assertDiscardError` は同 package に既存のため再利用（再定義せず）。`DeviceName` は verifier の接頭辞切り出しをせず AMAPI `name`（フル resource 名 = `devices.amapi_device_name`）をそのまま保持。空判定のみ `strings.TrimSpace` で行い値は非改変。
  - `ApplyStatusReport` の戻り error は transient/permanent を再分類せず透過（ack/nack 判定は Dispatcher の責務 / design.md Postconditions）。欠落フィールドは pointer nil のまま writer へ渡し「更新しない」を表現（Req 7.2）。機密値（payload 生値）は error 文言・ログに補間せず message_id 中心のログに限定（NFR 3.1）。
- **残存課題**: 次 task 6（device.StatusApplier）は `notification.DeviceStatusReport`（改名後の型名）を import して `DeviceStatusWriter` を実装し、`DeviceStatusReport` → `StatusApplyInput` へ写像する。`NonComplianceDetails` の nil（欠落）/ 空 `[]` / 非空 の 3 分岐で compliance 再判定する契約は task 6 側で担保する（本 task は wire-format パースと port dispatch のみ）。

## 確認事項

- **実 DB repository テストの配置（task 文面との差異）**: task 3 文面は実 DB テストを `repository_test.go` と記すが、本 codebase の実 DB harness（`requireDBURLs`/`seedDummyData`/`newAppPool`/migration）は `test/integration`（package `integration_test`）内にしか無く、design.md File Structure Plan も「実 DB 結合は `backend/test/integration/`」と規定する。したがって実 DB テストは `backend/test/integration/device_repository_test.go` に配置し、`internal/device/repository_test.go` は DB 非依存の near-neighbor 単体テスト（scan / error mapping / bind helper）に充てた。spec 本文は書き換えていない。
- **jsonb / enum の pgx bind/scan 実装判断**: jsonb は `json.RawMessage`↔`[]byte` 経由（`::jsonb` cast bind / `[]byte` scan）、enum は `string` 経由（`::compliance_status`/`::device_mode` cast bind / text scan）とした（前例 `notification.scanUnassigned` 準拠 / 詳細は上記 Task 3 learning）。`seedDevice` helper も同 cast で INSERT し、実 DB 実行時に SQL 契約を二重に検証する。

- **migration 採番の 0018→0019 変更（人間確認事項）**: tasks.md / design.md は本 task の migration を `0018` と記載しているが、実作業ツリーには既に `0018_tenant_binding_state_and_signup_url.{up,down}.sql`（Issue #52 の再採番済み migration）が存在し `0018` は使用できない。design.md Risks 節（「migration 番号衝突 … merge 順で再採番が必要になり得る点を PR で明示する」）に従い、番号のみ `0019` に採番して実装した。spec（tasks.md / design.md）本文は書き換えていない。develop への merge 順によっては別 in-flight branch と `0019` が再衝突し、再々採番が必要になり得る点を merge 時に確認されたい。

- **値オブジェクト名の design.md との差異（task 5 / 人間・Architect 確認事項）**: design.md L386 は STATUS_REPORT payload の値オブジェクトを `StatusReport` と表記するが、同 package `notification` には既に `NotificationType` 定数 `StatusReport`（`types.go` L22 / Issue #39 で merge 済み。`admin_handler.go` L161 が参照）が存在し、同名では build error（`StatusReport redeclared in this block`）になる。design.md を書いた時点で #39 の既存定数との衝突が見落とされていたと判断し、spec 本文（design.md / tasks.md）は書き換えず **型名のみ `DeviceStatusReport` へ改名**して衝突回避した。port method 名 `ApplyStatusReport` は別名前空間のため維持。後続 task 6 は `notification.DeviceStatusReport` を実装対象とする。design.md 側の型名表記を将来揃えるべきかは Architect 判断に委ねる。

- **AMAPI Device リソースの JSON tag（task 5 / #36 worker 本配線時に実 payload で要検証）**: `DeviceStatusReport` の各フィールドに AMAPI Device リソースの wire-format（camelCase）JSON tag を付与した: `name` / `lastStatusReportTime` / `appliedPolicyName` / `nonComplianceDetails` / `hardwareInfo` / `softwareInfo`。**`InstalledApps` は AMAPI Device リソースの `applicationReports`（インストール済みアプリを報告する array）を採用**した（design.md の論理名 `InstalledApps` と wire 名が異なる）。これらの wire-format 対応は本 codebase に一次情報が無く（WebSearch 不可の環境で確定）、特に `InstalledApps ↔ applicationReports` の対応は推測を含む。#36 の worker handlers map 本配線時に、実 AMAPI STATUS_REPORT payload に対してフィールド名（特に `applicationReports`）が正しく unmarshal されるかを結合テスト / 実データで最終検証されたい。tag が実 wire と不一致でも欠落＝pointer nil（更新しない / Req 7.2）に倒れるため既存値の破壊は起きないが、当該フィールドの更新が silent に落ちるリスクがある。

## 検証結果

- `cd backend && go build ./...`: 成功（exit 0）。Go コード変更は無く既存どおり通ることを確認。
- 実 DB 検証（up→down→up 冪等性 / `migrations_reversible_test.go`）: `DATABASE_URL` 未設定のため **skip**（既存 integration test の `t.Skip` 方針と同一）。SQL は既存 `0006` / `0018` migration と同型（`IF NOT EXISTS`/`IF EXISTS` 冪等）で目視確認済み。ファイル名は golang-migrate 規約（`<version>_<name>.{up,down}.sql`）に準拠。

STATUS: complete
