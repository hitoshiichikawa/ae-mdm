// Package tenant は ae-mdm の Tenant ドメイン Service（A4b / Issue #38）のドメイン層を
// 提供する。SaaS 運用者（SuperAdmin）が admin-console から顧客企業ごとのテナントを
// 作成・Enterprise バインド・無効化・参照する Tenant ライフサイクルを所有する。
//
// 本 package は Tenant ドメイン層一式を提供する: ドメイン型・DTO・監査記録ポート（types.go /
// audit_log.go）、永続化（repository.go）、状態機械を所有する Service（service.go）、
// `/api/admin/tenants` の HTTP Handler（handler.go）。
//
// # 依存方向ルール
//
// 本 package は以下のレイヤのみを import する。逆方向（platform 系 / 上位 application から
// tenant domain への import）は禁止（design.md「Architecture / Existing Architecture
// Analysis」節および `internal/auth` の依存方向ルールと整合）:
//
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/platform/amapi（Enterprise 作成代行）
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/platform/db（BeginTxFunc / TenantContext）
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/logger（監査ポートの interim 実装）
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/errors（sentinel error / Code 写像）
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/config（AMAPIProjectID）
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/platform/httpserver（presentation 層の
//     handler.go のみ。`AuthClaimsFromContext` で actor を取得し Service へ明示引数で渡す。
//     Service / Repository 層は httpserver を import しない / design.md L327-328）
//   - 禁止: 上位 application / cmd / 他 domain への直接 import
//
// # 構成
//
//   - doc.go         : 本 package ドキュメント（依存方向ルール / 機密値の非埋込契約）
//   - types.go       : Status enum（pending_bind / bound / disabled）と ParseStatus / Valid /
//     ドメイン型（TenantRow / TenantView）/ 入出力 DTO（CreateInput / BindInput /
//     DisableInput / SignupURL）/ 監査ポート（EventRecorder / Event / Operation / Result）/
//     sentinel error（ErrConflict / ErrInvalidState / ErrConfirmationRequired / ErrNotBound /
//     ErrTenantDisabled / ErrTenantNotFound）
//   - audit_log.go   : EventRecorder の `internal/logger` 実装（LoggerRecorder）。Audit
//     Service 実装後に差し替える interim binding。Event の安全フィールドのみ構造化出力する
//   - repository.go  : `Repository`（Insert / Get / List / UpdateBound / UpdateDisabled）の
//     pgxpool 実装。SuperAdmin context 昇格 + 条件付き UPDATE の affected で競合を表現する
//   - service.go     : 状態機械を所有する `Service`（Create / Bind / Disable / Get / List /
//     EnterpriseNameForTenant）。AMAPI オーケストレーション → 永続化 → 監査記録を担う
//   - handler.go     : `/api/admin/tenants` 配下 5 endpoint の HTTP Handler（presentation 層）
//   - *_test.go      : 各層の in-package 単体テスト（types / audit_log / service / handler）
//
// # 機密値の非埋込契約（NFR 2.3）
//
// 本 package の監査 Event（Event 構造体）は、サインアップ URL の秘密パラメータ・サービス
// アカウント資格情報・OAuth トークンの生値を **フィールドとして一切保持しない**。これにより
// EventRecorder の実装がどのような構造化出力を行っても、機密値がログへ surface しない一次
// 防御を構造的に成立させる（logger 側 redact allowlist への依存を持たない）。
package tenant
