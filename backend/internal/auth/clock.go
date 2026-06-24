package auth

import "time"

// Clock は現在時刻を提供する DI 境界。
//
// auth.Service / auth.Middleware は本 interface 経由で時刻を取得することで、テスト時に
// 任意の時刻を返す fake clock を差し込む（境界値テスト: idle 29:59 / 30:00 / 30:01、
// absolute 7:59:59 / 8:00:00 / 8:00:01 等）。design.md「Auth Service」節 / 「Auth Middleware」節
// と整合。
type Clock interface {
	Now() time.Time
}

// SystemClock は time.Now() を呼び出す本番用の Clock 実装。
//
// 構築コストが無いため値型で提供し、呼び出し側はゼロ値（`auth.SystemClock{}`）を渡せる。
type SystemClock struct{}

// Now は time.Now() の戻り値をそのまま返す。
func (SystemClock) Now() time.Time { return time.Now() }
