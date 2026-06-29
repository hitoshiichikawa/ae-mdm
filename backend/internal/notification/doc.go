// Package notification は ae-mdm の Notification Dispatcher（A6b / Issue #39）のドメイン層を
// 提供する。
//
// AMAPI（Android Management API）が Cloud Pub/Sub 経由で at-least-once 配信する通知
// （ENROLLMENT / STATUS_REPORT / COMMAND）を **冪等に処理し、種別ごとのドメインハンドラへ
// 振り分け、テナントを特定できない通知を退避する基盤** を担う。各ドメインハンドラ（種別の
// 実体処理）は後続 Issue の所有であり、本 package は NotificationHandler interface 経由で
// 振り分けるのみ（design.md「Notification Domain」節と整合）。
//
// 本 package が所有するテーブルは notification_dedupe（MessageID 単位の重複排除）と
// unassigned_notifications（テナント未割当通知の退避）の 2 つ。いずれも tenant_id を持たない
// cross-tenant infra テーブルであり、SuperAdmin context で越境アクセスする（RLS / migration
// 0010 / 0011 を消費。新規 migration は作らない）。
//
// # 依存方向ルール
//
// 本 package は以下のレイヤのみを import する。逆方向（platform 系から notification domain
// への import）や他 domain（tenant / audit 等）への直接 import は将来も禁止
// （design.md「Architecture Integration」節と整合）:
//
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/platform/pubsub
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/platform/db
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/logger
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/errors
//   - 禁止: 上位 application / cmd / 他 domain（tenant / audit 等）への直接 import
//
// 他 domain への依存は依存逆転で解決する。enterprise_name → tenant_id の逆引きは tenant
// パッケージを直接 import せず、本 package が宣言する TenantResolver interface 経由で
// 注入する（Dispatcher の構築時に tenant.Service を structural typing で受ける / 後続 task）。
//
// # 構成（task 2 時点）
//
//   - types.go        : Envelope / NotificationType / UnassignedNotification / Filter /
//     NotificationHandler IF / TenantResolver IF のドメイン型
//   - verifier.go     : *pubsub.Message → Envelope のパース・検証（空 payload / 種別判定不能 /
//     検証失敗を破棄 ack 相当の *errors.Error{IsTransient:false} で分類）
//   - dedupe.go       : notification_dedupe への冪等記録（ON CONFLICT DO NOTHING）と既処理判定
//   - verifier_test.go / dedupe_test.go : 単体テスト
//
// dispatcher.go / unassigned.go / admin_handler.go と結合テストは後続 task（3 以降）の責務。
//
// # 機密値の非格納契約
//
// payload 生値・token 等の機密値を error 文言・構造化ログに補間しない（NFR 3.1）。構造化ログは
// message_id（logger.MessageID）と非機密な notification_type 等の field に限定し、payload を
// message 本文へ埋め込まない（audit の機密値非格納契約と同方針）。
package notification
