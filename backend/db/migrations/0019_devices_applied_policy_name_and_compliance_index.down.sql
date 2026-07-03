-- 0019_devices_applied_policy_name_and_compliance_index.down.sql
-- 0019_devices_applied_policy_name_and_compliance_index.up.sql の対称巻き戻し（up の逆順）。
--   1. 複合 index idx_devices_tenant_compliance を DROP INDEX IF EXISTS で巻き戻す。
--   2. applied_policy_name 列を DROP COLUMN IF EXISTS で巻き戻す。
-- いずれも 0006（devices 本体）へ影響を与えず、再 up で完全に復元できる（reversible）。

DROP INDEX IF EXISTS idx_devices_tenant_compliance;

ALTER TABLE devices DROP COLUMN IF EXISTS applied_policy_name;
