package httpserver

import (
	"net/http"

	internalerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
)

// adminConsoleAudience は `/api/admin/*` ガードが許可する audience 値。
//
// `internal/platform/oidc.ConsoleAdmin` と同値だが、httpserver から oidc への直接依存を
// 避けるため文字列定数として独立に定義する。AuthClaims.Console（string）との直接比較で
// 判定する。
const adminConsoleAudience = "admin-console"

// authzDenyReason は `/api/admin/*` ガードが構造化ログに乗せる拒否理由の field 値。
//
// 値域は authz パッケージの DenyReason と意味的に並走するが、本層の関心は
// 「audience 不一致」「SuperAdmin 不在」「session 未確立」の 3 区分に限られるため、
// 重複定義を避けるために本ファイル内で固定の string リテラルを用いる。
const (
	authzDenyReasonSessionMissing      = "session_missing"
	authzDenyReasonAudienceMismatch    = "audience_mismatch"
	authzDenyReasonSuperAdminNotPresent = "super_admin_not_present"
)

// RequireSuperAdmin は `/api/admin/*` 配下に固定で挟む SuperAdmin ガード middleware。
//
// Issue #33 (A3a) で追加された当初実装は TenantContext の IsSuperAdmin のみで判定して
// いたが、Issue #37 (A3b) で audience（admin-console） の判定を別 middleware
// [RequireAdminConsoleAndSuperAdmin] に切り出した。
//
// 動作（requirements.md Req 5.4 / 5.5 / design.md Components: HTTP Server Bootstrap
// 節「`/api/admin/*` 配下には SuperAdmin ガードを router group の `Use()` で固定して挟む」
// と整合）:
//   - TenantContext が ctx に確立されていない（CodeTenantCtxMissing）→ **401**
//   - TenantContext あり + IsSuperAdmin=false → **403**
//   - TenantContext あり + IsSuperAdmin=true → next.ServeHTTP に通過
//
// Issue #37 以降は本 middleware よりも [RequireAdminConsoleAndSuperAdmin] を優先して
// 使うこと。本 middleware は audience 判定を行わないため、tenant-console aud で発行
// された SuperAdmin セッションを通過させる経路があり、要件 Req 2.4 を満たさない。
// 既存呼び出し箇所との後方互換のために残置する（後続 Issue で除去予定）。
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

// RequireAdminConsoleAndSuperAdmin は Issue #37 で追加された `/api/admin/*` ガード
// middleware。
//
// requirements.md Req 2.1〜2.7 / Req 6.1〜6.3 / NFR 1 / NFR 4 / design.md Components:
// HTTP Server Bootstrap 節「`/api/admin/*` 配下には admin-console aud かつ
// SuperAdmin の 2 条件 AND ガードを router group の `Use()` で固定して挟む」と整合。
//
// AuthClaims.Console と AuthClaims.IsSuperAdmin（Issue #33 で auth middleware が
// session lookup 成功時に注入）を見て判定する:
//
//   - AuthClaims が ctx に確立されていない → **401**
//     `*errors.Error{Code: CodeUnauthenticated}`（Req 2.6 / 6.3）
//   - AuthClaims.Console != "admin-console" → **403**
//     `*errors.Error{Code: CodeForbidden}`（Req 2.4: tenant-console aud で発行された
//     session を提示しても 403。body には対象リソース ID を含めない / Req 2.7 / 3.6）
//   - AuthClaims.IsSuperAdmin == false → **403**
//     `*errors.Error{Code: CodeForbidden}`（Req 2.5: 非 SuperAdmin の昇格試行を拒否）
//   - 両方 OK → next.ServeHTTP に通過（Req 2.3）
//
// 拒否時は log.Warn で `authz_deny_reason` / `console` / `role` 等の構造化 field を
// 出力する（Req 7.1 / 7.2）。本 middleware が出すログ field は以下:
//
//   - authz_deny_reason: "session_missing" / "audience_mismatch" / "super_admin_not_present"
//   - console: 提示された AuthClaims.Console（session 未確立時は省略）
//   - actor_id: AuthClaims.AdminUserID（session 未確立時は省略）
//   - request_id: AccessLog と共通の相関 ID
//
// `session_hash_prefix` は本層では取得できないため出さない（auth middleware が既に
// session_hash_prefix 付きの WARN を出している場合は重複しないよう本層は省略する /
// 二次的な観測 vs 既存 auth middleware ログの分離）。
func RequireAdminConsoleAndSuperAdmin(log logger.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := AuthClaimsFromContext(r.Context())
			if !ok {
				// 認証セッション未確立 → 401（Req 2.6 / 6.3）。
				logAdminAuthzDeny(log, r, "", "", authzDenyReasonSessionMissing)
				internalerrors.WriteHTTP(w, r, internalerrors.New(
					internalerrors.CodeUnauthenticated,
					"authentication required",
				), log)
				return
			}
			if claims.Console != adminConsoleAudience {
				// admin-console aud 以外 → 403（Req 2.4 / 2.7）。
				logAdminAuthzDeny(log, r, claims.Console,
					claims.AdminUserID.String(), authzDenyReasonAudienceMismatch)
				internalerrors.WriteHTTP(w, r, internalerrors.New(
					internalerrors.CodeForbidden,
					"admin console required",
				), log)
				return
			}
			if !claims.IsSuperAdmin {
				// 非 SuperAdmin → 403（Req 2.5 / 2.7）。
				logAdminAuthzDeny(log, r, claims.Console,
					claims.AdminUserID.String(), authzDenyReasonSuperAdminNotPresent)
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

// logAdminAuthzDeny は RequireAdminConsoleAndSuperAdmin の拒否経路で構造化 WARN ログを
// 出す helper（Req 7.1 / 7.2 / 7.4 / 7.5）。
//
// field 命名:
//   - authz_deny_reason: enum 値（session_missing / audience_mismatch / super_admin_not_present）
//   - console: 提示された audience（claims 不在時は空文字を省略）
//   - actor_id: AdminUserID（claims 不在時は空文字を省略）
//   - request_id: AccessLog と同じ X-Request-ID
//
// 機密値（session cookie 生値 / id_token / state MAC 鍵）は本ログに含めない
// （NFR 1.1 / NFR 4.2）。
func logAdminAuthzDeny(log logger.Logger, r *http.Request, console, actorID, reason string) {
	if log == nil {
		return
	}
	fields := []any{
		"authz_deny_reason", reason,
		"request_id", RequestIDFromContext(r.Context()),
		"path", r.URL.Path,
		"method", r.Method,
	}
	if console != "" {
		fields = append(fields, "console", console)
	}
	if actorID != "" {
		fields = append(fields, "actor_id", actorID)
	}
	log.Warn("admin authz denied", fields...)
}
