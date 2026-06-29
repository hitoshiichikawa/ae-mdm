// Package tenantaudit は tenant ドメインの監査イベントを監査ログ Service へ橋渡しする
// domain-glue（アダプタ）パッケージ。
//
// tenant パッケージ（`internal/tenant`）と audit パッケージ（`internal/audit`）の双方を import
// するため、core ドメイン同士に相互 import を持ち込まないよう独立パッケージとして切り出す
// （#38 design「DI 配線は将来 main の責務」と整合 / requirements Req 5）。`package main`
// （cmd/api）へ置くと単体テスト不能になるため、unit-test 可能な独立パッケージにする。
//
// 本パッケージの責務は「tenant.Event から audit.Event への変換」と「audit.Service への委譲」の
// 2 点に限定し、再試行・抑制・フィルタリング等の追加判断は持たない（Req 5.2）。
package tenantaudit

import (
	"context"

	"github.com/hitoshiichikawa/ae-mdm/internal/audit"
	"github.com/hitoshiichikawa/ae-mdm/internal/tenant"
)

// 監査イベント種別の正規語彙（Req 2.4〜2.7 / NFR 1.2）。
//
// tenant 監査閲覧の絞り込み条件として一貫利用できる固定値に保つため定数化する
// （マジック文字列の散在を避ける / NFR 1.2）。
const (
	// EventTypeCreate はテナント作成操作の監査イベント種別（Req 2.4）。
	EventTypeCreate audit.EventType = "tenant_create"
	// EventTypeBind は Enterprise バインド操作の監査イベント種別（Req 2.5）。
	EventTypeBind audit.EventType = "tenant_bind"
	// EventTypeDisable はテナント無効化操作の監査イベント種別（Req 2.6）。
	EventTypeDisable audit.EventType = "tenant_disable"
	// EventTypeUnknown は定義済み 3 種別のいずれにも一致しない未知操作の監査イベント種別
	// （Req 2.7）。無音破棄を避け、識別可能な種別として記録するための fallback 値。
	EventTypeUnknown audit.EventType = "tenant_unknown"
)

// Detail に載せる非機密フィールドの機械可読な鍵名（Req 4.2）。
const (
	// detailKeyConfirmationCompleted は二段階確認の完了有無を表す Detail 鍵（Req 4.2）。
	detailKeyConfirmationCompleted = "confirmation_completed"
	// detailKeyDenyReason は拒否理由（非機密の人間可読文言）を表す Detail 鍵（Req 4.2）。
	detailKeyDenyReason = "deny_reason"
)

// Recorder は tenant.EventRecorder を満たし、tenant 監査イベントを audit.Service へ委譲する
// アダプタ（Req 1 / Req 5.1）。
//
// 内部に audit.Service を保持し、Record 呼び出しごとに tenant.Event を audit.Event へ変換して
// audit.Service.Record へ渡す。永続化失敗は握りつぶさず呼び出し側へそのまま伝播する
// （fail-closed の射程 / Req 3.1 / 3.2）。
type Recorder struct {
	svc audit.Service
}

// NewRecorder は audit.Service を注入して Recorder を構築する。
//
// 引数 svc は tenant 監査イベントの永続化先となる監査ログ Service（`internal/audit` の
// Service interface）。返り値は tenant.EventRecorder を満たす *Recorder。副作用は持たない
// （構築のみ）。DI 配線（main.go）から audit.NewService の戻り値を渡して用いる（Req 5.3）。
func NewRecorder(svc audit.Service) *Recorder {
	return &Recorder{svc: svc}
}

// Record は tenant.EventRecorder の実装（Req 1 / Req 5.1）。
//
// 引数 e の tenant.Event を audit.Event へ変換し、audit.Service.Record へ委譲する。返り値は
// audit.Service.Record が返した error を **そのまま伝播** する（アダプタ境界で握りつぶさない
// / Req 3.1 / 3.2 fail-closed）。副作用は audit.Service 経由の永続化要求（audit_logs への追記）。
//
// audit.Event の ID / OccurredAt は設定せず zero のまま渡し、採番・現在時刻補完は audit.Service
// に委ねる（Req 2.9）。機密値は Detail に載せず、非機密フィールドのみを機械可読な鍵名で載せる
// （Req 4）。
func (r *Recorder) Record(ctx context.Context, e tenant.Event) error {
	ev := audit.Event{
		TenantID:   e.TenantID,
		ActorID:    e.Actor,
		EventType:  mapEventType(e.Operation),
		ResourceID: e.TenantID.String(),
		Detail:     buildDetail(e),
		Result:     mapResult(e.Result),
		// ID / OccurredAt は zero のまま（audit.Service が採番・clock 補完する / Req 2.9）。
	}
	return r.svc.Record(ctx, ev)
}

// mapEventType は tenant.Operation を audit.EventType へ写像する（Req 2.4〜2.7）。
//
// 定義済み 3 種別（create / bind / disable）に一致しない未知 Operation は EventTypeUnknown へ
// 写像し、無音破棄を避ける（Req 2.7）。
func mapEventType(op tenant.Operation) audit.EventType {
	switch op {
	case tenant.OperationCreate:
		return EventTypeCreate
	case tenant.OperationBind:
		return EventTypeBind
	case tenant.OperationDisable:
		return EventTypeDisable
	default:
		return EventTypeUnknown
	}
}

// mapResult は tenant.Result を audit.ResultType へ写像する（Req 2.3）。
//
// tenant.ResultFailure のみ audit.ResultFailure へ写像し、それ以外（success）は
// audit.ResultSuccess へ写像する。
func mapResult(result tenant.Result) audit.ResultType {
	if result == tenant.ResultFailure {
		return audit.ResultFailure
	}
	return audit.ResultSuccess
}

// buildDetail は tenant.Event の非機密フィールドのみを audit.Event.Detail へ写像する（Req 4）。
//
// 機密値（サインアップ URL の秘密パラメータ・SA 資格情報・OAuth トークンの生値）は tenant.Event
// が構造的に保持しないため、Detail へ持ち込む経路を作らない（一次防御に依拠 / Req 4.1 / 4.3）。
// 二段階確認の完了有無は常に載せる。拒否理由は非空のときのみ載せる。載せ得る非機密値が
// confirmation_completed のみで足りないケースは生じないため常に非 nil を返すが、将来 tenant.Event
// から ConfirmationCompleted が外れた場合に備え、何も載らなければ nil を返す（Req 4.4）。
func buildDetail(e tenant.Event) map[string]any {
	detail := map[string]any{
		detailKeyConfirmationCompleted: e.ConfirmationCompleted,
	}
	if e.DenyReason != "" {
		detail[detailKeyDenyReason] = e.DenyReason
	}
	if len(detail) == 0 {
		return nil
	}
	return detail
}
