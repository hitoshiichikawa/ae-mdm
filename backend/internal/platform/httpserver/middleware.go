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

// authClaims は **本 Issue のスコープ外**の auth middleware が
// request context に注入する「認証済みクレーム」の内部表現。
//
// 後続 Issue（umbrella tasks 3.1: OIDC Verifier / Session Manager）で OIDC 検証
// 経路が実装された際に同型を再利用する想定。本 Issue ではあくまで
// TenantContextMiddleware の入力契約 *だけ* を確定させる目的で、claims を **何も
// 注入しない default deny 状態** を成立させる。
//
// auth middleware が claims を注入する API は内部 helper [withAuthClaims] のみで提供し、
// 公開はしない（後続 Issue で auth package を切り出した時点で public 化する想定）。
type authClaims struct {
	TenantID     uuid.UUID
	AdminUserID  uuid.UUID
	Roles        []string
	IsSuperAdmin bool
}

// authClaimsCtxKey は authClaims を request context に格納する private な key 型。
type authClaimsCtxKey struct{}

// withAuthClaims は test fixture および後続 Issue の auth middleware から claims を
// request context に注入するための内部 helper。本 Issue ではテスト用 router からのみ
// 使う（package 外から呼べないため auth スタブの default deny を物理的に成立させる）。
func withAuthClaims(ctx context.Context, claims authClaims) context.Context {
	return context.WithValue(ctx, authClaimsCtxKey{}, claims)
}

// authClaimsFromContext は request context から authClaims を取り出す。
// 未設定なら (zero, false)。
func authClaimsFromContext(ctx context.Context) (authClaims, bool) {
	if ctx == nil {
		return authClaims{}, false
	}
	v := ctx.Value(authClaimsCtxKey{})
	c, ok := v.(authClaims)
	return c, ok
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
func AccessLog(log logger.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
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
			if tc, err := db.FromContext(r.Context()); err == nil && tc.TenantID != uuid.Nil {
				fields = append(fields, "tenant_id", tc.TenantID.String())
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
//   - 本 Issue では auth middleware の実装本体が未定であるため、上流の auth スタブが
//     `*authClaims` を request context に注入する前提のアダプタとして実装する
//   - claims が ctx に存在しない場合（default deny 状態）は TenantContext を確立せず、
//     `*errors.Error{Code: CodeUnauthenticated}` で **401 を返して chain を終端**する
//     （`/api/...` 配下のハンドラ未実装でも認証ガードが先に発火するため 401 で閉じる /
//     後段の DB アクセスは BeginTxFunc panic + RLS の二重防御で更に止まる）
//   - claims が存在する場合は TenantContext を組み立てて db.WithTenantContext で put し、
//     next.ServeHTTP に進める（TenantID / AdminUserID / Roles / IsSuperAdmin を転記）
//
// 後続 Issue で auth middleware 本体（OIDC 検証 / Session lookup）が実装された段階で、
// 本関数の入力契約（authClaims を ctx 経由で受け取る）は維持されたまま auth スタブが
// 実 claims を注入するようになる。
func TenantContextMiddleware(log logger.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			claims, ok := authClaimsFromContext(ctx)
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
			ctx = db.WithTenantContext(ctx, tc)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
