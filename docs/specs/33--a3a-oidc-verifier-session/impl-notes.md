# Implementation Notes

本ファイルは Issue #33（A3a: OIDC Verifier + Session 管理）の per-task 実装ループにおける
learning を `### Task <id>` 単位で追記する。`docs/specs/33--a3a-oidc-verifier-session/` 配下の
`requirements.md` / `design.md` / `tasks.md` は実装 PR では書き換えず、矛盾は本ファイル末尾の
「確認事項」節に記録する。

## Implementation Notes

### Task 1

- **採用方針**: タスク `1` は umbrella header（`_Requirements:_` / `_Boundary:_` を持たない親 task）
  であり、本起動では直接の実装は無く、子 task 1.1〜1.4（Config 拡張 / migration / httpserver 公開化 /
  logger redaction）が後続 iteration で順次実装される前提として `## Implementation Notes` 構造のみ
  整備する。
- **重要な判断**:
  - 親 task は `tasks.md` 上で `### Task 1` の learning スロットを成立させるためのプレースホルダ
    に留め、コード変更・migration 追加・テスト追加は本 iteration では行わない（実装本体は 1.1〜
    1.4 の各 fresh iteration が担当する設計）。
  - per-task ループ規約「1 commit = 1 task ID」に従い、本 iteration の marker commit は `docs(tasks):
    mark 1 as done` 単一の subject で `tasks.md` のみを含める。impl-notes.md セットアップは marker
    commit と分離した別 commit に積む。
- **残存課題**: 子 task 1.1（Config）/ 1.2（migration）/ 1.3（httpserver 公開化）/ 1.4（logger
  redaction）の各実装は後続 fresh iteration で消化する。子 task 全完了時に親 task `1` の昇格は
  既に本 iteration で完了済みのため、子完了時の auto-promotion 規約は no-op として扱う。

### Task 1.1

- **採用方針**: 既存 `backend/internal/config/env.go` のパターン（`requiredStr` / `optionalStr` /
  `intWithDefault` / `validateURL` / `buildConfigError`）を踏襲し、`durationWithDefault` helper を
  追加 + cross-field validation を `loadFrom` 最終段に集約することで、`*errors.Error{Code:
  CodeConfigInvalid}` の fail-fast を維持した。`Config` 構造体には `SessionIdleTimeout` /
  `SessionAbsoluteTimeout` / `StateCookieTTL` / `StateMACSecret` / `OIDCTenantClientSecret` /
  `OIDCAdminClientSecret` の 6 フィールドを追加し、`time` package import で `time.Duration` を解決。
- **重要な判断**:
  - **境界値は定数化**: `sessionTimeoutMin = 1*time.Second` / `sessionTimeoutMax = 24*time.Hour` /
    `stateCookieTTLMin = 1*time.Second` / `stateCookieTTLMax = 10*time.Minute` を `env.go` 冒頭に
    集約。マジックナンバー回避と将来の運用調整 (`NFR 2.1`) の両立。
  - **機密値の非埋込**: STATE_MAC_SECRET / OIDC client secret の生値はエラーメッセージに含めず、
    key 名と長さ (length=N) のみ surface する (NFR 1.1)。client secret 同値 reject のテスト
    `TestLoad_OIDCClientSecret_SameAcrossConsoles_Rejected` で `strings.Contains(de.Message,
    sharedSecret)` が false を assertion し、本契約を回帰的に守る。
  - **cross-field validation の実行タイミング**: `missing / invalid` を抜けた段階で実行する
    （前提が揃ってから比較）。これにより「duration parse 失敗 + idle > absolute」のような
    重複検出を避け、エラーメッセージを 1 つの原因に絞れる。
- **残存課題**:
  - 後続 task 1.4: `logger redaction allowlist` に `state_mac_secret` / `client_secret` /
    `state_cookie` / `session_cookie` を追加して、構造化ログでの値漏洩を二次防御する。
  - 後続 task 5.1: 本 Config を `auth.Service` / `oidc.Verifier` に DI し、`StateMACSecret`
    から HMAC 鍵を導出、`OIDCTenant/AdminClientSecret` を `oauth2.Config.ClientSecret` に
    注入する配線を実施する。
  - 確認事項: design.md の `.env.example` 仕様で
    `OIDC_TENANT_REDIRECT_URL=http://localhost:8080/api/auth/callback` を要請しているが、
    Keycloak realm export 側 client の登録 redirect URI もこの値と一致する必要がある。
    realm export 側更新は本 task のスコープ外（後続 task 7.1 で `docs/runbook/local-dev.md` に
    手順を追記する想定）。

## 確認事項

本セクションは `requirements.md` / `design.md` / `tasks.md` 本文の書き換えを伴わずに、実装フェーズ
で気付いた spec との矛盾点・人間判断が必要なポイントを集約する場である。

- なし（本 iteration は親 task の administrative iteration のみで、design 本文との矛盾点は発生して
  いない）。
