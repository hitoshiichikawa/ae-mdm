package integration_test

import (
	"context"
	stdErrors "errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	internalerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	platformdb "github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
)

// sqlstatePermissionDenied は PostgreSQL SQLSTATE で「permission denied」を示すコード。
// REVOKE 後の UPDATE/DELETE は本コードを返す。
const sqlstatePermissionDenied = "42501"

// sqlstateCheckViolation は PostgreSQL SQLSTATE で WITH CHECK 違反を示すコード。
// audit_logs INSERT の WITH CHECK policy 違反時に返る。
const sqlstateCheckViolation = "42501" // policy violation も pgx では 42501 として上がる

// TestMain は package 全体の setup / teardown を担う。
// (1) DB URL が無い → 何もせず m.Run へ（個別 test が requireDBURLs で skip）
// (2) DB URL あり → migrate up でクリーンな状態を確立し、テスト終了時に migrate down で完全に戻す
//
// 個別テスト間の独立性は各テスト冒頭の truncateAll で担保し、TestMain は schema 構築のみ責務。
// migrations_reversible_test.go は自前で up/down を繰り返すため、本 setup の after に
// 改めて up しなおして次の test が走れる状態を維持する（reversible test 内部で完結する設計）。
func TestMain(m *testing.M) {
	// Test 個別の setup は各テスト関数で行う。TestMain では deferred work が
	// os.Exit と相性が悪いため、共有 setup は最小化（個別 test の冒頭で applyMigrationsUp）。
	m.Run()
}

// TestDBTenantIsolation_DevicesSelectExcludesOtherTenant はテストシナリオ (b) 対応。
// TenantContext=A で SELECT FROM devices すると、tenant A の行のみが返り tenant B の行は
// 0 件であることを確認する（requirements.md Req 6.4 / NFR 1.2）。
func TestDBTenantIsolation_DevicesSelectExcludesOtherTenant(t *testing.T) {
	// Arrange
	urls := requireDBURLs(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	applyMigrationsUp(t, urls.migrate)
	truncateAll(t, ctx, urls)
	pool := newAppPool(t, ctx, urls)
	defer pool.Close()
	ids := seedDummyData(t, ctx, pool)

	tcA := platformdb.TenantContext{TenantID: ids.tenantAID, IsSuperAdmin: false}
	ctxA := platformdb.WithTenantContext(ctx, tcA)

	// Act + Assert
	var rowCountB int
	err := platformdb.BeginTxFunc(ctxA, pool, func(tx pgx.Tx) error {
		// tenant B の行は 0 件で見えないこと
		if err := tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM devices WHERE tenant_id = $1`, ids.tenantBID,
		).Scan(&rowCountB); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("BeginTxFunc: %v", err)
	}
	if rowCountB != 0 {
		t.Fatalf("tenant B の devices 行数 = %d; want 0（tenant A 文脈では見えない）", rowCountB)
	}

	// tenant A の行が見える
	var rowCountA int
	err = platformdb.BeginTxFunc(ctxA, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM devices`,
		).Scan(&rowCountA)
	})
	if err != nil {
		t.Fatalf("BeginTxFunc: %v", err)
	}
	if rowCountA != 1 {
		t.Errorf("tenant A 文脈での devices 行数 = %d; want 1", rowCountA)
	}
}

// TestDBTenantIsolation_DevicesUpdateDeleteOtherTenant_ZeroRows はシナリオ (c) 対応。
// TenantContext=A で tenant B の devices に対して UPDATE / DELETE を試みても 0 rows
// affected で返ること（requirements.md Req 6.4 / NFR 1.2）。
func TestDBTenantIsolation_DevicesUpdateDeleteOtherTenant_ZeroRows(t *testing.T) {
	urls := requireDBURLs(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	applyMigrationsUp(t, urls.migrate)
	truncateAll(t, ctx, urls)
	pool := newAppPool(t, ctx, urls)
	defer pool.Close()
	ids := seedDummyData(t, ctx, pool)

	tcA := platformdb.TenantContext{TenantID: ids.tenantAID, IsSuperAdmin: false}
	ctxA := platformdb.WithTenantContext(ctx, tcA)

	// Act + Assert: UPDATE
	err := platformdb.BeginTxFunc(ctxA, pool, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx,
			`UPDATE devices SET amapi_device_name = 'cross-tenant-hijack' WHERE id = $1`,
			ids.deviceBID)
		if err != nil {
			return err
		}
		if ct.RowsAffected() != 0 {
			t.Errorf("UPDATE rows affected = %d; want 0（tenant B 行は不可視）", ct.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("BeginTxFunc UPDATE: %v", err)
	}

	// Act + Assert: DELETE
	err = platformdb.BeginTxFunc(ctxA, pool, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx, `DELETE FROM devices WHERE id = $1`, ids.deviceBID)
		if err != nil {
			return err
		}
		if ct.RowsAffected() != 0 {
			t.Errorf("DELETE rows affected = %d; want 0（tenant B 行は不可視）", ct.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("BeginTxFunc DELETE: %v", err)
	}
}

// TestDBTenantIsolation_SuperAdminSeesAllTenants はシナリオ (d) 対応。
// IsSuperAdmin=true で SELECT FROM devices すると、全 tenant の行が返ること
// （requirements.md Req 6.3）。
func TestDBTenantIsolation_SuperAdminSeesAllTenants(t *testing.T) {
	urls := requireDBURLs(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	applyMigrationsUp(t, urls.migrate)
	truncateAll(t, ctx, urls)
	pool := newAppPool(t, ctx, urls)
	defer pool.Close()
	_ = seedDummyData(t, ctx, pool)

	tcSA := platformdb.TenantContext{TenantID: uuid.Nil, IsSuperAdmin: true}
	ctxSA := platformdb.WithTenantContext(ctx, tcSA)

	var total int
	err := platformdb.BeginTxFunc(ctxSA, pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM devices`).Scan(&total)
	})
	if err != nil {
		t.Fatalf("BeginTxFunc: %v", err)
	}
	if total != 2 {
		t.Errorf("SuperAdmin での devices 全行数 = %d; want 2（A + B）", total)
	}
}

// TestDBTenantIsolation_NoTenantContext_Panics はシナリオ (e) 対応。
// TenantContext を put しない ctx で BeginTxFunc を呼ぶと panic され、recover で
// `*errors.Error{Code: CodeTenantCtxMissing}` であることを確認する（Req 4.5）。
func TestDBTenantIsolation_NoTenantContext_Panics(t *testing.T) {
	urls := requireDBURLs(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	applyMigrationsUp(t, urls.migrate)
	pool := newAppPool(t, ctx, urls)
	defer pool.Close()

	// Act + Assert: panic 検知
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("BeginTxFunc は TenantContext 不在で panic することを期待")
		}
		err, ok := r.(*internalerrors.Error)
		if !ok {
			t.Fatalf("panic payload は *errors.Error を期待; got %T (%v)", r, r)
		}
		if err.Code != internalerrors.CodeTenantCtxMissing {
			t.Errorf("Code = %q; want %q", err.Code, internalerrors.CodeTenantCtxMissing)
		}
	}()
	// TenantContext を put しない素の ctx で BeginTxFunc を呼ぶ
	_ = platformdb.BeginTxFunc(ctx, pool, func(_ pgx.Tx) error { return nil })
}

// TestDBAuditLogs_AppUserUpdateDelete_Rejected はシナリオ (f) 対応。
// app_user 接続で audit_logs を UPDATE / DELETE しようとすると permission denied
// （SQLSTATE 42501）が返ること（requirements.md Req 7.1 / 7.2 / 7.4）。
//
// SuperAdmin 文脈で audit_logs に SELECT してから UPDATE / DELETE を試みることで、
// 「RLS policy 不在 + REVOKE」の二重防御が物理化されていることを確認する。
func TestDBAuditLogs_AppUserUpdateDelete_Rejected(t *testing.T) {
	urls := requireDBURLs(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	applyMigrationsUp(t, urls.migrate)
	truncateAll(t, ctx, urls)
	pool := newAppPool(t, ctx, urls)
	defer pool.Close()
	ids := seedDummyData(t, ctx, pool)

	tcSA := platformdb.TenantContext{TenantID: uuid.Nil, IsSuperAdmin: true}
	ctxSA := platformdb.WithTenantContext(ctx, tcSA)

	t.Run("UPDATE 試行: permission denied", func(t *testing.T) {
		err := platformdb.BeginTxFunc(ctxSA, pool, func(tx pgx.Tx) error {
			_, execErr := tx.Exec(ctx,
				`UPDATE audit_logs SET event_type = 'tampered' WHERE id = $1`,
				ids.auditAID)
			return execErr
		})
		// REVOKE による permission denied、または policy 不在による 0 rows どちらでも
		// 改竄不可は成立するが、本 spec では REVOKE が効いている状態を期待。
		if err == nil {
			t.Fatalf("UPDATE が成功した（permission denied / policy violation を期待）")
		}
		assertPgError(t, err, sqlstatePermissionDenied, "permission denied")
	})

	t.Run("DELETE 試行: permission denied", func(t *testing.T) {
		err := platformdb.BeginTxFunc(ctxSA, pool, func(tx pgx.Tx) error {
			_, execErr := tx.Exec(ctx, `DELETE FROM audit_logs WHERE id = $1`, ids.auditAID)
			return execErr
		})
		if err == nil {
			t.Fatalf("DELETE が成功した（permission denied を期待）")
		}
		assertPgError(t, err, sqlstatePermissionDenied, "permission denied")
	})
}

// TestDBAuditLogs_InsertWithCheck_NormalTenantContext はシナリオ (g-1) / (g-2) 対応。
// 通常テナント文脈（IsSuperAdmin=false / app.tenant_id=A）下で
//   - (g-1) tenant_id=B の audit_logs INSERT → policy violation
//   - (g-2) tenant_id=NULL の audit_logs INSERT → policy violation
//
// 両ケースとも WITH CHECK が物理拒否することを確認する（Req 7.4）。
func TestDBAuditLogs_InsertWithCheck_NormalTenantContext(t *testing.T) {
	urls := requireDBURLs(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	applyMigrationsUp(t, urls.migrate)
	truncateAll(t, ctx, urls)
	pool := newAppPool(t, ctx, urls)
	defer pool.Close()
	ids := seedDummyData(t, ctx, pool)

	tcA := platformdb.TenantContext{TenantID: ids.tenantAID, IsSuperAdmin: false}
	ctxA := platformdb.WithTenantContext(ctx, tcA)

	t.Run("g-1: tenant_id=B（cross-tenant）への INSERT は policy violation", func(t *testing.T) {
		err := platformdb.BeginTxFunc(ctxA, pool, func(tx pgx.Tx) error {
			_, execErr := tx.Exec(ctx,
				`INSERT INTO audit_logs (id, tenant_id, actor_id, event_type, result)
				 VALUES ($1, $2, $3, 'crosstenant.attempt', 'success')`,
				uuid.New(), ids.tenantBID, ids.adminAID)
			return execErr
		})
		if err == nil {
			t.Fatalf("cross-tenant INSERT が成功した（WITH CHECK 拒否を期待）")
		}
		assertPgError(t, err, sqlstateCheckViolation, "")
	})

	t.Run("g-2: tenant_id=NULL への INSERT は policy violation", func(t *testing.T) {
		err := platformdb.BeginTxFunc(ctxA, pool, func(tx pgx.Tx) error {
			_, execErr := tx.Exec(ctx,
				`INSERT INTO audit_logs (id, tenant_id, actor_id, event_type, result)
				 VALUES ($1, NULL, $2, 'system.attempt', 'success')`,
				uuid.New(), ids.adminAID)
			return execErr
		})
		if err == nil {
			t.Fatalf("tenant_id=NULL の INSERT が成功した（WITH CHECK 拒否を期待）")
		}
		assertPgError(t, err, sqlstateCheckViolation, "")
	})
}

// TestDBAuditLogs_InsertWithCheck_SuperAdminAllowsCrossTenantAndNull は (g-3) 対応。
// IsSuperAdmin=true 文脈で
//   - tenant_id=NULL（システム監査）
//   - tenant_id=B（cross-tenant 操作監査）
//
// 両方の INSERT が成功することを確認する（Req 7.3 と整合）。
func TestDBAuditLogs_InsertWithCheck_SuperAdminAllowsCrossTenantAndNull(t *testing.T) {
	urls := requireDBURLs(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	applyMigrationsUp(t, urls.migrate)
	truncateAll(t, ctx, urls)
	pool := newAppPool(t, ctx, urls)
	defer pool.Close()
	ids := seedDummyData(t, ctx, pool)

	tcSA := platformdb.TenantContext{TenantID: uuid.Nil, IsSuperAdmin: true}
	ctxSA := platformdb.WithTenantContext(ctx, tcSA)

	t.Run("g-3a: tenant_id=NULL の INSERT は成功", func(t *testing.T) {
		err := platformdb.BeginTxFunc(ctxSA, pool, func(tx pgx.Tx) error {
			_, execErr := tx.Exec(ctx,
				`INSERT INTO audit_logs (id, tenant_id, actor_id, event_type, result)
				 VALUES ($1, NULL, $2, 'system.event.superadmin', 'success')`,
				uuid.New(), ids.adminAID)
			return execErr
		})
		if err != nil {
			t.Fatalf("SuperAdmin の tenant_id=NULL INSERT が失敗: %v", err)
		}
	})

	t.Run("g-3b: tenant_id=B（cross-tenant）の INSERT は成功", func(t *testing.T) {
		err := platformdb.BeginTxFunc(ctxSA, pool, func(tx pgx.Tx) error {
			_, execErr := tx.Exec(ctx,
				`INSERT INTO audit_logs (id, tenant_id, actor_id, event_type, result)
				 VALUES ($1, $2, $3, 'crosstenant.event.superadmin', 'success')`,
				uuid.New(), ids.tenantBID, ids.adminAID)
			return execErr
		})
		if err != nil {
			t.Fatalf("SuperAdmin の cross-tenant INSERT が失敗: %v", err)
		}
	})
}

// TestDBSessions_TenantIsolation_SubselectPolicy はシナリオ (h) 対応。
// sessions は tenant_id カラムを持たないが、admin_users 経由の subselect ポリシー
// （tenant_isolation_sessions）で分離される。
//   - TenantContext=A で tenant B 配下の admin_user_id を持つ sessions に SELECT/UPDATE/DELETE → 0 rows
//   - IsSuperAdmin=true で全 sessions が返る
//
// （requirements.md Req 6.4 / NFR 1.2）
func TestDBSessions_TenantIsolation_SubselectPolicy(t *testing.T) {
	urls := requireDBURLs(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	applyMigrationsUp(t, urls.migrate)
	truncateAll(t, ctx, urls)
	pool := newAppPool(t, ctx, urls)
	defer pool.Close()
	ids := seedDummyData(t, ctx, pool)

	tcA := platformdb.TenantContext{TenantID: ids.tenantAID, IsSuperAdmin: false}
	ctxA := platformdb.WithTenantContext(ctx, tcA)
	tcSA := platformdb.TenantContext{TenantID: uuid.Nil, IsSuperAdmin: true}
	ctxSA := platformdb.WithTenantContext(ctx, tcSA)

	t.Run("tenant A 文脈では tenant B の session が見えない", func(t *testing.T) {
		var visibleB int
		err := platformdb.BeginTxFunc(ctxA, pool, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT COUNT(*) FROM sessions WHERE token_hash = $1`, ids.sessionBTH,
			).Scan(&visibleB)
		})
		if err != nil {
			t.Fatalf("BeginTxFunc: %v", err)
		}
		if visibleB != 0 {
			t.Errorf("tenant B 配下 session の SELECT 件数 = %d; want 0", visibleB)
		}
	})

	t.Run("tenant A 文脈での tenant B session UPDATE / DELETE は 0 rows", func(t *testing.T) {
		err := platformdb.BeginTxFunc(ctxA, pool, func(tx pgx.Tx) error {
			ct, e := tx.Exec(ctx,
				`UPDATE sessions SET last_seen_at = $1 WHERE token_hash = $2`,
				time.Now(), ids.sessionBTH)
			if e != nil {
				return e
			}
			if ct.RowsAffected() != 0 {
				t.Errorf("UPDATE rows affected = %d; want 0", ct.RowsAffected())
			}
			ct, e = tx.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, ids.sessionBTH)
			if e != nil {
				return e
			}
			if ct.RowsAffected() != 0 {
				t.Errorf("DELETE rows affected = %d; want 0", ct.RowsAffected())
			}
			return nil
		})
		if err != nil {
			t.Fatalf("BeginTxFunc: %v", err)
		}
	})

	t.Run("SuperAdmin 文脈で全 sessions が返る", func(t *testing.T) {
		var total int
		err := platformdb.BeginTxFunc(ctxSA, pool, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&total)
		})
		if err != nil {
			t.Fatalf("BeginTxFunc: %v", err)
		}
		if total != 2 {
			t.Errorf("SuperAdmin での sessions 全件 = %d; want 2", total)
		}
	})
}

// assertPgError は err が *pgconn.PgError であり、SQLSTATE が wantState（指定時のみ）と
// 一致し、Message に wantMsgSubstr を含むこと（指定時のみ）を確認する test helper。
func assertPgError(t *testing.T, err error, wantState, wantMsgSubstr string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !stdErrors.As(err, &pgErr) {
		t.Fatalf("err は *pgconn.PgError でない: %T (%v)", err, err)
	}
	if wantState != "" && pgErr.Code != wantState {
		t.Errorf("SQLSTATE = %q; want %q (msg=%q)", pgErr.Code, wantState, pgErr.Message)
	}
	if wantMsgSubstr != "" && !strings.Contains(strings.ToLower(pgErr.Message), strings.ToLower(wantMsgSubstr)) {
		t.Errorf("PgError.Message = %q; want substring %q", pgErr.Message, wantMsgSubstr)
	}
}
