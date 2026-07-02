-- 0018_tenant_binding_state_and_signup_url.down.sql
-- 0018_tenant_binding_state_and_signup_url.up.sql の対称巻き戻し。
--   1. 'binding' 状態の行を旧 3 値互換の 'pending_bind' へ正規化する（後方互換 / 下記理由）。
--   2. signup_url_name 列を DROP COLUMN IF EXISTS で巻き戻す。
--   3. enum 値 'binding' 自体の巻き戻しは no-op（下記理由）。
--
-- 'binding' 行の正規化について（後方互換 / #52 PR #62 review finding [low]）:
--   enum 値 'binding' は個別削除できず（下記）DROP せずに残るため、部分ロールバック
--   （0018 のみを 0016 まで戻して 3 状態前提の旧アプリを稼働させる）で status='binding' の
--   行が残ると、旧アプリの状態機械（pending_bind / bound / disabled の 3 値）が未知値を
--   受け取り API 出力・状態判定の後方互換性を壊す。そこで down では残存する 'binding' 行を
--   'pending_bind' へ戻す。binding は「bind 予約中（enterprise_name 未確定・NULL）」の
--   中間状態であり、旧 3 値では「未 bind = pending_bind」が意味的に等価な正本である
--   （ReleaseBinding / RecoverStaleBindings の binding→pending_bind 回収と同じ写像）。
--   これにより旧アプリは当該テナントを「未 bind（再 bind 可能）」として安全に扱える。
--   本 UPDATE は 0016 以前のスキーマ（enterprise_name 部分一意 index / RLS）と非衝突
--   （binding 行の enterprise_name は NULL のため部分 index 対象外）。全 down シーケンス
--   （0018 → ... → 0001）では後段の 0001 down が tenants ごと DROP するため実質空振り
--   （0 行更新）で無害であり、migrations_reversible_test の全 down→up は影響を受けない。
--
-- enum 値 'binding' の巻き戻しについて（重要 / migrations_reversible_test 非破壊）:
--   PostgreSQL には ALTER TYPE ... DROP VALUE 構文が存在せず、enum へ追加した値を
--   個別に削除することはできない。そのため 0018 down では 'binding' の削除を試みず
--   no-op とする。全 down シーケンス（0018 → ... → 0001）では 0001 の down が
--   `DROP TYPE IF EXISTS tenant_status` で enum 型ごと削除するため、0018 down が
--   'binding' を消せなくても down シーケンス全体は成功する。再 up では 0001 が enum を
--   3 値（pending_bind / bound / disabled）で再作成し、0018 が 'binding' を再 ADD VALUE
--   するため可逆性は満たされる（migrations_reversible_test の全 down→up が pass する）。

-- 1. 残存 'binding' 行を旧 3 値互換の 'pending_bind' へ正規化する（後方互換）。
UPDATE tenants SET status = 'pending_bind', updated_at = now() WHERE status = 'binding';

-- 2. signup_url_name 列を巻き戻す。
ALTER TABLE tenants DROP COLUMN IF EXISTS signup_url_name;

-- 3. enum 値 'binding' は PostgreSQL で個別削除できないため no-op（上記コメント参照）。
