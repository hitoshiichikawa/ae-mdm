package notification

import (
	stderrors "errors"
	"strings"
	"testing"
	"time"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// TestEnqueueUnassignedSQL_InsertsExpectedColumns は退避 INSERT 文が
// unassigned_notifications の期待列（id / message_id / notification_type / enterprise_name /
// payload）を $1〜$5 で bind することを検証する（Req 3.2 = 退避 INSERT / audit の SQL 文字列
// assert と同型の documenting テスト）。
func TestEnqueueUnassignedSQL_InsertsExpectedColumns(t *testing.T) {
	// Arrange / Act: enqueueUnassignedSQL は const のため直接検証する。

	// Assert
	if !strings.Contains(enqueueUnassignedSQL, "INSERT INTO unassigned_notifications") {
		t.Errorf("unassigned_notifications への INSERT であるべき: got %q", enqueueUnassignedSQL)
	}
	if !strings.Contains(enqueueUnassignedSQL, "(id, message_id, notification_type, enterprise_name, payload)") {
		t.Errorf("id / message_id / notification_type / enterprise_name / payload の 5 列を INSERT すべき: got %q", enqueueUnassignedSQL)
	}
	if !strings.Contains(enqueueUnassignedSQL, "VALUES ($1, $2, $3, $4, $5)") {
		t.Errorf("$1〜$5 のプレースホルダで bind すべき: got %q", enqueueUnassignedSQL)
	}
	// received_at は DB の DEFAULT now() に委ねるため INSERT 列に含めない。
	if strings.Contains(enqueueUnassignedSQL, "received_at") {
		t.Errorf("received_at は DEFAULT now() に委ねるため INSERT 列に含めるべきでない: got %q", enqueueUnassignedSQL)
	}
}

// TestBuildUnassignedListQuery は Filter（from/to/type）から組み立てる List クエリの WHERE 句と
// bind args が AC 通りに構築されることを検証する（Req 4.1 = 全件 / Req 4.2 = 絞り込み）。
func TestBuildUnassignedListQuery(t *testing.T) {
	from := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 30, 23, 59, 59, 0, time.UTC)

	tests := []struct {
		name           string
		filter         Filter
		wantContains   []string
		wantNotContain []string
		wantArgsLen    int
	}{
		{
			name:           "Filter 未指定のとき WHERE 句なしで全件取得する",
			filter:         Filter{},
			wantContains:   []string{"FROM unassigned_notifications", "ORDER BY received_at DESC"},
			wantNotContain: []string{"WHERE"},
			wantArgsLen:    0,
		},
		{
			name:           "From 指定のとき received_at >= で絞り込む",
			filter:         Filter{From: &from},
			wantContains:   []string{"WHERE received_at >= $1", "ORDER BY received_at DESC"},
			wantNotContain: []string{"notification_type = "},
			wantArgsLen:    1,
		},
		{
			name:           "To 指定のとき received_at <= で絞り込む",
			filter:         Filter{To: &to},
			wantContains:   []string{"WHERE received_at <= $1"},
			wantNotContain: nil,
			wantArgsLen:    1,
		},
		{
			name:           "Type 指定のとき notification_type = で絞り込む",
			filter:         Filter{Type: "ENROLLMENT"},
			wantContains:   []string{"WHERE notification_type = $1"},
			wantNotContain: []string{"received_at >="},
			wantArgsLen:    1,
		},
		{
			name:           "from/to/type 全指定のとき AND 連結で 3 条件を構築する",
			filter:         Filter{From: &from, To: &to, Type: "COMMAND"},
			wantContains:   []string{"received_at >= $1", "received_at <= $2", "notification_type = $3", " AND "},
			wantNotContain: nil,
			wantArgsLen:    3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange / Act
			sql, args := buildUnassignedListQuery(tt.filter)

			// Assert
			for _, want := range tt.wantContains {
				if !strings.Contains(sql, want) {
					t.Errorf("sql は %q を含むべき: got %q", want, sql)
				}
			}
			for _, notWant := range tt.wantNotContain {
				if strings.Contains(sql, notWant) {
					t.Errorf("sql は %q を含むべきでない: got %q", notWant, sql)
				}
			}
			if len(args) != tt.wantArgsLen {
				t.Errorf("args 件数 mismatch: want %d, got %d (%v)", tt.wantArgsLen, len(args), args)
			}
		})
	}
}

// TestPayloadOrEmptyJSON は payload が空のとき空 jsonb `{}` へ、非空はそのままへ写像することを
// 検証する（Req 3.5 = jsonb NOT NULL 列に NULL を bind しない / 境界値: nil / 空 / 非空）。
func TestPayloadOrEmptyJSON(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want string
	}{
		{name: "nil payload は空 jsonb に写像する", in: nil, want: "{}"},
		{name: "長さ 0 payload は空 jsonb に写像する", in: []byte{}, want: "{}"},
		{name: "非空 payload はそのまま返す", in: []byte(`{"name":"enterprises/LC01/devices/d1"}`), want: `{"name":"enterprises/LC01/devices/d1"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange / Act
			got := payloadOrEmptyJSON(tt.in)

			// Assert
			if string(got) != tt.want {
				t.Errorf("payloadOrEmptyJSON(%q) = %q, want %q", string(tt.in), string(got), tt.want)
			}
		})
	}
}

// TestWrapUnassignedPersistErr_MapsToTransientUnavailable は退避 / 参照の永続化失敗が
// *errors.Error{Code: CodeUnavailable, IsTransient: true} へ写像され、cause が保持されることを
// 検証する（Req 1.4 経路 / NFR 2.2 = 退避失敗で取りこぼさず再処理保持 → worker nack）。
func TestWrapUnassignedPersistErr_MapsToTransientUnavailable(t *testing.T) {
	// Arrange
	cause := stderrors.New("connection refused")

	// Act
	err := wrapUnassignedPersistErr(cause)

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
