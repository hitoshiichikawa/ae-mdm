package device

import "time"

// Clock は現在時刻を提供する DI 境界。
//
// device.Service は本 interface 経由で時刻を取得することで、同期遅延判定
// （syncCutoff = Clock.Now() - 閾値）を fake clock でテスト決定的にできる（境界値: 閾値
// 直前 / ちょうど / 直後 / last_status_at=NULL / Req 4.1・4.3）。手本: auth.Clock / audit.Clock。
type Clock interface {
	Now() time.Time
}

// SystemClock は time.Now() を呼び出す本番用の Clock 実装。
//
// 構築コストが無いため値型で提供し、呼び出し側はゼロ値（`device.SystemClock{}`）を渡せる。
type SystemClock struct{}

// Now は time.Now() の戻り値をそのまま返す。
func (SystemClock) Now() time.Time { return time.Now() }
