package authz

// Role は ae-mdm の RBAC で扱う 4 ロールを表す型。
//
// design.md「Authorization (RBAC) Service」節および requirements.md Req 1.1 と整合。
// Identity（auth domain）の Roles フィールドに含まれる文字列値はそのまま本型の値として
// 解釈される（OIDC groups クレーム由来 / `internal/auth/types.go` の Roles と一致）。
type Role string

const (
	// RoleSuperAdmin は SaaS 運営者ロール。テナント作成・削除 / 全テナント横断監査ログ /
	// cross-tenant 操作（admin-console aud と組み合わせ）が許可される。
	RoleSuperAdmin Role = "SuperAdmin"
	// RoleTenantAdmin は顧客企業の管理者ロール。自テナント内のポリシー・WIPE を含む
	// 全コマンド・アプリカタログ更新・テナント内管理者管理が許可される。
	RoleTenantAdmin Role = "TenantAdmin"
	// RoleOperator はオペレータロール。LOCK / REBOOT のコマンド発行と参照系のみ。
	// WIPE は許可されない（design.md Permission Matrix）。
	RoleOperator Role = "Operator"
	// RoleViewer は参照専用ロール。読み取り系のみ。
	RoleViewer Role = "Viewer"
)

// knownRoles は本パッケージが認識する有効ロール集合。未知ロールの fail-closed 判定
// （Req 1.5）に使う。
var knownRoles = map[Role]struct{}{
	RoleSuperAdmin:  {},
	RoleTenantAdmin: {},
	RoleOperator:    {},
	RoleViewer:      {},
}

// isKnownRole は role が本パッケージで定義済みかを返す。未知 role は false。
func isKnownRole(r Role) bool {
	_, ok := knownRoles[r]
	return ok
}
