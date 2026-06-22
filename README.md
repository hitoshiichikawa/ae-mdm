# ae-mdm

Android Enterprise（Android Management API / AMAPI）を用いた独自 EMM（Enterprise Mobility Management）SaaS。

> **現在のステータス**: 仕様策定フェーズ。実装は未着手。

## 概要

- Google 製 **Android Device Policy** にポリシーを配信する **AMAPI ベース**の構成（自前 DPC は持たない）。
- **マルチテナント SaaS**。**OIDC + RBAC**（SuperAdmin / TenantAdmin / Operator / Viewer）。
- MVP の管理モードは **Fully Managed / Dedicated(Kiosk)** を **QR エンロール**。
- バックエンド: **Go + PostgreSQL**（`api` / `worker` の2プロセス）。フロントエンド: **React + TypeScript + Vite** の **2 コンソール**（顧客向け `tenant-console` / 運用向け `admin-console`）。
- **コンテナ前提**（Docker Compose、将来 AWS Fargate）。

## ドキュメント

- 機能カタログ（AMAPI の機能一覧）: [ae-api-feature.md](ae-api-feature.md)
- MVP 仕様: [docs/specs/1-android-enterprise-emm-mvp/](docs/specs/1-android-enterprise-emm-mvp/)
  - [requirements.md](docs/specs/1-android-enterprise-emm-mvp/requirements.md) — 要件定義（EARS）
  - [design.md](docs/specs/1-android-enterprise-emm-mvp/design.md) — 設計
  - [tasks.md](docs/specs/1-android-enterprise-emm-mvp/tasks.md) — タスク分割
  - [ui-design-brief.md](docs/specs/1-android-enterprise-emm-mvp/ui-design-brief.md) — UI デザインブリーフ
  - [ui-design-prompts.md](docs/specs/1-android-enterprise-emm-mvp/ui-design-prompts.md) — UI デザイン用プロンプト

## 開発

[idd-claude](https://github.com/hitoshiichikawa/idd-claude) による仕様駆動開発を導入予定。
