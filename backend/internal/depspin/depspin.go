// Package depspin は go.mod の require ディレクティブを保持するための placeholder。
//
// 本 Issue（#2: 共通基盤）の時点で chi / pgx / pgxpool / golang-migrate / zap は
// internal/{config,logger,errors,platform/db,platform/httpserver} で実利用に移行し、
// 該当 blank import は本ファイルから削除済み。
//
// 残置されているのは以下 2 件で、いずれも後続 Issue（umbrella tasks 4.1 / 6.x）で
// 実利用に移行した時点で本ファイルから順次削除する前提:
//   - cloud.google.com/go/pubsub … 通知購読（umbrella task 6.x で利用）
//   - google.golang.org/api/androidmanagement/v1 … AMAPI クライアント（umbrella task 4.1）
//
// Issue #33（A3a OIDC Verifier + Session）で `github.com/coreos/go-oidc/v3` を
// `internal/platform/oidc` から **直接 import**（実利用）に移行したため、当該 blank import は
// 本ファイルから削除済み（task 7.1 で実施）。
//
// requirements.md Req 1.4（go.mod の require が `go mod tidy` で削除されない）を満たす
// ための一時措置。
package depspin

import (
	// cloud.google.com/go/pubsub: 通知購読（umbrella task 6.x で利用）
	_ "cloud.google.com/go/pubsub"
	// google.golang.org/api/androidmanagement/v1: AMAPI クライアント（umbrella task 4.1 で利用）
	_ "google.golang.org/api/androidmanagement/v1"
)
