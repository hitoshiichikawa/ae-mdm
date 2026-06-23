# ローカル開発 Runbook

ae-mdm（Android Enterprise EMM SaaS）のローカル開発環境を立ち上げ、
マイグレーション・seed を実行するまでの手順です。

> 対応要件: `docs/specs/1--a1-docker-compose/requirements.md` Requirement 8

## 前提

以下のツールをホストにインストールしてください。

| ツール | バージョン | 用途 |
|---|---|---|
| Go | 1.22 以上 | backend ビルド / golang-migrate 実行 |
| Node.js | 20 以上 | frontend ビルド |
| npm | 10 以上 | パッケージマネージャ（pnpm でも可） |
| Docker | 24 以上 | コンテナランタイム |
| Docker Compose | v2.20 以上 | サービスオーケストレーション |
| GNU make | 4 以上 | ワークフロー実行 |

## クイックスタート

```bash
# 1. リポジトリをクローン後、初期化
git clone <repository_url> ae-mdm && cd ae-mdm

# 2. 環境変数テンプレートをコピーして編集
cp .env.example .env
# .env を開いて <REPLACE_ME...> を実値に置き換える
# 特に POSTGRES_PASSWORD / KEYCLOAK_ADMIN_PASSWORD / SESSION_SECRET は必須
# SESSION_SECRET の生成例: openssl rand -hex 32
# Issue #2 以降: <REPLACE_ME_APP_PASSWORD> / <REPLACE_ME_MIGRATION_PASSWORD> も置換
# （backend/db/roles/0001_create_app_and_migration_roles.sql 内の placeholder も併せて編集）

# 3. 依存をインストール
npm install
( cd backend && GOTOOLCHAIN=local go mod download )

# 4. 全サービスをビルド + 起動
make build
make up

# 5. health check が通過するまで待機（30〜60 秒）
docker compose ps

# 6. アプリ用ロール（app_user / migration_user）の初期セットアップ
#    Issue #2 / Req 6.5 / docs/specs/2--a2-config-logger-errors-db-rls/design.md 参照。
#    本 target は golang-migrate の管轄外（schema_migrations に記録されない）。
make db-init-roles

# 7. マイグレーション適用（migration_user 接続で DDL を実行）
make migrate-up

# 8. （後続 Issue で実装される）初期 SuperAdmin の seed
# 本 Issue 時点では admin-seed CLI は未実装。umbrella task 14.2 で実装される。
# 暫定: Keycloak admin console (http://localhost:8081/) から手動でユーザーを作成し、
#       SuperAdmin グループに割り当てる。
```

## 詳細手順

### 1. 環境変数の用意

`.env.example` をコピーし、`<REPLACE_ME...>` プレースホルダを実値に置き換えます。

```bash
cp .env.example .env
```

最低限必須の項目:

- `POSTGRES_PASSWORD`: postgres コンテナ superuser のパスワード（コミット禁止）。
  `make db-init-roles` で app_user / migration_user を作成するときの superuser 接続にも使う
- `KEYCLOAK_ADMIN_PASSWORD`: Keycloak 管理コンソールの初期パスワード
- `SESSION_SECRET`: 32 バイト以上のランダム hex。`openssl rand -hex 32` で生成
- `DATABASE_URL`: `app_user` 接続（DML 専用、RLS バインド対象、audit_logs UPDATE/DELETE は
  REVOKE 済）。`<REPLACE_ME_APP_PASSWORD>` を、後述
  `backend/db/roles/0001_create_app_and_migration_roles.sql` で app_user に付与する
  パスワードと一致させること（compose 内部 hostname `postgres` を指す。コンテナ内 api /
  worker が参照）
- `MIGRATE_DATABASE_URL`: `migration_user` 接続（DDL 専用）。ホストから `make migrate-up` /
  `make migrate-down` を実行するときに使う接続文字列。`POSTGRES_HOST_PORT`（既定 5432）で
  publish された `localhost` を指す。`<REPLACE_ME_MIGRATION_PASSWORD>` を、
  `backend/db/roles/0001_create_app_and_migration_roles.sql` で migration_user に付与する
  パスワードと一致させること。`MIGRATE_DATABASE_URL` が空の場合 Makefile は
  `DATABASE_URL` にフォールバックするが、その場合 app_user は DDL 権限を持たないため
  migrate に失敗する

> **2 ロール分離（Issue #2 / Req 6.5）**:
> `app_user`（DML 専用、`audit_logs` UPDATE/DELETE は REVOKE 済）と `migration_user`
> （DDL 専用）の 2 ロールを `backend/db/roles/0001_create_app_and_migration_roles.sql` で
> 定義し、`make db-init-roles` で初期セットアップします。本ファイル内の `<REPLACE_ME_*>`
> placeholder は手動で置換するか、`envsubst` / `sed` 等で env から流し込んでから適用してください。
> compose の `POSTGRES_USER` / `POSTGRES_PASSWORD`（既定 `ae_mdm` / `<REPLACE_ME...>`）は
> postgres コンテナ superuser として残し、app_user / migration_user 作成のために
> `make db-init-roles` 実行時にも使用します。

> `.env` は `.gitignore` で除外されています。実値をコミットしないでください
> （requirements.md NFR 1.2）。

### 2. ビルド

```bash
make build
```

- `backend`: `go build ./...` で `cmd/api` と `cmd/worker` をコンパイル
- `frontend`: npm workspaces 経由で `@ae-mdm/shared`（tsc --noEmit）、
  `@ae-mdm/tenant-console`（vite build）、`@ae-mdm/admin-console`（vite build）をビルド

### 3. サービス起動

```bash
make up
```

7 つのサービスが立ち上がります（requirements.md Requirement 5.2）:

| サービス | 用途 | デフォルト URL |
|---|---|---|
| postgres | データベース | `localhost:5432` |
| keycloak | OIDC IdP（dev realm: `ae-mdm`） | `http://localhost:8081/` |
| pubsub-emulator | Cloud Pub/Sub emulator | `localhost:8085` |
| api | backend HTTP API（chi） | `http://localhost:8080/` |
| worker | Pub/Sub subscriber | — (ポート公開なし) |
| tenant-console | 顧客 IT 管理者向け SPA | `http://localhost:5173/` |
| admin-console | SaaS 運用者向け SPA | `http://localhost:5174/` |

依存関係（`depends_on`）により、`api` と `worker` は `postgres`・`keycloak`・
`pubsub-emulator` の health check が通過してから起動します（requirements.md 5.4）。

状態確認:

```bash
docker compose ps
docker compose logs -f api worker
```

### 4. アプリ用ロールの初期セットアップ + マイグレーション

#### 4.1 `make db-init-roles`（app_user / migration_user の作成）

```bash
# postgres superuser 接続で app_user / migration_user を作成・GRANT する
make db-init-roles
```

- 適用される SQL: `backend/db/roles/0001_create_app_and_migration_roles.sql`
- 接続 URL の決定順: `POSTGRES_INIT_URL` → `MIGRATE_DATABASE_URL` → `DATABASE_URL`。
  superuser 接続が必要なので、`POSTGRES_INIT_URL=postgres://ae_mdm:<superuser_pwd>@localhost:5432/ae_mdm?sslmode=disable`
  を `.env` または CLI から明示すると安全
- 前提: ホストに `psql` CLI がインストールされていること
- 本 target は **`golang-migrate` の管轄外**（schema_migrations テーブルに記録されない）。
  そのため `make migrate-down` で巻き戻されることもなく、2 回目以降の実行は
  `DO $$ ... duplicate_object EXCEPTION ... END $$` で冪等に skip される
- パスワード placeholder（`<REPLACE_ME_APP_PASSWORD>` / `<REPLACE_ME_MIGRATION_PASSWORD>`）は
  事前に SQL ファイルを編集するか、`envsubst` / `sed` で実値に置換してから適用する

##### ホストに psql がない場合の手動手順

```bash
# postgres コンテナに psql を投入する経路を使う
docker compose cp backend/db/roles/0001_create_app_and_migration_roles.sql \
    postgres:/tmp/init-roles.sql
docker compose exec -T postgres \
    psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -v ON_ERROR_STOP=1 -f /tmp/init-roles.sql
```

#### 4.2 `make migrate-up` / `make migrate-down`

```bash
# 順方向に全 migration を適用（migration_user 接続で DDL を実行）
make migrate-up

# 1 ステップだけ巻き戻す
make migrate-down
```

- Issue #2 で `backend/db/migrations/0001-0012_*.{up,down}.sql` を追加済み。`migrate-up` を
  実行すると 12 ペアが順に適用され、全テーブル作成 → 0011 で RLS 有効化 →
  0012 で audit_logs append-only 強制（FORCE RLS + REVOKE UPDATE/DELETE）まで進む
- `db-init-roles` を **先に** 実行していない状態で `migrate-up` を走らせると、0012 の
  `REVOKE ... FROM app_user` が `app_user` 未作成のため NOTICE で skip される。後から
  `db-init-roles` を実行した場合は `migrate-down → migrate-up` で REVOKE を再適用するか、
  手動で `REVOKE UPDATE, DELETE ON audit_logs FROM app_user` を実行する

### 5. 初期 SuperAdmin の seed

本 Issue では `admin-seed` CLI は未実装です。umbrella task 14.2 で実装される予定です
（[Out of Scope](../specs/1--a1-docker-compose/requirements.md) を参照）。

暫定的な手動手順:

1. Keycloak admin console (`http://localhost:8081/`) に
   `KEYCLOAK_ADMIN` / `KEYCLOAK_ADMIN_PASSWORD` でログイン
2. realm を `ae-mdm` に切り替える（左上のドロップダウン）
3. Users → Create user で SuperAdmin ユーザーを作成
4. 作成したユーザーの Groups タブで `SuperAdmin` グループに参加
5. Credentials タブで初期パスワードを設定

OIDC ログインフロー（umbrella task 3.1）が実装されると、`http://localhost:5174/`
（admin-console）からこのユーザーでログインして SuperAdmin 操作が可能になります。

### 6. 終了

```bash
make down
```

`docker compose down --remove-orphans` 相当で、コンテナとネットワークを除去します。
データボリューム（`postgres-data`）は残ります。完全に消すには:

```bash
docker compose down -v --remove-orphans
```

## トラブルシューティング

### 起動に時間がかかる / health check が失敗する

- Keycloak は初回起動時に realm import を行うため、最大 60 秒程度かかります
  （requirements.md NFR 3.1 の閾値内）。
- ホスト側で同じポート（5432 / 8080 / 8081 / 5173 / 5174 / 8085）を使う別プロセスが
  動いていないか確認してください。`.env` で `*_HOST_PORT` を変更すれば回避できます。

### `go mod download` が新しい Go バージョンを要求する

- `GOTOOLCHAIN=local` 環境変数を指定してください（Makefile 内で設定済み）。
- それでも失敗する場合は `cd backend && GOTOOLCHAIN=local go mod tidy` を再実行。

### npm install で peer dependency 警告

- `--legacy-peer-deps` を付与して回避可能ですが、本 Issue では既に整合した依存を宣言済み
  なので、通常は不要です。

## エンドポイント参照表

| 用途 | URL |
|---|---|
| tenant-console SPA | http://localhost:5173/ |
| admin-console SPA | http://localhost:5174/ |
| backend api ルート群 (テナント系) | http://localhost:8080/api/ |
| backend api ルート群 (運用系 = SuperAdmin) | http://localhost:8080/api/admin/ |
| Keycloak admin console | http://localhost:8081/ |
| Keycloak realm OIDC discovery | http://localhost:8081/realms/ae-mdm/.well-known/openid-configuration |
| Pub/Sub emulator | http://localhost:8085/ |
| PostgreSQL | localhost:5432 |

## 参考

- 全体設計: `docs/specs/24-android-enterprise-emm-mvp/design.md`
- 本 Issue の要件: `docs/specs/1--a1-docker-compose/requirements.md`
- 全体 Roadmap: `docs/specs/24-android-enterprise-emm-mvp/tasks.md`
