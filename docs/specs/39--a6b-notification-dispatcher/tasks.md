# Implementation Plan

- [ ] 1. tenant 逆引き（enterprise_name → tenant_id）を tenant パッケージに追加
- [ ] 1.1 `TenantIDByEnterpriseName` を Repository / Service に追加 (P)
  - `internal/tenant/repository.go`: `SELECT id FROM tenants WHERE enterprise_name=$1 AND status='bound'` を `superAdminContext` + `BeginTxFunc` で実装。0 件は `found=false`（エラーにしない）、DB 失敗は `*errors.Error{CodeUnavailable, IsTransient:true}` で wrap。部分一意 index `uq_tenants_enterprise_name` により最大 1 件
  - `internal/tenant/service.go`: Service IF に `TenantIDByEnterpriseName(ctx, name) (uuid.UUID, bool, error)` を追加。空 enterprise_name は DB を叩かず `found=false` を即返す（Req 3.4 退避経路）。SuperAdmin context 確立を前提とし、拒否時は構造化ログ（NFR 3.1）
  - 既存 `EnterpriseNameForTenant` / 既存 IF を破壊しない追加メソッドとする
  - `internal/tenant/repository_test.go` / `service_test.go` に逆引きテストを追加（bound 解決 / 未 bound・不在は found=false / 空入力 found=false / DB 失敗 transient）
  - _Requirements: 3.1, 3.2, 3.4_
  - _Boundary: TenantIDByEnterpriseName_

- [ ] 2. notification パッケージの型・冪等排除・検証（types / dedupe / verifier）
- [ ] 2.1 パッケージ型定義と Verifier (P)
  - `internal/notification/doc.go`: パッケージ概要（audit/doc.go に倣う）
  - `internal/notification/types.go`: `Envelope` / `NotificationType`（ENROLLMENT / STATUS_REPORT / COMMAND）/ `UnassignedNotification` / `Filter` / `NotificationHandler` IF / `TenantResolver` IF
  - `internal/notification/verifier.go`: `Parse(*pubsub.Message) (Envelope, error)`。空 payload（Req 2.6）/ 種別判定不能（Req 2.6）/ 検証失敗（Req 2.5）を `*errors.Error{IsTransient:false}`（破棄 ack 相当）で分類。enterprise_name 空/欠落は Envelope 内で空のまま通す（Req 3.4 は Dispatcher が退避判定）。機密値を error/ログに補間しない（NFR 3.1）
  - `internal/notification/verifier_test.go`: 各種別の正常 parse / 空 payload / 種別判定不能 / 検証失敗 / enterprise_name 空 の分類テスト
  - _Requirements: 2.5, 2.6, 3.4, NFR 3.1_
  - _Boundary: NotificationVerifier_
- [ ] 2.2 Dedupe（notification_dedupe 冪等記録）
  - `internal/notification/dedupe.go`: `Dedupe` IF（`IsProcessed` / `MarkProcessed`）と pgxpool 実装。`MarkProcessed` は `INSERT INTO notification_dedupe (message_id, notification_type) VALUES ($1,$2) ON CONFLICT (message_id) DO NOTHING`（並行同一 MessageID の直列化を PK + ON CONFLICT で担保 / Req 1.3）。`superAdminContext` + `BeginTxFunc`（RLS SuperAdmin only / 既存スキーマ 0010/0011 を消費、新規 migration なし）。永続化失敗は `CodeUnavailable, IsTransient:true`（Req 1.4）
  - `internal/notification/dedupe_test.go`: IsProcessed 検出/通過、MarkProcessed の ON CONFLICT 冪等（fake/mock repo）、永続化失敗の transient 写像
  - _Requirements: 1.1, 1.2, 1.3, 1.4, NFR 3.1_
  - _Boundary: NotificationDedupe_
  - _Depends: 2.1_

- [ ] 3. 退避キューと Dispatcher 本体（unassigned / dispatcher）
- [ ] 3.1 UnassignedQueue（退避 INSERT + List）
  - `internal/notification/unassigned.go`: `UnassignedQueue` IF（`Enqueue` / `List`）と pgxpool 実装。`Enqueue` は `unassigned_notifications` へ id(uuid 採番) / message_id / notification_type / enterprise_name / payload(jsonb) を INSERT（tenant scoped テーブルに触れない / NFR 2.2 / Req 3.5）。`List` は Filter（from/to/type）適用、0 件は非 nil 空 slice。`superAdminContext` + `BeginTxFunc`
  - `internal/notification/unassigned_test.go`: Enqueue 後の List 取得、Filter（from/to/type）適用、0 件空 slice
  - _Requirements: 3.2, 3.3, 3.5, 4.1, 4.2, NFR 2.2, NFR 3.1_
  - _Boundary: UnassignedQueue_
  - _Depends: 2.1_
- [ ] 3.2 Dispatcher（段階 orchestrate + ack/nack 写像）
  - `internal/notification/dispatcher.go`: `pubsub.MessageHandler` を実装（`Handle(ctx, *pubsub.Message) error`）。`NewDispatcher(verifier, dedupe, unassigned, tenantResolver, handlers map[NotificationType]NotificationHandler, log)`。段階: Verify → Dedup（既処理は即 ack / Req 1.2）→ Resolve（`TenantResolver.TenantIDByEnterpriseName`）→ 未割当は Enqueue + ack（Req 3.2 / 3.3）→ tenant context 確立 → 種別 handler 振り分け（Req 2.1-2.3）→ 成功時 dedupe MarkProcessed + ack（Req 1.1 / 5.2）
  - ack/nack 写像: 既処理 / 未割当退避成功 / 未登録種別（Req 2.4）/ 検証失敗・空 payload（Req 2.5 / 2.6）は `IsTransient=false` or nil（ack 完了扱い）。dedupe/退避の永続化失敗（Req 1.4）/ transient handler 失敗（Req 5.1 / 5.3）は `IsTransient=true`（nack 保持）。各段で message_id 構造化ログ（NFR 3.1）
  - tenant 未解決時はいずれのテナントリソースも更新しない（NFR 2.1 / 2.2）
  - `internal/notification/dispatcher_test.go`: 段階分岐 × ack/nack 種別を mock verifier/dedupe/unassigned/resolver/handler で表駆動（既処理→ack / 未割当→ack / 未登録種別→ack / 検証失敗→ack / transient handler→nack / dedupe 永続化失敗→nack / 正常→handler 1 回 + dedupe 記録）
  - _Requirements: 1.1, 1.2, 1.4, 2.1, 2.2, 2.3, 2.4, 2.5, 2.6, 3.1, 3.2, 3.3, 5.1, 5.2, 5.3, NFR 1.1, NFR 2.1, NFR 2.2, NFR 3.1_
  - _Boundary: NotificationDispatcher_
  - _Depends: 2.1, 2.2, 3.1, 1.1_

- [ ] 4. 退避キュー閲覧 API と cmd/api 配線
- [ ] 4.1 NotificationAdminHandler と routers.Admin への Mount
  - `internal/notification/admin_handler.go`: chi.Router を内包する `NewAdminHandler(queue UnassignedQueue, log)`。`GET /api/admin/notifications/unassigned` を root 相対 `Get("/", ...)` で登録（`audit/admin_handler.go` と同型）。query parse（from/to は RFC3339、type は ENROLLMENT/STATUS_REPORT/COMMAND 許可値検証）→ 不正は 400 + 不正項目提示（Req 4.5）。SuperAdmin TenantContext を `db.WithTenantContext` で確立 → `UnassignedQueue.List` → JSON 応答（0 件は 200 + `[]`）。401/403 は middleware 責務でハンドラ再実装しない（Req 4.3 / 4.4）。List の DB 失敗は 503。機密値を補間しない（NFR 3.1）
  - `cmd/api/main.go`: `notification.NewAdminHandler(...)` を構築し `routers.Admin.Mount("/notifications/unassigned", h)`（実 path `/api/admin/notifications/unassigned`、`RequireAdminConsoleAndSuperAdmin` ガード継承）。audit/tenant の (7)(8) 配線ブロックと同パターン
  - `internal/notification/admin_handler_test.go`: filter parse 正常 / 不正 from・to・type（400 + 不正項目）/ 200 + 空 `[]`（mock queue）。配線 smoke は task 5 の結合テストでカバー
  - _Requirements: 4.1, 4.2, 4.3, 4.4, 4.5, NFR 3.1_
  - _Boundary: NotificationAdminHandler_
  - _Depends: 3.1_

- [ ] 5. dispatch 経路の結合テスト（実 PostgreSQL）
- [ ] 5.1 notification_dispatch 結合テスト
  - `backend/test/integration/notification_dispatch_test.go`: 既存 helpers_test.go の requireDBURLs / migration 適用パターンに倣う。subscriber を介さず `Dispatcher.Handle` を直接駆動（Pub/Sub emulator 依存を避ける）。実 dedupe/unassigned repository + 実 tenant 逆引き、種別 handler は呼び出し回数を数える mock
  - 同一 MessageID を 2 回 Handle → 種別 handler 呼び出しが 1 回のみ（Req 6.1）
  - 未登録 enterprise_name の通知 → `unassigned_notifications` に INSERT され ack（Req 6.2）
  - ENROLLMENT / STATUS_REPORT / COMMAND が各 mock handler へ振り分け（Req 6.3）
  - 退避済み通知が admin_handler 経由（test server に Mount）または `UnassignedQueue.List` で取得可能、from/to/type 絞り込み（Req 6.4）
  - bound テナントの enterprise_name 解決時に tenant context が確立され handler が呼ばれること（Req 3.1 経路の結合確認）
  - DATABASE_URL 未設定環境では t.Skip（false-fail させない）
  - _Requirements: 6.1, 6.2, 6.3, 6.4_
  - _Boundary: NotificationDispatcher, NotificationDedupe, UnassignedQueue, NotificationAdminHandler, TenantIDByEnterpriseName_
  - _Depends: 3.2, 4.1_

## Verify

本 spec の実装後、watcher（stage-a-verify gate）が再実行すべき verify コマンドを宣言する。
結合テスト（`backend/test/integration`）は DB 未設定環境では各テストが t.Skip するため build に
含めて差し支えない。

<!-- stage-a-verify -->
```sh
cd backend && go build ./... && go vet ./... && go test ./internal/notification/... ./internal/tenant/...
```
