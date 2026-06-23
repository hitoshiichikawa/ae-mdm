-- 0012_audit_log_immutability.down.sql
-- 0012_audit_log_immutability.up.sql の効果を逆順で巻き戻す。

-- REVOKE の打ち消し（app_user が存在する場合のみ GRANT）
DO $$ BEGIN
    EXECUTE 'GRANT UPDATE, DELETE ON audit_logs TO app_user';
EXCEPTION
    WHEN undefined_object THEN
        RAISE NOTICE 'app_user role does not exist; skipping GRANT';
END $$;

DROP POLICY IF EXISTS audit_logs_insert ON audit_logs;
DROP POLICY IF EXISTS audit_logs_select ON audit_logs;

ALTER TABLE audit_logs NO FORCE ROW LEVEL SECURITY;
ALTER TABLE audit_logs DISABLE ROW LEVEL SECURITY;
