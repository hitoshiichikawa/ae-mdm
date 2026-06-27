// Package tenant は ae-mdm の Tenant ドメイン Service（A4b / Issue #38）のドメイン層を
// 提供する。SaaS 運用者（SuperAdmin）が admin-console から顧客企業ごとのテナントを
// 作成・Enterprise バインド・無効化・参照する Tenant ライフサイクルを所有する。
//
// 本ファイル時点（task 1.1）では、後続 task（Repository / Service / Handler）が共有する
// ドメイン型（types.go）と監査記録ポート（audit_log.go）の scaffold のみを提供する。
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
//   - 禁止: 上位 application / cmd / 他 domain への直接 import
//
// # 構成（task 1.1 時点）
//
//   - doc.go         : 本 package ドキュメント（依存方向ルール / 機密値の非埋込契約）
//   - types.go       : Status enum（pending_bind / bound / disabled）と ParseStatus / Valid /
//     ドメイン型（TenantRow / TenantView）/ 入出力 DTO（CreateInput / BindInput /
//     DisableInput / SignupURL）/ 監査ポート（EventRecorder / Event / Operation / Result）/
//     sentinel error（ErrConflict / ErrInvalidState / ErrConfirmationRequired / ErrNotBound /
//     ErrTenantDisabled / ErrTenantNotFound）
//   - audit_log.go   : EventRecorder の `internal/logger` 実装（LoggerRecorder）。Audit
//     Service 実装後に差し替える interim binding。Event の安全フィールドのみ構造化出力する
//   - types_test.go  : Status の正常 / 異常値判定の in-package 単体テスト（NFR 1.1）
//   - audit_log_test.go : LoggerRecorder が秘密値を出力しないこと（redact 観点 / NFR 2.3）と
//     監査項目を field 化すること（NFR 2.1）の in-package 単体テスト
//
// # 機密値の非埋込契約（NFR 2.3）
//
// 本 package の監査 Event（Event 構造体）は、サインアップ URL の秘密パラメータ・サービス
// アカウント資格情報・OAuth トークンの生値を **フィールドとして一切保持しない**。これにより
// EventRecorder の実装がどのような構造化出力を行っても、機密値がログへ surface しない一次
// 防御を構造的に成立させる（logger 側 redact allowlist への依存を持たない）。
package tenant
