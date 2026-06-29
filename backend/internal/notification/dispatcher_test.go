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

// fakeDedupe は Dedupe の mock。IsProcessed / Claim / Release の戻り値と呼び出し回数を記録する。
//
// claimConflict=true で Claim を claimed=false（既に他者が claim 済み = 並行重複）に倒す。
// claimErr / releaseErr で各操作の DB 失敗を注入できる。
type fakeDedupe struct {
	processed      bool
	isProcessedErr error
	claimConflict  bool
	claimErr       error
	releaseErr     error
	claimHit       int
	releaseHit     int
}

func (f *fakeDedupe) IsProcessed(_ context.Context, _ string) (bool, error) {
	return f.processed, f.isProcessedErr
}

func (f *fakeDedupe) Claim(_ context.Context, _ string, _ NotificationType) (bool, error) {
	f.claimHit++
	if f.claimErr != nil {
		return false, f.claimErr
	}
	return !f.claimConflict, nil
}

func (f *fakeDedupe) Release(_ context.Context, _ string) error {
	f.releaseHit++
	return f.releaseErr
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
		wantClaimHit   int // dedupe.Claim 呼び出し回数（dispatch / 退避の前に取る）
		wantReleaseHit int // dedupe.Release 呼び出し回数（後続処理失敗時の claim 取り消し）
		wantEnqueueHit int
	}{
		{
			name:           "検証失敗（恒常的）のとき claim せず ack 完了扱いにする（Req 2.5）",
			verifier:       &fakeVerifier{err: permanentErr()},
			dedupe:         &fakeDedupe{},
			unassigned:     &fakeUnassigned{},
			resolver:       &fakeResolver{},
			handler:        &fakeHandler{},
			wantAck:        true,
			wantHandlerHit: 0,
			wantClaimHit:   0,
			wantReleaseHit: 0,
			wantEnqueueHit: 0,
		},
		{
			name:           "既処理 MessageID のとき claim せず即 ack する（Req 1.2 fast-path）",
			verifier:       &fakeVerifier{env: validEnvelope(Enrollment)},
			dedupe:         &fakeDedupe{processed: true},
			unassigned:     &fakeUnassigned{},
			resolver:       &fakeResolver{id: resolvedTenantID, found: true},
			handler:        &fakeHandler{},
			wantAck:        true,
			wantHandlerHit: 0,
			wantClaimHit:   0,
			wantReleaseHit: 0,
			wantEnqueueHit: 0,
		},
		{
			name:           "dedupe 判定の DB 失敗のとき claim せず nack 保持する（Req 1.4）",
			verifier:       &fakeVerifier{env: validEnvelope(Enrollment)},
			dedupe:         &fakeDedupe{isProcessedErr: transientErr()},
			unassigned:     &fakeUnassigned{},
			resolver:       &fakeResolver{id: resolvedTenantID, found: true},
			handler:        &fakeHandler{},
			wantAck:        false,
			wantHandlerHit: 0,
			wantClaimHit:   0,
			wantReleaseHit: 0,
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
			wantClaimHit:   0, // 退避経路の claim は Enqueue 内部（同一 tx）で取るため dispatcher は Claim を呼ばない
			wantReleaseHit: 0,
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
			wantClaimHit:   0,
			wantReleaseHit: 0,
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
			wantClaimHit:   0,
			wantReleaseHit: 0,
			wantEnqueueHit: 0, // 未登録種別は退避せず ack（退避キューを汚さない / Req 2.4 が Req 3.2 に優先）
		},
		{
			name:           "tenant 逆引きの DB 失敗（transient）のとき claim せず nack 保持する（Req 1.4）",
			verifier:       &fakeVerifier{env: validEnvelope(Enrollment)},
			dedupe:         &fakeDedupe{},
			unassigned:     &fakeUnassigned{},
			resolver:       &fakeResolver{err: transientErr()},
			handler:        &fakeHandler{},
			wantAck:        false,
			wantHandlerHit: 0,
			wantClaimHit:   0, // claim は resolve 後のため未到達
			wantReleaseHit: 0,
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
			wantClaimHit:   0, // claim は Enqueue 内部（同一 tx）。失敗時も同 tx で rollback され dispatcher は release しない
			wantReleaseHit: 0,
			wantEnqueueHit: 1,
		},
		{
			name:           "未登録種別のとき claim せず取りこぼさず ack 完了扱いにする（Req 2.4）",
			verifier:       &fakeVerifier{env: validEnvelope(Enrollment)},
			dedupe:         &fakeDedupe{},
			unassigned:     &fakeUnassigned{},
			resolver:       &fakeResolver{id: resolvedTenantID, found: true},
			handler:        nil, // handlers map に Enrollment を登録しない
			wantAck:        true,
			wantHandlerHit: 0,
			wantClaimHit:   0, // handler 不在のため claim しない（dedupe 記録を残さない）
			wantReleaseHit: 0,
			wantEnqueueHit: 0,
		},
		{
			name:           "並行重複で claim 敗北（dispatch 経路）のとき handler を呼ばず ack する（Req 1.3）",
			verifier:       &fakeVerifier{env: validEnvelope(Enrollment)},
			dedupe:         &fakeDedupe{claimConflict: true},
			unassigned:     &fakeUnassigned{},
			resolver:       &fakeResolver{id: resolvedTenantID, found: true},
			handler:        &fakeHandler{},
			wantAck:        true,
			wantHandlerHit: 0, // 敗者は handler を呼ばない（二重実行防止）
			wantClaimHit:   1,
			wantReleaseHit: 0,
			wantEnqueueHit: 0,
		},
		{
			name:           "claim の DB 失敗（dispatch 経路）のとき handler を呼ばず nack 保持する（Req 1.4）",
			verifier:       &fakeVerifier{env: validEnvelope(Enrollment)},
			dedupe:         &fakeDedupe{claimErr: transientErr()},
			unassigned:     &fakeUnassigned{},
			resolver:       &fakeResolver{id: resolvedTenantID, found: true},
			handler:        &fakeHandler{},
			wantAck:        false,
			wantHandlerHit: 0,
			wantClaimHit:   1,
			wantReleaseHit: 0,
			wantEnqueueHit: 0,
		},
		{
			name:           "transient handler 失敗のとき claim を release して nack 保持する（Req 5.1 5.3）",
			verifier:       &fakeVerifier{env: validEnvelope(Enrollment)},
			dedupe:         &fakeDedupe{},
			unassigned:     &fakeUnassigned{},
			resolver:       &fakeResolver{id: resolvedTenantID, found: true},
			handler:        &fakeHandler{err: transientErr()},
			wantAck:        false,
			wantHandlerHit: 1,
			wantClaimHit:   1,
			wantReleaseHit: 1, // handler 失敗 → claim 取り消し
			wantEnqueueHit: 0,
		},
		{
			name:           "handler 失敗かつ release 失敗でも元の error で nack 保持する（claim orphan は ERROR ログ）",
			verifier:       &fakeVerifier{env: validEnvelope(Enrollment)},
			dedupe:         &fakeDedupe{releaseErr: transientErr()},
			unassigned:     &fakeUnassigned{},
			resolver:       &fakeResolver{id: resolvedTenantID, found: true},
			handler:        &fakeHandler{err: transientErr()},
			wantAck:        false, // release 失敗は元の handler 失敗の nack 判定を上書きしない
			wantHandlerHit: 1,
			wantClaimHit:   1,
			wantReleaseHit: 1,
			wantEnqueueHit: 0,
		},
		{
			name:           "成功時に claim 後 handler を 1 回呼び release せず ack する（Req 1.1 5.2）",
			verifier:       &fakeVerifier{env: validEnvelope(Enrollment)},
			dedupe:         &fakeDedupe{},
			unassigned:     &fakeUnassigned{},
			resolver:       &fakeResolver{id: resolvedTenantID, found: true},
			handler:        &fakeHandler{},
			wantAck:        true,
			wantHandlerHit: 1,
			wantClaimHit:   1,
			wantReleaseHit: 0, // 成功時は claim を残す
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
			if tt.dedupe.claimHit != tt.wantClaimHit {
				t.Errorf("Claim 呼び出し回数 mismatch: want %d, got %d", tt.wantClaimHit, tt.dedupe.claimHit)
			}
			if tt.dedupe.releaseHit != tt.wantReleaseHit {
				t.Errorf("Release 呼び出し回数 mismatch: want %d, got %d", tt.wantReleaseHit, tt.dedupe.releaseHit)
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
