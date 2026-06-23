-- 0001_create_tenants.up.sql
-- Req 6.1, 6.2 / NFR 2.1, NFR 2.2
-- umbrella docs/specs/24-android-enterprise-emm-mvp/design.md の Logical Data Model に従い
-- `tenants` テーブルと status enum を冪等に作成する。

-- status enum（pending_bind/bound/disabled）
DO $$ BEGIN
    CREATE TYPE tenant_status AS ENUM ('pending_bind', 'bound', 'disabled');
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

CREATE TABLE IF NOT EXISTS tenants (
    id              uuid PRIMARY KEY,
    name            text NOT NULL,
    status          tenant_status NOT NULL DEFAULT 'pending_bind',
    enterprise_name text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
