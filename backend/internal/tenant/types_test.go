package tenant

import (
	stdErrors "errors"
	"testing"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// TestStatusValid は Status.Valid が定義済み 4 値を正常判定し、未定義値を弾くことを検証する
// （NFR 1.1: 状態は常に pending_bind / binding / bound / disabled の 4 値のいずれか 1 つ）。
func TestStatusValid(t *testing.T) {
	t.Run("定義済み 4 値のとき true を返す", func(t *testing.T) {
		for _, s := range []Status{StatusPendingBind, StatusBinding, StatusBound, StatusDisabled} {
			// Act
			got := s.Valid()
			// Assert
			if !got {
				t.Errorf("Status(%q).Valid() = false, want true", s)
			}
		}
	})

	t.Run("binding を受理する（4 値化 / Req 4.1）", func(t *testing.T) {
		// Arrange
		s := StatusBinding
		// Act
		got := s.Valid()
		// Assert
		if !got {
			t.Errorf("Status(%q).Valid() = false, want true", s)
		}
		if string(s) != "binding" {
			t.Errorf("StatusBinding = %q, want %q", s, "binding")
		}
	})

	t.Run("空文字のとき false を返す（空入力）", func(t *testing.T) {
		// Arrange
		s := Status("")
		// Act
		got := s.Valid()
		// Assert
		if got {
			t.Errorf("Status(\"\").Valid() = true, want false")
		}
	})

	t.Run("未定義値や大文字違いのとき false を返す（異常系）", func(t *testing.T) {
		for _, s := range []Status{"unknown", "BOUND", "Pending_Bind", "deleted", " bound "} {
			// Act
			got := s.Valid()
			// Assert
			if got {
				t.Errorf("Status(%q).Valid() = true, want false", s)
			}
		}
	})
}

// TestParseStatus は ParseStatus が定義済み 4 値を Status へ変換し、未定義値を
// CodeInvalidRequest の *errors.Error として弾くことを検証する（NFR 1.1 / Req 4.1）。
func TestParseStatus(t *testing.T) {
	t.Run("定義済み 4 値を対応する Status へ変換する（正常系）", func(t *testing.T) {
		cases := map[string]Status{
			"pending_bind": StatusPendingBind,
			"binding":      StatusBinding,
			"bound":        StatusBound,
			"disabled":     StatusDisabled,
		}
		for in, want := range cases {
			// Act
			got, err := ParseStatus(in)
			// Assert
			if err != nil {
				t.Errorf("ParseStatus(%q) returned unexpected error: %v", in, err)
				continue
			}
			if got != want {
				t.Errorf("ParseStatus(%q) = %q, want %q", in, got, want)
			}
		}
	})

	t.Run("空文字を CodeInvalidRequest で弾く（空入力）", func(t *testing.T) {
		// Act
		_, err := ParseStatus("")
		// Assert
		assertInvalidRequest(t, err)
	})

	t.Run("未定義値を CodeInvalidRequest で弾く（異常系）", func(t *testing.T) {
		for _, in := range []string{"unknown", "BOUND", "Disabled", "pending"} {
			// Act
			_, err := ParseStatus(in)
			// Assert
			assertInvalidRequest(t, err)
		}
	})
}

// assertInvalidRequest は err が CodeInvalidRequest の *errors.Error であることを検証する。
func assertInvalidRequest(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var de *pkgerrors.Error
	if !stdErrors.As(err, &de) {
		t.Fatalf("expected *errors.Error, got %T (%v)", err, err)
	}
	if de.Code != pkgerrors.CodeInvalidRequest {
		t.Errorf("error code = %q, want %q", de.Code, pkgerrors.CodeInvalidRequest)
	}
}

// TestSentinelErrorCodes は sentinel error 群が design.md の Code 写像どおりの Code を持つ
// *errors.Error であることを検証する（Req 2.5 / 2.6 / 3.2 / 3.4 / 4.3 / 5.2 / 5.3 / 6.5）。
func TestSentinelErrorCodes(t *testing.T) {
	cases := []struct {
		name     string
		err      *pkgerrors.Error
		wantCode pkgerrors.Code
	}{
		{"ErrConflict は CodeConflict", ErrConflict, pkgerrors.CodeConflict},
		{"ErrInvalidState は CodeBusinessRule", ErrInvalidState, pkgerrors.CodeBusinessRule},
		{"ErrConfirmationRequired は CodeBusinessRule", ErrConfirmationRequired, pkgerrors.CodeBusinessRule},
		{"ErrNotBound は CodeBusinessRule", ErrNotBound, pkgerrors.CodeBusinessRule},
		{"ErrTenantDisabled は CodeBusinessRule", ErrTenantDisabled, pkgerrors.CodeBusinessRule},
		{"ErrTenantNotFound は CodeNotFound", ErrTenantNotFound, pkgerrors.CodeNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Assert
			if tc.err == nil {
				t.Fatalf("sentinel is nil")
			}
			if tc.err.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", tc.err.Code, tc.wantCode)
			}
			if tc.err.Message == "" {
				t.Errorf("sentinel message must not be empty")
			}
		})
	}
}

// TestOperationValues は Operation enum が監査対象の操作種別を期待文字列で保持することを
// 検証する（NFR 2.1 / 回収操作の追加 Req 2.4）。
func TestOperationValues(t *testing.T) {
	t.Run("recover 操作が追加され文字列 recover を持つ（Req 2.4）", func(t *testing.T) {
		// Act / Assert
		if string(OperationRecover) != "recover" {
			t.Errorf("OperationRecover = %q, want %q", OperationRecover, "recover")
		}
	})

	t.Run("既存操作種別が期待文字列を保持する", func(t *testing.T) {
		cases := map[Operation]string{
			OperationCreate:  "create",
			OperationBind:    "bind",
			OperationDisable: "disable",
		}
		for op, want := range cases {
			// Assert
			if string(op) != want {
				t.Errorf("Operation = %q, want %q", op, want)
			}
		}
	})
}

// TestViewFromRow は TenantRow から TenantView への変換で id/name/status/enterprise_name が
// 写像されることを検証する（Req 4.2 のシリアライズ前提）。
func TestViewFromRow(t *testing.T) {
	t.Run("bound 行は enterprise_name を引き継ぐ", func(t *testing.T) {
		// Arrange
		row := TenantRow{Name: "acme", Status: StatusBound, EnterpriseName: "enterprises/LC123"}
		// Act
		view := ViewFromRow(row)
		// Assert
		if view.Name != "acme" || view.Status != StatusBound || view.EnterpriseName != "enterprises/LC123" {
			t.Errorf("unexpected view: %+v", view)
		}
	})

	t.Run("pending_bind 行は enterprise_name が空", func(t *testing.T) {
		// Arrange
		row := TenantRow{Name: "acme", Status: StatusPendingBind}
		// Act
		view := ViewFromRow(row)
		// Assert
		if view.EnterpriseName != "" {
			t.Errorf("enterprise_name = %q, want empty", view.EnterpriseName)
		}
	})

	t.Run("binding 行は status を binding として載せ enterprise_name を露出しない（Req 4.4）", func(t *testing.T) {
		// Arrange: binding（予約中）は未バインド扱いであり enterprise_name は未確定（NFR 1.2）。
		// 仮に DB 上に値が残っていても View には載せない（status!=bound 非露出契約 / Req 4.2 / 6.5）。
		row := TenantRow{Name: "acme", Status: StatusBinding, EnterpriseName: "enterprises/LC123"}
		// Act
		view := ViewFromRow(row)
		// Assert
		if view.Status != StatusBinding {
			t.Errorf("status = %q, want binding", view.Status)
		}
		if view.EnterpriseName != "" {
			t.Errorf("binding tenant view must not expose enterprise_name, got %q", view.EnterpriseName)
		}
	})

	t.Run("disabled 行は DB に enterprise_name が残っていても View で露出しない（Req 4.2 / 6.5）", func(t *testing.T) {
		// Arrange: bound 済みテナントを無効化した行は DB 上 enterprise_name を監査目的で保持するが、
		// View には載せてはならない（status!=bound で enterprise 識別子を露出しない契約）。
		row := TenantRow{Name: "acme", Status: StatusDisabled, EnterpriseName: "enterprises/LC123"}
		// Act
		view := ViewFromRow(row)
		// Assert
		if view.EnterpriseName != "" {
			t.Errorf("disabled tenant view must not expose enterprise_name, got %q", view.EnterpriseName)
		}
		if view.Status != StatusDisabled {
			t.Errorf("status = %q, want disabled", view.Status)
		}
	})
}
