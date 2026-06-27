package audit

import "time"

// Clock は現在時刻を提供する DI 境界。
//
// audit.Service は本 interface 経由で時刻を取得することで、保持期間下限
// （retentionFloor = clock.Now() - retentionDays）の算出をテストで決定的にできる。
// design.md「Clock / failure_kinds（補助）」節と整合（手本: auth.Clock）。
type Clock interface {
	Now() time.Time
}

// SystemClock は time.Now() を呼び出す本番用の Clock 実装。
//
// 構築コストが無いため値型で提供し、呼び出し側はゼロ値（`audit.SystemClock{}`）を渡せる。
type SystemClock struct{}

// Now は time.Now() の戻り値をそのまま返す。
func (SystemClock) Now() time.Time { return time.Now() }
