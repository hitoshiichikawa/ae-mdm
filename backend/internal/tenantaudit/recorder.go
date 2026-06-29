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
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
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
//
// 加えて、永続化が成功したイベントについては結果を識別可能な構造化ログを出力する（NFR 2.1）。
// audit.Service は永続化「失敗」時のみ WARN（failure_kind=persist_error）を出すため、interim の
// LoggerRecorder から差し替えると成功経路の観測ログが失われる。本アダプタが成功経路の観測ログを
// 担い、監査経路全体（成功 = 本アダプタ / 失敗 = audit.Service）で結果が構造化ログに残ることを
// 保証する。ログ出力は観測性のための副作用であり、再試行・抑制・フィルタリング等の追加「判断」
// （Req 5.2 が禁ずるもの）は一切持たない（写像内容・委譲先・伝播 error を変えない）。
type Recorder struct {
	svc audit.Service
	log logger.Logger
}

// NewRecorder は audit.Service と logger を注入して Recorder を構築する。
//
// 引数 svc は tenant 監査イベントの永続化先となる監査ログ Service（`internal/audit` の
// Service interface）。引数 log は永続化成功時の観測ログ（NFR 2.1）出力先で、nil の場合は
// `logger.Default()`（未配線時は no-op logger）を採用し DI 未配線でも panic させない
// （`NewLoggerRecorder` / `amapi.NewClient` の nil-log フォールバックと同方針）。返り値は
// tenant.EventRecorder を満たす *Recorder。副作用は持たない（構築のみ）。DI 配線（main.go）から
// audit.NewService の戻り値と bootstrap 済み logger を渡して用いる（Req 5.3）。
func NewRecorder(svc audit.Service, log logger.Logger) *Recorder {
	if log == nil {
		log = logger.Default()
	}
	return &Recorder{svc: svc, log: log}
}

// Record は tenant.EventRecorder の実装（Req 1 / Req 5.1）。
//
// 引数 e の tenant.Event を audit.Event へ変換し、audit.Service.Record へ委譲する。返り値は
// audit.Service.Record が返した error を **そのまま伝播** する（アダプタ境界で握りつぶさない
// / Req 3.1 / 3.2 fail-closed）。副作用は (1) audit.Service 経由の永続化要求（audit_logs への
// 追記）と、(2) 永続化成功時の観測ログ出力（NFR 2.1）の 2 点。
//
// 永続化失敗時はログを出さずに error を伝播する。失敗経路の構造化ログ（failure_kind=
// persist_error の WARN）は audit.Service が担うため、ここで重複出力しない。永続化成功時のみ
// logPersisted で結果を識別可能な構造化ログを出力し、監査経路全体で成功・失敗いずれの結果も
// 構造化ログに残るようにする（NFR 2.1）。
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
	if err := r.svc.Record(ctx, ev); err != nil {
		return err
	}
	r.logPersisted(e)
	return nil
}

// logPersisted は永続化に成功した tenant 監査イベントの結果を構造化ログへ出力する（NFR 2.1）。
//
// 出力フィールドは operation / result / actor_id / tenant_id / confirmation_completed と、
// 拒否時のみ deny_reason に限定する（いずれも機密値を含まない / NFR 2.3）。tenant.Event は機密値
// フィールドを構造的に持たないため、安全フィールドのみを載せる一次防御に依拠する（Req 4 / #38 NFR 2.3）。
//
// **result 値も Warn / Info の level 判定も、audit row へ実際に永続化した結果（mapResult 後）に揃える。**
// raw な e.Result をそのまま載せると、unknown / zero 値が mapResult の fail-closed により audit row 上は
// failure なのに、ログ上は Info かつ result="" / "partial" となり、観測ログと監査証跡が乖離する
// （NFR 2.1 の結果識別と fail-closed の観測性が崩れる）。永続化済みだが結果が失敗（ResultFailure）の
// イベントは原因分析のため Warn、成功は Info で出力する。
func (r *Recorder) logPersisted(e tenant.Event) {
	// 永続化した audit row と同じ結果区分で観測ログを出す（mapResult と単一の真実源を共有する）。
	result := mapResult(e.Result)
	fields := []any{
		"operation", string(e.Operation),
		"result", string(result),
		logger.ActorID(e.Actor),
		logger.TenantID(e.TenantID),
		"confirmation_completed", e.ConfirmationCompleted,
	}
	if e.DenyReason != "" {
		fields = append(fields, "deny_reason", e.DenyReason)
	}
	if result == audit.ResultFailure {
		r.log.Warn("tenant audit event persisted", fields...)
		return
	}
	r.log.Info("tenant audit event persisted", fields...)
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
// **fail-closed（境界防御）**: 明示的な tenant.ResultSuccess のみ audit.ResultSuccess へ写像し、
// それ以外（tenant.ResultFailure / zero 値 / 未知の文字列値）はすべて audit.ResultFailure へ倒す。
// tenant.Result は string 型のため zero 値や未定義値を表現可能であり、これらを「成功」として
// 監査記録すると本当は失敗した操作が成功扱いで残るリスクがある。失敗を成功と誤認しない方向
// （= 未知値は failure 側）へ倒すのが安全側であるため、success を allow-list として扱う。
func mapResult(result tenant.Result) audit.ResultType {
	if result == tenant.ResultSuccess {
		return audit.ResultSuccess
	}
	return audit.ResultFailure
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
