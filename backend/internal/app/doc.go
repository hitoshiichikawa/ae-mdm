// Package app は ae-mdm のアプリ配信ドメイン（App Service / Issue #11 / D1）の
// backend 層を提供する。
//
// テナント管理者（TenantAdmin）が業務アプリを Managed Google Play 上で承認し、自テナントの
// 端末群へ配信するための以下 4 ユースケースを担う（design.md「App Domain」節 / Requirement 1〜5）:
//
//   - Managed Google Play 承認 UI を iframe 表示するための webToken 発行（Req 1）
//   - 承認済みアプリカタログ（tenant_apps）の参照（Req 2）
//   - Managed Google Play 承認結果のカタログ同期（Req 3）
//   - ポリシー紐付け時の「承認済みカタログ内アプリのみ受付」不変条件の read seam（Req 5）
//
// 本 package は先行実装 internal/policy/（#40）と同一のレイヤ構成・依存方向・
// consumer-defines-interface・RLS テナント分離・共有 AMAPI ラッパ経由の外部呼び出しを踏襲する。
//
// # テナント分離（NFR 1.2 / Req 4.x）
//
// App Service / Repository は tenant_id を入力パラメータに取らず、Handler が claims から得た
// 自テナント文脈（tenant-scoped TenantContext）のまま操作する。Repository は SuperAdmin へ
// 昇格せず、RLS（tenant_isolation_tenant_apps / migration 0011）による自テナント限定に分離を
// 委ねる。他テナント行は SELECT で 0 行、INSERT/UPDATE は RLS WITH CHECK で物理拒否される。
//
// # 秘匿値の非露出（NFR 1.1 / NFR 3.2）
//
// iframe 埋め込み用 webToken の値（PlayTokenView.Value）は短命の秘匿値であり、構造化ログ・
// 監査 Detail に平文出力しない。サービスアカウント資格情報・OAuth トークンの生値も同様に
// ログへ含めない。
//
// # 依存方向ルール
//
// 本 package が import してよいのは以下のみ。cmd は import しない（DI 配線は cmd/api 側が
// 本 package を呼び出す単方向）:
//
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/platform/amapi（webToken 発行の最小 port）
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/tenant（enterprise_name 解決 / bind gate の最小 port）
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/audit（同期監査記録の最小 port）
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/platform/authz（own-tenant RBAC / Handler のみ）
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/errors（Code 付きエラー写像）
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/logger（拒否経路の構造化ログ）
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/platform/httpserver（claims 取得 / HTTP 写像 / Handler のみ）
//   - 禁止: cmd への import（DI 配線は cmd/api 側が本 package を呼び出す単方向）
//
// # 構成（ファイル責務）
//
// 本 package は internal/policy/ と同じ doc/service_types/service/repository/handler +
// co-located test のレイヤ構成を取る。各ファイルの責務は以下:
//
//   - doc.go            : 本 package ドキュメント（依存方向ルール / テナント分離・秘匿値契約）
//   - service_types.go  : DTO（PlayTokenRequest/View, SyncRequest/SyncApp/SyncResult,
//     TenantAppRow/TenantAppView）と sentinel error（ErrAppNotApproved=422）を定義する。
//   - repository.go     : tenant_apps（migration 0008）の List / Upsert / ApprovedPackages を
//     raw pgx + RLS で集約する（tenant-scoped context のまま SuperAdmin 昇格しない）。
//   - service.go        : webToken 発行 / カタログ参照・同期 / 承認済み read seam のユースケースと
//     検証ゲート・AMAPI/監査オーケストレーションの単一所有者。consumer-defines-interface の
//     最小 port（webTokenClient / enterpriseResolver / eventRecorder）で外部依存を受け取る。
//   - handler.go        : /api/play-tokens / /api/apps / /api/apps/sync の HTTP I/O + own-tenant
//     RBAC 判定（authz）+ エラー写像を担う presentation 層。httpserver を import するのは Handler のみ。
//
// なお本 Issue（#11）の task 1 で実装するのは doc.go / service_types.go / repository.go と
// その co-located test（repository_test.go）に限る。service.go / handler.go は後続 task
// （2〜5）で追加する。上記構成節はレイヤ全体像の説明であり、実装は各 task の範囲に閉じる。
package app
