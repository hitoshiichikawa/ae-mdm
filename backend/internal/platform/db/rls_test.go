package db

import (
	stdErrors "errors"
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	internalerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// execCall は fake tx が記録する 1 件の Exec 呼び出し。
type execCall struct {
	sql  string
	args []any
}

// fakeTx は pgx.Tx interface を満たす最小モック。Exec / Commit / Rollback の履歴と
// 注入する error を保持する。テストで実 PostgreSQL に依存しないようにするための仕掛け。
type fakeTx struct {
	execCalls   []execCall
	commitCalls int
	rollbackN   int

	// 注入する error（nil なら成功）。
	execErr     error
	commitErr   error
	rollbackErr error
}

func (f *fakeTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.execCalls = append(f.execCalls, execCall{sql: sql, args: args})
	if f.execErr != nil {
		return pgconn.CommandTag{}, f.execErr
	}
	return pgconn.NewCommandTag("SELECT 1"), nil
}

func (f *fakeTx) Commit(ctx context.Context) error {
	f.commitCalls++
	return f.commitErr
}

func (f *fakeTx) Rollback(ctx context.Context) error {
	f.rollbackN++
	return f.rollbackErr
}

// pgx.Tx の残りメソッドは本テストでは使わないため最小実装で stub する。
func (f *fakeTx) Begin(ctx context.Context) (pgx.Tx, error)                  { return nil, nil }
func (f *fakeTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 0, nil
}
func (f *fakeTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults { return nil }
func (f *fakeTx) LargeObjects() pgx.LargeObjects                        { return pgx.LargeObjects{} }
func (f *fakeTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	return nil, nil
}
func (f *fakeTx) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, nil }
func (f *fakeTx) QueryRow(context.Context, string, ...any) pgx.Row        { return nil }
func (f *fakeTx) Conn() *pgx.Conn                                         { return nil }

// compile-time check: *fakeTx は pgx.Tx を満たす。
var _ pgx.Tx = (*fakeTx)(nil)

// helper: tenant uuid を生成する（テスト内で一意・固定）。
func mustTenantID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatalf("uuid parse: %v", err)
	}
	return id
}

// TestSetLocalTenant_NormalTenant_EmitsSetConfigForTenantID は requirements.md Req 4.3 と
// design.md「set_config('app.tenant_id', $1, true)」契約に対応する。通常テナント文脈
// （IsSuperAdmin=false）では `app.tenant_id` だけがバインドパラメータ経由で発行され、
// `app.is_superadmin` は発行されないことを確認する。
func TestSetLocalTenant_NormalTenant_EmitsSetConfigForTenantID(t *testing.T) {
	// Arrange
	tx := &fakeTx{}
	tenantID := mustTenantID(t, "11111111-1111-1111-1111-111111111111")
	tc := TenantContext{
		TenantID:     tenantID,
		AdminUserID:  uuid.New(),
		Roles:        []string{"TenantAdmin"},
		IsSuperAdmin: false,
	}

	// Act
	err := SetLocalTenant(context.Background(), tx, tc)

	// Assert
	if err != nil {
		t.Fatalf("SetLocalTenant: %v", err)
	}
	if len(tx.execCalls) != 1 {
		t.Fatalf("Exec 呼び出し数 = %d; want 1（app.tenant_id のみ）", len(tx.execCalls))
	}
	got := tx.execCalls[0]
	wantSQL := "SELECT set_config('app.tenant_id', $1, true)"
	if got.sql != wantSQL {
		t.Errorf("Exec SQL = %q; want %q", got.sql, wantSQL)
	}
	if len(got.args) != 1 {
		t.Fatalf("Exec args 件数 = %d; want 1", len(got.args))
	}
	if got.args[0] != tenantID.String() {
		t.Errorf("Exec args[0] = %v; want %q", got.args[0], tenantID.String())
	}
}

// TestSetLocalTenant_SuperAdminWithTenantID_EmitsBothSetConfigs は requirements.md Req 4.4
// に対応する。SuperAdmin で TenantID が指定された場合（テナント文脈下での
// SuperAdmin 操作）には `app.tenant_id` と `app.is_superadmin = 'true'` の両方が発行される。
func TestSetLocalTenant_SuperAdminWithTenantID_EmitsBothSetConfigs(t *testing.T) {
	// Arrange
	tx := &fakeTx{}
	tenantID := mustTenantID(t, "22222222-2222-2222-2222-222222222222")
	tc := TenantContext{
		TenantID:     tenantID,
		AdminUserID:  uuid.New(),
		Roles:        []string{"SuperAdmin"},
		IsSuperAdmin: true,
	}

	// Act
	err := SetLocalTenant(context.Background(), tx, tc)

	// Assert
	if err != nil {
		t.Fatalf("SetLocalTenant: %v", err)
	}
	if len(tx.execCalls) != 2 {
		t.Fatalf("Exec 呼び出し数 = %d; want 2（app.tenant_id + app.is_superadmin）", len(tx.execCalls))
	}
	if tx.execCalls[0].sql != "SELECT set_config('app.tenant_id', $1, true)" {
		t.Errorf("1st SQL = %q; want app.tenant_id", tx.execCalls[0].sql)
	}
	if tx.execCalls[1].sql != "SELECT set_config('app.is_superadmin', 'true', true)" {
		t.Errorf("2nd SQL = %q; want app.is_superadmin", tx.execCalls[1].sql)
	}
}

// TestSetLocalTenant_SuperAdminCrossTenant_OnlyIsSuperAdminEmitted は requirements.md Req 4.4
// と design.md「TenantID==uuid.Nil の SuperAdmin は app.tenant_id をセットしない」
// 注記に対応する。SuperAdmin + TenantID==uuid.Nil の cross-tenant 操作では
// `app.tenant_id` は発行されず `app.is_superadmin = 'true'` のみが発行され、
// current_setting('app.tenant_id', true) が NULL → default deny に倒れる経路を構成する。
func TestSetLocalTenant_SuperAdminCrossTenant_OnlyIsSuperAdminEmitted(t *testing.T) {
	// Arrange
	tx := &fakeTx{}
	tc := TenantContext{
		TenantID:     uuid.Nil,
		AdminUserID:  uuid.New(),
		Roles:        []string{"SuperAdmin"},
		IsSuperAdmin: true,
	}

	// Act
	err := SetLocalTenant(context.Background(), tx, tc)

	// Assert
	if err != nil {
		t.Fatalf("SetLocalTenant: %v", err)
	}
	if len(tx.execCalls) != 1 {
		t.Fatalf("Exec 呼び出し数 = %d; want 1（app.is_superadmin のみ）", len(tx.execCalls))
	}
	if tx.execCalls[0].sql != "SELECT set_config('app.is_superadmin', 'true', true)" {
		t.Errorf("SQL = %q; want app.is_superadmin", tx.execCalls[0].sql)
	}
	if len(tx.execCalls[0].args) != 0 {
		t.Errorf("args 件数 = %d; want 0（bind 不要）", len(tx.execCalls[0].args))
	}
}

// TestSetLocalTenant_ExecError_WrapsAsInternal は異常系。tx.Exec が error を返した場合、
// *errors.Error{Code: CodeInternal} で wrap されることを確認する（design.md「エラーは
// *errors.Error{Code: CodeInternal} で wrap」と整合）。
func TestSetLocalTenant_ExecError_WrapsAsInternal(t *testing.T) {
	// Arrange
	sentinel := fmt.Errorf("simulated exec failure")
	tx := &fakeTx{execErr: sentinel}
	tc := TenantContext{
		TenantID:    mustTenantID(t, "33333333-3333-3333-3333-333333333333"),
		AdminUserID: uuid.New(),
	}

	// Act
	err := SetLocalTenant(context.Background(), tx, tc)

	// Assert
	if err == nil {
		t.Fatalf("err == nil; CodeInternal で wrap された error を期待")
	}
	var de *internalerrors.Error
	if !stdErrors.As(err, &de) {
		t.Fatalf("err は *errors.Error として取り出せること, got %T", err)
	}
	if de.Code != internalerrors.CodeInternal {
		t.Errorf("Code = %q; want %q", de.Code, internalerrors.CodeInternal)
	}
	if !stdErrors.Is(err, sentinel) {
		t.Errorf("Cause が sentinel error を保持していない")
	}
}

// TestSetLocalTenant_NilTx_ReturnsInternal は nil tx を渡された場合の防御的経路。
func TestSetLocalTenant_NilTx_ReturnsInternal(t *testing.T) {
	err := SetLocalTenant(context.Background(), nil, TenantContext{})

	if err == nil {
		t.Fatalf("err == nil; nil tx で CodeInternal を期待")
	}
	var de *internalerrors.Error
	if !stdErrors.As(err, &de) || de.Code != internalerrors.CodeInternal {
		t.Errorf("err = %v; want CodeInternal", err)
	}
}

// compile-time check: *pgxpool.Pool は txBeginner interface を自然に満たす。
// （beginTxFuncWith の signature と一致することを compile 時に固定する）
var _ txBeginner = (*pgxpool.Pool)(nil)
