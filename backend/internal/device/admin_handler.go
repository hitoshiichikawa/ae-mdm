package device

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

// AdminHandler は `GET /api/admin/devices/overview`（admin-console / 全テナント横断 overview）を
// 提供する HTTP Handler（design.md「device.AdminHandler」節 / tasks.md task 8）。
//
// chi.Router を内包することで http.Handler と chi.Router の双方を満たし、cmd/api が
// `routers.Admin.Mount("/devices/overview", adminHandler)` で配線できる（chi.Mount 互換 / 実 path は
// `/api/admin/devices/overview`）。
//
// 本 handler は `RequireAdminConsoleAndSuperAdmin` 固定ガード配下に Mount される前提（tenant-console
// aud + 非 SuperAdmin はここに到達しない / Req 6.2）。SuperAdmin の admin-console claims は
// TenantID = uuid.Nil（cross-tenant context）であり、handler は自前で SuperAdmin TenantContext
// （`IsSuperAdmin=true`）を `db.WithTenantContext` で確立してから Service.Overview を呼ぶことで、
// RLS の is_superadmin 句により全テナントの端末を集計可視にする（Req 6.1 / NFR 3.1）。加えて
// `authz` の cross-tenant `ResourceDevice read` を **常に** 判定する二重防御を入れる
// （`audit.AdminHandler` の probe-tenant 方式に倣う / Req 6.2）。
//
// 機密値の非埋込契約（NFR 3.1）: 失敗パスの構造化 WARN ログ・error 文言・JSON body に
// query 生値・トークン等の機密値を補間しない（`device.Handler` と同方針）。
type AdminHandler struct {
	router     chi.Router
	svc        Service
	authorizer *authz.Authorizer
	log        logger.Logger
}

// crossTenantAuthzProbeTenantID は tenant_id query 無し（全テナント横断ビュー）の cross-tenant
// `device read` 認可を許可マトリクスで実際に gate するための代表 TargetTenantID。
//
// SuperAdmin セッションの SessionTenantID は uuid.Nil であり、任意の **非 nil** UUID は
// authz.Authorize の cross-tenant 分岐（SessionTenantID != target かつ target != uuid.Nil）へ
// 確実に落ちる。これにより「admin-console + SuperAdmin が cross-tenant device read を許可
// されているか」を許可マトリクスで判定でき、非 SuperAdmin は cross-tenant deny で 403 になる
// （Req 6.2）。target を claims.TenantID（=uuid.Nil）にすると authz が fail-closed で無条件 deny し
// 全テナントビューが 403 に化けるため、固定の非 nil sentinel 値で判定を決定的にする
// （`audit.crossTenantAuthzProbeTenantID` と同手法）。本値は **authz 判定専用** であり
// tenantFilter には設定しない（Filter は nil のまま Service.Overview が全テナント集計する）。
var crossTenantAuthzProbeTenantID = uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")

// NewAdminHandler は本番用 admin-console AdminHandler を構築する。
//
// svc は横断 overview の Overview を、authorizer は cross-tenant `device read` の認可判定を
// （二重防御 / Req 6.2）、log は失敗パスの構造化 WARN ログ（NFR 3.1）に用いる。log が nil の場合は
// logger.Default() を採用し、DI 未配線でも `errors.WriteHTTP` のログ呼び出しで panic させない
// （device.NewHandler と同方針）。返り値は chi.Mount 互換の http.Handler / chi.Router。
func NewAdminHandler(svc Service, authorizer *authz.Authorizer, log logger.Logger) *AdminHandler {
	if log == nil {
		log = logger.Default()
	}
	h := &AdminHandler{
		router:     chi.NewRouter(),
		svc:        svc,
		authorizer: authorizer,
		log:        log,
	}
	// Mount("/devices/overview", h) 配下で `GET /api/admin/devices/overview` を成立させるため
	// root 相対（`/`）で **GET のみ** 登録する（write endpoint は公開しない / Req 7.3 と整合）。
	h.router.Get("/", h.overview)
	return h
}

// ServeHTTP は http.Handler / chi.Router を満たす（内包 router に委譲する）。
func (h *AdminHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.router.ServeHTTP(w, r)
}

// overview は `GET /api/admin/devices/overview` の handler 本体（全テナント横断集計 / Req 6.1〜6.4）。
//
// 処理順（design.md「device.AdminHandler」Responsibilities & Constraints / audit.AdminHandler 手本）:
//  1. AuthClaimsFromContext で claims 取得（固定ガードが admin aud + SuperAdmin を強制済み /
//     防御的に claims 不在は 401 / Req 6.2 前段）
//  2. tenant_id query の任意 uuid parse（不正書式は 400 / Req 6.3）
//  3. cross-tenant authz matrix 判定を **常に** 行う（二重防御 / Req 6.2）。TargetTenantID は
//     tenant_id 指定時は当該テナント、無指定（全テナント横断ビュー）は代表 probe テナントを渡す。
//     deny は 403
//  4. SuperAdmin TenantContext（TenantID=uuid.Nil, IsSuperAdmin=true）を db.WithTenantContext で
//     確立（RLS の is_superadmin 句で全テナント可視 / Req 6.1 / NFR 3.1）
//  5. svc.Overview（DB 失敗は CodeUnavailable → 503）
//  6. []TenantOverview を JSON encode（空集計は [] で 200 / Req 6.4）
func (h *AdminHandler) overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	claims, ok := httpserver.AuthClaimsFromContext(ctx)
	if !ok {
		// claims 不在は未認証として 401（通常は固定ガード / TenantContextMiddleware が先行 / Req 6.2 補完）。
		h.logDeny(r, "missing auth claims")
		pkgerrors.WriteHTTP(w, r, pkgerrors.New(
			pkgerrors.CodeUnauthenticated,
			"authentication required",
		), h.log)
		return
	}

	// tenant_id は横断 overview でのみ解釈する任意の絞り込み条件（不正書式は 400 / Req 6.3）。
	tenantFilter, err := parseTenantIDQuery(r)
	if err != nil {
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}

	// cross-tenant authz matrix 判定を **常に** 行う（二重防御 / Req 6.2）。tenant_id 指定時は
	// 当該テナントを、無指定（全テナント横断ビュー）は代表 probe テナントを TargetTenantID に渡し、
	// いずれの経路でも許可マトリクスが cross-tenant `device read`（SuperAdmin のみ）を gate する。
	targetTenantID := crossTenantAuthzProbeTenantID.String()
	if tenantFilter != nil {
		targetTenantID = tenantFilter.String()
	}
	decision := h.authorizer.AuthorizeAndLog(ctx, h.log, authz.LogContext{
		RequestID:         httpserver.RequestIDFromContext(ctx),
		ActorID:           claims.AdminUserID.String(),
		SessionHashPrefix: claims.SessionHashPrefix,
	}, authz.Request{
		Roles:           claims.Roles,
		SessionTenantID: claims.TenantID,
		Audience:        authz.AudienceAdminConsole,
		Action:          authz.ActionRead,
		Resource:        authz.ResourceDevice,
		TargetTenantID:  targetTenantID,
	})
	if !decision.Allowed {
		h.logDeny(r, "authz denied")
		pkgerrors.WriteHTTP(w, r, pkgerrors.New(
			pkgerrors.CodeForbidden,
			"device overview forbidden",
		), h.log)
		return
	}

	// SuperAdmin TenantContext を確立してから Service へ。RLS の is_superadmin 句で全テナントの端末を
	// 集計可視にする（Req 6.1 / NFR 3.1）。
	ctx = db.WithTenantContext(ctx, db.TenantContext{
		TenantID:     uuid.Nil,
		IsSuperAdmin: true,
	})

	overviews, err := h.svc.Overview(ctx, tenantFilter)
	if err != nil {
		// AggregateOverview / scan の DB 失敗（CodeUnavailable → 503）。
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}
	// 空集計は null ではなく [] で 200（Req 6.4）。Service.Overview は非 nil 空 slice を返すが、
	// 防御的に nil を空 slice へ正規化して JSON が [] になることを保証する。
	if overviews == nil {
		overviews = []TenantOverview{}
	}
	writeJSON(w, http.StatusOK, overviews)
}

// parseTenantIDQuery は HTTP query の `tenant_id` を任意の *uuid.UUID に parse する（Req 6.3）。
//
// 空（未指定）なら nil（= 全テナント横断ビュー）を返す。非空は uuid parse し、不正書式は
// *errors.Error{Code: CodeInvalidRequest}（400）で返す。query 生値は error 文言に補間しない
// （NFR 3.1）。admin overview 専用句であり tenant-console Handler では解釈しない
// （audit.parseTenantIDQuery と同方式）。
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

// logDeny は拒否経路（401 / 403）で原因属性を構造化 WARN に出す（NFR 3.1）。
//
// request_id / path / method / deny_reason の非機密 field のみ載せ、query 生値等の機密値は
// 補間しない。log が nil の場合は no-op（device.Handler.logDeny と同型だが AdminHandler は
// action 固定 read / tenant_id を持たないため field を簡素化）。
func (h *AdminHandler) logDeny(r *http.Request, reason string) {
	if h.log == nil {
		return
	}
	h.log.Warn("device overview denied",
		"deny_reason", reason,
		"action", string(authz.ActionRead),
		"request_id", httpserver.RequestIDFromContext(r.Context()),
		"path", r.URL.Path,
		"method", r.Method,
	)
}
