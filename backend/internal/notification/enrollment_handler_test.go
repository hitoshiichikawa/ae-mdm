package notification

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
)

// fakeEnrollmentRegistrar は EnrollmentRegistrar port の spy。UpsertEnrolledDevice の呼び出し回数と
// 受け取った引数を記録し、任意の error を返せる。
type fakeEnrollmentRegistrar struct {
	err           error
	hit           int
	gotDevice     string
	gotMode       string
	gotCompStatus string
}

func (f *fakeEnrollmentRegistrar) UpsertEnrolledDevice(_ context.Context, amapiDeviceName, mode, complianceStatus string) error {
	f.hit++
	f.gotDevice = amapiDeviceName
	f.gotMode = mode
	f.gotCompStatus = complianceStatus
	return f.err
}

// enrollmentEnvelope は additionalData（tenant_id / mode）と softwareInfo.androidVersion を載せた
// 正常 wire-format の ENROLLMENT Envelope を作る helper（enrollmentTokenData に additionalData JSON
// 文字列を回送する design.md リスク 1 の wire-format を再現）。
func enrollmentEnvelope(tenantID uuid.UUID, mode, androidVersion string) Envelope {
	add := fmt.Sprintf(`{"tenant_id":%q,"issued_by":%q,"mode":%q}`,
		tenantID.String(), uuid.New().String(), mode)
	return envelopeWithRawFields(add, androidVersion)
}

// envelopeWithRawFields は enrollmentTokenData（additionalData 生文字列）と androidVersion を
// 任意に差し込んだ Envelope を作る helper（欠落 / parse 不能ケースを表現するため）。
func envelopeWithRawFields(rawAdditionalData, androidVersion string) Envelope {
	payload := fmt.Sprintf(
		`{"name":"enterprises/LC01/devices/d1","enrollmentTokenData":%q,"softwareInfo":{"androidVersion":%q}}`,
		rawAdditionalData, androidVersion)
	return Envelope{
		MessageID:        "msg-enroll-1",
		NotificationType: Enrollment,
		EnterpriseName:   "enterprises/LC01",
		Payload:          []byte(payload),
	}
}

// tenantCtx は Dispatcher が確立する tenant context（enterprise 由来テナント）を模して ctx に載せる。
func tenantCtx(tenantID uuid.UUID) context.Context {
	return db.WithTenantContext(context.Background(), db.TenantContext{TenantID: tenantID})
}

// TestEnrollmentHandler_Handle_TenantMatch_RegistersDeviceWithoutQuarantine は突合一致時に Registrar を
// 呼び、退避しないことを検証する（Req 3.1）。
func TestEnrollmentHandler_Handle_TenantMatch_RegistersDeviceWithoutQuarantine(t *testing.T) {
	// Arrange
	tenantID := uuid.New()
	reg := &fakeEnrollmentRegistrar{}
	unassigned := &fakeUnassigned{}
	h := NewEnrollmentHandler(reg, unassigned, &fakeLogger{})
	env := enrollmentEnvelope(tenantID, "dedicated", "11")

	// Act
	err := h.Handle(tenantCtx(tenantID), env)

	// Assert
	if err != nil {
		t.Fatalf("突合一致で error を返すべきでない: %v", err)
	}
	if reg.hit != 1 {
		t.Errorf("突合一致では Registrar を 1 回呼ぶべき（Req 3.1）: got %d", reg.hit)
	}
	if unassigned.enqueueHit != 0 {
		t.Errorf("突合一致では退避すべきでない: got %d", unassigned.enqueueHit)
	}
	if reg.gotDevice != "enterprises/LC01/devices/d1" {
		t.Errorf("amapi_device_name の写像 mismatch: got %q", reg.gotDevice)
	}
	if reg.gotMode != "dedicated" {
		t.Errorf("mode の写像 mismatch: want dedicated, got %q", reg.gotMode)
	}
	if reg.gotCompStatus != complianceUnknown {
		t.Errorf("compliance の写像 mismatch: want %q, got %q", complianceUnknown, reg.gotCompStatus)
	}
}

// TestEnrollmentHandler_Handle_TenantMismatch_QuarantinesWithoutRegister は additionalData.tenant_id と
// ctx 由来テナントの不一致時に退避のみ行い Registrar を呼ばないことを検証する（Req 3.2 / NFR 2.1 / NFR 2.2）。
func TestEnrollmentHandler_Handle_TenantMismatch_QuarantinesWithoutRegister(t *testing.T) {
	// Arrange
	ctxTenant := uuid.New()
	otherTenant := uuid.New() // additionalData に載る発行元テナント（ctx とは別）
	reg := &fakeEnrollmentRegistrar{}
	unassigned := &fakeUnassigned{}
	log := &fakeLogger{}
	h := NewEnrollmentHandler(reg, unassigned, log)
	env := enrollmentEnvelope(otherTenant, "fully_managed", "12")

	// Act
	err := h.Handle(tenantCtx(ctxTenant), env)

	// Assert
	if err != nil {
		t.Fatalf("退避経路は ack（nil）を返すべき: %v", err)
	}
	if reg.hit != 0 {
		t.Errorf("不一致では Registrar を呼ぶべきでない（NFR 2.1）: got %d", reg.hit)
	}
	if unassigned.enqueueHit != 1 {
		t.Errorf("不一致では退避を 1 回行うべき（Req 3.2）: got %d", unassigned.enqueueHit)
	}
	// NFR 4.1: 退避理由と enterprise_name を構造化 WARN で追跡できること（message_id は
	// logger.MessageID の zap.Field として常時付与される）。
	assertWarnField(t, log, "quarantine_reason", quarantineReasonTenantMismatch)
	assertWarnField(t, log, "enterprise_name", env.EnterpriseName)
}

// TestEnrollmentHandler_Handle_TenantMissing_Quarantines は additionalData の tenant_id 欠落 /
// parse 不能時に退避することを検証する（Req 3.3）。
func TestEnrollmentHandler_Handle_TenantMissing_Quarantines(t *testing.T) {
	tenantID := uuid.New()

	tests := []struct {
		name              string
		rawAdditionalData string
	}{
		{name: "tenant_id 欠落のとき退避する", rawAdditionalData: `{"issued_by":"x","mode":"fully_managed"}`},
		{name: "tenant_id 空文字のとき退避する", rawAdditionalData: `{"tenant_id":"","mode":"dedicated"}`},
		{name: "enrollmentTokenData 空のとき退避する", rawAdditionalData: ``},
		{name: "additionalData が JSON 不正のとき退避する", rawAdditionalData: `{not-json`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			reg := &fakeEnrollmentRegistrar{}
			unassigned := &fakeUnassigned{}
			h := NewEnrollmentHandler(reg, unassigned, &fakeLogger{})
			env := envelopeWithRawFields(tt.rawAdditionalData, "11")

			// Act
			err := h.Handle(tenantCtx(tenantID), env)

			// Assert
			if err != nil {
				t.Fatalf("退避経路は ack（nil）を返すべき: %v", err)
			}
			if reg.hit != 0 {
				t.Errorf("tenant_id 欠落では Registrar を呼ぶべきでない（Req 3.3）: got %d", reg.hit)
			}
			if unassigned.enqueueHit != 1 {
				t.Errorf("tenant_id 欠落では退避を 1 回行うべき（Req 3.3）: got %d", unassigned.enqueueHit)
			}
		})
	}
}

// TestEnrollmentHandler_Handle_MissingTenantContext_Quarantines は ctx tenant context 未確立時に
// 安全側で退避し Registrar を呼ばないことを検証する（NFR 2.2）。
func TestEnrollmentHandler_Handle_MissingTenantContext_Quarantines(t *testing.T) {
	// Arrange: additionalData は正常だが ctx に TenantContext を載せない。
	reg := &fakeEnrollmentRegistrar{}
	unassigned := &fakeUnassigned{}
	log := &fakeLogger{}
	h := NewEnrollmentHandler(reg, unassigned, log)
	env := enrollmentEnvelope(uuid.New(), "fully_managed", "11")

	// Act
	err := h.Handle(context.Background(), env)

	// Assert
	if err != nil {
		t.Fatalf("退避経路は ack（nil）を返すべき: %v", err)
	}
	if reg.hit != 0 {
		t.Errorf("ctx 未確立では Registrar を呼ぶべきでない（NFR 2.2）: got %d", reg.hit)
	}
	if unassigned.enqueueHit != 1 {
		t.Errorf("ctx 未確立では退避を 1 回行うべき（NFR 2.2）: got %d", unassigned.enqueueHit)
	}
	assertWarnField(t, log, "quarantine_reason", quarantineReasonTenantCtxMissing)
}

// TestEnrollmentHandler_Handle_MalformedPayload_Quarantines は payload 本体が JSON 不正のとき安全側で
// 退避し Registrar を呼ばないことを検証する（NFR 2.2 = テナント特定不能時は無更新）。
func TestEnrollmentHandler_Handle_MalformedPayload_Quarantines(t *testing.T) {
	// Arrange
	reg := &fakeEnrollmentRegistrar{}
	unassigned := &fakeUnassigned{}
	log := &fakeLogger{}
	h := NewEnrollmentHandler(reg, unassigned, log)
	env := Envelope{
		MessageID:        "msg-bad-payload",
		NotificationType: Enrollment,
		EnterpriseName:   "enterprises/LC01",
		Payload:          []byte(`{not valid json`),
	}

	// Act
	err := h.Handle(tenantCtx(uuid.New()), env)

	// Assert
	if err != nil {
		t.Fatalf("退避経路は ack（nil）を返すべき: %v", err)
	}
	if reg.hit != 0 {
		t.Errorf("payload 不正では Registrar を呼ぶべきでない（NFR 2.2）: got %d", reg.hit)
	}
	if unassigned.enqueueHit != 1 {
		t.Errorf("payload 不正では退避を 1 回行うべき（NFR 2.2）: got %d", unassigned.enqueueHit)
	}
	assertWarnField(t, log, "quarantine_reason", quarantineReasonPayloadInvalid)
}

// TestEnrollmentHandler_Handle_AndroidVersion_MapsCompliance は androidVersion から compliance_status を
// 決定することを検証する（Req 3.5 / NFR 1.1）。10 未満は unsupported、10 以上 / 判定不能は unknown。
func TestEnrollmentHandler_Handle_AndroidVersion_MapsCompliance(t *testing.T) {
	tenantID := uuid.New()

	tests := []struct {
		name           string
		androidVersion string
		wantCompliance string
	}{
		{name: "Android 9 は unsupported", androidVersion: "9", wantCompliance: complianceUnsupported},
		{name: "Android 8.1.0 は unsupported", androidVersion: "8.1.0", wantCompliance: complianceUnsupported},
		{name: "Android 10 は unknown（境界）", androidVersion: "10", wantCompliance: complianceUnknown},
		{name: "Android 11.0.0 は unknown", androidVersion: "11.0.0", wantCompliance: complianceUnknown},
		{name: "androidVersion 空は unknown（判定不能フォールバック）", androidVersion: "", wantCompliance: complianceUnknown},
		{name: "androidVersion 非数値は unknown（判定不能フォールバック）", androidVersion: "unknown-ver", wantCompliance: complianceUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			reg := &fakeEnrollmentRegistrar{}
			unassigned := &fakeUnassigned{}
			h := NewEnrollmentHandler(reg, unassigned, &fakeLogger{})
			env := enrollmentEnvelope(tenantID, "fully_managed", tt.androidVersion)

			// Act
			err := h.Handle(tenantCtx(tenantID), env)

			// Assert
			if err != nil {
				t.Fatalf("突合一致で error を返すべきでない: %v", err)
			}
			if reg.hit != 1 {
				t.Fatalf("突合一致では Registrar を 1 回呼ぶべき: got %d", reg.hit)
			}
			if reg.gotCompStatus != tt.wantCompliance {
				t.Errorf("compliance mismatch: want %q, got %q", tt.wantCompliance, reg.gotCompStatus)
			}
		})
	}
}

// TestEnrollmentHandler_Handle_RegistrarTransientError_ReturnsNack は Registrar の transient 失敗を
// そのまま返し nack（再処理保持）へ写像することを検証する（Req 3.6）。
func TestEnrollmentHandler_Handle_RegistrarTransientError_ReturnsNack(t *testing.T) {
	// Arrange
	tenantID := uuid.New()
	reg := &fakeEnrollmentRegistrar{err: transientErr()}
	unassigned := &fakeUnassigned{}
	h := NewEnrollmentHandler(reg, unassigned, &fakeLogger{})
	env := enrollmentEnvelope(tenantID, "fully_managed", "11")

	// Act
	err := h.Handle(tenantCtx(tenantID), env)

	// Assert
	if err == nil {
		t.Fatalf("transient 登録失敗では error を返すべき（Req 3.6）")
	}
	if pkgerrors.ShouldAck(err, nil) {
		t.Errorf("transient 登録失敗は nack（ShouldAck=false）であるべき（Req 3.6）: err=%v", err)
	}
	if reg.hit != 1 {
		t.Errorf("Registrar を 1 回呼ぶべき: got %d", reg.hit)
	}
	if unassigned.enqueueHit != 0 {
		t.Errorf("登録失敗では退避すべきでない: got %d", unassigned.enqueueHit)
	}
}

// TestEnrollmentHandler_Handle_WarnLogsDoNotLeakSecrets は退避 / サポート対象外 / 登録失敗の各 WARN に
// payload 生値・additionalData 生値・tenant_id が混入しないことを検証する（NFR 4.1 / NFR 3.1）。
func TestEnrollmentHandler_Handle_WarnLogsDoNotLeakSecrets(t *testing.T) {
	const secret = "SENSITIVE_ADD_DATA_XYZ"

	tests := []struct {
		name         string
		ctxTenant    uuid.UUID
		addTenant    uuid.UUID
		android      string
		registrarErr error
	}{
		{name: "退避（不一致）の WARN", ctxTenant: uuid.New(), addTenant: uuid.New(), android: "11"},
		{name: "サポート対象外記録の WARN", android: "9"},                           // ctx==add で一致 / android<10
		{name: "登録失敗の WARN", android: "11", registrarErr: transientErr()}, // ctx==add で一致
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange: 一致ケースは ctx と additionalData のテナントを揃える。
			ctxTenant := tt.ctxTenant
			addTenant := tt.addTenant
			if addTenant == uuid.Nil {
				addTenant = uuid.New()
				ctxTenant = addTenant
			}
			// additionalData に機密マーカーを埋め込む（tenant_id 生値も含めて WARN 非混入を確認）。
			add := fmt.Sprintf(`{"tenant_id":%q,"issued_by":%q,"mode":"fully_managed","secret":%q}`,
				addTenant.String(), uuid.New().String(), secret)
			env := envelopeWithRawFields(add, tt.android)

			reg := &fakeEnrollmentRegistrar{err: tt.registrarErr}
			unassigned := &fakeUnassigned{}
			log := &fakeLogger{}
			h := NewEnrollmentHandler(reg, unassigned, log)

			// Act
			_ = h.Handle(tenantCtx(ctxTenant), env)

			// Assert: WARN が 1 件以上あり、機密値（payload 生値 / additionalData 生値 / tenant_id）を含まない。
			assertNoSecretInWarns(t, log,
				secret,              // additionalData の機密マーカー
				string(env.Payload), // payload 生値
				add,                 // additionalData 生文字列
				addTenant.String(),  // additionalData の tenant_id 生値
			)
		})
	}
}

// TestEnrollmentHandler_Handle_InvalidMode_QuarantinesWithoutRegister は additionalData.mode が
// devices.mode enum（fully_managed / dedicated）の値域外のとき、DB enum エラーによる再処理ループ
// （毒メッセージ）を避けて安全側で退避し、Registrar を呼ばず ack することを検証する（Req 3.6 の
// transient 限定整合 / PR #67 reviewer 指摘）。
func TestEnrollmentHandler_Handle_InvalidMode_QuarantinesWithoutRegister(t *testing.T) {
	tenantID := uuid.New()

	tests := []struct {
		name string
		mode string
	}{
		{name: "mode 欠落のとき退避する", mode: ""},
		{name: "mode が未知の値のとき退避する", mode: "kiosk"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange: tenant は一致させ、mode のみ不正値にする。
			reg := &fakeEnrollmentRegistrar{}
			unassigned := &fakeUnassigned{}
			log := &fakeLogger{}
			h := NewEnrollmentHandler(reg, unassigned, log)
			env := enrollmentEnvelope(tenantID, tt.mode, "11")

			// Act
			err := h.Handle(tenantCtx(tenantID), env)

			// Assert
			if err != nil {
				t.Fatalf("退避経路は ack（nil）を返すべき: %v", err)
			}
			if reg.hit != 0 {
				t.Errorf("不正 mode では Registrar を呼ぶべきでない（毒メッセージ防止）: got %d", reg.hit)
			}
			if unassigned.enqueueHit != 1 {
				t.Errorf("不正 mode では退避を 1 回行うべき: got %d", unassigned.enqueueHit)
			}
			assertWarnField(t, log, "quarantine_reason", quarantineReasonModeInvalid)
		})
	}
}

// TestEnrollmentHandler_Handle_InvalidDeviceName_QuarantinesWithoutRegister は payload.name が AMAPI
// device リソース名（enterprises/{ent}/devices/{dev}）の形式でないとき、実端末でない junk 登録を避けて
// 退避し、Registrar を呼ばず ack することを検証する（NFR 2.2 / PR #67 reviewer 指摘）。
func TestEnrollmentHandler_Handle_InvalidDeviceName_QuarantinesWithoutRegister(t *testing.T) {
	tenantID := uuid.New()

	tests := []struct {
		name       string
		deviceName string
	}{
		{name: "name 欠落のとき退避する", deviceName: ""},
		{name: "name が enterprise だけのとき退避する", deviceName: "enterprises/LC01"},
		{name: "name の device セグメントが空のとき退避する", deviceName: "enterprises/LC01/devices/"},
		{name: "name の enterprise セグメントが空のとき退避する", deviceName: "enterprises//devices/d1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange: additionalData（tenant 一致 / mode 正常）は保ち、payload.name のみ不正にする。
			add := fmt.Sprintf(`{"tenant_id":%q,"issued_by":%q,"mode":"fully_managed"}`,
				tenantID.String(), uuid.New().String())
			payload := fmt.Sprintf(
				`{"name":%q,"enrollmentTokenData":%q,"softwareInfo":{"androidVersion":"11"}}`,
				tt.deviceName, add)
			env := Envelope{
				MessageID:        "msg-bad-name",
				NotificationType: Enrollment,
				EnterpriseName:   "enterprises/LC01",
				Payload:          []byte(payload),
			}
			reg := &fakeEnrollmentRegistrar{}
			unassigned := &fakeUnassigned{}
			log := &fakeLogger{}
			h := NewEnrollmentHandler(reg, unassigned, log)

			// Act
			err := h.Handle(tenantCtx(tenantID), env)

			// Assert
			if err != nil {
				t.Fatalf("退避経路は ack（nil）を返すべき: %v", err)
			}
			if reg.hit != 0 {
				t.Errorf("不正 device name では Registrar を呼ぶべきでない（junk 登録防止）: got %d", reg.hit)
			}
			if unassigned.enqueueHit != 1 {
				t.Errorf("不正 device name では退避を 1 回行うべき: got %d", unassigned.enqueueHit)
			}
			assertWarnField(t, log, "quarantine_reason", quarantineReasonDeviceNameInvalid)
		})
	}
}

// TestEnrollmentHandler_Handle_WarnLogsIncludeDeviceName は登録失敗 / サポート対象外記録 / 退避（tenant
// 特定後）の各 WARN に対象端末（amapi_device_name）が載り、運用者が対象端末を事後追跡できることを
// 検証する（NFR 4.1 / PR #67 reviewer 指摘）。
func TestEnrollmentHandler_Handle_WarnLogsIncludeDeviceName(t *testing.T) {
	const deviceName = "enterprises/LC01/devices/d1" // enrollmentEnvelope / envelopeWithRawFields が固定で載せる name
	ctxTenant := uuid.New()
	otherTenant := uuid.New()

	tests := []struct {
		name         string
		addTenant    uuid.UUID
		android      string
		registrarErr error
	}{
		{name: "登録失敗の WARN に device name", addTenant: ctxTenant, android: "11", registrarErr: transientErr()},
		{name: "サポート対象外記録の WARN に device name", addTenant: ctxTenant, android: "9"},
		{name: "退避（不一致）の WARN に device name", addTenant: otherTenant, android: "11"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			reg := &fakeEnrollmentRegistrar{err: tt.registrarErr}
			unassigned := &fakeUnassigned{}
			log := &fakeLogger{}
			h := NewEnrollmentHandler(reg, unassigned, log)
			env := enrollmentEnvelope(tt.addTenant, "fully_managed", tt.android)

			// Act
			_ = h.Handle(tenantCtx(ctxTenant), env)

			// Assert
			assertWarnField(t, log, "amapi_device_name", deviceName)
		})
	}
}

// assertWarnField は WARN 呼び出し列に string の key=want field が含まれることを検証する（NFR 4.1 の正検証）。
func assertWarnField(t *testing.T, log *fakeLogger, key, want string) {
	t.Helper()
	for _, call := range log.warnCalls {
		if got, ok := warnStringField(call.fields, key); ok && got == want {
			return
		}
	}
	t.Errorf("WARN ログに %s=%q が存在しない; warnCalls=%+v", key, want, log.warnCalls)
}

// warnStringField は logger の (...any) field 列から string の key=value を取り出す。
//
// logger.toZapFields の slot 解釈（zap.Field / error は 1 slot、"key",value ペアは 2 slot）に一致させ、
// 先頭に zap.Field（例: logger.MessageID）が混在してもペアのずれなく key を走査できるようにする。
func warnStringField(fields []any, key string) (string, bool) {
	i := 0
	for i < len(fields) {
		s, isStr := fields[i].(string)
		if !isStr {
			i++ // zap.Field / error は 1 slot
			continue
		}
		if i+1 >= len(fields) {
			i++
			continue
		}
		if s == key {
			if v, ok := fields[i+1].(string); ok {
				return v, true
			}
		}
		i += 2
	}
	return "", false
}

// assertNoSecretInWarns は WARN の msg / 全 field 値に機密値 secrets が混入しないこと、かつ WARN が
// 1 件以上出力されていること（NFR 4.1）を検証する。
func assertNoSecretInWarns(t *testing.T, log *fakeLogger, secrets ...string) {
	t.Helper()
	if len(log.warnCalls) == 0 {
		t.Fatalf("WARN ログが 1 件も出力されていない（NFR 4.1）")
	}
	for _, call := range log.warnCalls {
		for _, secret := range secrets {
			if secret == "" {
				continue
			}
			if strings.Contains(call.msg, secret) {
				t.Errorf("WARN msg に機密値 %q が混入: %q", secret, call.msg)
			}
			for _, f := range call.fields {
				if rendered := fmt.Sprintf("%v", f); strings.Contains(rendered, secret) {
					t.Errorf("WARN field に機密値 %q が混入: %v", secret, f)
				}
			}
		}
	}
}
