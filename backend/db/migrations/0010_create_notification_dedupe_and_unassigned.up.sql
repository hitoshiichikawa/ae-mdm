-- 0010_create_notification_dedupe_and_unassigned.up.sql
-- Req 6.1, 6.2
-- Pub/Sub 通知の冪等処理状態と未割当退避。両テーブルとも tenant_id を持たない
-- infra 用テーブル（cross-tenant の SuperAdmin operations 対象 / RLS は 0011 で SuperAdmin
-- のみ可視に倒す）。

CREATE TABLE IF NOT EXISTS notification_dedupe (
    message_id        text PRIMARY KEY,
    notification_type text NOT NULL,
    processed_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS unassigned_notifications (
    id                 uuid PRIMARY KEY,
    message_id         text NOT NULL,
    notification_type  text NOT NULL,
    enterprise_name    text NOT NULL,
    payload            jsonb NOT NULL DEFAULT '{}'::jsonb,
    received_at        timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_unassigned_notifications_received_at ON unassigned_notifications(received_at);
