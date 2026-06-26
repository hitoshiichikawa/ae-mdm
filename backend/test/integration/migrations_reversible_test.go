package integration_test

import (
	"context"
	stdErrors "errors"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/jackc/pgx/v5"
)

// migration が作成する主要テーブル群。down 後に「これらが消失」していることを確認する。
// 全 12 ペア（0001-0012）が作る 12 テーブル + Issue #33 (A3a) で追加された
// state_nonces（0015）+ 関連する enum / RLS policy のうち、テーブル存在で代表させる。
// 0013（sessions 拡張）/ 0014（admin_users 拡張）は既存テーブルへの列追加のため
// 本リストの対象外（テーブル存在のみで代表できないため、reversibility は migration 適用
// 自体の成功で担保する）。
var primaryTables = []string{
	"tenants",
	"admin_users",
	"admin_role_assignments",
	"sessions",
	"enrollment_tokens",
	"policies",
	"devices",
	"device_commands",
	"tenant_apps",
	"audit_logs",
	"notification_dedupe",
	"unassigned_notifications",
	"state_nonces",
}

// TestMigrationsReversible_DownDropsTablesUpRecreates はシナリオ (a) (b) 対応。
//
//   - (a) migrate-down 後に主要テーブル群が public schema から消失している
//   - (b) 再度 migrate-up を適用すると同じ最終状態（全テーブル復活）に到達する
//
// （requirements.md NFR 2.2）
//
// 注意: 他テストと同じ DB を共有するため、本テスト終了時点では migrate-up 状態に
// 戻して終了する（後続テスト実行や他テスト同時実行で schema 不在を引き起こさない）。
func TestMigrationsReversible_DownDropsTablesUpRecreates(t *testing.T) {
	urls := requireDBURLs(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()

	// 事前に必ず up しておく（他テストの後に走るとは限らない）
	applyMigrationsUp(t, urls.migrate)

	// テスト終了時は必ず up に戻す（他テストへの副作用回避）
	defer applyMigrationsUp(t, urls.migrate)

	// Act: down で全テーブル巻き戻し
	applyMigrationsDown(t, urls.migrate)

	// Assert (a): 主要テーブルが消失
	for _, tbl := range primaryTables {
		if exists, err := tableExists(ctx, urls.migrate, tbl); err != nil {
			t.Fatalf("tableExists(%q): %v", tbl, err)
		} else if exists {
			t.Errorf("table %q が down 後も残存している", tbl)
		}
	}

	// Act: 再度 up
	applyMigrationsUp(t, urls.migrate)

	// Assert (b): 全テーブル復活
	for _, tbl := range primaryTables {
		if exists, err := tableExists(ctx, urls.migrate, tbl); err != nil {
			t.Fatalf("tableExists(%q): %v", tbl, err)
		} else if !exists {
			t.Errorf("table %q が再 up 後に復活していない", tbl)
		}
	}
}

// TestMigrationsReversible_RepeatedUpIsNoop はシナリオ (c) 対応。
// 2 回目の up が冪等で no-op（migrate.ErrNoChange）として終わることを確認する
// （requirements.md NFR 2.1）。
//
// golang-migrate は 2 回目の Up() で全 migration が既適用なら ErrNoChange を返す。
func TestMigrationsReversible_RepeatedUpIsNoop(t *testing.T) {
	urls := requireDBURLs(t)
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	_ = ctx

	// Arrange: まず up（既適用なら no-op）
	applyMigrationsUp(t, urls.migrate)

	// Act: もう一度 Up() を呼び、ErrNoChange が返ることを直接検証する。
	// applyMigrationsUp helper は ErrNoChange を成功扱いするため、本テストでは
	// 直接 m.Up() を呼んで戻り値を確認する。
	m := migrateInstance(t, urls.migrate)
	defer func() { _, _ = m.Close() }()
	upErr := m.Up()

	// Assert: ErrNoChange であること（=「適用済み」を意味する idempotent 状態）
	if upErr == nil {
		t.Fatalf("2 回目の Up() が nil; want migrate.ErrNoChange（idempotent 検証）")
	}
	if !stdErrors.Is(upErr, migrate.ErrNoChange) {
		t.Errorf("2 回目の Up() = %v; want migrate.ErrNoChange", upErr)
	}
}

// tableExists は migration_user 接続で `pg_tables` を読み、指定テーブルの存在を返す。
// public schema 限定で照会する。
func tableExists(ctx context.Context, migrateURL, table string) (bool, error) {
	conn, err := pgx.Connect(ctx, migrateURL)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close(ctx) }()

	var exists bool
	err = conn.QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM pg_tables
			WHERE schemaname = 'public' AND tablename = $1
		)`, table,
	).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}
