// Package main は ae-mdm の api プロセスのエントリポイント。
//
// 本 Issue（#1: プロジェクト骨格・Docker Compose）の時点では、umbrella spec の
// Technology Stack / File Structure Plan の物理化のみを目的としており、ドメインロジック
// （internal/tenant, internal/auth, internal/policy 等）の実装は後続 Issue で追加される。
//
// 現時点では起動ログを出して即時に exit する最小実装に留め、`go build ./...` が成功すること
// のみを保証する（requirements.md Requirement 1.5, 1.6 / Out of Scope）。
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "ae-mdm api: scaffold entrypoint (Issue #1). Domain logic to be added in subsequent issues.")
}
