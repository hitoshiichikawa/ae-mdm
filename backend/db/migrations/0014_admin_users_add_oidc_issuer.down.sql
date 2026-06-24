-- 0014_admin_users_add_oidc_issuer.down.sql
-- 0014_admin_users_add_oidc_issuer.up.sql の対称巻き戻し。
--   1. 複合 UNIQUE 制約 (oidc_issuer, oidc_subject) を解除
--   2. 既存の oidc_subject 単独 UNIQUE 制約を復元
--   3. oidc_issuer 列を DROP

-- 1) 複合 UNIQUE を解除
ALTER TABLE admin_users DROP CONSTRAINT admin_users_oidc_issuer_subject_key;

-- 2) oidc_subject 単独 UNIQUE を復元（0002 が暗黙生成した制約名と一致させる）
ALTER TABLE admin_users ADD CONSTRAINT admin_users_oidc_subject_key UNIQUE (oidc_subject);

-- 3) oidc_issuer 列を DROP
ALTER TABLE admin_users DROP COLUMN oidc_issuer;
