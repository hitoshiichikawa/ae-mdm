-- 0006_create_devices.up.sql
-- Req 6.1, 6.2
-- 端末本体。amapi_device_name は tenant 内で UNIQUE。

DO $$ BEGIN
    CREATE TYPE device_mode AS ENUM ('fully_managed', 'dedicated');
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

DO $$ BEGIN
    CREATE TYPE compliance_status AS ENUM ('compliant', 'non_compliant', 'unknown', 'unsupported');
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

CREATE TABLE IF NOT EXISTS devices (
    id                      uuid PRIMARY KEY,
    tenant_id               uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    amapi_device_name       text NOT NULL,
    mode                    device_mode NOT NULL,
    applied_policy_id       uuid REFERENCES policies(id),
    hardware_info           jsonb NOT NULL DEFAULT '{}'::jsonb,
    software_info           jsonb NOT NULL DEFAULT '{}'::jsonb,
    compliance_status       compliance_status NOT NULL DEFAULT 'unknown',
    non_compliance_details  jsonb NOT NULL DEFAULT '[]'::jsonb,
    installed_apps          jsonb NOT NULL DEFAULT '[]'::jsonb,
    last_status_at          timestamptz,
    enrolled_at             timestamptz NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, amapi_device_name)
);

CREATE INDEX IF NOT EXISTS idx_devices_tenant_id ON devices(tenant_id);
CREATE INDEX IF NOT EXISTS idx_devices_last_status_at ON devices(last_status_at);
