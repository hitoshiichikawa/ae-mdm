package auth

// 本ファイルは auth.Service が返す `*errors.Error` の Cause チェーンに含める
// `failureKind` sentinel 定数を集約する。`failureKind` 型本体（および state.go 由来の
// state_invalid / state_expired / state_mismatch、repository_failure_kinds.go 由来の
// state_replay / admin_user_not_provisioned / session_tamper）は同 package 内で定義済み。
//
// Service が独自に追加する定数を別ファイルに分けるのは、state.go / repository.go の
// 責務を膨張させずに Service 固有の sentinel を追加するため（design.md / tasks.md の
// 設計判断と整合）。Handler / Middleware / Logger は `errors.As` / `errors.Is` で本定数を
// 識別し、構造化ログの `failure_kind` field に surface する（NFR 4.1 / design.md
// 「failure_kind ログフィールド一覧」）。

const (
	// FailureKindStateConsoleMismatch は state cookie 内 `Console` と callback handler の
	// console（URL パス由来）の不一致（Req 2.9 / 6.2 / design.md L1150）。401 にマッピング。
	FailureKindStateConsoleMismatch failureKind = "state_console_mismatch"

	// FailureKindInvalidAud は ID トークン `Claims.MatchedConsole` と handler の expected
	// console の不一致（Req 6.2 / design.md L1142）。Service レイヤで再 surface する（Verifier
	// 側でも同名 sentinel が存在するが、依存方向ルールにより auth package で独自に再定義）。
	FailureKindInvalidAud failureKind = "invalid_aud"

	// FailureKindNonceMismatch は ID トークン `nonce` クレームと `StatePayload.OIDCNonce`
	// の不一致（authorization code injection 防止 / OIDC Core 1.0 §3.1.2.7 / design.md L1151）。
	// 401 にマッピング。
	FailureKindNonceMismatch failureKind = "nonce_mismatch"

	// FailureKindCSPRNGFailure は session token 生成 (`crypto/rand.Read`) 失敗
	// （Req 3.5 / NFR 3.1 fail-closed / design.md L1159）。500 にマッピング。
	FailureKindCSPRNGFailure failureKind = "csprng_failure"

	// FailureKindUpstreamOIDCToken は OIDC token endpoint の 5xx / `id_token` 不在
	// （NFR 3.1 / design.md L1160）。502 にマッピング。
	FailureKindUpstreamOIDCToken failureKind = "upstream_oidc_token"

	// FailureKindConsoleMismatch は Session.Console と middleware の expectedConsole の
	// 不一致（Req 6.2 / 6.3 / design.md L1156）。LookupAndRefresh で 401 にマッピング。
	FailureKindConsoleMismatch failureKind = "console_mismatch"

	// FailureKindSessionExpired は absolute timeout 超過（Req 4.5 / design.md L1152）。
	// LookupAndRefresh で 401 にマッピング。
	FailureKindSessionExpired failureKind = "session_expired"

	// FailureKindSessionRevoked は revoked_at != nil（Req 5.3 / design.md L1154）。
	// LookupAndRefresh で 401 にマッピング。
	FailureKindSessionRevoked failureKind = "session_revoked"

	// FailureKindSessionIdle は idle timeout 超過（Req 4.4 / design.md L1153）。
	// LookupAndRefresh で 401 にマッピング。
	FailureKindSessionIdle failureKind = "session_idle"

	// FailureKindReturnToInvalid は `return_to` クエリパラメータ検証失敗
	// （open redirect 候補 / 確認事項 3 / design.md L767）。BeginLogin で 400 にマッピング。
	FailureKindReturnToInvalid failureKind = "return_to_invalid"
)
