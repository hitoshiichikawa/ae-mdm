package notification

import (
	stderrors "errors"
	"testing"
	"time"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/pubsub"
)

// deviceResourcePayload は ENROLLMENT / STATUS_REPORT 通知が運ぶ Device リソースの最小 JSON。
// name は `enterprises/{enterpriseId}/devices/{deviceId}` 形式（AMAPI wire-format）。
const deviceResourcePayload = `{"name":"enterprises/LC0123abcd/devices/dev-1"}`

// newMessage は attribute（種別）と payload を持つ *pubsub.Message を組み立てるテスト helper。
func newMessage(id, notifType, payload string) *pubsub.Message {
	attrs := map[string]string{}
	if notifType != "" {
		attrs[notificationTypeAttribute] = notifType
	}
	return &pubsub.Message{
		ID:          id,
		Data:        []byte(payload),
		Attributes:  attrs,
		PublishTime: time.Date(2026, 6, 29, 12, 0, 0, 0, time.UTC),
	}
}

// assertDiscardError は err が破棄 ack 相当（*errors.Error{IsTransient:false}）であることを検証する。
func assertDiscardError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("破棄相当のエラーを返すべき: got nil")
	}
	var de *pkgerrors.Error
	if !stderrors.As(err, &de) {
		t.Fatalf("*errors.Error で分類されるべき: got %T (%v)", err, err)
	}
	if de.IsTransient {
		t.Fatalf("破棄 ack 相当のため IsTransient は false であるべき: got true (%v)", de)
	}
}

// TestParse_EachNotificationTypeProducesEnvelope は各種別の正常 parse で、種別・MessageID・
// enterprise_name・payload・PublishTime が Envelope に正しく写像されることを検証する
// （Req 2.1〜2.3 の種別抽出 / design.md Verifier Postconditions）。
func TestParse_EachNotificationTypeProducesEnvelope(t *testing.T) {
	v := NewVerifier(nil)

	tests := []struct {
		name      string
		notifType string
		want      NotificationType
	}{
		{name: "ENROLLMENT 種別を Envelope に写像する", notifType: "ENROLLMENT", want: Enrollment},
		{name: "STATUS_REPORT 種別を Envelope に写像する", notifType: "STATUS_REPORT", want: StatusReport},
		{name: "COMMAND 種別を Envelope に写像する", notifType: "COMMAND", want: Command},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			msg := newMessage("msg-1", tt.notifType, deviceResourcePayload)

			// Act
			env, err := v.Parse(msg)

			// Assert
			if err != nil {
				t.Fatalf("正常 payload で error を返すべきでない: %v", err)
			}
			if env.NotificationType != tt.want {
				t.Errorf("NotificationType mismatch: want %q, got %q", tt.want, env.NotificationType)
			}
			if env.MessageID != "msg-1" {
				t.Errorf("MessageID mismatch: want %q, got %q", "msg-1", env.MessageID)
			}
			if env.EnterpriseName != "enterprises/LC0123abcd" {
				t.Errorf("EnterpriseName mismatch: want %q, got %q", "enterprises/LC0123abcd", env.EnterpriseName)
			}
			if string(env.Payload) != deviceResourcePayload {
				t.Errorf("Payload は元の生 bytes をそのまま運ぶべき: got %q", string(env.Payload))
			}
			if !env.PublishTime.Equal(msg.PublishTime) {
				t.Errorf("PublishTime mismatch: want %v, got %v", msg.PublishTime, env.PublishTime)
			}
		})
	}
}

// TestParse_EmptyPayloadIsDiscarded は空 payload（nil / 長さ 0）が破棄 ack 相当で分類される
// ことを検証する（Req 2.6 = 空 payload は dispatch せず破棄）。
func TestParse_EmptyPayloadIsDiscarded(t *testing.T) {
	v := NewVerifier(nil)

	tests := []struct {
		name string
		data []byte
	}{
		{name: "nil payload は破棄される", data: nil},
		{name: "長さ 0 payload は破棄される", data: []byte{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			msg := &pubsub.Message{
				ID:         "msg-empty",
				Data:       tt.data,
				Attributes: map[string]string{notificationTypeAttribute: "ENROLLMENT"},
			}

			// Act
			_, err := v.Parse(msg)

			// Assert
			assertDiscardError(t, err)
		})
	}
}

// TestParse_UndeterminableTypeIsDiscarded は種別 attribute の欠落 / 空値が「種別判定不能」と
// して破棄 ack 相当で分類されることを検証する（Req 2.6 = 種別判定不能は破棄）。
func TestParse_UndeterminableTypeIsDiscarded(t *testing.T) {
	v := NewVerifier(nil)

	tests := []struct {
		name  string
		attrs map[string]string
	}{
		{name: "種別 attribute が欠落しているとき破棄される", attrs: map[string]string{}},
		{name: "種別 attribute が空文字のとき破棄される", attrs: map[string]string{notificationTypeAttribute: ""}},
		{name: "種別 attribute が空白のみのとき破棄される", attrs: map[string]string{notificationTypeAttribute: "   "}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			msg := &pubsub.Message{
				ID:         "msg-notype",
				Data:       []byte(deviceResourcePayload),
				Attributes: tt.attrs,
			}

			// Act
			_, err := v.Parse(msg)

			// Assert
			assertDiscardError(t, err)
		})
	}
}

// TestParse_InvalidJSONPayloadFailsVerification は JSON として解釈できない payload が検証失敗
// として破棄 ack 相当で分類されることを検証する（Req 2.5 = 検証失敗は破棄）。
func TestParse_InvalidJSONPayloadFailsVerification(t *testing.T) {
	// Arrange
	v := NewVerifier(nil)
	msg := newMessage("msg-badjson", "ENROLLMENT", `{not-json`)

	// Act
	_, err := v.Parse(msg)

	// Assert
	assertDiscardError(t, err)
}

// TestParse_NilMessageFailsVerification は nil メッセージが検証失敗として破棄 ack 相当で
// 分類されることを検証する（Req 2.5 / 防御的境界）。
func TestParse_NilMessageFailsVerification(t *testing.T) {
	// Arrange
	v := NewVerifier(nil)

	// Act
	_, err := v.Parse(nil)

	// Assert
	assertDiscardError(t, err)
}

// TestParse_EmptyEnterpriseNamePassesThroughWithoutError は enterprise_name を payload から
// 抽出できない場合（name 欠落 / 接頭辞不一致）でも、Verifier はエラーにせず空文字のまま
// Envelope を通すことを検証する（Req 3.4 = 退避判定は Dispatcher の責務）。
func TestParse_EmptyEnterpriseNamePassesThroughWithoutError(t *testing.T) {
	v := NewVerifier(nil)

	tests := []struct {
		name    string
		payload string
	}{
		{name: "name field が無い payload は enterprise_name 空で通す", payload: `{"state":"ACTIVE"}`},
		{name: "name が enterprises/ 接頭辞でない payload は enterprise_name 空で通す", payload: `{"name":"signupUrls/abc"}`},
		{name: "enterprises/ 直後が空（enterprise ID なし）の payload は enterprise_name 空で通す", payload: `{"name":"enterprises/"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			msg := newMessage("msg-noent", "STATUS_REPORT", tt.payload)

			// Act
			env, err := v.Parse(msg)

			// Assert
			if err != nil {
				t.Fatalf("enterprise_name 不明はエラーにせず通すべき: %v", err)
			}
			if env.EnterpriseName != "" {
				t.Errorf("enterprise_name は空文字であるべき: got %q", env.EnterpriseName)
			}
			if env.NotificationType != StatusReport {
				t.Errorf("種別は維持されるべき: want %q, got %q", StatusReport, env.NotificationType)
			}
		})
	}
}

// TestEnterpriseNameFromResourceName はリソース名接頭辞からの enterprise_name 抽出（純粋関数）を
// 正常系・境界系で検証する（Req 3.1 経路の enterprise_name 抽出契約 / Req 3.4 の空写像）。
func TestEnterpriseNameFromResourceName(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "devices サブパス付きから enterprise 接頭辞を切り出す", in: "enterprises/LC01/devices/d1", want: "enterprises/LC01"},
		{name: "enterprise リソース単体はそのまま返す", in: "enterprises/LC01", want: "enterprises/LC01"},
		{name: "前後空白を trim してから判定する", in: "  enterprises/LC02/devices/d2  ", want: "enterprises/LC02"},
		{name: "空文字は空文字を返す", in: "", want: ""},
		{name: "接頭辞不一致は空文字を返す", in: "signupUrls/x", want: ""},
		{name: "enterprises/ 直後が空なら空文字を返す", in: "enterprises/", want: ""},
		{name: "enterprises/ 直後が / なら enterprise ID 空で空文字を返す", in: "enterprises//devices/d", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange / Act
			got := enterpriseNameFromResourceName(tt.in)

			// Assert
			if got != tt.want {
				t.Errorf("enterpriseNameFromResourceName(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
