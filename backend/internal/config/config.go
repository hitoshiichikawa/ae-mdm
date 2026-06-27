// Package config は ae-mdm の全プロセス（api / worker / 補助 CLI）が共通利用する
// 設定値の不変スナップショットと、その読み込み API を提供する。
//
// 本パッケージは他の internal package を import しない（依存方向の起点として扱う / design.md
// Components: Config Loader 節と整合）。
//
// 設定は環境変数からのみ注入する（NFR 4.2: ローカル状態を持たず Fargate 等のコンテナ実行
// 環境に移行可能な構成を維持する）。
package config

import "time"

// Config は全プロセスで共通利用される設定値の不変スナップショット。
// requirements.md Req 1.1 / 1.2 / 1.4 / design.md Components: Config Loader 節と整合。
//
// 戻り値は値型で返し、ランタイム書き換え API を提供しない（Req 1.4: immutable）。
type Config struct {
	// DatabaseURL は app_user で接続する PostgreSQL 接続文字列（必須）。
	DatabaseURL string
	// MigrateDatabaseURL は migration_user で接続する PostgreSQL 接続文字列。
	// 未指定時は DatabaseURL にフォールバックする。
	MigrateDatabaseURL string

	// OIDC: テナント向け
	OIDCTenantIssuerURL   string
	OIDCTenantClientID    string
	OIDCTenantRedirectURL string
	// OIDCTenantClientSecret は tenant-console OIDC confidential client の client_secret
	// （client_secret_basic 認証用 / Issue #33 design.md Req 6.2 と確認事項 6 で確定）。
	OIDCTenantClientSecret string

	// OIDC: SaaS 運用者（admin）向け
	OIDCAdminIssuerURL   string
	OIDCAdminClientID    string
	OIDCAdminRedirectURL string
	// OIDCAdminClientSecret は admin-console OIDC confidential client の client_secret。
	OIDCAdminClientSecret string

	// Pub/Sub
	PubSubProjectID    string
	PubSubTopic        string
	PubSubSubscription string
	PubSubEmulatorHost string // optional: 本番では空文字
	// PubSubDeadLetterTopic は処理不能メッセージの送出先 dead-letter topic 名（optional）。
	// 未設定（空文字）の場合、dead-letter 送出 IF は構造化エラーを返して当該メッセージを
	// ack しない（Issue #35 requirements 5.4。送出先が無いまま喪失することを防ぐ）。
	PubSubDeadLetterTopic string
	// PubSubMaxOutstandingMessages は subscriber が同時に未確定（ack 待ち）で保持する
	// メッセージ数の上限（Issue #35 requirements 4.1）。未設定時は既定値を用いる。
	// 1 未満を設定すると subscriber 構築が CodeConfigInvalid で fail-fast する
	// （requirements 4.3 / 6.5。運用者が並行度を誤設定したまま起動するのを防ぐ）。
	PubSubMaxOutstandingMessages int

	// AMAPI（Android Management API）
	AMAPIProjectID               string
	GoogleApplicationCredentials string

	// Session
	SessionSecret string // 32 文字以上を要求

	// SessionIdleTimeout は管理者セッションのアイドルタイムアウト（Req 4.4 / NFR 2.1）。
	// 既定 30 分。`1s <= ttl <= 24h` の境界 validation あり（Issue #33 tasks.md 1.1）。
	SessionIdleTimeout time.Duration
	// SessionAbsoluteTimeout は管理者セッションの絶対有効期限（Req 4.5 / NFR 2.1）。
	// 既定 8 時間。`1s <= ttl <= 24h` の境界 validation あり。
	SessionAbsoluteTimeout time.Duration
	// StateCookieTTL は OIDC 認可コードフローで使う state 保護 cookie の有効期限
	// （Req 2.4 / NFR 2.1）。既定 10 分。`1s <= ttl <= 10m` の境界 validation あり。
	StateCookieTTL time.Duration
	// StateMACSecret は state 保護 cookie の HMAC-SHA256 鍵（Req 2.2）。
	// 32 バイト以上を要求（Issue #33 tasks.md 1.1）。
	StateMACSecret string

	// 業務閾値
	AuditLogRetentionDays         int // default 180
	DeviceSyncDelayThresholdHours int // default 24

	// 構造化ログ
	LogLevel  string // default "info"
	LogFormat string // default "json"
	LogOutput string // default "stderr"

	// HTTP listen
	HTTPListenAddr string // default ":8080"
}
