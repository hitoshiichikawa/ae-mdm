-- 0008_create_tenant_apps.up.sql
-- Req 6.1, 6.2
-- テナントごとに承認されたアプリのカタログ snapshot。
-- UNIQUE (tenant_id, package_name) で同一テナント内の重複を防ぐ。

CREATE TABLE IF NOT EXISTS tenant_apps (
    id            uuid PRIMARY KEY,
    tenant_id     uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    package_name  text NOT NULL,
    title         text NOT NULL,
    icon_url      text,
    approved_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, package_name)
);

CREATE INDEX IF NOT EXISTS idx_tenant_apps_tenant_id ON tenant_apps(tenant_id);
