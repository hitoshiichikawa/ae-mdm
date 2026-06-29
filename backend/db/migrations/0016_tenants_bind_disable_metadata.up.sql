-- 0016_tenants_bind_disable_metadata.up.sql
-- Issue #38 (A4b) Req 2.1 / NFR 1.1 / NFR 3.1
--
-- tenants テーブルに無効化監査列（disabled_at / disabled_by）を追加し、
-- enterprise_name の部分一意 index を作成する。0001（tenants 本体）/ 0011（RLS）は変更せず
-- ALTER のみで冪等に追加する（IF NOT EXISTS 活用 / NFR 3.1 既存スキーマ非破壊）。
--
-- 設計意図:
--   - disabled_at / disabled_by: Disable 操作（Req 3.1）で `now()` / 操作者 id を記録する
--     監査列。いずれも NULL 許容で、bound/pending_bind 行では NULL のまま。
--   - uq_tenants_enterprise_name: enterprise_name が非 NULL の行に限った部分一意 index。
--     同一 Enterprise への二重バインドを DB 層で防止し（Req 2.1 invariant 補強）、競合は
--     UpdateBound（task 3）で 23505 → CodeConflict に写像される前提。enterprise_name は
--     pending_bind 行で NULL のため、部分 index にして未バインド行同士の衝突を回避する。

ALTER TABLE tenants ADD COLUMN IF NOT EXISTS disabled_at timestamptz;
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS disabled_by uuid;

CREATE UNIQUE INDEX IF NOT EXISTS uq_tenants_enterprise_name
    ON tenants (enterprise_name)
    WHERE enterprise_name IS NOT NULL;
