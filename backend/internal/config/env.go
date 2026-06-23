package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	internalerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// SessionSecretMinLen は SESSION_SECRET に要求される最小長（文字数）。
// requirements.md Req 1.3 / tasks.md 1.1 詳細項目の「SESSION_SECRET 32 文字未満で
// CodeConfigInvalid を返す」と整合。
const SessionSecretMinLen = 32

// 既定値定数（requirements.md / tasks.md 1.1 詳細項目と整合）。
const (
	defaultAuditLogRetentionDays         = 180
	defaultDeviceSyncDelayThresholdHours = 24
	defaultLogLevel                      = "info"
	defaultLogFormat                     = "json"
	defaultLogOutput                     = "stderr"
	defaultHTTPListenAddr                = ":8080"
)

// envGetter は os.LookupEnv を抽象化する関数型。テストでは map[string]string を使った
// in-memory 実装で差し替えできる。本パッケージの外部 API は os.LookupEnv に固定し、
// 内部実装の差し替えは loadFrom 経由で行う。
type envGetter func(key string) (string, bool)

// Load は環境変数から Config を構築する。
// requirements.md Req 1.1 / 1.2 / 1.3 / 1.4 / 1.5 と design.md Components: Config Loader 節に対応。
//
// 必須欠落・int 解析失敗・SESSION_SECRET 32 文字未満の場合は
// *errors.Error{Code: CodeConfigInvalid} を返す（fail-fast）。
// 戻り値の Config は immutable（呼び出し側で書き換えない契約）。
func Load() (Config, error) {
	return loadFrom(os.LookupEnv)
}

// loadFrom は envGetter から Config を組み立てる内部実装。テストから使用する。
func loadFrom(get envGetter) (Config, error) {
	var cfg Config
	missing := []string{}
	invalid := []string{}

	requiredStr := func(key string, dst *string) {
		v, ok := get(key)
		if !ok || strings.TrimSpace(v) == "" {
			missing = append(missing, key)
			return
		}
		*dst = v
	}
	optionalStr := func(key string, dst *string, def string) {
		v, ok := get(key)
		if !ok || strings.TrimSpace(v) == "" {
			*dst = def
			return
		}
		*dst = v
	}
	intWithDefault := func(key string, dst *int, def int) {
		v, ok := get(key)
		if !ok || strings.TrimSpace(v) == "" {
			*dst = def
			return
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			invalid = append(invalid, fmt.Sprintf("%s (not an integer: %q)", key, v))
			return
		}
		*dst = n
	}

	// 必須 string
	requiredStr("DATABASE_URL", &cfg.DatabaseURL)
	requiredStr("OIDC_TENANT_ISSUER_URL", &cfg.OIDCTenantIssuerURL)
	requiredStr("OIDC_TENANT_CLIENT_ID", &cfg.OIDCTenantClientID)
	requiredStr("OIDC_TENANT_REDIRECT_URL", &cfg.OIDCTenantRedirectURL)
	requiredStr("OIDC_ADMIN_ISSUER_URL", &cfg.OIDCAdminIssuerURL)
	requiredStr("OIDC_ADMIN_CLIENT_ID", &cfg.OIDCAdminClientID)
	requiredStr("OIDC_ADMIN_REDIRECT_URL", &cfg.OIDCAdminRedirectURL)
	requiredStr("PUBSUB_PROJECT_ID", &cfg.PubSubProjectID)
	requiredStr("PUBSUB_TOPIC", &cfg.PubSubTopic)
	requiredStr("PUBSUB_SUBSCRIPTION", &cfg.PubSubSubscription)
	requiredStr("AMAPI_PROJECT_ID", &cfg.AMAPIProjectID)
	requiredStr("GOOGLE_APPLICATION_CREDENTIALS", &cfg.GoogleApplicationCredentials)
	requiredStr("SESSION_SECRET", &cfg.SessionSecret)

	// optional / default 付き
	if v, ok := get("MIGRATE_DATABASE_URL"); ok && strings.TrimSpace(v) != "" {
		cfg.MigrateDatabaseURL = v
	} else {
		// fallback to DATABASE_URL (Makefile target との整合)。空でも整合性は維持される。
		cfg.MigrateDatabaseURL = cfg.DatabaseURL
	}
	optionalStr("PUBSUB_EMULATOR_HOST", &cfg.PubSubEmulatorHost, "")
	intWithDefault("AUDIT_LOG_RETENTION_DAYS", &cfg.AuditLogRetentionDays, defaultAuditLogRetentionDays)
	intWithDefault("DEVICE_SYNC_DELAY_THRESHOLD_HOURS", &cfg.DeviceSyncDelayThresholdHours, defaultDeviceSyncDelayThresholdHours)
	optionalStr("LOG_LEVEL", &cfg.LogLevel, defaultLogLevel)
	optionalStr("LOG_FORMAT", &cfg.LogFormat, defaultLogFormat)
	optionalStr("LOG_OUTPUT", &cfg.LogOutput, defaultLogOutput)
	optionalStr("HTTP_LISTEN_ADDR", &cfg.HTTPListenAddr, defaultHTTPListenAddr)

	// SESSION_SECRET 長さ検証（required が成功している場合のみ）。
	if cfg.SessionSecret != "" && len(cfg.SessionSecret) < SessionSecretMinLen {
		invalid = append(invalid, fmt.Sprintf("SESSION_SECRET (length %d < required %d)", len(cfg.SessionSecret), SessionSecretMinLen))
	}

	if len(missing) > 0 || len(invalid) > 0 {
		return Config{}, buildConfigError(missing, invalid)
	}

	return cfg, nil
}

// buildConfigError は missing / invalid のリストから *errors.Error を組み立てる。
// メッセージはどの env 変数が問題かを判別可能な形にする（Req 1.3）。
func buildConfigError(missing, invalid []string) error {
	parts := []string{}
	if len(missing) > 0 {
		parts = append(parts, fmt.Sprintf("missing required env: %s", strings.Join(missing, ", ")))
	}
	if len(invalid) > 0 {
		parts = append(parts, fmt.Sprintf("invalid env: %s", strings.Join(invalid, "; ")))
	}
	return internalerrors.New(internalerrors.CodeConfigInvalid, strings.Join(parts, "; "))
}
