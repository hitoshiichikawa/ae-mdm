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

# 3. 依存をインストール
npm install
( cd backend && GOTOOLCHAIN=local go mod download )

# 4. 全サービスをビルド + 起動
make build
make up

# 5. health check が通過するまで待機（30〜60 秒）
docker compose ps

# 6. マイグレーション適用
make migrate-up

# 7. （後続 Issue で実装される）初期 SuperAdmin の seed
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

- `POSTGRES_PASSWORD`: 任意の dev 用パスワード（コミット禁止）
- `KEYCLOAK_ADMIN_PASSWORD`: Keycloak 管理コンソールの初期パスワード
- `SESSION_SECRET`: 32 バイト以上のランダム hex。`openssl rand -hex 32` で生成
- `DATABASE_URL`: `POSTGRES_PASSWORD` と整合した接続文字列に書き換える

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

### 4. マイグレーション

```bash
# 順方向に全 migration を適用
make migrate-up

# 1 ステップだけ巻き戻す
make migrate-down
```

> 本 Issue（#1）時点では `backend/db/migrations/` 配下に migration ファイル本体は
> ありません。テーブル定義の追加は umbrella task 2.3 で行われます。Makefile のターゲット
> 自体は動作することを確認できます（migration ディレクトリが空の場合、`migrate-up` は
> "no change" を返します）。

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
