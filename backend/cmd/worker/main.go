// Package main は ae-mdm の worker プロセスのエントリポイント。
//
// worker は Cloud Pub/Sub の pull subscription を介して AMAPI からの通知
// （ENROLLMENT / STATUS_REPORT / COMMAND）を受信・処理する責務を持つが、本 Issue では
// scaffold のみで実装は後続 Issue（umbrella tasks 6.x）に委ねる。
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "ae-mdm worker: scaffold entrypoint (Issue #1). Pub/Sub subscriber to be added in subsequent issues.")
}
