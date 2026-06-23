//go:build tools

// Package tools は ae-mdm の generator / CLI tool 依存を `go.mod` に固定するための
// build-tag 付き package。ビルド時には `tools` タグが付かないため通常ビルドからは除外され、
// `go run` 経由でのみ呼び出される。
//
// 参考: https://go.dev/wiki/Modules#how-can-i-track-tool-dependencies-for-a-module
//
// 含めるツール:
//   - sqlc: backend/db/queries 配下の SQL から型安全な Go クライアントを生成する
//     （requirements.md Requirement 1.4 で go.mod への宣言を要求）
package tools

import (
	_ "github.com/sqlc-dev/sqlc/cmd/sqlc"
)
