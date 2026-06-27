package audit

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/authz"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/httpserver"
)

// Handler は `GET /api/audit-logs`（tenant-console / own-tenant 閲覧）を提供する HTTP Handler。
//
// design.md「Audit Handler（tenant-console）」節と整合する。chi.Router を内包することで
// http.Handler と chi.Router の双方を満たし、cmd/api が
// `routers.API.Mount("/audit-logs", handler)` で配線できる（chi.Mount 互換）。
//
// 認可は handler 内で Authorizer の own-tenant `audit_log read` 判定を行い（Operator / Viewer /
// 権限不足を 403 / Req 4.1 / 4.5 / 4.6）、未認証は防御的に 401（通常は TenantContextMiddleware が
// 先行 401 / Req 4.2）。実データ取得は TenantContextMiddleware が確立した tenant TenantContext の
// まま Service → Repository へ伝播し、RLS が自テナント分離を物理担保する（Filter.TenantID は
// **設定しない** / Req 2.9 / 4.3 / NFR 2.1）。
//
// 機密値の非埋込契約（NFR 3.1）: 失敗パスの構造化 WARN ログ・error 文言・JSON body に
// query 生値・detail 生値・トークン等の機密値を補間しない。失敗種別は failure_kind で識別する
// （NFR 3.2）。
type Handler struct {
	router     chi.Router
	svc        Service
	authorizer *authz.Authorizer
	log        logger.Logger
}

// NewHandler は本番用 tenant-console Handler を構築する。
//
// svc は own-tenant 閲覧の List を、authorizer は own-tenant `audit_log read` の認可判定を、
// log は失敗パスの構造化 WARN ログ（NFR 3.2）に用いる。返り値は chi.Mount 互換の
// http.Handler / chi.Router（design.md「Audit Handler」API Contract）。
func NewHandler(svc Service, authorizer *authz.Authorizer, log logger.Logger) *Handler {
	h := &Handler{
		router:     chi.NewRouter(),
		svc:        svc,
		authorizer: authorizer,
		log:        log,
	}
	// Mount("/audit-logs", h) 配下で `GET /api/audit-logs` を成立させるため root 相対で登録する。
	h.router.Get("/", h.list)
	return h
}

// ServeHTTP は http.Handler / chi.Router を満たす（内包 router に委譲する）。
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.router.ServeHTTP(w, r)
}

// list は `GET /api/audit-logs` の handler 本体（own-tenant 閲覧 / Req 2.1〜2.8 / 4.x）。
//
// 処理順（design.md「Audit Handler」Responsibilities & Constraints）:
//  1. AuthClaimsFromContext で claims 取得（不在は防御的に 401 / Req 4.2）
//  2. Authorizer.AuthorizeAndLog で own-tenant `audit_log read` 判定（deny は 403 / Req 4.1 / 4.5 / 4.6）
//  3. query parse（不正は 400 + failure_kind=parse_invalid / Req 2.8 と区別）
//  4. Filter 構築（TenantID は設定しない = own-tenant 固定）→ svc.List（Req 2.1〜2.9）
//  5. []AuditLogDTO に写像して JSON encode（空は [] で 200 / Req 2.8 / NFR 3.1）
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	claims, ok := httpserver.AuthClaimsFromContext(ctx)
	if !ok {
		// claims 不在は未認証として 401（通常は TenantContextMiddleware が先行 / Req 4.2）。
		h.warnFailure(r, FailureKindAuthzDenied)
		pkgerrors.WriteHTTP(w, r, pkgerrors.New(
			pkgerrors.CodeUnauthenticated,
			"authentication required",
		), h.log)
		return
	}

	// own-tenant `audit_log read` の認可判定（Operator / Viewer / 権限不足を 403 / Req 4.1 / 4.5 / 4.6）。
	decision := h.authorizer.AuthorizeAndLog(ctx, h.log, authz.LogContext{
		RequestID:         httpserver.RequestIDFromContext(ctx),
		ActorID:           claims.AdminUserID.String(),
		SessionHashPrefix: claims.SessionHashPrefix,
	}, authz.Request{
		Roles:           claims.Roles,
		SessionTenantID: claims.TenantID,
		Audience:        authz.AudienceTenantConsole,
		Action:          authz.ActionRead,
		Resource:        authz.ResourceAuditLog,
		TargetTenantID:  claims.TenantID.String(),
	})
	if !decision.Allowed {
		// AuthorizeAndLog は platform 層の `authz denied`（authz_deny_reason）を出すが、audit ドメインの
		// 失敗観測契約（NFR 3.2）は failure_kind で識別するため、認可拒否分岐でも
		// failure_kind=authz_denied を構造化 WARN に出す（他の失敗分岐と識別軸を揃える / Req 4.1 / 4.5 / 4.6）。
		h.warnFailure(r, FailureKindAuthzDenied)
		pkgerrors.WriteHTTP(w, r, pkgerrors.New(
			pkgerrors.CodeForbidden,
			"audit log read forbidden",
		), h.log)
		return
	}

	filter, err := parseFilter(r)
	if err != nil {
		// query parse 失敗は 400（不正入力 / 空結果の 200 とは区別 / Req 2.8）。
		h.warnFailure(r, FailureKindParseInvalid)
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}
	// own-tenant 固定: TenantID は設定せず RLS に自テナント分離を委ねる（Req 2.9 / 4.3 / NFR 2.1）。
	filter.TenantID = nil

	events, err := h.svc.List(ctx, filter)
	if err != nil {
		// SELECT / scan の DB 失敗（CodeUnavailable → 503 / NFR 3.2）。
		h.warnFailure(r, FailureKindQueryError)
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}

	writeAuditLogs(w, events)
}

// parseFilter は HTTP query から共通の絞り込み Filter を構築する。
//
// event_type / resource_id はそのまま、actor_id は uuid parse、from / to は RFC3339 parse する。
// 不正書式は *errors.Error{Code: CodeInvalidRequest}（400 / failure_kind=parse_invalid）で返し、
// query 生値を error 文言に補間しない（NFR 3.1）。tenant_id は本 handler では解釈しない
// （own-tenant 固定 / admin handler 専用句）。
func parseFilter(r *http.Request) (Filter, error) {
	q := r.URL.Query()
	var f Filter

	f.EventType = EventType(q.Get("event_type"))
	f.ResourceID = q.Get("resource_id")

	if actorID := q.Get("actor_id"); actorID != "" {
		if _, err := uuid.Parse(actorID); err != nil {
			return Filter{}, pkgerrors.New(pkgerrors.CodeInvalidRequest, "invalid actor_id")
		}
		f.ActorID = actorID
	}

	from, err := parseRFC3339Query(q.Get("from"), "invalid from")
	if err != nil {
		return Filter{}, err
	}
	f.From = from

	to, err := parseRFC3339Query(q.Get("to"), "invalid to")
	if err != nil {
		return Filter{}, err
	}
	f.To = to

	return f, nil
}

// parseRFC3339Query は raw が空なら nil（未指定）、非空なら RFC3339 parse した *time.Time を返す。
//
// 非 RFC3339 は *errors.Error{Code: CodeInvalidRequest}（400）で返す。msg は固定文言で、
// query 生値を補間しない（NFR 3.1）。
func parseRFC3339Query(raw, msg string) (*time.Time, error) {
	if raw == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil, pkgerrors.New(pkgerrors.CodeInvalidRequest, msg)
	}
	return &t, nil
}

// warnFailure は失敗パスで failure_kind を含む構造化 WARN を出す（NFR 3.2）。
//
// request_id / path / method の非機密 field のみ載せ、query 生値・detail 生値・トークン等の
// 機密値は補間しない（NFR 3.1）。log が nil の場合は no-op。
func (h *Handler) warnFailure(r *http.Request, kind string) {
	if h.log == nil {
		return
	}
	h.log.Warn("audit failure",
		"failure_kind", kind,
		"request_id", httpserver.RequestIDFromContext(r.Context()),
		"path", r.URL.Path,
		"method", r.Method,
	)
}

// AuditLogDTO は監査ログ閲覧 API（tenant / admin の両 handler 共通）の JSON 応答形。
//
// Event をそのまま露出せず DTO に写像する（design.md「AuditLogDTO」節）。tenant_id は NULL
// テナント（uuid.Nil）の場合に JSON `null` として出力するため *string とする。occurred_at は
// RFC3339 文字列。detail は jsonb をそのまま透過する。NFR 3.1 のため DTO 化時に追加の機密値を
// 載せない（detail の sanitize は記録側責務）。
type AuditLogDTO struct {
	ID         string         `json:"id"`
	TenantID   *string        `json:"tenant_id"`
	ActorID    string         `json:"actor_id"`
	EventType  string         `json:"event_type"`
	ResourceID string         `json:"resource_id"`
	Detail     map[string]any `json:"detail"`
	Result     string         `json:"result"`
	OccurredAt string         `json:"occurred_at"`
}

// toAuditLogDTO は Event を JSON 応答用 DTO に写像する。
//
// TenantID == uuid.Nil（NULL テナント / Req 1.2）は JSON `null` に倒すため *string の nil を返す。
// occurred_at は RFC3339 で直列化する（design.md「AuditLogDTO」）。
func toAuditLogDTO(ev Event) AuditLogDTO {
	dto := AuditLogDTO{
		ID:         ev.ID.String(),
		ActorID:    ev.ActorID.String(),
		EventType:  string(ev.EventType),
		ResourceID: ev.ResourceID,
		Detail:     ev.Detail,
		Result:     string(ev.Result),
		OccurredAt: ev.OccurredAt.Format(time.RFC3339),
	}
	if ev.TenantID != uuid.Nil {
		tid := ev.TenantID.String()
		dto.TenantID = &tid
	}
	return dto
}

// writeAuditLogs は events を []AuditLogDTO に写像して 200 + JSON 配列で応答する。
//
// 0 件でも JSON `[]`（空配列）を返し 200 にする（Req 2.8 / 3.4 = 絞り込み一致 0 件は正常応答）。
// nil slice を `null` に倒さないよう、必ず長さ 0 の slice を初期化してから encode する。
func writeAuditLogs(w http.ResponseWriter, events []Event) {
	dtos := make([]AuditLogDTO, 0, len(events))
	for _, ev := range events {
		dtos = append(dtos, toAuditLogDTO(ev))
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	// encode 失敗は応答ボディを諦めるしかない（headers は既送）。既存 WriteHTTP と同じ方針。
	_ = json.NewEncoder(w).Encode(dtos)
}
