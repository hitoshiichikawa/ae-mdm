package notification

import (
	"context"
	"testing"

	"github.com/google/uuid"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/pubsub"
)

// ----- mock 依存（段階分岐 × ack/nack 種別を表駆動するための注入点） -----

// fakeVerifier は Verifier の mock。env / err を固定で返す。
type fakeVerifier struct {
	env Envelope
	err error
}

func (f *fakeVerifier) Parse(_ *pubsub.Message) (Envelope, error) { return f.env, f.err }

// fakeDedupe は Dedupe の mock。IsProcessed / MarkProcessed の戻り値と呼び出し回数を記録する。
//
// markErr で MarkProcessed（handler 成功後の dedupe 記録）の DB 失敗を、isProcessedErr で既処理
// 判定の DB 失敗を注入できる。
type fakeDedupe struct {
	processed      bool
	isProcessedErr error
	markErr        error
	markHit        int
}

func (f *fakeDedupe) IsProcessed(_ context.Context, _ string) (bool, error) {
	return f.processed, f.isProcessedErr
}

func (f *fakeDedupe) MarkProcessed(_ context.Context, _ string, _ NotificationType) error {
	f.markHit++
	return f.markErr
}

// fakeUnassigned は UnassignedQueue の mock。Enqueue の戻り値と呼び出し回数を記録する。
type fakeUnassigned struct {
	enqueueErr error
	enqueueHit int
}

func (f *fakeUnassigned) Enqueue(_ context.Context, _ Envelope) error {
	f.enqueueHit++
	return f.enqueueErr
}

func (f *fakeUnassigned) List(_ context.Context, _ Filter) ([]UnassignedNotification, error) {
	return nil, nil
}

// fakeResolver は TenantResolver の mock。逆引きの戻り値を固定で返す。
type fakeResolver struct {
	id    uuid.UUID
	found bool
	err   error
}

func (f *fakeResolver) TenantIDByEnterpriseName(_ context.Context, _ string) (uuid.UUID, bool, error) {
	return f.id, f.found, f.err
}

// fakeHandler は NotificationHandler の mock。呼び出し回数と確立された tenant context を記録する。
type fakeHandler struct {
	err           error
	hit           int
	gotTenantID   uuid.UUID
	tenantCtxSeen bool
}

func (f *fakeHandler) Handle(ctx context.Context, _ Envelope) error {
	f.hit++
	if tc, ctxErr := db.FromContext(ctx); ctxErr == nil {
		f.tenantCtxSeen = true
		f.gotTenantID = tc.TenantID
	}
	return f.err
}

// transientErr は IsTransient=true（nack 保持）の domain error を作る helper。
func transientErr() error {
	return &pkgerrors.Error{Code: pkgerrors.CodeUnavailable, Message: "transient", IsTransient: true}
}

// permanentErr は IsTransient=false（ack 完了扱い）の domain error を作る helper。
func permanentErr() error {
	return &pkgerrors.Error{Code: pkgerrors.CodeBusinessRule, Message: "permanent", IsTransient: false}
}

// validEnvelope は解決可能な enterprise_name を持つ正常 Envelope を作る helper。
func validEnvelope(t NotificationType) Envelope {
	return Envelope{
		MessageID:        "msg-1",
		NotificationType: t,
		EnterpriseName:   "enterprises/LC01",
		Payload:          []byte(`{"name":"enterprises/LC01/devices/d1"}`),
	}
}

// TestDispatcherHandle は段階分岐 × ack/nack 種別を mock 依存で表駆動検証する
// （Req 1.1 1.2 1.4 / 2.1〜2.6 / 3.1〜3.3 / 5.1〜5.3）。ack/nack は errors.ShouldAck を通して
// 検証する（worker 最外層の写像と一致させる）。
func TestDispatcherHandle(t *testing.T) {
	resolvedTenantID := uuid.New()

	tests := []struct {
		name string
		// 依存の状態
		verifier   *fakeVerifier
		dedupe     *fakeDedupe
		unassigned *fakeUnassigned
		resolver   *fakeResolver
		handler    *fakeHandler // nil なら handlers 未登録（Enrollment キー）
		// 期待
		wantAck        bool // ShouldAck の期待値（true=ack 完了扱い / false=nack 保持）
		wantHandlerHit int
		wantMarkHit    int // dedupe.MarkProcessed 呼び出し回数（handler 成功後にのみ記録 = record-after-success）
		wantEnqueueHit int
	}{
		{
			name:           "検証失敗（恒常的）のとき記録せず ack 完了扱いにする（Req 2.5）",
			verifier:       &fakeVerifier{err: permanentErr()},
			dedupe:         &fakeDedupe{},
			unassigned:     &fakeUnassigned{},
			resolver:       &fakeResolver{},
			handler:        &fakeHandler{},
			wantAck:        true,
			wantHandlerHit: 0,
			wantMarkHit:    0,
			wantEnqueueHit: 0,
		},
		{
			name:           "既処理 MessageID のとき記録せず即 ack する（Req 1.2 fast-path）",
			verifier:       &fakeVerifier{env: validEnvelope(Enrollment)},
			dedupe:         &fakeDedupe{processed: true},
			unassigned:     &fakeUnassigned{},
			resolver:       &fakeResolver{id: resolvedTenantID, found: true},
			handler:        &fakeHandler{},
			wantAck:        true,
			wantHandlerHit: 0,
			wantMarkHit:    0,
			wantEnqueueHit: 0,
		},
		{
			name:           "dedupe 判定の DB 失敗のとき記録せず nack 保持する（Req 1.4）",
			verifier:       &fakeVerifier{env: validEnvelope(Enrollment)},
			dedupe:         &fakeDedupe{isProcessedErr: transientErr()},
			unassigned:     &fakeUnassigned{},
			resolver:       &fakeResolver{id: resolvedTenantID, found: true},
			handler:        &fakeHandler{},
			wantAck:        false,
			wantHandlerHit: 0,
			wantMarkHit:    0,
			wantEnqueueHit: 0,
		},
		{
			name:           "tenant 未解決（found=false）のとき退避して ack する（Req 3.2 3.3）",
			verifier:       &fakeVerifier{env: validEnvelope(Enrollment)},
			dedupe:         &fakeDedupe{},
			unassigned:     &fakeUnassigned{},
			resolver:       &fakeResolver{found: false},
			handler:        &fakeHandler{},
			wantAck:        true,
			wantHandlerHit: 0,
			wantMarkHit:    0, // 退避経路の dedupe 記録は Enqueue 内部（同一 tx）。dispatcher は MarkProcessed を呼ばない
			wantEnqueueHit: 1,
		},
		{
			name: "enterprise_name 空のとき逆引きせず退避して ack する（Req 3.4）",
			verifier: &fakeVerifier{env: Envelope{
				MessageID:        "msg-1",
				NotificationType: Enrollment,
				EnterpriseName:   "",
			}},
			dedupe:         &fakeDedupe{},
			unassigned:     &fakeUnassigned{},
			resolver:       &fakeResolver{err: transientErr()}, // 呼ばれない（呼ばれたら err で nack に倒れ wantAck=true と矛盾）
			handler:        &fakeHandler{},
			wantAck:        true,
			wantHandlerHit: 0,
			wantMarkHit:    0,
			wantEnqueueHit: 1,
		},
		{
			name:           "未登録種別かつ tenant 未解決のとき退避せず ack 完了扱いにする（Req 2.4 が退避 Req 3.2 に優先）",
			verifier:       &fakeVerifier{env: validEnvelope(Enrollment)},
			dedupe:         &fakeDedupe{},
			unassigned:     &fakeUnassigned{},
			resolver:       &fakeResolver{found: false},
			handler:        nil, // handlers map に Enrollment を登録しない（未登録種別 = 例: AMAPI test 通知）
			wantAck:        true,
			wantHandlerHit: 0,
			wantMarkHit:    0,
			wantEnqueueHit: 0, // 未登録種別は退避せず ack（退避キューを汚さない / Req 2.4 が Req 3.2 に優先）
		},
		{
			name:           "tenant 逆引きの DB 失敗（transient）のとき記録せず nack 保持する（Req 1.4）",
			verifier:       &fakeVerifier{env: validEnvelope(Enrollment)},
			dedupe:         &fakeDedupe{},
			unassigned:     &fakeUnassigned{},
			resolver:       &fakeResolver{err: transientErr()},
			handler:        &fakeHandler{},
			wantAck:        false,
			wantHandlerHit: 0,
			wantMarkHit:    0,
			wantEnqueueHit: 0,
		},
		{
			name:           "退避（Enqueue）失敗（transient）のとき nack 保持する（Req 1.4 / 5.1 / NFR 2.2）",
			verifier:       &fakeVerifier{env: validEnvelope(Enrollment)},
			dedupe:         &fakeDedupe{},
			unassigned:     &fakeUnassigned{enqueueErr: transientErr()},
			resolver:       &fakeResolver{found: false},
			handler:        &fakeHandler{},
			wantAck:        false,
			wantHandlerHit: 0,
			wantMarkHit:    0, // 退避経路の dedupe 記録は Enqueue 内部（同一 tx）。失敗時も同 tx で rollback される
			wantEnqueueHit: 1,
		},
		{
			name:           "未登録種別のとき記録せず取りこぼさず ack 完了扱いにする（Req 2.4）",
			verifier:       &fakeVerifier{env: validEnvelope(Enrollment)},
			dedupe:         &fakeDedupe{},
			unassigned:     &fakeUnassigned{},
			resolver:       &fakeResolver{id: resolvedTenantID, found: true},
			handler:        nil, // handlers map に Enrollment を登録しない
			wantAck:        true,
			wantHandlerHit: 0,
			wantMarkHit:    0, // handler 不在のため dispatch せず dedupe も記録しない
			wantEnqueueHit: 0,
		},
		{
			name:           "transient handler 失敗のとき記録せず nack 保持する（再配信で再処理 / Req 5.1 5.3）",
			verifier:       &fakeVerifier{env: validEnvelope(Enrollment)},
			dedupe:         &fakeDedupe{},
			unassigned:     &fakeUnassigned{},
			resolver:       &fakeResolver{id: resolvedTenantID, found: true},
			handler:        &fakeHandler{err: transientErr()},
			wantAck:        false,
			wantHandlerHit: 1,
			wantMarkHit:    0, // handler 失敗時は dedupe を記録しない（記録が無いので再配信で再処理される）
			wantEnqueueHit: 0,
		},
		{
			name:           "恒常的 handler 失敗のとき記録せず ack 完了扱いにする（成功した処理のみ dedupe される / 5.1 の対偶）",
			verifier:       &fakeVerifier{env: validEnvelope(Enrollment)},
			dedupe:         &fakeDedupe{},
			unassigned:     &fakeUnassigned{},
			resolver:       &fakeResolver{id: resolvedTenantID, found: true},
			handler:        &fakeHandler{err: permanentErr()},
			wantAck:        true, // non-transient → ShouldAck=ack（恒常的失敗は再配信しても結果が変わらない）
			wantHandlerHit: 1,
			wantMarkHit:    0, // 恒常的失敗も記録しない（dedupe には成功した処理のみが残る）
			wantEnqueueHit: 0,
		},
		{
			name:           "handler 成功後の dedupe 記録失敗（transient）のとき nack 保持する（Req 1.4）",
			verifier:       &fakeVerifier{env: validEnvelope(Enrollment)},
			dedupe:         &fakeDedupe{markErr: transientErr()},
			unassigned:     &fakeUnassigned{},
			resolver:       &fakeResolver{id: resolvedTenantID, found: true},
			handler:        &fakeHandler{},
			wantAck:        false, // 記録失敗は transient nack（再配信で handler 再実行 → 冪等吸収）
			wantHandlerHit: 1,
			wantMarkHit:    1, // handler 成功後に記録を試みるが失敗
			wantEnqueueHit: 0,
		},
		{
			name:           "成功時に handler を 1 回呼び handler 成功後に dedupe 記録して ack する（Req 1.1 5.2）",
			verifier:       &fakeVerifier{env: validEnvelope(Enrollment)},
			dedupe:         &fakeDedupe{},
			unassigned:     &fakeUnassigned{},
			resolver:       &fakeResolver{id: resolvedTenantID, found: true},
			handler:        &fakeHandler{},
			wantAck:        true,
			wantHandlerHit: 1,
			wantMarkHit:    1, // 成功時のみ dedupe 記録（record-after-success）
			wantEnqueueHit: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			handlers := map[NotificationType]NotificationHandler{}
			if tt.handler != nil {
				handlers[Enrollment] = tt.handler
			}
			d := NewDispatcher(tt.verifier, tt.dedupe, tt.unassigned, tt.resolver, handlers, nil)

			// Act
			err := d.Handle(context.Background(), &pubsub.Message{ID: "msg-1"})

			// Assert: ack/nack 写像を ShouldAck を通して検証する。
			if got := pkgerrors.ShouldAck(err, nil); got != tt.wantAck {
				t.Errorf("ShouldAck mismatch: want %v, got %v (err=%v)", tt.wantAck, got, err)
			}
			if tt.handler != nil && tt.handler.hit != tt.wantHandlerHit {
				t.Errorf("handler 呼び出し回数 mismatch: want %d, got %d", tt.wantHandlerHit, tt.handler.hit)
			}
			if tt.dedupe.markHit != tt.wantMarkHit {
				t.Errorf("MarkProcessed 呼び出し回数 mismatch: want %d, got %d", tt.wantMarkHit, tt.dedupe.markHit)
			}
			if tt.unassigned.enqueueHit != tt.wantEnqueueHit {
				t.Errorf("Enqueue 呼び出し回数 mismatch: want %d, got %d", tt.wantEnqueueHit, tt.unassigned.enqueueHit)
			}
		})
	}
}

// TestDispatcherHandle_RoutesByNotificationType は ENROLLMENT / STATUS_REPORT / COMMAND が
// それぞれ対応する handler へ振り分けられることを検証する（Req 2.1 2.2 2.3）。
func TestDispatcherHandle_RoutesByNotificationType(t *testing.T) {
	resolvedTenantID := uuid.New()

	tests := []struct {
		name      string
		notifType NotificationType
	}{
		{name: "ENROLLMENT を ENROLLMENT handler へ振り分ける", notifType: Enrollment},
		{name: "STATUS_REPORT を STATUS_REPORT handler へ振り分ける", notifType: StatusReport},
		{name: "COMMAND を COMMAND handler へ振り分ける", notifType: Command},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange: 全種別の handler を登録し、対象種別のみが呼ばれることを確認する。
			enroll := &fakeHandler{}
			status := &fakeHandler{}
			command := &fakeHandler{}
			handlers := map[NotificationType]NotificationHandler{
				Enrollment:   enroll,
				StatusReport: status,
				Command:      command,
			}
			d := NewDispatcher(
				&fakeVerifier{env: validEnvelope(tt.notifType)},
				&fakeDedupe{},
				&fakeUnassigned{},
				&fakeResolver{id: resolvedTenantID, found: true},
				handlers,
				nil,
			)

			// Act
			err := d.Handle(context.Background(), &pubsub.Message{ID: "msg-1"})

			// Assert
			if err != nil {
				t.Fatalf("正常 dispatch で error を返すべきでない: %v", err)
			}
			target := handlers[tt.notifType].(*fakeHandler)
			if target.hit != 1 {
				t.Errorf("対象種別の handler は 1 回呼ばれるべき: got %d", target.hit)
			}
			// 他種別の handler は呼ばれない。
			for typ, h := range handlers {
				if typ == tt.notifType {
					continue
				}
				if h.(*fakeHandler).hit != 0 {
					t.Errorf("非対象種別 %q の handler は呼ばれるべきでない: got %d", typ, h.(*fakeHandler).hit)
				}
			}
		})
	}
}

// TestDispatcherHandle_EstablishesTenantContextBeforeHandler は解決成功時に handler 呼び出し前へ
// 解決した tenant_id の TenantContext が確立されることを検証する（Req 3.1 / NFR 2.1）。
func TestDispatcherHandle_EstablishesTenantContextBeforeHandler(t *testing.T) {
	// Arrange
	resolvedTenantID := uuid.New()
	handler := &fakeHandler{}
	d := NewDispatcher(
		&fakeVerifier{env: validEnvelope(Enrollment)},
		&fakeDedupe{},
		&fakeUnassigned{},
		&fakeResolver{id: resolvedTenantID, found: true},
		map[NotificationType]NotificationHandler{Enrollment: handler},
		nil,
	)

	// Act
	if err := d.Handle(context.Background(), &pubsub.Message{ID: "msg-1"}); err != nil {
		t.Fatalf("正常 dispatch で error を返すべきでない: %v", err)
	}

	// Assert
	if !handler.tenantCtxSeen {
		t.Fatalf("handler 呼び出し時に TenantContext が確立されているべき（Req 3.1）")
	}
	if handler.gotTenantID != resolvedTenantID {
		t.Errorf("確立された tenant_id mismatch: want %v, got %v", resolvedTenantID, handler.gotTenantID)
	}
}

// TestDispatcherHandle_UnassignedDoesNotTouchTenantHandler は未割当退避経路で種別 handler が
// 一切呼ばれないこと（いずれのテナントリソースも更新しない / NFR 2.1 2.2）を検証する。
func TestDispatcherHandle_UnassignedDoesNotTouchTenantHandler(t *testing.T) {
	// Arrange
	handler := &fakeHandler{}
	unassigned := &fakeUnassigned{}
	d := NewDispatcher(
		&fakeVerifier{env: validEnvelope(Enrollment)},
		&fakeDedupe{},
		unassigned,
		&fakeResolver{found: false},
		map[NotificationType]NotificationHandler{Enrollment: handler},
		nil,
	)

	// Act
	if err := d.Handle(context.Background(), &pubsub.Message{ID: "msg-1"}); err != nil {
		t.Fatalf("退避経路で error を返すべきでない: %v", err)
	}

	// Assert
	if handler.hit != 0 {
		t.Errorf("未割当退避では種別 handler を呼ぶべきでない（NFR 2.2）: got %d", handler.hit)
	}
	if unassigned.enqueueHit != 1 {
		t.Errorf("未割当退避では Enqueue を 1 回呼ぶべき: got %d", unassigned.enqueueHit)
	}
}

// TestDispatcher_ImplementsMessageHandler は Dispatcher が pubsub.MessageHandler を満たすことを
// コンパイル時に保証する（subscriber への注入互換 / design.md Dispatcher 契約）。
func TestDispatcher_ImplementsMessageHandler(t *testing.T) {
	var _ pubsub.MessageHandler = (*Dispatcher)(nil)
}
