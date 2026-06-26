package authz

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
)

// Audience は OIDC ID トークンの aud から判別したコンソール種別を表す型。
//
// `internal/platform/oidc.Console` と同じ値域（"tenant-console" / "admin-console"）を
// 持つが、authz パッケージは oidc パッケージへの依存を持たないため独立に再定義する
// （依存方向ルール / doc.go）。呼び出し側（admin middleware / 各 domain handler）は
// `oidc.Console` を `authz.Audience(value)` の型変換で渡す。
type Audience string

const (
	// AudienceTenantConsole は tenant-console 由来のセッション（顧客 IT 管理者向け）。
	AudienceTenantConsole Audience = "tenant-console"
	// AudienceAdminConsole は admin-console 由来のセッション（SaaS 運用者向け）。
	AudienceAdminConsole Audience = "admin-console"
)

// knownAudiences は未知 audience の fail-closed 判定（Req 6.5）に使う。
var knownAudiences = map[Audience]struct{}{
	AudienceTenantConsole: {},
	AudienceAdminConsole:  {},
}

// DenyReason は Authorizer.Authorize の deny 判定の区分を表す enum 型。
//
// requirements.md Req 5.3 の 5 区分（role による deny / cross-tenant 越境による deny /
// audience 不一致による deny / targetTenantID fail-closed / 未知の role / action /
// resource fail-closed）を canonical に表現する。Logger.Warn の構造化 field 値
// （`authz_deny_reason`）として直接 surface する（Req 7.2）。
type DenyReason string

const (
	// DenyReasonNone は allow 判定時に Decision.DenyReason が取る zero value。
	DenyReasonNone DenyReason = ""
	// DenyReasonRoleNotPermitted は role が permissionMatrix で当該 action × resource を
	// 許可されていない（Req 5.3 「role による deny」）。
	DenyReasonRoleNotPermitted DenyReason = "role_not_permitted"
	// DenyReasonCrossTenant は cross-tenant 操作（session.TenantID != targetTenantID）が
	// admin-console aud + SuperAdmin 以外の組み合わせで拒否される（Req 3.3 / 3.4 / 3.5）。
	DenyReasonCrossTenant DenyReason = "cross_tenant"
	// DenyReasonAudienceMismatch は audience が unknown 値（tenant-console / admin-console
	// 以外）の fail-closed 拒否（Req 6.5）。
	DenyReasonAudienceMismatch DenyReason = "audience_mismatch"
	// DenyReasonTargetTenantInvalid は targetTenantID が空 / 不正書式（uuid.Nil 含む）の
	// fail-closed 拒否（Req 4.1 / 4.2 / 4.3 / 4.4）。
	DenyReasonTargetTenantInvalid DenyReason = "target_tenant_invalid"
	// DenyReasonUnknownRole は input.Roles 全てが unknown 値（Req 1.5）。
	DenyReasonUnknownRole DenyReason = "unknown_role"
	// DenyReasonUnknownAction は action が permissionMatrix で 1 度も宣言されていない
	// （Req 1.6）。
	DenyReasonUnknownAction DenyReason = "unknown_action"
	// DenyReasonUnknownResource は resource が permissionMatrix で 1 度も宣言されていない
	// （Req 1.6）。
	DenyReasonUnknownResource DenyReason = "unknown_resource"
)

// Request は Authorizer.Authorize の入力契約（Req 5.1）。
//
// design.md「Authorization (RBAC) Service」節および requirements.md Req 5.1 と整合。
//
//   - Roles: 認証済みセッションの役割集合（Identity.Roles をそのまま渡す）。複数 role
//     兼務時は Req 5.5（最も寛容なロールの結果）で OR 評価する
//   - SessionTenantID: 認証済みセッション保有者の所属テナント（Identity.TenantID）。
//     SuperAdmin の場合は uuid.Nil（A2 の TenantContext 規約と整合）
//   - Audience: 認証済みセッションの aud（Session.Console をそのまま渡す）
//   - Action: 要求された動作
//   - Resource: 要求された対象
//   - TargetTenantID: 操作対象テナント識別子の文字列表現。空文字は「未指定」と解釈し
//     fail-closed で拒否する（Req 4.1 / 4.2）。UUID 不正書式も fail-closed（Req 4.3）
type Request struct {
	Roles           []string
	SessionTenantID uuid.UUID
	Audience        Audience
	Action          Action
	Resource        Resource
	TargetTenantID  string
}

// Decision は Authorizer.Authorize の返り値（Req 5.2）。
//
//   - Allowed: 許可結果。true なら呼び出し側は処理を続行、false なら拒否
//   - DenyReason: Allowed=false 時の区分（Req 5.3 / 7.2）。Allowed=true 時は DenyReasonNone
type Decision struct {
	Allowed    bool
	DenyReason DenyReason
}

// allow / deny は Decision を組み立てる internal helper。
func allow() Decision               { return Decision{Allowed: true, DenyReason: DenyReasonNone} }
func deny(reason DenyReason) Decision { return Decision{Allowed: false, DenyReason: reason} }

// Authorizer は許可判定の単一エントリポイント。
//
// 構築は New() で行う。状態を持たない（permissionMatrix への読み取り専用 lookup のみ）
// ため goroutine-safe（NFR 1.2）。
//
// 単一実装でテストもこれを直接使う（インターフェース抽象化は本 Issue 範囲外）。
type Authorizer struct{}

// New は Authorizer を構築する。本実装は状態を持たないため引数を受け取らない。
func New() *Authorizer {
	return &Authorizer{}
}

// Authorize は 1 回の許可判定を行う（Req 5.1 / 5.2 / 5.3 / 5.4 / 5.5）。
//
// 判定順序（fail-closed を優先的に成立させるため strict に固定）:
//
//  1. 未知 audience（tenant-console / admin-console 以外）→ DenyReasonAudienceMismatch
//     （Req 6.5）
//  2. 未知 action → DenyReasonUnknownAction（Req 1.6）
//  3. 未知 resource → DenyReasonUnknownResource（Req 1.6）
//  4. cross-tenant 操作の場合（SessionTenantID != normalizedTargetTenantID）:
//     - targetTenantID 不正書式 → DenyReasonTargetTenantInvalid（Req 4.x）
//     - audience != admin-console → DenyReasonCrossTenant（Req 3.4）
//     - SuperAdmin role 不在 → DenyReasonCrossTenant（Req 3.5）
//     - admin-console + SuperAdmin → continue to matrix（Req 3.3）
//  5. same-tenant 内の場合:
//     - targetTenantID 空文字 / 不正書式 → DenyReasonTargetTenantInvalid（Req 4.1 / 4.2 / 4.3）
//   - 例外: SessionTenantID が uuid.Nil（SuperAdmin の context）でも target が
//     未指定なら拒否（Req 4.4「SuperAdmin / admin-console であっても許可しない」）
//  6. matrix 判定: いずれかの **known role** で allow → allow（Req 5.5 / 最も寛容）
//  7. 全 known role で deny / 全 role が unknown → 区別:
//     - 全 role が unknown → DenyReasonUnknownRole（Req 1.5）
//     - 1 つ以上の known role があるが matrix で全 deny → DenyReasonRoleNotPermitted
//
// 本関数は外部 I/O を発生させない決定論的判定（NFR 1.2 / 3.1）。
func (a *Authorizer) Authorize(req Request) Decision {
	// 1. 未知 audience の fail-closed（Req 6.5）。
	if _, ok := knownAudiences[req.Audience]; !ok {
		return deny(DenyReasonAudienceMismatch)
	}
	// 2. 未知 action の fail-closed（Req 1.6）。
	if !isKnownAction(req.Action) {
		return deny(DenyReasonUnknownAction)
	}
	// 3. 未知 resource の fail-closed（Req 1.6）。
	if !isKnownResource(req.Resource) {
		return deny(DenyReasonUnknownResource)
	}

	// 4. targetTenantID の正規化と書式検証。
	targetID, targetErr := parseTargetTenantID(req.TargetTenantID)
	if targetErr != nil {
		return deny(DenyReasonTargetTenantInvalid)
	}
	// Req 4.4: targetTenantID が uuid.Nil（空文字も含む）なら role / aud に関わらず拒否。
	if targetID == uuid.Nil {
		return deny(DenyReasonTargetTenantInvalid)
	}

	// 5. cross-tenant 判定（Req 3.x）。
	if req.SessionTenantID != targetID {
		// admin-console aud かつ SuperAdmin の場合のみ cross-tenant 許可。
		if req.Audience != AudienceAdminConsole {
			return deny(DenyReasonCrossTenant)
		}
		if !hasKnownRole(req.Roles, RoleSuperAdmin) {
			return deny(DenyReasonCrossTenant)
		}
		// cross-tenant 許可後も resource 自体は matrix で判定する（同じ permissionMatrix を
		// 使う / Req 3.3 / 5.5）。SuperAdmin が全 resource を許可されているとは限らない
		// （例: SuperAdmin が WIPE を発行することは想定外なので matrix で deny）。
		// 以降 matrix 評価へ落ちる。
	}

	// 6. role 集合の matrix 評価（Req 5.5: 最も寛容なロールの結果を採用）。
	anyKnown := false
	for _, raw := range req.Roles {
		r := Role(raw)
		if !isKnownRole(r) {
			continue
		}
		anyKnown = true
		if isAllowedByMatrix(r, req.Action, req.Resource) {
			return allow()
		}
	}

	// 7. 全 known role で deny / 全 role が unknown → 区別。
	if !anyKnown {
		return deny(DenyReasonUnknownRole)
	}
	return deny(DenyReasonRoleNotPermitted)
}

// parseTargetTenantID は req.TargetTenantID（文字列）を uuid.UUID に正規化する。
//
// 空文字 → uuid.Nil + nil error を返す（呼び出し側で uuid.Nil を「未指定」と解釈し
// fail-closed に倒す / Req 4.1 / 4.2）。
//
// 不正書式 → uuid.Nil + non-nil error を返す（呼び出し側で error を見て
// DenyReasonTargetTenantInvalid に倒す / Req 4.3）。
//
// 有効 UUID → 当該 UUID + nil error を返す。
func parseTargetTenantID(s string) (uuid.UUID, error) {
	if s == "" {
		return uuid.Nil, nil
	}
	return uuid.Parse(s)
}

// hasKnownRole は roles 集合に want が含まれるかを返す。want が unknown role でも
// 文字列一致を判定するため、本関数自体は known/unknown を区別しない（呼び出し側で
// SuperAdmin 等の known role を渡す前提）。
func hasKnownRole(roles []string, want Role) bool {
	for _, raw := range roles {
		if Role(raw) == want {
			return true
		}
	}
	return false
}

// LogContext は Authorizer.AuthorizeAndLog が denied ログに付加する追加 context を表す。
//
// HTTP middleware / domain handler 配下で Authorizer を呼ぶ際、呼び出し元しか保持しない
// 識別子（request_id / actor_id / session_hash_prefix）を Authorizer の log field に乗せる
// ための入力契約。zero value は安全（フィールドは出力時に空文字判定で省略される）。
//
// requirements.md Req 7.3 / 7.4 / 7.5 の denied ログ field を満たすために用意する。
type LogContext struct {
	// RequestID は AccessLog と共通の相関 ID（X-Request-ID）。空文字なら省略。
	RequestID string
	// ActorID は AuthClaims.AdminUserID（操作主体）。空文字なら省略。
	ActorID string
	// SessionHashPrefix は AuthClaims.SessionHashPrefix（短縮 hash prefix のみ）。
	// raw token / 全 hash は受け取らない（NFR 1.1 / NFR 4.2）。空文字なら省略。
	SessionHashPrefix string
}

// AuthorizeAndLog は Authorize の結果を返しつつ、deny 判定時に構造化 WARN ログを出力する。
//
// requirements.md Req 7.1 / 7.2 / 7.3 / 7.4 / 7.5 の denied ログ契約を satisfy する公開
// エントリポイント。呼び出し側（domain handler 等）は本メソッドを使うことで roles /
// audience / action / resource / targetTenantID / authz_deny_reason / actor_id /
// session_hash_prefix / request_id を含む構造化ログを Authorizer 側で一元的に発行できる
// （呼び出し側で都度 log.Warn を組み立てる重複を避ける）。
//
// Decision は Authorize() の戻り値と同一（外部状態に依存しない決定論的判定 / NFR 1.2）。
// 本メソッドは Authorize の上に薄く logging を被せるだけで、判定ロジックは Authorize に
// 集約する。log == nil の場合はログ出力を skip（Authorize の戻り値のみ返す）。ctx は
// 将来 trace_id 等の context-bound field を載せるための拡張点として受け取る（現状未使用）。
//
// 機密値（raw token / id_token / state MAC 鍵 / 全 hash）は本メソッドが組み立てる log field に
// 含まれない契約（NFR 1.1 / NFR 4.2）。session_hash_prefix は呼び出し側が短縮済みで渡す。
func (a *Authorizer) AuthorizeAndLog(_ context.Context, log logger.Logger, lc LogContext, req Request) Decision {
	decision := a.Authorize(req)
	if decision.Allowed {
		return decision
	}
	if log == nil {
		return decision
	}
	fields := []any{
		"authz_deny_reason", string(decision.DenyReason),
		"audience", string(req.Audience),
		"action", string(req.Action),
		"resource", string(req.Resource),
		"target_tenant_id", req.TargetTenantID,
	}
	if len(req.Roles) > 0 {
		fields = append(fields, "roles", strings.Join(req.Roles, ","))
	}
	if req.SessionTenantID != uuid.Nil {
		fields = append(fields, "session_tenant_id", req.SessionTenantID.String())
	}
	if lc.RequestID != "" {
		fields = append(fields, "request_id", lc.RequestID)
	}
	if lc.ActorID != "" {
		fields = append(fields, "actor_id", lc.ActorID)
	}
	if lc.SessionHashPrefix != "" {
		fields = append(fields, "session_hash_prefix", lc.SessionHashPrefix)
	}
	log.Warn("authz denied", fields...)
	return decision
}
