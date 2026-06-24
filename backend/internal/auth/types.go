package auth

import (
	"time"

	"github.com/google/uuid"

	"github.com/hitoshiichikawa/ae-mdm/internal/platform/oidc"
)

// Identity は OIDC subject から解決された内部管理者識別子と紐付け情報。
//
// design.md「Domain Layer (Auth) / Auth Types」節と整合。RBAC 解釈（Roles の意味付け）は
// 後続 Issue の責務で、本 struct は groups クレーム由来のロール文字列をそのまま保持する。
// TenantID == uuid.Nil は SuperAdmin を表す（A2 の TenantContext 規約と整合）。
type Identity struct {
	// AdminUserID は admin_users テーブルの primary key（内部一意 ID）。
	AdminUserID uuid.UUID
	// OIDCSubject は OIDC `sub` クレーム。issuer スコープで一意のため、admin_users 解決
	// では Issuer とセットで使う（Repository は issuer + subject の組で SELECT する）。
	OIDCSubject string
	// Email は OIDC `email` クレーム（IdP 側で未提供なら空文字）。
	Email string
	// TenantID は所属テナント。**SuperAdmin の場合は uuid.Nil**（design.md L475 と整合）。
	TenantID uuid.UUID
	// Roles は groups クレームをそのまま転記したロール文字列の集合。
	// RBAC 解釈は後続 Issue で行う。
	Roles []string
	// IsSuperAdmin は Roles に "SuperAdmin" 相当が含まれるかを集約した派生フラグ。
	// Repository 側で集計し、Identity を返す時にセットする。
	IsSuperAdmin bool
}

// Session は永続ストアに記録されるセッションの値オブジェクト。
//
// design.md「Domain Layer (Auth) / Auth Types」節および sessions テーブル拡張（task 1.2）と整合。
// 生 cookie 値は **保持しない**（TokenHash のみを永続化 / Req 3.6 / 3.7）。
type Session struct {
	// TokenHash は cookie 生値の SHA-256 hex 表記。永続ストア上の lookup key として使う。
	TokenHash string
	// AdminUserID はセッション保有者の admin_users.id。
	AdminUserID uuid.UUID
	// Console は OIDC 認証成功時の MatchedConsole を引き継いだコンソール種別。
	// `internal/platform/oidc.Console` を再利用（独自型は定義しない）。
	Console oidc.Console
	// IssuedAt はセッション発行時刻（Req 4.2 の absolute timeout 算出基準）。
	IssuedAt time.Time
	// LastSeenAt は最終操作時刻（Req 4.1 / 4.3 の idle timeout 判定基準）。
	LastSeenAt time.Time
	// ExpiresAt は IssuedAt + cfg.SessionAbsoluteTimeout（Req 4.2 / 4.8）。
	// idle 更新によって延長されない（absolute 固定）。
	ExpiresAt time.Time
	// RevokedAt は logout / 失効検出時にセットされる。nil は有効状態（Req 5.1）。
	RevokedAt *time.Time
}
