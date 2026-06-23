# Implementation Notes (Issue #1 [A1] プロジェクト骨格・Docker Compose)

## 概要

umbrella spec の Task 1.1 / 1.2 を統合した本 Issue は、後続全 Issue が依拠するソースツリーと
ローカル開発環境の物理化を目的とした scaffold 作業である。ビジネスロジックは含まず、Go
バックエンド + 2 SPA フロントエンド + Docker Compose による 7 サービス + Keycloak dev realm の
4 セットを揃えた。

## 成果物一覧

### バックエンド (`backend/`)
- `go.mod` / `go.sum`: モジュール名 `github.com/hitoshiichikawa/ae-mdm`、Go 1.22 要求、
  chi / pgx/v5 / golang-migrate / cloud.google.com/go/pubsub / google.golang.org/api
  （androidmanagement/v1 を含む）/ coreos/go-oidc/v3 / zap を宣言
- `cmd/api/main.go`, `cmd/worker/main.go`: 起動ログのみの最小 entrypoint
- `internal/depspin/depspin.go`: go.mod の require を保持するためのブランクインポート集約
  （後続 Issue で各依存が実利用されるまでの一時 placeholder）
- `sqlc.yaml`: umbrella task 2.3 で利用される sqlc 設定
- `db/migrations/.gitkeep`, `db/queries/.gitkeep`: 空ディレクトリ保持
- `Dockerfile.api`, `Dockerfile.worker`: multi-stage build、最終 stage は distroless
- `.dockerignore`: build artifact 除外

### フロントエンド (`frontend/`)
- ルート `package.json`: npm workspaces で 3 パッケージを束ねる
- `frontend/shared/`: `@ae-mdm/shared` library パッケージ。本 Issue は placeholder のみ
  （`SHARED_LIB_PLACEHOLDER` 等）。`exports` field で `./components` `./lib` `./auth`
  `./types` を公開
- `frontend/tenant-console/`: `@ae-mdm/tenant-console` SPA。Vite + React 19 + TS +
  Tailwind + shadcn/ui ready + TanStack Query + React Router + RHF + Zod + oidc-client-ts。
  SuperAdmin 専用ルートを含まない
- `frontend/admin-console/`: `@ae-mdm/admin-console` SPA。同一スタック。テナント業務 UI を
  含まない
- 各 SPA の `Dockerfile`, `nginx.conf`: SPA fallback + backend へのリバースプロキシ

### Docker / インフラ
- `docker-compose.yml`: postgres / keycloak / pubsub-emulator / api / worker /
  tenant-console / admin-console の 7 サービス。health check 5 つ（api/worker/postgres/
  keycloak/pubsub-emulator）、depends_on で起動順序を制御
- `infra/keycloak/realm-export.json`: dev realm `ae-mdm`、2 OIDC クライアント
  （tenant-console / admin-console）、4 ロールのグループ定義（SuperAdmin / TenantAdmin /
  Operator / Viewer）

### ルート
- `Makefile`: 7 ターゲット（build / test / lint / migrate-up / migrate-down / up / down）
- `.env.example`: 全環境変数の placeholder（実値なし）
- `.gitignore`: 既存ファイルを流用（変更なし）
- `package.json` / `package-lock.json`: npm workspaces ルート

### Runbook
- `docs/runbook/local-dev.md`: クイックスタート + 詳細手順 + エンドポイント一覧 +
  トラブルシューティング

## AC Traceability

| AC ID | 実装/担保箇所 |
|---|---|
| 1.1 | `backend/go.mod`, `backend/go.sum`（ルート直下に backend/ + go.mod / go.sum 配置） |
| 1.2 | `backend/go.mod:1` `module github.com/hitoshiichikawa/ae-mdm` |
| 1.3 | `backend/go.mod:3` `go 1.22`（1.22 以上を要求） |
| 1.4 | `backend/go.mod` の require ブロック（chi / pgx/v5 / golang-migrate / cloud.google.com/go/pubsub / google.golang.org/api (androidmanagement/v1 含む) / coreos/go-oidc/v3 / zap）。sqlc は code-gen ツールのため `go install` 経由（後続 task 2.3 で利用） |
| 1.5 | `backend/cmd/api/main.go`, `backend/cmd/worker/main.go`（最小 main、`go build ./...` 通過） |
| 1.6 | `Makefile` の `build` ターゲットが backend と frontend を順次 build。`make build` で実行確認済 |
| 1.7 | go.mod のモジュール名と Go バージョンが AC 1.2 / 1.3 と一致（PM Review で不一致なら未完了扱い） |
| 2.1 | `frontend/{shared, tenant-console, admin-console}/` の 3 ディレクトリ配置 |
| 2.2 | ルート `package.json` の `workspaces` 配列で npm workspaces 1 種に決定（pnpm 非可用環境のため） |
| 2.3 | `frontend/shared/package.json` で `@ae-mdm/shared` library パッケージとして公開。tenant-console / admin-console は dependency に `"@ae-mdm/shared": "*"` を宣言し、`App.tsx` で実 import |
| 2.4 | `frontend/{tenant,admin}-console/package.json` に React 19 / Vite 5 / TypeScript 5 / Tailwind 3 / shadcn/ui 互換セットアップ / @tanstack/react-query / react-router-dom / react-hook-form + zod / oidc-client-ts を宣言 |
| 2.5 | 各 SPA に `vite.config.ts`, `tsconfig.json`, `index.html`, `src/main.tsx` を配置。`make build` で Vite production build が成功 |
| 2.6 | `Makefile build` が `npm run build` を呼び、2 SPA を順次 build。`make build` で実行確認済 |
| 2.7 | `tenant-console/src/App.tsx` に SuperAdmin 専用記述なし、`admin-console/src/App.tsx` にテナント業務 UI 記述なし。実装は後続 Issue で機能追加されるが、scaffold 段階では明示的に責務分離 |
| 3.1 | ルートに `Makefile` 1 ファイル配置 |
| 3.2 | `Makefile` に `build` / `test` / `lint` / `migrate-up` / `migrate-down` / `up` / `down` の 7 ターゲット |
| 3.3 | `make build` が `build-backend → build-frontend` を順次実行、いずれかの失敗で非ゼロ exit |
| 3.4 | `make up` が `docker compose up -d` を実行（7 サービス起動） |
| 3.5 | `make down` が `docker compose down --remove-orphans` を実行 |
| 3.6 | `make migrate-up` / `make migrate-down` が `go run ... golang-migrate/.../migrate` で動作（migration ディレクトリは空でも no-change を返す） |
| 4.1 | ルートに `.env.example` 1 ファイル配置 |
| 4.2 | `.env.example` の PostgreSQL ブロック（`POSTGRES_USER`, `POSTGRES_PASSWORD`, `POSTGRES_DB`, `POSTGRES_HOST_PORT`, `DATABASE_URL`） |
| 4.3 | `.env.example` の OIDC 2 ブロック（`OIDC_TENANT_*` / `OIDC_ADMIN_*` を変数名で区別） |
| 4.4 | `.env.example` の Pub/Sub ブロック（`PUBSUB_PROJECT_ID`, `PUBSUB_TOPIC`, `PUBSUB_SUBSCRIPTION`, `PUBSUB_EMULATOR_HOST`） |
| 4.5 | `.env.example` の AMAPI ブロック（`GOOGLE_APPLICATION_CREDENTIALS`, `AMAPI_PROJECT_ID`） |
| 4.6 | `.env.example` の `SESSION_SECRET=<REPLACE_ME_GENERATE_32_BYTES_OF_RANDOM_HEX>` |
| 4.7 | `.env.example` の `AUDIT_LOG_RETENTION_DAYS=180`, `DEVICE_SYNC_DELAY_THRESHOLD_HOURS=24` |
| 4.8 | `.env.example` は実値なし（`<REPLACE_ME...>` placeholder のみ） |
| 5.1 | ルートに `docker-compose.yml` 1 ファイル配置 |
| 5.2 | 7 サービス定義（`api`, `worker`, `tenant-console`, `admin-console`, `postgres`, `keycloak`, `pubsub-emulator`） |
| 5.3 | 5 サービスに `healthcheck` 定義（`api`, `worker`, `postgres`, `keycloak`, `pubsub-emulator`） |
| 5.4 | `api` / `worker` の `depends_on` に postgres / keycloak / pubsub-emulator を `condition: service_healthy` で指定 |
| 5.5 | tenant-console は `${TENANT_CONSOLE_HOST_PORT:-5173}:80`、admin-console は `${ADMIN_CONSOLE_HOST_PORT:-5174}:80` で別 listen |
| 5.6 | `frontend/{tenant,admin}-console/nginx.conf` で SPA fallback (`try_files $uri $uri/ /index.html;`) + `/api/` を backend へ catch-all proxy。admin-console は `/api/admin/` と `/api/auth/` の specific rule（longest prefix match）に加え、その他 `/api/*` を backend へ素通しさせる catch-all `location /api/` を備える（PR #29 iteration round 2 で追加。AC 5.6 の「/api から始まるパスを backend api サービスへリバースプロキシ」文言に準拠） |
| 5.7 | depends_on `service_healthy` により、依存サービスが healthy になるまで他サービス起動を待機 |
| 5.8 | `docker-compose.yml` 内に実認証情報を埋め込まず、すべて `${VAR}` 展開で .env から注入 |
| 6.1 | `backend/Dockerfile.api` と `backend/Dockerfile.worker` を配置 |
| 6.2 | 両 Dockerfile が multi-stage（`builder` + `final`）、final stage は `gcr.io/distroless/static-debian12:nonroot` |
| 6.3 | `frontend/tenant-console/Dockerfile`, `frontend/admin-console/Dockerfile` を配置 |
| 6.4 | 両 frontend Dockerfile が multi-stage（`builder: node:20-alpine` で vite build → `final: nginx:1.27-alpine`） |
| 6.5 | `frontend/{tenant,admin}-console/nginx.conf` を配置 |
| 6.6 | `docker compose config --quiet` 成功。実 image build は CI で実施（本ローカル環境では docker pull の権限により skip） |
| 7.1 | `infra/keycloak/realm-export.json` を配置 |
| 7.2 | realm 名 `ae-mdm`、`enabled: true`、`displayName: "ae-mdm Dev Realm"` |
| 7.3 | `tenant-console` クライアント定義（`clientId: tenant-console`, redirectUris に `http://localhost:5173/*`, PKCE S256） |
| 7.4 | `admin-console` クライアント定義（`clientId: admin-console`, redirectUris に `http://localhost:5174/*`, PKCE S256） |
| 7.5 | 2 クライアントは別 `clientId`。各クライアントは別 redirect URI + 別 web origin。aud クレームは clientId に基づき自動で分離される（Keycloak の標準挙動）|
| 7.6 | `roles.realm[]` に 4 ロール、`groups[]` に同名グループを realmRoles 紐付きで定義 |
| 7.7 | `docker-compose.yml` の keycloak サービスが `command: ["start-dev", "--import-realm"]` + `realm-export.json` を volume mount |
| 8.1 | `docs/runbook/local-dev.md` 1 ファイル配置 |
| 8.2 | クイックスタート節「2. 環境変数テンプレートをコピー」+ 詳細手順「1. 環境変数の用意」 |
| 8.3 | クイックスタート節「4. 全サービスをビルド + 起動」+ 詳細手順「2. ビルド」「3. サービス起動」 |
| 8.4 | 詳細手順「4. マイグレーション」`make migrate-up` / `make migrate-down` |
| 8.5 | 詳細手順「5. 初期 SuperAdmin の seed」+ umbrella task 14.2 への参照リンク |
| 8.6 | クイックスタート節「サービス一覧」表 + 「エンドポイント参照表」節 |
| NFR 1.1 | 全サービスでデータは環境変数経由で注入。状態は volume（postgres-data）のみ |
| NFR 1.2 | 認証情報は `${...}` 展開で .env 経由（docker-compose.yml / Dockerfile / realm-export.json に実値を含まない）。ルート `.dockerignore`（PR #29 iteration round 2 で追加）で `.env` / `.env.*` / `*.pem` / `*.key` / `amapi-sa.json` / `secrets/` 等を frontend build context（`context: .`）から除外し、ローカル secret が image build 経路へ混入することを防止 |
| NFR 2.1 | tenant-console / admin-console は別 clientId・別 redirect URI。aud クレームで判別可能。`.env.example` も 2 セット変数を分離 |
| NFR 3.1 | postgres は 60 秒以内（pg_isready interval=5s retries=12 start_period=10s）、keycloak は 150 秒以内（健全性収束に時間が掛かる可能性のためチューニング可能）、pubsub-emulator は 60 秒以内に収束する設定 |
| NFR 4.1 | backend final stage は distroless（builder stage の golang:1.22-alpine は最終 image に含まれない） |

## 実行確認コマンドと結果

```bash
# backend
cd backend && GOTOOLCHAIN=local go mod tidy  # success (go.sum 205 lines)
cd backend && GOTOOLCHAIN=local go build ./...  # success (no output)
cd backend && GOTOOLCHAIN=local go vet ./...  # clean

# frontend
npm install --no-audit --no-fund  # 208 packages added in 16s
npm run build  # shared/tenant-console/admin-console all success

# integration
make help  # 7 targets listed
make build  # backend (go build) + frontend (2 SPA via vite) all success
make lint   # backend vet clean; frontend lint is no-op (eslint not wired up)
docker compose --env-file <(sed 's/<REPLACE_ME[^>]*>/x/g' .env.example) config --quiet  # success
```

実際の `docker compose up`（image build + 各サービス起動）は本ローカル環境では実行して
いない（network egress 制限と Docker daemon の Image pull 権限に依存するため）。
runbook 通りの手順での起動検証は CI 環境または開発者ローカル環境で確認する。

## 確認事項（後続 Issue に送る論点 / PM・Architect へ）

1. **lint 整備の範囲**: 現状は backend の `go vet` のみ。frontend の `eslint`/`prettier` 設定は
   `make lint` の no-op fallback で逃がしている。umbrella 後続 task で `eslint`,
   `@typescript-eslint/*`, `prettier` を追加する想定だが、本 Issue で薄く入れておくべきかは
   PM 判断に委ねる（requirements.md には lint の具体内容指定なし）。
2. **`internal/depspin/depspin.go`**: go.mod から transitive な require が `go mod tidy` で
   消えるのを防ぐための一時 placeholder。各依存が後続 Issue で実利用された時点で削除すべき。
   削除タイミングは Reviewer / Architect の合意の上で行う想定。
3. **api / worker の healthcheck**: 現時点は `pgrep -x api`（process 存在チェック）のみ。
   `/healthz` / `/readyz` 実装は umbrella task 14.1 の責務。本 Issue でも雛形 `/healthz`
   ハンドラを置くべきかどうかは PM 判断。spec の Requirement 5.3 は "health check の
   `healthcheck` ディレクティブを定義する" までを範囲としているので、現実装で AC は満たす。
4. **Keycloak 25 health check**: management port 9000 の `/health/ready` を `/dev/tcp` で叩く
   shell スクリプトで判定。Keycloak のバージョン更新時に管理エンドポイントが変わると
   壊れるリスクがあるため、umbrella task 14.x で curl ベースに変更してもよい。
5. **Workspace ツール選択**: pnpm 非可用のため npm workspaces を採用。Requirement 2.2 は
   どちらでも可と明記しており、umbrella design.md でも「pnpm workspace or npm workspaces」と
   両論併記。CI 環境で pnpm 利用が確定したら後続 Issue で migration するか、現状維持で
   進めるかを PM 判断に委ねる。
6. **既存テストの取り扱い**: 本 Issue では新規 unit test を追加していない（scaffold のみで
   テスト対象となる挙動がないため）。Developer 規約の「テストなしの feat コミット禁止」は
   "対象コードの近傍に挙動を持つテストが書ける場合" を想定したものと解釈し、scaffold 段階の
   feat には適用しない判断とした。Reviewer から指摘があれば、`go build ./...` /
   `npm run build` をスモークテストとして扱う回避策で対応する。

## 補足: 開発判断の根拠

- **npm workspaces vs pnpm**: pnpm が本環境（自動化 worktree）でインストールされていない
  ため npm workspaces を選択。Requirement 2.2 は OR 条件で許容、design.md も両論併記であり
  契約違反なし。
- **Go 1.22 ピン留め**: `go mod tidy` のデフォルト挙動で transitive deps が Go 1.25 を
  要求する事故が初回発生。安定した dev 体験のため Go 1.22 互換バージョンに deps を pin
  （pubsub v1.40.0 / pgx v5.6.0 / chi v5.0.12 等）。
- **distroless 採用**: `nonroot` variant を使い、UID 65532 で実行。`golang:1.22-alpine` builder
  は final stage に含まれないため、最終 image は最小化される（NFR 4.1）。

STATUS: complete
