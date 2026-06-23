# ae-mdm 開発ワークフロー Makefile
# requirements.md Requirement 3（build / test / lint / migrate-up / migrate-down / up / down）
#
# 各ターゲットの責務:
#   build          backend と frontend 2 SPA をビルド
#   test           backend (go test) と frontend (vitest) を実行
#   lint           backend (go vet) と frontend (npm lint) を実行
#   migrate-up     golang-migrate で backend/db/migrations を順方向適用
#   migrate-down   golang-migrate で migrations を 1 ステップ巻き戻し
#   up             docker compose up -d で 7 サービスを起動
#   down           docker compose down で停止 + コンテナ・ネットワーク除去
#
# 注意:
#   - GOTOOLCHAIN=local を指定して、Go 1.22 ローカルツールチェーンを優先利用する
#     （go.mod 依存解決で 1.25 を要求するキャッシュ汚染を回避）
#   - migrate-up / migrate-down は `go run` 経由で実行することで、ホストに golang-migrate
#     CLI を別途インストールしなくても動作する

.PHONY: help build test lint migrate-up migrate-down db-init-roles up down clean fmt

# DATABASE_URL は .env から取得（migrate ターゲットでのみ参照）
ifneq (,$(wildcard .env))
include .env
export
endif

help:
	@echo "ae-mdm Makefile"
	@echo ""
	@echo "Targets:"
	@echo "  build         backend と frontend 2 SPA をビルド"
	@echo "  test          backend (go test) と frontend (vitest) を実行"
	@echo "  lint          backend (go vet) と frontend (npm lint) を実行"
	@echo "  migrate-up    DB migration を順方向適用"
	@echo "  migrate-down  DB migration を 1 ステップ巻き戻し"
	@echo "  db-init-roles app_user / migration_user ロールを初期セットアップ"
	@echo "  up            docker compose で 7 サービスを起動"
	@echo "  down          docker compose 停止 + コンテナ・ネットワーク除去"
	@echo "  fmt           gofmt / prettier 相当を実行"
	@echo "  clean         build artifact を削除"

build: build-backend build-frontend

build-backend:
	@echo "==> backend: go build ./..."
	cd backend && GOTOOLCHAIN=local go build ./...

build-frontend:
	@echo "==> frontend: npm run build (workspaces)"
	npm run build

test: test-backend test-frontend

test-backend:
	@echo "==> backend: go test ./..."
	cd backend && GOTOOLCHAIN=local go test ./...

test-frontend:
	@echo "==> frontend: npm test"
	npm run test --workspaces --if-present

lint: lint-backend lint-frontend

lint-backend:
	@echo "==> backend: go vet ./..."
	cd backend && GOTOOLCHAIN=local go vet ./...

lint-frontend:
	@echo "==> frontend: npm run lint"
	npm run lint --workspaces --if-present

fmt:
	@echo "==> backend: gofmt -w ./..."
	cd backend && gofmt -w .

# golang-migrate を go run 経由で実行
# requirements.md 3.6: migrate-up / migrate-down ターゲット自体は動作すること
#   （本 Issue 時点で migration ファイル本体は空でも良い）
#
# DATABASE_URL は compose 内部 hostname (`postgres`) を指すためホスト実行では解決不能。
# ホストから `make migrate-*` する想定で、まず MIGRATE_DATABASE_URL を優先利用し、
# それも未設定なら DATABASE_URL にフォールバックする（後者は `docker compose exec` 等の
# 内部実行用途）。MIGRATE_DB_URL 自体は CLI からの上書きを許容する。
MIGRATE_PATH := backend/db/migrations
MIGRATE_DB_URL ?= $(or $(MIGRATE_DATABASE_URL),$(DATABASE_URL))

migrate-up:
	@echo "==> migrate up: $(MIGRATE_PATH)"
	@if [ -z "$(MIGRATE_DB_URL)" ]; then \
		echo "error: DATABASE_URL is not set (load .env first)"; exit 1; \
	fi
	cd backend && GOTOOLCHAIN=local go run -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate \
		-path db/migrations -database "$(MIGRATE_DB_URL)" up

migrate-down:
	@echo "==> migrate down (1 step): $(MIGRATE_PATH)"
	@if [ -z "$(MIGRATE_DB_URL)" ]; then \
		echo "error: DATABASE_URL is not set (load .env first)"; exit 1; \
	fi
	cd backend && GOTOOLCHAIN=local go run -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate \
		-path db/migrations -database "$(MIGRATE_DB_URL)" down 1

# db-init-roles: app_user / migration_user ロールを初期セットアップする
# （Issue #2 / task 3.3 + 3.4 / Req 6.5 / NFR 3.1）
#
# 注意:
#   - 本ターゲットは golang-migrate の管轄外。schema_migrations テーブルに記録されない
#     初期セットアップ SQL (backend/db/roles/0001_create_app_and_migration_roles.sql) を
#     postgres superuser 接続経由で適用する
#   - psql CLI を使う前提のレシピ。ホストに psql 未インストールの場合は runbook
#     (docs/runbook/local-dev.md) に記載の手動手順 (docker compose exec postgres psql ...) を参照
#   - 接続は postgres superuser（compose の POSTGRES_USER / POSTGRES_PASSWORD）で行う必要がある。
#     app_user / migration_user はまだ未作成のため、これらのロールで接続する fallback は
#     使えない。明示しなければ POSTGRES_HOST_PORT・POSTGRES_USER・POSTGRES_DB・POSTGRES_PASSWORD
#     から superuser 接続 URL を組み立てる
#   - 明示的に POSTGRES_INIT_URL を `.env` または CLI から渡せばそれが優先される
#   - パスワード placeholder (<REPLACE_ME_*>) は事前に sed / envsubst で実値に置換するか、
#     SQL ファイルを編集してから適用する想定（運用判断 / 本 SQL の冒頭コメント参照）
ROLES_PATH := backend/db/roles/0001_create_app_and_migration_roles.sql
POSTGRES_INIT_HOST ?= localhost
POSTGRES_INIT_PORT ?= $(or $(POSTGRES_HOST_PORT),5432)
POSTGRES_INIT_URL ?= postgres://$(POSTGRES_USER):$(POSTGRES_PASSWORD)@$(POSTGRES_INIT_HOST):$(POSTGRES_INIT_PORT)/$(POSTGRES_DB)?sslmode=disable

db-init-roles:
	@echo "==> db-init-roles: $(ROLES_PATH)"
	@if [ -z "$(POSTGRES_USER)" ] || [ -z "$(POSTGRES_PASSWORD)" ] || [ -z "$(POSTGRES_DB)" ]; then \
		echo "error: POSTGRES_USER / POSTGRES_PASSWORD / POSTGRES_DB のいずれかが未設定"; \
		echo "       .env をロードするか POSTGRES_INIT_URL を直接指定してください"; exit 1; \
	fi
	@if ! command -v psql >/dev/null 2>&1; then \
		echo "error: psql CLI が見つかりません。runbook (docs/runbook/local-dev.md) の手動手順を参照してください"; exit 1; \
	fi
	psql "$(POSTGRES_INIT_URL)" -v ON_ERROR_STOP=1 -f $(ROLES_PATH)

up:
	@echo "==> docker compose up -d"
	docker compose up -d

down:
	@echo "==> docker compose down (volumes + networks)"
	docker compose down --remove-orphans

clean:
	@echo "==> clean build artifacts"
	rm -rf backend/bin backend/out
	rm -rf frontend/*/dist
	rm -rf frontend/*/node_modules
