package db

import (
	stdErrors "errors"
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	internalerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// fakePool は txBeginner interface を満たすテスト用 pool。
// BeginTx 呼び出しのたびに事前構築された fakeTx を返す（または注入された error を返す）。
type fakePool struct {
	tx       *fakeTx
	beginErr error
	beginN   int
}

func (p *fakePool) BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error) {
	p.beginN++
	if p.beginErr != nil {
		return nil, p.beginErr
	}
	return p.tx, nil
}

// ctxWithTenant はテスト用 context に通常テナント文脈の TenantContext を put する helper。
func ctxWithTenant(t *testing.T) context.Context {
	t.Helper()
	return WithTenantContext(context.Background(), TenantContext{
		TenantID:    uuid.New(),
		AdminUserID: uuid.New(),
		Roles:       []string{"TenantAdmin"},
	})
}

// TestBeginTxFunc_TenantContextMissing_Panics は requirements.md Req 4.5 と
// design.md「ctx に TenantContext が無い場合 panic」契約に対応する。
//
// 通常運用では発生しないことが invariant であり、recover middleware（task 4.1）が
// 500 + 構造化 ERROR ログに写像する想定。本テストでは panic の payload が
// *errors.Error{Code: CodeTenantCtxMissing} であることを確認する。
func TestBeginTxFunc_TenantContextMissing_Panics(t *testing.T) {
	// Arrange: TenantContext を put していない素の context を用意する。
	bareCtx := context.Background()
	pool := &fakePool{tx: &fakeTx{}}

	// Act + Assert: panic を recover で捕捉し、payload を検査する。
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("BeginTxFunc は panic することを期待")
		}
		de, ok := r.(*internalerrors.Error)
		if !ok {
			t.Fatalf("panic payload は *errors.Error を期待, got %T (%v)", r, r)
		}
		if de.Code != internalerrors.CodeTenantCtxMissing {
			t.Errorf("Code = %q; want %q", de.Code, internalerrors.CodeTenantCtxMissing)
		}
		if pool.beginN != 0 {
			t.Errorf("pool.BeginTx は呼ばれてはならない; beginN = %d", pool.beginN)
		}
	}()

	_ = beginTxFuncWith(bareCtx, pool, func(tx pgx.Tx) error { return nil })
}

// TestBeginTxFunc_NormalReturn_Commits は正常終了経路。fn が nil error を返したとき
// commit が 1 回呼ばれ、rollback は 0 回であることを確認する（requirements.md Req 4.2 /
// design.md「正常終了 → commit」と整合）。
func TestBeginTxFunc_NormalReturn_Commits(t *testing.T) {
	// Arrange
	tx := &fakeTx{}
	pool := &fakePool{tx: tx}
	ctx := ctxWithTenant(t)

	fnCalls := 0

	// Act
	err := beginTxFuncWith(ctx, pool, func(actual pgx.Tx) error {
		fnCalls++
		// SetLocalTenant が事前に走っているはずなので Exec が 1 回記録されているべき。
		if len(tx.execCalls) != 1 {
			t.Errorf("fn 実行時点で SetLocalTenant が走っていない; Exec 件数 = %d", len(tx.execCalls))
		}
		return nil
	})

	// Assert
	if err != nil {
		t.Fatalf("BeginTxFunc: %v", err)
	}
	if fnCalls != 1 {
		t.Errorf("fn は 1 回呼ばれること; fnCalls = %d", fnCalls)
	}
	if tx.commitCalls != 1 {
		t.Errorf("commit は 1 回呼ばれること; commitCalls = %d", tx.commitCalls)
	}
	if tx.rollbackN != 0 {
		t.Errorf("rollback は呼ばれてはならない; rollbackN = %d", tx.rollbackN)
	}
}

// TestBeginTxFunc_FnReturnsError_RollsBackAndReturnsError は requirements.md Req 4.2 と
// design.md「fn が error を返した → rollback + その error 返す」契約に対応する。
func TestBeginTxFunc_FnReturnsError_RollsBackAndReturnsError(t *testing.T) {
	// Arrange
	tx := &fakeTx{}
	pool := &fakePool{tx: tx}
	ctx := ctxWithTenant(t)
	sentinel := fmt.Errorf("fn business failure")

	// Act
	err := beginTxFuncWith(ctx, pool, func(actual pgx.Tx) error {
		return sentinel
	})

	// Assert
	if !stdErrors.Is(err, sentinel) {
		t.Fatalf("BeginTxFunc は fn の error をそのまま返すこと; got %v", err)
	}
	if tx.rollbackN != 1 {
		t.Errorf("rollback は 1 回呼ばれること; rollbackN = %d", tx.rollbackN)
	}
	if tx.commitCalls != 0 {
		t.Errorf("commit は呼ばれてはならない; commitCalls = %d", tx.commitCalls)
	}
}

// TestBeginTxFunc_FnPanics_RollsBackAndRePanics は requirements.md Req 4.5 と
// design.md「fn が panic した → rollback + re-panic（recover はせず上位 middleware に委譲）」
// 契約に対応する。
func TestBeginTxFunc_FnPanics_RollsBackAndRePanics(t *testing.T) {
	// Arrange
	tx := &fakeTx{}
	pool := &fakePool{tx: tx}
	ctx := ctxWithTenant(t)
	panicPayload := "deliberate fn panic"

	// Act + Assert
	defer func() {
		r := recover()
		if r != panicPayload {
			t.Fatalf("panic payload = %v; want %q（同一 payload で re-panic）", r, panicPayload)
		}
		if tx.rollbackN != 1 {
			t.Errorf("rollback は 1 回呼ばれること; rollbackN = %d", tx.rollbackN)
		}
		if tx.commitCalls != 0 {
			t.Errorf("commit は呼ばれてはならない; commitCalls = %d", tx.commitCalls)
		}
	}()

	_ = beginTxFuncWith(ctx, pool, func(actual pgx.Tx) error {
		panic(panicPayload)
	})
}

// TestBeginTxFunc_SetLocalTenantFails_RollsBackAndReturnsError は SetLocalTenant の
// Exec が失敗した場合の経路。tx.Exec が error を返すと SetLocalTenant が
// *errors.Error{Code: CodeInternal} を返し、BeginTxFunc は rollback + その error を返す。
func TestBeginTxFunc_SetLocalTenantFails_RollsBackAndReturnsError(t *testing.T) {
	// Arrange
	tx := &fakeTx{execErr: fmt.Errorf("simulated set_config failure")}
	pool := &fakePool{tx: tx}
	ctx := ctxWithTenant(t)

	fnCalls := 0

	// Act
	err := beginTxFuncWith(ctx, pool, func(actual pgx.Tx) error {
		fnCalls++
		return nil
	})

	// Assert
	if err == nil {
		t.Fatalf("err == nil; CodeInternal で wrap された error を期待")
	}
	var de *internalerrors.Error
	if !stdErrors.As(err, &de) || de.Code != internalerrors.CodeInternal {
		t.Errorf("err = %v; want CodeInternal", err)
	}
	if fnCalls != 0 {
		t.Errorf("SetLocalTenant 失敗時は fn を呼んではならない; fnCalls = %d", fnCalls)
	}
	if tx.rollbackN != 1 {
		t.Errorf("rollback は 1 回呼ばれること; rollbackN = %d", tx.rollbackN)
	}
	if tx.commitCalls != 0 {
		t.Errorf("commit は呼ばれてはならない")
	}
}

// TestBeginTxFunc_BeginTxFails_WrapsAsInternal は pool.BeginTx が失敗した場合の経路。
// fn は呼ばれず、*errors.Error{Code: CodeInternal} で wrap された error が返る。
func TestBeginTxFunc_BeginTxFails_WrapsAsInternal(t *testing.T) {
	// Arrange
	sentinel := fmt.Errorf("simulated begin failure")
	pool := &fakePool{beginErr: sentinel}
	ctx := ctxWithTenant(t)

	fnCalls := 0

	// Act
	err := beginTxFuncWith(ctx, pool, func(actual pgx.Tx) error {
		fnCalls++
		return nil
	})

	// Assert
	if err == nil {
		t.Fatalf("err == nil; CodeInternal を期待")
	}
	var de *internalerrors.Error
	if !stdErrors.As(err, &de) || de.Code != internalerrors.CodeInternal {
		t.Errorf("err = %v; want CodeInternal", err)
	}
	if !stdErrors.Is(err, sentinel) {
		t.Errorf("Cause が sentinel を保持していない")
	}
	if fnCalls != 0 {
		t.Errorf("BeginTx 失敗時は fn を呼んではならない; fnCalls = %d", fnCalls)
	}
}

// TestBeginTxFunc_CommitFails_WrapsAsInternal は commit エラーが
// *errors.Error{Code: CodeInternal} で wrap されることを確認する。
func TestBeginTxFunc_CommitFails_WrapsAsInternal(t *testing.T) {
	// Arrange
	sentinel := fmt.Errorf("simulated commit failure")
	tx := &fakeTx{commitErr: sentinel}
	pool := &fakePool{tx: tx}
	ctx := ctxWithTenant(t)

	// Act
	err := beginTxFuncWith(ctx, pool, func(actual pgx.Tx) error { return nil })

	// Assert
	if err == nil {
		t.Fatalf("err == nil; commit エラーで CodeInternal を期待")
	}
	var de *internalerrors.Error
	if !stdErrors.As(err, &de) || de.Code != internalerrors.CodeInternal {
		t.Errorf("err = %v; want CodeInternal", err)
	}
	if !stdErrors.Is(err, sentinel) {
		t.Errorf("Cause が sentinel を保持していない")
	}
}

// TestBeginTxFunc_NilPool_ReturnsInternal は防御的経路。
// 公開 API BeginTxFunc に nil pool を渡しても panic せず *errors.Error{Code: CodeInternal}
// を返すこと（design.md「pool は cmd/api の bootstrap で必ず構築済み」前提で
// 通常運用では発生しないが、fail-fast 用の安全網として確認）。
func TestBeginTxFunc_NilPool_ReturnsInternal(t *testing.T) {
	ctx := ctxWithTenant(t)
	err := BeginTxFunc(ctx, nil, func(tx pgx.Tx) error { return nil })

	if err == nil {
		t.Fatalf("err == nil; CodeInternal を期待")
	}
	var de *internalerrors.Error
	if !stdErrors.As(err, &de) || de.Code != internalerrors.CodeInternal {
		t.Errorf("err = %v; want CodeInternal", err)
	}
}

// TestFromContext_Missing_ReturnsTenantCtxMissing は requirements.md Req 4.5 / 5.2 の
// 入口契約に対応する。TenantContext を put していない ctx から FromContext を呼ぶと
// *errors.Error{Code: CodeTenantCtxMissing} が返る。
func TestFromContext_Missing_ReturnsTenantCtxMissing(t *testing.T) {
	tc, err := FromContext(context.Background())

	if err == nil {
		t.Fatalf("err == nil; CodeTenantCtxMissing を期待")
	}
	var de *internalerrors.Error
	if !stdErrors.As(err, &de) || de.Code != internalerrors.CodeTenantCtxMissing {
		t.Errorf("err = %v; want CodeTenantCtxMissing", err)
	}
	// TenantContext は []string を含むため `!=` 比較できない。
	// 個別フィールドの zero value 等価性で代用する。
	if tc.TenantID != uuid.Nil || tc.AdminUserID != uuid.Nil || tc.IsSuperAdmin || len(tc.Roles) != 0 {
		t.Errorf("err 時の戻り値 TenantContext は zero value を期待; got %+v", tc)
	}
}

// TestFromContext_NilCtx_ReturnsTenantCtxMissing は nil ctx の防御的経路。
func TestFromContext_NilCtx_ReturnsTenantCtxMissing(t *testing.T) {
	_, err := FromContext(nil)
	if err == nil {
		t.Fatalf("err == nil; CodeTenantCtxMissing を期待")
	}
	var de *internalerrors.Error
	if !stdErrors.As(err, &de) || de.Code != internalerrors.CodeTenantCtxMissing {
		t.Errorf("err = %v; want CodeTenantCtxMissing", err)
	}
}

// TestWithTenantContext_RoundTrip は WithTenantContext + FromContext の往復で
// 値が保持されることを確認する（requirements.md Req 5.2 の入口契約）。
func TestWithTenantContext_RoundTrip(t *testing.T) {
	tc := TenantContext{
		TenantID:     uuid.New(),
		AdminUserID:  uuid.New(),
		Roles:        []string{"TenantAdmin", "Viewer"},
		IsSuperAdmin: false,
	}

	ctx := WithTenantContext(context.Background(), tc)
	got, err := FromContext(ctx)
	if err != nil {
		t.Fatalf("FromContext: %v", err)
	}

	if got.TenantID != tc.TenantID {
		t.Errorf("TenantID = %v; want %v", got.TenantID, tc.TenantID)
	}
	if got.AdminUserID != tc.AdminUserID {
		t.Errorf("AdminUserID = %v; want %v", got.AdminUserID, tc.AdminUserID)
	}
	if got.IsSuperAdmin != tc.IsSuperAdmin {
		t.Errorf("IsSuperAdmin = %v; want %v", got.IsSuperAdmin, tc.IsSuperAdmin)
	}
	if len(got.Roles) != len(tc.Roles) {
		t.Errorf("Roles 件数不一致; got %v, want %v", got.Roles, tc.Roles)
	}
}
