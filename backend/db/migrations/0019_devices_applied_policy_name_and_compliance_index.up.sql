-- 0019_devices_applied_policy_name_and_compliance_index.up.sql
-- Issue #9 (#24 umbrella Req 5) Req 7.1 / Req 2.1 / NFR 1.1
--
-- 端末（devices / 0006）へ STATUS_REPORT が報告する適用中ポリシー名を保持する
-- applied_policy_name 列を追加し、コンプライアンス分類フィルタ一覧の性能担保（NFR 1.1）の
-- ための複合 index (tenant_id, compliance_status) を追加する。0006（devices 本体）は変更せず
-- ALTER / CREATE INDEX のみで冪等に追加する（IF NOT EXISTS 活用 / 既存スキーマ非破壊）。
--
-- 設計意図:
--   - applied_policy_name 列（Req 2.1 の「適用中ポリシー」/ Req 7.1）: STATUS_REPORT が
--     報告する適用中ポリシー名を faithful に保持するための列。nullable（既存行・STATUS_REPORT
--     未受信端末の整合のため）。既存 applied_policy_id（割当 intent の複合 FK / #40）とは
--     別概念であり、報告値をそのまま保持し FK / 名前解決に依存しない（design.md Data Models）。
--   - idx_devices_tenant_compliance（NFR 1.1）: 1 テナント 5,000 端末規模でコンプライアンス
--     分類フィルタ付き一覧（WHERE tenant_id=$ AND compliance_status=$）の p95<1s を担保する
--     複合 index。同期遅延フィルタは既存 idx_devices_last_status_at（0006）を活用する。
--   - RLS は既存 tenant_isolation_devices（0011）を列追加で継承するため、新規 RLS policy は
--     不要（列追加は行アクセス制御の対象を変えない / design.md Physical Data Model）。

ALTER TABLE devices ADD COLUMN IF NOT EXISTS applied_policy_name text;

CREATE INDEX IF NOT EXISTS idx_devices_tenant_compliance ON devices(tenant_id, compliance_status);
