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

## 確認事項

- **migration 採番の 0018→0019 変更（人間確認事項）**: tasks.md / design.md は本 task の migration を `0018` と記載しているが、実作業ツリーには既に `0018_tenant_binding_state_and_signup_url.{up,down}.sql`（Issue #52 の再採番済み migration）が存在し `0018` は使用できない。design.md Risks 節（「migration 番号衝突 … merge 順で再採番が必要になり得る点を PR で明示する」）に従い、番号のみ `0019` に採番して実装した。spec（tasks.md / design.md）本文は書き換えていない。develop への merge 順によっては別 in-flight branch と `0019` が再衝突し、再々採番が必要になり得る点を merge 時に確認されたい。

## 検証結果

- `cd backend && go build ./...`: 成功（exit 0）。Go コード変更は無く既存どおり通ることを確認。
- 実 DB 検証（up→down→up 冪等性 / `migrations_reversible_test.go`）: `DATABASE_URL` 未設定のため **skip**（既存 integration test の `t.Skip` 方針と同一）。SQL は既存 `0006` / `0018` migration と同型（`IF NOT EXISTS`/`IF EXISTS` 冪等）で目視確認済み。ファイル名は golang-migrate 規約（`<version>_<name>.{up,down}.sql`）に準拠。

STATUS: complete
