package config

import (
	stdErrors "errors"
	"strings"
	"testing"
	"time"

	internalerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// validEnv は正常系のテスト fixture。必須 env をすべて埋めた map を返す。
func validEnv() map[string]string {
	return map[string]string{
		"DATABASE_URL":                   "postgres://app_user:pw@localhost:5432/ae_mdm?sslmode=disable",
		"OIDC_TENANT_ISSUER_URL":         "http://keycloak:8080/realms/tenant",
		"OIDC_TENANT_CLIENT_ID":          "tenant-console",
		"OIDC_TENANT_REDIRECT_URL":       "http://localhost:8080/api/auth/callback",
		"OIDC_TENANT_CLIENT_SECRET":      "tenant-secret-for-tests-do-not-use-in-prod-1",
		"OIDC_ADMIN_ISSUER_URL":          "http://keycloak:8080/realms/admin",
		"OIDC_ADMIN_CLIENT_ID":           "admin-console",
		"OIDC_ADMIN_REDIRECT_URL":        "http://localhost:8080/api/admin/auth/callback",
		"OIDC_ADMIN_CLIENT_SECRET":       "admin-secret-for-tests-do-not-use-in-prod-2",
		"PUBSUB_PROJECT_ID":              "ae-mdm-local",
		"PUBSUB_TOPIC":                   "amapi-notifications",
		"PUBSUB_SUBSCRIPTION":            "amapi-notifications-sub",
		"AMAPI_PROJECT_ID":               "ae-mdm-local",
		"GOOGLE_APPLICATION_CREDENTIALS": "/secrets/sa.json",
		"SESSION_SECRET":                 "this-is-a-32-char-secret-okk!!!!",
		"STATE_MAC_SECRET":               "this-is-a-32-byte-mac-secret-ok!",
	}
}

// envGetterFromMap は map ベースの envGetter を返す（テスト専用）。
func envGetterFromMap(m map[string]string) envGetter {
	return func(key string) (string, bool) {
		v, ok := m[key]
		return v, ok
	}
}

func TestLoad_AllRequiredPresent_ReturnsConfig(t *testing.T) {
	// Arrange
	getter := envGetterFromMap(validEnv())

	// Act
	cfg, err := loadFrom(getter)

	// Assert
	if err != nil {
		t.Fatalf("loadFrom returned error: %v", err)
	}
	if cfg.DatabaseURL == "" {
		t.Fatalf("DatabaseURL is empty")
	}
	if cfg.SessionSecret == "" {
		t.Fatalf("SessionSecret is empty")
	}
}

func TestLoad_MissingRequired_ReturnsConfigInvalid(t *testing.T) {
	// Arrange: DATABASE_URL を欠落させる
	env := validEnv()
	delete(env, "DATABASE_URL")

	// Act
	_, err := loadFrom(envGetterFromMap(env))

	// Assert
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var de *internalerrors.Error
	if !stdErrors.As(err, &de) || de.Code != internalerrors.CodeConfigInvalid {
		t.Fatalf("expected CodeConfigInvalid, got %v", err)
	}
	if !strings.Contains(de.Message, "DATABASE_URL") {
		t.Fatalf("error message should mention DATABASE_URL, got %q", de.Message)
	}
}

func TestLoad_InvalidIntFormat_ReturnsConfigInvalid(t *testing.T) {
	// Arrange
	env := validEnv()
	env["AUDIT_LOG_RETENTION_DAYS"] = "not-a-number"

	// Act
	_, err := loadFrom(envGetterFromMap(env))

	// Assert
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var de *internalerrors.Error
	if !stdErrors.As(err, &de) || de.Code != internalerrors.CodeConfigInvalid {
		t.Fatalf("expected CodeConfigInvalid, got %v", err)
	}
	if !strings.Contains(de.Message, "AUDIT_LOG_RETENTION_DAYS") {
		t.Fatalf("error message should mention AUDIT_LOG_RETENTION_DAYS, got %q", de.Message)
	}
}

// TestLoad_InvalidURL_ReturnsConfigInvalid は Req 1.3 のうち「不正フォーマット」検出を
// URL 形式の env で確認する（PR #31 round-3 review 由来）。scheme / host 欠落および
// 許可されない scheme のいずれも fail-fast で `CodeConfigInvalid` を返す。
func TestLoad_InvalidURL_ReturnsConfigInvalid(t *testing.T) {
	cases := []struct {
		name     string
		key      string
		value    string
		wantHint string
	}{
		{
			name:     "DATABASE_URL: scheme なし",
			key:      "DATABASE_URL",
			value:    "app_user:pw@localhost:5432/ae_mdm",
			wantHint: "DATABASE_URL",
		},
		{
			name:     "DATABASE_URL: 許可されない scheme",
			key:      "DATABASE_URL",
			value:    "mysql://app_user:pw@localhost:5432/ae_mdm",
			wantHint: "DATABASE_URL",
		},
		{
			name:     "OIDC_TENANT_ISSUER_URL: host なし",
			key:      "OIDC_TENANT_ISSUER_URL",
			value:    "http://",
			wantHint: "OIDC_TENANT_ISSUER_URL",
		},
		{
			name:     "OIDC_ADMIN_REDIRECT_URL: 許可されない scheme",
			key:      "OIDC_ADMIN_REDIRECT_URL",
			value:    "ftp://example.com/cb",
			wantHint: "OIDC_ADMIN_REDIRECT_URL",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			// Arrange
			env := validEnv()
			env[c.key] = c.value
			// Act
			_, err := loadFrom(envGetterFromMap(env))
			// Assert
			if err == nil {
				t.Fatalf("%s=%q を渡したが error なし", c.key, c.value)
			}
			var de *internalerrors.Error
			if !stdErrors.As(err, &de) || de.Code != internalerrors.CodeConfigInvalid {
				t.Fatalf("expected CodeConfigInvalid, got %v", err)
			}
			if !strings.Contains(de.Message, c.wantHint) {
				t.Fatalf("message should mention %q; got %q", c.wantHint, de.Message)
			}
		})
	}
}

func TestLoad_SessionSecretTooShort_ReturnsConfigInvalid(t *testing.T) {
	// Arrange
	env := validEnv()
	env["SESSION_SECRET"] = "short"

	// Act
	_, err := loadFrom(envGetterFromMap(env))

	// Assert
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var de *internalerrors.Error
	if !stdErrors.As(err, &de) || de.Code != internalerrors.CodeConfigInvalid {
		t.Fatalf("expected CodeConfigInvalid, got %v", err)
	}
	if !strings.Contains(de.Message, "SESSION_SECRET") {
		t.Fatalf("error message should mention SESSION_SECRET, got %q", de.Message)
	}
}

func TestLoad_AppliesDefaults_WhenOptionalEnvAbsent(t *testing.T) {
	// Arrange: 必須のみ。optional は付けない
	env := validEnv()
	// 念のため default 対象の key が含まれていないことを確認
	delete(env, "AUDIT_LOG_RETENTION_DAYS")
	delete(env, "DEVICE_SYNC_DELAY_THRESHOLD_HOURS")
	delete(env, "LOG_LEVEL")
	delete(env, "LOG_FORMAT")
	delete(env, "LOG_OUTPUT")
	delete(env, "HTTP_LISTEN_ADDR")
	delete(env, "SESSION_IDLE_TIMEOUT")
	delete(env, "SESSION_ABSOLUTE_TIMEOUT")
	delete(env, "STATE_COOKIE_TTL")

	// Act
	cfg, err := loadFrom(envGetterFromMap(env))

	// Assert
	if err != nil {
		t.Fatalf("loadFrom returned error: %v", err)
	}
	if cfg.AuditLogRetentionDays != 180 {
		t.Errorf("AuditLogRetentionDays = %d, want 180", cfg.AuditLogRetentionDays)
	}
	if cfg.DeviceSyncDelayThresholdHours != 24 {
		t.Errorf("DeviceSyncDelayThresholdHours = %d, want 24", cfg.DeviceSyncDelayThresholdHours)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("LogLevel = %q, want \"info\"", cfg.LogLevel)
	}
	if cfg.LogFormat != "json" {
		t.Errorf("LogFormat = %q, want \"json\"", cfg.LogFormat)
	}
	if cfg.LogOutput != "stderr" {
		t.Errorf("LogOutput = %q, want \"stderr\"", cfg.LogOutput)
	}
	if cfg.HTTPListenAddr != ":8080" {
		t.Errorf("HTTPListenAddr = %q, want \":8080\"", cfg.HTTPListenAddr)
	}
	// MigrateDatabaseURL は DATABASE_URL にフォールバック
	if cfg.MigrateDatabaseURL != cfg.DatabaseURL {
		t.Errorf("MigrateDatabaseURL should fallback to DATABASE_URL: got %q vs %q",
			cfg.MigrateDatabaseURL, cfg.DatabaseURL)
	}
	// Issue #33 tasks.md 1.1 (a): 3 つの duration が既定値
	if cfg.SessionIdleTimeout != 30*time.Minute {
		t.Errorf("SessionIdleTimeout = %s, want 30m", cfg.SessionIdleTimeout)
	}
	if cfg.SessionAbsoluteTimeout != 8*time.Hour {
		t.Errorf("SessionAbsoluteTimeout = %s, want 8h", cfg.SessionAbsoluteTimeout)
	}
	if cfg.StateCookieTTL != 10*time.Minute {
		t.Errorf("StateCookieTTL = %s, want 10m", cfg.StateCookieTTL)
	}
}

func TestLoad_OverridesDefaults_WhenOptionalEnvSet(t *testing.T) {
	// Arrange
	env := validEnv()
	env["AUDIT_LOG_RETENTION_DAYS"] = "365"
	env["LOG_LEVEL"] = "debug"
	env["LOG_FORMAT"] = "console"
	env["HTTP_LISTEN_ADDR"] = ":9090"
	env["MIGRATE_DATABASE_URL"] = "postgres://migration_user@localhost:5432/ae_mdm"

	// Act
	cfg, err := loadFrom(envGetterFromMap(env))

	// Assert
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AuditLogRetentionDays != 365 {
		t.Errorf("AuditLogRetentionDays = %d, want 365", cfg.AuditLogRetentionDays)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}
	if cfg.LogFormat != "console" {
		t.Errorf("LogFormat = %q, want console", cfg.LogFormat)
	}
	if cfg.HTTPListenAddr != ":9090" {
		t.Errorf("HTTPListenAddr = %q, want :9090", cfg.HTTPListenAddr)
	}
	if cfg.MigrateDatabaseURL == cfg.DatabaseURL {
		t.Errorf("MIGRATE_DATABASE_URL override did not take effect")
	}
}

// TestLoad_FromOSEnv_Smoke は os.LookupEnv を使う公開 API Load を t.Setenv で動かす smoke test。
// 主要な分岐は loadFrom テストで検証済みのため、ここでは Load -> loadFrom の配線確認のみ。
func TestLoad_FromOSEnv_Smoke(t *testing.T) {
	for k, v := range validEnv() {
		t.Setenv(k, v)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if cfg.DatabaseURL == "" {
		t.Fatalf("DatabaseURL is empty after Load")
	}
}

// assertConfigInvalid は loadFrom が *errors.Error{Code: CodeConfigInvalid} を返し、
// message に期待する key 名を含むことを確認する共通 helper。
func assertConfigInvalid(t *testing.T, env map[string]string, wantHint string) {
	t.Helper()
	_, err := loadFrom(envGetterFromMap(env))
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var de *internalerrors.Error
	if !stdErrors.As(err, &de) || de.Code != internalerrors.CodeConfigInvalid {
		t.Fatalf("expected CodeConfigInvalid, got %v", err)
	}
	if wantHint != "" && !strings.Contains(de.Message, wantHint) {
		t.Fatalf("error message should mention %q, got %q", wantHint, de.Message)
	}
}

// --- Issue #33 task 1.1 追加テスト群 -------------------------------------------------
//
// (a) default 値適用は TestLoad_AppliesDefaults_WhenOptionalEnvAbsent で検証済み。
// (b)-(l) を以下のテストで網羅する。
//
// テストは AAA（Arrange / Act / Assert）パターンを保ちつつ、複数 fixture を共通化する
// 部分のみ table-driven にする。

// TestLoad_StateMACSecret_MissingOrTooShort は (b)。
// STATE_MAC_SECRET 未設定 / 32 バイト未満で CodeConfigInvalid。
func TestLoad_StateMACSecret_MissingOrTooShort(t *testing.T) {
	cases := []struct {
		name  string
		value string // 空文字 = 削除
	}{
		{name: "missing", value: ""},
		{name: "too short 31 bytes", value: strings.Repeat("a", 31)},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			// Arrange
			env := validEnv()
			if c.value == "" {
				delete(env, "STATE_MAC_SECRET")
			} else {
				env["STATE_MAC_SECRET"] = c.value
			}
			// Act + Assert
			assertConfigInvalid(t, env, "STATE_MAC_SECRET")
		})
	}
}

// TestLoad_OIDCClientSecret_Missing は (c)。
// OIDC_TENANT_CLIENT_SECRET / OIDC_ADMIN_CLIENT_SECRET 未設定で CodeConfigInvalid。
func TestLoad_OIDCClientSecret_Missing(t *testing.T) {
	cases := []struct {
		name string
		key  string
	}{
		{name: "tenant client secret missing", key: "OIDC_TENANT_CLIENT_SECRET"},
		{name: "admin client secret missing", key: "OIDC_ADMIN_CLIENT_SECRET"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			// Arrange
			env := validEnv()
			delete(env, c.key)
			// Act + Assert
			assertConfigInvalid(t, env, c.key)
		})
	}
}

// TestLoad_DurationInvalidFormat は (d)。
// duration 不正フォーマットで CodeConfigInvalid。
func TestLoad_DurationInvalidFormat(t *testing.T) {
	cases := []struct {
		name  string
		key   string
		value string
	}{
		{name: "SESSION_IDLE_TIMEOUT garbage", key: "SESSION_IDLE_TIMEOUT", value: "not-a-duration"},
		{name: "SESSION_ABSOLUTE_TIMEOUT garbage", key: "SESSION_ABSOLUTE_TIMEOUT", value: "abc"},
		{name: "STATE_COOKIE_TTL garbage", key: "STATE_COOKIE_TTL", value: "5"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			// Arrange
			env := validEnv()
			env[c.key] = c.value
			// Act + Assert
			assertConfigInvalid(t, env, c.key)
		})
	}
}

// TestLoad_AllSixValuesLoaded は (e)。
// 正常系で 6 つの新フィールド値（3 duration + 3 secret）が読み込まれること。
func TestLoad_AllSixValuesLoaded(t *testing.T) {
	// Arrange
	env := validEnv()
	env["SESSION_IDLE_TIMEOUT"] = "15m"
	env["SESSION_ABSOLUTE_TIMEOUT"] = "4h"
	env["STATE_COOKIE_TTL"] = "5m"

	// Act
	cfg, err := loadFrom(envGetterFromMap(env))

	// Assert
	if err != nil {
		t.Fatalf("loadFrom returned error: %v", err)
	}
	if cfg.SessionIdleTimeout != 15*time.Minute {
		t.Errorf("SessionIdleTimeout = %s, want 15m", cfg.SessionIdleTimeout)
	}
	if cfg.SessionAbsoluteTimeout != 4*time.Hour {
		t.Errorf("SessionAbsoluteTimeout = %s, want 4h", cfg.SessionAbsoluteTimeout)
	}
	if cfg.StateCookieTTL != 5*time.Minute {
		t.Errorf("StateCookieTTL = %s, want 5m", cfg.StateCookieTTL)
	}
	if cfg.StateMACSecret != env["STATE_MAC_SECRET"] {
		t.Errorf("StateMACSecret mismatch")
	}
	if cfg.OIDCTenantClientSecret != env["OIDC_TENANT_CLIENT_SECRET"] {
		t.Errorf("OIDCTenantClientSecret mismatch")
	}
	if cfg.OIDCAdminClientSecret != env["OIDC_ADMIN_CLIENT_SECRET"] {
		t.Errorf("OIDCAdminClientSecret mismatch")
	}
}

// TestLoad_StateCookieTTL_Boundary は (f)。
// STATE_COOKIE_TTL の境界値: 0s / -1m / 500ms / 11m / 999ms / 10m1s で reject、
// 1s / 10m で受理。
func TestLoad_StateCookieTTL_Boundary(t *testing.T) {
	rejectCases := []string{"0s", "-1m", "500ms", "11m", "999ms", "10m1s"}
	acceptCases := []string{"1s", "10m"}

	for _, v := range rejectCases {
		v := v
		t.Run("reject_"+v, func(t *testing.T) {
			// Arrange
			env := validEnv()
			env["STATE_COOKIE_TTL"] = v
			// Act + Assert
			assertConfigInvalid(t, env, "STATE_COOKIE_TTL")
		})
	}
	for _, v := range acceptCases {
		v := v
		t.Run("accept_"+v, func(t *testing.T) {
			// Arrange
			env := validEnv()
			env["STATE_COOKIE_TTL"] = v
			// Act
			cfg, err := loadFrom(envGetterFromMap(env))
			// Assert
			if err != nil {
				t.Fatalf("STATE_COOKIE_TTL=%q should be accepted, got error: %v", v, err)
			}
			want, _ := time.ParseDuration(v)
			if cfg.StateCookieTTL != want {
				t.Errorf("StateCookieTTL = %s, want %s", cfg.StateCookieTTL, want)
			}
		})
	}
}

// TestLoad_SessionIdleTimeout_Boundary は (g)。
// SESSION_IDLE_TIMEOUT の境界値: 0s / -1m / 500ms / 25h / 999ms / 24h1s で reject、
// 1s / 24h で受理。
func TestLoad_SessionIdleTimeout_Boundary(t *testing.T) {
	rejectCases := []string{"0s", "-1m", "500ms", "25h", "999ms", "24h1s"}
	acceptCases := []string{"1s", "24h"}

	for _, v := range rejectCases {
		v := v
		t.Run("reject_"+v, func(t *testing.T) {
			// Arrange
			env := validEnv()
			env["SESSION_IDLE_TIMEOUT"] = v
			// IDLE > ABS の cross-field エラーを避けるため ABS も最大に揃える
			env["SESSION_ABSOLUTE_TIMEOUT"] = "24h"
			// Act + Assert
			assertConfigInvalid(t, env, "SESSION_IDLE_TIMEOUT")
		})
	}
	for _, v := range acceptCases {
		v := v
		t.Run("accept_"+v, func(t *testing.T) {
			// Arrange
			env := validEnv()
			env["SESSION_IDLE_TIMEOUT"] = v
			env["SESSION_ABSOLUTE_TIMEOUT"] = "24h"
			// Act
			cfg, err := loadFrom(envGetterFromMap(env))
			// Assert
			if err != nil {
				t.Fatalf("SESSION_IDLE_TIMEOUT=%q should be accepted, got error: %v", v, err)
			}
			want, _ := time.ParseDuration(v)
			if cfg.SessionIdleTimeout != want {
				t.Errorf("SessionIdleTimeout = %s, want %s", cfg.SessionIdleTimeout, want)
			}
		})
	}
}

// TestLoad_SessionAbsoluteTimeout_Boundary は (h)。
// SESSION_ABSOLUTE_TIMEOUT の境界値: 0s / -1m / 500ms / 25h で reject、1s / 24h で受理。
func TestLoad_SessionAbsoluteTimeout_Boundary(t *testing.T) {
	rejectCases := []string{"0s", "-1m", "500ms", "25h"}
	acceptCases := []string{"1s", "24h"}

	for _, v := range rejectCases {
		v := v
		t.Run("reject_"+v, func(t *testing.T) {
			// Arrange
			env := validEnv()
			env["SESSION_ABSOLUTE_TIMEOUT"] = v
			// IDLE は default 30m なので、ABS が 30m 未満になる場合は IDLE も併せて
			// 下げる（同じ値にする）ことで cross-field エラーとの混同を避ける。
			env["SESSION_IDLE_TIMEOUT"] = "1s"
			// Act + Assert
			assertConfigInvalid(t, env, "SESSION_ABSOLUTE_TIMEOUT")
		})
	}
	for _, v := range acceptCases {
		v := v
		t.Run("accept_"+v, func(t *testing.T) {
			// Arrange
			env := validEnv()
			env["SESSION_ABSOLUTE_TIMEOUT"] = v
			env["SESSION_IDLE_TIMEOUT"] = "1s"
			// Act
			cfg, err := loadFrom(envGetterFromMap(env))
			// Assert
			if err != nil {
				t.Fatalf("SESSION_ABSOLUTE_TIMEOUT=%q should be accepted, got error: %v", v, err)
			}
			want, _ := time.ParseDuration(v)
			if cfg.SessionAbsoluteTimeout != want {
				t.Errorf("SessionAbsoluteTimeout = %s, want %s", cfg.SessionAbsoluteTimeout, want)
			}
		})
	}
}

// TestLoad_SessionIdleGreaterThanAbsolute_Rejected は (i)。
// SESSION_IDLE_TIMEOUT > SESSION_ABSOLUTE_TIMEOUT で reject、IDLE == ABS は受理。
func TestLoad_SessionIdleGreaterThanAbsolute_Rejected(t *testing.T) {
	t.Run("idle > absolute → reject", func(t *testing.T) {
		// Arrange
		env := validEnv()
		env["SESSION_IDLE_TIMEOUT"] = "2h"
		env["SESSION_ABSOLUTE_TIMEOUT"] = "1h"
		// Act + Assert
		assertConfigInvalid(t, env, "SESSION_IDLE_TIMEOUT")
	})

	t.Run("idle == absolute → accept", func(t *testing.T) {
		// Arrange
		env := validEnv()
		env["SESSION_IDLE_TIMEOUT"] = "1h"
		env["SESSION_ABSOLUTE_TIMEOUT"] = "1h"
		// Act
		_, err := loadFrom(envGetterFromMap(env))
		// Assert
		if err != nil {
			t.Fatalf("idle == absolute should be accepted, got error: %v", err)
		}
	})
}

// TestLoad_OIDCClientID_SameAcrossConsoles_Rejected は (j)。
// OIDC_TENANT_CLIENT_ID == OIDC_ADMIN_CLIENT_ID で reject。
func TestLoad_OIDCClientID_SameAcrossConsoles_Rejected(t *testing.T) {
	// Arrange
	env := validEnv()
	env["OIDC_TENANT_CLIENT_ID"] = "shared-client"
	env["OIDC_ADMIN_CLIENT_ID"] = "shared-client"
	// Act + Assert
	assertConfigInvalid(t, env, "OIDC_TENANT_CLIENT_ID")
}

// TestLoad_OIDCClientSecret_SameAcrossConsoles_Rejected は (k)。
// OIDC_TENANT_CLIENT_SECRET == OIDC_ADMIN_CLIENT_SECRET で reject。
// 生値はエラーメッセージに含まれないことも確認する（NFR 1.1）。
func TestLoad_OIDCClientSecret_SameAcrossConsoles_Rejected(t *testing.T) {
	// Arrange
	const sharedSecret = "this-secret-is-shared-across-tenant-and-admin-clients"
	env := validEnv()
	env["OIDC_TENANT_CLIENT_SECRET"] = sharedSecret
	env["OIDC_ADMIN_CLIENT_SECRET"] = sharedSecret
	// Act
	_, err := loadFrom(envGetterFromMap(env))
	// Assert
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var de *internalerrors.Error
	if !stdErrors.As(err, &de) || de.Code != internalerrors.CodeConfigInvalid {
		t.Fatalf("expected CodeConfigInvalid, got %v", err)
	}
	if !strings.Contains(de.Message, "OIDC_TENANT_CLIENT_SECRET") {
		t.Fatalf("error message should mention OIDC_TENANT_CLIENT_SECRET, got %q", de.Message)
	}
	if strings.Contains(de.Message, sharedSecret) {
		t.Fatalf("error message must not contain the client secret value, got %q", de.Message)
	}
}

// TestLoad_OIDCRedirectURL_SameAcrossConsoles_Rejected は (l)。
// OIDC_TENANT_REDIRECT_URL == OIDC_ADMIN_REDIRECT_URL で reject。
func TestLoad_OIDCRedirectURL_SameAcrossConsoles_Rejected(t *testing.T) {
	// Arrange
	env := validEnv()
	env["OIDC_TENANT_REDIRECT_URL"] = "http://localhost:8080/api/auth/callback"
	env["OIDC_ADMIN_REDIRECT_URL"] = "http://localhost:8080/api/auth/callback"
	// Act + Assert
	assertConfigInvalid(t, env, "OIDC_TENANT_REDIRECT_URL")
}
