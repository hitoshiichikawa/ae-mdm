package db

import (
	"context"

	"github.com/google/uuid"

	internalerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// TenantContext は 1 リクエスト（または 1 Pub/Sub message）につき 1 つだけ確立される
// 認可コンテキスト。requirements.md Req 5.2 / design.md Components: Tenant Context
// Middleware 節に対応する。
//
// SuperAdmin の cross-tenant 操作時のみ TenantID = uuid.Nil（IsSuperAdmin=true と組み合わせて
// `set_config('app.tenant_id', ...)` を発行しない経路に分岐する）。それ以外のリクエストでは
// TenantID は必ず有効な UUID を保持する invariant。
type TenantContext struct {
	TenantID     uuid.UUID
	AdminUserID  uuid.UUID
	Roles        []string
	IsSuperAdmin bool
}

// tenantContextKey は context.Context への put/get に用いる package-private な key 型。
// 外部 package から context を直接書き換える経路を物理的に塞ぐため非公開にしてある。
type tenantContextKey struct{}

// WithTenantContext は ctx に TenantContext を埋め込んだ新しい context を返す。
// HTTP middleware（後続 task 4.1）および Pub/Sub handler（後続 Issue）の両方から
// 共通で呼ばれる静的 API。
func WithTenantContext(ctx context.Context, tc TenantContext) context.Context {
	return context.WithValue(ctx, tenantContextKey{}, tc)
}

// FromContext は ctx に乗った TenantContext を取り出す。
// 未設定時は *errors.Error{Code: CodeTenantCtxMissing} を返す（requirements.md Req 4.5 /
// design.md Components: TxManager + RLS Helper の invariant と整合）。
func FromContext(ctx context.Context) (TenantContext, error) {
	if ctx == nil {
		return TenantContext{}, internalerrors.New(
			internalerrors.CodeTenantCtxMissing,
			"db: ctx が nil のため TenantContext を取り出せない",
		)
	}
	v := ctx.Value(tenantContextKey{})
	tc, ok := v.(TenantContext)
	if !ok {
		return TenantContext{}, internalerrors.New(
			internalerrors.CodeTenantCtxMissing,
			"db: ctx に TenantContext が確立されていない",
		)
	}
	return tc, nil
}
