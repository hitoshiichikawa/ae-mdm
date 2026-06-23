# Requirements Document

## Introduction

本要件は ae-mdm（Android Enterprise EMM SaaS）MVP の最下層となる「プロジェクト骨格と
ローカル開発環境」を確立するためのものである。Umbrella Issue #24（`docs/specs/24-android-enterprise-emm-mvp/`）
の Task 1.1（リポジトリ scaffold）と Task 1.2（Docker Compose 構成）を 1 Issue に統合し、
後続の全 Issue（テナント基盤・認証認可・エンロール・ポリシー・デバイス・コマンド・アプリ配信・
通知）が「ローカルで一発起動して開発を始められる」状態を提供する。

本 Issue が成立するためには、(1) Go バックエンドと 2 SPA フロントエンド + 共有ライブラリの
ソースツリーがビルド可能、(2) Docker Compose で 7 サービスが起動し health check を通過、
(3) Keycloak の dev realm に **tenant-console / admin-console の 2 つの OIDC クライアント**
が分離して定義済み、(4) 起動・migrate・seed 手順が runbook に記述されていること、の 4 点を
満たす必要がある。ビジネスロジックは含まず、umbrella spec の Technology Stack と File
Structure Plan を物理的に具現化することにスコープを限定する。

## Requirements

### Requirement 1: バックエンド Go モジュールの初期化

**Objective:** As a 開発者, I want backend の Go module と最低限の依存が解決済みのソースツリー, so that 後続 Issue で internal/ 配下にドメイン実装を追加し始められる

#### Acceptance Criteria

1. The EMM Repository shall ルート直下に `backend/` ディレクトリを持ち、その配下に `go.mod` と `go.sum` を配置する
2. The EMM Repository shall `backend/go.mod` のモジュール名として `github.com/hitoshiichikawa/ae-mdm` を宣言する
3. The EMM Repository shall `backend/go.mod` に Go 1.22 以上を要求するディレクティブを記載する
4. The EMM Repository shall `backend/go.mod` に以下の依存を宣言する: chi（HTTP router）、pgx（PostgreSQL driver）、sqlc 関連（型安全 SQL 生成）、golang-migrate（DB マイグレーション）、cloud.google.com/go/pubsub（Pub/Sub クライアント）、googleapis の androidmanagement v1（AMAPI クライアント）、coreos/go-oidc（OIDC ID トークン検証）、zap（構造化ログ）
5. The EMM Repository shall `backend/cmd/api/` と `backend/cmd/worker/` の 2 つのエントリポイントディレクトリを持ち、それぞれに最小の `main.go` を配置する（main 関数は起動ログのみで終了して良いが、`go build ./...` でビルド可能であること）
6. When 開発者がリポジトリルートで `make build` を実行したとき, the EMM Repository shall backend の `go build ./...` を成功させる
7. If `backend/go.mod` のモジュール名や Go バージョン宣言が上記と異なるとき, the PM Review shall 本 Issue を未完了として扱う

### Requirement 2: フロントエンド 2 SPA + shared ワークスペースの初期化

**Objective:** As a 開発者, I want frontend に shared / tenant-console / admin-console の 3 パッケージが workspace として宣言され、各 SPA が独立してビルド可能な状態, so that 後続 Issue で共通基盤と各 SPA の画面を別 PR で追加できる

#### Acceptance Criteria

1. The EMM Repository shall ルート直下に `frontend/` ディレクトリを持ち、その配下に `shared/`, `tenant-console/`, `admin-console/` の 3 サブディレクトリを配置する
2. The EMM Repository shall リポジトリルートまたは `frontend/` 配下に、上記 3 パッケージを束ねる workspace 設定（pnpm workspaces または npm workspaces のいずれか）を 1 種類に決定して宣言する
3. The EMM Repository shall `frontend/shared/` を library 形式の package として定義し、`frontend/tenant-console/` および `frontend/admin-console/` からインポート可能な状態にする
4. The EMM Repository shall `frontend/tenant-console/package.json` および `frontend/admin-console/package.json` に、React 19・Vite・TypeScript・Tailwind CSS・shadcn/ui・TanStack Query・React Router・React Hook Form・Zod・oidc-client-ts への依存を宣言する
5. The EMM Repository shall `frontend/tenant-console/` および `frontend/admin-console/` の各々に対し、Vite による production build が成功する設定（`vite.config.ts`、`tsconfig.json`、`index.html`、最小の `src/main.tsx`）を提供する
6. When 開発者がリポジトリルートで `make build` を実行したとき, the EMM Repository shall frontend の 2 SPA に対する production build を順次成功させる
7. The EMM Repository shall `frontend/tenant-console/` 配下に SuperAdmin 専用画面のソースを含めず、`frontend/admin-console/` 配下にテナント業務 UI（端末操作・ポリシー編集等）のソースを含めない

### Requirement 3: ルート Makefile による開発ワークフロー集約

**Objective:** As a 開発者, I want ビルド・テスト・lint・マイグレーション・コンテナ起動を単一の Makefile から呼び出せる状態, so that 個別ツールの呼び出し手順を覚えずに開発フローを進められる

#### Acceptance Criteria

1. The EMM Repository shall リポジトリルートに `Makefile` を 1 ファイル配置する
2. The EMM Repository shall `Makefile` に `build`, `test`, `lint`, `migrate-up`, `migrate-down`, `up`, `down` の 7 ターゲットを定義する
3. When 開発者が `make build` を実行したとき, the EMM Repository shall backend と frontend 2 SPA のビルドを連続して実行し、いずれかが失敗した場合は非ゼロの exit code を返す
4. When 開発者が `make up` を実行したとき, the EMM Repository shall `docker compose up -d` 相当のコマンドを実行し、Requirement 5 で定義する 7 サービスを起動する
5. When 開発者が `make down` を実行したとき, the EMM Repository shall 起動済みの Docker Compose サービスを停止し、ローカルに残存するコンテナ・ネットワークを除去する
6. When 開発者が `make migrate-up` または `make migrate-down` を実行したとき, the EMM Repository shall golang-migrate を用いて `backend/db/migrations/` 配下のマイグレーションを適用または巻き戻すコマンドを実行する（本 Issue 時点で migration ファイル本体は空でも良いが、ターゲット自体は動作すること）

### Requirement 4: 環境変数テンプレート（.env.example）の整備

**Objective:** As a 開発者, I want 起動に必要な全環境変数の placeholder が `.env.example` に列挙された状態, so that ローカル起動時に未設定値を fail-fast で検出できる

#### Acceptance Criteria

1. The EMM Repository shall リポジトリルートに `.env.example` を 1 ファイル配置する
2. The EMM Repository shall `.env.example` に PostgreSQL 接続情報（接続 URL またはホスト・ポート・ユーザー・パスワード・データベース名）の placeholder を含める
3. The EMM Repository shall `.env.example` に **tenant-console 用 OIDC クライアント**（client_id・issuer URL・redirect URL）と **admin-console 用 OIDC クライアント**（client_id・issuer URL・redirect URL）の 2 セットを区別可能な変数名で含める
4. The EMM Repository shall `.env.example` に Cloud Pub/Sub 接続用変数（プロジェクト ID・トピック名・サブスクリプション名・`PUBSUB_EMULATOR_HOST` 切替変数）の placeholder を含める
5. The EMM Repository shall `.env.example` に Android Management API（AMAPI）呼び出しに必要な変数（サービスアカウント鍵パスまたは認証情報パス・プロジェクト ID）の placeholder を含める
6. The EMM Repository shall `.env.example` に管理者セッション暗号化用の秘密鍵（session secret）の placeholder を含める
7. The EMM Repository shall `.env.example` に監査ログ保持期間および端末同期遅延閾値の placeholder（数値設定）を含める
8. If `.env.example` に実値（実際の鍵・トークン・本番接続情報）が記載されているとき, the PM Review shall 本 Issue を未完了として扱う

### Requirement 5: Docker Compose による 7 サービス起動

**Objective:** As a 開発者, I want `docker compose up` 1 コマンドでローカル開発に必要な 7 サービスが起動・健全化する状態, so that 各 SPA のブラウザアクセスから backend を経由した DB / Pub/Sub / IdP 連携の手動疎通確認まで一気通貫で行える

#### Acceptance Criteria

1. The EMM Repository shall リポジトリルートに `docker-compose.yml` を 1 ファイル配置する
2. The EMM Repository shall `docker-compose.yml` に以下 7 サービスを定義する: `api`, `worker`, `tenant-console`, `admin-console`, `postgres`, `keycloak`, `pubsub-emulator`
3. The EMM Repository shall `api`, `worker`, `postgres`, `keycloak`, `pubsub-emulator` の各サービスに対して health check（コンテナの readiness を判定する `healthcheck` ディレクティブ）を定義する
4. The EMM Repository shall `api` および `worker` サービスの `depends_on` に `postgres`, `keycloak`, `pubsub-emulator` を指定し、依存サービスが健全化してから起動する順序制約を構成する
5. The EMM Repository shall `tenant-console` および `admin-console` サービスを別ポートで listen させ、各々独立した nginx コンテナとして配信する
6. The EMM Repository shall `tenant-console` および `admin-console` の各 nginx 設定で SPA fallback（ルーティング未マッチを `index.html` にフォールバック）を有効化し、`/api` から始まるパスを backend `api` サービスへリバースプロキシする
7. When 開発者が `make up` を実行したとき, the EMM Repository shall 上記 7 サービスをまとめて起動し、各 health check が通過するまでの間に他サービスへの起動失敗を伝播させない
8. The EMM Repository shall `docker-compose.yml` 内で本番認証情報を埋め込まず、認証情報は `.env` 経由で注入する構造とする

### Requirement 6: backend / frontend の Dockerfile 整備

**Objective:** As a 開発者, I want backend 2 プロセスと frontend 2 SPA がそれぞれ独立した Dockerfile で multi-stage build される状態, so that 後続の本番デプロイ（AWS Fargate 等）まで同一の build artifact 形式を流用できる

#### Acceptance Criteria

1. The EMM Repository shall `backend/Dockerfile.api` と `backend/Dockerfile.worker` の 2 ファイルを配置する
2. The EMM Repository shall `backend/Dockerfile.api` および `backend/Dockerfile.worker` を multi-stage build で構成し、最終ステージのベースイメージとして distroless を使用する
3. The EMM Repository shall `frontend/tenant-console/Dockerfile` と `frontend/admin-console/Dockerfile` の 2 ファイルを配置する
4. The EMM Repository shall `frontend/tenant-console/Dockerfile` および `frontend/admin-console/Dockerfile` を multi-stage build で構成し、Vite のビルド成果物を nginx ベースイメージで配信する
5. The EMM Repository shall `frontend/tenant-console/` および `frontend/admin-console/` の各々に対し、SPA fallback とリバースプロキシを宣言した `nginx.conf` を配置する
6. When 開発者が `make build` または `make up` を実行したとき, the EMM Repository shall 上記 4 つの Dockerfile を介したコンテナイメージビルドを成功させる

### Requirement 7: Keycloak dev realm の 2 OIDC クライアント定義

**Objective:** As a 開発者, I want Keycloak の dev realm に tenant-console と admin-console の 2 つの OIDC クライアントと 4 ロールのグループが事前定義された状態, so that ローカル起動直後から両 SPA の OIDC ログインフローを試験できる

#### Acceptance Criteria

1. The EMM Repository shall `infra/keycloak/realm-export.json` を 1 ファイル配置する
2. The EMM Repository shall `infra/keycloak/realm-export.json` に dev 用の realm 定義を 1 つ含める
3. The EMM Repository shall `infra/keycloak/realm-export.json` に **`tenant-console` クライアント**を 1 つ宣言し、tenant-console SPA からの OIDC PKCE 認証フローに対応する redirect URI を設定する
4. The EMM Repository shall `infra/keycloak/realm-export.json` に **`admin-console` クライアント**を 1 つ宣言し、admin-console SPA からの OIDC PKCE 認証フローに対応する redirect URI を設定する
5. The EMM Repository shall 上記 2 クライアントを別の `client_id` で識別可能とし、ID トークンの `aud` クレームでどちらの SPA 由来かを backend が判別できるよう構成する
6. The EMM Repository shall `infra/keycloak/realm-export.json` に SuperAdmin / TenantAdmin / Operator / Viewer の 4 ロールに対応するグループ定義を含める
7. When `keycloak` サービスが Docker Compose で起動したとき, the EMM Repository shall `realm-export.json` を Keycloak の import 機構で読み込ませる構成（コンテナの volume mount または環境変数による import 指定）を提供する

### Requirement 8: ローカル開発 runbook の整備

**Objective:** As a 新規参加者, I want ローカル起動・マイグレーション・seed の実行手順が 1 つの runbook にまとまっている状態, so that リポジトリクローンから開発開始までを口頭引き継ぎなしで達成できる

#### Acceptance Criteria

1. The EMM Repository shall `docs/runbook/local-dev.md` を 1 ファイル配置する
2. The EMM Repository shall `docs/runbook/local-dev.md` に、リポジトリクローン後に `.env.example` から `.env` を作成する手順を記載する
3. The EMM Repository shall `docs/runbook/local-dev.md` に `make build` および `make up` による起動手順を記載する
4. The EMM Repository shall `docs/runbook/local-dev.md` に `make migrate-up` および `make migrate-down` によるマイグレーション手順を記載する
5. The EMM Repository shall `docs/runbook/local-dev.md` に初期管理者（SuperAdmin）を seed する手順、または seed 機構が後続 Issue で導入される場合はその参照リンクを記載する
6. The EMM Repository shall `docs/runbook/local-dev.md` に tenant-console / admin-console / Keycloak / Pub/Sub emulator の各エンドポイント URL とポート番号を記載する

## Non-Functional Requirements

### NFR 1: 12-factor / ステートレス構成

1. The EMM Repository shall 全コンテナサービスについて、ホスト固有の状態（永続化ボリュームに依存しない設定）をコンテナ内部に持たず、設定は環境変数として注入する構成とする
2. The EMM Repository shall 認証情報（DB パスワード・OIDC client secret・サービスアカウント鍵）をリポジトリ管理対象のファイル（`docker-compose.yml`・Dockerfile・`realm-export.json`）に直接記載せず、`.env` 経由で注入する

### NFR 2: テナント分離の物理担保（OIDC クライアント分離）

1. The EMM Repository shall tenant-console と admin-console の OIDC クライアントを別 `client_id` として宣言し、callback URL・許可スコープ・ID トークン `aud` を分離することで、ID トークンの混在使用を物理的に防止する

### NFR 3: 起動時間と health check 収束

1. While `make up` 実行後の起動シーケンスが進行中であるとき, the EMM Repository shall 各サービスの health check が一定時間内（既定 60 秒以内、`docker-compose.yml` 内で個別チューニング可能）に healthy 状態に収束する設定を提供する

### NFR 4: イメージサイズ / 依存最小化

1. The EMM Repository shall backend の最終ステージイメージのベースとして distroless を使用し、ビルド用依存（コンパイラ・パッケージマネージャ）を最終ステージから除外する

## Out of Scope

- 各ドメイン（テナント / 認証認可 / エンロール / ポリシー / デバイス / コマンド / アプリ配信 / 監査ログ / 通知）のビジネスロジック実装は後続 Issue に分離する。本 Issue では `cmd/api/main.go` および `cmd/worker/main.go` は起動可能な最小コードに留め、ドメイン package（`internal/tenant/` 等）の実装は含めない
- DB マイグレーション本体（`backend/db/migrations/*.sql` の SQL 内容）は umbrella Task 2.3 で実装する。本 Issue では `migrate-up` / `migrate-down` のターゲット動作と空のマイグレーションディレクトリの設置までを範囲とする
- 初期 SuperAdmin の自動 seed CLI 実装は umbrella Task 14.2 で実装する。本 Issue では runbook での手順記載までを範囲とする
- frontend 各 SPA の画面実装（dashboard / devices / policies 等の features）は umbrella Task 12.x / 13.x で実装する。本 Issue では `main.tsx` と空の App コンポーネントまでを範囲とする
- 本番 AWS Fargate 用のインフラ定義（Terraform 等）は MVP のスコープ外とする
- CI（GitHub Actions）の workflow ファイル整備は本 Issue のスコープ外とする

## Open Questions

なし（umbrella spec で技術選定が確定済み、Issue 本文と既存設計の範囲で実装可能。Workspace ツール（pnpm vs npm）の選択は Architect / Developer の判断に委ねる）
