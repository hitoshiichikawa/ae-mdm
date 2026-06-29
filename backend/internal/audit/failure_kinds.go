package audit

// failure_kind は構造化ログ（WARN）に出力する失敗種別の識別子（NFR 3.2）。
//
// 監査ログの追記または閲覧が認可・永続化エラーで失敗したとき、Service / Handler は
// `log.Warn("audit failure", "failure_kind", <kind>, ...)` 形式で失敗種別を識別可能に出力する。
// 機密値（detail の生値・query 生値・トークン等）はログ本文に補間しない（NFR 3.1）。
const (
	// FailureKindAuthzDenied は認可拒否（権限不足 / Operator・Viewer・非 SuperAdmin 等）を表す。
	FailureKindAuthzDenied = "authz_denied"
	// FailureKindParseInvalid はクエリ parse 失敗（from/to 非 RFC3339 / actor_id・tenant_id 非 uuid 等）を表す。
	FailureKindParseInvalid = "parse_invalid"
	// FailureKindPersistError は監査ログ追記（INSERT）の永続化エラーを表す。
	FailureKindPersistError = "persist_error"
	// FailureKindQueryError は監査ログ閲覧（SELECT / scan）のクエリエラーを表す。
	FailureKindQueryError = "query_error"
)
