// Package httpserver は ae-mdm の HTTP server を構築する Platform Layer のモジュール。
//
// 本パッケージは requirements.md Req 5.1〜5.6 / design.md Components: Tenant Context
// Middleware / HTTP Server Bootstrap 節に対応する。
//
// 主に以下 3 つの責務を持つ:
//   - chi router + middleware chain（recover / request_id / access log）の組み立て
//   - `/api` と `/api/admin` の 2 サブルータ mount
//   - TenantContext を request context に確立し、`/api/admin` 配下に SuperAdmin ガードを挟む
//
// 依存方向: config / errors / logger / platform/db を import 可。逆方向は禁止。
package httpserver

import (
	"context"
	stdErrors "errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	internalerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
)

// requestIDHeader はリクエスト / レスポンスで使う request_id の HTTP ヘッダ名。
// クライアントが既に X-Request-ID を送ってきた場合はその値を再利用する（再利用される
// 値が空文字なら新規 uuid v4 を生成）。
const requestIDHeader = "X-Request-ID"

// requestIDCtxKey は request_id を request context に格納する private な key 型。
type requestIDCtxKey struct{}

// RequestIDFromContext は request context から request_id を取り出す。未設定なら空文字。
//
// 後続 Issue のドメインハンドラ / Logger ヘルパが request_id を構造化ログへ載せる際の
// 取り出し口として公開する。
func RequestIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v := ctx.Value(requestIDCtxKey{}); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// AuthClaims は auth middleware が request context に注入する「認証済みクレーム」の
// 内部表現。
//
// Issue #33（OIDC Verifier + Session 管理 / umbrella tasks 3.1）の auth domain
// （`backend/internal/auth`）から `httpserver.WithAuthClaims(ctx, AuthClaims{...})` で
// claims を注入できるよう、本型および関連 helper は public シンボルとして公開する
// （A2 では同型が private（`authClaims`）であり、test 用にのみ内部 helper が露出していた）。
//
// Issue #37（RBAC Authorizer / A3b）で `Console` フィールドを追加した。本フィールドは
// `internal/platform/oidc.Console` の文字列表現（"tenant-console" / "admin-console"）を
// 保持する。oidc パッケージへの逆依存を避けるため string 型で受け、呼び出し側で型変換
// する。zero value（空文字）は本 Issue 以前の auth middleware が注入した legacy claims を
// 表し、`/api/admin/*` ガード（RequireAdminConsoleAndSuperAdmin）は空文字を
// audience 不一致として 403 で reject する fail-closed 経路に倒れる。
//
// TenantContextMiddleware は claims が ctx に存在しない場合に **default deny** で 401 を
// 返し、存在する場合のみ TenantContext を組み立てて next chain に進める入力契約を持つ
// （後段の RequireSuperAdmin / RLS と二重防御を成す）。
type AuthClaims struct {
	TenantID     uuid.UUID
	AdminUserID  uuid.UUID
	Roles        []string
	IsSuperAdmin bool
	// Console は OIDC ID トークンの aud から判別したコンソール種別を string で持つ
	// （"tenant-console" / "admin-console" のいずれか）。Issue #37 で追加。
	Console string
	// SessionHashPrefix は auth middleware が確立した session の token_hash 短縮 prefix
	// （先頭 8 文字）。Issue #37 (#44 PR iteration round 1) で Req 7.3 の denied ログに
	// 載せるために追加。raw token / 全 hash は決して持たない（NFR 1.1 / NFR 4.2）。
	// auth middleware が成功時のみ populate する。zero value（空文字）は legacy 経路
	// （session_hash_prefix 取得経路が無い fixture / 過渡的な後方互換）を表す。
	SessionHashPrefix string
}

// authClaimsCtxKey は AuthClaims を request context に格納する private な key 型。
// package 外からは [WithAuthClaims] / [AuthClaimsFromContext] 経由でのみ操作する
// （key 自体は外部に露出させず、key 型の衝突や直接書込みを防ぐ）。
type authClaimsCtxKey struct{}

// WithAuthClaims は auth middleware（`backend/internal/auth`）が AuthClaims を
// request context に注入するための public helper。test fixture も同経路で利用する。
//
// 本 helper は context.WithValue のラッパであり、ctx に同一 key の既存値がある場合は
// 後勝ちで上書きする標準挙動に従う。
func WithAuthClaims(ctx context.Context, claims AuthClaims) context.Context {
	return context.WithValue(ctx, authClaimsCtxKey{}, claims)
}

// AuthClaimsFromContext は request context から AuthClaims を取り出す。
// 未設定なら (zero, false)。
//
// TenantContextMiddleware が default deny 判定で参照するほか、後続 Issue の handler が
// claims を直接参照する場合の取り出し口としても公開する。
func AuthClaimsFromContext(ctx context.Context) (AuthClaims, bool) {
	if ctx == nil {
		return AuthClaims{}, false
	}
	v := ctx.Value(authClaimsCtxKey{})
	c, ok := v.(AuthClaims)
	return c, ok
}

// accessLogState は AccessLog が確保する状態オブジェクト。inner middleware
// （TenantContextMiddleware）が tenant_id を書き戻すための共有スロットを 1 件持ち、
// next.ServeHTTP が返った後で AccessLog から読み戻す。
//
// PR #31 round-1 / round-3 review 由来: AccessLog は middleware chain の外側に位置し、
// TenantContextMiddleware が `next.ServeHTTP(w, r.WithContext(ctx))` で差し替えた新しい
// request context は AccessLog の外側 r からは見えないため、`r.Context()` 経由だけだと
// tenant_id を access log に乗せられない（Req 2.2 / 5.6 未達）。共有 state pointer を
// 経由することで、middleware chain の外側に位置する AccessLog からも内側で確立された
// tenant_id を読み出せる。
type accessLogState struct {
	tenantID uuid.UUID
}

// accessLogStateCtxKey は accessLogState を request context に格納する private な key 型。
type accessLogStateCtxKey struct{}

// accessLogStateFromContext は request context から *accessLogState を取り出す。
// 未設定なら nil。AccessLog が確保した state を inner middleware から書き戻すために使う。
func accessLogStateFromContext(ctx context.Context) *accessLogState {
	if ctx == nil {
		return nil
	}
	if v := ctx.Value(accessLogStateCtxKey{}); v != nil {
		if s, ok := v.(*accessLogState); ok {
			return s
		}
	}
	return nil
}

// statusRecorder は ResponseWriter をラップし、書き込まれたステータスコードを捕捉する。
// access log の status field と recover middleware の panic 後のステータス判定に用いる。
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

// Write は status を 200 でデフォルトする標準 net/http の挙動と整合させる。
func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wroteHeader {
		s.status = http.StatusOK
		s.wroteHeader = true
	}
	return s.ResponseWriter.Write(b)
}

// Recoverer は handler 内で発生した panic を recover し、500 応答 + ERROR ログに写像する
// middleware。
//
// panic payload が *errors.Error の場合は internalerrors.WriteHTTP 経由でその Code に応じた
// status code を発行する（特に CodeTenantCtxMissing は 500 + ERROR ログとして処理される /
// task 2 の BeginTxFunc panic 契約と一致）。それ以外の panic は CodeInternal で wrap して
// 500 + ERROR ログを出す。
//
// 必ず internalerrors.ClearWriter を defer 呼び出ししてリーク（writer sentinel の残置）を
// 防ぐ（task 1 の WriteHTTP called-once 契約と整合）。
func Recoverer(log logger.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer internalerrors.ClearWriter(w)
			defer func() {
				p := recover()
				if p == nil {
					return
				}
				var domainErr error
				if e, ok := p.(*internalerrors.Error); ok && e != nil {
					domainErr = e
				} else {
					domainErr = internalerrors.Wrap(
						internalerrors.CodeInternal,
						"http: handler panic",
						panicToError(p),
					)
				}
				internalerrors.WriteHTTP(w, r, domainErr, log)
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// panicToError は panic payload を error に変換する。string / error / それ以外を網羅する。
func panicToError(p any) error {
	if err, ok := p.(error); ok {
		return err
	}
	return stdErrors.New(panicString(p))
}

// panicString は payload を表示用文字列に変換する（log 用途）。
func panicString(p any) string {
	if s, ok := p.(string); ok {
		return s
	}
	if err, ok := p.(error); ok {
		return err.Error()
	}
	return "non-error panic value"
}

// RequestID は X-Request-ID ヘッダを応答に乗せ、request context にも格納する middleware。
//
// クライアント側ヘッダ X-Request-ID が空でない場合はその値をそのまま再利用し、
// 空または未設定の場合は新規 uuid v4 を発行する。これにより上流（API Gateway 等）が
// 採番した相関 ID を貫通させつつ、入口で必ず ID が確立されることを保証する。
func RequestID() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get(requestIDHeader)
			if id == "" {
				id = uuid.NewString()
			}
			w.Header().Set(requestIDHeader, id)
			ctx := context.WithValue(r.Context(), requestIDCtxKey{}, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// AccessLog は HTTP リクエストの method / path / status / duration_ms / request_id /
// tenant_id（あれば）を構造化 INFO ログとして出力する middleware。
//
// duration の測定は handler 開始前の `time.Now()` を起点とする。status は statusRecorder
// で捕捉する（handler が WriteHeader を呼ばずに完了した場合は 200 default）。
//
// log が nil の場合 INFO 出力は no-op（handler 実行は通常通り）。
//
// tenant_id 取得は 2 経路を順に試す（PR #31 round-1 / round-3 review 由来）:
//  1. accessLogState pointer 経由: inner TenantContextMiddleware が claims から確立した
//     tenant_id を書き戻す。これにより AccessLog が middleware chain の外側にあっても
//     access log に tenant_id が乗る（Req 2.2 / 5.6）。
//  2. r.Context() 経由の TenantContext: テスト fixture や直接 db.WithTenantContext で
//     pre-inject される経路（test compat）。
//
// statusRecorder は `internalerrors.WriteHTTP` の sentinel sync.Map にエントリされうるため、
// defer ClearWriter(rec) で必ず除去する（PR #31 round-1 / round-3 review: 401/403/404
// 応答ごとに rec が leak し sync.Map が増え続ける問題への対処）。
func AccessLog(log logger.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			defer internalerrors.ClearWriter(rec)
			state := &accessLogState{}
			ctx := context.WithValue(r.Context(), accessLogStateCtxKey{}, state)
			next.ServeHTTP(rec, r.WithContext(ctx))
			if log == nil {
				return
			}
			fields := []any{
				"method", r.Method,
				"path", r.URL.Path,
				"status", recordedStatus(rec),
				"duration_ms", time.Since(start).Milliseconds(),
				"request_id", RequestIDFromContext(r.Context()),
			}
			tenantID := state.tenantID
			if tenantID == uuid.Nil {
				if tc, err := db.FromContext(r.Context()); err == nil {
					tenantID = tc.TenantID
				}
			}
			if tenantID != uuid.Nil {
				fields = append(fields, "tenant_id", tenantID.String())
			}
			log.Info("http_access", fields...)
		})
	}
}

// recordedStatus は statusRecorder が捕捉した値を返す。handler が WriteHeader を呼ばず
// completed した場合は 200 を返す（net/http の default 挙動と揃える）。
func recordedStatus(rec *statusRecorder) int {
	if rec == nil || rec.status == 0 {
		return http.StatusOK
	}
	return rec.status
}

// TenantContextMiddleware は本 Issue のスコープにおける **default deny** な
// TenantContext 確立 middleware。
//
// 入力契約（design.md「Tenant Context Middleware」節 / 同節 Invariants「tenant_id を
// 持たないリクエストが `/api/...` に到達した場合 401 を返す」と整合）:
//   - 上流の auth middleware（`backend/internal/auth`）が
//     [WithAuthClaims] で `AuthClaims` を request context に注入する前提のアダプタとして
//     実装する（Issue #33 の OIDC Verifier + Session 管理経路）
//   - claims が ctx に存在しない場合（default deny 状態）は TenantContext を確立せず、
//     `*errors.Error{Code: CodeUnauthenticated}` で **401 を返して chain を終端**する
//     （`/api/...` 配下のハンドラ未実装でも認証ガードが先に発火するため 401 で閉じる /
//     後段の DB アクセスは BeginTxFunc panic + RLS の二重防御で更に止まる）
//   - claims が存在する場合は TenantContext を組み立てて db.WithTenantContext で put し、
//     next.ServeHTTP に進める（TenantID / AdminUserID / Roles / IsSuperAdmin を転記）
func TenantContextMiddleware(log logger.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			claims, ok := AuthClaimsFromContext(ctx)
			if !ok {
				// default deny: claims 不在は未認証として 401 を返し、chain を終端する。
				internalerrors.WriteHTTP(w, r, internalerrors.New(
					internalerrors.CodeUnauthenticated,
					"authentication required",
				), log)
				return
			}
			tc := db.TenantContext{
				TenantID:     claims.TenantID,
				AdminUserID:  claims.AdminUserID,
				Roles:        append([]string(nil), claims.Roles...),
				IsSuperAdmin: claims.IsSuperAdmin,
			}
			// AccessLog が確保した state pointer に tenant_id を書き戻す。AccessLog は
			// middleware chain の外側にあり、`r.WithContext(ctx)` で差し替えた context は
			// 外側から見えないため、共有 state pointer 経由で tenant_id を伝播させる
			// （PR #31 round-1 / round-3 review 由来）。
			if s := accessLogStateFromContext(ctx); s != nil {
				s.tenantID = tc.TenantID
			}
			ctx = db.WithTenantContext(ctx, tc)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
