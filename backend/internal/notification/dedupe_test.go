package notification

import (
	stderrors "errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// TestMarkProcessedSQL_ContainsOnConflictDoNothing は dedupe 記録 INSERT 文が
// ON CONFLICT (message_id) DO NOTHING を含むことを検証する（Req 1.2 / 1.3 = 並行 / 再配信の同一
// MessageID 記録を PK + ON CONFLICT で 1 行へ収束。audit の SQL 文字列 assert と同型の
// documenting テスト）。dispatch 経路では handler 成功後に本 SQL で記録する（record-after-success）。
func TestMarkProcessedSQL_ContainsOnConflictDoNothing(t *testing.T) {
	// Arrange / Act: markProcessedSQL は const のため直接検証する。

	// Assert
	if !strings.Contains(markProcessedSQL, "INSERT INTO notification_dedupe") {
		t.Errorf("notification_dedupe への INSERT であるべき: got %q", markProcessedSQL)
	}
	if !strings.Contains(markProcessedSQL, "ON CONFLICT (message_id) DO NOTHING") {
		t.Errorf("ON CONFLICT (message_id) DO NOTHING を含むべき（冪等機構 / Req 1.2 / 1.3）: got %q", markProcessedSQL)
	}
	// $1 = message_id / $2 = notification_type の 2 引数を bind する。
	if !strings.Contains(markProcessedSQL, "(message_id, notification_type)") {
		t.Errorf("message_id / notification_type の 2 列を INSERT すべき: got %q", markProcessedSQL)
	}
	if !strings.Contains(markProcessedSQL, "VALUES ($1, $2)") {
		t.Errorf("$1 / $2 のプレースホルダで bind すべき: got %q", markProcessedSQL)
	}
}

// TestWrapDedupePersistErr_IsNilSafeAndIdempotent は wrapDedupePersistErr が
// BeginTxFunc の fn 内部 / 外部の両 wrap 点で共用できるよう、nil を素通しし、既に transient な
// *errors.Error を二重 wrap しない一方、非 transient error（外側 tx 失敗の CodeInternal 等）は
// transient に正規化することを検証する（Req 1.4 = 外側 tx 失敗も再処理保持へ倒す）。
func TestWrapDedupePersistErr_IsNilSafeAndIdempotent(t *testing.T) {
	t.Run("nil のとき nil を返す（成功経路をエラー化しない）", func(t *testing.T) {
		if got := wrapDedupePersistErr(nil); got != nil {
			t.Errorf("nil を返すべき: got %v", got)
		}
	})

	t.Run("既に transient *Error なら同一 error をそのまま返す（二重 wrap しない）", func(t *testing.T) {
		// Arrange
		already := &pkgerrors.Error{Code: pkgerrors.CodeUnavailable, Message: "x", IsTransient: true}
		// Act
		got := wrapDedupePersistErr(already)
		// Assert
		var de *pkgerrors.Error
		if !stderrors.As(got, &de) || de != already {
			t.Errorf("transient *Error は二重 wrap せずそのまま返すべき: got %#v", got)
		}
	})

	t.Run("非 transient error（外側 tx 失敗）を transient に正規化する", func(t *testing.T) {
		// Arrange: BeginTx / Commit 失敗を模した CodeInternal / 非 transient な *Error。
		txFail := &pkgerrors.Error{Code: pkgerrors.CodeInternal, Message: "tx begin failed", IsTransient: false}
		// Act
		got := wrapDedupePersistErr(txFail)
		// Assert
		var de *pkgerrors.Error
		if !stderrors.As(got, &de) {
			t.Fatalf("*errors.Error で写像されるべき: got %T", got)
		}
		if !de.IsTransient {
			t.Errorf("外側 tx 失敗は IsTransient=true へ正規化されるべき（nack 保持 / Req 1.4）: got false")
		}
		if de.Code != pkgerrors.CodeUnavailable {
			t.Errorf("Code は CodeUnavailable であるべき: got %q", de.Code)
		}
		if !stderrors.Is(got, txFail) {
			t.Errorf("cause が errors.Is で辿れるべき: got %v", got)
		}
	})
}

// TestWrapDedupePersistErr_MapsToTransientUnavailable は永続化失敗が
// *errors.Error{Code: CodeUnavailable, IsTransient: true} へ写像され、cause が保持されることを
// 検証する（Req 1.4 = 永続化失敗は完了扱いにせず再処理対象として保持 → worker nack）。
func TestWrapDedupePersistErr_MapsToTransientUnavailable(t *testing.T) {
	// Arrange
	cause := stderrors.New("connection refused")

	// Act
	err := wrapDedupePersistErr(cause)

	// Assert
	var de *pkgerrors.Error
	if !stderrors.As(err, &de) {
		t.Fatalf("*errors.Error で写像されるべき: got %T (%v)", err, err)
	}
	if de.Code != pkgerrors.CodeUnavailable {
		t.Errorf("Code は CodeUnavailable であるべき: got %q", de.Code)
	}
	if !de.IsTransient {
		t.Errorf("永続化失敗は IsTransient=true であるべき（nack 保持 / Req 1.4）: got false")
	}
	if !stderrors.Is(err, cause) {
		t.Errorf("cause が errors.Is で辿れるべき: got %v", err)
	}
	// 機密値（query 生値）を message 本文に補間しない固定文言（NFR 3.1）。
	if strings.Contains(de.Message, "connection refused") {
		t.Errorf("message に cause 文言を補間すべきでない（固定文言 / NFR 3.1）: got %q", de.Message)
	}
}

// TestMapIsProcessedScan は IsProcessed の scan 結果 → bool/err 写像（純粋関数）を検証する。
//
//   - scan 成功（行あり）   → 既処理 true / 非エラー（Req 1.2）
//   - pgx.ErrNoRows（行なし） → 未処理 false / 非エラー（Req 1.2）
//   - その他の DB 失敗      → false / transient *errors.Error（Req 1.4）
func TestMapIsProcessedScan(t *testing.T) {
	dbFail := stderrors.New("db is down")

	tests := []struct {
		name          string
		scanErr       error
		wantProcessed bool
		wantErr       bool
		wantTransient bool
	}{
		{name: "行ありのとき既処理 true を返す", scanErr: nil, wantProcessed: true, wantErr: false},
		{name: "pgx.ErrNoRows のとき未処理 false を非エラーで返す", scanErr: pgx.ErrNoRows, wantProcessed: false, wantErr: false},
		{name: "その他 DB 失敗のとき transient エラーを返す", scanErr: dbFail, wantProcessed: false, wantErr: true, wantTransient: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange / Act
			processed, err := mapIsProcessedScan(tt.scanErr)

			// Assert
			if processed != tt.wantProcessed {
				t.Errorf("processed mismatch: want %v, got %v", tt.wantProcessed, processed)
			}
			if tt.wantErr {
				if err == nil {
					t.Fatalf("DB 失敗時は error を返すべき: got nil")
				}
				var de *pkgerrors.Error
				if !stderrors.As(err, &de) {
					t.Fatalf("*errors.Error で写像されるべき: got %T", err)
				}
				if de.IsTransient != tt.wantTransient {
					t.Errorf("IsTransient mismatch: want %v, got %v", tt.wantTransient, de.IsTransient)
				}
				if de.Code != pkgerrors.CodeUnavailable {
					t.Errorf("Code は CodeUnavailable であるべき: got %q", de.Code)
				}
				return
			}
			if err != nil {
				t.Fatalf("非エラーであるべき: got %v", err)
			}
		})
	}
}

// TestIsProcessedSQL_SelectsByMessageID は既処理判定 SELECT が message_id を条件にすることを
// 検証する（Req 1.2 = 既処理 MessageID 判定）。
func TestIsProcessedSQL_SelectsByMessageID(t *testing.T) {
	// Arrange / Act / Assert
	if !strings.Contains(isProcessedSQL, "FROM notification_dedupe") {
		t.Errorf("notification_dedupe を参照すべき: got %q", isProcessedSQL)
	}
	if !strings.Contains(isProcessedSQL, "WHERE message_id = $1") {
		t.Errorf("message_id を条件にすべき: got %q", isProcessedSQL)
	}
}
