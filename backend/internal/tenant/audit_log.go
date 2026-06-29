package tenant

import (
	"context"

	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
)

// LoggerRecorder は EventRecorder の `internal/logger` 実装（interim binding）。
//
// Audit Service（audit_logs テーブルへの永続化 / umbrella #24 tasks 5.1）が未実装のため、
// 当面は構造化ログへ監査イベントを出力する暫定実装として用いる。Audit Service 実装後に
// 当該 Service の Recorder へ差し替える（DI 配線は将来 main の責務 / design.md
// 「tenant.EventRecorder」節）。`internal/platform/amapi` の `logOutcome` が手本。
//
// 機密値（サインアップ URL の秘密パラメータ・SA 資格情報・OAuth トークンの生値）は
// **Event の安全フィールドのみを出力することで構造的に排除する**（NFR 2.3）。Event 自体が
// 機密値フィールドを持たないため、logger 側の redact allowlist に依存しない一次防御を成す。
type LoggerRecorder struct {
	log logger.Logger
}

// NewLoggerRecorder は logger を注入して LoggerRecorder を構築する。
//
// log が nil の場合は `logger.Default()`（未配線時は no-op logger）を採用し、DI 未配線でも
// 監査記録呼び出しで panic させない（`amapi.NewClient` の nil-log フォールバックと同方針）。
func NewLoggerRecorder(log logger.Logger) *LoggerRecorder {
	if log == nil {
		log = logger.Default()
	}
	return &LoggerRecorder{log: log}
}

// Record は監査イベントの安全フィールドのみを構造化ログへ出力する（NFR 2.1 / 2.2 / 2.3）。
//
// 出力フィールドは operation / result / actor_id / tenant_id / confirmation_completed と、
// 拒否時のみ deny_reason に限定する（いずれも機密値を含まない）。失敗 / 拒否（ResultFailure）は
// 原因分析のため Warn、成功は Info で出力する。本 interim 実装は永続化を伴わないため常に
// nil を返す（将来の Audit Service 実装は永続化失敗を返し得る）。
func (r *LoggerRecorder) Record(_ context.Context, e Event) error {
	fields := []any{
		"operation", string(e.Operation),
		"result", string(e.Result),
		logger.ActorID(e.Actor),
		logger.TenantID(e.TenantID),
		"confirmation_completed", e.ConfirmationCompleted,
	}
	if e.DenyReason != "" {
		fields = append(fields, "deny_reason", e.DenyReason)
	}

	if e.Result == ResultFailure {
		r.log.Warn("tenant audit event", fields...)
		return nil
	}
	r.log.Info("tenant audit event", fields...)
	return nil
}
