-- 0007_create_device_commands.up.sql
-- Req 6.1, 6.2
-- 個別コマンドのライフサイクル。type=lock/wipe/reboot、status=issued/succeeded/failed/timed_out。
-- confirmation_used は WIPE の二段階確認フラグ。

DO $$ BEGIN
    CREATE TYPE device_command_type AS ENUM ('lock', 'wipe', 'reboot');
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

DO $$ BEGIN
    CREATE TYPE device_command_status AS ENUM ('issued', 'succeeded', 'failed', 'timed_out');
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

CREATE TABLE IF NOT EXISTS device_commands (
    id                 uuid PRIMARY KEY,
    tenant_id          uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    device_id          uuid NOT NULL,
    type               device_command_type NOT NULL,
    status             device_command_status NOT NULL DEFAULT 'issued',
    amapi_command_id   text,
    issued_by          uuid NOT NULL REFERENCES admin_users(id),
    issued_at          timestamptz NOT NULL DEFAULT now(),
    completed_at       timestamptz,
    result_detail      jsonb NOT NULL DEFAULT '{}'::jsonb,
    confirmation_used  boolean NOT NULL DEFAULT false,
    -- device_id は (id, tenant_id) を devices へ向ける複合 FK で「同一テナントの device
    -- しか参照できない」ことを DB レベルで強制する（PR #31 round-2 review 由来）。
    -- 単純な REFERENCES devices(id) では「他テナントの device 行に対する command 発行」を
    -- DB が拒否できない（RLS は SELECT 経路のみ）。
    FOREIGN KEY (device_id, tenant_id)
        REFERENCES devices(id, tenant_id)
        ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_device_commands_tenant_id ON device_commands(tenant_id);
CREATE INDEX IF NOT EXISTS idx_device_commands_device_id ON device_commands(device_id);
CREATE INDEX IF NOT EXISTS idx_device_commands_status ON device_commands(status);
