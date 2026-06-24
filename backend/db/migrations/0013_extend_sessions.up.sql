-- 0013_extend_sessions.up.sql
-- Issue #33 (A3a: OIDC Verifier + Session 管理) Req 3.7, 3.9, 4.1, 4.2, 4.3, 4.8, 5.1, 6.3
--
-- sessions テーブルに以下の拡張を加える:
--   1. idle_at カラムを last_seen_at に rename（ADD → backfill → DROP の対称手順）
--      Req 4.3 / 4.8: idle timeout 判定の正確化のため命名を「最終アクセス時刻」に統一
--   2. revoked_at timestamptz NULL を追加
--      Req 5.1: 明示的失効（logout / 管理失効）を物理的に表現するための tombstone 列
--   3. console text NOT NULL CHECK (console IN ('tenant-console','admin-console')) を追加
--      Req 6.3: tenant / admin の 2 console を物理分離するための識別列
--      既存行は 'tenant-console' で backfill（A2 時点では tenant 用 session のみ存在の想定）
--      backfill 後に DROP DEFAULT して NOT NULL のまま明示指定を強制する
--
-- 既存 RLS ポリシー（tenant_isolation_sessions / 0011_enable_rls.up.sql）は本マイグレーションで
-- 再定義しない（列追加・rename のみ / subselect ポリシーは sessions ↔ admin_users.tenant_id 経由で
-- そのまま機能する）。

-- 1) idle_at → last_seen_at の rename（ADD → UPDATE → DROP）
--    既存値の保全のため DEFAULT now() で ADD した後に UPDATE で idle_at から復写する。
ALTER TABLE sessions ADD COLUMN last_seen_at timestamptz NOT NULL DEFAULT now();
UPDATE sessions SET last_seen_at = idle_at;
ALTER TABLE sessions DROP COLUMN idle_at;

-- 2) revoked_at（NULL 許容 / 明示失効時のみセット）
ALTER TABLE sessions ADD COLUMN revoked_at timestamptz NULL;

-- 3) console（NOT NULL + CHECK / 既存行は 'tenant-console' で backfill 後 DROP DEFAULT）
ALTER TABLE sessions ADD COLUMN console text NOT NULL DEFAULT 'tenant-console'
    CHECK (console IN ('tenant-console', 'admin-console'));
ALTER TABLE sessions ALTER COLUMN console DROP DEFAULT;
