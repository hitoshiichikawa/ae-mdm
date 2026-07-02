package app

import (
	"encoding/json"
	stderrors "errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/authz"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/httpserver"
)

// Handler は `/api/play-tokens` / `/api/apps` / `/api/apps/sync` 3 endpoint の HTTP I/O +
// own-tenant RBAC 判定 + エラー写像を担う presentation 層（design.md「App Handler」節 /
// tasks.md task 5）。
//
// 各 endpoint は actor（操作実行者の admin_users.id）/ tenantID（所有テナント）を
// `httpserver.AuthClaimsFromContext` で取得し、`authz.Authorizer.AuthorizeAndLog`
// （`ResourceApp` × `ActionRead/Update`）で own-tenant RBAC を判定する（policy.Handler.authorize と
// 同方式）。deny は 403、claims 不在は防御的に 401 を返し、いずれも構造化 WARN を出す（Req 4.1）。
//
// JSON decode は `tenant.Handler` / `policy.Handler` の `decodeJSON` と同方式（malformed / 空
// body は 400）。Service が返す `*errors.Error`（未バインド 422 / AMAPI 502 / DB 503 / 未承認
// 422）は最外層で `errors.WriteHTTP` により Code → HTTP status + JSON body に写像し、Handler 側で
// 独自に status を組み立てない。存在差は sentinel / 汎用 message に委ね露出しない（Req 4.2 / NFR 1.2）。
//
// webToken の値（PlayTokenView.Value）は秘匿値であり、Handler も含めログ経路に載せない
// （NFR 1.2 / NFR 1.1 と整合）。`httpserver` を import するのは Handler のみで、Service へは
// actor / tenantID を引数で渡す（依存方向ルール / doc.go）。
//
// ルーティングは `tenant.Handler.Mount(r chi.Router)` パターンを採る（3 endpoint が別々の
// トップレベルパス `/play-tokens` / `/apps` / `/apps/sync` を持つため、単一 prefix 配下の
// sub-router には収めず、渡された router へ直接登録する）。cmd/api（task 6）が
// `appHandler.Mount(routers.API)` を呼ぶことで `/api/...` chain（TenantContextMiddleware による
// RLS tenant-scoped）配下で稼働する。
type Handler struct {
	svc        Service
	authorizer *authz.Authorizer
	log        logger.Logger
}

// NewHandler は本番用 Handler を構築する。
//
// svc は App ユースケース（本番では `app.NewService(...)` の戻り値）、authorizer は own-tenant
// `app` RBAC 判定、log は失敗パスの構造化 WARN ログに用いる。log が nil の場合は
// logger.Default()（未配線時は no-op）を採用し、DI 未配線でも `errors.WriteHTTP` のログ呼び出しで
// panic させない（policy.NewHandler / tenant.NewHandler の nil-log フォールバックと同方針）。
func NewHandler(svc Service, authorizer *authz.Authorizer, log logger.Logger) *Handler {
	if log == nil {
		log = logger.Default()
	}
	return &Handler{
		svc:        svc,
		authorizer: authorizer,
		log:        log,
	}
}

// Mount は渡された chi.Router へ 3 endpoint を root 相対で登録する
// （`tenant.Handler.Mount` パターン。3 endpoint が別々のトップレベルパスを持つため sub-router に
// まとめない）。
//
// 呼び出し側（cmd/api / task 6）が `appHandler.Mount(routers.API)` を呼ぶことで、`/api/play-tokens`
// （ActionRead）/ `/api/apps`（ActionRead）/ `/api/apps/sync`（ActionUpdate）が `/api` chain
// （RLS tenant-scoped）配下で稼働する（Req 4.1）。
//
//	r.Post("/play-tokens", h.createPlayToken)
//	r.Get("/apps", h.listApps)
//	r.Post("/apps/sync", h.syncApps)
func (h *Handler) Mount(r chi.Router) {
	r.Post("/play-tokens", h.createPlayToken)
	r.Get("/apps", h.listApps)
	r.Post("/apps/sync", h.syncApps)
}

// createPlayToken は `POST /api/play-tokens` の handler（iframe 表示用 webToken 発行 / Req 1.1 / 1.2）。
//
// webToken 発行は「承認 iframe を開く read 操作」であり ActionRead で判定する（design 対応表）。
//
//  1. AuthorizeAndLog で own-tenant `app read` 判定（deny は 403 / claims 不在は 401 / Req 4.1）
//  2. request body を PlayTokenRequest に decode（malformed / 空 body は 400 / Req 1.2）
//  3. Service.CreatePlayToken を actor / tenantID 付きで呼ぶ。parent_frame_url 空は 400、未バインドは
//     422、AMAPI 上流エラーは 502 を Service の写像（top-level Code）に委ねる（Req 1.1）
//  4. 成功時は PlayTokenView（{value}）を 200 で返す（design API Contract）
func (h *Handler) createPlayToken(w http.ResponseWriter, r *http.Request) {
	claims, ok := h.authorize(w, r, authz.ActionRead)
	if !ok {
		return
	}
	var in PlayTokenRequest
	if err := decodeJSON(r, &in); err != nil {
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}
	view, err := h.svc.CreatePlayToken(r.Context(), claims.AdminUserID, claims.TenantID, in)
	if err != nil {
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}
	// view.Value は秘匿値（NFR 1.2）。ログには載せず、レスポンス body でのみ呼び出し側へ返す。
	writeJSON(w, http.StatusOK, view)
}

// listApps は `GET /api/apps` の handler（自テナント承認済みカタログ一覧 / Req 2.1 / 2.3）。
//
// 参照系のため ActionRead で判定する。Service へは claims.TenantID（own-tenant）を渡し、RLS +
// tenant_id 述語で自テナント行のみを返す（他テナントのアプリを含めない / Req 2.3 / 4.2）。
// 0 件でも非 nil の空 slice を `[]` としてシリアライズする（Req 2.2 は Service 側で担保）。
func (h *Handler) listApps(w http.ResponseWriter, r *http.Request) {
	claims, ok := h.authorize(w, r, authz.ActionRead)
	if !ok {
		return
	}
	views, err := h.svc.ListApps(r.Context(), claims.TenantID)
	if err != nil {
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}
	writeJSON(w, http.StatusOK, views)
}

// syncApps は `POST /api/apps/sync` の handler（承認結果のカタログ同期 / Req 3.3）。
//
// 承認反映（書込）は更新系のため ActionUpdate で判定する（app:update を持たないロール〔Operator /
// Viewer〕は 403 / Req 4.1）。
//
//  1. AuthorizeAndLog で own-tenant `app update` 判定（deny は 403 / claims 不在は 401）
//  2. request body を SyncRequest に decode（malformed / 空 body は 400）
//  3. Service.SyncApps を actor / tenantID 付きで呼ぶ。未バインドは 422、DB 失敗は 503 を Service の
//     写像に委ねる（Req 4.2）
//  4. 成功時は SyncResult（{synced_at, count}）を 200 で返す（Req 3.3）
func (h *Handler) syncApps(w http.ResponseWriter, r *http.Request) {
	claims, ok := h.authorize(w, r, authz.ActionUpdate)
	if !ok {
		return
	}
	var in SyncRequest
	if err := decodeJSON(r, &in); err != nil {
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}
	result, err := h.svc.SyncApps(r.Context(), claims.AdminUserID, claims.TenantID, in)
	if err != nil {
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// authorize は claims を取得し own-tenant `app` RBAC 判定を行う共通前段（Req 4.1 / policy.authorize と
// 同方式）。
//
// claims 不在は防御的に 401（通常は TenantContextMiddleware が先行 401）。deny は 403。いずれの
// 拒否経路も AuthorizeAndLog（platform 層）/ logDeny（app 層）で原因属性を構造化 WARN に出す。
// allow の場合のみ (claims, true) を返す。
//
// TargetTenantID には claims.TenantID（own-tenant）を渡し、RBAC を自テナント境界に閉じる
// （cross-tenant は Authorizer が deny / Req 4.x）。
func (h *Handler) authorize(w http.ResponseWriter, r *http.Request, action authz.Action) (httpserver.AuthClaims, bool) {
	ctx := r.Context()
	claims, ok := httpserver.AuthClaimsFromContext(ctx)
	if !ok {
		// claims 不在は未認証として 401（通常は TenantContextMiddleware が先行）。
		h.logDeny(r, uuid.Nil, action, "missing auth claims")
		pkgerrors.WriteHTTP(w, r, pkgerrors.New(
			pkgerrors.CodeUnauthenticated,
			"authentication required",
		), h.log)
		return httpserver.AuthClaims{}, false
	}

	decision := h.authorizer.AuthorizeAndLog(ctx, h.log, authz.LogContext{
		RequestID:         httpserver.RequestIDFromContext(ctx),
		ActorID:           claims.AdminUserID.String(),
		SessionHashPrefix: claims.SessionHashPrefix,
	}, authz.Request{
		Roles:           claims.Roles,
		SessionTenantID: claims.TenantID,
		Audience:        authz.AudienceTenantConsole,
		Action:          action,
		Resource:        authz.ResourceApp,
		TargetTenantID:  claims.TenantID.String(),
	})
	if !decision.Allowed {
		h.logDeny(r, claims.TenantID, action, "authz denied")
		pkgerrors.WriteHTTP(w, r, pkgerrors.New(
			pkgerrors.CodeForbidden,
			"app operation forbidden",
		), h.log)
		return httpserver.AuthClaims{}, false
	}
	return claims, true
}

// logDeny は拒否経路（401 / 403）で原因属性を構造化 WARN に出す（policy.logDeny と同方針）。
//
// request_id / path / method / action / deny_reason の非機密 field のみ載せ、webToken.Value 等の
// 機密値は補間しない（NFR 1.2 / NFR 3.2）。tenantID が uuid.Nil（claims 不在）の場合は tenant_id
// field を省略する。log が nil の場合は no-op。
func (h *Handler) logDeny(r *http.Request, tenantID uuid.UUID, action authz.Action, reason string) {
	if h.log == nil {
		return
	}
	fields := []any{
		"deny_reason", reason,
		"action", string(action),
		"request_id", httpserver.RequestIDFromContext(r.Context()),
		"path", r.URL.Path,
		"method", r.Method,
	}
	if tenantID != uuid.Nil {
		fields = append(fields, logger.TenantID(tenantID))
	}
	h.log.Warn("app operation denied", fields...)
}

// decodeJSON は request body を v へ JSON decode する（policy.decodeJSON / tenant.decodeJSON と同方式）。
//
// decode 失敗（malformed JSON / 型不一致 / 空 body）は CodeInvalidRequest（400）を返す。先頭の 1 値を
// decode した後、後続トークン（例: `{} trailing`）が残る body も malformed として 400 を返す
// （単一 JSON document のみを正当な入力とする入力検証契約）。
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		return pkgerrors.New(pkgerrors.CodeInvalidRequest, "invalid request body")
	}
	if err := dec.Decode(&struct{}{}); !stderrors.Is(err, io.EOF) {
		return pkgerrors.New(pkgerrors.CodeInvalidRequest, "invalid request body")
	}
	return nil
}

// writeJSON は status code と JSON body を応答に書き出す共通ヘルパ（policy.writeJSON と同方式）。
// encode 失敗は header 送出済みのため諦める（`errors.WriteHTTP` の方針と同じ）。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
