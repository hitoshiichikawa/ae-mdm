package audit

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fixedEffectiveFrom は buildSelectQuery のテストで使う決定的な保持下限時刻。
var fixedEffectiveFrom = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// TestBuildSelectQuery_EmptyFilter は Filter 全空のとき、保持下限句のみが付与され余分な
// WHERE 句が無く ORDER BY occurred_at DESC で終わることを検証する（Req 2.1 / 5.2 / 5.3）。
func TestBuildSelectQuery_EmptyFilter(t *testing.T) {
	// Arrange
	f := Filter{}

	// Act
	sql, args := buildSelectQuery(f, fixedEffectiveFrom)

	// Assert
	wantSQL := "SELECT id, tenant_id, actor_id, event_type, resource_id, detail, result, occurred_at " +
		"FROM audit_logs WHERE occurred_at >= $1 ORDER BY occurred_at DESC"
	if sql != wantSQL {
		t.Fatalf("sql mismatch:\n want %q\n got  %q", wantSQL, sql)
	}
	if len(args) != 1 {
		t.Fatalf("args 件数が想定外: want 1, got %d (%v)", len(args), args)
	}
	if args[0] != fixedEffectiveFrom {
		t.Fatalf("args[0] は effectiveFrom であるべき: want %v, got %v", fixedEffectiveFrom, args[0])
	}
}

// TestBuildSelectQuery_AlwaysAppliesRetentionFloor は保持下限句 occurred_at >= $1 が
// 常に先頭で付与されることを検証する（Req 5.2 = effectiveFrom を必ず付与）。
func TestBuildSelectQuery_AlwaysAppliesRetentionFloor(t *testing.T) {
	// Arrange
	eventType := EventType("role_change")
	f := Filter{EventType: eventType}

	// Act
	sql, args := buildSelectQuery(f, fixedEffectiveFrom)

	// Assert
	if !strings.Contains(sql, "occurred_at >= $1") {
		t.Fatalf("保持下限句 occurred_at >= $1 が常に付与されるべき: got %q", sql)
	}
	if args[0] != fixedEffectiveFrom {
		t.Fatalf("$1 は effectiveFrom であるべき: want %v, got %v", fixedEffectiveFrom, args[0])
	}
}

// TestBuildSelectQuery_AppendsOnlySpecifiedFilters は各 Filter 句が指定時のみ追加され、
// args の順序・値が WHERE 句に追加した順（$1=effectiveFrom 始まり）と一致することを検証する
// （Req 2.2 / 2.3 / 2.4 / 2.7 / 3.2 / 3.3）。
func TestBuildSelectQuery_AppendsOnlySpecifiedFilters(t *testing.T) {
	to := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	tenantID := uuid.MustParse("11111111-1111-1111-1111-111111111111")

	tests := []struct {
		name           string
		filter         Filter
		wantConditions []string
		wantArgs       []any
	}{
		{
			name:           "EventType 指定のとき event_type 句のみ追加される",
			filter:         Filter{EventType: EventType("policy_change")},
			wantConditions: []string{"occurred_at >= $1", "event_type = $2"},
			wantArgs:       []any{fixedEffectiveFrom, "policy_change"},
		},
		{
			name:           "ActorID 指定のとき actor_id 句のみ追加される",
			filter:         Filter{ActorID: "actor-123"},
			wantConditions: []string{"occurred_at >= $1", "actor_id = $2"},
			wantArgs:       []any{fixedEffectiveFrom, "actor-123"},
		},
		{
			name:           "ResourceID 指定のとき resource_id 句のみ追加される",
			filter:         Filter{ResourceID: "res-9"},
			wantConditions: []string{"occurred_at >= $1", "resource_id = $2"},
			wantArgs:       []any{fixedEffectiveFrom, "res-9"},
		},
		{
			name:           "To 指定のとき occurred_at <= 句が追加される",
			filter:         Filter{To: &to},
			wantConditions: []string{"occurred_at >= $1", "occurred_at <= $2"},
			wantArgs:       []any{fixedEffectiveFrom, to},
		},
		{
			name: "全 Filter 句指定のとき WHERE 句追加順どおりに $n と args が並ぶ",
			filter: Filter{
				To:         &to,
				EventType:  EventType("command_wipe"),
				ActorID:    "actor-7",
				ResourceID: "res-1",
				TenantID:   &tenantID,
			},
			wantConditions: []string{
				"occurred_at >= $1",
				"occurred_at <= $2",
				"event_type = $3",
				"actor_id = $4",
				"resource_id = $5",
				"tenant_id = $6",
			},
			wantArgs: []any{fixedEffectiveFrom, to, "command_wipe", "actor-7", "res-1", tenantID},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange / Act
			sql, args := buildSelectQuery(tt.filter, fixedEffectiveFrom)

			// Assert: 各条件句が SQL に含まれる
			for _, cond := range tt.wantConditions {
				if !strings.Contains(sql, cond) {
					t.Errorf("条件句 %q が SQL に含まれるべき: got %q", cond, sql)
				}
			}
			// Assert: args の順序と値が一致する
			if len(args) != len(tt.wantArgs) {
				t.Fatalf("args 件数が想定外: want %d, got %d (%v)", len(tt.wantArgs), len(args), args)
			}
			for i := range tt.wantArgs {
				if args[i] != tt.wantArgs[i] {
					t.Errorf("args[%d] mismatch: want %v, got %v", i, tt.wantArgs[i], args[i])
				}
			}
		})
	}
}

// TestBuildSelectQuery_OmitsUnspecifiedFilters は未指定の Filter 句が SQL に含まれない
// ことを検証する（Req 2.6 = 指定された条件のみ絞り込む / 自テナント条件を書かない）。
func TestBuildSelectQuery_OmitsUnspecifiedFilters(t *testing.T) {
	// Arrange: From のみ指定（buildSelectQuery は From を直接使わず effectiveFrom 経由のため
	// WHERE 句に from 由来の追加句が出ないことを確認する）。
	f := Filter{}

	// Act
	sql, _ := buildSelectQuery(f, fixedEffectiveFrom)

	// Assert
	for _, cond := range []string{"event_type =", "actor_id =", "resource_id =", "tenant_id =", "occurred_at <="} {
		if strings.Contains(sql, cond) {
			t.Errorf("未指定 Filter の条件句 %q は SQL に含まれないべき: got %q", cond, sql)
		}
	}
}

// TestBuildSelectQuery_TenantIDPresenceTogglesClause は TenantID != nil で tenant_id 句が
// 入り、TenantID == nil で入らないことを検証する（admin 横断の明示絞り込みのみ tenant_id 句を
// 出す / 自テナント条件は RLS に委ねる / Req 3.2 / 3.5 / NFR 2.1）。
func TestBuildSelectQuery_TenantIDPresenceTogglesClause(t *testing.T) {
	tenantID := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	tests := []struct {
		name             string
		filter           Filter
		wantTenantClause bool
		wantArgsLen      int
	}{
		{
			name:             "TenantID 指定のとき tenant_id 句が入る",
			filter:           Filter{TenantID: &tenantID},
			wantTenantClause: true,
			wantArgsLen:      2,
		},
		{
			name:             "TenantID nil のとき tenant_id 句が入らない（RLS に委ねる）",
			filter:           Filter{TenantID: nil},
			wantTenantClause: false,
			wantArgsLen:      1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange / Act
			sql, args := buildSelectQuery(tt.filter, fixedEffectiveFrom)

			// Assert: tenant_id 句の有無
			gotTenantClause := strings.Contains(sql, "tenant_id =")
			if gotTenantClause != tt.wantTenantClause {
				t.Errorf("tenant_id 句の有無 mismatch: want %v, got %v (sql=%q)", tt.wantTenantClause, gotTenantClause, sql)
			}
			// Assert: args 件数（effectiveFrom + tenant_id 有無）
			if len(args) != tt.wantArgsLen {
				t.Fatalf("args 件数 mismatch: want %d, got %d (%v)", tt.wantArgsLen, len(args), args)
			}
			if tt.wantTenantClause && args[len(args)-1] != tenantID {
				t.Errorf("tenant_id 句の bind 値は TenantID であるべき: want %v, got %v", tenantID, args[len(args)-1])
			}
		})
	}
}

// TestBuildSelectQuery_OrdersByOccurredAtDesc は組み立てた SQL が occurred_at 降順で
// 終わることを検証する（Req 2.1 / 3.1）。
func TestBuildSelectQuery_OrdersByOccurredAtDesc(t *testing.T) {
	// Arrange
	f := Filter{}

	// Act
	sql, _ := buildSelectQuery(f, fixedEffectiveFrom)

	// Assert
	if !strings.HasSuffix(sql, "ORDER BY occurred_at DESC") {
		t.Fatalf("SQL は ORDER BY occurred_at DESC で終わるべき: got %q", sql)
	}
}

// TestMarshalDetail_NilReturnsEmptyJSONObject は Detail が nil のとき空 jsonb `{}` を bind
// 用に返すことを検証する（既存スキーマ detail NOT NULL DEFAULT '{}' と整合 / Req 1.1）。
func TestMarshalDetail_NilReturnsEmptyJSONObject(t *testing.T) {
	// Arrange / Act
	got, err := marshalDetail(nil)

	// Assert
	if err != nil {
		t.Fatalf("nil detail で error を返すべきでない: %v", err)
	}
	if string(got) != "{}" {
		t.Fatalf("nil detail は空 jsonb {} を返すべき: got %q", string(got))
	}
}

// TestNullableTenantID_NilUUIDMapsToNullBind は uuid.Nil が NULL bind（nil ポインタ）に、
// 非 nil が非 NULL bind に写像されることを検証する（Req 1.2 = NULL テナント bind）。
func TestNullableTenantID_NilUUIDMapsToNullBind(t *testing.T) {
	tenantID := uuid.MustParse("33333333-3333-3333-3333-333333333333")

	tests := []struct {
		name     string
		in       uuid.UUID
		wantNil  bool
		wantUUID uuid.UUID
	}{
		{name: "uuid.Nil は NULL bind（nil ポインタ）になる", in: uuid.Nil, wantNil: true},
		{name: "非 nil uuid は非 NULL bind になる", in: tenantID, wantNil: false, wantUUID: tenantID},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange / Act
			got := nullableTenantID(tt.in)

			// Assert
			if tt.wantNil {
				if got != nil {
					t.Fatalf("uuid.Nil は nil ポインタ（NULL bind）になるべき: got %v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("非 nil uuid は非 NULL bind になるべき: got nil")
			}
			if *got != tt.wantUUID {
				t.Fatalf("bind 値 mismatch: want %v, got %v", tt.wantUUID, *got)
			}
		})
	}
}
