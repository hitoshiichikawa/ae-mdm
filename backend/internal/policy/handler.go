package policy

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

// Handler は `/api/policies` 配下 6 endpoint の HTTP I/O + RBAC 判定 + エラー写像を担う
// presentation 層（design.md「policy.Handler」節 / tasks.md task 5.1）。
//
// 各 endpoint は actor（操作実行者の admin_users.id）/ tenantID（所有テナント）を
// `httpserver.AuthClaimsFromContext` で取得し、`authz.Authorizer.AuthorizeAndLog`
// （`ResourcePolicy` × `ActionCreate/Update/Delete/Read`）で own-tenant RBAC を判定する。
// deny は 403、claims 不在は防御的に 401 を返し、いずれも構造化 WARN を出す（Req 4.1 / NFR 3.1）。
//
// JSON decode / path param parse は `tenant.Handler` の `decodeJSON` / `parseID` と同方式。
// Service が返す `*errors.Error`（`ValidationFailedError` を含む）は最外層で
// `errors.WriteHTTP` により Code → HTTP status + JSON body に写像し、Handler 側で独自に
// status を組み立てない。404 / 検証拒否の body は sentinel error / top-level Code に委ね、
// 対象ポリシーの存在差を露出しない（Req 4.5）。検証エラーは `ValidationFailedError` を
// 展開し、details を JSON body に載せて 400（invalid field）/ 422（business rule）を分ける。
//
// `httpserver` を import するのは Handler のみで、Service へは actor / tenantID を引数で
// 渡す（依存方向ルール / design Components）。chi.Router を内包することで http.Handler と
// chi.Router の双方を満たし、cmd/api（task 6.1）が `routers.API.Mount("/policies", handler)`
// で配線できる（chi.Mount 互換 / `audit.Handler` が手本）。
type Handler struct {
	router     chi.Router
	svc        Service
	authorizer *authz.Authorizer
	log        logger.Logger
}

// NewHandler は本番用 Handler を構築する。
//
// svc は policy ユースケース、authorizer は own-tenant `policy` RBAC 判定、log は失敗パスの
// 構造化 WARN ログ（NFR 3.1）に用いる。log が nil の場合は logger.Default()（未配線時は
// no-op）を採用し、DI 未配線でも `errors.WriteHTTP` のログ呼び出しで panic させない
// （tenant.NewHandler / policy.NewService の nil-log フォールバックと同方針）。
//
// Mount("/policies", h) 配下で `/api/policies` 6 endpoint を成立させるため、内包 router へ
// root 相対（`/` / `/{id}` / `/{id}/assign`）で登録する（audit.Handler と同方式）。
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
	h.router.Get("/", h.list)
	h.router.Post("/", h.create)
	h.router.Get("/{id}", h.get)
	h.router.Put("/{id}", h.update)
	h.router.Delete("/{id}", h.delete)
	h.router.Put("/{id}/assign", h.assign)
	return h
}

// ServeHTTP は http.Handler / chi.Router を満たす（内包 router に委譲する）。
//
// 呼び出し側（main / task 6.1）が `routers.API.Mount("/policies", h)` を呼ぶことで、chi の
// prefix strip 後に内包 router の root route が成立する（`/api/policies` で稼働）。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.router.ServeHTTP(w, r)
}

// list は `GET /api/policies` の handler（自テナント一覧 / Req 4.4）。read は ActionRead 判定。
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	claims, ok := h.authorize(w, r, authz.ActionRead)
	if !ok {
		return
	}
	summaries, err := h.svc.List(r.Context(), claims.TenantID)
	if err != nil {
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}
	writeJSON(w, http.StatusOK, summaries)
}

// create は `POST /api/policies` の handler（作成 / Req 1.1 / 2.x / 5.1）。
//
//  1. AuthorizeAndLog で own-tenant `policy create` 判定（deny は 403 / claims 不在は 401）
//  2. request body を PolicyRequest に decode（malformed JSON は 400）
//  3. Service.Create を呼ぶ。検証失敗は ValidationFailedError（400 / 422）、AMAPI 失敗は 502 等を
//     Service の写像（top-level Code）に委ねる
//  4. 成功時は PolicyView を 200 で返す（design API Contract）
func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	claims, ok := h.authorize(w, r, authz.ActionCreate)
	if !ok {
		return
	}
	var in PolicyRequest
	if err := decodeJSON(r, &in); err != nil {
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}
	view, err := h.svc.Create(r.Context(), claims.AdminUserID, claims.TenantID, in)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// get は `GET /api/policies/{id}` の handler（詳細 / Req 4.4 / 4.5）。read は ActionRead 判定。
//
// 不在 / 他テナント越境は Service が ErrPolicyNotFound（404 / 固定 message）で返し、存在差を
// 露出しない（Req 4.5）。
func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	claims, ok := h.authorize(w, r, authz.ActionRead)
	if !ok {
		return
	}
	id, err := parseID(r)
	if err != nil {
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}
	view, err := h.svc.Get(r.Context(), claims.TenantID, id)
	if err != nil {
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// update は `PUT /api/policies/{id}` の handler（更新 / Req 1.2 / 2.x / 4.1 / 5.2）。
func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	claims, ok := h.authorize(w, r, authz.ActionUpdate)
	if !ok {
		return
	}
	id, err := parseID(r)
	if err != nil {
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}
	var in PolicyRequest
	if err := decodeJSON(r, &in); err != nil {
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}
	view, err := h.svc.Update(r.Context(), claims.AdminUserID, claims.TenantID, id, in)
	if err != nil {
		h.writeServiceError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// delete は `DELETE /api/policies/{id}` の handler（削除 / Req 5.3）。
//
// 不在 / 他テナント越境は 404、割当済み端末ありの競合は 409 を Service の写像に委ねる。
// 成功時は body 無しで 204 No Content を返す（design API Contract）。
func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	claims, ok := h.authorize(w, r, authz.ActionDelete)
	if !ok {
		return
	}
	id, err := parseID(r)
	if err != nil {
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}
	if err := h.svc.Delete(r.Context(), claims.AdminUserID, claims.TenantID, id); err != nil {
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// assign は `PUT /api/policies/{id}/assign` の handler（端末割当 / Req 3.x / 4.2 / 4.3）。
//
// 割当は変更操作のため ActionUpdate で判定する（policy 行は変えず devices.applied_policy_id を
// 更新する操作であり、policy に対する update 権限と同一視する）。自テナント不在の policy /
// device は Service が NotFound（404 / 存在差非露出）に写像する（Req 3.2 / 3.3 / 4.5）。
// 成功時は body 無しで 204 No Content を返す（design API Contract）。
func (h *Handler) assign(w http.ResponseWriter, r *http.Request) {
	claims, ok := h.authorize(w, r, authz.ActionUpdate)
	if !ok {
		return
	}
	id, err := parseID(r)
	if err != nil {
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}
	var in AssignInput
	if err := decodeJSON(r, &in); err != nil {
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}
	if err := h.svc.Assign(r.Context(), claims.AdminUserID, claims.TenantID, in.DeviceID, id); err != nil {
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// authorize は claims を取得し own-tenant `policy` RBAC 判定を行う共通前段（Req 4.1 / NFR 3.1）。
//
// claims 不在は防御的に 401（通常は TenantContextMiddleware が先行 401）。deny は 403。
// いずれの拒否経路も AuthorizeAndLog（platform 層）/ logDeny（policy 層）で原因属性を
// 構造化 WARN に出す（NFR 3.1）。allow の場合のみ (claims, true) を返す。
//
// TargetTenantID には claims.TenantID（own-tenant）を渡し、RBAC を自テナント境界に閉じる
// （cross-tenant は Authorizer が DenyReasonCrossTenant で deny / Req 4.x）。
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
		Resource:        authz.ResourcePolicy,
		TargetTenantID:  claims.TenantID.String(),
	})
	if !decision.Allowed {
		h.logDeny(r, claims.TenantID, action, "authz denied")
		pkgerrors.WriteHTTP(w, r, pkgerrors.New(
			pkgerrors.CodeForbidden,
			"policy operation forbidden",
		), h.log)
		return httpserver.AuthClaims{}, false
	}
	return claims, true
}

// writeServiceError は Service が返したエラーを HTTP 応答へ写像する（Create / Update 経路）。
//
// `ValidationFailedError`（検証エラー全件）の場合は、top-level Code（400 invalid field /
// 422 business rule）に応じた status を `EffectiveHTTPStatus` で決定し、details を JSON body に
// 載せて返す（Req 2.2 / 2.3）。details の Message は mapper / Validator が機密値を載せない契約
// （Req 5.4 / NFR 3.2）。それ以外の `*errors.Error`（AMAPI 502 / NotFound 404 等）は通常どおり
// `errors.WriteHTTP` で写像する。
func (h *Handler) writeServiceError(w http.ResponseWriter, r *http.Request, err error) {
	var verr *ValidationFailedError
	if stderrors.As(err, &verr) {
		h.writeValidationError(w, r, verr)
		return
	}
	pkgerrors.WriteHTTP(w, r, err, h.log)
}

// validationErrorBody は検証エラー（400 / 422）の JSON 応答 body。
//
// code / message は top-level の写像（WriteHTTP と同一フォーマット）に揃え、details に全不正
// 項目を載せる（Req 2.2 / 2.3 / 2.4）。
type validationErrorBody struct {
	Code    string                `json:"code"`
	Message string                `json:"message"`
	Details []validationErrorItem `json:"details"`
}

// validationErrorItem は不正項目 1 件の JSON 表現。domain / field / kind / message のみを
// 露出し、raw body の生値（機密値）は含めない（Req 5.4 / NFR 3.2）。
type validationErrorItem struct {
	Domain  string `json:"domain"`
	Field   string `json:"field"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// writeValidationError は ValidationFailedError を 400 / 422 + details 付き JSON で応答する。
func (h *Handler) writeValidationError(w http.ResponseWriter, r *http.Request, verr *ValidationFailedError) {
	status := verr.Unwrap().(*pkgerrors.Error).EffectiveHTTPStatus()
	// 4xx は WARN として失敗を観測する（WriteHTTP の 4xx ログ方針と揃える / NFR 3.1）。
	h.log.Warn("policy validation rejected",
		"request_id", httpserver.RequestIDFromContext(r.Context()),
		"path", r.URL.Path,
		"method", r.Method,
		"code", string(verr.Code()),
		"error_count", len(verr.Errors),
	)
	body := validationErrorBody{
		Code:    string(verr.Code()),
		Message: "policy validation failed",
		Details: make([]validationErrorItem, 0, len(verr.Errors)),
	}
	for _, ve := range verr.Errors {
		body.Details = append(body.Details, validationErrorItem{
			Domain:  string(ve.Domain),
			Field:   ve.Field,
			Kind:    string(ve.Kind),
			Message: ve.Message,
		})
	}
	writeJSON(w, status, body)
}

// logDeny は拒否経路（401 / 403）で原因属性を構造化 WARN に出す（NFR 3.1）。
//
// request_id / path / method / action / deny_reason の非機密 field のみ載せ、raw body 等の
// 機密値は補間しない（Req 5.4 / NFR 3.2）。tenantID が uuid.Nil（claims 不在）の場合は
// tenant_id field を省略する。log が nil の場合は no-op。
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
	h.log.Warn("policy operation denied", fields...)
}

// parseID は path param {id} を UUID へ parse する。
// 不正な UUID は CodeInvalidRequest（400）の `*errors.Error` を返す（design.md API Contract）。
func parseID(r *http.Request) (uuid.UUID, error) {
	raw := chi.URLParam(r, "id")
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, pkgerrors.New(pkgerrors.CodeInvalidRequest, "invalid policy id")
	}
	return id, nil
}

// decodeJSON は request body を v へ JSON decode する（tenant.Handler.decodeJSON と同方式）。
//
// decode 失敗（malformed JSON / 型不一致 / 空 body）は CodeInvalidRequest（400）を返す。
// 先頭の 1 値を decode した後、後続トークン（例: `{} trailing`）が残る body も malformed として
// 400 を返す（単一 JSON document のみを正当な入力とする入力検証契約）。
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

// writeJSON は status code と JSON body を応答に書き出す共通ヘルパ（tenant.writeJSON と同方式）。
// encode 失敗は header 送出済みのため諦める（`errors.WriteHTTP` の方針と同じ）。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
