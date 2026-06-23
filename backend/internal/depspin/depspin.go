// Package depspin は go.mod の require ディレクティブを保持するための placeholder。
//
// 本 Issue（#1）の時点ではドメインロジックを実装していないため、umbrella spec の
// Technology Stack（chi / pgx / golang-migrate / cloud.google.com/go/pubsub /
// google.golang.org/api/androidmanagement/v1 / coreos/go-oidc / zap）の各依存が
// `go mod tidy` で go.mod から削除されてしまう。後続 Issue（umbrella tasks 2.x 以降）で
// internal/platform/* / internal/auth/* / internal/notification/* 等で実利用されるまでの間、
// 本ファイルがブランクインポートで依存を保持する。
//
// このファイルは後続 Issue で各依存が実利用された時点で削除する想定（実装の進行に伴って
// 自然消滅する）。requirements.md Requirement 1.4 を満たすための一時措置。
package depspin

import (
	// chi: HTTP router（umbrella task 2.2 で利用）
	_ "github.com/go-chi/chi/v5"
	// pgx: PostgreSQL driver（umbrella task 2.2 で利用）
	_ "github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/pgxpool"
	// golang-migrate: マイグレーション（umbrella task 2.3 で利用）
	_ "github.com/golang-migrate/migrate/v4"
	// cloud.google.com/go/pubsub: 通知購読（umbrella task 6.x で利用）
	_ "cloud.google.com/go/pubsub"
	// google.golang.org/api/androidmanagement/v1: AMAPI クライアント（umbrella task 4.1 で利用）
	_ "google.golang.org/api/androidmanagement/v1"
	// coreos/go-oidc: OIDC ID トークン検証（umbrella task 3.1 で利用）
	_ "github.com/coreos/go-oidc/v3/oidc"
	// zap: 構造化ログ（umbrella task 2.1 で利用）
	_ "go.uber.org/zap"
)
