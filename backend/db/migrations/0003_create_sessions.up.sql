-- 0003_create_sessions.up.sql
-- Req 6.1, 6.2
-- サーバ側セッション。token_hash を PK とし、admin_user_id 経由で tenant 分離する
-- （tenant_id カラムを持たない設計 / RLS は 0011 の subselect ポリシーで担保）。

CREATE TABLE IF NOT EXISTS sessions (
    token_hash    text PRIMARY KEY,
    admin_user_id uuid NOT NULL REFERENCES admin_users(id) ON DELETE CASCADE,
    issued_at     timestamptz NOT NULL DEFAULT now(),
    idle_at       timestamptz NOT NULL,
    expires_at    timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sessions_admin_user_id ON sessions(admin_user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expires_at ON sessions(expires_at);
