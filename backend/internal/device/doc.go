// Package device は ae-mdm の端末インベントリ・ドメイン層（Issue #9 / C1 / umbrella #24
// Requirement 5 / 7.5 / NFR 1.2 / 3.2 のバックエンドスライス）を提供する。
//
// テナント管理者向けの端末一覧・詳細・コンプライアンス分類・同期遅延の可視化（読み取り系）と、
// SaaS 運営者向けの全テナント横断 overview、そして STATUS_REPORT 通知を契機とした端末属性の
// 更新を担う。端末属性は既存 devices テーブル（migration 0006 + 0019）を唯一の所有者とし、
// 新規テーブルは追加しない（design.md「Data Models」節と整合）。
//
// # 3 層構成と read/write 分離（Req 7.3）
//
// 本 package は既存ドメイン（policy / audit）と同じ 3 層構成を採る:
//
//   - Handler / AdminHandler : HTTP I/O + RBAC + エラー写像（presentation）
//   - Service（read）          : 一覧 / 詳細 / overview のユースケース + コンプライアンス分類・同期遅延判定
//   - StatusApplier（write）    : STATUS_REPORT の意味付け（compliance 算出）+ 端末属性の部分更新
//   - Repository               : devices への tenant-scoped 参照 / SuperAdmin 集計 / 部分更新（raw pgx + RLS）
//
// 読み取り（Service）と書込み（StatusApplier）を **別型に分離** し、HTTP Handler が参照する
// Service に書込みメソッドを一切持たせないことで、「HTTP 経由での端末属性の直接書込み不可」
// （Req 7.3）を型レベルの不変条件として担保する。端末属性の write 経路は StatusApplier のみである。
//
// # 依存方向ルール
//
// 本 package は以下のレイヤのみを import する。特に device → notification を **許可** する
// （StatusApplier が notification.DeviceStatusWriter port を実装し、notification.StatusReport 値
// オブジェクトを参照する。notification 側は device を import しない不変条件を維持するため
// 循環しない / design.md「Architecture Integration」節）:
//
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/notification（StatusReport / DeviceStatusWriter port）
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/platform/db（BeginTxFunc / WithTenantContext）
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/platform/authz（own-tenant / cross-tenant RBAC / Handler のみ）
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/config（同期遅延閾値 DeviceSyncDelayThresholdHours）
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/errors（Code 付きエラー写像）
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/logger（拒否 / no-op 経路の構造化ログ）
//   - 禁止: cmd への import（DI 配線は cmd/api / cmd/worker 側が device を呼び出す単方向）
//
// # RLS 消費方針
//
// tenant-scoped な参照（一覧 / 詳細）は ambient な TenantContext（Handler は Middleware 由来、
// StatusApplier は Dispatcher 由来）のまま db.BeginTxFunc で tx を開き RLS に分離を委ねる
// （**昇格しない** / policy.Repository と同方針）。全テナント横断 overview のみ SuperAdmin
// TenantContext（IsSuperAdmin=true）を db.WithTenantContext で確立してから集計する。いずれも
// 既存 devices（migration 0006 + 0019）と RLS tenant_isolation_devices（0011）を消費し、新規
// テーブル / RLS policy は追加しない。
//
// # 構成（task 2 時点）
//
//   - doc.go   : 本 package ドキュメント（責務 / 3 層構成 / read/write 分離 / 依存方向 / RLS 方針）
//   - types.go : enum（ComplianceStatus / DeviceMode）・API DTO（ListFilter / DeviceSummary /
//     DeviceDetail / TenantOverview）・DB 層型（DeviceRow / TenantComplianceCount /
//     StatusApplyInput）・sentinel error（ErrDeviceNotFound=404）を定義する。
//   - clock.go : Clock interface + SystemClock（同期遅延判定の時刻注入 / auth.Clock・audit.Clock と同型）。
//
// repository.go / service.go / status_applier.go / handler.go / admin_handler.go と各テストは
// 後続 task（3 以降）の責務。notification.StatusHandler（DeviceStatusWriter port の定義側）と
// StatusReport 値オブジェクトは notification package・task 5 の責務。
package device
