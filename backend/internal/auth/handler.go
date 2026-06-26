package auth

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/oidc"
)

// Handler は OIDC ログイン / コールバック / ログアウトの 3 endpoint を提供する HTTP Handler。
//
// design.md「Auth Handler」節 / tasks.md task 5.2 と整合。tenant / admin の 2 系統で同一
// 構造を持つため、`Mount` を 2 回呼び分ける形（`/api/auth` を `ConsoleTenant`、
// `/api/admin/auth` を `ConsoleAdmin`）で 6 endpoint を提供する。`console` は Mount 時点で
// closure で固定し、HTTP request からは読み取らない（user-controlled な console 注入を物理的に
// 拒否する設計 / Req 6.2 / 6.4）。
//
// 機密値の非埋込契約（NFR 1.1 / NFR 4.2 / design.md「機密値を Cause メッセージ本文に
// 埋め込まない実装契約」節）: 本 Handler が wrap する error 文言・logger field 値に
// **state cookie 生値・session cookie 生値・id_token raw JWT・OIDC client secret を文字列
// 補間しない**。`errors.WriteHTTP` 経由の JSON body にも cookie / token 値を含めない。
type Handler struct {
	svc Service
	log logger.Logger
}

// NewHandler は本番用 Handler を構築する。Service は本番では `auth.NewService(...)` の戻り値。
func NewHandler(svc Service, log logger.Logger) *Handler {
	return &Handler{svc: svc, log: log}
}

// Mount は `consolePrefix` 配下に 3 つの route を sub-router で登録する。
//
// 構造（tasks.md task 5.2 詳細項目より）:
//
//	r.Route(consolePrefix, func(sub chi.Router) {
//	    sub.Get("/login", h.login(console))
//	    sub.Get("/callback", h.callback(console))
//	    sub.Post("/logout", h.logout(console))
//	})
//
// `r.Get("/login", ...)` のように root 相対で登録する誤実装をすると `consolePrefix` 配下
// （= `/api/auth/login`）ではなく root 直下（= `/login`）に登録されてしまい、Req 6.2 の
// path-based クライアント分離が成立しない。必ず `r.Route(consolePrefix, ...)` 経由で
// sub-router を作る。
//
// `console` は closure で各 handler に固定する。Mount を 2 度呼ぶことで tenant / admin の
// 2 系統で 6 endpoint をカバーする（呼び出し側責務）。
func (h *Handler) Mount(r chi.Router, consolePrefix string, console oidc.Console) {
	r.Route(consolePrefix, func(sub chi.Router) {
		sub.Get("/login", h.login(console))
		sub.Get("/callback", h.callback(console))
		sub.Post("/logout", h.logout(console))
	})
}

// login は `GET <consolePrefix>/login` の HTTP handler を返す。
//
// 動作:
//  1. `return_to` クエリを取得（不在は Service 側で default `/` に正規化される）
//  2. `service.BeginLogin(ctx, console, returnTo)` を呼ぶ
//  3. 成功時は `Set-Cookie: state_cookie` + 302 `Location: redirectURL`
//  4. エラー時は `errors.WriteHTTP` 経由で 400 等を返す
func (h *Handler) login(console oidc.Console) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		returnTo := r.URL.Query().Get("return_to")

		redirectURL, stateCookie, err := h.svc.BeginLogin(r.Context(), console, returnTo)
		if err != nil {
			pkgerrors.WriteHTTP(w, r, err, h.log)
			return
		}

		http.SetCookie(w, &stateCookie)
		w.Header().Set("Location", redirectURL)
		w.WriteHeader(http.StatusFound)
	}
}

// callback は `GET <consolePrefix>/callback` の HTTP handler を返す。
//
// 動作:
//  1. `code` / `state` クエリを取得
//  2. **両クエリの欠落判定**（`code == ""` || `queryState == ""` のいずれかが成立すれば
//     Service 呼び出し前に 400 `invalid_request` で即時 reject + state cookie 削除）
//  3. cookie から state cookie 値を取得（不在なら空文字 → Service 側で `state_invalid` 401）
//  4. `service.HandleCallback(ctx, console, code, queryState, rawStateCookie)`
//  5. 成功時: `Set-Cookie: session_cookie` + `Set-Cookie: state.ExpireCookieAttributes()` +
//     302 `Location: returnTo`
//  6. エラー時: `state.ExpireCookieAttributes()` を発行して state cookie を即時無効化 +
//     `errors.WriteHTTP` 経由で 401 / 502 等を返す（Req 2.8 / state replay 攻撃 surface 縮小）
//
// **`session.ExpireCookieAttributes()` を使わない**: 削除対象が `__Host-ae_mdm_session`
// になり state cookie が残置されて Req 2.8 違反になるため、必ず
// `ExpireCookieAttributes()`（state cookie 用）を使う。
func (h *Handler) callback(console oidc.Console) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		queryState := r.URL.Query().Get("state")

		// 入口判定: `code` / `state` のいずれか欠落で 400 invalid_request + state cookie 削除
		// （design.md API Contract /api/auth/callback Errors 列「400（return_to が不正 URL /
		// `code` 欠落 / `state` 欠落）」/ tasks.md task 5.2 (c2)(c3)）。Service 内に流すと
		// state_invalid (401) / oauth2 502 に化けて契約と矛盾するため必ず Handler 入口で判定する。
		if code == "" || queryState == "" {
			expireCookie := ExpireCookieAttributes()
			http.SetCookie(w, &expireCookie)
			err := pkgerrors.Wrap(
				pkgerrors.CodeInvalidRequest,
				"missing required query parameter (code or state)",
				FailureKindInvalidRequest,
			)
			pkgerrors.WriteHTTP(w, r, err, h.log)
			return
		}

		rawStateCookie := ""
		if c, cerr := r.Cookie(stateCookieName); cerr == nil {
			rawStateCookie = c.Value
		}

		rawSessionToken, sessionCookie, returnTo, err := h.svc.HandleCallback(
			r.Context(), console, code, queryState, rawStateCookie,
		)
		if err != nil {
			// エラー時も state cookie を即時無効化（Req 2.8 / state replay 攻撃 surface 縮小）
			expireCookie := ExpireCookieAttributes()
			http.SetCookie(w, &expireCookie)
			pkgerrors.WriteHTTP(w, r, err, h.log)
			return
		}

		_ = rawSessionToken // raw session token は cookie Value にのみ載せる（log には出さない）

		// 成功時: session cookie 発行 + state cookie 削除 + 302
		http.SetCookie(w, &sessionCookie)
		expireCookie := ExpireCookieAttributes()
		http.SetCookie(w, &expireCookie)
		w.Header().Set("Location", returnTo)
		w.WriteHeader(http.StatusFound)
	}
}

// logout は `POST <consolePrefix>/logout` の HTTP handler を返す。
//
// 動作:
//  1. cookie から session token を取得
//  2. cookie 不在は **401**（tasks.md L606「cookie 不在で 401」/ Req 5.2）
//  3. `service.Logout(ctx, rawSessionToken)` を呼ぶ（既に revoke 済みでも no-op）
//  4. 成功時: `Set-Cookie: session.ExpireCookieAttributes()` + 204 No Content
//
// `console` 引数は将来的なログ field 追加用に閉包で保持する（現状の Logout 経路では使用しない
// が、tasks.md の `h.logout(console)` シグネチャと整合）。
func (h *Handler) logout(console oidc.Console) http.HandlerFunc {
	_ = console // 将来のログ field 用に予約。現状の Logout 経路では console を使わない。
	return func(w http.ResponseWriter, r *http.Request) {
		c, cerr := r.Cookie(sessionCookieName)
		if cerr != nil || c == nil || c.Value == "" {
			// cookie 不在は 401（tasks.md L606 / Req 5.2）
			err := pkgerrors.Wrap(
				pkgerrors.CodeUnauthenticated,
				"session cookie missing",
				FailureKindSessionTamper,
			)
			pkgerrors.WriteHTTP(w, r, err, h.log)
			return
		}

		if err := h.svc.Logout(r.Context(), c.Value); err != nil {
			pkgerrors.WriteHTTP(w, r, err, h.log)
			return
		}

		// session cookie 削除（必ず session 用 helper を使う）
		expireCookie := SessionExpireCookieAttributes()
		http.SetCookie(w, &expireCookie)
		w.WriteHeader(http.StatusNoContent)
	}
}
