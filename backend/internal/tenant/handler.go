package tenant

import (
	"encoding/json"
	stderrors "errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/httpserver"
)

// Handler は `/api/admin/tenants` 配下 5 endpoint の HTTP I/O を担う presentation 層
// （design.md「tenant.Handler」節 / tasks.md task 6.1）。
//
// 各 endpoint は request の JSON decode / path param parse を行い、actor（操作実行者の
// admin_users.id）を `httpserver.AuthClaimsFromContext` で取得して Service へ委譲する。
// Service / Repository が返す `*errors.Error` は最外層で `errors.WriteHTTP` により Code →
// HTTP status + JSON body に写像し、Handler 側では独自に status を組み立てない。404 / 競合
// の body は sentinel error の固定 message に委ね、対象テナントの存在差を露出しない（Req 6.5）。
//
// 認可は `/api/admin` route guard（#37 `RequireAdminConsoleAndSuperAdmin`）が
// 呼び出し側（`Routers.Admin`）で適用済みのため、Handler 内で audience / role を再判定しない
// （Req 6.1〜6.4 を継承）。
type Handler struct {
	svc Service
	log logger.Logger
}

// NewHandler は本番用 Handler を構築する。Service は本番では `tenant.NewService(...)` の戻り値。
//
// log が nil の場合は logger.Default()（未配線時は no-op）を採用し、DI 未配線でも
// `errors.WriteHTTP` のログ呼び出しで panic させない（`NewService` の nil-log フォールバックと
// 同方針）。
func NewHandler(svc Service, log logger.Logger) *Handler {
	if log == nil {
		log = logger.Default()
	}
	return &Handler{svc: svc, log: log}
}

// Mount は `/tenants` プレフィックス配下に 5 つの route を sub-router で登録する
// （`internal/auth/handler.go` の `Mount` と同方式）。
//
// 呼び出し側（将来の main）が `Routers.Admin`（admin chain 適用済みの `/api/admin` サブルータ）
// に対して `h.Mount(routers.Admin)` を呼ぶことで、`/api/admin/tenants` 配下 5 endpoint が
// admin guard 継承で稼働する（Req 6.1）。`r.Route("/tenants", ...)` 経由で sub-router を作り、
// root 相対登録（`r.Post("/tenants", ...)`）はしない（admin chain 配下に正しく入れるため）。
//
//	r.Route("/tenants", func(sub chi.Router) {
//	    sub.Post("/", h.create)
//	    sub.Get("/", h.list)
//	    sub.Get("/{id}", h.get)
//	    sub.Post("/{id}/bind", h.bind)
//	    sub.Delete("/{id}", h.disable)
//	})
func (h *Handler) Mount(r chi.Router) {
	r.Route("/tenants", func(sub chi.Router) {
		sub.Post("/", h.create)
		sub.Get("/", h.list)
		sub.Get("/{id}", h.get)
		sub.Post("/{id}/bind", h.bind)
		sub.Delete("/{id}", h.disable)
	})
}

// createResponse は `POST /tenants` のレスポンス body。
//
// Service の `Create` が返す `(TenantView, SignupURL, error)` を合成し、
// `{id, name, status, signup_url, signup_url_name}` を返す。`signup_url_name` は後続
// `POST /tenants/{id}/bind` の入力（`CreateEnterprise` の引数）として呼び出し側が必要とする
// 識別子であり、DB には保存しないため作成応答で返さないと bind フロー（Req 2.1）が成立しない。
// NFR 2.3（機密値の非ログ）はログ出力に対する制約であり、API レスポンスへの本識別子の露出は
// 対象外（より機微な `signup_url` 本体も応答 body に載せている）。
type createResponse struct {
	ID            uuid.UUID `json:"id"`
	Name          string    `json:"name"`
	Status        Status    `json:"status"`
	SignupURL     string    `json:"signup_url"`
	SignupURLName string    `json:"signup_url_name"`
}

// create は `POST /tenants` の HTTP handler（Req 1.1 / 1.3）。
//
//  1. request body を CreateInput に decode（JSON 不正は 400）
//  2. actor を AuthClaims から取得
//  3. Service.Create を呼ぶ。name 空は Service が CodeInvalidRequest（400 / Req 1.3）
//  4. 成功時は status=pending_bind + signup_url を含む JSON を 201 で返す
func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	var in CreateInput
	if err := decodeJSON(r, &in); err != nil {
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}

	actor := actorFromContext(r)
	view, su, err := h.svc.Create(r.Context(), actor, in)
	if err != nil {
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}

	resp := createResponse{
		ID:            view.ID,
		Name:          view.Name,
		Status:        view.Status,
		SignupURL:     su.URL,
		SignupURLName: su.Name,
	}
	writeJSON(w, http.StatusCreated, resp)
}

// bind は `POST /tenants/{id}/bind` の HTTP handler（Req 2.x）。
//
//  1. path param {id} を UUID parse（失敗は 400）
//  2. request body を BindInput に decode（JSON 不正は 400）
//  3. Service.Bind を呼ぶ。bound 重複は 409 / disabled へ bind は 422 / 不在は 404 など
//     Service の写像に委ねる
//  4. 成功時は bound 状態の TenantView（enterprise_name 含む）を 200 で返す
func (h *Handler) bind(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}

	var in BindInput
	if err := decodeJSON(r, &in); err != nil {
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}

	actor := actorFromContext(r)
	view, err := h.svc.Bind(r.Context(), actor, id, in)
	if err != nil {
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// disable は `DELETE /tenants/{id}` の HTTP handler（Req 3.x）。
//
//  1. path param {id} を UUID parse（失敗は 400）
//  2. request body を DisableInput に decode（malformed JSON は 400 / 空 body は確認テキスト
//     未入力として decode を許容）。確認テキスト欠落（空 body / `{}`）は Service が対象 name と
//     不一致として扱い 422（確認未完了 / Req 3.2）を返す。Handler 入口で 400 にしない
//  3. Service.Disable を呼ぶ。二重無効化は 409 / 不在は 404 を Service の写像に委ねる
//  4. 成功時は body 無しで 204 No Content を返す（design.md API Contract）
func (h *Handler) disable(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}

	var in DisableInput
	if err := decodeJSONAllowEmpty(r, &in); err != nil {
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}

	actor := actorFromContext(r)
	if _, err := h.svc.Disable(r.Context(), actor, id, in); err != nil {
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// list は `GET /tenants` の HTTP handler（Req 4.1 / 4.4）。
//
// Service.List を呼び、0 件でも非 nil の空配列 `[]` を JSON で 200 返す。
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	views, err := h.svc.List(r.Context())
	if err != nil {
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}
	writeJSON(w, http.StatusOK, views)
}

// get は `GET /tenants/{id}` の HTTP handler（Req 4.2 / 4.3 / 6.5）。
//
//  1. path param {id} を UUID parse（失敗は 400）
//  2. Service.Get を呼ぶ。不在は 404 で固定 message（存在差を露出しない / Req 6.5）
//  3. 成功時は TenantView を 200 で返す
func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}

	view, err := h.svc.Get(r.Context(), id)
	if err != nil {
		pkgerrors.WriteHTTP(w, r, err, h.log)
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// actorFromContext は request context から actor（操作実行者の admin_users.id）を取得する。
//
// admin chain（`RequireAdminConsoleAndSuperAdmin`）通過後は AuthClaims が確立済みのため
// `AdminUserID` を返す。claims 不在（route guard 未適用の異常経路）では uuid.Nil を返し、
// 監査 Event の actor が空でも Service 本体のユースケースを止めない（認可は guard 層の責務）。
func actorFromContext(r *http.Request) uuid.UUID {
	if claims, ok := httpserver.AuthClaimsFromContext(r.Context()); ok {
		return claims.AdminUserID
	}
	return uuid.Nil
}

// parseID は path param {id} を UUID へ parse する。
// 不正な UUID は CodeInvalidRequest（400）の `*errors.Error` を返す（design.md API Contract）。
func parseID(r *http.Request) (uuid.UUID, error) {
	raw := chi.URLParam(r, "id")
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, pkgerrors.New(pkgerrors.CodeInvalidRequest, "invalid tenant id")
	}
	return id, nil
}

// decodeJSON は request body を v へ JSON decode する。
// decode 失敗（malformed JSON / 型不一致）は CodeInvalidRequest（400）の `*errors.Error` を返す。
// body が空（EOF）の場合も不正入力として 400 を返す。
func decodeJSON(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return pkgerrors.New(pkgerrors.CodeInvalidRequest, "invalid request body")
	}
	return nil
}

// decodeJSONAllowEmpty は request body を v へ JSON decode するが、空 body（EOF）は
// エラーとせず v を zero value のまま残す。`DELETE /tenants/{id}` のように body が無い
// （= 二段階確認テキスト未入力）リクエストを Handler 入口の 400 ではなく、Service の確認未完了
// 判定（422 / Req 3.2）へ委ねるために用いる。malformed JSON / 型不一致は通常どおり 400 を返す。
func decodeJSONAllowEmpty(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		if stderrors.Is(err, io.EOF) {
			return nil
		}
		return pkgerrors.New(pkgerrors.CodeInvalidRequest, "invalid request body")
	}
	return nil
}

// writeJSON は status code と JSON body を応答に書き出す共通ヘルパ。
// encode 失敗は header 送出済みのため諦める（`errors.WriteHTTP` の方針と同じ）。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
