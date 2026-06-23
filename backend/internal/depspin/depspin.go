// Package depspin は go.mod の require ディレクティブを保持するための placeholder。
//
// 本 Issue（#2: 共通基盤）の時点で chi / pgx / pgxpool / golang-migrate / zap は
// internal/{config,logger,errors,platform/db,platform/httpserver} で実利用に移行し、
// 該当 blank import は本ファイルから削除済み。
//
// 残置されているのは以下 3 件で、いずれも後続 Issue（umbrella tasks 3.1 / 4.1 / 6.x）で
// 実利用に移行した時点で本ファイルから順次削除する前提:
//   - cloud.google.com/go/pubsub … 通知購読（umbrella task 6.x で利用）
//   - google.golang.org/api/androidmanagement/v1 … AMAPI クライアント（umbrella task 4.1）
//   - github.com/coreos/go-oidc/v3 … OIDC Verifier（umbrella task 3.1 で utilize / PR #31
//     round-3 review 由来。pinned 状態を維持して将来 Issue が re-add せずに済むようにする）
//
// requirements.md Req 1.4（go.mod の require が `go mod tidy` で削除されない）を満たす
// ための一時措置。
package depspin

import (
	// cloud.google.com/go/pubsub: 通知購読（umbrella task 6.x で利用）
	_ "cloud.google.com/go/pubsub"
	// github.com/coreos/go-oidc/v3/oidc: OIDC Verifier（umbrella task 3.1 で利用）
	_ "github.com/coreos/go-oidc/v3/oidc"
	// google.golang.org/api/androidmanagement/v1: AMAPI クライアント（umbrella task 4.1 で利用）
	_ "google.golang.org/api/androidmanagement/v1"
)
