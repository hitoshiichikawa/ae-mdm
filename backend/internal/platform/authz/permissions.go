package authz

// Action は許可マトリクスの「動作」軸を表す型。
//
// design.md「Authorization (RBAC) Service」節の Permission Matrix に対応。
// 値は文字列定数で、外部仕様（HTTP ルート命名等）とは独立した内部 vocabulary。
type Action string

const (
	// ActionRead は読み取り（list / get）操作。
	ActionRead Action = "read"
	// ActionCreate は新規作成操作。
	ActionCreate Action = "create"
	// ActionUpdate は更新操作。
	ActionUpdate Action = "update"
	// ActionDelete は削除操作。
	ActionDelete Action = "delete"
	// ActionLock は LOCK コマンド発行。
	ActionLock Action = "command:lock"
	// ActionReboot は REBOOT コマンド発行。
	ActionReboot Action = "command:reboot"
	// ActionWipe は WIPE コマンド発行（TenantAdmin のみ）。
	ActionWipe Action = "command:wipe"
)

// Resource は許可マトリクスの「対象」軸を表す型。
//
// design.md「Authorization (RBAC) Service」節 Permission Matrix の resource 列に対応。
type Resource string

const (
	// ResourceTenant はテナント entity（SuperAdmin の admin-console 専用操作）。
	ResourceTenant Resource = "tenant"
	// ResourceAdminUser は管理者ユーザー entity。本 Issue では HTTP ルートを実装しないが、
	// マトリクス上は permission を宣言する（後続 Issue で参照される）。
	ResourceAdminUser Resource = "admin_user"
	// ResourcePolicy はポリシー entity。
	ResourcePolicy Resource = "policy"
	// ResourceDevice は端末 entity。
	ResourceDevice Resource = "device"
	// ResourceCommand はコマンド entity（issue / list）。LOCK / WIPE / REBOOT 等の
	// 具体 action は ActionLock / ActionWipe / ActionReboot で区別する。
	ResourceCommand Resource = "command"
	// ResourceApp はアプリカタログ entity。
	ResourceApp Resource = "app"
	// ResourceAuditLog は監査ログ entity。
	ResourceAuditLog Resource = "audit_log"
	// ResourceEnrollment はエンロールメントトークン entity。
	ResourceEnrollment Resource = "enrollment_token"
)

// permissionKey は permissionMatrix の lookup key。
//
// O(1) 参照（NFR 3.1）のため (Role, Action, Resource) の triple を struct で集約する。
type permissionKey struct {
	Role     Role
	Action   Action
	Resource Resource
}

// permissionMatrix は 4 ロール × action × resource の許可関係を保持する単一定数テーブル
// （requirements.md Req 1.1 / 1.7 / NFR 2.1 / 2.2）。
//
// テーブルに **存在する entry のみが allow**。未登録の (Role, Action, Resource) は
// fail-closed で deny に倒れる（map index の ok=false 判定 / Req 1.4）。
//
// design.md「Authorization (RBAC) Service」節 Permission Matrix と整合する canonical な
// 宣言（design.md 側の表記揺れは本テーブルで吸収する）。matrix の意味:
//
//   - SuperAdmin: tenant create/delete / 全テナント横断 audit_log read / 全 resource 参照
//     （TenantAdmin / Operator / Viewer の許可を継承するわけではなく、本テーブルに明示）
//   - TenantAdmin: policy create/update/delete / WIPE / 全 command / app:update /
//     own-tenant audit_log:read / admin_user:create/update/delete（own tenant）
//   - Operator: command:lock / command:reboot / 全 read
//   - Viewer: 全 read のみ
//
// NOTE: 「same-tenant 内の判定のみ」を本テーブルが扱う。cross-tenant 判定は authz.go
// 側で別途行う（Req 3.x）。
var permissionMatrix = map[permissionKey]struct{}{
	// ---- SuperAdmin ----
	{RoleSuperAdmin, ActionCreate, ResourceTenant}:    {},
	{RoleSuperAdmin, ActionDelete, ResourceTenant}:    {},
	{RoleSuperAdmin, ActionRead, ResourceTenant}:      {},
	{RoleSuperAdmin, ActionUpdate, ResourceTenant}:    {},
	{RoleSuperAdmin, ActionCreate, ResourceAdminUser}: {},
	{RoleSuperAdmin, ActionUpdate, ResourceAdminUser}: {},
	{RoleSuperAdmin, ActionDelete, ResourceAdminUser}: {},
	{RoleSuperAdmin, ActionRead, ResourceAdminUser}:   {},
	{RoleSuperAdmin, ActionRead, ResourcePolicy}:      {},
	{RoleSuperAdmin, ActionRead, ResourceDevice}:      {},
	{RoleSuperAdmin, ActionRead, ResourceCommand}:     {},
	{RoleSuperAdmin, ActionRead, ResourceApp}:         {},
	{RoleSuperAdmin, ActionRead, ResourceAuditLog}:    {},
	{RoleSuperAdmin, ActionRead, ResourceEnrollment}:  {},

	// ---- TenantAdmin ----
	{RoleTenantAdmin, ActionRead, ResourcePolicy}:        {},
	{RoleTenantAdmin, ActionCreate, ResourcePolicy}:      {},
	{RoleTenantAdmin, ActionUpdate, ResourcePolicy}:      {},
	{RoleTenantAdmin, ActionDelete, ResourcePolicy}:      {},
	{RoleTenantAdmin, ActionRead, ResourceDevice}:        {},
	{RoleTenantAdmin, ActionRead, ResourceCommand}:       {},
	{RoleTenantAdmin, ActionCreate, ResourceCommand}:     {},
	{RoleTenantAdmin, ActionLock, ResourceCommand}:       {},
	{RoleTenantAdmin, ActionReboot, ResourceCommand}:     {},
	{RoleTenantAdmin, ActionWipe, ResourceCommand}:       {},
	{RoleTenantAdmin, ActionRead, ResourceApp}:           {},
	{RoleTenantAdmin, ActionUpdate, ResourceApp}:         {},
	{RoleTenantAdmin, ActionRead, ResourceAuditLog}:      {},
	{RoleTenantAdmin, ActionCreate, ResourceEnrollment}:  {},
	{RoleTenantAdmin, ActionRead, ResourceEnrollment}:    {},
	{RoleTenantAdmin, ActionCreate, ResourceAdminUser}:   {},
	{RoleTenantAdmin, ActionUpdate, ResourceAdminUser}:   {},
	{RoleTenantAdmin, ActionDelete, ResourceAdminUser}:   {},
	{RoleTenantAdmin, ActionRead, ResourceAdminUser}:     {},

	// ---- Operator ----
	{RoleOperator, ActionRead, ResourcePolicy}:       {},
	{RoleOperator, ActionRead, ResourceDevice}:       {},
	{RoleOperator, ActionRead, ResourceCommand}:      {},
	{RoleOperator, ActionLock, ResourceCommand}:      {},
	{RoleOperator, ActionReboot, ResourceCommand}:    {},
	{RoleOperator, ActionRead, ResourceApp}:          {},
	{RoleOperator, ActionCreate, ResourceEnrollment}: {},
	{RoleOperator, ActionRead, ResourceEnrollment}:   {},

	// ---- Viewer ----
	{RoleViewer, ActionRead, ResourcePolicy}:     {},
	{RoleViewer, ActionRead, ResourceDevice}:     {},
	{RoleViewer, ActionRead, ResourceCommand}:    {},
	{RoleViewer, ActionRead, ResourceApp}:        {},
	{RoleViewer, ActionRead, ResourceEnrollment}: {},
}

// knownActions / knownResources は未知値の fail-closed 判定（Req 1.6）に使う。
// permissionMatrix の宣言から導出。
var (
	knownActions   = buildKnownActions()
	knownResources = buildKnownResources()
)

func buildKnownActions() map[Action]struct{} {
	out := make(map[Action]struct{})
	for k := range permissionMatrix {
		out[k.Action] = struct{}{}
	}
	return out
}

func buildKnownResources() map[Resource]struct{} {
	out := make(map[Resource]struct{})
	for k := range permissionMatrix {
		out[k.Resource] = struct{}{}
	}
	return out
}

// isKnownAction は action が permissionMatrix で 1 度でも宣言されているかを返す。
func isKnownAction(a Action) bool {
	_, ok := knownActions[a]
	return ok
}

// isKnownResource は resource が permissionMatrix で 1 度でも宣言されているかを返す。
func isKnownResource(r Resource) bool {
	_, ok := knownResources[r]
	return ok
}

// isAllowedByMatrix は (role, action, resource) の triple が permissionMatrix に
// 宣言されているかを返す。本関数は same-tenant 内の判定のみを扱い、cross-tenant /
// targetTenantID fail-closed の判定は authz.go 側の責務。
func isAllowedByMatrix(role Role, action Action, resource Resource) bool {
	_, ok := permissionMatrix[permissionKey{Role: role, Action: action, Resource: resource}]
	return ok
}
