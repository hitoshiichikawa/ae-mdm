// Package audit は ae-mdm の監査ログ Service（A5 / Issue #5）のドメイン層を提供する。
//
// 各ドメイン Service が重要操作を改竄不能に蓄積するための append-only な記録 IF（Record）と、
// tenant-console 経由の自テナント監査ログ閲覧（TenantAdmin 以上）/ admin-console 経由の
// 全テナント横断閲覧（SuperAdmin）の 2 つの閲覧経路（List）を提供する。
// 監査ログストア（audit_logs テーブル）・append-only 強制・INSERT-only 権限・保持期間 config は
// A2 / #33 / #37 で完成済みであり、本 package はこれらを **利用・連携**する。
//
// # 依存方向ルール
//
// 本 package は以下のレイヤのみを import する。逆方向（platform 系から audit domain への
// import）や他 domain への直接 import は禁止（design.md「Architecture Integration」節と整合）:
//
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/platform/db
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/platform/authz
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/platform/httpserver
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/config
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/logger
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/errors
//   - 禁止: 上位 application / cmd / 他 domain への直接 import
//
// # 構成（task 1 時点）
//
//   - types.go          : Event / EventType / ResultType / Filter のドメイン型
//   - clock.go          : Clock interface と SystemClock 実装（保持期間下限算出の DI 境界）
//   - failure_kinds.go  : 構造化ログ用 failure_kind 定数（authz_denied / parse_invalid /
//     persist_error / query_error / NFR 3.2）
//   - service.go        : Service interface（Record / List）+ 本番実装。update/delete IF は
//     公開しない（Req 1.5）。Repository interface を consumer-defines-interface イディオムで
//     本ファイル内に宣言する（task 1 完了時点で audit package が単独でビルド可能にするため。
//     詳細は impl-notes.md「確認事項」参照）
//   - service_test.go   : Service の単体テスト（fake Repository / fake Clock 経由 / 表駆動）
//
// # 機密値の非格納契約
//
// Event.Detail / 構造化ログ / エラーメッセージ本文に、ID トークン本体・セッション cookie の
// 生値・パスワード等の機密値の平文を含めない（Req 1.7 / NFR 3.1）。Service は Detail を
// 素通しするため、detail の sanitize は記録を要求する呼び出し側の責務である。
package audit
