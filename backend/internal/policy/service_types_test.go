package policy

import (
	"encoding/json"
	stderrors "errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// --- task 1.1: application 層 DTO の zero-value / JSON tag / sentinel error（Req 4.5） ---

// TestPolicyRow_ZeroValue は PolicyRow の zero-value が想定どおり「空」であることを確認する。
// 列ごと型付き scan（NFR 2.1）前提のため、nullable な UpdatedBy が nil・Body が nil である
// ことを保証する（境界値 / 空入力の観点）。
func TestPolicyRow_ZeroValue(t *testing.T) {
	// Arrange / Act
	var row PolicyRow

	// Assert
	if row.ID != uuid.Nil {
		t.Errorf("zero-value PolicyRow.ID = %v, want uuid.Nil", row.ID)
	}
	if row.UpdatedBy != nil {
		t.Errorf("zero-value PolicyRow.UpdatedBy = %v, want nil（NULL 列を nil で表現）", row.UpdatedBy)
	}
	if row.Body != nil {
		t.Errorf("zero-value PolicyRow.Body = %v, want nil", row.Body)
	}
	if row.Version != 0 {
		t.Errorf("zero-value PolicyRow.Version = %d, want 0", row.Version)
	}
}

// TestPolicyView_JSONTags は PolicyView の JSON tag が API Contract（snake_case）に一致する
// ことを、marshal 結果のキー集合で検証する（正常系）。Handler が直接シリアライズする契約。
func TestPolicyView_JSONTags(t *testing.T) {
	// Arrange
	view := PolicyView{
		ID:      uuid.New(),
		Name:    "default",
		Body:    map[string]any{"applications": []any{}},
		Version: 3,
	}

	// Act
	b, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("PolicyView の marshal に失敗: %v", err)
	}

	// Assert
	for _, key := range []string{`"id"`, `"name"`, `"body"`, `"version"`, `"created_at"`, `"updated_at"`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("PolicyView JSON にキー %s が含まれない: %s", key, b)
		}
	}
}

// TestPolicySummary_OmitsBody は一覧表現（PolicySummary）が本体 JSON snapshot（body）を
// 持たないことを検証する（Req 5.4 / NFR 3.2: 一覧経路で本体機密値を広く露出させない）。
func TestPolicySummary_OmitsBody(t *testing.T) {
	// Arrange
	summary := PolicySummary{ID: uuid.New(), Name: "default", Version: 1}

	// Act
	b, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("PolicySummary の marshal に失敗: %v", err)
	}

	// Assert
	if strings.Contains(string(b), `"body"`) {
		t.Errorf("PolicySummary JSON が body を含んでいる（一覧では本体を載せない契約）: %s", b)
	}
}

// TestPolicyRequest_Unmarshal は入力 DTO（PolicyRequest）が JSON から name / body を読み取れる
// ことを検証する（正常系）。body は map[string]any で pass-through 受領する契約。
func TestPolicyRequest_Unmarshal(t *testing.T) {
	// Arrange
	raw := []byte(`{"name":"kiosk","body":{"passwordMinimumLength":6}}`)

	// Act
	var req PolicyRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("PolicyRequest の unmarshal に失敗: %v", err)
	}

	// Assert
	if req.Name != "kiosk" {
		t.Errorf("PolicyRequest.Name = %q, want \"kiosk\"", req.Name)
	}
	if got, ok := req.Body["passwordMinimumLength"].(float64); !ok || got != 6 {
		t.Errorf("PolicyRequest.Body[passwordMinimumLength] = %v, want 6", req.Body["passwordMinimumLength"])
	}
}

// TestAssignInput_Unmarshal は割当入力（AssignInput）が device_id を UUID として読み取れる
// ことを検証する（正常系 + 異常系: 不正 UUID 文字列は unmarshal エラー）。
func TestAssignInput_Unmarshal(t *testing.T) {
	t.Run("有効な device_id のとき読み取れる", func(t *testing.T) {
		// Arrange
		id := uuid.New()
		raw := []byte(`{"device_id":"` + id.String() + `"}`)

		// Act
		var in AssignInput
		if err := json.Unmarshal(raw, &in); err != nil {
			t.Fatalf("AssignInput の unmarshal に失敗: %v", err)
		}

		// Assert
		if in.DeviceID != id {
			t.Errorf("AssignInput.DeviceID = %v, want %v", in.DeviceID, id)
		}
	})

	t.Run("不正な device_id 文字列のとき unmarshal エラーになる", func(t *testing.T) {
		// Arrange
		raw := []byte(`{"device_id":"not-a-uuid"}`)

		// Act
		var in AssignInput
		err := json.Unmarshal(raw, &in)

		// Assert
		if err == nil {
			t.Fatal("不正な UUID 文字列で unmarshal エラーを期待したが nil")
		}
	})
}

// TestPolicySentinelErrors は sentinel error が期待する Code（HTTP ステータス写像元）を持ち、
// 存在差を露出しない汎用 message であることを検証する（Req 4.5 / design Error Handling）。
func TestPolicySentinelErrors(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode pkgerrors.Code
	}{
		{name: "ErrPolicyNotFound は CodeNotFound(404)", err: ErrPolicyNotFound, wantCode: pkgerrors.CodeNotFound},
		{name: "ErrDeleteConflict は CodeConflict(409)", err: ErrDeleteConflict, wantCode: pkgerrors.CodeConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange / Act
			var domainErr *pkgerrors.Error
			ok := stderrors.As(tt.err, &domainErr)

			// Assert
			if !ok {
				t.Fatalf("%v は *errors.Error として扱えない", tt.err)
			}
			if domainErr.Code != tt.wantCode {
				t.Errorf("Code = %s, want %s", domainErr.Code, tt.wantCode)
			}
		})
	}
}

// TestErrPolicyNotFound_NoExistenceLeak は ErrPolicyNotFound の message が対象の存在有無や
// テナント識別子を露出しない汎用文言であることを確認する（Req 4.5: 存在差非露出）。
func TestErrPolicyNotFound_NoExistenceLeak(t *testing.T) {
	// Arrange / Act
	var domainErr *pkgerrors.Error
	if !stderrors.As(ErrPolicyNotFound, &domainErr) {
		t.Fatal("ErrPolicyNotFound が *errors.Error ではない")
	}
	msg := strings.ToLower(domainErr.Message)

	// Assert: tenant / id 等の存在差を示唆する語を含めない（汎用 message であること）
	for _, leak := range []string{"tenant", "exists", "other"} {
		if strings.Contains(msg, leak) {
			t.Errorf("ErrPolicyNotFound message が存在差を示唆する語 %q を含む: %q", leak, domainErr.Message)
		}
	}
}
