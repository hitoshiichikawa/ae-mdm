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
package policy
