// Package authz は ae-mdm の認可基盤（A3b / Issue #37）を提供する。
//
// 本パッケージは requirements.md Req 1〜7 / NFR 1〜4 / design.md「Authorization (RBAC)
// Service」節に対応する。4 ロール（SuperAdmin / TenantAdmin / Operator / Viewer）×
// action × resource の許可関係を **表駆動の宣言テーブル**として保持し、`/api/admin/*`
// 配下や各ドメインの HTTP ハンドラから許可判定の単一エントリポイントとして呼ばれる。
//
// # 依存方向ルール
//
// 本 package は他の internal package（auth / platform/db / platform/httpserver 等）を
// import しない。横断的 platform 機能として errors のみに依存し、上位の各 domain
// （auth / tenant / policy / device / command / app / audit / enrollment）および
// `internal/platform/httpserver/admin_middleware.go` からのみ呼び出される。
//
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/errors
//   - 禁止: github.com/hitoshiichikawa/ae-mdm/internal/auth ほか上位 / 横並び domain
//
// # 表駆動マトリクスの責務
//
// 4 ロール × action × resource の許可関係は `permissions.go` の単一定数テーブル
// `permissionMatrix` に集約する。Authorizer.Authorize は本テーブルへの O(1) lookup
// （map index）で判定し、外部 I/O を発生させない（NFR 2.1 / NFR 3.1）。未知の role /
// action / resource は fail-closed で拒否する（Req 1.5 / 1.6）。
//
// # cross-tenant / targetTenantID の責務
//
// 同テナント内操作は permissionMatrix の判定結果に従う（Req 3.2）。cross-tenant 操作
// （session.TenantID != targetTenantID）は admin-console aud かつ SuperAdmin の場合に
// 限り許可する（Req 3.3 / 3.4 / 3.5）。targetTenantID が空 / null / UUID 不正書式の
// 場合は role / audience に関係なく fail-closed で拒否する（Req 4.1 / 4.2 / 4.3 / 4.4）。
//
// # 拒否理由 enum の責務
//
// Authorize の返り値 Decision.DenyReason は呼び出し側で区別可能な enum 形式で返却する
// （Req 5.2 / 5.3）。Logger.Warn の構造化 field 値として直接 surface し、運用者の事後
// 追跡に用いる（Req 7.2）。
package authz
