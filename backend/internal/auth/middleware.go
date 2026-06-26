package auth

import (
	"net/http"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/httpserver"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/oidc"
)

// NewMiddleware は session lookup + console 照合 + AuthClaims ctx 注入を担う HTTP
// middleware を構築する。
//
// design.md「Auth Middleware」節 / tasks.md task 6.1 と整合。`expectedConsole` は本
// middleware インスタンスが守る console を closure に固定する。tenant 系 / admin 系で
// **別インスタンス**を構築することで、漏洩した tenant cookie が admin route に提示
// された場合に Service 側で `console_mismatch` で即拒否される経路（Req 6.2 / 6.3）を
// 物理的に成立させる。
//
// 動作:
//  1. `__Host-ae_mdm_session` cookie 取得（不在は default deny で 401 + cookie 削除 +
//     `failure_kind: session_tamper` で log.Warn）
//  2. `service.LookupAndRefresh(ctx, raw, expectedConsole, clock.Now())` を呼ぶ
//     （Service 側で console 照合 → absolute → revoked → idle の順で失効判定 / Req 4.x）
//  3. 失効時は session cookie 削除属性 (`SessionExpireCookieAttributes()`) を Set-Cookie で
//     発行 + `errors.WriteHTTP(401)` + `log.Warn` に `failure_kind` を含む構造化 field を出す
//     （NFR 4.1）
//  4. 成功時は `httpserver.AuthClaims{TenantID, AdminUserID, Roles, IsSuperAdmin}` を
//     `httpserver.WithAuthClaims(ctx, ...)` で ctx に注入 → `next.ServeHTTP`
//
// panic / DB error 等の想定外例外は **fail-closed**（NFR 3.1）。本 middleware は panic を
// 握りつぶさず外側 `httpserver.Recoverer` に委ねる（Recoverer が 500 + ERROR ログに写像する）。
//
// 機密値の非埋込契約（NFR 1.1 / NFR 4.2 / design.md「機密値を Cause メッセージ本文に
// 埋め込まない実装契約」節）: `log.Warn` の field 値および `errors.WriteHTTP` 経由の
// response body / header に **session cookie 生値・OIDC client secret・state MAC 鍵を
// 文字列補間しない**。本 middleware が field 化するのは `session_hash_prefix` /
// `failure_kind` / `console` の 3 種のみ。
//
// 注意: 本 middleware が削除する cookie は `__Host-ae_mdm_session`（session 用）のみ。
// state cookie 用の `ExpireCookieAttributes()` を使わない（cookie 名が異なり、削除対象が
// 取り違わるため）。`SessionExpireCookieAttributes()` を必ず使う（task 3.2 で確立した
// 命名規約 / impl-notes.md task 3.2 確認事項）。
func NewMiddleware(svc Service, expectedConsole oidc.Console, log logger.Logger, clock Clock) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 1. cookie 取得（不在は default deny で 401 + cookie 削除 + session_tamper ログ）
			c, cerr := r.Cookie(sessionCookieName)
			if cerr != nil || c == nil || c.Value == "" {
				deny(w, r, expectedConsole, "", FailureKindSessionTamper, "session cookie missing", log)
				return
			}

			rawToken := c.Value

			// 2. Service.LookupAndRefresh: console 照合 → absolute → revoked → idle の順で判定
			identity, _, err := svc.LookupAndRefresh(r.Context(), rawToken, expectedConsole, clock.Now())
			if err != nil {
				// failure_kind は err の Cause チェーンから抽出（Service が wrap 済み）
				kind := extractFailureKind(err)
				logSessionFailure(log, expectedConsole, rawToken, kind)
				expireCookie := SessionExpireCookieAttributes()
				http.SetCookie(w, &expireCookie)
				pkgerrors.WriteHTTP(w, r, err, log)
				return
			}

			// 3. 成功 → AuthClaims を ctx に注入して next に進める
			claims := httpserver.AuthClaims{
				TenantID:     identity.TenantID,
				AdminUserID:  identity.AdminUserID,
				Roles:        append([]string(nil), identity.Roles...),
				IsSuperAdmin: identity.IsSuperAdmin,
			}
			ctx := httpserver.WithAuthClaims(r.Context(), claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// deny は cookie 不在 / 改竄 等の middleware 入口判定での 401 経路を集約する。
//
// 1) `*errors.Error{Code: CodeUnauthenticated, failure_kind: <kind>}` を構築
// 2) `log.Warn` に `failure_kind` / `console` / 任意で `session_hash_prefix` を field 化
// 3) `Set-Cookie: SessionExpireCookieAttributes()` を発行
// 4) `errors.WriteHTTP` で 401 応答（response body には message のみ。機密値は含めない）
func deny(w http.ResponseWriter, r *http.Request, console oidc.Console, tokenHash string, kind failureKind, message string, log logger.Logger) {
	err := pkgerrors.Wrap(pkgerrors.CodeUnauthenticated, message, kind)
	logSessionFailureWithHash(log, console, tokenHash, string(kind))
	expireCookie := SessionExpireCookieAttributes()
	http.SetCookie(w, &expireCookie)
	pkgerrors.WriteHTTP(w, r, err, log)
}

// logSessionFailure は Service が返した error を log.Warn に変換する helper。
//
// `failure_kind` が空文字（extractFailureKind が拾えなかった場合）でも field は出力する
// （Logger 側で空値として記録される）。`session_hash_prefix` は **raw token の HashToken**
// 適用後の先頭 8 文字（Req 3.8）。生 token / 全 hash は出力しない（NFR 1.1 / 1.2）。
func logSessionFailure(log logger.Logger, console oidc.Console, rawToken, kind string) {
	if log == nil {
		return
	}
	fields := []any{
		"failure_kind", kind,
		"console", string(console),
	}
	if rawToken != "" {
		fields = append(fields, "session_hash_prefix", HashPrefix(HashToken(rawToken)))
	}
	log.Warn("auth failure", fields...)
}

// logSessionFailureWithHash は既に `HashToken` 済みの token_hash を直接受け取って WARN 出力する。
//
// cookie 不在パス（rawToken なし）でも session_hash_prefix を skip するためのオーバーロード。
// `tokenHash` が空文字なら prefix field 自体を省略する。
func logSessionFailureWithHash(log logger.Logger, console oidc.Console, tokenHash, kind string) {
	if log == nil {
		return
	}
	fields := []any{
		"failure_kind", kind,
		"console", string(console),
	}
	if tokenHash != "" {
		fields = append(fields, "session_hash_prefix", HashPrefix(tokenHash))
	}
	log.Warn("auth failure", fields...)
}
