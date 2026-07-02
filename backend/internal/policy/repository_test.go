package policy

import (
	stderrors "errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// fakeRow は rowScanner を満たすテスト用 1 行。Scan へ渡す列値を保持し、注入された
// scanErr があればそれを返す（実 PostgreSQL に依存せず scanPolicyRow を単体テストする）。
//
// 列順は scanPolicyRow の Scan 順（id, tenant_id, name, amapi_policy_name, body,
// version, updated_by, created_at, updated_at）に一致させる。
type fakeRow struct {
	id        uuid.UUID
	tenantID  uuid.UUID
	name      string
	amapiName string
	body      map[string]any
	version   int64
	updatedBy *uuid.UUID
	createdAt time.Time
	updatedAt time.Time
	scanErr   error
}

func (f *fakeRow) Scan(dest ...any) error {
	if f.scanErr != nil {
		return f.scanErr
	}
	if len(dest) != 9 {
		return fmt.Errorf("fakeRow.Scan: dest 件数 = %d; want 9", len(dest))
	}
	*(dest[0].(*uuid.UUID)) = f.id
	*(dest[1].(*uuid.UUID)) = f.tenantID
	*(dest[2].(*string)) = f.name
	*(dest[3].(*string)) = f.amapiName
	*(dest[4].(*map[string]any)) = f.body
	*(dest[5].(*int64)) = f.version
	*(dest[6].(**uuid.UUID)) = f.updatedBy
	*(dest[7].(*time.Time)) = f.createdAt
	*(dest[8].(*time.Time)) = f.updatedAt
	return nil
}

// fkViolation は applied_policy_id 複合 FK / devices.applied_policy_id 参照違反を模した
// pgconn.PgError（pgerrcode 23503）。fake が実 pgx の FK violation 検出経路を再現する。
func fkViolation() error {
	return &pgconn.PgError{Code: pgerrcode.ForeignKeyViolation, Message: "fk violation"}
}

// TestScanPolicyRow_AllColumns は scanPolicyRow が全列を型付きで PolicyRow に束ねること
// （NFR 2.1）を検証する。body jsonb は map[string]any に、updated_by 非 NULL は *uuid.UUID に
// 写像される。
func TestScanPolicyRow_AllColumns(t *testing.T) {
	// Arrange
	id := uuid.New()
	tenantID := uuid.New()
	updater := uuid.New()
	now := time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC)
	row := &fakeRow{
		id:        id,
		tenantID:  tenantID,
		name:      "Kiosk Policy",
		amapiName: "enterprises/e1/policies/" + id.String(),
		body:      map[string]any{"applications": []any{}},
		version:   3,
		updatedBy: &updater,
		createdAt: now,
		updatedAt: now,
	}

	// Act
	got, err := scanPolicyRow(row)

	// Assert
	if err != nil {
		t.Fatalf("scanPolicyRow: %v", err)
	}
	if got.ID != id || got.TenantID != tenantID {
		t.Errorf("ID/TenantID mismatch: got %v/%v", got.ID, got.TenantID)
	}
	if got.Name != "Kiosk Policy" || got.AMAPIPolicyName != row.amapiName {
		t.Errorf("Name/AMAPIPolicyName mismatch: got %q/%q", got.Name, got.AMAPIPolicyName)
	}
	if got.Version != 3 {
		t.Errorf("Version = %d; want 3", got.Version)
	}
	if got.UpdatedBy == nil || *got.UpdatedBy != updater {
		t.Errorf("UpdatedBy = %v; want %v", got.UpdatedBy, updater)
	}
	if _, ok := got.Body["applications"]; !ok {
		t.Errorf("Body は applications キーを保持すべき: got %v", got.Body)
	}
}

// TestScanPolicyRow_NullUpdatedBy は updated_by が NULL（nil ポインタ）の行で
// PolicyRow.UpdatedBy が nil になることを検証する（境界値 / impl-notes task1 learning）。
func TestScanPolicyRow_NullUpdatedBy(t *testing.T) {
	// Arrange
	row := &fakeRow{
		id:        uuid.New(),
		tenantID:  uuid.New(),
		name:      "no updater",
		amapiName: "enterprises/e1/policies/x",
		body:      map[string]any{},
		version:   1,
		updatedBy: nil,
		createdAt: time.Now(),
		updatedAt: time.Now(),
	}

	// Act
	got, err := scanPolicyRow(row)

	// Assert
	if err != nil {
		t.Fatalf("scanPolicyRow: %v", err)
	}
	if got.UpdatedBy != nil {
		t.Errorf("UpdatedBy = %v; want nil", got.UpdatedBy)
	}
}

// TestScanPolicyRow_ScanError は Scan が error を返した場合、そのまま伝播される
// （上位 Get/List が NotFound / Unavailable 写像を担う）ことを検証する（異常系）。
func TestScanPolicyRow_ScanError(t *testing.T) {
	// Arrange
	sentinel := fmt.Errorf("simulated scan failure")
	row := &fakeRow{scanErr: sentinel}

	// Act
	_, err := scanPolicyRow(row)

	// Assert
	if !stderrors.Is(err, sentinel) {
		t.Fatalf("scanPolicyRow は scan error をそのまま返すべき: got %v", err)
	}
}

// TestMapGetError_NoRows は 0 行（pgx.ErrNoRows）が存在差非露出の ErrPolicyNotFound
// （CodeNotFound / 404）に写像されることを検証する（Req 4.4 / 4.5）。
func TestMapGetError_NoRows(t *testing.T) {
	// Arrange / Act
	err := mapGetError(pgx.ErrNoRows)

	// Assert
	var de *pkgerrors.Error
	if !stderrors.As(err, &de) || de.Code != pkgerrors.CodeNotFound {
		t.Fatalf("err = %v; want CodeNotFound", err)
	}
	if !stderrors.Is(err, ErrPolicyNotFound) {
		t.Errorf("err は ErrPolicyNotFound と Is 一致すべき: got %v", err)
	}
	// 存在差非露出: message に policy id 等の識別子を補間しない汎用文言であること。
	if de.Message != "policy not found" {
		t.Errorf("message = %q; want 汎用 'policy not found'", de.Message)
	}
}

// TestMapGetError_OtherDBError は 0 行以外の DB エラーが CodeUnavailable（503）に
// 写像されることを検証する（異常系 / fail-closed）。
func TestMapGetError_OtherDBError(t *testing.T) {
	// Arrange
	sentinel := fmt.Errorf("connection refused")

	// Act
	err := mapGetError(sentinel)

	// Assert
	var de *pkgerrors.Error
	if !stderrors.As(err, &de) || de.Code != pkgerrors.CodeUnavailable {
		t.Fatalf("err = %v; want CodeUnavailable", err)
	}
	if !stderrors.Is(err, sentinel) {
		t.Errorf("Cause が sentinel を保持していない: %v", err)
	}
}

// TestMapDeleteError_FKViolation は割当済み端末ありの DELETE（FK 違反 23503）が
// ErrDeleteConflict（CodeConflict / 409）に写像されることを検証する
// （design 確認事項 3 推奨案 / Req 5.3）。
func TestMapDeleteError_FKViolation(t *testing.T) {
	// Arrange / Act
	err := mapDeleteError(fkViolation())

	// Assert
	var de *pkgerrors.Error
	if !stderrors.As(err, &de) || de.Code != pkgerrors.CodeConflict {
		t.Fatalf("err = %v; want CodeConflict", err)
	}
	if !stderrors.Is(err, ErrDeleteConflict) {
		t.Errorf("err は ErrDeleteConflict と Is 一致すべき: got %v", err)
	}
}

// TestMapDeleteError_OtherDBError は FK 違反以外の DELETE エラーが CodeUnavailable に
// 写像されることを検証する（異常系）。
func TestMapDeleteError_OtherDBError(t *testing.T) {
	// Arrange
	sentinel := fmt.Errorf("disk full")

	// Act
	err := mapDeleteError(sentinel)

	// Assert
	var de *pkgerrors.Error
	if !stderrors.As(err, &de) || de.Code != pkgerrors.CodeUnavailable {
		t.Fatalf("err = %v; want CodeUnavailable", err)
	}
	if !stderrors.Is(err, sentinel) {
		t.Errorf("Cause が sentinel を保持していない: %v", err)
	}
}

// TestMapDeleteError_Nil は成功（err == nil）が nil を返すことを検証する（境界値）。
func TestMapDeleteError_Nil(t *testing.T) {
	if got := mapDeleteError(nil); got != nil {
		t.Fatalf("mapDeleteError(nil) = %v; want nil", got)
	}
}

// TestMapAssignError_FKViolation は他テナント policy を applied_policy_id に指定した複合 FK
// 違反（23503）が ErrPolicyNotFound（CodeNotFound / 404）に写像されることを検証する。
// 存在差非露出で「未検出」として拒否する（Req 3.2 / 4.2 / 4.5）。
func TestMapAssignError_FKViolation(t *testing.T) {
	// Arrange / Act
	err := mapAssignError(fkViolation())

	// Assert
	var de *pkgerrors.Error
	if !stderrors.As(err, &de) || de.Code != pkgerrors.CodeNotFound {
		t.Fatalf("err = %v; want CodeNotFound", err)
	}
	if !stderrors.Is(err, ErrPolicyNotFound) {
		t.Errorf("err は ErrPolicyNotFound と Is 一致すべき: got %v", err)
	}
}

// TestMapAssignError_OtherDBError は FK 違反以外の割当 UPDATE エラーが CodeUnavailable に
// 写像されることを検証する（異常系）。
func TestMapAssignError_OtherDBError(t *testing.T) {
	// Arrange
	sentinel := fmt.Errorf("deadlock detected")

	// Act
	err := mapAssignError(sentinel)

	// Assert
	var de *pkgerrors.Error
	if !stderrors.As(err, &de) || de.Code != pkgerrors.CodeUnavailable {
		t.Fatalf("err = %v; want CodeUnavailable", err)
	}
	if !stderrors.Is(err, sentinel) {
		t.Errorf("Cause が sentinel を保持していない: %v", err)
	}
}

// TestMapAssignError_Nil は成功（err == nil）が nil を返すことを検証する（境界値 /
// affected=0 の他テナント device 経路は err == nil で (0, nil) を返し Service が NotFound 判定する）。
func TestMapAssignError_Nil(t *testing.T) {
	if got := mapAssignError(nil); got != nil {
		t.Fatalf("mapAssignError(nil) = %v; want nil", got)
	}
}

// TestNullableUpdatedBy は UpdatedBy ポインタがそのまま bind 値として渡る（nil → NULL bind /
// 非 nil → 値 bind）ことを検証する（境界値 / INSERT・UPDATE の引数化）。
func TestNullableUpdatedBy(t *testing.T) {
	updater := uuid.New()
	tests := []struct {
		name    string
		in      *uuid.UUID
		wantNil bool
	}{
		{name: "nil は NULL bind（nil）になる", in: nil, wantNil: true},
		{name: "非 nil はその値が bind される", in: &updater, wantNil: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := nullableUpdatedBy(tt.in)
			if tt.wantNil {
				if got != nil {
					t.Fatalf("got %v; want nil", got)
				}
				return
			}
			if got == nil || *got != updater {
				t.Fatalf("got %v; want %v", got, updater)
			}
		})
	}
}

// compile-time check: *fakeRow は rowScanner を満たす。
var _ rowScanner = (*fakeRow)(nil)
