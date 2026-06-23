package config

import (
	stdErrors "errors"
	"strings"
	"testing"

	internalerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// validEnv は正常系のテスト fixture。必須 env をすべて埋めた map を返す。
func validEnv() map[string]string {
	return map[string]string{
		"DATABASE_URL":                   "postgres://app_user:pw@localhost:5432/ae_mdm?sslmode=disable",
		"OIDC_TENANT_ISSUER_URL":         "http://keycloak:8080/realms/tenant",
		"OIDC_TENANT_CLIENT_ID":          "tenant-console",
		"OIDC_TENANT_REDIRECT_URL":       "http://localhost:3000/callback",
		"OIDC_ADMIN_ISSUER_URL":          "http://keycloak:8080/realms/admin",
		"OIDC_ADMIN_CLIENT_ID":           "admin-console",
		"OIDC_ADMIN_REDIRECT_URL":        "http://localhost:3001/callback",
		"PUBSUB_PROJECT_ID":              "ae-mdm-local",
		"PUBSUB_TOPIC":                   "amapi-notifications",
		"PUBSUB_SUBSCRIPTION":            "amapi-notifications-sub",
		"AMAPI_PROJECT_ID":               "ae-mdm-local",
		"GOOGLE_APPLICATION_CREDENTIALS": "/secrets/sa.json",
		"SESSION_SECRET":                 "this-is-a-32-char-secret-okk!!!!",
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
