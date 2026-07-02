package enrollment

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

// Handler は `/api/enrollment-tokens` の `POST`（発行）/ `GET`（一覧）の HTTP I/O +
// RBAC 判定 + エラー写像を担う presentation 層（design.md「enrollment.Handler」節 /
// tasks.md task 3 / Req 1.1 / 1.2 / 2.1 / 2.2 / 2.3 / 4.1）。
//
// 各 endpoint は actor（操作実行者の admin_users.id）/ tenantID（所有テナント）を
// `httpserver.AuthClaimsFromContext` で取得し、`authz.Authorizer.AuthorizeAndLog`
// （`ResourceEnrollment` × `ActionCreate`(POST) / `ActionRead`(GET)、`TargetTenantID=claims.TenantID`）で
// own-tenant RBAC を判定する。Viewer は permissionMatrix 上 `ActionCreate` を持たないため POST は
// 403（Req 2.2）、`ActionRead` は許可されるため GET は 200（`policy.Handler` を手本）。deny は 403、
// claims 不在は防御的に 401 を返し、いずれも構造化 WARN を出す（NFR 3.1 / NFR 4.1）。
//
// Service が返す `*errors.Error` は最外層で `errors.WriteHTTP` により Code → HTTP status + JSON body に
// 写像する（400 不正モード / policy 欠落・不在, 403 Viewer, 404 越境非露出, 502 AMAPI, 503 DB）。
// 越境（他テナント policy 指定）は Service/policyChecker 側で存在差非露出の NotFound（404）に倒れる
// （Req 2.3）。`httpserver` を import するのは Handler のみで、Service へは actor / tenantID を引数で
// 渡す（依存方向ルール / design Components）。
//
// `chi.Router` を内包することで http.Handler と chi.Router の双方を満たし、cmd/api（task 3）が
// `routers.API.Mount("/enrollment-tokens", handler)` で `/api/enrollment-tokens` を配線できる
// （chi.Mount 互換 / `policy.Handler` と同方式）。
type Handler struct {
	router     chi.Router
	svc        Service
	authorizer *authz.Authorizer
	log        logger.Logger
}

// NewHandler は本番用 Handler を構築する（policy.NewHandler と同方式の deps 注入）。
//
// svc は token 発行 / 一覧ユースケース、authorizer は own-tenant `enrollment_token` RBAC 判定、
// log は失敗パスの構造化 WARN ログ（NFR 4.1）に用いる。log が nil の場合は logger.Default()
// （未配線時は no-op）を採用し、DI 未配線でも `errors.WriteHTTP` のログ呼び出しで panic させない
// （policy.NewHandler / enrollment.NewService の nil-log フォールバックと同方針）。
//
// Mount("/enrollment-tokens", h) 配下で `/api/enrollment-tokens` を成立させるため、内包 router へ
// root 相対（`/`）で登録する（policy.Handler と同方式）。
func NewHandler(svc Service, authorizer *authz.Authorizer, log logger.Logger) *Handler {
	if log == nil {
		log = logger.Default()
	}
	h := &Handler{
		router:     chi.NewRouter(),
		svc:        svc,
		authorizer: authorizer,
		log:        log,
	}
	h.router.Post("/", h.issue)
	h.router.Get("/", h.list)
	return h
}

// ServeHTTP は http.Handler / chi.Router を満たす（内包 router に委譲する）。
//
// 呼び出し側（cmd/api / task 3）が `routers.API.Mount("/enrollment-tokens", h)` を呼ぶことで、
// chi の prefix strip 後に内包 router の root route が成立する（`/api/enrollment-tokens` で稼働）。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.router.ServeHTTP(w, r)
}

// issue は `POST /api/enrollment-tokens` の handler（発行 / Req 1.1 / 1.2 / 2.1 / 2.2 / 2.3）。
//
//  1. AuthorizeAndLog で own-tenant `enrollment_token create` 判定（Viewer は 403 / claims 不在は 401 / Req 2.2）
//  2. request body を IssueRequest に decode（malformed JSON は 400）
//  3. Service.IssueToken を呼ぶ。不正モード（400 / Req 1.4）、DEDICATED policy 欠落・越境不在
//     （400 / 404 非露出 / Req 1.5 / 2.3）、AMAPI 失敗（502）は Service の写像に委ねる
//  4. 成功時は TokenView（Value / QRCodeData を一度だけ含む）を 200 で返す（design API Contract）
func (h *Handler) issue(w http.ResponseWriter, r *http.Request) {
	claims, ok := h.authorize(w, r, authz.ActionCreate)
	if !ok {
		return
	}
	var in IssueRequest
	if err := decodeJSON(r, &in); err != nil {
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}
	// actor / tenantID は claims から取得して Service へ引数で渡す（own-tenant 境界 / Req 2.3）。
	// 呼び出し側が任意のテナントを指定する経路を設けないことで越境発行を構造的に排除する。
	view, err := h.svc.IssueToken(r.Context(), claims.AdminUserID, claims.TenantID, in)
	if err != nil {
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// list は `GET /api/enrollment-tokens` の handler（自テナント一覧 / Req 4.1）。read は ActionRead 判定。
//
// TokenSummary.Status は `expires_at` 由来（active / expired）のみを返す。使用済み（Req 4.2）は
// 既存 enrollment_tokens スキーマに消費列が無いため能動追跡せず、GET は expired のみ表示する
// （design リスク 2 / 無効トークンでの「未登録」observable は notification path が担保 / 検証は task 6）。
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	claims, ok := h.authorize(w, r, authz.ActionRead)
	if !ok {
		return
	}
	// 自テナント scoped: Service へ claims.TenantID を渡し、RLS に分離を委ねる（Req 2.3 / 4.1）。
	summaries, err := h.svc.ListTokens(r.Context(), claims.TenantID)
	if err != nil {
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}
	writeJSON(w, http.StatusOK, summaries)
}

// authorize は claims を取得し own-tenant `enrollment_token` RBAC 判定を行う共通前段（Req 2.1 / 2.2 / 2.3）。
//
// claims 不在は防御的に 401（通常は TenantContextMiddleware が先行 401）。deny は 403。いずれの拒否経路も
// AuthorizeAndLog（platform 層）/ logDeny（enrollment 層）で原因属性を構造化 WARN に出す（NFR 4.1）。
// allow の場合のみ (claims, true) を返す。
//
// TargetTenantID には claims.TenantID（own-tenant）を渡し、RBAC を自テナント境界に閉じる
// （cross-tenant は Authorizer が deny / Req 2.3）。
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
		Resource:        authz.ResourceEnrollment,
		TargetTenantID:  claims.TenantID.String(),
	})
	if !decision.Allowed {
		h.logDeny(r, claims.TenantID, action, "authz denied")
		pkgerrors.WriteHTTP(w, r, pkgerrors.New(
			pkgerrors.CodeForbidden,
			"enrollment operation forbidden",
		), h.log)
		return httpserver.AuthClaims{}, false
	}
	return claims, true
}

// logDeny は拒否経路（401 / 403）で原因属性を構造化 WARN に出す（NFR 4.1）。
//
// request_id / path / method / action / deny_reason の非機密 field のみ載せ、raw body 等の
// 機密値は補間しない（NFR 3.1）。tenantID が uuid.Nil（claims 不在）の場合は tenant_id field を
// 省略する。log が nil の場合は no-op。
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
	h.log.Warn("enrollment operation denied", fields...)
}

// decodeJSON は request body を v へ JSON decode する（policy.Handler.decodeJSON と同方式）。
//
// decode 失敗（malformed JSON / 型不一致 / 空 body）は CodeInvalidRequest（400）を返す。先頭の
// 1 値を decode した後、後続トークン（例: `{} trailing`）が残る body も malformed として 400 を返す
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
