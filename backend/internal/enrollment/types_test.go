package enrollment

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestAdditionalData_Marshal は AdditionalData.Marshal（Req 1.3）を検証する。
// tenant_id / issued_by / mode の 3 キーを含む決定論的 JSON を返すこと、および Nil UUID・空 Mode の
// 境界でも error なく全キーを出力することを固定する。

func TestAdditionalData_Marshal_WithValues_ContainsTenantIssuerMode(t *testing.T) {
	// Arrange
	tenantID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	issuedBy := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	ad := AdditionalData{TenantID: tenantID, IssuedBy: issuedBy, Mode: ModeFullyManaged}

	// Act
	got, err := ad.Marshal()

	// Assert
	if err != nil {
		t.Fatalf("Marshal() 予期しない error: %v", err)
	}
	var decoded map[string]string
	if uerr := json.Unmarshal([]byte(got), &decoded); uerr != nil {
		t.Fatalf("Marshal() の出力が JSON として parse できない: %v (raw=%q)", uerr, got)
	}
	if decoded["tenant_id"] != tenantID.String() {
		t.Errorf("tenant_id = %q, want %q", decoded["tenant_id"], tenantID.String())
	}
	if decoded["issued_by"] != issuedBy.String() {
		t.Errorf("issued_by = %q, want %q", decoded["issued_by"], issuedBy.String())
	}
	if decoded["mode"] != string(ModeFullyManaged) {
		t.Errorf("mode = %q, want %q", decoded["mode"], ModeFullyManaged)
	}
}

func TestAdditionalData_Marshal_DoesNotLeakSecretKeys(t *testing.T) {
	// Arrange: 秘密値（value / qr）を思わせるキーが混入しないことを固定する（NFR 3.1）。
	ad := AdditionalData{TenantID: uuid.New(), IssuedBy: uuid.New(), Mode: ModeDedicated}

	// Act
	got, err := ad.Marshal()

	// Assert
	if err != nil {
		t.Fatalf("Marshal() 予期しない error: %v", err)
	}
	var decoded map[string]any
	if uerr := json.Unmarshal([]byte(got), &decoded); uerr != nil {
		t.Fatalf("Marshal() の出力が JSON として parse できない: %v", uerr)
	}
	if len(decoded) != 3 {
		t.Fatalf("キー数 = %d, want 3 (tenant_id/issued_by/mode のみ) / raw=%q", len(decoded), got)
	}
	for k := range decoded {
		lower := strings.ToLower(k)
		if strings.Contains(lower, "value") || strings.Contains(lower, "qr") {
			t.Errorf("additionalData に秘密値キー %q が混入している", k)
		}
	}
}

func TestAdditionalData_Marshal_NilUUIDs_EmitsZeroUUIDStrings(t *testing.T) {
	// Arrange: 境界（Nil UUID / 空 Mode）。error なく zero-uuid 文字列と空 mode を出力すること。
	ad := AdditionalData{TenantID: uuid.Nil, IssuedBy: uuid.Nil, Mode: ""}

	// Act
	got, err := ad.Marshal()

	// Assert
	if err != nil {
		t.Fatalf("Marshal() 予期しない error: %v", err)
	}
	var decoded map[string]string
	if uerr := json.Unmarshal([]byte(got), &decoded); uerr != nil {
		t.Fatalf("Marshal() の出力が JSON として parse できない: %v", uerr)
	}
	const zeroUUID = "00000000-0000-0000-0000-000000000000"
	if decoded["tenant_id"] != zeroUUID {
		t.Errorf("tenant_id = %q, want %q", decoded["tenant_id"], zeroUUID)
	}
	if decoded["issued_by"] != zeroUUID {
		t.Errorf("issued_by = %q, want %q", decoded["issued_by"], zeroUUID)
	}
	if _, ok := decoded["mode"]; !ok {
		t.Errorf("空 Mode でも mode キーは存在すべき / raw=%q", got)
	}
}

// TestDeriveStatus は expires_at からの Status 派生（Req 4.1）を境界含めて検証する。
// 判定境界: now < expires_at → active、now >= expires_at → expired（now == expires_at は expired）。

func TestDeriveStatus_WhenNowBeforeExpiry_ReturnsActive(t *testing.T) {
	// Arrange
	expiresAt := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	now := expiresAt.Add(-time.Hour)

	// Act
	got := DeriveStatus(expiresAt, now)

	// Assert
	if got != StatusActive {
		t.Errorf("DeriveStatus(期限前) = %q, want %q", got, StatusActive)
	}
}

func TestDeriveStatus_WhenNowAfterExpiry_ReturnsExpired(t *testing.T) {
	// Arrange
	expiresAt := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	now := expiresAt.Add(time.Hour)

	// Act
	got := DeriveStatus(expiresAt, now)

	// Assert
	if got != StatusExpired {
		t.Errorf("DeriveStatus(期限後) = %q, want %q", got, StatusExpired)
	}
}

func TestDeriveStatus_WhenNowEqualsExpiry_ReturnsExpired(t *testing.T) {
	// Arrange: 境界。now == expires_at はちょうど失効した瞬間として expired 扱い。
	expiresAt := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	now := expiresAt

	// Act
	got := DeriveStatus(expiresAt, now)

	// Assert
	if got != StatusExpired {
		t.Errorf("DeriveStatus(now==expires_at) = %q, want %q", got, StatusExpired)
	}
}

func TestDeriveStatus_WhenNowOneNanoBeforeExpiry_ReturnsActive(t *testing.T) {
	// Arrange: 境界直前（1ns 前）は active。
	expiresAt := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	now := expiresAt.Add(-time.Nanosecond)

	// Act
	got := DeriveStatus(expiresAt, now)

	// Assert
	if got != StatusActive {
		t.Errorf("DeriveStatus(1ns 前) = %q, want %q", got, StatusActive)
	}
}

func TestDeriveStatus_WhenNowOneNanoAfterExpiry_ReturnsExpired(t *testing.T) {
	// Arrange: 境界直後（1ns 後）は expired。
	expiresAt := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	now := expiresAt.Add(time.Nanosecond)

	// Act
	got := DeriveStatus(expiresAt, now)

	// Assert
	if got != StatusExpired {
		t.Errorf("DeriveStatus(1ns 後) = %q, want %q", got, StatusExpired)
	}
}

// TestTokenRow_HasNoSecretFields は TokenRow に秘密値 field（Value / QRCode 相当）が存在しないことを
// reflect でフィールド名走査して固定する（NFR 3.1）。永続化スナップショットに秘密値を持ち込まない契約。
func TestTokenRow_HasNoSecretFields(t *testing.T) {
	// Arrange
	rt := reflect.TypeOf(TokenRow{})

	// Act & Assert
	for i := 0; i < rt.NumField(); i++ {
		name := strings.ToLower(rt.Field(i).Name)
		if strings.Contains(name, "value") || strings.Contains(name, "qr") {
			t.Errorf("TokenRow に秘密値 field %q が存在する（NFR 3.1 違反）", rt.Field(i).Name)
		}
	}
	// 期待する field 集合（秘密値を含まないこと）を明示的に固定する。
	want := map[string]bool{
		"ID": true, "TenantID": true, "AMAPITokenName": true, "Mode": true,
		"PolicyID": true, "AdditionalData": true, "ExpiresAt": true,
		"IssuedBy": true, "CreatedAt": true,
	}
	if rt.NumField() != len(want) {
		t.Fatalf("TokenRow の field 数 = %d, want %d (%v)", rt.NumField(), len(want), want)
	}
	for i := 0; i < rt.NumField(); i++ {
		if !want[rt.Field(i).Name] {
			t.Errorf("TokenRow に想定外の field %q が存在する", rt.Field(i).Name)
		}
	}
}

// TestMode_Valid / TestParseMode は Mode enum の pure helper を補助的に検証する（AC 非紐付け）。
// Req 1.4 の不正モードハンドリングは Service（task 2）所有のため、ここでは enum 判定の健全性のみを固定する。

func TestMode_Valid(t *testing.T) {
	cases := []struct {
		name string
		mode Mode
		want bool
	}{
		{"fully_managed は有効", ModeFullyManaged, true},
		{"dedicated は有効", ModeDedicated, true},
		{"未知の値は無効", Mode("kiosk"), false},
		{"空文字は無効", Mode(""), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.mode.Valid(); got != tc.want {
				t.Errorf("Mode(%q).Valid() = %v, want %v", tc.mode, got, tc.want)
			}
		})
	}
}

func TestParseMode(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    Mode
		wantErr bool
	}{
		{"fully_managed を parse", "fully_managed", ModeFullyManaged, false},
		{"dedicated を parse", "dedicated", ModeDedicated, false},
		{"未知の値は ErrInvalidMode", "kiosk", "", true},
		{"空文字は ErrInvalidMode", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseMode(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseMode(%q) は error を期待したが nil", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseMode(%q) 予期しない error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("ParseMode(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
