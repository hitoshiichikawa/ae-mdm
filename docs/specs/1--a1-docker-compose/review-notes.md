# Review Notes

<!-- idd-claude:review round=1 model=claude-sonnet-4.5 timestamp=2026-06-23T16:30:00Z -->

## Reviewed Scope

- Branch: claude/issue-1-impl--a1-docker-compose
- HEAD commit: a7f97a6180805754e783107fe69e2b98fcf8a718
- Compared to: develop..HEAD
- Spec path: docs/specs/1--a1-docker-compose/
- 起動経路: 非 Architect ルート（`tasks.md` / `design.md` は存在せず、`_Boundary:_`
  アノテーションは未定義）。要件定義 `requirements.md` の Out of Scope 節を境界の正本として用いる
- Feature Flag Protocol: CLAUDE.md `## Feature Flag Protocol` の **採否: opt-out**
  のため flag 観点の確認は適用しない（既定の 3 カテゴリ判定のみ）

## Verified Requirements

### Requirement 1 (Backend Go module)
- 1.1 — `backend/go.mod`, `backend/go.sum` がルート直下 `backend/` 配下に配置
- 1.2 — `backend/go.mod:1` `module github.com/hitoshiichikawa/ae-mdm`
- 1.3 — `backend/go.mod:3` `go 1.22`（≥ 1.22）
- 1.4 — `backend/go.mod` require: chi (`go-chi/chi/v5`) / pgx (`jackc/pgx/v5`) /
  golang-migrate (`golang-migrate/migrate/v4`) / pubsub (`cloud.google.com/go/pubsub`) /
  AMAPI (`google.golang.org/api` 経由で androidmanagement/v1 へ到達 + `depspin.go` で
  `_ "google.golang.org/api/androidmanagement/v1"` ブランクインポート保持) /
  coreos/go-oidc (`coreos/go-oidc/v3`) / zap (`go.uber.org/zap`) を宣言。sqlc は
  `backend/sqlc.yaml` 配置 + `go install` 経由（code-gen ツール扱い、impl-notes 確認事項 1
  で説明済み）
- 1.5 — `backend/cmd/api/main.go`, `backend/cmd/worker/main.go` の最小 entrypoint 配置
- 1.6 — `Makefile` `build` ターゲットが `cd backend && GOTOOLCHAIN=local go build ./...`
  を実行。Reviewer 環境でも `go build ./...` が成功することを確認済み
- 1.7 — モジュール名 / Go バージョン宣言が 1.2 / 1.3 と一致

### Requirement 2 (Frontend 2 SPA + shared)
- 2.1 — `frontend/{shared, tenant-console, admin-console}/` の 3 ディレクトリ配置
- 2.2 — ルート `package.json` の `workspaces` 配列で npm workspaces を採用（OR 条件の片方）
- 2.3 — `frontend/shared/package.json` で `@ae-mdm/shared` library パッケージとして
  `exports` 公開。tenant-console / admin-console の `App.tsx` が
  `import { SHARED_LIB_PLACEHOLDER } from '@ae-mdm/shared'` で実 import
- 2.4 — `frontend/{tenant,admin}-console/package.json` に React 19 / Vite 5 /
  TypeScript 5 / Tailwind 3 / @tanstack/react-query / react-router-dom /
  react-hook-form / zod / oidc-client-ts を宣言。shadcn/ui は npm パッケージではなく
  Tailwind/Radix ベースの copy-paste CLI のため `shadcn/ui 互換セットアップ` として
  postcss + tailwind + tsx 環境を整備（impl-notes でも明記）
- 2.5 — 各 SPA に `vite.config.ts` / `tsconfig.json` / `index.html` / `src/main.tsx` 配置
- 2.6 — `Makefile` `build-frontend` が `npm run build`（workspaces 経由で 2 SPA を順次 build）
- 2.7 — `tenant-console/src/App.tsx` に SuperAdmin 専用記述なし、`admin-console/src/App.tsx`
  にテナント業務 UI 記述なし（各 App.tsx のコメントでも責務分離を明示）

### Requirement 3 (Makefile)
- 3.1 — ルートに `Makefile` 1 ファイル配置
- 3.2 — `build` / `test` / `lint` / `migrate-up` / `migrate-down` / `up` / `down` の 7 ターゲット定義
- 3.3 — `build: build-backend build-frontend` 順次実行（いずれか失敗で非ゼロ exit）
- 3.4 — `up: docker compose up -d`
- 3.5 — `down: docker compose down --remove-orphans`
- 3.6 — `migrate-up` / `migrate-down` が `go run -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate`
  で実行。migrations ディレクトリが空でも no-change を返す動作

### Requirement 4 (.env.example)
- 4.1 — ルートに `.env.example` 1 ファイル配置
- 4.2 — PostgreSQL: `POSTGRES_USER` / `POSTGRES_PASSWORD` / `POSTGRES_DB` /
  `POSTGRES_HOST_PORT` / `DATABASE_URL` 全てあり
- 4.3 — OIDC 2 セット: `OIDC_TENANT_{ISSUER_URL,CLIENT_ID,REDIRECT_URL}` と
  `OIDC_ADMIN_{ISSUER_URL,CLIENT_ID,REDIRECT_URL}` で別変数名として区別
- 4.4 — Pub/Sub: `PUBSUB_PROJECT_ID` / `PUBSUB_TOPIC` / `PUBSUB_SUBSCRIPTION` /
  `PUBSUB_EMULATOR_HOST` を宣言
- 4.5 — AMAPI: `GOOGLE_APPLICATION_CREDENTIALS` (鍵 JSON パス) / `AMAPI_PROJECT_ID`
- 4.6 — `SESSION_SECRET=<REPLACE_ME_GENERATE_32_BYTES_OF_RANDOM_HEX>`
- 4.7 — `AUDIT_LOG_RETENTION_DAYS=180` / `DEVICE_SYNC_DELAY_THRESHOLD_HOURS=24` の数値設定
- 4.8 — 実値（実 token / 鍵）は埋め込まれておらず、機微項目はすべて `<REPLACE_ME_*>`
  placeholder で表現

### Requirement 5 (Docker Compose 7 サービス)
- 5.1 — ルートに `docker-compose.yml` 1 ファイル配置
- 5.2 — 7 サービス定義: `postgres`, `keycloak`, `pubsub-emulator`, `api`, `worker`,
  `tenant-console`, `admin-console`
- 5.3 — health check 5 つ（postgres: `pg_isready`、keycloak: `/health/ready` を
  `/dev/tcp` 経由、pubsub-emulator: tcp 接続、api: `pgrep -x api`、worker: `pgrep -x worker`）
- 5.4 — `api` / `worker` の `depends_on` に postgres / keycloak / pubsub-emulator を
  `condition: service_healthy` で指定
- 5.5 — tenant-console: `${TENANT_CONSOLE_HOST_PORT:-5173}:80`、admin-console:
  `${ADMIN_CONSOLE_HOST_PORT:-5174}:80` で別ポート listen。各々独立 nginx コンテナ
- 5.6 — `frontend/{tenant,admin}-console/nginx.conf` で SPA fallback
  (`try_files $uri $uri/ /index.html;`) + `/api/` (tenant) / `/api/admin/` (admin) を
  backend `api` サービスへ reverse proxy
- 5.7 — `condition: service_healthy` により依存サービスが healthy になるまで起動順序を制約
- 5.8 — `docker-compose.yml` 内で実認証情報を埋め込まず `${VAR}` 展開で `.env` 注入。
  `POSTGRES_PASSWORD` / `KEYCLOAK_ADMIN_PASSWORD` / `SESSION_SECRET` は `:?...` 形式で
  missing 時 fail-fast。`docker compose config --quiet` (env 補完) で構文妥当性を確認済み

### Requirement 6 (Dockerfiles)
- 6.1 — `backend/Dockerfile.api` と `backend/Dockerfile.worker` を配置
- 6.2 — 両 Dockerfile が `FROM golang:1.22-alpine AS builder` + `FROM gcr.io/distroless/static-debian12:nonroot AS final`
  の multi-stage 構成。final stage は distroless
- 6.3 — `frontend/tenant-console/Dockerfile`, `frontend/admin-console/Dockerfile` を配置
- 6.4 — 両 frontend Dockerfile が `FROM node:20-alpine AS builder`（Vite build）+
  `FROM nginx:1.27-alpine AS final` の multi-stage 構成
- 6.5 — `frontend/{tenant,admin}-console/nginx.conf` を配置（SPA fallback + reverse proxy 設定）
- 6.6 — `make build` 実行で `go build` / `vite build` 系が成功する状態。Docker image
  build 本体は impl-notes 内で network/pull 権限の制約により skip と明記され、CI / 開発者
  ローカルで実行する前提（`docker compose config --quiet` で yaml の構文妥当性は確認）

### Requirement 7 (Keycloak realm-export.json)
- 7.1 — `infra/keycloak/realm-export.json` 1 ファイル配置
- 7.2 — `realm: "ae-mdm"`, `enabled: true`, `displayName: "ae-mdm Dev Realm"` で
  dev realm 定義 1 つ
- 7.3 — `tenant-console` クライアント定義（`clientId: tenant-console`,
  `redirectUris: ["http://localhost:5173/*", ...]`, PKCE `S256`）
- 7.4 — `admin-console` クライアント定義（`clientId: admin-console`,
  `redirectUris: ["http://localhost:5174/*", ...]`, PKCE `S256`）
- 7.5 — 2 クライアントは別 `clientId`・別 redirect URI・別 web origin。Keycloak の標準
  挙動により ID トークン `aud` クレームが clientId で分離される
- 7.6 — `roles.realm[]` に SuperAdmin / TenantAdmin / Operator / Viewer の 4 ロール、
  `groups[]` に同名グループを `realmRoles` 紐付きで定義
- 7.7 — `docker-compose.yml` の keycloak サービスが `command: ["start-dev", "--import-realm"]`
  + `realm-export.json` を `/opt/keycloak/data/import/realm-export.json` に volume mount

### Requirement 8 (Local-dev runbook)
- 8.1 — `docs/runbook/local-dev.md` 1 ファイル配置
- 8.2 — クイックスタート節「2. 環境変数テンプレートをコピーして編集」+ 詳細手順「1. 環境変数の用意」
- 8.3 — クイックスタート節「4. 全サービスをビルド + 起動」+ 詳細手順「2. ビルド」「3. サービス起動」
- 8.4 — 詳細手順「4. マイグレーション」`make migrate-up` / `make migrate-down`
- 8.5 — 詳細手順「5. 初期 SuperAdmin の seed」で暫定の Keycloak admin console 手順 +
  umbrella task 14.2 への参照リンク
- 8.6 — クイックスタート節「サービス一覧」表 + 「エンドポイント参照表」節で
  tenant-console / admin-console / Keycloak / Pub/Sub emulator / api / postgres の URL/ポートを列挙

### Non-Functional Requirements
- NFR 1.1 — 全サービスの設定は `${VAR}` で `.env` から注入。状態は `postgres-data` volume
  のみで他は stateless
- NFR 1.2 — 認証情報はリポジトリ管理対象ファイルに埋め込まれず、すべて `.env` 経由
- NFR 2.1 — tenant-console / admin-console は別 `clientId` + 別 redirect URI +
  別 webOrigin。`.env.example` も `OIDC_TENANT_*` / `OIDC_ADMIN_*` で物理分離
- NFR 3.1 — postgres は `interval=5s retries=12 start_period=10s`、keycloak は
  `interval=10s retries=12 start_period=30s`、pubsub-emulator は `interval=5s retries=12 start_period=10s`
  で 60〜180 秒以内の収束に設定（個別チューニング可能、impl-notes 確認事項 3〜4 で
  後続 task 14.x との関係も明記）
- NFR 4.1 — backend の final stage は `gcr.io/distroless/static-debian12:nonroot`。
  builder stage の `golang:1.22-alpine` は最終 image に含まれない

## Findings

なし

## Summary

requirements.md の Requirement 1〜8（計 48 個の AC）および NFR 1〜4 すべてに対し、対応する
実装ファイル（`backend/`, `frontend/`, `docker-compose.yml`, `Makefile`, `.env.example`,
`infra/keycloak/realm-export.json`, `docs/runbook/local-dev.md`）が配置され、impl-notes に
列挙された Verification コマンド（`go build ./...`, `npm run build`, `make build`,
`docker compose config --quiet`）も Reviewer 環境で再実行して成功を確認した。本 Issue は
scaffold スコープであり、AC 自体が「ファイル配置」「build 成功」等の構造的観点に限定される
ため Developer が新規 unit test を追加しない判断は妥当（impl-notes 確認事項 6 で明記済み・
要件側にも unit test 追加の指定なし）。tasks.md が存在しないため `_Boundary:_` アノテーション
逸脱の論点は適用外で、requirements.md Out of Scope（ドメイン実装 / migration 本体 / seed CLI /
本番 Terraform / CI workflow）への踏み込みも認められない。Feature Flag Protocol は opt-out
のため flag 観点は適用外。3 カテゴリ（AC 未カバー / missing test / boundary 逸脱）いずれも
該当なし。

RESULT: approve
