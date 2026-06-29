-- 0016_tenants_bind_disable_metadata.down.sql
-- 0016_tenants_bind_disable_metadata.up.sql の対称巻き戻し。
--   1. enterprise_name 部分一意 index を DROP
--   2. disabled_by 列を DROP
--   3. disabled_at 列を DROP
-- DROP は index → 列の順、列は up の追加と逆順で揃える（IF EXISTS で冪等）。

DROP INDEX IF EXISTS uq_tenants_enterprise_name;

ALTER TABLE tenants DROP COLUMN IF EXISTS disabled_by;
ALTER TABLE tenants DROP COLUMN IF EXISTS disabled_at;
