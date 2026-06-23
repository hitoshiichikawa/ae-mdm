package db

import (
	stdErrors "errors"
	"context"
	"testing"
	"time"

	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	internalerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// TestNewPool_InvalidConnString_ReturnsServiceUnavailable は requirements.md Req 4.1 /
// NFR 3.1 に対応する。pgxpool.ParseConfig が失敗するような不正な接続文字列を渡したとき
// *errors.Error{Code: CodeUnavailable} が返ることを確認する。
func TestNewPool_InvalidConnString_ReturnsServiceUnavailable(t *testing.T) {
	// Arrange: ParseConfig が失敗する明らかに不正な接続文字列を渡す。
	cfg := config.Config{
		DatabaseURL: "not-a-valid-conn-string://%%%",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// Act
	pool, err := NewPool(ctx, cfg)

	// Assert
	if pool != nil {
		t.Fatalf("pool は nil を期待したが %T が返った", pool)
	}
	if err == nil {
		t.Fatalf("err == nil; CodeUnavailable を期待")
	}
	var de *internalerrors.Error
	if !stdErrors.As(err, &de) {
		t.Fatalf("err は *errors.Error として取り出せること, got %T (%v)", err, err)
	}
	if de.Code != internalerrors.CodeUnavailable {
		t.Errorf("Code=%q; want %q", de.Code, internalerrors.CodeUnavailable)
	}
	if de.Cause == nil {
		t.Errorf("Cause が wrap されていない（ParseConfig の原因 error を保持すべき）")
	}
}

// TestNewPool_UnreachableHost_ReturnsServiceUnavailable は requirements.md Req 4.1 /
// NFR 3.1 / NFR 3.2 に対応する。ParseConfig は成功するが Ping で疎通できない
// 接続文字列を short context timeout で叩いたとき、*errors.Error{Code: CodeUnavailable}
// が返ることを確認する。
//
// 実 PostgreSQL を必要としない方針（design.md の test 戦略・tasks.md 2.1 詳細項目と整合）。
// CI 環境で 127.0.0.1:1 への TCP 接続が即座に refused されることに依拠し、context timeout
// は 500ms の短時間に設定する。
func TestNewPool_UnreachableHost_ReturnsServiceUnavailable(t *testing.T) {
	// Arrange: 文法上は valid だが、解決可能でも実サービスは port 1 で listen していない。
	// connect_timeout=1 を URL に含めて長期ブロックを防ぐ。
	cfg := config.Config{
		DatabaseURL: "postgres://user:pass@127.0.0.1:1/db?sslmode=disable&connect_timeout=1",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Act
	pool, err := NewPool(ctx, cfg)

	// Assert
	if pool != nil {
		// Pool が返ってきた場合、テスト終了時に Close しておく（leak 防止）。
		pool.Close()
		t.Fatalf("pool != nil; Ping 失敗で nil 戻りを期待")
	}
	if err == nil {
		t.Fatalf("err == nil; CodeUnavailable を期待")
	}
	var de *internalerrors.Error
	if !stdErrors.As(err, &de) {
		t.Fatalf("err は *errors.Error として取り出せること, got %T (%v)", err, err)
	}
	if de.Code != internalerrors.CodeUnavailable {
		t.Errorf("Code=%q; want %q", de.Code, internalerrors.CodeUnavailable)
	}
}
