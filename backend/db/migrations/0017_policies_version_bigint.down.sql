-- 0017_policies_version_bigint.down.sql
-- 0017_policies_version_bigint.up.sql の対称巻き戻し。
--
-- bigint → integer へ戻す。値域を狭める変換のため、int32 範囲外の version を持つ行が存在する場合は
-- "integer out of range" で失敗する（MVP では version は 1 から逐次増加するため通常は範囲内）。
-- DEFAULT 1（0005 で定義）は型変更の影響を受けず維持される。

ALTER TABLE policies ALTER COLUMN version TYPE integer;
