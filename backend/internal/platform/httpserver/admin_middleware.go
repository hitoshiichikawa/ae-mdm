package httpserver

import (
	"net/http"

	internalerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
)

// RequireSuperAdmin は `/api/admin/*` 配下に固定で挟む SuperAdmin ガード middleware。
//
// 動作（requirements.md Req 5.4 / 5.5 / design.md Components: HTTP Server Bootstrap
// 節「`/api/admin/*` 配下には SuperAdmin ガードを router group の `Use()` で固定して挟む」
// と整合）:
//   - TenantContext が ctx に確立されていない（CodeTenantCtxMissing）→ **401**
//     `*errors.Error{Code: CodeUnauthenticated}` で応答（本 Issue 範囲ではこの経路は
//     通常 TenantContextMiddleware で先に 401 になるが、admin 専用 middleware 単体でも
//     fail-closed で動作するように本ガードを内包する）
//   - TenantContext あり + IsSuperAdmin=false → **403**
//     `*errors.Error{Code: CodeForbidden}` で応答（Req 5.5 の「対象リソースの存在を露出しない」
//     ため body には対象 ID 等を含めない）
//   - TenantContext あり + IsSuperAdmin=true → next.ServeHTTP に通過
//
// 本 Issue では auth スタブが claims を ctx に注入しない default deny 状態のため、
// 通常運用では 401 で先に弾かれる。後続 Issue で OIDC 認証 + RBAC が配線された後、
// 本 middleware は SuperAdmin チェックの正典として動作し続ける（実 RBAC への差し替えは
// 後続 Issue で本関数の signature を保ったまま行う想定）。
func RequireSuperAdmin(log logger.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tc, err := db.FromContext(r.Context())
			if err != nil {
				// TenantContext 未確立 → 401。
				internalerrors.WriteHTTP(w, r, internalerrors.New(
					internalerrors.CodeUnauthenticated,
					"authentication required",
				), log)
				return
			}
			if !tc.IsSuperAdmin {
				// SuperAdmin ロールが無い → 403（対象リソースの存在は body に含めない）。
				internalerrors.WriteHTTP(w, r, internalerrors.New(
					internalerrors.CodeForbidden,
					"super admin role required",
				), log)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
