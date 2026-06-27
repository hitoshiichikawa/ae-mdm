package audit

import (
	"context"
	stderrors "errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/hitoshiichikawa/ae-mdm/internal/config"
)

// ---- Fake Clock ----

// fakeClock は固定時刻を返す Clock 実装。保持期間下限算出を決定的にする。
type fakeClock struct {
	now time.Time
}

func (c fakeClock) Now() time.Time { return c.now }

// ---- Fake Repository ----

// fakeRepository は Insert で受信した Event / Select で受信した Filter・effectiveFrom を記録し、
// 任意の結果・エラーを返せる test double。
type fakeRepository struct {
	// Insert 記録
	insertCalls   int
	insertedEvent Event
	insertErr     error

	// Select 記録
	selectCalls         int
	selectedFilter      Filter
	selectedEffectiveAt time.Time
	selectOut           []Event
	selectErr           error
}

func (r *fakeRepository) Insert(_ context.Context, ev Event) error {
	r.insertCalls++
	r.insertedEvent = ev
	return r.insertErr
}

func (r *fakeRepository) Select(_ context.Context, f Filter, effectiveFrom time.Time) ([]Event, error) {
	r.selectCalls++
	r.selectedFilter = f
	r.selectedEffectiveAt = effectiveFrom
	return r.selectOut, r.selectErr
}

// fixedNow はテストで使う基準時刻（UTC 固定）。
var fixedNow = time.Date(2026, 6, 28, 12, 0, 0, 0, time.UTC)

// ---- Record tests ----

func TestService_Record(t *testing.T) {
	t.Run("ID/OccurredAt 未設定かつ結果成功のとき採番・補完して ResultSuccess を Repository へ渡す", func(t *testing.T) {
		// Arrange (Req 1.1 / 1.3)
		repo := &fakeRepository{}
		svc := NewService(config.Config{AuditLogRetentionDays: 180}, repo, fakeClock{now: fixedNow}, nil)
		ev := Event{
			TenantID:  uuid.New(),
			ActorID:   uuid.New(),
			EventType: "tenant_create",
			Result:    ResultSuccess,
		}

		// Act
		err := svc.Record(context.Background(), ev)

		// Assert
		if err != nil {
			t.Fatalf("Record returned unexpected error: %v", err)
		}
		if repo.insertCalls != 1 {
			t.Fatalf("Insert call count = %d, want 1", repo.insertCalls)
		}
		got := repo.insertedEvent
		if got.ID == uuid.Nil {
			t.Errorf("ID was not assigned (still uuid.Nil)")
		}
		if !got.OccurredAt.Equal(fixedNow) {
			t.Errorf("OccurredAt = %v, want %v (補完)", got.OccurredAt, fixedNow)
		}
		if got.Result != ResultSuccess {
			t.Errorf("Result = %q, want %q", got.Result, ResultSuccess)
		}
	})

	t.Run("結果失敗のとき ResultFailure をそのまま Repository へ渡す", func(t *testing.T) {
		// Arrange (Req 1.4)
		repo := &fakeRepository{}
		svc := NewService(config.Config{AuditLogRetentionDays: 180}, repo, fakeClock{now: fixedNow}, nil)
		ev := Event{ActorID: uuid.New(), EventType: "role_change", Result: ResultFailure}

		// Act
		err := svc.Record(context.Background(), ev)

		// Assert
		if err != nil {
			t.Fatalf("Record returned unexpected error: %v", err)
		}
		if repo.insertedEvent.Result != ResultFailure {
			t.Errorf("Result = %q, want %q", repo.insertedEvent.Result, ResultFailure)
		}
	})

	t.Run("ID/OccurredAt が設定済みのとき上書きせず保持する", func(t *testing.T) {
		// Arrange (Req 1.1 — 既存値の温存。境界: 採番・補完が発火しないケース)
		repo := &fakeRepository{}
		svc := NewService(config.Config{AuditLogRetentionDays: 180}, repo, fakeClock{now: fixedNow}, nil)
		presetID := uuid.New()
		presetTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		ev := Event{ID: presetID, ActorID: uuid.New(), EventType: "policy_change", Result: ResultSuccess, OccurredAt: presetTime}

		// Act
		if err := svc.Record(context.Background(), ev); err != nil {
			t.Fatalf("Record returned unexpected error: %v", err)
		}

		// Assert
		got := repo.insertedEvent
		if got.ID != presetID {
			t.Errorf("ID = %v, want preset %v (上書き禁止)", got.ID, presetID)
		}
		if !got.OccurredAt.Equal(presetTime) {
			t.Errorf("OccurredAt = %v, want preset %v (上書き禁止)", got.OccurredAt, presetTime)
		}
	})

	t.Run("TenantID == uuid.Nil（NULL テナント）がそのまま Repository へ渡る", func(t *testing.T) {
		// Arrange (Req 1.2 — NULL bind は Repository 責務。Service は素通し)
		repo := &fakeRepository{}
		svc := NewService(config.Config{AuditLogRetentionDays: 180}, repo, fakeClock{now: fixedNow}, nil)
		ev := Event{TenantID: uuid.Nil, ActorID: uuid.New(), EventType: "command_wipe", Result: ResultSuccess}

		// Act
		if err := svc.Record(context.Background(), ev); err != nil {
			t.Fatalf("Record returned unexpected error: %v", err)
		}

		// Assert
		if repo.insertedEvent.TenantID != uuid.Nil {
			t.Errorf("TenantID = %v, want uuid.Nil (NULL テナント素通し)", repo.insertedEvent.TenantID)
		}
	})

	t.Run("Repository が永続化エラーを返したとき成功扱いせずエラーを伝播する", func(t *testing.T) {
		// Arrange (Req 1.6 — fail-closed)
		wantErr := stderrors.New("insert failed")
		repo := &fakeRepository{insertErr: wantErr}
		svc := NewService(config.Config{AuditLogRetentionDays: 180}, repo, fakeClock{now: fixedNow}, nil)
		ev := Event{ActorID: uuid.New(), EventType: "token_issue", Result: ResultSuccess}

		// Act
		err := svc.Record(context.Background(), ev)

		// Assert
		if err == nil {
			t.Fatalf("Record returned nil error, want propagated error")
		}
		if !stderrors.Is(err, wantErr) {
			t.Errorf("error chain does not contain repo error: got %v", err)
		}
	})
}

// TestService_Record_PersistError_LogsPersistErrorWarn は永続化失敗時に
// failure_kind=persist_error の構造化 WARN が出ることを検証する（NFR 3.2）。
//
// 永続化失敗のエラー伝播そのもの（Req 1.6）は別テストでカバーするため、本テストは
// 構造化ログの failure_kind 観点のみに限定する（1 テスト = 1 検証対象）。
func TestService_Record_PersistError_LogsPersistErrorWarn(t *testing.T) {
	// Arrange (NFR 3.2)
	repo := &fakeRepository{insertErr: stderrors.New("insert failed")}
	log := &fakeLogger{}
	svc := NewService(config.Config{AuditLogRetentionDays: 180}, repo, fakeClock{now: fixedNow}, log)
	ev := Event{ActorID: uuid.New(), EventType: "token_issue", Result: ResultSuccess}

	// Act
	err := svc.Record(context.Background(), ev)

	// Assert
	if err == nil {
		t.Fatalf("Record returned nil error, want propagated error")
	}
	assertWarnFailureKind(t, log, FailureKindPersistError)
}

// TestService_Record_Success_NoWarn は永続化成功時に WARN を出さないことを検証する
// （NFR 3.2 の境界 = 失敗時のみ failure_kind ログを出す）。
func TestService_Record_Success_NoWarn(t *testing.T) {
	// Arrange
	repo := &fakeRepository{}
	log := &fakeLogger{}
	svc := NewService(config.Config{AuditLogRetentionDays: 180}, repo, fakeClock{now: fixedNow}, log)
	ev := Event{ActorID: uuid.New(), EventType: "policy_change", Result: ResultSuccess}

	// Act
	if err := svc.Record(context.Background(), ev); err != nil {
		t.Fatalf("Record returned unexpected error: %v", err)
	}

	// Assert
	if len(log.warnCalls) != 0 {
		t.Errorf("成功時に WARN が出力されている: %+v", log.warnCalls)
	}
}

// ---- List tests ----

func TestService_List_RetentionFloor(t *testing.T) {
	tests := []struct {
		name              string
		retentionDays     int
		from              *time.Time
		wantEffectiveFrom time.Time
	}{
		{
			// (d) from が retention 起点より前 → effectiveFrom が起点に丸められる (Req 5.3)
			name:              "from が保持起点より前のとき effectiveFrom は保持起点に丸められる",
			retentionDays:     180,
			from:              timePtr(fixedNow.AddDate(0, 0, -365)), // 365 日前 < 180 日前(起点)
			wantEffectiveFrom: fixedNow.AddDate(0, 0, -180),
		},
		{
			// (e) from が retention 起点より後 → effectiveFrom == from (Req 5.3 境界)
			name:              "from が保持起点より後のとき effectiveFrom は from に一致する",
			retentionDays:     180,
			from:              timePtr(fixedNow.AddDate(0, 0, -30)), // 30 日前 > 180 日前(起点)
			wantEffectiveFrom: fixedNow.AddDate(0, 0, -30),
		},
		{
			// from 未指定 → effectiveFrom == retentionFloor (Req 5.1 / 5.2)
			name:              "from 未指定のとき effectiveFrom は保持起点になる",
			retentionDays:     180,
			from:              nil,
			wantEffectiveFrom: fixedNow.AddDate(0, 0, -180),
		},
		{
			// (f) retention 既定 180 と変更値 365 で下限が切り替わる (Req 5.1 / 5.4 / NFR 1.1)
			name:              "retention が 365 のとき effectiveFrom は 365 日前の保持起点になる",
			retentionDays:     365,
			from:              nil,
			wantEffectiveFrom: fixedNow.AddDate(0, 0, -365),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			repo := &fakeRepository{selectOut: []Event{}}
			svc := NewService(config.Config{AuditLogRetentionDays: tc.retentionDays}, repo, fakeClock{now: fixedNow}, nil)

			// Act
			_, err := svc.List(context.Background(), Filter{From: tc.from})

			// Assert
			if err != nil {
				t.Fatalf("List returned unexpected error: %v", err)
			}
			if repo.selectCalls != 1 {
				t.Fatalf("Select call count = %d, want 1", repo.selectCalls)
			}
			if !repo.selectedEffectiveAt.Equal(tc.wantEffectiveFrom) {
				t.Errorf("effectiveFrom = %v, want %v", repo.selectedEffectiveAt, tc.wantEffectiveFrom)
			}
		})
	}
}

func TestService_List_DefaultRetentionDiffersFrom365(t *testing.T) {
	// Arrange — 同一 from・同一 Clock で retention=180 と retention=365 の effectiveFrom が
	// 切り替わることを直接対比する (Req 5.1 / 5.4 / NFR 1.1)
	from := timePtr(fixedNow.AddDate(0, 0, -400)) // 起点より十分前に置き、retentionFloor が支配的になるようにする

	repo180 := &fakeRepository{selectOut: []Event{}}
	svc180 := NewService(config.Config{AuditLogRetentionDays: 180}, repo180, fakeClock{now: fixedNow}, nil)
	repo365 := &fakeRepository{selectOut: []Event{}}
	svc365 := NewService(config.Config{AuditLogRetentionDays: 365}, repo365, fakeClock{now: fixedNow}, nil)

	// Act
	if _, err := svc180.List(context.Background(), Filter{From: from}); err != nil {
		t.Fatalf("svc180.List error: %v", err)
	}
	if _, err := svc365.List(context.Background(), Filter{From: from}); err != nil {
		t.Fatalf("svc365.List error: %v", err)
	}

	// Assert
	if !repo180.selectedEffectiveAt.Equal(fixedNow.AddDate(0, 0, -180)) {
		t.Errorf("retention=180 effectiveFrom = %v, want %v", repo180.selectedEffectiveAt, fixedNow.AddDate(0, 0, -180))
	}
	if !repo365.selectedEffectiveAt.Equal(fixedNow.AddDate(0, 0, -365)) {
		t.Errorf("retention=365 effectiveFrom = %v, want %v", repo365.selectedEffectiveAt, fixedNow.AddDate(0, 0, -365))
	}
	if repo180.selectedEffectiveAt.Equal(repo365.selectedEffectiveAt) {
		t.Errorf("retention 180 と 365 で effectiveFrom が切り替わっていない: %v", repo180.selectedEffectiveAt)
	}
}

func TestService_List_PassesFilterAndResult(t *testing.T) {
	// Arrange — Filter が Repository へそのまま伝播し、結果が呼び出し側へ返ることを確認 (Req 2.x / 3.x)
	repo := &fakeRepository{selectOut: []Event{{EventType: "policy_change"}}}
	svc := NewService(config.Config{AuditLogRetentionDays: 180}, repo, fakeClock{now: fixedNow}, nil)
	tenantID := uuid.New()
	filter := Filter{TenantID: &tenantID, EventType: "policy_change", ActorID: "actor-1", ResourceID: "res-1"}

	// Act
	got, err := svc.List(context.Background(), filter)

	// Assert
	if err != nil {
		t.Fatalf("List returned unexpected error: %v", err)
	}
	if repo.selectedFilter.EventType != filter.EventType ||
		repo.selectedFilter.ActorID != filter.ActorID ||
		repo.selectedFilter.ResourceID != filter.ResourceID ||
		repo.selectedFilter.TenantID == nil || *repo.selectedFilter.TenantID != tenantID {
		t.Errorf("Filter not forwarded as-is: got %+v", repo.selectedFilter)
	}
	if len(got) != 1 || got[0].EventType != "policy_change" {
		t.Errorf("List result = %+v, want repository result forwarded", got)
	}
}

func TestService_List_EmptyResult(t *testing.T) {
	// Arrange — (g) fake Repository が 0 行を返したら空 slice + nil (Req 2.8 / 3.4)
	repo := &fakeRepository{selectOut: []Event{}}
	svc := NewService(config.Config{AuditLogRetentionDays: 180}, repo, fakeClock{now: fixedNow}, nil)

	// Act
	got, err := svc.List(context.Background(), Filter{})

	// Assert
	if err != nil {
		t.Fatalf("List returned unexpected error: %v", err)
	}
	if got == nil {
		t.Errorf("List returned nil slice, want non-nil empty slice")
	}
	if len(got) != 0 {
		t.Errorf("List returned %d rows, want 0", len(got))
	}
}

func TestService_List_PropagatesSelectError(t *testing.T) {
	// Arrange — SELECT エラー時はそのまま伝播する（閲覧経路の fail パス）
	wantErr := stderrors.New("select failed")
	repo := &fakeRepository{selectErr: wantErr}
	svc := NewService(config.Config{AuditLogRetentionDays: 180}, repo, fakeClock{now: fixedNow}, nil)

	// Act
	_, err := svc.List(context.Background(), Filter{})

	// Assert
	if !stderrors.Is(err, wantErr) {
		t.Errorf("error chain does not contain repo error: got %v", err)
	}
}

func timePtr(t time.Time) *time.Time { return &t }
