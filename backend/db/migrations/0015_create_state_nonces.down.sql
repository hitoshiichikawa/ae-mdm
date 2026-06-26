-- 0015_create_state_nonces.down.sql
-- 0015_create_state_nonces.up.sql の対称巻き戻し。
-- DROP TABLE で INDEX も同時に消えるが、tasks.md L150〜L152 の明示手順に従い、policy →
-- RLS disable → index → table の順で明示的に巻き戻す。

DROP POLICY IF EXISTS state_nonces_superadmin_only ON state_nonces;
ALTER TABLE state_nonces DISABLE ROW LEVEL SECURITY;
DROP INDEX IF EXISTS idx_state_nonces_expires_at;
DROP TABLE IF EXISTS state_nonces;
