-- 0014_admin_users_add_oidc_issuer.up.sql
-- Issue #33 (A3a) Req 6.3 / 6.4 / 1.4 / 1.5
--
-- admin_users テーブルに oidc_issuer 列を追加し、既存の oidc_subject 単独 UNIQUE 制約を
-- (oidc_issuer, oidc_subject) 複合 UNIQUE 制約へ差し替える。
--
-- 理由（design.md / tasks.md L115〜L123 と整合）:
--   OIDC `sub` は issuer スコープでのみ一意のため、tenant / admin で issuer を分けられる本設計では
--   (oidc_issuer, oidc_subject) の組で admin_users を解決する必要がある。oidc_subject 単独の
--   UNIQUE 制約は別 issuer の同 sub と衝突する偽陽性を生むため、複合キー化する。
--
-- 重要 — backfill は literal placeholder で行わない（fail-closed 設計）:
--   既存行が存在する場合、DEFAULT '' によって空文字でバックフィルされる。空文字は
--   claims.Issuer（実 issuer URL）と一致しないため、既存行は ResolveAdminUser で 403
--   admin_user_not_provisioned になる。これは「issuer URL を後から admin-seed CLI /
--   管理 UI（後続 Issue）で正しい値に上書きするまでログインできない」という意図された
--   fail-closed 挙動である。環境依存の literal placeholder URL を migration に埋めると、
--   本番 / dev 等で異なる issuer URL を必要とする運用で誤動作するため意図的に避ける。

-- 1) oidc_issuer 列を追加（NOT NULL DEFAULT '' で backfill 後、DROP DEFAULT で明示指定強制）
ALTER TABLE admin_users ADD COLUMN oidc_issuer text NOT NULL DEFAULT '';
ALTER TABLE admin_users ALTER COLUMN oidc_issuer DROP DEFAULT;

-- 2) 既存 UNIQUE 制約 (oidc_subject) を解除し、(oidc_issuer, oidc_subject) 複合 UNIQUE に置換
--    Postgres は CREATE TABLE で UNIQUE 列を宣言すると `<table>_<column>_key` という制約名を
--    自動で付与する（0002_create_admin_users_and_roles.up.sql 参照）。
ALTER TABLE admin_users DROP CONSTRAINT admin_users_oidc_subject_key;
ALTER TABLE admin_users ADD CONSTRAINT admin_users_oidc_issuer_subject_key
    UNIQUE (oidc_issuer, oidc_subject);
