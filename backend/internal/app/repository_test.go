package app

import (
	"context"
	stderrors "errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// fakeRow は rowScanner を満たすテスト用 1 行。Scan へ渡す列値を保持し、注入された scanErr が
// あればそれを返す（実 PostgreSQL に依存せず scanTenantAppRow を単体テストする / policy.fakeRow と同型）。
//
// 列順は scanTenantAppRow の Scan 順（id, tenant_id, package_name, title, icon_url, approved_at）
// に一致させる。
type fakeRow struct {
	id          uuid.UUID
	tenantID    uuid.UUID
	packageName string
	title       string
	iconURL     *string
	approvedAt  time.Time
	scanErr     error
}

func (f *fakeRow) Scan(dest ...any) error {
	if f.scanErr != nil {
		return f.scanErr
	}
	if len(dest) != 6 {
		return fmt.Errorf("fakeRow.Scan: dest 件数 = %d; want 6", len(dest))
	}
	*(dest[0].(*uuid.UUID)) = f.id
	*(dest[1].(*uuid.UUID)) = f.tenantID
	*(dest[2].(*string)) = f.packageName
	*(dest[3].(*string)) = f.title
	*(dest[4].(**string)) = f.iconURL
	*(dest[5].(*time.Time)) = f.approvedAt
	return nil
}

// fakeRows は rowsScanner を満たすテスト用の行イテレータ。列値を `[][]any`（行 × 列）で保持し、
// pgx.Rows に依存せず collectTenantAppRows / collectPackageSet の 0 行 / N 行 / Err 経路を検証する。
type fakeRows struct {
	vals    [][]any
	pos     int
	scanErr error
	iterErr error
	closed  bool
}

func newFakeRows(vals [][]any) *fakeRows {
	return &fakeRows{vals: vals, pos: -1}
}

func (f *fakeRows) Next() bool {
	f.pos++
	return f.pos < len(f.vals)
}

func (f *fakeRows) Scan(dest ...any) error {
	if f.scanErr != nil {
		return f.scanErr
	}
	if f.pos < 0 || f.pos >= len(f.vals) {
		return fmt.Errorf("fakeRows.Scan: pos %d が範囲外", f.pos)
	}
	return assignRow(dest, f.vals[f.pos])
}

func (f *fakeRows) Err() error { return f.iterErr }

func (f *fakeRows) Close() { f.closed = true }

// assignRow は 1 行分の列値（vals）を Scan の dest ポインタ群へ型付きで書き込む。
// scanTenantAppRow（uuid/uuid/string/string/*string/time）と collectPackageSet（string）双方の
// dest 型を型スイッチで扱う。
func assignRow(dest []any, vals []any) error {
	if len(dest) != len(vals) {
		return fmt.Errorf("assignRow: dest 件数 = %d; want %d", len(dest), len(vals))
	}
	for i := range dest {
		switch d := dest[i].(type) {
		case *uuid.UUID:
			v, ok := vals[i].(uuid.UUID)
			if !ok {
				return fmt.Errorf("col %d: want uuid.UUID, got %T", i, vals[i])
			}
			*d = v
		case *string:
			v, ok := vals[i].(string)
			if !ok {
				return fmt.Errorf("col %d: want string, got %T", i, vals[i])
			}
			*d = v
		case **string:
			if vals[i] == nil {
				*d = nil
				continue
			}
			v, ok := vals[i].(*string)
			if !ok {
				return fmt.Errorf("col %d: want *string, got %T", i, vals[i])
			}
			*d = v
		case *time.Time:
			v, ok := vals[i].(time.Time)
			if !ok {
				return fmt.Errorf("col %d: want time.Time, got %T", i, vals[i])
			}
			*d = v
		default:
			return fmt.Errorf("assignRow: 未対応 dest 型 %T", dest[i])
		}
	}
	return nil
}

func strptr(s string) *string { return &s }

// --- scanTenantAppRow（scan 写像 / rowScanner seam） ---

// TestScanTenantAppRow_AllColumns は scanTenantAppRow が全列を型付きで TenantAppRow に束ねること
// （NFR 2.1）を検証する。icon_url 非 NULL は *string に写像される。
func TestScanTenantAppRow_AllColumns(t *testing.T) {
	// Arrange
	id := uuid.New()
	tenantID := uuid.New()
	icon := "https://cdn.example.com/icon.png"
	approvedAt := time.Date(2026, 6, 30, 9, 0, 0, 0, time.UTC)
	row := &fakeRow{
		id:          id,
		tenantID:    tenantID,
		packageName: "com.example.app",
		title:       "Example App",
		iconURL:     &icon,
		approvedAt:  approvedAt,
	}

	// Act
	got, err := scanTenantAppRow(row)

	// Assert
	if err != nil {
		t.Fatalf("scanTenantAppRow: %v", err)
	}
	if got.ID != id || got.TenantID != tenantID {
		t.Errorf("ID/TenantID mismatch: got %v/%v", got.ID, got.TenantID)
	}
	if got.PackageName != "com.example.app" || got.Title != "Example App" {
		t.Errorf("PackageName/Title mismatch: got %q/%q", got.PackageName, got.Title)
	}
	if got.IconURL == nil || *got.IconURL != icon {
		t.Errorf("IconURL = %v; want %q", got.IconURL, icon)
	}
	if !got.ApprovedAt.Equal(approvedAt) {
		t.Errorf("ApprovedAt = %v; want %v", got.ApprovedAt, approvedAt)
	}
}

// TestScanTenantAppRow_NullIconURL は icon_url が NULL（nil ポインタ）の行で TenantAppRow.IconURL
// が nil になることを検証する（境界値 / nullable 列の写像）。
func TestScanTenantAppRow_NullIconURL(t *testing.T) {
	// Arrange
	row := &fakeRow{
		id:          uuid.New(),
		tenantID:    uuid.New(),
		packageName: "com.example.noicon",
		title:       "No Icon",
		iconURL:     nil,
		approvedAt:  time.Now(),
	}

	// Act
	got, err := scanTenantAppRow(row)

	// Assert
	if err != nil {
		t.Fatalf("scanTenantAppRow: %v", err)
	}
	if got.IconURL != nil {
		t.Errorf("IconURL = %v; want nil", got.IconURL)
	}
}

// TestScanTenantAppRow_ScanError は Scan が error を返した場合、そのまま伝播される（上位の
// collectTenantAppRows が CodeUnavailable 写像を担う）ことを検証する（異常系）。
func TestScanTenantAppRow_ScanError(t *testing.T) {
	// Arrange
	sentinel := fmt.Errorf("simulated scan failure")
	row := &fakeRow{scanErr: sentinel}

	// Act
	_, err := scanTenantAppRow(row)

	// Assert
	if !stderrors.Is(err, sentinel) {
		t.Fatalf("scanTenantAppRow は scan error をそのまま返すべき: got %v", err)
	}
}

// --- collectTenantAppRows（List の行集約 / rowsScanner seam） ---

// tenantAppValsRow は TenantAppRow の Scan 順に沿った 1 行分の列値を組み立てるテストヘルパー。
func tenantAppValsRow(id, tenantID uuid.UUID, pkg, title string, icon *string, approvedAt time.Time) []any {
	return []any{id, tenantID, pkg, title, icon, approvedAt}
}

// TestCollectTenantAppRows_EmptyReturnsNonNilSlice は 0 行のとき非 nil の空 slice を返すこと
// （Req 2.2）を検証する。
func TestCollectTenantAppRows_EmptyReturnsNonNilSlice(t *testing.T) {
	// Arrange
	rows := newFakeRows(nil)

	// Act
	got, err := collectTenantAppRows(rows)

	// Assert
	if err != nil {
		t.Fatalf("collectTenantAppRows: %v", err)
	}
	if got == nil {
		t.Fatal("0 行のとき非 nil 空 slice を返すべき（Req 2.2）: got nil")
	}
	if len(got) != 0 {
		t.Errorf("len = %d; want 0", len(got))
	}
	if !rows.closed {
		t.Error("collectTenantAppRows は rows を Close すべき")
	}
}

// TestCollectTenantAppRows_MultipleOrdered は N 行を scan 写像し、SQL の順序を保持して返すこと
// を検証する（Req 2.1）。
func TestCollectTenantAppRows_MultipleOrdered(t *testing.T) {
	// Arrange
	tenantID := uuid.New()
	id1, id2 := uuid.New(), uuid.New()
	t1 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)
	rows := newFakeRows([][]any{
		tenantAppValsRow(id1, tenantID, "com.a", "A", strptr("https://icon/a"), t1),
		tenantAppValsRow(id2, tenantID, "com.b", "B", nil, t2),
	})

	// Act
	got, err := collectTenantAppRows(rows)

	// Assert
	if err != nil {
		t.Fatalf("collectTenantAppRows: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d; want 2", len(got))
	}
	if got[0].PackageName != "com.a" || got[1].PackageName != "com.b" {
		t.Errorf("順序が保持されていない: got %q, %q", got[0].PackageName, got[1].PackageName)
	}
	if got[0].IconURL == nil || *got[0].IconURL != "https://icon/a" {
		t.Errorf("row0 IconURL = %v; want https://icon/a", got[0].IconURL)
	}
	if got[1].IconURL != nil {
		t.Errorf("row1 IconURL = %v; want nil（NULL 境界）", got[1].IconURL)
	}
}

// TestCollectTenantAppRows_ScanError は行の Scan が失敗した場合 CodeUnavailable（503）へ wrap
// されることを検証する（異常系）。
func TestCollectTenantAppRows_ScanError(t *testing.T) {
	// Arrange
	rows := newFakeRows([][]any{{uuid.New()}}) // 1 行あるが scanErr を注入
	rows.scanErr = fmt.Errorf("scan boom")

	// Act
	_, err := collectTenantAppRows(rows)

	// Assert
	assertCodeUnavailable(t, err)
}

// TestCollectTenantAppRows_IterationError は rows.Err() が非 nil のとき CodeUnavailable（503）へ
// wrap されることを検証する（異常系 / DB error→CodeUnavailable）。
func TestCollectTenantAppRows_IterationError(t *testing.T) {
	// Arrange
	rows := newFakeRows(nil)
	rows.iterErr = fmt.Errorf("connection reset")

	// Act
	_, err := collectTenantAppRows(rows)

	// Assert
	assertCodeUnavailable(t, err)
	if !rows.closed {
		t.Error("エラー経路でも rows を Close すべき")
	}
}

// --- collectPackageSet（ApprovedPackages の集合集約 / Req 5.1） ---

// TestCollectPackageSet_EmptyReturnsNonNilMap は 0 行のとき非 nil の空 map を返すことを検証する。
func TestCollectPackageSet_EmptyReturnsNonNilMap(t *testing.T) {
	// Arrange
	rows := newFakeRows(nil)

	// Act
	got, err := collectPackageSet(rows)

	// Assert
	if err != nil {
		t.Fatalf("collectPackageSet: %v", err)
	}
	if got == nil {
		t.Fatal("0 行のとき非 nil 空 map を返すべき: got nil")
	}
	if len(got) != 0 {
		t.Errorf("len = %d; want 0", len(got))
	}
}

// TestCollectPackageSet_Multiple は複数行の package_name を集合へ写像すること（Req 5.1）を検証する。
func TestCollectPackageSet_Multiple(t *testing.T) {
	// Arrange
	rows := newFakeRows([][]any{{"com.a"}, {"com.b"}})

	// Act
	got, err := collectPackageSet(rows)

	// Assert
	if err != nil {
		t.Fatalf("collectPackageSet: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d; want 2", len(got))
	}
	if _, ok := got["com.a"]; !ok {
		t.Error("com.a が集合に含まれるべき")
	}
	if _, ok := got["com.b"]; !ok {
		t.Error("com.b が集合に含まれるべき")
	}
}

// TestCollectPackageSet_IterationError は rows.Err() が非 nil のとき CodeUnavailable へ wrap される
// ことを検証する（異常系 / DB error→CodeUnavailable）。
func TestCollectPackageSet_IterationError(t *testing.T) {
	// Arrange
	rows := newFakeRows(nil)
	rows.iterErr = fmt.Errorf("iteration boom")

	// Act
	_, err := collectPackageSet(rows)

	// Assert
	assertCodeUnavailable(t, err)
}

// --- SQL 述語の静的検証（tenant_id / ON CONFLICT / 昇格しない） ---

// TestListSQL_TenantPredicate は List SQL が tenant_id 述語を含む（RLS との二重防御 / Req 2.3）
// ことを検証する。
func TestListSQL_TenantPredicate(t *testing.T) {
	if !strings.Contains(listTenantAppsSQL, "tenant_id") {
		t.Errorf("listTenantAppsSQL は tenant_id 述語を含むべき（Req 2.3）: %q", listTenantAppsSQL)
	}
	if !strings.Contains(listTenantAppsSQL, "ORDER BY approved_at") {
		t.Errorf("listTenantAppsSQL は approved_at 昇順を含むべき（Req 2.1）: %q", listTenantAppsSQL)
	}
}

// TestUpsertSQL_OnConflictDoUpdate は Upsert SQL が ON CONFLICT ... DO UPDATE と tenant_id を
// 含む（重複 package を更新し重複作成しない / Req 3.2）ことを検証する。
func TestUpsertSQL_OnConflictDoUpdate(t *testing.T) {
	if !strings.Contains(upsertTenantAppSQL, "ON CONFLICT") {
		t.Errorf("upsertTenantAppSQL は ON CONFLICT を含むべき（Req 3.2）: %q", upsertTenantAppSQL)
	}
	if !strings.Contains(upsertTenantAppSQL, "DO UPDATE") {
		t.Errorf("upsertTenantAppSQL は DO UPDATE を含むべき（Req 3.2）: %q", upsertTenantAppSQL)
	}
	if !strings.Contains(upsertTenantAppSQL, "tenant_id") {
		t.Errorf("upsertTenantAppSQL は tenant_id を含むべき（Req 2.3）: %q", upsertTenantAppSQL)
	}
	if !strings.Contains(upsertTenantAppSQL, "(tenant_id, package_name)") {
		t.Errorf("upsertTenantAppSQL は UNIQUE(tenant_id, package_name) を conflict target にすべき: %q", upsertTenantAppSQL)
	}
}

// TestApprovedPackagesSQL_TenantPredicate は ApprovedPackages SQL が tenant_id 述語を含む（Req 5.1）
// ことを検証する。
func TestApprovedPackagesSQL_TenantPredicate(t *testing.T) {
	if !strings.Contains(approvedPackagesSQL, "tenant_id") {
		t.Errorf("approvedPackagesSQL は tenant_id 述語を含むべき（Req 5.1）: %q", approvedPackagesSQL)
	}
	if !strings.Contains(approvedPackagesSQL, "ANY($2)") {
		t.Errorf("approvedPackagesSQL は package_name = ANY($2) を含むべき（Req 5.1）: %q", approvedPackagesSQL)
	}
}

// TestAllSQL_NoPrivilegeEscalation は全 SQL が SuperAdmin 昇格 / RLS バイパス（SET ROLE /
// set_config / bypass_rls / superadmin）を含まないこと（tenant-scoped のまま RLS に分離を委ねる /
// Req 2.3 の「昇格しない」）を検証する。
func TestAllSQL_NoPrivilegeEscalation(t *testing.T) {
	forbidden := []string{"set role", "set_config", "bypass_rls", "superadmin", "is_superadmin"}
	sqls := map[string]string{
		"listTenantAppsSQL":   listTenantAppsSQL,
		"upsertTenantAppSQL":  upsertTenantAppSQL,
		"approvedPackagesSQL": approvedPackagesSQL,
	}
	for name, sql := range sqls {
		lower := strings.ToLower(sql)
		for _, bad := range forbidden {
			if strings.Contains(lower, bad) {
				t.Errorf("%s は昇格/RLS バイパス語 %q を含んではならない（Req 2.3）: %q", name, bad, sql)
			}
		}
	}
}

// --- 空入力の早期 return（pool へ触れない） ---

// TestUpsert_EmptyList_ReturnsZeroWithoutPool は Upsert(nil) / Upsert([]) が pool へ触れず
// (0, nil) を返すこと（Req 3.4）を検証する。nil pool を渡し、BeginTxFunc を経由すると
// CodeInternal エラーになる（=早期 return していない）ことで早期 return を確認する。
func TestUpsert_EmptyList_ReturnsZeroWithoutPool(t *testing.T) {
	repo := NewRepository(nil) // nil pool: 早期 return しなければ BeginTxFunc が CodeInternal を返す
	ctx := context.Background()
	tenantID := uuid.New()

	cases := map[string][]SyncApp{
		"nil スライス": nil,
		"空スライス":   {},
	}
	for name, apps := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			count, err := repo.Upsert(ctx, tenantID, apps)

			// Assert
			if err != nil {
				t.Fatalf("空リストは (0, nil) を返すべき（pool へ触れない / Req 3.4）: err=%v", err)
			}
			if count != 0 {
				t.Errorf("count = %d; want 0", count)
			}
		})
	}
}

// TestApprovedPackages_EmptyInput_ReturnsEmptyMapWithoutPool は ApprovedPackages([]) が pool へ
// 触れず非 nil 空 map を返すことを検証する（nil pool で早期 return を確認）。
func TestApprovedPackages_EmptyInput_ReturnsEmptyMapWithoutPool(t *testing.T) {
	repo := NewRepository(nil)
	ctx := context.Background()
	tenantID := uuid.New()

	cases := map[string][]string{
		"nil スライス": nil,
		"空スライス":   {},
	}
	for name, names := range cases {
		t.Run(name, func(t *testing.T) {
			// Act
			got, err := repo.ApprovedPackages(ctx, tenantID, names)

			// Assert
			if err != nil {
				t.Fatalf("空入力は非 nil 空 map を返すべき（pool へ触れない）: err=%v", err)
			}
			if got == nil {
				t.Fatal("非 nil 空 map を返すべき: got nil")
			}
			if len(got) != 0 {
				t.Errorf("len = %d; want 0", len(got))
			}
		})
	}
}

// assertCodeUnavailable は err が *pkgerrors.Error かつ Code==CodeUnavailable であることを表明する。
func assertCodeUnavailable(t *testing.T, err error) {
	t.Helper()
	var de *pkgerrors.Error
	if !stderrors.As(err, &de) || de.Code != pkgerrors.CodeUnavailable {
		t.Fatalf("err = %v; want *pkgerrors.Error{Code: CodeUnavailable}", err)
	}
}

// compile-time check: *fakeRow は rowScanner を、*fakeRows は rowsScanner を満たす。
var (
	_ rowScanner  = (*fakeRow)(nil)
	_ rowsScanner = (*fakeRows)(nil)
)
