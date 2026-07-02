# Implementation Plan

> 既存の `devices` テーブル（migration 0006）/ `authz` マトリクス（`ResourceDevice` read 済み）/
> Notification Dispatcher（#39）/ config（`DeviceSyncDelayThresholdHours`）は **呼ぶのみ**で変更しない。
> 新規ファイルは `backend/internal/device/` 配下と `backend/internal/notification/status_handler.go`。
> `cmd/worker/main.go` は変更しない（handlers map 登録は #36 の責務 / design.md Risks）。
> 依存方向: `device → notification`（StatusReport 型 / port を参照）。逆は発生させない。

- [ ] 1. データモデル拡張（migration 0018）(P)
  - `backend/db/migrations/0018_devices_applied_policy_name_and_compliance_index.up.sql`:
    `ALTER TABLE devices ADD COLUMN IF NOT EXISTS applied_policy_name text`（nullable / STATUS_REPORT 報告値 / Req 7.1・2.1）
  - 同 up: `CREATE INDEX IF NOT EXISTS idx_devices_tenant_compliance ON devices(tenant_id, compliance_status)`（分類フィルタ一覧 p95<1s / NFR 1.1）
  - `0018_...down.sql`: index DROP + column DROP（reversible）
  - migrate up/down が既存 0017 の後段で冪等に通ることを確認（migration 番号衝突リスクは PR 確認事項へ / design.md Risks）
  - schema-only task。振る舞い/性能テストは後続へ deferred（7.1→task 6 の結合テスト / 2.1→task 4・7 / NFR 1.1→task 8.1 の EXPLAIN）
  - _Requirements: 7.1, 2.1, NFR 1.1_
  - _Requirements_partial: 7.1, 2.1, NFR 1.1_
  - _Boundary: device.Repository_
- [ ] 2. device ドメイン型・骨格（types.go / clock.go / doc.go）(P)
  - `types.go`: `ComplianceStatus`（compliant/non_compliant/unknown/unsupported / Req 3.1・3.4）・`DeviceMode` enum、`ListFilter`・`DeviceSummary`・`DeviceDetail`（jsonb 空 default `{}`/`[]` / Req 2.5）・`TenantOverview`・`TenantComplianceCount`・`DeviceRow`・`StatusApplyInput`・sentinel `ErrDeviceNotFound`（404 汎用 message）
  - `clock.go`: `Clock` interface + `SystemClock`（`auth.Clock`/`audit.Clock` と同型 / 同期遅延判定の時刻注入）
  - `doc.go`: package 責務・依存方向（`device → notification` 許可 / RLS 消費方針 / write 経路は StatusApplier のみ）
  - 型の zero-value / JSON tag / enum 値集合（4 分類に unsupported を含む）を検証する単体テストを近傍に追加（同 task 内テスト / Req 3.1・3.4・2.5 の宣言面）
  - _Requirements: 3.1, 3.4, 2.5_
  - _Boundary: device.Service, device.Repository, device.Handler, device.StatusApplier_
- [ ] 3. device Repository（pgx + RLS）+ テスト
  - `repository.go`: `ListByTenant`（filter + `syncCutoff` で `WHERE compliance_status/mode/last_status_at<cutoff` + `LIMIT/OFFSET` / Req 1.1〜1.5）・`GetByID`（0 行 → `ErrDeviceNotFound` / Req 2.4・5.1・5.2）・`AggregateOverview`（SuperAdmin ctx 信頼 / `GROUP BY tenant_id, compliance_status` / 任意 tenant_id 絞り込み / Req 6.1・6.3・6.4）・`UpdateFromStatusReport`（`WHERE amapi_device_name=$` の `COALESCE($n, col)` 部分更新 / affected 返却 / Req 7.2）
  - tenant-scoped メソッドは ambient TenantContext のまま `db.BeginTxFunc`（昇格しない / `policy.Repository` 手本）。列ごと型付き scan（jsonb→`json.RawMessage`, nullable→pointer）。0 件は非 nil 空 slice（Req 1.7）
  - `repository_test.go`（実 DB / `DATABASE_URL` 未設定は `t.Skip`）: 他テナント行の一覧非可視・詳細 0 行 → NotFound（存在秘匿 / Req 5.1・5.2・NFR 3.1）、COALESCE 部分更新で欠落フィールドの既存値保持（Req 7.2）、分類/mode/sync フィルタが該当行のみ（Req 1.2〜1.4）、集計と tenant_id 絞り込み（Req 6.1・6.3・6.4）
  - _Requirements: 1.1, 1.2, 1.3, 1.4, 1.5, 1.7, 2.4, 5.1, 5.2, 6.1, 6.3, 6.4, 7.2, NFR 3.1_
  - _Depends: 1, 2_
- [ ] 4. device Service（read）+ 単体テスト
  - `service.go`: `List`/`Get`/`Overview` 実装。`syncCutoff = Clock.Now() - config 閾値` を算出し filter と per-row `SyncDelayed` に共用。`isSyncDelayed`: `lastStatusAt!=nil && lastStatusAt.Before(cutoff)`（strict `<` / 閾値ちょうど非遅延 / Req 4.3、NULL は非遅延）
  - `Get` は `DeviceRow`→`DeviceDetail` 写像（mode/applied_policy_name/HW/SW/最終同期/compliance/installed_apps/sync flag / Req 2.1・2.2・2.3・2.5）。stored `compliance_status` を 4 分類でそのまま返却（未観測は 'unknown' / Req 3.1・3.3）+ 非準拠理由併記（Req 3.2）
  - `Overview` は flat 件数行を tenant 単位に畳み込み 4 分類 0 埋め（全体 0 件 → 空 slice / Req 6.1・6.4）。write メソッドは持たない（Req 7.3 の型担保）
  - `service_test.go`: `isSyncDelayed` 境界（超過/ちょうど/NULL / Req 4.1・4.2・4.3）、詳細写像の空属性（Req 2.5）、overview 畳み込みと空集計（Req 6.4）（同 task 内テスト）
  - _Requirements: 1.7, 2.1, 2.2, 2.3, 2.5, 3.1, 3.2, 3.3, 4.1, 4.2, 4.3, 6.1, 6.4_
  - _Depends: 3_
- [ ] 5. notification StatusHandler + DeviceStatusWriter port + payload parse + 単体テスト (P)
  - `backend/internal/notification/status_handler.go`: `DeviceStatusWriter` port・`StatusReport` 値オブジェクト（任意フィールドを pointer / `*json.RawMessage` で保持 / Req 7.2）・`StatusHandler`（`NotificationHandler` 実装）・`NewStatusHandler(w DeviceStatusWriter, log)` を定義
  - `Handle`: `Envelope.Payload` の AMAPI Device JSON を `StatusReport` へパースし `DeviceName`（=amapi_device_name）を抽出、`w.ApplyStatusReport` を 1 回呼ぶ。空 payload / malformed JSON / `DeviceName` 空は `*errors.Error{IsTransient:false}`（破棄 ack / Verifier 分類方針と整合）。機密値を error 文言・ログに補間しない（NFR 3.1）
  - `notification` は device を import しない（doc.go 不変条件を維持）。tenant ctx は Dispatcher が確立済み前提
  - `status_handler_test.go`: 全フィールド有 / 一部欠落（pointer nil）/ 空 payload・malformed → 破棄 ack / `DeviceName` 空 → 破棄 ack（同 task 内テスト / failure path / Req 7.2）
  - _Requirements: 7.1, 7.2_
  - _Boundary: notification.StatusHandler_
- [ ] 6. device StatusApplier（write）+ 結合テスト
  - `status_applier.go`: `notification.DeviceStatusWriter` を実装（`device → notification` import）。`StatusReport`→`StatusApplyInput` 写像。compliance 算出は `NonComplianceDetails` が **payload に存在**する時のみ（空→compliant / 非空→non_compliant + 理由 / 欠落→未更新 / Req 3.2・7.2）。`unsupported` は書き込まない（Open Q）。`Repository.UpdateFromStatusReport` へ委譲、`affected=0`（未登録端末）は no-op ack + WARN（Open Q）
  - `status_applier_test.go`（単体）: compliance 3 分岐、部分保持、affected=0 no-op（同 task 内テスト）
  - `backend/test/integration/device_test.go`（実 DB / Skip 可）: test-only 配線で `StatusHandler → StatusApplier → Repository` を通し、STATUS_REPORT 適用後に `Service.Get` が反映を返す read-after-write（NFR 2.1）+ 部分 payload で既存値保持（Req 7.1・7.2）
  - _Requirements: 7.1, 7.2, 3.2, NFR 2.1_
  - _Depends: 3, 5_
- [ ] 7. device Handler（tenant-console）+ テスト
  - `handler.go`: `GET /api/devices`（一覧）・`GET /api/devices/{id}`（詳細）。`authorize` は own-tenant `ResourceDevice`×`ActionRead`（claims 不在 401 / deny 403 / `policy.Handler` 手本）。`parseListFilter`: 未定義 enum 値は 400（Req 1.6）、`page`/`page_size` 既定 1/50・上限 200 clamp・非数値は 400。詳細不在/越境は `ErrDeviceNotFound`→404（存在差非露出 / Req 5.1・5.2）。JSON encode / parseID は `policy.Handler` と同方式。`chi.Router` 内包で `Mount("/devices", h)` 対応。**write endpoint を公開しない**（Req 7.3）
  - `handler_test.go`: parseListFilter 400（Req 1.6）、未認証 401 / deny 403、不在・越境 404 が同一応答（Req 5.1・5.2）、GET のみで write route 不在（Req 7.3）（同 task 内テスト / parse failure・existence-hiding）
  - _Requirements: 1.1, 1.6, 2.1, 2.4, 3.4, 5.1, 5.2, 7.3_
  - _Depends: 4_
- [ ] 8. device AdminHandler（overview）+ cmd/api 配線 + テスト
  - `admin_handler.go`: `GET /api/admin/devices/overview`。`RequireAdminConsoleAndSuperAdmin` 配下 Mount 前提 + `authz` cross-tenant `ResourceDevice read` 二重防御（`audit.AdminHandler` probe-tenant 手本 / Req 6.2）。SuperAdmin `TenantContext` を `db.WithTenantContext` で確立してから `Service.Overview`。`tenant_id` query 任意（不正書式 400 / Req 6.3）。空集計は `[]` で 200（Req 6.4）
  - `cmd/api/main.go`: `buildDeviceHandler` / `buildDeviceAdminHandler` helper を追加し（pool/authorizer/config/log は既存構築済みを再利用）、`routers.API.Mount("/devices", ...)` と `routers.Admin.Mount("/devices/overview", ...)` を配線（既存 (7)〜(10) ブロックと同パターン）
  - `admin_handler_test.go`: 非 SuperAdmin / tenant-console aud → 403（Req 6.2）、tenant_id 絞り込み（Req 6.3）、空集計 200（Req 6.4）。`cmd/api/main_test.go`: `buildDeviceHandler` 型レベル回帰（`buildPolicyHandler` 手本）（同 task 内テスト）
  - _Requirements: 6.1, 6.2, 6.3, 6.4_
  - _Depends: 4, 7_
- [ ]* 8.1 NFR 1 index 妥当性の確認 + 一覧 E2E 補完（task 1 の deferred 性能テストを解消）
  - 分類フィルタ一覧 SELECT が `idx_devices_tenant_compliance` を使うことを `EXPLAIN` で手動確認（task 1 の `_Requirements_partial:_` NFR 1.1 を解消 / NFR 1.1）
  - tenant-console 一覧の end-to-end（認証 → フィルタ/ページング → 応答）を coverage 補完として追加
  - _Requirements: NFR 1.1_

## Verify

本 spec の実装後、watcher（stage-a-verify gate）が再実行すべき verify コマンドを宣言する。

<!-- stage-a-verify -->
```sh
cd backend && go build ./... && go test ./internal/device/... ./internal/notification/... ./cmd/api/...
```
