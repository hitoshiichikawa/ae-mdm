// Package depspin は go.mod の require ディレクティブを保持するための placeholder。
//
// 本 Issue（#2: 共通基盤）の時点で chi / pgx / pgxpool / golang-migrate / zap /
// coreos/go-oidc は internal/{config,logger,errors,platform/db,platform/httpserver}
// で実利用に移行したため、本ファイルから blank import を削除した。
//
// 残置されているのは以下 2 件で、いずれも後続 Issue（umbrella tasks 4.1 / 6.x）で
// 実利用に移行した時点で **本ファイル全体を削除**して自然消滅させる前提:
//   - cloud.google.com/go/pubsub … 通知購読（umbrella task 6.x で utilize）
//   - google.golang.org/api/androidmanagement/v1 … AMAPI クライアント（umbrella task 4.1）
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
