package auth

// 本ファイルは auth.Repository が返す `*errors.Error` の Cause チェーンに含める
// `failureKind` sentinel 定数を集約する。`failureKind` 型本体（および state.go 由来の
// state_invalid / state_expired / state_mismatch）は state.go で定義済み（同 package 内）。
//
// Repository が独自に追加する定数を別ファイルに分けるのは、state.go の責務（state cookie
// helper）を膨張させずに Repository 固有の sentinel を追加するため。Service / Logger は
// `errors.As` / `errors.Is` で本定数を識別し、構造化ログの `failure_kind` field に
// surface する（NFR 4.1 / design.md「failure_kind ログフィールド一覧」）。

const (
	// FailureKindStateReplay は state_nonces の PRIMARY KEY UNIQUE 制約違反
	// （pgerrcode 23505）が示す state nonce の再使用（Req 2.9 / replay 拒否）。
	FailureKindStateReplay failureKind = "state_replay"

	// FailureKindAdminUserNotProvisioned は (oidc_issuer, oidc_subject) 複合キーで
	// admin_users が 0 行（事前 provisioning が完了していない）/ Service が 403 にマッピング。
	FailureKindAdminUserNotProvisioned failureKind = "admin_user_not_provisioned"

	// FailureKindSessionTamper は sessions.token_hash 単独 lookup が 0 行
	// （cookie 改竄 / cookie 値の hash が DB と一致しない / Req 5.4）。Service / Middleware が
	// 401 にマッピング。
	FailureKindSessionTamper failureKind = "session_tamper"
)
