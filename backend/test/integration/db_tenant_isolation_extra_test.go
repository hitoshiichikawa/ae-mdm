package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	platformdb "github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
)

// TestDBAuditLogs_SelectTenantIsolation_NormalAndSuperAdmin は audit_logs テーブルの
// SELECT 経路の分離挙動を直接検証する（PR #31 round-3 review 由来 / Req 7.3）。
//
// シナリオ:
//   - tenant A 文脈で SELECT すると tenant A の audit_logs のみ返り、tenant B の行は 0 件
//   - SuperAdmin 文脈では tenant A / B 両方の audit_logs が返る
//
// audit_logs は 0012 で `audit_logs_select` ポリシーが個別定義されており、0011 の汎用
// tenant_isolation_<table> とは別経路。SELECT 分離は既存テスト
// （`TestDBAuditLogs_AppUserUpdateDelete_Rejected` / `TestDBAuditLogs_InsertWithCheck_*`）では
// 直接検証されていなかったため、本テストで明示的にカバーする。
func TestDBAuditLogs_SelectTenantIsolation_NormalAndSuperAdmin(t *testing.T) {
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

	t.Run("tenant A 文脈では tenant B の audit_logs 行が見えない", func(t *testing.T) {
		var visibleB int
		err := platformdb.BeginTxFunc(ctxA, pool, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT COUNT(*) FROM audit_logs WHERE id = $1`, ids.auditBID,
			).Scan(&visibleB)
		})
		if err != nil {
			t.Fatalf("BeginTxFunc: %v", err)
		}
		if visibleB != 0 {
			t.Errorf("tenant A 文脈での tenant B audit_logs 件数 = %d; want 0", visibleB)
		}
	})

	t.Run("tenant A 文脈では tenant A の audit_logs 行のみが見える", func(t *testing.T) {
		var visibleA int
		err := platformdb.BeginTxFunc(ctxA, pool, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT COUNT(*) FROM audit_logs`).Scan(&visibleA)
		})
		if err != nil {
			t.Fatalf("BeginTxFunc: %v", err)
		}
		if visibleA != 1 {
			t.Errorf("tenant A 文脈での audit_logs 全行 = %d; want 1（A のみ）", visibleA)
		}
	})

	t.Run("SuperAdmin 文脈では tenant A / B 両方の audit_logs が返る", func(t *testing.T) {
		var total int
		err := platformdb.BeginTxFunc(ctxSA, pool, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT COUNT(*) FROM audit_logs`).Scan(&total)
		})
		if err != nil {
			t.Fatalf("BeginTxFunc: %v", err)
		}
		if total != 2 {
			t.Errorf("SuperAdmin での audit_logs 全行 = %d; want 2（A + B）", total)
		}
	})
}

// extraSeededIDs は seedExtraIsolationData が投入する追加テーブル行の ID 群。
type extraSeededIDs struct {
	enrollAID     uuid.UUID
	enrollBID     uuid.UUID
	deviceCmdAID  uuid.UUID
	deviceCmdBID  uuid.UUID
	tenantAppAID  uuid.UUID
	tenantAppBID  uuid.UUID
	roleAssignAID uuid.UUID
	roleAssignBID uuid.UUID
}

// seedExtraIsolationData は admin_role_assignments / enrollment_tokens /
// device_commands / tenant_apps の 4 テーブルへ tenant A / B 各 1 行ずつ追加投入する
// （PR #31 round-3 review 由来）。seedDummyData が既に投入した tenants / admin_users /
// devices / policies を前提とする。
//
// SuperAdmin GUC（is_superadmin=true）で RLS を一括バイパスして投入する。
func seedExtraIsolationData(t *testing.T, ctx context.Context, pool *pgxpool.Pool, base seededIDs) extraSeededIDs {
	t.Helper()
	extra := extraSeededIDs{
		enrollAID:     uuid.New(),
		enrollBID:     uuid.New(),
		deviceCmdAID:  uuid.New(),
		deviceCmdBID:  uuid.New(),
		tenantAppAID:  uuid.New(),
		tenantAppBID:  uuid.New(),
		roleAssignAID: uuid.New(),
		roleAssignBID: uuid.New(),
	}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.is_superadmin', 'true', true)"); err != nil {
		t.Fatalf("set_config app.is_superadmin: %v", err)
	}

	// admin_role_assignments (TenantAdmin role for both tenants)
	for _, p := range []struct {
		id       uuid.UUID
		adminID  uuid.UUID
		tenantID uuid.UUID
	}{
		{extra.roleAssignAID, base.adminAID, base.tenantAID},
		{extra.roleAssignBID, base.adminBID, base.tenantBID},
	} {
		if _, err := tx.Exec(ctx,
			`INSERT INTO admin_role_assignments (id, admin_user_id, role, tenant_id)
			 VALUES ($1, $2, 'TenantAdmin', $3)`,
			p.id, p.adminID, p.tenantID); err != nil {
			t.Fatalf("INSERT admin_role_assignments: %v", err)
		}
	}

	// enrollment_tokens
	expiry := time.Now().Add(1 * time.Hour)
	for _, p := range []struct {
		id        uuid.UUID
		tenantID  uuid.UUID
		amapiName string
		issuedBy  uuid.UUID
	}{
		{extra.enrollAID, base.tenantAID, "enterprises/X/enrollmentTokens/A1", base.adminAID},
		{extra.enrollBID, base.tenantBID, "enterprises/X/enrollmentTokens/B1", base.adminBID},
	} {
		if _, err := tx.Exec(ctx,
			`INSERT INTO enrollment_tokens (id, tenant_id, amapi_token_name, mode, expires_at, issued_by)
			 VALUES ($1, $2, $3, 'fully_managed', $4, $5)`,
			p.id, p.tenantID, p.amapiName, expiry, p.issuedBy); err != nil {
			t.Fatalf("INSERT enrollment_tokens: %v", err)
		}
	}

	// device_commands (uses composite FK (device_id, tenant_id))
	for _, p := range []struct {
		id       uuid.UUID
		tenantID uuid.UUID
		deviceID uuid.UUID
		issuedBy uuid.UUID
	}{
		{extra.deviceCmdAID, base.tenantAID, base.deviceAID, base.adminAID},
		{extra.deviceCmdBID, base.tenantBID, base.deviceBID, base.adminBID},
	} {
		if _, err := tx.Exec(ctx,
			`INSERT INTO device_commands (id, tenant_id, device_id, type, issued_by)
			 VALUES ($1, $2, $3, 'lock', $4)`,
			p.id, p.tenantID, p.deviceID, p.issuedBy); err != nil {
			t.Fatalf("INSERT device_commands: %v", err)
		}
	}

	// tenant_apps
	for _, p := range []struct {
		id          uuid.UUID
		tenantID    uuid.UUID
		packageName string
	}{
		{extra.tenantAppAID, base.tenantAID, "com.example.app.a"},
		{extra.tenantAppBID, base.tenantBID, "com.example.app.b"},
	} {
		if _, err := tx.Exec(ctx,
			`INSERT INTO tenant_apps (id, tenant_id, package_name, title)
			 VALUES ($1, $2, $3, $4)`,
			p.id, p.tenantID, p.packageName, fmt.Sprintf("App %s", p.tenantID.String()[:8])); err != nil {
			t.Fatalf("INSERT tenant_apps: %v", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("seedExtraIsolationData commit: %v", err)
	}
	return extra
}

// TestDBTenantIsolation_AllTenantTables_NormalContextOnlySelfRows は、
// tenant_id 列を持つ全テーブルについて、TenantA 文脈では TenantA の行のみ可視で
// TenantB の行は SELECT / UPDATE / DELETE 経路で 0 件であることを横断的に検証する
// （PR #31 round-3 review 由来 / NFR 1.2「テナント A のセッションが tenant B 行を
// 一切操作できない」の網羅性補強）。
//
// 既存テスト（devices / sessions / audit_logs）でカバーされていなかった残りテーブル
// （admin_users / admin_role_assignments / enrollment_tokens / policies / device_commands /
// tenant_apps）を対象に、SELECT / UPDATE / DELETE 3 経路をまとめて検証する。
func TestDBTenantIsolation_AllTenantTables_NormalContextOnlySelfRows(t *testing.T) {
	urls := requireDBURLs(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	applyMigrationsUp(t, urls.migrate)
	truncateAll(t, ctx, urls)
	pool := newAppPool(t, ctx, urls)
	defer pool.Close()
	base := seedDummyData(t, ctx, pool)
	extra := seedExtraIsolationData(t, ctx, pool, base)

	tcA := platformdb.TenantContext{TenantID: base.tenantAID, IsSuperAdmin: false}
	ctxA := platformdb.WithTenantContext(ctx, tcA)

	// 各テーブル × tenant B 行の (SELECT 0 / UPDATE 0 / DELETE 0) 共通アサート。
	cases := []struct {
		name      string
		table     string
		idColumn  string
		idValue   any
		updateSQL string
	}{
		{
			name:      "admin_users",
			table:     "admin_users",
			idColumn:  "id",
			idValue:   base.adminBID,
			updateSQL: `UPDATE admin_users SET email = 'tampered@example.com' WHERE id = $1`,
		},
		{
			name:      "admin_role_assignments",
			table:     "admin_role_assignments",
			idColumn:  "id",
			idValue:   extra.roleAssignBID,
			updateSQL: `UPDATE admin_role_assignments SET role = 'Viewer' WHERE id = $1`,
		},
		{
			name:      "enrollment_tokens",
			table:     "enrollment_tokens",
			idColumn:  "id",
			idValue:   extra.enrollBID,
			updateSQL: `UPDATE enrollment_tokens SET additional_data = '{"tampered":true}'::jsonb WHERE id = $1`,
		},
		{
			name:      "policies",
			table:     "policies",
			idColumn:  "id",
			idValue:   base.policyBID,
			updateSQL: `UPDATE policies SET name = 'tampered' WHERE id = $1`,
		},
		{
			name:      "device_commands",
			table:     "device_commands",
			idColumn:  "id",
			idValue:   extra.deviceCmdBID,
			updateSQL: `UPDATE device_commands SET status = 'failed' WHERE id = $1`,
		},
		{
			name:      "tenant_apps",
			table:     "tenant_apps",
			idColumn:  "id",
			idValue:   extra.tenantAppBID,
			updateSQL: `UPDATE tenant_apps SET title = 'tampered' WHERE id = $1`,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			// SELECT: tenant A 文脈では tenant B の行が 0 件
			var visible int
			err := platformdb.BeginTxFunc(ctxA, pool, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx,
					fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE %s = $1`, c.table, c.idColumn),
					c.idValue).Scan(&visible)
			})
			if err != nil {
				t.Fatalf("SELECT %s: BeginTxFunc: %v", c.table, err)
			}
			if visible != 0 {
				t.Errorf("%s: tenant A 文脈で tenant B 行の SELECT 件数 = %d; want 0", c.table, visible)
			}

			// UPDATE: tenant A 文脈での tenant B 行 UPDATE は 0 rows affected
			err = platformdb.BeginTxFunc(ctxA, pool, func(tx pgx.Tx) error {
				ct, e := tx.Exec(ctx, c.updateSQL, c.idValue)
				if e != nil {
					return e
				}
				if ct.RowsAffected() != 0 {
					t.Errorf("%s: UPDATE rows affected = %d; want 0", c.table, ct.RowsAffected())
				}
				return nil
			})
			if err != nil {
				t.Fatalf("UPDATE %s: BeginTxFunc: %v", c.table, err)
			}

			// DELETE: tenant A 文脈での tenant B 行 DELETE は 0 rows affected
			err = platformdb.BeginTxFunc(ctxA, pool, func(tx pgx.Tx) error {
				ct, e := tx.Exec(ctx,
					fmt.Sprintf(`DELETE FROM %s WHERE %s = $1`, c.table, c.idColumn),
					c.idValue)
				if e != nil {
					return e
				}
				if ct.RowsAffected() != 0 {
					t.Errorf("%s: DELETE rows affected = %d; want 0", c.table, ct.RowsAffected())
				}
				return nil
			})
			if err != nil {
				t.Fatalf("DELETE %s: BeginTxFunc: %v", c.table, err)
			}
		})
	}
}
