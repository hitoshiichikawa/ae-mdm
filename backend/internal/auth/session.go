package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"time"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// sessionCookieName は session cookie の Set-Cookie 名。
//
// `__Host-` プレフィックスにより以下が強制される（design.md「Session Manager」節 / Req 3.2 〜 3.4）:
//   - Path=/ 必須
//   - Secure 必須
//   - Domain 属性指定不可（sub-domain 跨ぎを物理的に禁止 / requirements.md 確認事項 4 対応）
const sessionCookieName = "__Host-ae_mdm_session"

// sessionRawTokenBytes は session 生 token の byte 長。
//
// design.md「Session Manager」節および Req 3.5 と整合。32 byte の暗号学的乱数を
// `crypto/rand.Read` で取得し、`base64.RawURLEncoding`（no-padding）で文字列化する
// （= cookie value）。base64url 文字列長は ceil(32*4/3) = 43 文字となる。
const sessionRawTokenBytes = 32

// sessionHashPrefixLen は構造化ログに surface する hash の先頭文字数（Req 3.8）。
//
// SHA-256 hex（64 文字）の先頭 8 文字（4 byte 相当の情報量）を短縮識別子として使う。
// 生 token や全 hash は出力しない（Req 3.6 / 3.7 / NFR 1.1 / NFR 1.2）。
const sessionHashPrefixLen = 8

// New は opaque な session ID（cookie に格納する raw token）を 32 byte の
// `crypto/rand.Read` から生成し、base64url no-padding で文字列化して返す。
//
// design.md「Session Manager」節 / Req 3.5 / NFR 3.1 と整合。`crypto/rand.Read` の err は
// fail-closed のため呼び出し側で **必ず** チェックする（rand 不在環境では session 発行不可
// として 5xx に倒す）。
//
// 機密値の非埋込契約（NFR 1.1 / NFR 4.2）: err wrap には raw token を含めない。
// 失敗パスは `crypto/rand.Read` 自体のエラーのみを wrap する。
func New() (string, error) {
	buf := make([]byte, sessionRawTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		// rand 失敗は OS / 環境起因の致命的状態。Code は CodeInternal（5xx 系）。
		// 機密値（buf の partial 内容）はメッセージに含めない（fixed message のみ）。
		return "", pkgerrors.Wrap(pkgerrors.CodeInternal, "session token rand read failed", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// HashToken は raw token の SHA-256 hex（64 文字 / lowercase）を返す。
//
// 永続ストア（sessions.token_hash）の lookup key および同値性比較に使う。生 token を
// カラムに記録せず、hash のみを保存する（Req 3.6 / 3.7 / NFR 1.2）。
func HashToken(rawToken string) string {
	sum := sha256.Sum256([]byte(rawToken))
	return hex.EncodeToString(sum[:])
}

// SessionCookieAttributes は session cookie の Set-Cookie 属性を返す。
//
// design.md「Session Manager」節および Req 3.2 〜 3.4 と整合。Value は呼び出し側が
// `New()` の戻り値で設定する（本 helper は属性のみを提供）。`ttl` には
// `cfg.SessionAbsoluteTimeout` を渡し、cookie の MaxAge を永続ストア側 absolute
// timeout と一致させる（実際の失効判定は DB 側、ブラウザ側 cookie の物理的寿命も合わせる）。
//
// 命名について: state cookie 用 `auth.CookieAttributes`（state.go）と関数名が衝突する
// ため、本 helper は **`Session` プレフィックス付き**で命名する（design.md / tasks.md の
// 散文では `session.CookieAttributes` のような sub-package 風表記を使うが、auth package を
// フラット配置する task 3.1 の判断と整合させるための rename）。属性値・契約は spec の指示
// 通りに維持している。
func SessionCookieAttributes(ttl time.Duration) http.Cookie {
	return http.Cookie{
		Name:     sessionCookieName,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(ttl / time.Second),
	}
}

// SessionExpireCookieAttributes は session cookie の **削除用** Set-Cookie 属性を返す。
//
// Go の `http.Cookie` 規約: `MaxAge < 0` のみが削除 cookie として `Set-Cookie` ヘッダに
// `Max-Age=0` 属性 + 過去日付の `Expires` 属性を出力する。`MaxAge = 0` だと Max-Age 属性
// 自体が省略され session cookie 化（ブラウザを閉じるまで保持）して即時削除が成立しない。
// design.md「Session Manager」節 / Req 5.x（logout）/ Req 4.7（失効時の削除レスポンス）と整合。
//
// state cookie 用の `auth.ExpireCookieAttributes()`（state.go）とは Name が異なるため、
// session cookie 削除には必ず本 helper を使用する。命名について `SessionCookieAttributes`
// と同じく `Session` プレフィックスを付与し、state cookie helper との関数名衝突を回避する。
func SessionExpireCookieAttributes() http.Cookie {
	return http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	}
}

// HashPrefix は logger field に載せる短縮識別子（hash 先頭 8 文字）を返す（Req 3.8）。
//
// 引数 hash は `HashToken` の戻り値（SHA-256 hex 64 文字）を想定するが、入力長が
// `sessionHashPrefixLen` 未満の場合は丸ごと返す（defensive。空文字には空文字を返す）。
// 構造化ログには本 helper の出力 + 「session_hash_prefix」field key を組み合わせて出力し、
// 全 hash や raw token は出力しない（Req 3.6 / 3.7 / 3.8 / NFR 1.1）。
func HashPrefix(hash string) string {
	if len(hash) <= sessionHashPrefixLen {
		return hash
	}
	return hash[:sessionHashPrefixLen]
}
