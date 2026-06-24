package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	internalerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// SessionSecretMinLen は SESSION_SECRET に要求される最小長（文字数）。
// requirements.md Req 1.3 / tasks.md 1.1 詳細項目の「SESSION_SECRET 32 文字未満で
// CodeConfigInvalid を返す」と整合。
const SessionSecretMinLen = 32

// StateMACSecretMinLen は STATE_MAC_SECRET に要求される最小長（バイト数）。
// Issue #33 tasks.md 1.1 / design.md StateCookie 節と整合（HMAC-SHA256 鍵として
// 32 バイト以上）。
const StateMACSecretMinLen = 32

// 既定値定数（requirements.md / tasks.md 1.1 詳細項目と整合）。
const (
	defaultAuditLogRetentionDays         = 180
	defaultDeviceSyncDelayThresholdHours = 24
	defaultLogLevel                      = "info"
	defaultLogFormat                     = "json"
	defaultLogOutput                     = "stderr"
	defaultHTTPListenAddr                = ":8080"
)

// セッション / state cookie の duration 既定値および境界値（Issue #33 tasks.md 1.1）。
// 下限 1s は Go の `http.Cookie.MaxAge = int(ttl/time.Second)` が `MaxAge == 0` の
// 場合に Max-Age 属性を Set-Cookie ヘッダから drop する境界を回避するため
// （`MaxAge == 0` は browser session cookie 化して TTL 制御が効かなくなる）。
// 上限 24h は absolute timeout の運用最大値（誤設定による無期限 idle / token 終身化を防ぐ）。
// StateCookieTTL の上限 10m は Req 2.4 「10 分以内に有効期限が切れる」に由来。
const (
	defaultSessionIdleTimeout     = 30 * time.Minute
	defaultSessionAbsoluteTimeout = 8 * time.Hour
	defaultStateCookieTTL         = 10 * time.Minute

	sessionTimeoutMin = 1 * time.Second
	sessionTimeoutMax = 24 * time.Hour
	stateCookieTTLMin = 1 * time.Second
	stateCookieTTLMax = 10 * time.Minute
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
	// durationWithDefault は env に time.Duration 値を読み込む。`time.ParseDuration` を
	// ベースとし、parse 失敗 / 境界（min <= ttl <= max）外を invalid に積む。
	// 機密値ではないため値そのものをエラーメッセージに含める（key 名と値の併記で
	// 運用者が原因特定できることを優先）。
	durationWithDefault := func(key string, dst *time.Duration, def, min, max time.Duration) {
		v, ok := get(key)
		if !ok || strings.TrimSpace(v) == "" {
			*dst = def
			return
		}
		d, err := time.ParseDuration(strings.TrimSpace(v))
		if err != nil {
			invalid = append(invalid, fmt.Sprintf("%s (not a duration: %q)", key, v))
			return
		}
		if d < min || d > max {
			invalid = append(invalid, fmt.Sprintf("%s (%s out of range [%s, %s])", key, d, min, max))
			return
		}
		*dst = d
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
	requiredStr("STATE_MAC_SECRET", &cfg.StateMACSecret)
	requiredStr("OIDC_TENANT_CLIENT_SECRET", &cfg.OIDCTenantClientSecret)
	requiredStr("OIDC_ADMIN_CLIENT_SECRET", &cfg.OIDCAdminClientSecret)

	// URL 形式バリデーション（PR #31 round-3 review 由来 / Req 1.3）。
	// scheme + host を持つ URL を要求する。OIDC 系は http(s) スキームを要求し、
	// DATABASE_URL / MIGRATE_DATABASE_URL は postgres / postgresql / pgx5 を許容する。
	validateURL := func(key, value string, allowSchemes map[string]struct{}) {
		if value == "" {
			return // missing 側で既にエラー化されている
		}
		u, err := url.Parse(value)
		if err != nil {
			invalid = append(invalid, fmt.Sprintf("%s (URL parse failed: %v)", key, err))
			return
		}
		if u.Scheme == "" || u.Host == "" {
			invalid = append(invalid, fmt.Sprintf("%s (scheme/host が欠落: %q)", key, value))
			return
		}
		if allowSchemes != nil {
			if _, ok := allowSchemes[strings.ToLower(u.Scheme)]; !ok {
				schemes := make([]string, 0, len(allowSchemes))
				for s := range allowSchemes {
					schemes = append(schemes, s)
				}
				invalid = append(invalid, fmt.Sprintf("%s (許可されない scheme %q; expected one of %v)", key, u.Scheme, schemes))
			}
		}
	}
	httpSchemes := map[string]struct{}{"http": {}, "https": {}}
	dbSchemes := map[string]struct{}{"postgres": {}, "postgresql": {}, "pgx5": {}}
	validateURL("DATABASE_URL", cfg.DatabaseURL, dbSchemes)
	validateURL("OIDC_TENANT_ISSUER_URL", cfg.OIDCTenantIssuerURL, httpSchemes)
	validateURL("OIDC_TENANT_REDIRECT_URL", cfg.OIDCTenantRedirectURL, httpSchemes)
	validateURL("OIDC_ADMIN_ISSUER_URL", cfg.OIDCAdminIssuerURL, httpSchemes)
	validateURL("OIDC_ADMIN_REDIRECT_URL", cfg.OIDCAdminRedirectURL, httpSchemes)

	// optional / default 付き
	if v, ok := get("MIGRATE_DATABASE_URL"); ok && strings.TrimSpace(v) != "" {
		cfg.MigrateDatabaseURL = v
		validateURL("MIGRATE_DATABASE_URL", cfg.MigrateDatabaseURL, dbSchemes)
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

	// Duration 系（Issue #33 tasks.md 1.1 / Req 2.4 / 4.4 / 4.5 / NFR 2.1）。
	durationWithDefault("SESSION_IDLE_TIMEOUT", &cfg.SessionIdleTimeout,
		defaultSessionIdleTimeout, sessionTimeoutMin, sessionTimeoutMax)
	durationWithDefault("SESSION_ABSOLUTE_TIMEOUT", &cfg.SessionAbsoluteTimeout,
		defaultSessionAbsoluteTimeout, sessionTimeoutMin, sessionTimeoutMax)
	durationWithDefault("STATE_COOKIE_TTL", &cfg.StateCookieTTL,
		defaultStateCookieTTL, stateCookieTTLMin, stateCookieTTLMax)

	// SESSION_SECRET 長さ検証（required が成功している場合のみ）。
	if cfg.SessionSecret != "" && len(cfg.SessionSecret) < SessionSecretMinLen {
		invalid = append(invalid, fmt.Sprintf("SESSION_SECRET (length %d < required %d)", len(cfg.SessionSecret), SessionSecretMinLen))
	}
	// STATE_MAC_SECRET 長さ検証（HMAC-SHA256 鍵として 32 バイト以上を要求）。
	// 機密値そのものはエラーメッセージに埋め込まず、長さ情報のみ surface する
	// （Issue #33 tasks.md 1.1 / NFR 1.1）。
	if cfg.StateMACSecret != "" && len(cfg.StateMACSecret) < StateMACSecretMinLen {
		invalid = append(invalid, fmt.Sprintf("STATE_MAC_SECRET (length %d < required %d)", len(cfg.StateMACSecret), StateMACSecretMinLen))
	}

	// cross-field validation（Issue #33 tasks.md 1.1 / Req 6.1 / 6.2 / 6.4 / Req 4.4 / 4.5）。
	// 上記の missing / invalid を抜けた段階で実行する（前提が揃ってから比較）。
	if len(missing) == 0 && len(invalid) == 0 {
		// (a) SessionIdleTimeout <= SessionAbsoluteTimeout（idle が absolute を
		//     超える設定は階層的失効判定を破壊するため reject）。
		if cfg.SessionIdleTimeout > cfg.SessionAbsoluteTimeout {
			invalid = append(invalid, fmt.Sprintf("SESSION_IDLE_TIMEOUT (%s) must be <= SESSION_ABSOLUTE_TIMEOUT (%s)",
				cfg.SessionIdleTimeout, cfg.SessionAbsoluteTimeout))
		}
		// (b) OIDC_TENANT_CLIENT_ID != OIDC_ADMIN_CLIENT_ID（aud 排他一致が
		//     無効化される）。
		if cfg.OIDCTenantClientID == cfg.OIDCAdminClientID {
			invalid = append(invalid, "OIDC_TENANT_CLIENT_ID and OIDC_ADMIN_CLIENT_ID must differ")
		}
		// (c) OIDC_TENANT_CLIENT_SECRET != OIDC_ADMIN_CLIENT_SECRET（一方の漏洩が
		//     他方の信頼を毀損する構造を防ぐ）。生値はエラーメッセージに含めない。
		if cfg.OIDCTenantClientSecret == cfg.OIDCAdminClientSecret {
			invalid = append(invalid, "OIDC_TENANT_CLIENT_SECRET and OIDC_ADMIN_CLIENT_SECRET must differ")
		}
		// (d) OIDC_TENANT_REDIRECT_URL != OIDC_ADMIN_REDIRECT_URL（path-based
		//     console 分離が成立しない）。
		if cfg.OIDCTenantRedirectURL == cfg.OIDCAdminRedirectURL {
			invalid = append(invalid, "OIDC_TENANT_REDIRECT_URL and OIDC_ADMIN_REDIRECT_URL must differ")
		}
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
