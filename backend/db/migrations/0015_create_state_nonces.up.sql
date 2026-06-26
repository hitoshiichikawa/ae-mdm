-- 0015_create_state_nonces.up.sql
-- Issue #33 (A3a) Req 2.9
--
-- OAuth state の一度限り消費を物理的に保証する state_nonces テーブル。
-- callback handler が ConsumeStateNonce(nonce, console, expires_at) で INSERT し、PRIMARY KEY
-- (nonce) の UNIQUE 違反（pgerrcode 23505）を replay 検出に利用する（design.md / tasks.md
-- L126〜L147 と整合）。
--
-- GC（WHERE expires_at < now() での DELETE）は後続 sweeper task の責務で本 Issue 範囲外。
-- sweeper も同 SuperAdmin context で動作する前提。

CREATE TABLE state_nonces (
    nonce       text PRIMARY KEY,
    console     text NOT NULL CHECK (console IN ('tenant-console', 'admin-console')),
    consumed_at timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz NOT NULL
);

CREATE INDEX idx_state_nonces_expires_at ON state_nonces(expires_at);

-- defense in depth: app DB role からの直接 DML を物理的に拒否し、SuperAdmin context
-- 経由（callback handler / Repository.ConsumeStateNonce）でのみ INSERT を許可する。
-- tenant_id を持たない infra テーブルだが、A2 の既存方針（tenant_id を持たない infra
-- テーブルも SuperAdmin-only RLS に寄せる）に従う。万一 app role の SQL injection 等で
-- 当該テーブルに到達しても、`app.is_superadmin = 'true'` 設定が無い限り DML がすべて
-- 0 行 / reject に倒れる（replay 防止の二次防御）。
ALTER TABLE state_nonces ENABLE ROW LEVEL SECURITY;
CREATE POLICY state_nonces_superadmin_only ON state_nonces
    USING (current_setting('app.is_superadmin', true) = 'true')
    WITH CHECK (current_setting('app.is_superadmin', true) = 'true');
