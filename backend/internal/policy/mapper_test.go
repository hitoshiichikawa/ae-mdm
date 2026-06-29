package policy

import (
	"reflect"
	"strings"
	"testing"
)

// findError は errs に (domain, field) の ValidationError が含まれるかを返す（mapper テスト用）。
func findError(errs []ValidationError, domain Domain, field string) (ValidationError, bool) {
	for _, e := range errs {
		if e.Domain == domain && e.Field == field {
			return e, true
		}
	}
	return ValidationError{}, false
}

// --- task 1.2: RawToPolicyInput 正常変換（Req 2.1） ---

// TestRawToPolicyInput_FullMapping は 5 領域を含む raw body が PolicyInput の各 struct へ
// 正しく写像されることを検証する（正常系 / Req 2.1）。
func TestRawToPolicyInput_FullMapping(t *testing.T) {
	// Arrange
	raw := map[string]any{
		"applications": []any{
			map[string]any{"packageName": "com.example.a", "installType": "FORCE_INSTALLED"},
			map[string]any{"packageName": "com.example.kiosk", "installType": "KIOSK"},
		},
		"encryptionPolicy": "ENABLED_WITH_PASSWORD",
		"passwordPolicies": []any{
			map[string]any{"passwordMinimumLength": float64(8), "passwordQuality": "NUMERIC"},
		},
		"systemUpdate": map[string]any{
			"type":         "WINDOWED",
			"startMinutes": float64(120),
			"endMinutes":   float64(240),
		},
	}

	// Act
	in, errs := RawToPolicyInput(raw)

	// Assert
	if len(errs) != 0 {
		t.Fatalf("正常 body で変換エラーが発生: %+v", errs)
	}
	if in.App == nil || in.App.AppCount != 2 {
		t.Errorf("App.AppCount = %v, want 2", in.App)
	}
	if in.Kiosk == nil || !reflect.DeepEqual(in.Kiosk.PackageNames, []string{"com.example.kiosk"}) {
		t.Errorf("Kiosk.PackageNames = %v, want [com.example.kiosk]", in.Kiosk)
	}
	if in.Security == nil || in.Security.EncryptionPolicy != "ENABLED_WITH_PASSWORD" || in.Security.PasswordQuality != "NUMERIC" {
		t.Errorf("Security = %+v, want EncryptionPolicy=ENABLED_WITH_PASSWORD / PasswordQuality=NUMERIC", in.Security)
	}
	if in.Password == nil || in.Password.MinimumLength != 8 {
		t.Errorf("Password.MinimumLength = %v, want 8", in.Password)
	}
	if in.SystemUpdate == nil || in.SystemUpdate.Type != "WINDOWED" || in.SystemUpdate.StartMinutes != 120 || in.SystemUpdate.EndMinutes != 240 {
		t.Errorf("SystemUpdate = %+v, want WINDOWED/120/240", in.SystemUpdate)
	}
}

// TestRawToPolicyInput_PartialMapping は一部領域のみを含む raw body で、未指定領域の
// PolicyInput ポインタが nil（=Validator が検証 skip する部分更新）になることを検証する
// （境界値 / 部分更新の観点）。
func TestRawToPolicyInput_PartialMapping(t *testing.T) {
	// Arrange: applications のみ指定（Kiosk アプリ無し）
	raw := map[string]any{
		"applications": []any{
			map[string]any{"packageName": "com.example.a", "installType": "AVAILABLE"},
		},
	}

	// Act
	in, errs := RawToPolicyInput(raw)

	// Assert
	if len(errs) != 0 {
		t.Fatalf("部分 body で変換エラーが発生: %+v", errs)
	}
	if in.App == nil || in.App.AppCount != 1 {
		t.Errorf("App.AppCount = %v, want 1", in.App)
	}
	// Kiosk アプリが無いので Kiosk は nil（検証 skip）
	if in.Kiosk != nil {
		t.Errorf("Kiosk = %+v, want nil（KIOSK installType の app が無い）", in.Kiosk)
	}
	for name, ptrNil := range map[string]bool{
		"Password":     in.Password == nil,
		"Security":     in.Security == nil,
		"SystemUpdate": in.SystemUpdate == nil,
	} {
		if !ptrNil {
			t.Errorf("未指定領域 %s が nil でない（部分更新で検証 skip されるべき）", name)
		}
	}
}

// --- task 1.2: 型不整合 → invalid field（Req 2.3 / failure path） ---

// TestRawToPolicyInput_TypeMismatch は各領域の型不整合が invalid field 相当の
// ValidationError（Kind: KindInvalidField）へ写像されることを検証する（異常系 / Req 2.3）。
func TestRawToPolicyInput_TypeMismatch(t *testing.T) {
	tests := []struct {
		name       string
		raw        map[string]any
		wantDomain Domain
		wantField  string
	}{
		{
			name:       "applications が配列でないとき invalid field",
			raw:        map[string]any{"applications": "not-an-array"},
			wantDomain: DomainApp,
			wantField:  "applications",
		},
		{
			name:       "encryptionPolicy が文字列でないとき invalid field",
			raw:        map[string]any{"encryptionPolicy": float64(1)},
			wantDomain: DomainSecurity,
			wantField:  "EncryptionPolicy",
		},
		{
			name: "passwordMinimumLength が整数でないとき invalid field",
			raw: map[string]any{
				"passwordPolicies": []any{map[string]any{"passwordMinimumLength": "eight"}},
			},
			wantDomain: DomainPassword,
			wantField:  "MinimumLength",
		},
		{
			name: "passwordMinimumLength が小数のとき invalid field",
			raw: map[string]any{
				"passwordPolicies": []any{map[string]any{"passwordMinimumLength": float64(8.5)}},
			},
			wantDomain: DomainPassword,
			wantField:  "MinimumLength",
		},
		{
			name:       "systemUpdate がオブジェクトでないとき invalid field",
			raw:        map[string]any{"systemUpdate": []any{}},
			wantDomain: DomainSystemUpdate,
			wantField:  "systemUpdate",
		},
		{
			name: "systemUpdate.startMinutes が整数でないとき invalid field",
			raw: map[string]any{
				"systemUpdate": map[string]any{"type": "WINDOWED", "startMinutes": "morning"},
			},
			wantDomain: DomainSystemUpdate,
			wantField:  "StartMinutes",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Act
			_, errs := RawToPolicyInput(tt.raw)

			// Assert
			e, found := findError(errs, tt.wantDomain, tt.wantField)
			if !found {
				t.Fatalf("invalid field を期待したが (domain=%s field=%s) のエラーが無い: %+v", tt.wantDomain, tt.wantField, errs)
			}
			if e.Kind != KindInvalidField {
				t.Errorf("Kind = %s, want %s", e.Kind, KindInvalidField)
			}
		})
	}
}

// --- PR #60 round5: passwordPolicies 全要素検証（Req 2.3 / 2.5） ---

// TestRawToPolicyInput_MultiElementPasswordPolicies は passwordPolicies の 2 件目以降の要素も
// 型・範囲・enum 検証され、不正値が（BuildPolicyBody 経由で）AMAPI へ到達する前に拒否される
// ことを検証する（異常系 / Req 2.3 / 2.5 / PR #60 round5）。
func TestRawToPolicyInput_MultiElementPasswordPolicies(t *testing.T) {
	tests := []struct {
		name       string
		raw        map[string]any
		wantDomain Domain
		wantField  string
	}{
		{
			name: "2 件目の passwordMinimumLength が範囲外のとき invalid field（配列添字付き）",
			raw: map[string]any{
				"passwordPolicies": []any{
					map[string]any{"passwordMinimumLength": float64(8)},  // 妥当（先頭）
					map[string]any{"passwordMinimumLength": float64(17)}, // 範囲外（0〜16 超過）
				},
			},
			wantDomain: DomainPassword,
			wantField:  "passwordPolicies[1].passwordMinimumLength",
		},
		{
			name: "2 件目の passwordMinimumLength が整数でないとき invalid field（配列添字付き）",
			raw: map[string]any{
				"passwordPolicies": []any{
					map[string]any{"passwordMinimumLength": float64(8)},
					map[string]any{"passwordMinimumLength": "eight"}, // 型不整合
				},
			},
			wantDomain: DomainPassword,
			wantField:  "passwordPolicies[1].passwordMinimumLength",
		},
		{
			name: "2 件目の passwordQuality が enum 許容値外のとき invalid field（配列添字付き）",
			raw: map[string]any{
				"passwordPolicies": []any{
					map[string]any{"passwordQuality": "NUMERIC"},
					map[string]any{"passwordQuality": "BOGUS_QUALITY"}, // enum 外
				},
			},
			wantDomain: DomainSecurity,
			wantField:  "passwordPolicies[1].passwordQuality",
		},
		{
			name: "2 件目の passwordQuality が文字列でないとき invalid field（配列添字付き）",
			raw: map[string]any{
				"passwordPolicies": []any{
					map[string]any{"passwordQuality": "NUMERIC"},
					map[string]any{"passwordQuality": float64(1)}, // 型不整合
				},
			},
			wantDomain: DomainSecurity,
			wantField:  "passwordPolicies[1].passwordQuality",
		},
		{
			name: "2 件目が配列要素としてオブジェクトでないとき invalid field（配列添字付き）",
			raw: map[string]any{
				"passwordPolicies": []any{
					map[string]any{"passwordMinimumLength": float64(8)},
					"not-an-object", // 要素がオブジェクトでない
				},
			},
			wantDomain: DomainPassword,
			wantField:  "passwordPolicies[1]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Act
			_, errs := RawToPolicyInput(tt.raw)

			// Assert
			e, found := findError(errs, tt.wantDomain, tt.wantField)
			if !found {
				t.Fatalf("追加要素の invalid field を期待したが (domain=%s field=%s) のエラーが無い: %+v", tt.wantDomain, tt.wantField, errs)
			}
			if e.Kind != KindInvalidField {
				t.Errorf("Kind = %s, want %s", e.Kind, KindInvalidField)
			}
		})
	}
}

// TestRawToPolicyInput_MultiElementPasswordPolicies_AllValid は複数要素がすべて妥当な場合に
// 変換エラーが発生せず、先頭要素が PolicyInput へ写像されることを検証する（正常系 / 回帰防止）。
func TestRawToPolicyInput_MultiElementPasswordPolicies_AllValid(t *testing.T) {
	// Arrange: 2 要素ともに範囲内・enum 許容値内
	raw := map[string]any{
		"passwordPolicies": []any{
			map[string]any{"passwordMinimumLength": float64(8), "passwordQuality": "NUMERIC"},
			map[string]any{"passwordMinimumLength": float64(6), "passwordQuality": "ALPHABETIC"},
		},
	}

	// Act
	in, errs := RawToPolicyInput(raw)

	// Assert
	if len(errs) != 0 {
		t.Fatalf("全要素妥当な passwordPolicies で変換エラーが発生: %+v", errs)
	}
	// 先頭要素が PolicyInput へ写像される（Validator の範囲・enum 検証経路へ載る）。
	if in.Password == nil || in.Password.MinimumLength != 8 {
		t.Errorf("Password.MinimumLength = %v, want 8（先頭要素を写像）", in.Password)
	}
	if in.Security == nil || in.Security.PasswordQuality != "NUMERIC" {
		t.Errorf("Security.PasswordQuality = %+v, want NUMERIC（先頭要素を写像）", in.Security)
	}
}

// TestRawToPolicyInput_FirstPasswordElementRangeDeferredToValidator は先頭要素の範囲外を mapper が
// エラー化せず（型は妥当なので）PolicyInput へ写像し、範囲検証を Validator へ委ねることを検証する
// （境界 / mapper と Validator の二重検証を避ける契約の明示）。
func TestRawToPolicyInput_FirstPasswordElementRangeDeferredToValidator(t *testing.T) {
	// Arrange: 先頭要素の minimumLength が範囲外（17）。型は整数なので mapper は写像のみ。
	raw := map[string]any{
		"passwordPolicies": []any{
			map[string]any{"passwordMinimumLength": float64(17)},
		},
	}

	// Act
	in, errs := RawToPolicyInput(raw)

	// Assert: mapper は範囲エラーを出さず（Validator の責務）、値を写像する。
	if len(errs) != 0 {
		t.Fatalf("先頭要素は mapper で範囲検証しない想定だがエラーが発生: %+v", errs)
	}
	if in.Password == nil || in.Password.MinimumLength != 17 {
		t.Errorf("Password.MinimumLength = %v, want 17（Validator が範囲検証する）", in.Password)
	}
}

// TestRawToPolicyInput_NoRawValueLeak は型不整合エラーの Message に raw body の生値を
// 載せないことを検証する（Req 5.4 / NFR 3.2 を見据えた変換層の安全側設計）。
func TestRawToPolicyInput_NoRawValueLeak(t *testing.T) {
	// Arrange: 機密値を模した文字列を不正型として混入させる
	const secret = "s3cr3t-token-value"
	raw := map[string]any{"encryptionPolicy": map[string]any{"leak": secret}}

	// Act
	_, errs := RawToPolicyInput(raw)

	// Assert
	if len(errs) == 0 {
		t.Fatal("型不整合で invalid field を期待したがエラーが無い")
	}
	for _, e := range errs {
		if strings.Contains(e.Message, secret) {
			t.Errorf("ValidationError.Message が raw 生値 %q を露出している: %q", secret, e.Message)
		}
	}
}

// --- task 1.2: 空 body（境界 / 空入力） ---

// TestRawToPolicyInput_EmptyBody は空 / nil の raw body で、エラーも領域抽出も発生しない
// （全領域 nil = 検証 skip）ことを検証する（境界値 / 空入力 / Req 2.1）。
func TestRawToPolicyInput_EmptyBody(t *testing.T) {
	for _, raw := range []map[string]any{nil, {}} {
		// Act
		in, errs := RawToPolicyInput(raw)

		// Assert
		if len(errs) != 0 {
			t.Errorf("空 body で変換エラーが発生: %+v", errs)
		}
		if in.App != nil || in.Password != nil || in.Security != nil || in.SystemUpdate != nil || in.Kiosk != nil {
			t.Errorf("空 body で領域が抽出された: %+v", in)
		}
	}
}

// --- task 1.2: BuildPolicyBody pass-through（NFR 1.1） ---

// TestBuildPolicyBody_PassThrough は検証通過後の raw body を加工せず PolicyBody.Raw へ
// pass-through すること、Validator が抽出しない追加フィールド（minimumApiLevel 等）も
// 保持されることを検証する（NFR 1.1 / design 確認事項 4）。
func TestBuildPolicyBody_PassThrough(t *testing.T) {
	// Arrange: Validator 非対象の追加フィールド（NFR 1.1 の minimumApiLevel）を含む raw body
	raw := map[string]any{
		"applications":    []any{},
		"minimumApiLevel": float64(29),
		"cameraDisabled":  false,
	}
	const policyName = "enterprises/LC123/policies/p-1"

	// Act
	body := BuildPolicyBody(policyName, raw)

	// Assert
	if body.Name != policyName {
		t.Errorf("PolicyBody.Name = %q, want %q", body.Name, policyName)
	}
	if !reflect.DeepEqual(body.Raw, raw) {
		t.Errorf("PolicyBody.Raw が pass-through されていない: got %+v want %+v", body.Raw, raw)
	}
	// minimumApiLevel（Validator 非対象）が保持されていること（NFR 1.1 透過）
	if _, ok := body.Raw["minimumApiLevel"]; !ok {
		t.Error("追加フィールド minimumApiLevel が pass-through で失われた")
	}
}
