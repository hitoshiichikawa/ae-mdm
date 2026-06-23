-- 0009_create_audit_logs.up.sql
-- Req 6.1, 6.2
-- 監査ログ（append-only）。tenant_id は NULL 許容（SuperAdmin の cross-tenant 操作監査用）。
-- 本マイグレーションでは CREATE TABLE のみ行い、RLS + append-only 強制は 0012 で適用する。

DO $$ BEGIN
    CREATE TYPE audit_log_result AS ENUM ('success', 'failure');
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

CREATE TABLE IF NOT EXISTS audit_logs (
    id           uuid PRIMARY KEY,
    tenant_id    uuid REFERENCES tenants(id),
    actor_id     uuid REFERENCES admin_users(id),
    event_type   text NOT NULL,
    resource_id  text,
    detail       jsonb NOT NULL DEFAULT '{}'::jsonb,
    result       audit_log_result NOT NULL,
    occurred_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_audit_logs_tenant_id ON audit_logs(tenant_id);
CREATE INDEX IF NOT EXISTS idx_audit_logs_occurred_at ON audit_logs(occurred_at);
CREATE INDEX IF NOT EXISTS idx_audit_logs_event_type ON audit_logs(event_type);
