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

### Task 1.2

- **採用方針**: 0013（sessions 拡張）/ 0014（admin_users.oidc_issuer + 複合 UNIQUE 化）/
  0015（state_nonces 新規）の 3 migration ペアを tasks.md L99〜L173 の指示通り対称な up/down
  で実装。値保全が必要な rename（`idle_at` → `last_seen_at`）は ADD→UPDATE→DROP の 3 段手順で
  実施し、down も対称に巻き戻すことで `migrate-up && migrate-down && migrate-up` の整合性を
  確保した。
- **重要な判断**:
  - **既存 UNIQUE 制約名の特定**: 0002 では `oidc_subject text NOT NULL UNIQUE` という inline
    宣言で UNIQUE 制約を作っており、Postgres が自動付与する制約名は `<table>_<column>_key` =
    `admin_users_oidc_subject_key` と確定できる。0014 ではこの名称を明示して DROP し、down
    では同名で復元することで「up したら別名・down したら違う形」という非対称を防いだ。
  - **複合 UNIQUE 制約名は明示**: 0014 で新規追加する `(oidc_issuer, oidc_subject)` 複合
    UNIQUE は `ADD CONSTRAINT admin_users_oidc_issuer_subject_key UNIQUE (...)` と制約名を
    明示して付与する。これにより後続 task の Repository から PgError.ConstraintName による
    UNIQUE 違反の分類が安定して可能になる。
  - **`oidc_issuer` の backfill 戦略は空文字 + fail-closed を採用**: design.md / tasks.md
    の指示通り `DEFAULT ''` で backfill し、空文字は実 issuer URL と一致しないため
    `ResolveAdminUser` で 403 `admin_user_not_provisioned` に倒れる。environment-specific な
    literal placeholder URL を migration に埋める案も検討したが、本番 / dev / test で異なる
    issuer URL が必要な運用に migration 側で対応するのは越権なので採用しない（admin-seed CLI /
    管理 UI 経由で正しい URL を上書きするまでログイン不可、という意図された fail-closed 挙動）。
  - **`console` 列の backfill は `'tenant-console'`**: A2 時点で稼働する session は tenant 用のみ
    という前提で `DEFAULT 'tenant-console'` で backfill 後 DROP DEFAULT し、後続 INSERT では
    必ず明示指定を要求する形にした（test fixture も明示指定済み）。
  - **`state_nonces` の RLS ポリシー文法は既存 0011 と微差**: 0011 の SuperAdmin-only ポリシー
    （notification_dedupe 等）は `current_setting('app.is_superadmin', true)::boolean` を使うが、
    tasks.md L143〜L146 は `current_setting('app.is_superadmin', true) = 'true'` を指定している。
    後者は GUC 未設定時に NULL が返って `NULL = 'true'` が NULL（= falsy）に評価されるため
    同等の default-deny 挙動になる。tasks.md の指示を優先して `= 'true'` 形を採用した。
  - **`migrations_reversible_test.go` の対象に新規 migration を加えるかは task 6 の責務**:
    tasks.md L171〜L173 の通り本 task では更新せず、task 6 で `state_nonces` を `primaryTables`
    に追加し integration test で up/down 対称性を検証する前提。本 task ではローカルで
    `go build ./... && go vet ./...` のみ green を確認し、実 DB での migrate-up/down 検証は
    docker compose 環境が整っていないため未実施（後続 task 6 / CI 環境で実施される想定）。
- **残存課題**:
  - 後続 task 4.1: `Repository.ConsumeStateNonce` / `Repository.ResolveAdminUser` 実装時に、
    本 task で追加した `state_nonces` の PRIMARY KEY UNIQUE 違反（pgerrcode 23505 /
    `pgerrcode.UniqueViolation`）を `failure_kind: state_replay` にマッピングする SQLState
    handling を実装する。
  - 後続 task 4.1: `admin_users_oidc_issuer_subject_key` 複合 UNIQUE 制約は read-modify-write
    の `ResolveAdminUser` で `SELECT ... WHERE oidc_issuer = $1 AND oidc_subject = $2 FOR UPDATE`
    の form で活用される（INSERT は行わない / 事前 provisioning 必須）。
  - 後続 task 6: 本 task で追加した 3 migration ペアを `migrations_reversible_test.go` の
    `primaryTables` リストに追加し（少なくとも `state_nonces` を加える）、実 DB での up/down
    対称性を CI で検証できるようにする。
  - 確認事項: 既存 `oidc_subject UNIQUE` 制約名が `admin_users_oidc_subject_key` で正しい前提
    は、0002 が `oidc_subject text NOT NULL UNIQUE` の inline 宣言を使っている事実から
    Postgres の autogen 規則 `<table>_<column>_key` で確定できる（migrate `ALTER TABLE ...
    DROP CONSTRAINT admin_users_oidc_subject_key` が実 DB で成功するかは task 6 の reversibility
    テストで検証される / 本 task ではローカル build のみで担保）。

## 確認事項

本セクションは `requirements.md` / `design.md` / `tasks.md` 本文の書き換えを伴わずに、実装フェーズ
で気付いた spec との矛盾点・人間判断が必要なポイントを集約する場である。

- なし（task 1.2 では tasks.md L99〜L173 の指示通りに migration / test fixture を更新でき、
  design 本文との矛盾点は発生していない）。
