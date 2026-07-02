package device

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/authz"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/httpserver"
)

// ページング既定値と上限（Req 1.6 / design.md「device.Handler」節）。マジックナンバーを避け、
// parseListFilter の補完・clamp ロジックで共有する。
const (
	// defaultPage は page query 未指定時に補完する 1-based ページ番号。
	defaultPage = 1
	// defaultPageSize は page_size query 未指定時に補完する取得件数。
	defaultPageSize = 50
	// maxPageSize は page_size の上限。超過値は 400 とせず本値に clamp する（design.md 設計判断）。
	maxPageSize = 200
)

// sync_state query の許容値（design.md L316）。delayed=同期遅延のみ / ok=非遅延のみ。
const (
	// syncStateDelayed は同期遅延端末のみを抽出する sync_state query 値。
	syncStateDelayed = "delayed"
	// syncStateOK は非遅延端末のみを抽出する sync_state query 値。
	syncStateOK = "ok"
)

// Handler は `/api/devices` 配下の read-only 2 endpoint（一覧 / 詳細）の HTTP I/O + RBAC 判定 +
// エラー写像を担う presentation 層（design.md「device.Handler」節 / tasks.md task 7）。
//
// 各 endpoint は tenantID（所有テナント）を `httpserver.AuthClaimsFromContext` で取得し、
// `authz.Authorizer.AuthorizeAndLog`（`ResourceDevice` × `ActionRead`）で own-tenant RBAC を
// 判定する。deny は 403、claims 不在は防御的に 401 を返し、いずれも構造化 WARN を出す
// （NFR 3.1）。TenantAdmin / Operator / Viewer はいずれも device read を許可されている
// （permissionMatrix / policy.Handler と同型）。
//
// **端末属性の write endpoint は公開しない**（POST / PUT / DELETE を登録しない）。端末属性の
// 更新経路は StatusApplier（STATUS_REPORT 通知駆動）のみであり、HTTP 経由の直接書込みを提供
// しないことを route レベルでも担保する（Req 7.3 / Service interface が write を持たない型担保と
// 二重で成立）。
//
// path param parse（parseID）/ JSON encode（writeJSON）は `policy.Handler` と同方式。Service が
// 返す `ErrDeviceNotFound`（不在 / 他テナント越境）は最外層で `errors.WriteHTTP` により
// Code → HTTP status に写像し、Handler 側で status を組み立てない。不在と越境で同一応答を返し、
// 対象端末の存在差を露出しない（Req 2.4 / 5.1 / 5.2）。
//
// chi.Router を内包することで http.Handler と chi.Router の双方を満たし、cmd/api（task 8）が
// `routers.API.Mount("/devices", handler)` で配線できる（chi.Mount 互換 / `policy.Handler` が手本）。
type Handler struct {
	router     chi.Router
	svc        Service
	authorizer *authz.Authorizer
	log        logger.Logger
}

// NewHandler は本番用 Handler を構築する。
//
// svc は device read ユースケース、authorizer は own-tenant `device` RBAC 判定、log は失敗パスの
// 構造化 WARN ログ（NFR 3.1）に用いる。log が nil の場合は logger.Default()（未配線時は no-op）を
// 採用し、DI 未配線でも `errors.WriteHTTP` のログ呼び出しで panic させない（policy.NewHandler と
// 同方針）。
//
// Mount("/devices", h) 配下で `/api/devices` を成立させるため、内包 router へ root 相対
// （`/` / `/{id}`）で **GET のみ** 登録する。write メソッド（POST / PUT / DELETE）は登録しない
// （Req 7.3 / write endpoint 非公開）。
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
	h.router.Get("/{id}", h.get)
	return h
}

// ServeHTTP は http.Handler / chi.Router を満たす（内包 router に委譲する）。
//
// 呼び出し側（main / task 8）が `routers.API.Mount("/devices", h)` を呼ぶことで、chi の
// prefix strip 後に内包 router の root route が成立する（`/api/devices` で稼働）。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.router.ServeHTTP(w, r)
}

// list は `GET /api/devices` の handler（自テナント端末一覧 / Req 1.1）。read は ActionRead 判定。
//
// authorize → parseListFilter → svc.List(claims.TenantID, filter) → 200 で []DeviceSummary。
// フィルタの未定義 enum 値・非数値ページングは parseListFilter が 400（CodeInvalidRequest）に
// 写像する（Req 1.6）。
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	claims, ok := h.authorize(w, r, authz.ActionRead)
	if !ok {
		return
	}
	filter, err := parseListFilter(r)
	if err != nil {
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}
	summaries, err := h.svc.List(r.Context(), claims.TenantID, filter)
	if err != nil {
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}
	writeJSON(w, http.StatusOK, summaries)
}

// get は `GET /api/devices/{id}` の handler（端末詳細 / Req 2.1）。read は ActionRead 判定。
//
// 不在 / 他テナント越境は Service が ErrDeviceNotFound（404 / 汎用 message）で返し、存在差を
// 露出しない（Req 2.4 / 5.1 / 5.2）。不正 UUID path は parseID が 400 に写像する。
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
	detail, err := h.svc.Get(r.Context(), claims.TenantID, id)
	if err != nil {
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

// authorize は claims を取得し own-tenant `device` RBAC 判定を行う共通前段（Req 1.1 / NFR 3.1）。
//
// claims 不在は防御的に 401（通常は TenantContextMiddleware が先行 401）。deny は 403。いずれの
// 拒否経路も AuthorizeAndLog（platform 層）/ logDeny（device 層）で原因属性を構造化 WARN に出す
// （NFR 3.1）。allow の場合のみ (claims, true) を返す。
//
// TargetTenantID には claims.TenantID（own-tenant）を渡し、RBAC を自テナント境界に閉じる。
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
		Resource:        authz.ResourceDevice,
		TargetTenantID:  claims.TenantID.String(),
	})
	if !decision.Allowed {
		h.logDeny(r, claims.TenantID, action, "authz denied")
		pkgerrors.WriteHTTP(w, r, pkgerrors.New(
			pkgerrors.CodeForbidden,
			"device operation forbidden",
		), h.log)
		return httpserver.AuthClaims{}, false
	}
	return claims, true
}

// logDeny は拒否経路（401 / 403）で原因属性を構造化 WARN に出す（NFR 3.1）。
//
// request_id / path / method / action / deny_reason の非機密 field のみ載せ、raw body 等の機密値は
// 補間しない（NFR 3.2）。tenantID が uuid.Nil（claims 不在）の場合は tenant_id field を省略する。
// log が nil の場合は no-op。
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
	h.log.Warn("device operation denied", fields...)
}

// parseListFilter は `GET /api/devices` の query を ListFilter へ解釈する（Req 1.6）。
//
// compliance / mode / sync_state の未定義 enum 値は 400（CodeInvalidRequest）に倒す（Req 1.6）。
// page / page_size は既定補完（1 / 50）・上限 clamp（<=200）・非数値/1 未満は 400（design.md
// 「device.Handler」節）。各 query の解釈は単一責務の helper に委譲する。
func parseListFilter(r *http.Request) (ListFilter, error) {
	q := r.URL.Query()
	compliance, err := parseComplianceFilter(q.Get("compliance"))
	if err != nil {
		return ListFilter{}, err
	}
	mode, err := parseModeFilter(q.Get("mode"))
	if err != nil {
		return ListFilter{}, err
	}
	syncDelayed, err := parseSyncStateFilter(q.Get("sync_state"))
	if err != nil {
		return ListFilter{}, err
	}
	page, err := parsePage(q.Get("page"))
	if err != nil {
		return ListFilter{}, err
	}
	pageSize, err := parsePageSize(q.Get("page_size"))
	if err != nil {
		return ListFilter{}, err
	}
	return ListFilter{
		Compliance:  compliance,
		Mode:        mode,
		SyncDelayed: syncDelayed,
		Page:        page,
		PageSize:    pageSize,
	}, nil
}

// parseComplianceFilter は compliance query を解釈する。空は nil（無条件）、4 分類以外は 400
// （Req 1.6 / 3.4：unsupported も有効値として parse 成功する）。
func parseComplianceFilter(raw string) (*ComplianceStatus, error) {
	if raw == "" {
		return nil, nil
	}
	c := ComplianceStatus(raw)
	switch c {
	case ComplianceStatusCompliant, ComplianceStatusNonCompliant,
		ComplianceStatusUnknown, ComplianceStatusUnsupported:
		return &c, nil
	default:
		return nil, pkgerrors.New(pkgerrors.CodeInvalidRequest, "invalid compliance filter")
	}
}

// parseModeFilter は mode query を解釈する。空は nil（無条件）、2 値以外は 400（Req 1.6）。
func parseModeFilter(raw string) (*DeviceMode, error) {
	if raw == "" {
		return nil, nil
	}
	m := DeviceMode(raw)
	switch m {
	case DeviceModeFullyManaged, DeviceModeDedicated:
		return &m, nil
	default:
		return nil, pkgerrors.New(pkgerrors.CodeInvalidRequest, "invalid mode filter")
	}
}

// parseSyncStateFilter は sync_state query を解釈する。空は nil（無条件）、delayed→true /
// ok→false、それ以外は 400（Req 1.6）。
func parseSyncStateFilter(raw string) (*bool, error) {
	switch raw {
	case "":
		return nil, nil
	case syncStateDelayed:
		v := true
		return &v, nil
	case syncStateOK:
		v := false
		return &v, nil
	default:
		return nil, pkgerrors.New(pkgerrors.CodeInvalidRequest, "invalid sync_state filter")
	}
}

// parsePage は page query を解釈する。空は既定 1、非数値 / 1 未満は 400。
func parsePage(raw string) (int, error) {
	if raw == "" {
		return defaultPage, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, pkgerrors.New(pkgerrors.CodeInvalidRequest, "invalid page")
	}
	return n, nil
}

// parsePageSize は page_size query を解釈する。空は既定 50、非数値 / 1 未満は 400、上限 200 超は
// 200 に clamp（400 にしない / design.md 設計判断）。
func parsePageSize(raw string) (int, error) {
	if raw == "" {
		return defaultPageSize, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, pkgerrors.New(pkgerrors.CodeInvalidRequest, "invalid page_size")
	}
	if n > maxPageSize {
		n = maxPageSize
	}
	return n, nil
}

// parseID は path param {id} を UUID へ parse する。
// 不正な UUID は CodeInvalidRequest（400）の `*errors.Error` を返す（policy.parseID と同方式）。
func parseID(r *http.Request) (uuid.UUID, error) {
	raw := chi.URLParam(r, "id")
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, pkgerrors.New(pkgerrors.CodeInvalidRequest, "invalid device id")
	}
	return id, nil
}

// writeJSON は status code と JSON body を応答に書き出す共通ヘルパ（policy.writeJSON と同方式）。
// encode 失敗は header 送出済みのため諦める（`errors.WriteHTTP` の方針と同じ）。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
