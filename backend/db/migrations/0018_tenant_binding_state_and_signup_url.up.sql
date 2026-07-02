-- 0018_tenant_binding_state_and_signup_url.up.sql
-- Issue #52 (#38 follow-up) Req 4.1 / Req 3.1
--
-- Tenant Bind の競合制御強化（バインド予約状態）と signup_url_name 紐付けのため、
-- tenant_status enum へ予約中状態 'binding' を追加し、tenants へ signup_url_name 列を
-- 追加する。0001（tenants 本体 / enum 定義）/ 0011（RLS）/ 0016（無効化監査列）は変更せず
-- ALTER のみで冪等に追加する（IF NOT EXISTS 活用 / 既存スキーマ非破壊）。
--
-- 設計意図:
--   - tenant_status への 'binding' 追加（Req 4.1 / NFR 1.1）: 状態機械を
--     pending_bind / binding / bound / disabled の 4 値へ拡張する。bind 要求は
--     pending_bind → binding の原子遷移に成功した勝者のみ CreateEnterprise へ進み、
--     AMAPI 上に未紐付けの Enterprise（orphan）が残らないようにする。
--     PostgreSQL 12+ では ALTER TYPE ... ADD VALUE をトランザクション内で実行でき、
--     追加した値を同じ migration 内で参照しない限り制約に抵触しない（0018 は値追加 +
--     列追加のみで 'binding' を参照する DML を含まない）。IF NOT EXISTS で再適用に耐える。
--   - signup_url_name 列（Req 3.1）: テナント作成時に発行する signup_url_name を発行元
--     テナントに永続化し、bind 時の正本として束縛するための列。NULL 許容（既存行・create
--     前の整合のため）。0016 と同じ ALTER のみの冪等パターンで追加する。

ALTER TYPE tenant_status ADD VALUE IF NOT EXISTS 'binding';

ALTER TABLE tenants ADD COLUMN IF NOT EXISTS signup_url_name text;
