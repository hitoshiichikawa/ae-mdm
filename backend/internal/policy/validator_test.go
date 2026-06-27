package policy

import (
	"testing"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// hasError は result に (domain, field) の ValidationError が含まれるか、含まれる場合は
// その ValidationError を返す。複数収集された不正項目から特定の 1 件を取り出す helper。
func hasError(r ValidationResult, domain Domain, field string) (ValidationError, bool) {
	for _, e := range r.Errors {
		if e.Domain == domain && e.Field == field {
			return e, true
		}
	}
	return ValidationError{}, false
}

// --- Requirement 1: アプリ領域（件数上限） ---

func TestValidate_App_CountLimit(t *testing.T) {
	tests := []struct {
		name       string // <条件>のとき<期待結果>
		count      int
		wantReject bool
	}{
		// Req 1.1: 3,000 件以下を受理
		{name: "件数が0件のとき受理する(Req1.4 境界 空入力)", count: 0, wantReject: false},
		{name: "件数が1件のとき受理する(Req1.1 正常系)", count: 1, wantReject: false},
		// Req 1.3: 境界値 2,999 / 3,000 / 3,001
		{name: "件数が2999件のとき受理する(Req1.3 境界下)", count: 2999, wantReject: false},
		{name: "件数が3000件ちょうどのとき受理する(Req1.3 境界)", count: 3000, wantReject: false},
		{name: "件数が3001件のとき上限超過で拒否する(Req1.2/1.3 異常系)", count: 3001, wantReject: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			in := PolicyInput{App: &AppPolicy{AppCount: tt.count}}

			// Act
			got := Validate(in)

			// Assert
			e, found := hasError(got, DomainApp, "AppCount")
			if tt.wantReject {
				if !found {
					t.Fatalf("AppCount=%d: 拒否を期待したが検証エラーが無い (result=%+v)", tt.count, got)
				}
				// Req 6.3: 上限超過はビジネスルール違反種別で識別される
				if e.Kind != KindBusinessRule {
					t.Errorf("AppCount=%d: Kind=%s, want %s", tt.count, e.Kind, KindBusinessRule)
				}
			} else if found {
				t.Fatalf("AppCount=%d: 受理を期待したが検証エラーが返った (err=%+v)", tt.count, e)
			}
		})
	}
}

// --- Requirement 2: パスワード領域（桁数の数値範囲） ---

func TestValidate_Password_LengthRange(t *testing.T) {
	tests := []struct {
		name       string
		length     int
		wantReject bool
	}{
		// Req 2.1: 許容範囲内を受理
		{name: "桁数が範囲中央(8)のとき受理する(Req2.1 正常系)", length: 8, wantReject: false},
		// Req 2.4: 下限直下 / 下限ちょうど / 上限ちょうど / 上限直上
		{name: "桁数が下限直下(0)のとき範囲外で拒否する(Req2.2/2.4 境界下異常)", length: 0, wantReject: true},
		{name: "桁数が下限ちょうど(1)のとき受理する(Req2.4 境界下)", length: 1, wantReject: false},
		{name: "桁数が上限ちょうど(16)のとき受理する(Req2.4 境界上)", length: 16, wantReject: false},
		{name: "桁数が上限直上(17)のとき範囲外で拒否する(Req2.3/2.4 境界上異常)", length: 17, wantReject: true},
		// Req 2.2: 負値も下限未満として拒否
		{name: "桁数が負値(-1)のとき範囲外で拒否する(Req2.2 異常系)", length: -1, wantReject: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			in := PolicyInput{Password: &PasswordPolicy{MinimumLength: tt.length}}

			// Act
			got := Validate(in)

			// Assert
			e, found := hasError(got, DomainPassword, "MinimumLength")
			if tt.wantReject {
				if !found {
					t.Fatalf("length=%d: 拒否を期待したが検証エラーが無い", tt.length)
				}
				// Req 6.4: 範囲外は invalid field 種別で識別される
				if e.Kind != KindInvalidField {
					t.Errorf("length=%d: Kind=%s, want %s", tt.length, e.Kind, KindInvalidField)
				}
			} else if found {
				t.Fatalf("length=%d: 受理を期待したが検証エラーが返った (err=%+v)", tt.length, e)
			}
		})
	}
}

// --- Requirement 3: セキュリティ領域（必須項目・enum 値） ---

func TestValidate_Security_RequiredAndEnum(t *testing.T) {
	tests := []struct {
		name        string
		sec         SecurityPolicy
		wantField   string // 空文字なら受理を期待
		wantRejects bool
	}{
		// Req 3.1: 必須充足 + enum 許容値内を受理
		{
			name: "必須充足かつ選択肢が許容値内のとき受理する(Req3.1 正常系)",
			sec:  SecurityPolicy{EncryptionPolicy: "ENABLED_WITHOUT_PASSWORD", PasswordQuality: "NUMERIC"},
		},
		// Req 3.2: 必須項目欠落（空文字）を拒否
		{
			name:        "必須項目EncryptionPolicyが欠落のとき拒否する(Req3.2 異常系)",
			sec:         SecurityPolicy{EncryptionPolicy: "", PasswordQuality: "NUMERIC"},
			wantField:   "EncryptionPolicy",
			wantRejects: true,
		},
		// Req 3.3: enum 許容値外を拒否
		{
			name:        "選択肢項目PasswordQualityが許容値外のとき拒否する(Req3.3 異常系)",
			sec:         SecurityPolicy{EncryptionPolicy: "ENABLED_WITH_PASSWORD", PasswordQuality: "INVALID_QUALITY"},
			wantField:   "PasswordQuality",
			wantRejects: true,
		},
		// Req 3.3: EncryptionPolicy の許容値外（非空）を拒否
		{
			name:        "必須項目EncryptionPolicyが許容値外のとき拒否する(Req3.3 異常系)",
			sec:         SecurityPolicy{EncryptionPolicy: "BROKEN_ENC", PasswordQuality: "COMPLEX"},
			wantField:   "EncryptionPolicy",
			wantRejects: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			in := PolicyInput{Security: &tt.sec}

			// Act
			got := Validate(in)

			// Assert
			if !tt.wantRejects {
				if !got.IsValid() {
					t.Fatalf("受理を期待したが検証エラーが返った (result=%+v)", got)
				}
				return
			}
			e, found := hasError(got, DomainSecurity, tt.wantField)
			if !found {
				t.Fatalf("field=%s の拒否を期待したが検証エラーが無い (result=%+v)", tt.wantField, got)
			}
			if e.Kind != KindInvalidField {
				t.Errorf("field=%s: Kind=%s, want %s", tt.wantField, e.Kind, KindInvalidField)
			}
		})
	}
}

// --- Requirement 4: システム更新領域（必須項目・不正値） ---

func TestValidate_SystemUpdate_RequiredAndRange(t *testing.T) {
	tests := []struct {
		name        string
		upd         SystemUpdatePolicy
		wantField   string
		wantRejects bool
	}{
		// Req 4.1: 必須充足 + 範囲内を受理
		{
			name: "AUTOMATIC指定のとき受理する(Req4.1 正常系)",
			upd:  SystemUpdatePolicy{Type: "AUTOMATIC"},
		},
		{
			name: "WINDOWED指定かつ分範囲内のとき受理する(Req4.1 正常系)",
			upd:  SystemUpdatePolicy{Type: "WINDOWED", StartMinutes: 0, EndMinutes: 1439},
		},
		// Req 4.2: 必須項目欠落を拒否
		{
			name:        "必須項目Typeが欠落のとき拒否する(Req4.2 異常系)",
			upd:         SystemUpdatePolicy{Type: ""},
			wantField:   "Type",
			wantRejects: true,
		},
		// Req 4.3: enum 許容値外を拒否
		{
			name:        "Typeが許容値外のとき拒否する(Req4.3 異常系)",
			upd:         SystemUpdatePolicy{Type: "UNKNOWN_TYPE"},
			wantField:   "Type",
			wantRejects: true,
		},
		// Req 4.3: WINDOWED の分範囲外（上限直上）を拒否
		{
			name:        "WINDOWEDのStartMinutesが範囲上限直上(1440)のとき拒否する(Req4.3 境界異常)",
			upd:         SystemUpdatePolicy{Type: "WINDOWED", StartMinutes: 1440, EndMinutes: 100},
			wantField:   "StartMinutes",
			wantRejects: true,
		},
		// Req 4.3: WINDOWED の分範囲外（下限直下）を拒否
		{
			name:        "WINDOWEDのEndMinutesが範囲下限直下(-1)のとき拒否する(Req4.3 境界異常)",
			upd:         SystemUpdatePolicy{Type: "WINDOWED", StartMinutes: 0, EndMinutes: -1},
			wantField:   "EndMinutes",
			wantRejects: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			in := PolicyInput{SystemUpdate: &tt.upd}

			// Act
			got := Validate(in)

			// Assert
			if !tt.wantRejects {
				if !got.IsValid() {
					t.Fatalf("受理を期待したが検証エラーが返った (result=%+v)", got)
				}
				return
			}
			e, found := hasError(got, DomainSystemUpdate, tt.wantField)
			if !found {
				t.Fatalf("field=%s の拒否を期待したが検証エラーが無い (result=%+v)", tt.wantField, got)
			}
			if e.Kind != KindInvalidField {
				t.Errorf("field=%s: Kind=%s, want %s", tt.wantField, e.Kind, KindInvalidField)
			}
		})
	}
}

// --- Requirement 5: Kiosk 領域（パッケージ名形式） ---

func TestValidate_Kiosk_PackageNameFormat(t *testing.T) {
	tests := []struct {
		name       string
		packages   []string
		wantReject bool
	}{
		// Req 5.1 / 5.3: 妥当な形式を受理
		{name: "妥当な2セグメントパッケージ名のとき受理する(Req5.1 正常系)", packages: []string{"com.example"}, wantReject: false},
		{name: "妥当な3セグメント+数字+アンダースコアのとき受理する(Req5.3 正常系)", packages: []string{"com.example_app.v2"}, wantReject: false},
		// Req 5.2 / 5.3: 形式不正を拒否
		{name: "1セグメントのみのとき形式不正で拒否する(Req5.2/5.3 異常系)", packages: []string{"example"}, wantReject: true},
		{name: "セグメントが数字始まりのとき形式不正で拒否する(Req5.3 異常系)", packages: []string{"com.1example"}, wantReject: true},
		{name: "ハイフン混入のとき形式不正で拒否する(Req5.3 異常系)", packages: []string{"com.exa-mple"}, wantReject: true},
		{name: "末尾ドットで空セグメントのとき形式不正で拒否する(Req5.3 境界異常)", packages: []string{"com.example."}, wantReject: true},
		// Req 5.4: 空文字列を拒否
		{name: "空文字列のとき形式不正で拒否する(Req5.4 境界異常)", packages: []string{""}, wantReject: true},
		// Req 5.1: パッケージ集合が空（0件）は形式不正なし=受理
		{name: "パッケージ集合が空のとき受理する(Req5.1 空入力)", packages: []string{}, wantReject: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			in := PolicyInput{Kiosk: &KioskPolicy{PackageNames: tt.packages}}

			// Act
			got := Validate(in)

			// Assert
			e, found := hasError(got, DomainKiosk, "PackageNames")
			if tt.wantReject {
				if !found {
					t.Fatalf("packages=%v: 拒否を期待したが検証エラーが無い", tt.packages)
				}
				if e.Kind != KindInvalidField {
					t.Errorf("packages=%v: Kind=%s, want %s", tt.packages, e.Kind, KindInvalidField)
				}
			} else if found {
				t.Fatalf("packages=%v: 受理を期待したが検証エラーが返った (err=%+v)", tt.packages, e)
			}
		})
	}
}

// --- Requirement 6: 検証エラーの報告（収集 / 種別区別 / 成功） ---

// Req 6.2: 複数領域の不正項目を 1 度に渡したとき全件収集される
func TestValidate_CollectsAllErrors(t *testing.T) {
	// Arrange: 4 領域に同時に不正値を入れる（app 上限超過 / password 範囲外 /
	// security 必須欠落 / kiosk 形式不正）
	in := PolicyInput{
		App:          &AppPolicy{AppCount: 3001},
		Password:     &PasswordPolicy{MinimumLength: 0},
		Security:     &SecurityPolicy{EncryptionPolicy: ""},
		SystemUpdate: &SystemUpdatePolicy{Type: "AUTOMATIC"}, // 妥当（収集対象外）
		Kiosk:        &KioskPolicy{PackageNames: []string{"invalid"}},
	}

	// Act
	got := Validate(in)

	// Assert: 4 件すべて収集されている
	if len(got.Errors) != 4 {
		t.Fatalf("検証エラー件数 = %d, want 4 (result=%+v)", len(got.Errors), got)
	}
	for _, want := range []struct {
		domain Domain
		field  string
	}{
		{DomainApp, "AppCount"},
		{DomainPassword, "MinimumLength"},
		{DomainSecurity, "EncryptionPolicy"},
		{DomainKiosk, "PackageNames"},
	} {
		if _, found := hasError(got, want.domain, want.field); !found {
			t.Errorf("期待した不正項目が収集されていない: domain=%s field=%s", want.domain, want.field)
		}
	}
}

// Req 6.3 / 6.4: 上限超過(business rule)と invalid field を機械可読に区別する
func TestValidate_DistinguishesErrorKinds(t *testing.T) {
	// Arrange: 上限超過（business rule）と範囲外（invalid field）を同時に発生させる
	in := PolicyInput{
		App:      &AppPolicy{AppCount: 5000},
		Password: &PasswordPolicy{MinimumLength: 100},
	}

	// Act
	got := Validate(in)

	// Assert
	appErr, ok := hasError(got, DomainApp, "AppCount")
	if !ok {
		t.Fatal("app 上限超過の検証エラーが無い")
	}
	if appErr.Kind != KindBusinessRule {
		t.Errorf("app Kind = %s, want %s", appErr.Kind, KindBusinessRule)
	}
	// Req 6.3 → errors.CodeBusinessRule(422) へマッピングされる
	if appErr.Kind.Code() != pkgerrors.CodeBusinessRule {
		t.Errorf("app Kind.Code() = %s, want %s", appErr.Kind.Code(), pkgerrors.CodeBusinessRule)
	}

	pwErr, ok := hasError(got, DomainPassword, "MinimumLength")
	if !ok {
		t.Fatal("password 範囲外の検証エラーが無い")
	}
	if pwErr.Kind != KindInvalidField {
		t.Errorf("password Kind = %s, want %s", pwErr.Kind, KindInvalidField)
	}
	// Req 6.4 → errors.CodeInvalidRequest(400) へマッピングされる
	if pwErr.Kind.Code() != pkgerrors.CodeInvalidRequest {
		t.Errorf("password Kind.Code() = %s, want %s", pwErr.Kind.Code(), pkgerrors.CodeInvalidRequest)
	}
}

// Req 6.5: すべて妥当なとき検証エラー 0 件の成功結果を返す
func TestValidate_AllValid_ReturnsSuccess(t *testing.T) {
	// Arrange: 5 領域すべて妥当な値
	in := PolicyInput{
		App:          &AppPolicy{AppCount: 3000},
		Password:     &PasswordPolicy{MinimumLength: 8},
		Security:     &SecurityPolicy{EncryptionPolicy: "ENABLED_WITH_PASSWORD", PasswordQuality: "COMPLEX"},
		SystemUpdate: &SystemUpdatePolicy{Type: "WINDOWED", StartMinutes: 120, EndMinutes: 240},
		Kiosk:        &KioskPolicy{PackageNames: []string{"com.example.kiosk", "jp.co.example_app.main"}},
	}

	// Act
	got := Validate(in)

	// Assert
	if !got.IsValid() {
		t.Fatalf("全妥当を期待したが検証エラーが返った (result=%+v)", got)
	}
	if len(got.Errors) != 0 {
		t.Errorf("Errors 件数 = %d, want 0", len(got.Errors))
	}
}

// NFR 1.1: 同一入力に対して常に同一結果を返す（決定論的）
func TestValidate_Deterministic(t *testing.T) {
	// Arrange
	in := PolicyInput{
		App:      &AppPolicy{AppCount: 3001},
		Kiosk:    &KioskPolicy{PackageNames: []string{"bad", "com.ok"}},
		Password: &PasswordPolicy{MinimumLength: 0},
	}

	// Act: 2 回呼び出す
	first := Validate(in)
	second := Validate(in)

	// Assert: エラー件数と各項目（domain/field/kind）が一致する
	if len(first.Errors) != len(second.Errors) {
		t.Fatalf("件数が一致しない: first=%d second=%d", len(first.Errors), len(second.Errors))
	}
	for i := range first.Errors {
		if first.Errors[i] != second.Errors[i] {
			t.Errorf("index %d で不一致: first=%+v second=%+v", i, first.Errors[i], second.Errors[i])
		}
	}
}

// nil 領域は検証対象から除外される（部分更新の許容 / 設計方針）
func TestValidate_NilDomains_AreSkipped(t *testing.T) {
	// Arrange: 全領域 nil（空の PolicyInput）
	in := PolicyInput{}

	// Act
	got := Validate(in)

	// Assert: 検証対象が無いため成功
	if !got.IsValid() {
		t.Fatalf("全領域 nil は成功を期待したが検証エラーが返った (result=%+v)", got)
	}
}
