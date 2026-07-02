// Package policy は ae-mdm のポリシー入力検証（Policy Validator / Issue #36 / B2a）の
// ドメイン層を提供する。
//
// テナント管理者が作成・更新する Android Management ポリシーを AMAPI へ反映する前に、
// 5 領域（アプリ / パスワード / セキュリティ / システム更新 / Kiosk）の設定値が妥当である
// ことを検証する純粋関数層である。本 package は umbrella #24 Requirement 4 のうち、検証
// ロジック部分のみを切り出す（永続化 / 割当 / AMAPI patch / UI は別 Issue の責務）。
//
// # 純粋性（NFR 1）
//
// Validator は外部副作用を持たない純粋関数中心の設計である。DB / HTTP / ファイル /
// システム時刻のいずれにも依存せず、同一入力に対して常に同一の検証結果を返す（決定論的）。
// 入力は本 package が定義するドメイン入力型（PolicyInput / 各領域 struct）であり、
// amapi.PolicyBody（Raw map）には依存しない（Raw map → ドメイン型の変換は Policy Service の
// 責務であり本 Issue 対象外）。
//
// # 依存方向ルール
//
// 本 package が import してよいのは以下のみ。amapi / 上位 service / cmd は import しない
// （純粋関数層として隔離し、上位レイヤから単方向に呼ばれる）:
//
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/errors（検証エラーの Code 区別）
//   - 禁止: amapi / 上位 application（policy service 等）/ cmd / 他 domain への import
//
// # 構成（task 8.1 時点）
//
//   - doc.go        : 本 package ドキュメント（依存方向ルール / 純粋性契約）
//   - types.go      : ドメイン入力型（PolicyInput / 5 領域 struct / enum）と検証結果型
//     （ValidationResult / ValidationError / Domain / ErrorKind）
//   - validator.go  : table-driven な rule struct（rule）と Validate エントリポイント。
//     各領域の検証規則を rule のスライスとして宣言し、複数の不正項目を 1 つの
//     ValidationResult に収集する
//   - validator_test.go : 5 領域 × 正常系 / 異常系 / 境界値の単体テスト（table-driven）
//
// # 検証エラーの機械可読性（Req 6）
//
// 各検証エラー（ValidationError）は「不正な領域（Domain）」「対象フィールド（Field）」
// 「種別（Kind: business rule 違反 / invalid field）」を機械可読に保持する。種別は
// internal/errors の Code（CodeBusinessRule=422 / CodeInvalidRequest=400）に対応づき、
// 呼び出し側（Policy Service）が適切な HTTP ステータスへマッピングできる。
//
// # application 層（Policy Service / Issue #40 / B2b）
//
// 本 package には、上記の純粋 Validator 層に加えて、ポリシーの upsert / 割当 / 参照 / 削除を
// オーケストレーションする application 層が同居する。Validator が純粋関数層であるのに対し、
// application 層は DB / AMAPI / 監査記録という外部副作用を伴うユースケース層であり、両者は
// **同一 package 内の別レイヤ**として共存する（Validator の純粋性契約は application 層の追加に
// よって変化しない。application 層は Validator を呼び出すのみで、Validator が外部依存を獲得する
// ことはない）。
//
// application 層のファイル構成（各ファイルの責務）:
//
//   - service_types.go : application 層 DTO（PolicyRow / PolicyView / PolicySummary /
//     PolicyRequest / AssignInput）と sentinel error（ErrPolicyNotFound=404 /
//     ErrDeleteConflict=409）を定義する。
//   - mapper.go        : Raw JSON body → ドメイン入力型（PolicyInput）への変換と、検証通過後の
//     amapi.PolicyBody pass-through 組み立て（BuildPolicyBody）を担う。型不整合 / 必須キー欠落は
//     invalid field 相当の ValidationError に写像する。
//   - service.go       : upsert / 割当 / 参照 / 削除のユースケースと検証ゲート・AMAPI
//     オーケストレーション・監査記録の単一所有者（Service interface / 6 メソッド）。
//     consumer-defines-interface の最小 port（upsertClient / eventRecorder /
//     enterpriseResolver）で amapi / audit / tenant の各 Service を受け取る。
//   - repository.go    : policies の CRUD と devices.applied_policy_id UPDATE を raw pgx + RLS で
//     集約する（tenant-scoped context のまま SuperAdmin 昇格しない）。
//   - handler.go       : /api/policies 配下 6 endpoint の HTTP I/O + RBAC 判定（authz）+ エラー
//     写像を担う presentation 層。httpserver を import するのは Handler のみ。
//
// # application 層の依存方向ルール
//
// application 層が import してよいのは以下のみ。cmd は import しない（上位レイヤから単方向に
// 配線・呼び出される）:
//
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/platform/amapi（AMAPI 反映の最小 port）
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/audit（監査記録の最小 port）
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/tenant（enterprise_name 解決の最小 port）
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/platform/authz（own-tenant RBAC / Handler のみ）
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/errors（Code 付きエラー写像）
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/logger（拒否経路の構造化ログ）
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/platform/httpserver（claims 取得 / HTTP 写像 / Handler のみ）
//   - 禁止: cmd への import（DI 配線は cmd/api 側が application 層を呼び出す単方向）
//
// なお上記「# 依存方向ルール」節（純粋 Validator 層が amapi / 上位 service / cmd を import しない
// 契約）は本 application 層の追加によって変化しない。Validator 層と application 層が同一 package に
// 同居しても、Validator のソース（types.go / validator.go）は引き続き application 層・amapi・cmd を
// import しない純粋関数層として隔離される。
package policy
