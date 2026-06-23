-- 0004_create_enrollment_tokens.up.sql
-- Req 6.1, 6.2
-- enrollmentTokens の snapshot。mode は fully_managed/dedicated。
-- policy_id は nullable（ポリシー未指定の token も許容）。

DO $$ BEGIN
    CREATE TYPE enrollment_mode AS ENUM ('fully_managed', 'dedicated');
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

CREATE TABLE IF NOT EXISTS enrollment_tokens (
    id                uuid PRIMARY KEY,
    tenant_id         uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    amapi_token_name  text NOT NULL,
    mode              enrollment_mode NOT NULL,
    policy_id         uuid,
    additional_data   jsonb NOT NULL DEFAULT '{}'::jsonb,
    expires_at        timestamptz NOT NULL,
    issued_by         uuid NOT NULL REFERENCES admin_users(id),
    created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_enrollment_tokens_tenant_id ON enrollment_tokens(tenant_id);
