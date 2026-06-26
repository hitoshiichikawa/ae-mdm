-- 0013_extend_sessions.down.sql
-- 0013_extend_sessions.up.sql の対称巻き戻し。
--   1. console カラムを DROP（CHECK 制約も一緒に消える）
--   2. revoked_at カラムを DROP
--   3. last_seen_at → idle_at の逆 rename（ADD → backfill → DROP の対称手順）

-- 3) console DROP
ALTER TABLE sessions DROP COLUMN console;

-- 2) revoked_at DROP
ALTER TABLE sessions DROP COLUMN revoked_at;

-- 1) last_seen_at → idle_at へ巻き戻す（ADD → UPDATE → DROP）
--    up と対称の手順で値を保全する。
ALTER TABLE sessions ADD COLUMN idle_at timestamptz NOT NULL DEFAULT now();
UPDATE sessions SET idle_at = last_seen_at;
ALTER TABLE sessions DROP COLUMN last_seen_at;
ALTER TABLE sessions ALTER COLUMN idle_at DROP DEFAULT;
