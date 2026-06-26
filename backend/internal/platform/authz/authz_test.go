package authz

import (
	"testing"

	"github.com/google/uuid"
)

// testTenantA / testTenantB は表駆動テストで使う固定 UUID（test 内で再利用）。
var (
	testTenantA = uuid.MustParse("aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa")
	testTenantB = uuid.MustParse("bbbbbbbb-bbbb-4bbb-bbbb-bbbbbbbbbbbb")
)

// makeReq は same-tenant の典型 Request を組み立てる helper。
//
// テスト本文の noise を減らすために role / action / resource / audience のみを差し替え、
// 他フィールドは安全な default（同テナント / tenant-console）で埋める。
func makeReq(role Role, action Action, resource Resource) Request {
	return Request{
		Roles:           []string{string(role)},
		SessionTenantID: testTenantA,
		Audience:        AudienceTenantConsole,
		Action:          action,
		Resource:        resource,
		TargetTenantID:  testTenantA.String(),
	}
}

// TestAuthorize_Matrix_AllRoles_PrimaryActions は requirements.md Req 1.1 / 1.2 / 1.3 /
// 1.4 / 1.7 / 5.5 / NFR 2.2 の中核として、4 ロール × 主要 action × 主要 resource の
// 全組合せを表駆動で網羅する。design.md Permission Matrix と整合。
func TestAuthorize_Matrix_AllRoles_PrimaryActions(t *testing.T) {
	cases := []struct {
		name     string
		role     Role
		action   Action
		resource Resource
		audience Audience
		// tenant フィールドは下記で固定。targetTenantID 不一致 / SuperAdmin の
		// cross-tenant 経路は別テストで網羅する。
		wantAllow bool
	}{
		// ---- SuperAdmin: tenant: create/delete 可、policy:update / WIPE 不可 ----
		{"SuperAdmin tenant:create", RoleSuperAdmin, ActionCreate, ResourceTenant, AudienceAdminConsole, true},
		{"SuperAdmin tenant:delete", RoleSuperAdmin, ActionDelete, ResourceTenant, AudienceAdminConsole, true},
		{"SuperAdmin tenant:read", RoleSuperAdmin, ActionRead, ResourceTenant, AudienceAdminConsole, true},
		{"SuperAdmin policy:update_DENIED", RoleSuperAdmin, ActionUpdate, ResourcePolicy, AudienceAdminConsole, false},
		{"SuperAdmin policy:read", RoleSuperAdmin, ActionRead, ResourcePolicy, AudienceAdminConsole, true},
		{"SuperAdmin command:wipe_DENIED", RoleSuperAdmin, ActionWipe, ResourceCommand, AudienceAdminConsole, false},
		{"SuperAdmin audit_log:read", RoleSuperAdmin, ActionRead, ResourceAuditLog, AudienceAdminConsole, true},

		// ---- TenantAdmin: policy:create/update/delete 可、tenant:create/delete 不可 ----
		{"TenantAdmin policy:create", RoleTenantAdmin, ActionCreate, ResourcePolicy, AudienceTenantConsole, true},
		{"TenantAdmin policy:update", RoleTenantAdmin, ActionUpdate, ResourcePolicy, AudienceTenantConsole, true},
		{"TenantAdmin policy:delete", RoleTenantAdmin, ActionDelete, ResourcePolicy, AudienceTenantConsole, true},
		{"TenantAdmin policy:read", RoleTenantAdmin, ActionRead, ResourcePolicy, AudienceTenantConsole, true},
		{"TenantAdmin command:wipe", RoleTenantAdmin, ActionWipe, ResourceCommand, AudienceTenantConsole, true},
		{"TenantAdmin command:lock", RoleTenantAdmin, ActionLock, ResourceCommand, AudienceTenantConsole, true},
		{"TenantAdmin command:reboot", RoleTenantAdmin, ActionReboot, ResourceCommand, AudienceTenantConsole, true},
		{"TenantAdmin tenant:create_DENIED", RoleTenantAdmin, ActionCreate, ResourceTenant, AudienceTenantConsole, false},
		{"TenantAdmin tenant:delete_DENIED", RoleTenantAdmin, ActionDelete, ResourceTenant, AudienceTenantConsole, false},
		{"TenantAdmin app:update", RoleTenantAdmin, ActionUpdate, ResourceApp, AudienceTenantConsole, true},
		{"TenantAdmin audit_log:read", RoleTenantAdmin, ActionRead, ResourceAuditLog, AudienceTenantConsole, true},
		{"TenantAdmin enrollment:create", RoleTenantAdmin, ActionCreate, ResourceEnrollment, AudienceTenantConsole, true},

		// ---- Operator: lock/reboot 可、wipe 不可、policy:update 不可 ----
		{"Operator command:lock", RoleOperator, ActionLock, ResourceCommand, AudienceTenantConsole, true},
		{"Operator command:reboot", RoleOperator, ActionReboot, ResourceCommand, AudienceTenantConsole, true},
		{"Operator command:wipe_DENIED", RoleOperator, ActionWipe, ResourceCommand, AudienceTenantConsole, false},
		{"Operator policy:update_DENIED", RoleOperator, ActionUpdate, ResourcePolicy, AudienceTenantConsole, false},
		{"Operator policy:read", RoleOperator, ActionRead, ResourcePolicy, AudienceTenantConsole, true},
		{"Operator device:read", RoleOperator, ActionRead, ResourceDevice, AudienceTenantConsole, true},
		{"Operator app:update_DENIED", RoleOperator, ActionUpdate, ResourceApp, AudienceTenantConsole, false},
		{"Operator audit_log:read_DENIED", RoleOperator, ActionRead, ResourceAuditLog, AudienceTenantConsole, false},
		{"Operator enrollment:create", RoleOperator, ActionCreate, ResourceEnrollment, AudienceTenantConsole, true},

		// ---- Viewer: read のみ、書き込み全拒否 ----
		{"Viewer policy:read", RoleViewer, ActionRead, ResourcePolicy, AudienceTenantConsole, true},
		{"Viewer device:read", RoleViewer, ActionRead, ResourceDevice, AudienceTenantConsole, true},
		{"Viewer command:read", RoleViewer, ActionRead, ResourceCommand, AudienceTenantConsole, true},
		{"Viewer command:lock_DENIED", RoleViewer, ActionLock, ResourceCommand, AudienceTenantConsole, false},
		{"Viewer command:wipe_DENIED", RoleViewer, ActionWipe, ResourceCommand, AudienceTenantConsole, false},
		{"Viewer policy:update_DENIED", RoleViewer, ActionUpdate, ResourcePolicy, AudienceTenantConsole, false},
		{"Viewer enrollment:create_DENIED", RoleViewer, ActionCreate, ResourceEnrollment, AudienceTenantConsole, false},
		{"Viewer audit_log:read_DENIED", RoleViewer, ActionRead, ResourceAuditLog, AudienceTenantConsole, false},
	}

	a := New()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := makeReq(tc.role, tc.action, tc.resource)
			req.Audience = tc.audience
			got := a.Authorize(req)
			if got.Allowed != tc.wantAllow {
				t.Fatalf("Allowed = %v; want %v (reason=%q)", got.Allowed, tc.wantAllow, got.DenyReason)
			}
			if tc.wantAllow && got.DenyReason != DenyReasonNone {
				t.Errorf("allow 時の DenyReason = %q; want %q", got.DenyReason, DenyReasonNone)
			}
			if !tc.wantAllow && got.DenyReason == DenyReasonNone {
				t.Errorf("deny 時の DenyReason が zero value")
			}
			// Allowed=false 時の DenyReason は role_not_permitted が期待される
			// （未知 role / cross-tenant / unknown action / unknown resource は別ケース）
			if !tc.wantAllow && got.DenyReason != DenyReasonRoleNotPermitted {
				t.Errorf("deny 時の DenyReason = %q; want %q",
					got.DenyReason, DenyReasonRoleNotPermitted)
			}
		})
	}
}

// TestAuthorize_UnknownRole_FailsClosed は Req 1.5 を verify する。
//
// permissionMatrix に無い role 値（例: "Hacker"）は role unknown として fail-closed。
func TestAuthorize_UnknownRole_FailsClosed(t *testing.T) {
	a := New()
	req := makeReq(RoleViewer, ActionRead, ResourcePolicy)
	req.Roles = []string{"Hacker", "UnknownRole"}
	got := a.Authorize(req)
	if got.Allowed {
		t.Fatalf("unknown role が allow されてはならない: %+v", got)
	}
	if got.DenyReason != DenyReasonUnknownRole {
		t.Errorf("DenyReason = %q; want %q", got.DenyReason, DenyReasonUnknownRole)
	}
}

// TestAuthorize_UnknownAction_FailsClosed は Req 1.6（action 側）を verify する。
func TestAuthorize_UnknownAction_FailsClosed(t *testing.T) {
	a := New()
	req := makeReq(RoleSuperAdmin, ActionRead, ResourceTenant)
	req.Action = Action("xss_payload")
	got := a.Authorize(req)
	if got.Allowed {
		t.Fatalf("unknown action が allow されてはならない: %+v", got)
	}
	if got.DenyReason != DenyReasonUnknownAction {
		t.Errorf("DenyReason = %q; want %q", got.DenyReason, DenyReasonUnknownAction)
	}
}

// TestAuthorize_UnknownResource_FailsClosed は Req 1.6（resource 側）を verify する。
func TestAuthorize_UnknownResource_FailsClosed(t *testing.T) {
	a := New()
	req := makeReq(RoleSuperAdmin, ActionRead, ResourceTenant)
	req.Resource = Resource("ghost")
	got := a.Authorize(req)
	if got.Allowed {
		t.Fatalf("unknown resource が allow されてはならない: %+v", got)
	}
	if got.DenyReason != DenyReasonUnknownResource {
		t.Errorf("DenyReason = %q; want %q", got.DenyReason, DenyReasonUnknownResource)
	}
}

// TestAuthorize_UnknownAudience_FailsClosed は Req 6.5 を verify する。
func TestAuthorize_UnknownAudience_FailsClosed(t *testing.T) {
	a := New()
	req := makeReq(RoleSuperAdmin, ActionRead, ResourceTenant)
	req.Audience = Audience("evil-console")
	got := a.Authorize(req)
	if got.Allowed {
		t.Fatalf("unknown audience が allow されてはならない: %+v", got)
	}
	if got.DenyReason != DenyReasonAudienceMismatch {
		t.Errorf("DenyReason = %q; want %q", got.DenyReason, DenyReasonAudienceMismatch)
	}
}

// TestAuthorize_CrossTenant_AdminConsoleSuperAdmin_Allowed は Req 3.3 を verify する。
// admin-console aud + SuperAdmin role の場合、cross-tenant 操作は matrix で
// 当該 (role, action, resource) が allow なら allow される。
func TestAuthorize_CrossTenant_AdminConsoleSuperAdmin_Allowed(t *testing.T) {
	a := New()
	req := Request{
		Roles:           []string{string(RoleSuperAdmin)},
		SessionTenantID: uuid.Nil, // SuperAdmin context 慣習
		Audience:        AudienceAdminConsole,
		Action:          ActionRead,
		Resource:        ResourceAuditLog,
		TargetTenantID:  testTenantB.String(),
	}
	got := a.Authorize(req)
	if !got.Allowed {
		t.Fatalf("SuperAdmin + admin-console の cross-tenant audit_log:read が allow されない: %+v", got)
	}
}

// TestAuthorize_CrossTenant_NotAdminConsole_Denied は Req 3.4 を verify する。
// tenant-console aud では SuperAdmin role があっても cross-tenant は拒否。
func TestAuthorize_CrossTenant_NotAdminConsole_Denied(t *testing.T) {
	a := New()
	req := Request{
		Roles:           []string{string(RoleSuperAdmin)},
		SessionTenantID: testTenantA,
		Audience:        AudienceTenantConsole,
		Action:          ActionRead,
		Resource:        ResourceAuditLog,
		TargetTenantID:  testTenantB.String(),
	}
	got := a.Authorize(req)
	if got.Allowed {
		t.Fatalf("tenant-console aud で cross-tenant が allow されてはならない: %+v", got)
	}
	if got.DenyReason != DenyReasonCrossTenant {
		t.Errorf("DenyReason = %q; want %q", got.DenyReason, DenyReasonCrossTenant)
	}
}

// TestAuthorize_CrossTenant_NotSuperAdmin_Denied は Req 3.5 を verify する。
// admin-console aud でも SuperAdmin role がなければ cross-tenant は拒否。
func TestAuthorize_CrossTenant_NotSuperAdmin_Denied(t *testing.T) {
	a := New()
	req := Request{
		Roles:           []string{string(RoleTenantAdmin)},
		SessionTenantID: testTenantA,
		Audience:        AudienceAdminConsole,
		Action:          ActionRead,
		Resource:        ResourceDevice,
		TargetTenantID:  testTenantB.String(),
	}
	got := a.Authorize(req)
	if got.Allowed {
		t.Fatalf("非 SuperAdmin の cross-tenant が allow されてはならない: %+v", got)
	}
	if got.DenyReason != DenyReasonCrossTenant {
		t.Errorf("DenyReason = %q; want %q", got.DenyReason, DenyReasonCrossTenant)
	}
}

// TestAuthorize_TargetTenantID_EmptyOrInvalid_FailsClosed は Req 4.1 / 4.2 / 4.3 を verify する。
// 空文字 / 不正書式 / "00000000-..." はすべて fail-closed で拒否される。
func TestAuthorize_TargetTenantID_EmptyOrInvalid_FailsClosed(t *testing.T) {
	cases := []struct {
		name           string
		targetTenantID string
	}{
		{"empty", ""},
		{"malformed", "not-a-uuid"},
		{"partial-uuid", "aaaaaaaa-aaaa"},
		{"uuid-nil-string", uuid.Nil.String()},
	}
	a := New()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := Request{
				Roles:           []string{string(RoleSuperAdmin)},
				SessionTenantID: testTenantA,
				Audience:        AudienceAdminConsole,
				Action:          ActionRead,
				Resource:        ResourceTenant,
				TargetTenantID:  tc.targetTenantID,
			}
			got := a.Authorize(req)
			if got.Allowed {
				t.Fatalf("targetTenantID=%q が allow されてはならない: %+v", tc.targetTenantID, got)
			}
			if got.DenyReason != DenyReasonTargetTenantInvalid {
				t.Errorf("DenyReason = %q; want %q",
					got.DenyReason, DenyReasonTargetTenantInvalid)
			}
		})
	}
}

// TestAuthorize_TargetTenantID_FailClosed_PreemptsRoleAndAudience は Req 4.4 を verify する。
// SuperAdmin / admin-console であっても targetTenantID が空 / 不正なら拒否される。
func TestAuthorize_TargetTenantID_FailClosed_PreemptsRoleAndAudience(t *testing.T) {
	a := New()
	req := Request{
		Roles:           []string{string(RoleSuperAdmin)},
		SessionTenantID: uuid.Nil,
		Audience:        AudienceAdminConsole,
		Action:          ActionRead,
		Resource:        ResourceTenant,
		TargetTenantID:  "", // SuperAdmin でも fail-closed
	}
	got := a.Authorize(req)
	if got.Allowed {
		t.Fatalf("SuperAdmin / admin-console でも target 空文字なら拒否されるべき: %+v", got)
	}
	if got.DenyReason != DenyReasonTargetTenantInvalid {
		t.Errorf("DenyReason = %q; want %q", got.DenyReason, DenyReasonTargetTenantInvalid)
	}
}

// TestAuthorize_MultiRole_TakesMostPermissive は Req 5.5 を verify する。
// Operator + Viewer 兼務時、Viewer 単体では拒否される command:lock が Operator 経由で allow される。
func TestAuthorize_MultiRole_TakesMostPermissive(t *testing.T) {
	a := New()
	req := Request{
		Roles:           []string{string(RoleViewer), string(RoleOperator)},
		SessionTenantID: testTenantA,
		Audience:        AudienceTenantConsole,
		Action:          ActionLock,
		Resource:        ResourceCommand,
		TargetTenantID:  testTenantA.String(),
	}
	got := a.Authorize(req)
	if !got.Allowed {
		t.Fatalf("Operator 兼務時の command:lock が allow されるべき: %+v", got)
	}
}

// TestAuthorize_MultiRole_UnknownPlusKnown_KnownWins は Req 1.5 と 5.5 の境界を verify する。
// 未知 role 1 件 + 既知 role 1 件の組み合わせで、既知 role の許可結果が採用される。
func TestAuthorize_MultiRole_UnknownPlusKnown_KnownWins(t *testing.T) {
	a := New()
	req := Request{
		Roles:           []string{"GhostRole", string(RoleTenantAdmin)},
		SessionTenantID: testTenantA,
		Audience:        AudienceTenantConsole,
		Action:          ActionUpdate,
		Resource:        ResourcePolicy,
		TargetTenantID:  testTenantA.String(),
	}
	got := a.Authorize(req)
	if !got.Allowed {
		t.Fatalf("未知 + TenantAdmin の組合せで policy:update が allow されるべき: %+v", got)
	}
}

// TestAuthorize_DenyReasonsAreDistinct は Req 5.3 を verify する。
// Authorize の各拒否経路で DenyReason が呼び出し側で区別可能な異なる enum 値を返す。
func TestAuthorize_DenyReasonsAreDistinct(t *testing.T) {
	a := New()
	// ケース 1: role による deny（既知 role が matrix で deny 宣言）
	r1 := a.Authorize(makeReq(RoleViewer, ActionUpdate, ResourcePolicy))
	if r1.Allowed || r1.DenyReason != DenyReasonRoleNotPermitted {
		t.Errorf("role_not_permitted ケース失敗: %+v", r1)
	}
	// ケース 2: cross-tenant による deny
	r2 := a.Authorize(Request{
		Roles:           []string{string(RoleTenantAdmin)},
		SessionTenantID: testTenantA,
		Audience:        AudienceTenantConsole,
		Action:          ActionRead,
		Resource:        ResourceDevice,
		TargetTenantID:  testTenantB.String(),
	})
	if r2.Allowed || r2.DenyReason != DenyReasonCrossTenant {
		t.Errorf("cross_tenant ケース失敗: %+v", r2)
	}
	// ケース 3: audience 不一致による deny
	req3 := makeReq(RoleViewer, ActionRead, ResourcePolicy)
	req3.Audience = "evil"
	r3 := a.Authorize(req3)
	if r3.Allowed || r3.DenyReason != DenyReasonAudienceMismatch {
		t.Errorf("audience_mismatch ケース失敗: %+v", r3)
	}
	// ケース 4: targetTenantID fail-closed
	req4 := makeReq(RoleViewer, ActionRead, ResourcePolicy)
	req4.TargetTenantID = ""
	r4 := a.Authorize(req4)
	if r4.Allowed || r4.DenyReason != DenyReasonTargetTenantInvalid {
		t.Errorf("target_tenant_invalid ケース失敗: %+v", r4)
	}
	// ケース 5: unknown role fail-closed
	req5 := makeReq(RoleViewer, ActionRead, ResourcePolicy)
	req5.Roles = []string{"NoSuchRole"}
	r5 := a.Authorize(req5)
	if r5.Allowed || r5.DenyReason != DenyReasonUnknownRole {
		t.Errorf("unknown_role ケース失敗: %+v", r5)
	}
	// ケース 6: unknown action
	req6 := makeReq(RoleViewer, ActionRead, ResourcePolicy)
	req6.Action = "ghost"
	r6 := a.Authorize(req6)
	if r6.Allowed || r6.DenyReason != DenyReasonUnknownAction {
		t.Errorf("unknown_action ケース失敗: %+v", r6)
	}
	// ケース 7: unknown resource
	req7 := makeReq(RoleViewer, ActionRead, ResourcePolicy)
	req7.Resource = "ghost"
	r7 := a.Authorize(req7)
	if r7.Allowed || r7.DenyReason != DenyReasonUnknownResource {
		t.Errorf("unknown_resource ケース失敗: %+v", r7)
	}

	// 5 区分 + 2 fail-closed 区分の合計 7 種類が distinct であることを確認。
	seen := map[DenyReason]struct{}{
		r1.DenyReason: {}, r2.DenyReason: {}, r3.DenyReason: {},
		r4.DenyReason: {}, r5.DenyReason: {}, r6.DenyReason: {}, r7.DenyReason: {},
	}
	if len(seen) != 7 {
		t.Errorf("拒否理由 enum 値が distinct でない: %v", seen)
	}
}

// TestAuthorize_Deterministic_NoExternalIO は NFR 1.2 を verify する。
// 同一入力で繰り返し呼んでも同一結果が返り、副作用が発生しないこと。
func TestAuthorize_Deterministic_NoExternalIO(t *testing.T) {
	a := New()
	req := makeReq(RoleTenantAdmin, ActionWipe, ResourceCommand)
	first := a.Authorize(req)
	for i := 0; i < 100; i++ {
		got := a.Authorize(req)
		if got != first {
			t.Fatalf("iter %d: 同一入力で結果が変化した: first=%+v got=%+v", i, first, got)
		}
	}
}

// TestAuthorize_SameTenant_NoSuperAdmin_StillAllowedByMatrix は Req 3.2 の正常系を verify する。
// session.TenantID == targetTenantID なら role の matrix に従って allow される
// （SuperAdmin / admin-console でなくても OK）。
func TestAuthorize_SameTenant_NoSuperAdmin_StillAllowedByMatrix(t *testing.T) {
	a := New()
	req := Request{
		Roles:           []string{string(RoleOperator)},
		SessionTenantID: testTenantA,
		Audience:        AudienceTenantConsole,
		Action:          ActionLock,
		Resource:        ResourceCommand,
		TargetTenantID:  testTenantA.String(),
	}
	got := a.Authorize(req)
	if !got.Allowed {
		t.Fatalf("same-tenant Operator command:lock が allow されるべき: %+v", got)
	}
}
