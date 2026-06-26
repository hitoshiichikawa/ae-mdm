package auth

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"
)

// (a) New() が 32 byte 相当の base64url 文字列を返す（長さ・文字種）
//
// base64.RawURLEncoding は no-padding なので 32 byte → 43 文字。文字種は
// base64url のアルファベット（A-Z, a-z, 0-9, '-', '_'）のみが許容され、
// 標準 base64 の '+' / '/' / '=' は出現しないことを確認する。
func TestSession_New_ReturnsBase64URLOf32Bytes(t *testing.T) {
	// Act
	raw, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Assert: 長さは ceil(32*4/3) = 43 文字
	const wantLen = 43
	if len(raw) != wantLen {
		t.Errorf("len(raw) = %d, want %d", len(raw), wantLen)
	}

	// 文字種: base64url アルファベットのみ
	for i, r := range raw {
		switch {
		case r >= 'A' && r <= 'Z':
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			t.Errorf("raw[%d] = %q is not in base64url alphabet (raw=%q)", i, r, raw)
		}
	}

	// 標準 base64 の '+' / '/' / '=' は出現しない（URL-safe + no-padding）
	if strings.ContainsAny(raw, "+/=") {
		t.Errorf("raw contains non-URL-safe / padding chars: %q", raw)
	}

	// decode して 32 byte が復元されることも確認（base64url no-padding として valid）
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		t.Fatalf("base64.RawURLEncoding.DecodeString: %v", err)
	}
	if len(decoded) != sessionRawTokenBytes {
		t.Errorf("len(decoded) = %d, want %d", len(decoded), sessionRawTokenBytes)
	}
}

// (b) New() を 100 回呼んで重複なし（乱数性の sanity check）
//
// 完全な統計検定ではないが、`crypto/rand.Read` を経由している前提で 100 ループ中の
// 衝突確率は無視できる（256 bit 空間からの抽出）。衝突したら確実に乱数源不良。
func TestSession_New_100CallsProduceUniqueTokens(t *testing.T) {
	const iterations = 100
	seen := make(map[string]struct{}, iterations)

	for i := 0; i < iterations; i++ {
		raw, err := New()
		if err != nil {
			t.Fatalf("iter %d: New: %v", i, err)
		}
		if _, dup := seen[raw]; dup {
			t.Fatalf("iter %d: duplicate token detected: %q", i, raw)
		}
		seen[raw] = struct{}{}
	}

	if len(seen) != iterations {
		t.Errorf("unique tokens = %d, want %d", len(seen), iterations)
	}
}

// (c) HashToken が SHA-256 hex（64 文字）を返す
//
// SHA-256 は 32 byte → hex 表記で 64 文字。lowercase a-f / 0-9 のみ。
func TestSession_HashToken_ReturnsSHA256Hex(t *testing.T) {
	// Arrange
	raw := "fixed-token-for-hash-shape-check"

	// Act
	hash := HashToken(raw)

	// Assert: 長さ 64 文字
	const wantLen = 64
	if len(hash) != wantLen {
		t.Errorf("len(hash) = %d, want %d", len(hash), wantLen)
	}

	// 文字種: lowercase hex のみ
	for i, r := range hash {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		default:
			t.Errorf("hash[%d] = %q is not lowercase hex (hash=%q)", i, r, hash)
		}
	}
}

// (d) HashToken の冪等性: 同一入力に対して同一出力を返す
func TestSession_HashToken_IsIdempotent(t *testing.T) {
	// Arrange
	raw := "fixed-token-for-idempotency-check"

	// Act
	first := HashToken(raw)
	second := HashToken(raw)
	third := HashToken(raw)

	// Assert
	if first != second {
		t.Errorf("HashToken not idempotent: first=%q second=%q", first, second)
	}
	if second != third {
		t.Errorf("HashToken not idempotent: second=%q third=%q", second, third)
	}

	// 異なる入力では異なる出力（衝突しない sanity check）
	other := HashToken(raw + "x")
	if other == first {
		t.Errorf("HashToken collided for distinct inputs: %q == %q", other, first)
	}
}

// (e) SessionCookieAttributes(ttl) の Name / Secure / HttpOnly / SameSite / Path 値 +
// MaxAge == int(ttl/time.Second) 検証（ttl = 8h で MaxAge == 28800 等）
func TestSession_SessionCookieAttributes_ReturnsExpectedAttributes(t *testing.T) {
	// Arrange: design.md / Req 4.2 の absolute timeout = 8 時間を代表値として使用
	ttl := 8 * time.Hour

	// Act
	c := SessionCookieAttributes(ttl)

	// Assert: 名前
	if c.Name != sessionCookieName {
		t.Errorf("Name = %q, want %q", c.Name, sessionCookieName)
	}
	if c.Name != "__Host-ae_mdm_session" {
		t.Errorf("Name constant mismatch: %q", c.Name)
	}

	// Assert: 保護属性（Req 3.2 / 3.3 / 3.4）
	if !c.HttpOnly {
		t.Errorf("HttpOnly = false, want true (Req 3.2)")
	}
	if !c.Secure {
		t.Errorf("Secure = false, want true (Req 3.3)")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want SameSiteLaxMode (Req 3.4)", c.SameSite)
	}
	if c.Path != "/" {
		t.Errorf("Path = %q, want \"/\" (__Host- prefix 要求)", c.Path)
	}

	// Assert: MaxAge == int(ttl/time.Second) = 28800 秒
	const want = 28800
	if c.MaxAge != want {
		t.Errorf("MaxAge = %d, want %d (ttl=8h)", c.MaxAge, want)
	}
	if c.MaxAge != int(ttl/time.Second) {
		t.Errorf("MaxAge = %d, want int(ttl/time.Second) = %d", c.MaxAge, int(ttl/time.Second))
	}
}

// (e2) SessionCookieAttributes の MaxAge 計算が ttl の単位スケールに一貫して追従する
//
// 30 分 / 8 時間 / 24 時間の代表 ttl で int(ttl/time.Second) と一致することを確認。
func TestSession_SessionCookieAttributes_MaxAgeScalesWithTTL(t *testing.T) {
	cases := []struct {
		name string
		ttl  time.Duration
		want int
	}{
		{"30m", 30 * time.Minute, 1800},
		{"8h", 8 * time.Hour, 28800},
		{"24h", 24 * time.Hour, 86400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := SessionCookieAttributes(tc.ttl)
			if c.MaxAge != tc.want {
				t.Errorf("MaxAge = %d, want %d (ttl=%s)", c.MaxAge, tc.want, tc.ttl)
			}
		})
	}
}

// (f) SessionExpireCookieAttributes の MaxAge < 0 確認（Go の http.Cookie 削除規約）+
// 具体値 -1
func TestSession_SessionExpireCookieAttributes_ReturnsDeletionCookie(t *testing.T) {
	// Act
	c := SessionExpireCookieAttributes()

	// Assert: 名前は session cookie 名（state cookie と取り違えない）
	if c.Name != sessionCookieName {
		t.Errorf("Name = %q, want %q", c.Name, sessionCookieName)
	}
	if c.Name != "__Host-ae_mdm_session" {
		t.Errorf("Name constant mismatch: %q", c.Name)
	}

	// Assert: Value は空文字（削除 cookie）
	if c.Value != "" {
		t.Errorf("Value = %q, want \"\"", c.Value)
	}

	// Assert: Go の http.Cookie 規約 — MaxAge < 0 のみが削除 cookie として扱われる
	if c.MaxAge >= 0 {
		t.Errorf("MaxAge = %d, want negative (deletion cookie / Go http.Cookie 規約)", c.MaxAge)
	}
	if c.MaxAge != -1 {
		t.Errorf("MaxAge = %d, want -1 specifically", c.MaxAge)
	}

	// Assert: 保護属性は維持（削除 cookie でも __Host- prefix の要件は変わらない）
	if c.Path != "/" {
		t.Errorf("Path = %q, want \"/\"", c.Path)
	}
	if !c.HttpOnly {
		t.Errorf("HttpOnly = false, want true")
	}
	if !c.Secure {
		t.Errorf("Secure = false, want true")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want SameSiteLaxMode", c.SameSite)
	}
}

// (g) HashPrefix が 8 文字（Req 3.8）
//
// 構造化ログ用の短縮識別子は SHA-256 hex の先頭 8 文字（4 byte 相当の情報量）。
// 通常入力（64 文字 hash）に対して 8 文字を返すこと、および境界（空文字 /
// 8 文字未満）で defensive 挙動を取ることを確認する。
func TestSession_HashPrefix_Returns8Chars(t *testing.T) {
	// Arrange: HashToken の戻り値（64 文字）を典型入力とする
	hash := HashToken("input-for-prefix-check")
	if len(hash) != 64 {
		t.Fatalf("precondition: len(hash) = %d, want 64", len(hash))
	}

	// Act
	prefix := HashPrefix(hash)

	// Assert: 8 文字 / hash の先頭 8 文字と一致
	if len(prefix) != 8 {
		t.Errorf("len(prefix) = %d, want 8", len(prefix))
	}
	if prefix != hash[:8] {
		t.Errorf("prefix = %q, want %q", prefix, hash[:8])
	}
}

// (g2) HashPrefix の defensive 境界: 空文字入力には空文字を返す
func TestSession_HashPrefix_EmptyInputReturnsEmpty(t *testing.T) {
	got := HashPrefix("")
	if got != "" {
		t.Errorf("HashPrefix(\"\") = %q, want \"\"", got)
	}
}

// (g3) HashPrefix の defensive 境界: 8 文字未満の入力は丸ごと返す
func TestSession_HashPrefix_ShortInputReturnsFullInput(t *testing.T) {
	short := "abc1234" // 7 文字
	got := HashPrefix(short)
	if got != short {
		t.Errorf("HashPrefix(%q) = %q, want %q (短入力は丸ごと返す)", short, got, short)
	}
}
