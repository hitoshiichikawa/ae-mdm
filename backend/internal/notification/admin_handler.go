package notification

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/httpserver"
)

// 退避キュー閲覧 admin handler の失敗パスを構造化 WARN ログ（NFR 3.1）で識別するための
// failure_kind 値（audit.failure_kinds.go と同方針 / notification ドメインへローカルに持つ）。
const (
	// failureKindParseInvalid は filter query（from / to / type）の parse 失敗（400 / Req 4.5）。
	failureKindParseInvalid = "parse_invalid"
	// failureKindQueryError は UnassignedQueue.List の DB 失敗（503）。
	failureKindQueryError = "query_error"
)

// AdminHandler は `GET /api/admin/notifications/unassigned`（admin-console / SuperAdmin 専用の
// 退避キュー閲覧）を提供する HTTP Handler。
//
// design.md「NotificationAdminHandler」節と整合する。chi.Router を内包することで
// http.Handler と chi.Router の双方を満たし、cmd/api が
// `routers.Admin.Mount("/notifications/unassigned", h)` で配線できる（chi.Mount 互換 / 実 path は
// `/api/admin/notifications/unassigned`）。
//
// 本 handler は `RequireAdminConsoleAndSuperAdmin` 固定ガード配下に Mount される前提であり、
// 401（未認証 / Req 4.3）・403（非 SuperAdmin / Req 4.4）は middleware が担う。handler 内では
// 認可を再実装せず、filter parse（400 / Req 4.5）と List 結果の JSON 応答（200）/ DB 失敗
// （503）に集中する（audit.AdminHandler と同型だが、本 endpoint は authz.Authorizer / tenant_id
// query を持たないため authorizer 依存を取らない）。
//
// 機密値の非埋込契約（NFR 3.1）: 失敗パスの構造化 WARN ログ・error 文言・JSON body に
// query 生値・payload 生値・トークン等の機密値を補間しない。失敗種別は failure_kind で識別する。
type AdminHandler struct {
	router chi.Router
	queue  UnassignedQueue
	log    logger.Logger
}

// NewAdminHandler は本番用 admin-console 退避キュー閲覧 AdminHandler を構築する。
//
// queue は退避済み通知の List を、log は失敗パスの構造化 WARN ログ（NFR 3.1）に用いる。
// 返り値は chi.Mount 互換の http.Handler / chi.Router（design.md「NotificationAdminHandler」
// API Contract）。
func NewAdminHandler(queue UnassignedQueue, log logger.Logger) *AdminHandler {
	h := &AdminHandler{
		router: chi.NewRouter(),
		queue:  queue,
		log:    log,
	}
	// Mount("/notifications/unassigned", h) 配下で `GET /api/admin/notifications/unassigned` を
	// 成立させるため root 相対で登録する（audit.AdminHandler と同型）。
	h.router.Get("/", h.list)
	return h
}

// ServeHTTP は http.Handler / chi.Router を満たす（内包 router に委譲する）。
func (h *AdminHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.router.ServeHTTP(w, r)
}

// list は `GET /api/admin/notifications/unassigned` の handler 本体（Req 4.1 / 4.2 / 4.5 / NFR 3.1）。
//
// 処理順（design.md「NotificationAdminHandler」Responsibilities & Constraints / tasks.md 4.1）:
//  1. query parse: from / to は RFC3339、type は許可種別値（ENROLLMENT / STATUS_REPORT / COMMAND）。
//     不正は 400 + 不正項目提示 + failure_kind=parse_invalid（Req 4.5）
//  2. SuperAdmin TenantContext（TenantID=uuid.Nil, IsSuperAdmin=true）を db.WithTenantContext で
//     確立（RLS の SuperAdmin only 句で cross-tenant infra テーブル `unassigned_notifications` を
//     可視にする / NFR 3.1）
//  3. queue.List（DB 失敗は CodeUnavailable → 503 + failure_kind=query_error）
//  4. []UnassignedNotificationDTO に写像して JSON encode（空は [] で 200 / Req 4.1）
//
// 401（Req 4.3）/ 403（Req 4.4）は RequireAdminConsoleAndSuperAdmin middleware の責務であり
// handler では再実装しない。
func (h *AdminHandler) list(w http.ResponseWriter, r *http.Request) {
	filter, err := parseFilter(r)
	if err != nil {
		// from / to / type の parse 失敗は 400（不正入力 + 不正項目提示 / Req 4.5）。
		h.warnFailure(r, failureKindParseInvalid)
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}

	// SuperAdmin TenantContext を確立してから List する。unassigned_notifications は tenant_id を
	// 持たない cross-tenant infra テーブルであり、RLS（migration 0011）は SuperAdmin only。
	ctx := db.WithTenantContext(r.Context(), db.TenantContext{
		TenantID:     uuid.Nil,
		IsSuperAdmin: true,
	})

	notifications, err := h.queue.List(ctx, filter)
	if err != nil {
		// SELECT / scan の DB 失敗（CodeUnavailable → 503 / NFR 3.1）。
		h.warnFailure(r, failureKindQueryError)
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}

	writeUnassignedNotifications(w, notifications)
}

// parseFilter は HTTP query から退避キュー閲覧の絞り込み Filter を構築する。
//
// from / to は RFC3339 parse（parseRFC3339Query）、type は許可種別値（ENROLLMENT /
// STATUS_REPORT / COMMAND）検証を行う。不正書式は *errors.Error{Code: CodeInvalidRequest}
// （400 / failure_kind=parse_invalid）で返し、不正項目名を message に提示する（Req 4.5）。
// query 生値は error 文言に補間しない（NFR 3.1）。
func parseFilter(r *http.Request) (Filter, error) {
	q := r.URL.Query()
	var f Filter

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

	if typeRaw := q.Get("type"); typeRaw != "" {
		if !isAllowedNotificationType(typeRaw) {
			return Filter{}, pkgerrors.New(pkgerrors.CodeInvalidRequest, "invalid type")
		}
		f.Type = typeRaw
	}

	return f, nil
}

// parseRFC3339Query は raw が空なら nil（未指定）、非空なら RFC3339 parse した *time.Time を返す。
//
// 非 RFC3339 は *errors.Error{Code: CodeInvalidRequest}（400）で返す。msg は不正項目を示す
// 固定文言で、query 生値を補間しない（Req 4.5 / NFR 3.1。audit.parseRFC3339Query と同方針）。
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

// isAllowedNotificationType は type query が許可種別値（ENROLLMENT / STATUS_REPORT / COMMAND）
// のいずれかであるかを判定する（Req 4.5 の許可値検証）。
func isAllowedNotificationType(raw string) bool {
	switch NotificationType(raw) {
	case Enrollment, StatusReport, Command:
		return true
	default:
		return false
	}
}

// warnFailure は失敗パスで failure_kind を含む構造化 WARN を出す（NFR 3.1）。
//
// request_id / path / method の非機密 field のみ載せ、query 生値・payload 生値・トークン等の
// 機密値は補間しない。log が nil の場合は no-op（audit.AdminHandler.warnFailure と同型）。
func (h *AdminHandler) warnFailure(r *http.Request, kind string) {
	if h.log == nil {
		return
	}
	h.log.Warn("notification admin failure",
		"failure_kind", kind,
		"request_id", httpserver.RequestIDFromContext(r.Context()),
		"path", r.URL.Path,
		"method", r.Method,
	)
}

// UnassignedNotificationDTO は退避キュー閲覧 API の JSON 応答形。
//
// UnassignedNotification をそのまま露出せず DTO に写像する。payload は jsonb をそのまま透過し、
// received_at は RFC3339 文字列で直列化する（audit.AuditLogDTO と同方針）。NFR 3.1 のため
// DTO 化時に追加の機密値を載せない（payload の sanitize は退避記録側責務）。
type UnassignedNotificationDTO struct {
	ID               string          `json:"id"`
	MessageID        string          `json:"message_id"`
	NotificationType string          `json:"notification_type"`
	EnterpriseName   string          `json:"enterprise_name"`
	Payload          json.RawMessage `json:"payload"`
	ReceivedAt       string          `json:"received_at"`
}

// toUnassignedNotificationDTO は UnassignedNotification を JSON 応答用 DTO に写像する。
//
// payload（jsonb 生 bytes）は json.RawMessage として透過し（二重 encode を避ける）、空のときは
// `{}` に倒す（NULL / 空を JSON null にしない / unassigned.payloadOrEmptyJSON と整合）。
// received_at は RFC3339 で直列化する。
func toUnassignedNotificationDTO(n UnassignedNotification) UnassignedNotificationDTO {
	return UnassignedNotificationDTO{
		ID:               n.ID.String(),
		MessageID:        n.MessageID,
		NotificationType: string(n.NotificationType),
		EnterpriseName:   n.EnterpriseName,
		Payload:          json.RawMessage(payloadOrEmptyJSON(n.Payload)),
		ReceivedAt:       n.ReceivedAt.Format(time.RFC3339),
	}
}

// writeUnassignedNotifications は notifications を []UnassignedNotificationDTO に写像して
// 200 + JSON 配列で応答する。
//
// 0 件でも JSON `[]`（空配列）を返し 200 にする（Req 4.1 = 絞り込み一致 0 件は正常応答）。
// nil slice を `null` に倒さないよう、必ず長さ 0 の slice を初期化してから encode する
// （audit.writeAuditLogs と同型）。
func writeUnassignedNotifications(w http.ResponseWriter, notifications []UnassignedNotification) {
	dtos := make([]UnassignedNotificationDTO, 0, len(notifications))
	for _, n := range notifications {
		dtos = append(dtos, toUnassignedNotificationDTO(n))
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	// encode 失敗は応答ボディを諦めるしかない（headers は既送）。既存 writeAuditLogs と同じ方針。
	_ = json.NewEncoder(w).Encode(dtos)
}
