package audit

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/authz"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/httpserver"
)

// AdminHandler は `GET /api/admin/audit-logs`（admin-console / cross-tenant 閲覧）を提供する
// HTTP Handler。
//
// design.md「Audit Admin Handler（admin-console）」節と整合する。chi.Router を内包することで
// http.Handler と chi.Router の双方を満たし、cmd/api が
// `routers.Admin.Mount("/audit-logs", adminHandler)` で配線できる（chi.Mount 互換 / 実 path は
// `/api/admin/audit-logs`）。
//
// 本 handler は `RequireAdminConsoleAndSuperAdmin` 固定ガード配下に Mount される前提（admin aud +
// SuperAdmin でない要求はここに到達しない / Req 4.4）。SuperAdmin の admin-console claims は
// TenantID = uuid.Nil（cross-tenant context）であり、handler は自前で SuperAdmin TenantContext
// （`IsSuperAdmin=true`）を `db.WithTenantContext` で確立してから Service.List を呼ぶことで、
// RLS の is_superadmin 句により全テナント + NULL テナントの監査ログを可視にする（Req 3.1 / 3.5）。
//
// 機密値の非埋込契約（NFR 3.1）: 失敗パスの構造化 WARN ログ・error 文言・JSON body に
// query 生値・detail 生値・トークン等の機密値を補間しない。失敗種別は failure_kind で識別する
// （NFR 3.2）。
type AdminHandler struct {
	router     chi.Router
	svc        Service
	authorizer *authz.Authorizer
	log        logger.Logger
}

// NewAdminHandler は本番用 admin-console AdminHandler を構築する。
//
// svc は cross-tenant 閲覧の List を、authorizer は cross-tenant `audit_log read` の認可判定を
// （tenant_id 指定時の二重防御 / Req 4.6）、log は失敗パスの構造化 WARN ログ（NFR 3.2）に用いる。
// 返り値は chi.Mount 互換の http.Handler / chi.Router（design.md「Audit Admin Handler」API Contract）。
func NewAdminHandler(svc Service, authorizer *authz.Authorizer, log logger.Logger) *AdminHandler {
	h := &AdminHandler{
		router:     chi.NewRouter(),
		svc:        svc,
		authorizer: authorizer,
		log:        log,
	}
	// Mount("/audit-logs", h) 配下で `GET /api/admin/audit-logs` を成立させるため root 相対で登録する。
	h.router.Get("/", h.list)
	return h
}

// ServeHTTP は http.Handler / chi.Router を満たす（内包 router に委譲する）。
func (h *AdminHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.router.ServeHTTP(w, r)
}

// list は `GET /api/admin/audit-logs` の handler 本体（cross-tenant 閲覧 / Req 3.x / 4.4 / 4.6）。
//
// 処理順（design.md「Audit Admin Handler」Responsibilities & Constraints / tasks.md task 4）:
//  1. AuthClaimsFromContext で claims 取得（固定ガードが既に admin aud + SuperAdmin を強制済み /
//     防御的に claims 不在は 401 / Req 4.4）
//  2. query parse: 共通絞り込み（parseFilter）＋ tenant_id の任意 uuid parse（不正は 400 +
//     failure_kind=parse_invalid / Req 3.2 / 3.3）
//  3. tenant_id 指定時のみ cross-tenant authz matrix 判定（SuperAdmin × admin-console ×
//     cross-tenant → allow / deny は 403 / Req 4.6）。tenant_id 無し（全テナント横断ビュー）は
//     authz の uuid.Nil fail-closed を回避する（下記「authz↔design 不整合の解決方針」参照）
//  4. SuperAdmin TenantContext（TenantID=uuid.Nil, IsSuperAdmin=true）を db.WithTenantContext で
//     確立（RLS の is_superadmin 句で全テナント + NULL 可視 / Req 3.1 / 3.5）
//  5. svc.List（DB 失敗は CodeUnavailable → 503 + failure_kind=query_error / NFR 3.2）
//  6. []AuditLogDTO に写像して JSON encode（空は [] で 200 / Req 3.4 / NFR 3.1）
//
// authz↔design 不整合の解決方針（impl-notes.md「確認事項」参照）: design.md は authz を
// 「常に claims.TenantID fallback で TargetTenantID 指定」と書くが、authz.Authorize は
// TargetTenantID が uuid.Nil（SuperAdmin claims.TenantID）に正規化されると role/aud に関わらず
// 無条件 deny する（authz.go Req 4.4 fail-closed）。これをそのまま全テナント横断ビュー
// （tenant_id query 無し）に適用すると 403 になり AC Req 3.1 と矛盾するため、cross-tenant の
// authz matrix 判定は **具体的な tenant_id query が指定されたときのみ** 行う。tenant_id 無しの
// 全テナントビューは `RequireAdminConsoleAndSuperAdmin` 固定ガードが既に admin-console +
// SuperAdmin を強制済み（Req 4.4）であることに依拠して保護する。
func (h *AdminHandler) list(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	claims, ok := httpserver.AuthClaimsFromContext(ctx)
	if !ok {
		// claims 不在は未認証として 401（通常は固定ガード / TenantContextMiddleware が先行 / Req 4.4 補完）。
		h.warnFailure(r, FailureKindAuthzDenied)
		pkgerrors.WriteHTTP(w, r, pkgerrors.New(
			pkgerrors.CodeUnauthenticated,
			"authentication required",
		), h.log)
		return
	}

	filter, err := parseFilter(r)
	if err != nil {
		// 共通絞り込み（event_type / actor_id / resource_id / from / to）の parse 失敗は 400（Req 3.3）。
		h.warnFailure(r, FailureKindParseInvalid)
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}
	// tenant_id は admin 横断閲覧でのみ解釈する任意の絞り込み条件（不正書式は 400 / Req 3.2）。
	tenantID, err := parseTenantIDQuery(r)
	if err != nil {
		h.warnFailure(r, FailureKindParseInvalid)
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}
	filter.TenantID = tenantID

	// tenant_id 指定時のみ cross-tenant authz matrix 判定（二重防御 / Req 4.6）。
	// tenant_id 無し（全テナント横断ビュー）は authz の uuid.Nil fail-closed を回避する
	// （上記 godoc「authz↔design 不整合の解決方針」参照）。
	if filter.TenantID != nil {
		decision := h.authorizer.AuthorizeAndLog(ctx, h.log, authz.LogContext{
			RequestID:         httpserver.RequestIDFromContext(ctx),
			ActorID:           claims.AdminUserID.String(),
			SessionHashPrefix: claims.SessionHashPrefix,
		}, authz.Request{
			Roles:           claims.Roles,
			SessionTenantID: claims.TenantID,
			Audience:        authz.AudienceAdminConsole,
			Action:          authz.ActionRead,
			Resource:        authz.ResourceAuditLog,
			TargetTenantID:  filter.TenantID.String(),
		})
		if !decision.Allowed {
			// AuthorizeAndLog が authz の構造化 WARN を出すため、ここでは status 写像のみ行う（Req 4.6）。
			pkgerrors.WriteHTTP(w, r, pkgerrors.New(
				pkgerrors.CodeForbidden,
				"audit log read forbidden",
			), h.log)
			return
		}
	}

	// SuperAdmin TenantContext を確立してから Service へ。RLS の is_superadmin 句で全テナント +
	// NULL テナントの監査ログを可視にする（Req 3.1 / 3.5）。
	ctx = db.WithTenantContext(ctx, db.TenantContext{
		TenantID:     uuid.Nil,
		IsSuperAdmin: true,
	})

	events, err := h.svc.List(ctx, filter)
	if err != nil {
		// SELECT / scan の DB 失敗（CodeUnavailable → 503 / NFR 3.2）。
		h.warnFailure(r, FailureKindQueryError)
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}

	writeAuditLogs(w, events)
}

// parseTenantIDQuery は HTTP query の `tenant_id` を任意の *uuid.UUID に parse する。
//
// 空（未指定）なら nil（= 全テナント横断ビュー）を返す。非空は uuid parse し、不正書式は
// *errors.Error{Code: CodeInvalidRequest}（400 / failure_kind=parse_invalid）で返す。query 生値は
// error 文言に補間しない（NFR 3.1）。admin handler 専用句であり tenant handler では解釈しない。
func parseTenantIDQuery(r *http.Request) (*uuid.UUID, error) {
	raw := r.URL.Query().Get("tenant_id")
	if raw == "" {
		return nil, nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return nil, pkgerrors.New(pkgerrors.CodeInvalidRequest, "invalid tenant_id")
	}
	return &id, nil
}

// warnFailure は失敗パスで failure_kind を含む構造化 WARN を出す（NFR 3.2）。
//
// request_id / path / method の非機密 field のみ載せ、query 生値・detail 生値・トークン等の
// 機密値は補間しない（NFR 3.1）。log が nil の場合は no-op。
//
// handler.go の `(*Handler).warnFailure` と同等のロジックだが、boundary（AuditAdminHandler）を
// 保つため共有関数へ切り出さず AdminHandler 専用に持たせる（軽微な重複を許容して handler.go を
// 触らない）。
func (h *AdminHandler) warnFailure(r *http.Request, kind string) {
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
