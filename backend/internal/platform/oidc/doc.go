// Package oidc は ae-mdm の認証基盤（A3a / Issue #33）で使う OIDC ID トークン
// 検証ロジックを提供する。`coreos/go-oidc/v3` の `oidc.Provider` + `oidc.RemoteKeySet` を
// tenant-console / admin-console の 2 issuer 分構築し、ID トークンの iss / aud / exp /
// 署名を一元検証する。
//
// # 依存方向ルール
//
// 本 package は他の internal package（auth / platform/db / platform/httpserver 等）を
// import しない。横断的 platform 機能として errors / config / logger のみに依存し、
// 上位の auth domain からのみ呼び出される（design.md 「OIDC Verifier」節および
// 「auth claims の package 境界」節と整合）。
//
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/errors
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/config
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/logger
//   - 禁止: github.com/hitoshiichikawa/ae-mdm/internal/auth ほか上位 / 横並び domain
//
// # aud 排他一致の責務
//
// go-oidc 内蔵の aud 検証（`Config.ClientID` / `SkipClientIDCheck`）は
// `&oidc.Config{ClientID: "", SkipClientIDCheck: true}` を `provider.Verifier(cfg)` に
// 渡して **明示的に切り**、aud の判定は本 package が `Claims.MatchedConsole` として
// 行う。tenant-console / admin-console のいずれか **排他一致** を強制し、両方含む場合は
// `aud_ambiguous` で reject する（requirements.md Req 1.4 / 1.5）。
//
// # 機密値の非埋込契約
//
// id_token raw JWT や `cfg.OIDCTenantClientSecret` / `cfg.OIDCAdminClientSecret` /
// `cfg.StateMACSecret` を `*errors.Error.Message` や Cause メッセージ本文に文字列補間
// しない（design.md「Error Handling」節 / NFR 1.1 / NFR 4.2）。go-oidc の error は
// そのまま wrap し、追加 context は `failure_kind` / `console` 種別 / `issuer` 等の
// 非機密値のみを surface する。
package oidc
