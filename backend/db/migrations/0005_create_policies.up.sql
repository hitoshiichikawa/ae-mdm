-- 0005_create_policies.up.sql
-- Req 6.1, 6.2
-- ポリシー snapshot。AMAPI policyName と本体 JSON を保持。

CREATE TABLE IF NOT EXISTS policies (
    id                uuid PRIMARY KEY,
    tenant_id         uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name              text NOT NULL,
    amapi_policy_name text NOT NULL,
    body              jsonb NOT NULL DEFAULT '{}'::jsonb,
    version           integer NOT NULL DEFAULT 1,
    updated_by        uuid REFERENCES admin_users(id),
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_policies_tenant_id ON policies(tenant_id);
