package enrollment

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
)

// fakeDeviceExecer は deviceExecer を満たす test double。実行された SQL / bind 引数を捕捉し、
// err を設定すると Exec 失敗（DB 失敗）を模擬する。実 PostgreSQL に依存しない。
type fakeDeviceExecer struct {
	calls    int
	lastSQL  string
	lastArgs []any
	err      error
}

func (f *fakeDeviceExecer) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	f.calls++
	f.lastSQL = sql
	f.lastArgs = args
	if f.err != nil {
		return pgconn.CommandTag{}, f.err
	}
	return pgconn.CommandTag{}, nil
}

// txRunner は Registrar.withTx seam の test double。fn を fake execer へ直結し、tenant-scoped tx を
// 開いた回数（invoked）を記録する。ガード検証で「tx を開かない = exec 非呼出」を固定するのに使う。
type txRunner struct {
	invoked int
	exec    *fakeDeviceExecer
	// txErr を設定すると withTx（BeginTxFunc 相当）自体が error を返す状況（BeginTx / Commit 由来）を模擬する。
	txErr error
}

func (t *txRunner) run(_ context.Context, fn func(deviceExecer) error) error {
	t.invoked++
	if t.txErr != nil {
		return t.txErr
	}
	return fn(t.exec)
}

func newTestRegistrar(runner *txRunner) *Registrar {
	return &Registrar{withTx: runner.run}
}

func ctxWithTenant(tenantID uuid.UUID) context.Context {
	return db.WithTenantContext(context.Background(), db.TenantContext{TenantID: tenantID})
}

// Req 3.1 / NFR 2.1: TenantContext を確立した ctx で呼ぶと tenant_id（ctx 由来）と
// amapi_device_name / mode / compliance_status が bind へ正しく写像される（越境更新不可の構造を bind で固定）。
func TestRegistrar_UpsertEnrolledDevice_BindsTenantFromContext(t *testing.T) {
	// Arrange
	tenantID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	runner := &txRunner{exec: &fakeDeviceExecer{}}
	r := newTestRegistrar(runner)
	ctx := ctxWithTenant(tenantID)

	// Act
	err := r.UpsertEnrolledDevice(ctx, "enterprises/LC01/devices/dev-1", "fully_managed", "unknown")

	// Assert
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if runner.exec.calls != 1 {
		t.Fatalf("expected exec called exactly once, got %d", runner.exec.calls)
	}
	args := runner.exec.lastArgs
	if len(args) != 5 {
		t.Fatalf("expected 5 bind args, got %d", len(args))
	}
	if gotTenant, ok := args[1].(uuid.UUID); !ok || gotTenant != tenantID {
		t.Errorf("expected tenant_id bound from ctx = %s, got %v", tenantID, args[1])
	}
	if gotName, ok := args[2].(string); !ok || gotName != "enterprises/LC01/devices/dev-1" {
		t.Errorf("expected amapi_device_name bound, got %v", args[2])
	}
	if gotMode, ok := args[3].(string); !ok || gotMode != "fully_managed" {
		t.Errorf("expected mode bound, got %v", args[3])
	}
	if gotCompliance, ok := args[4].(string); !ok || gotCompliance != "unknown" {
		t.Errorf("expected compliance_status bound, got %v", args[4])
	}
	// id（args[0]）は初回 INSERT 用の fresh uuid（ON CONFLICT 時のみ既存 id が保持される）。
	if gotID, ok := args[0].(uuid.UUID); !ok || gotID == uuid.Nil {
		t.Errorf("expected fresh non-nil uuid id, got %v", args[0])
	}
}

// NFR 2.1（ガード）: TenantContext 未確立の ctx では tx を開かず（exec 非呼出）エラーを返す
// （越境更新を構造的に防ぐ安全側ガード）。
func TestRegistrar_UpsertEnrolledDevice_MissingTenantContext_GuardsWithoutExec(t *testing.T) {
	// Arrange: ctx に TenantContext を確立しない
	runner := &txRunner{exec: &fakeDeviceExecer{}}
	r := newTestRegistrar(runner)

	// Act
	err := r.UpsertEnrolledDevice(context.Background(), "enterprises/LC01/devices/dev-1", "fully_managed", "unknown")

	// Assert
	if err == nil {
		t.Fatalf("expected error when TenantContext is missing, got nil")
	}
	if runner.invoked != 0 {
		t.Errorf("expected tenant-scoped tx not opened, invoked=%d", runner.invoked)
	}
	if runner.exec.calls != 0 {
		t.Errorf("expected exec never called on missing tenant ctx, got %d", runner.exec.calls)
	}
}

// Req 3.4: 発行 SQL に冪等 upsert 機構（ON CONFLICT (tenant_id, amapi_device_name) DO UPDATE ...）が
// 含まれることを固定する（実際の重複登録なし挙動は task 6 結合テストが所有）。
func TestRegistrar_UpsertEnrolledDevice_EmitsIdempotentOnConflictSQL(t *testing.T) {
	// Arrange
	runner := &txRunner{exec: &fakeDeviceExecer{}}
	r := newTestRegistrar(runner)
	ctx := ctxWithTenant(uuid.New())

	// Act
	if err := r.UpsertEnrolledDevice(ctx, "enterprises/LC01/devices/dev-1", "dedicated", "unsupported"); err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}

	// Assert
	sql := runner.exec.lastSQL
	if !strings.Contains(sql, "INSERT INTO devices (id, tenant_id, amapi_device_name, mode, compliance_status)") {
		t.Errorf("expected INSERT INTO devices with the 5 upsert columns, got:\n%s", sql)
	}
	if !strings.Contains(sql, "ON CONFLICT (tenant_id, amapi_device_name)") {
		t.Errorf("expected ON CONFLICT (tenant_id, amapi_device_name), got:\n%s", sql)
	}
	if !strings.Contains(sql, "DO UPDATE SET mode = EXCLUDED.mode, compliance_status = EXCLUDED.compliance_status") {
		t.Errorf("expected DO UPDATE SET mode/compliance from EXCLUDED, got:\n%s", sql)
	}
}

// mode / compliance の値域写像: fully_managed/dedicated × unknown/unsupported の代表組み合わせが
// bind へそのまま渡ることを table-driven で固定する。
func TestRegistrar_UpsertEnrolledDevice_MapsModeAndComplianceToBind(t *testing.T) {
	cases := []struct {
		name       string
		mode       string
		compliance string
	}{
		{"fully_managed_unknown", "fully_managed", "unknown"},
		{"fully_managed_unsupported", "fully_managed", "unsupported"},
		{"dedicated_unknown", "dedicated", "unknown"},
		{"dedicated_unsupported", "dedicated", "unsupported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Arrange
			runner := &txRunner{exec: &fakeDeviceExecer{}}
			r := newTestRegistrar(runner)
			ctx := ctxWithTenant(uuid.New())

			// Act
			if err := r.UpsertEnrolledDevice(ctx, "enterprises/LC01/devices/dev-x", tc.mode, tc.compliance); err != nil {
				t.Fatalf("expected nil error, got %v", err)
			}

			// Assert
			args := runner.exec.lastArgs
			if got, ok := args[3].(string); !ok || got != tc.mode {
				t.Errorf("expected mode=%s bound, got %v", tc.mode, args[3])
			}
			if got, ok := args[4].(string); !ok || got != tc.compliance {
				t.Errorf("expected compliance_status=%s bound, got %v", tc.compliance, args[4])
			}
		})
	}
}

// Req 3.6 への写像 / design 225: exec（DB）失敗は *errors.Error{Code: CodeUnavailable, IsTransient: true}
// で返す（nack = 再処理保持）。Message に機密値（device 名）を補間しないことも確認する（NFR 3.1）。
func TestRegistrar_UpsertEnrolledDevice_ExecError_ReturnsTransientUnavailable(t *testing.T) {
	// Arrange: exec が DB 失敗を返す
	runner := &txRunner{exec: &fakeDeviceExecer{err: errors.New("connection refused")}}
	r := newTestRegistrar(runner)
	ctx := ctxWithTenant(uuid.New())

	// Act
	err := r.UpsertEnrolledDevice(ctx, "enterprises/LC01/devices/dev-secret", "fully_managed", "unknown")

	// Assert
	if err == nil {
		t.Fatalf("expected error on exec failure, got nil")
	}
	var de *pkgerrors.Error
	if !errors.As(err, &de) {
		t.Fatalf("expected *errors.Error, got %T", err)
	}
	if de.Code != pkgerrors.CodeUnavailable {
		t.Errorf("expected CodeUnavailable, got %s", de.Code)
	}
	if !pkgerrors.IsTransient(err) {
		t.Errorf("expected IsTransient=true (nack / 再処理保持), got false")
	}
	// NFR 3.1: 固定文言で機密値（device 名・payload 生値）を Message に補間しない。
	if strings.Contains(de.Message, "dev-secret") {
		t.Errorf("expected fixed message without device identifier, got %q", de.Message)
	}
}

// design 225（tenant.asTransientReverseLookupErr を手本）: BeginTx / Commit 由来の非 transient error も
// transient CodeUnavailable へ正規化し、DB 失敗を一律 nack（再処理保持）へ写像することを固定する。
func TestRegistrar_UpsertEnrolledDevice_NonTransientTxError_NormalizedToTransient(t *testing.T) {
	// Arrange: withTx（BeginTxFunc 相当）自体が非 transient error を返す
	runner := &txRunner{
		exec:  &fakeDeviceExecer{},
		txErr: pkgerrors.New(pkgerrors.CodeInternal, "tx begin failed"),
	}
	r := newTestRegistrar(runner)
	ctx := ctxWithTenant(uuid.New())

	// Act
	err := r.UpsertEnrolledDevice(ctx, "enterprises/LC01/devices/dev-1", "fully_managed", "unknown")

	// Assert
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !pkgerrors.IsTransient(err) {
		t.Errorf("expected non-transient tx error normalized to transient (nack), got non-transient")
	}
	var de *pkgerrors.Error
	if !errors.As(err, &de) || de.Code != pkgerrors.CodeUnavailable {
		t.Errorf("expected CodeUnavailable, got %v", err)
	}
}
