-- 0017_tenant_binding_state_and_signup_url.down.sql
-- 0017_tenant_binding_state_and_signup_url.up.sql の対称巻き戻し。
--   1. signup_url_name 列を DROP COLUMN IF EXISTS で巻き戻す。
--   2. enum 値 'binding' の巻き戻しは no-op（下記理由）。
--
-- enum 値 'binding' の巻き戻しについて（重要 / migrations_reversible_test 非破壊）:
--   PostgreSQL には ALTER TYPE ... DROP VALUE 構文が存在せず、enum へ追加した値を
--   個別に削除することはできない。そのため 0017 down では 'binding' の削除を試みず
--   no-op とする。全 down シーケンス（0017 → ... → 0001）では 0001 の down が
--   `DROP TYPE IF EXISTS tenant_status` で enum 型ごと削除するため、0017 down が
--   'binding' を消せなくても down シーケンス全体は成功する。再 up では 0001 が enum を
--   3 値（pending_bind / bound / disabled）で再作成し、0017 が 'binding' を再 ADD VALUE
--   するため可逆性は満たされる（migrations_reversible_test の全 down→up が pass する）。

ALTER TABLE tenants DROP COLUMN IF EXISTS signup_url_name;

-- enum 値 'binding' は PostgreSQL で個別削除できないため no-op（上記コメント参照）。
