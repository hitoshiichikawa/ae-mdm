package tenant

import "testing"

// nullableString は enterprise_name / signup_url_name の「空文字 → NULL（nil）」写像を
// 担う DB 非依存ヘルパ。Insert の引数化で nullable 列へ書く正本のため、in-package で
// 写像挙動を検証する（affected rows 等の実 SQL 挙動は task 7.1 の integration test へ deferred）。

// TestNullableString_空文字はnilへ写像 は、空入力が NULL（nil）として書かれることを検証する（Req 3.1）。
func TestNullableString_空文字はnilへ写像する(t *testing.T) {
	// Arrange
	input := ""

	// Act
	got := nullableString(input)

	// Assert
	if got != nil {
		t.Fatalf("空文字は nil（NULL）へ写像されるべきだが、非 nil の %q が返った", *got)
	}
}

// TestNullableString_非空は値ポインタへ写像 は、非空入力がそのまま *string で返ることを検証する（Req 3.1）。
func TestNullableString_非空は値ポインタへ写像する(t *testing.T) {
	// Arrange
	input := "signup-xyz"

	// Act
	got := nullableString(input)

	// Assert
	if got == nil {
		t.Fatal("非空文字は非 nil の *string へ写像されるべきだが、nil が返った")
	}
	if *got != input {
		t.Fatalf("写像された値が入力と一致しない: got=%q want=%q", *got, input)
	}
}
