// Package auth は ae-mdm の認証基盤（A3a / Issue #33）のドメイン層を提供する。
// OIDC ID トークン検証（`internal/platform/oidc`）を経た Claims から内部 Identity を解決し、
// state cookie / session cookie の発行・検証・失効を担当する。
//
// # 依存方向ルール
//
// 本 package は以下のレイヤのみを import する。逆方向（platform 系から auth domain への
// import）は禁止（design.md「Domain Layer (Auth)」節および「auth claims の package 境界」
// 節と整合）:
//
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/platform/oidc
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/platform/db
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/platform/httpserver
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/logger
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/errors
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/config
//   - 禁止: 上位 application / cmd / 他 domain への直接 import
//
// # 構成（task 3.1 時点）
//
//   - types.go     : Identity / Session のドメイン型
//   - clock.go     : Clock interface と SystemClock 実装（DI 境界）
//   - state.go     : state cookie の Sign / Verify / CookieAttributes /
//     ExpireCookieAttributes / StatePayload / failureKind sentinel
//   - state_test.go: state cookie の単体テスト
//
// # 機密値の非埋込契約
//
// state MAC 鍵（`cfg.StateMACSecret`）/ state cookie 生値 / session cookie 生値 /
// id_token raw JWT を `*errors.Error.Message` や Cause メッセージ本文に
// 文字列補間しない（NFR 1.1 / NFR 4.2 / design.md「機密値を Cause メッセージ本文に
// 埋め込まない実装契約」節）。redaction は二次防御であり、本 package の各失敗パスは
// メッセージ本文に機密値を含めない一次防御を守る。
package auth
