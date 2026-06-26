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

### Task 1.3

- **採用方針**: `backend/internal/platform/httpserver/middleware.go` で A2 から private だった
  3 シンボル（`authClaims` 型 / `withAuthClaims` 関数 / `authClaimsFromContext` 関数）を
  public 化（`AuthClaims` / `WithAuthClaims` / `AuthClaimsFromContext`）し、フィールド構成
  および default deny / 401 / 403 chain の挙動を一切変えずに rename を完了。`authClaimsCtxKey`
  は外部から直接 ctx.WithValue で型衝突 / 上書きされる経路を生まないよう private 維持
  （tasks.md L181 と整合）。
- **重要な判断**:
  - **doc comment の歴史的記述の整理**: `AuthClaims` の godoc は「A2 では同型が private
    （`authClaims`）であり、test 用にのみ内部 helper が露出していた」という背景を 1 文だけ
    残し、後続 auth domain（`backend/internal/auth`）からの注入経路が確立した旨を明示する。
    `TenantContextMiddleware` の godoc 内にあった「auth スタブが `*authClaims` を注入する
    前提」の暫定文言は、A2 直後の暫定状態を指す散文だったので削除し、Issue #33 auth
    middleware を正式な上流として記述に置換した。
  - **同 package 外の test も rename に追随**: tasks.md L181〜L182 は「同 package 内の test
    呼び出し箇所」のみを rename 対象と明示するが、`backend/test/integration/
    http_subrouter_mount_test.go` の prose 1 箇所が「internal package private な authClaims
    を package 外から注入できない」と stale 状態になっていたため、test 挙動（TenantContext
    bypass 経路）はそのままに、bypass の **理由** を「RequireSuperAdmin 単体挙動の境界網羅」
    に書き換える doc accuracy fix を同 commit に同梱した（test の期待値・bypass の選択は
    不変）。テスト logic / RequireSuperAdmin の判定経路には触れない（tasks.md L183「既存挙動
    は不変であること（test の期待値は変えない）」を守る）。
  - **検証手段**: `go build ./... && go vet ./... && go test ./...` の 3 系統で rename
    完全性を担保。`grep -rn "\bauthClaims\b\|\bwithAuthClaims\b\|\bauthClaimsFromContext\b"`
    で残った 1 件（middleware.go の godoc 内 historical 言及）が新 godoc の意図通り
    backtick 引用で参照される historical literal であることを目視確認し、stale 参照ゼロを
    確認した。
- **残存課題**:
  - 後続 task 4.1 / 5.1 / 6.1: 本 task で public 化した `httpserver.WithAuthClaims(ctx,
    AuthClaims{...})` は、auth middleware 本体（`backend/internal/auth/middleware.go`）が
    session lookup 成功後に呼び出す入力契約として使われる。`AuthClaims` のフィールド構成
    （TenantID / AdminUserID / Roles / IsSuperAdmin）は本 task で不変としたため、auth
    middleware は `Identity` struct（後段 task 3.1 で定義）から 4 フィールドを転記する単純
    アダプタとして実装される想定。
  - 確認事項: 本 task では tasks.md L183 の「test の期待値は変えない」契約を守るため、
    `http_subrouter_mount_test.go` の bypass 経路（`platformdb.WithTenantContext` を直接
    呼ぶ）は維持した。本テストは後続 Issue で `httpserver.WithAuthClaims` 経路に書き換える
    余地があるが、書き換えると RequireSuperAdmin 単体ではなく auth middleware chain 全体を
    通すテストになるため、責務が変質する。本 task では責務 1 件のテスト構造を維持する
    判断を採用した。

### Task 1.4

- **採用方針**: `backend/internal/logger/redact.go` の `redactKeySubstrings` slice に
  `state_mac_secret` / `client_secret` / `state_cookie` / `session_cookie` の 4 件を追加し、
  既存意味カテゴリ（cookie 系の隣接 / generic secret 系）に従って配置。test は
  `TestRedactFields_RedactsSecretsBySubstring` の case 表に追加 4 件の完全一致行を追加 +
  新規 `TestRedactFields_AuthAllowlistAdditions` で prefix / suffix / 中央埋込 /
  case-insensitive / cookie 重複カバレッジの substring 一致観点を独立 allowlist として明示。
- **重要な判断**:
  - **独立 allowlist 化の動機**: `cookie` substring が既に `session_cookie` /
    `state_cookie` を substring 一致でカバーするが、tasks.md L189〜L194 の指示通り 4 件を
    独立 allowlist として明示することで「auth ドメインの cookie 名を意味的に
    明示」する責務を可視化した。これは将来 `cookie` substring が縮小される場合の
    回帰耐性（auth cookie の redaction が確実に残る）も兼ねる。
  - **変更影響範囲は 1 箇所に閉じる**: 既存 redaction 経路（`redactZapField` /
    `redactFields` / `redactCauseString`）はすべて `redactKeySubstrings` を
    `shouldRedact()` 経由で参照するため、allowlist 列挙 1 箇所の追加で全経路に波及する。
    `freeTextRedactKeys` / `causePatternReplacers` は本 task の責務外（cause 文字列の
    部分置換は別系統 / `client_secret` 文字列補間禁止は実装契約側の責務）。
  - **red→green 手順を確実に踏んだ**: 先に test を追加して `go test ./internal/logger/...`
    で 8 ケース fail を確認（`state_mac_secret` / `client_secret` 系のみ fail し、
    `state_cookie` / `session_cookie` 系は既存 `cookie` substring でカバー済みのため
    PASS）。その後 redact.go の allowlist 拡張で全 PASS に到達。観点不備（盲目的に
    green で始まるテスト）を避けた。
  - **HTTP header 風 hyphen キー（`X-State-MAC-Secret` 等）は substring 一致しない
    境界を観察**: `cookie` のような単一 word は `set-cookie` 等のハイフン区切りでも
    一致するが、`state_mac_secret` は snake_case の compound key のため
    `x-state-mac-secret`（lowercased）にマッチしない。auth 領域の構造化ログ field 名は
    snake_case で field 化される前提（A2 既存 logger / zap field の慣習）のため、
    test も snake_case の prefix / 中央埋込で独立 allowlist の発火を確認する形に整えた。
- **残存課題**:
  - 後続 task 2.1 / 5.1 / 5.2 / 6.1: 本 task の field 名 redaction は二次防御で、
    各失敗パスの error wrap 文言に機密値（`cfg.StateMACSecret` /
    `cfg.OIDCTenantClientSecret` / `cfg.OIDCAdminClientSecret` / state cookie 生値 /
    session cookie 生値 / id_token raw JWT）を文字列補間しない一次防御は実装側の
    責務として残る（tasks.md L201〜L210 の design.md「機密値を Cause メッセージ本文に
    埋め込まない実装契約」と整合）。本 redaction は zap field key の substring 一致でのみ
    値を置換する設計のため、`fmt.Errorf("... client_secret=%s ...", ...)` のような
    メッセージ補間経路は redaction を bypass する点に注意。
  - 後続 task 4.1 / 5.1: auth.Repository / auth.Service が `failure_kind` ログを出す
    時、`session_hash_prefix` / `state_mac_prefix` 等の hash prefix を field 化する一次
    防御を守りつつ、誤って `session_cookie` field 名で生値を渡してしまった場合の二次
    防御として本 allowlist が効く（NFR 1.1 / NFR 4.2 / Req 1.11 / Req 3.6）。

### Task 2

- **採用方針**: タスク `2` は umbrella header（`_Requirements:_` / `_Boundary:_` を持たない親 task）
  であり、本起動では直接の実装は無く、子 task 2.1（OIDC Verifier 本体 + 単体テスト）が後続
  iteration で実装される前提として `### Task 2` learning スロットのみ整備する。
- **重要な判断**:
  - 親 task は `tasks.md` 上で `### Task 2` の learning スロットを成立させるためのプレースホルダ
    に留め、`backend/internal/platform/oidc/` 配下のコード追加・テスト追加は本 iteration では
    行わない（実装本体は 2.1 の fresh iteration が担当する設計 / `### Task 1` と同パターン）。
  - per-task ループ規約「1 commit = 1 task ID」に従い、本 iteration の marker commit は
    `docs(tasks): mark 2 as done` 単一の subject で `tasks.md` のみを含める。impl-notes.md への
    `### Task 2` 追加は marker commit と分離した別 commit に積む。
- **残存課題**: 子 task 2.1（`backend/internal/platform/oidc/verifier.go` + `verifier_test.go` /
  `doc.go` の新規追加、`coreos/go-oidc/v3` を用いた JWKS キャッシュ + ID トークン検証実装、
  tenant / admin の aud 排他一致 + nonce 取り出し + `oauth2.AuthStyleInHeader` 固定 +
  機密値非埋込契約の遵守）は後続 fresh iteration で消化する。子 task 2.1 完了時の親 task `2` の
  昇格は本 iteration で完了済みのため、auto-promotion 規約は no-op として扱う。

### Task 2.1

- **採用方針**: `backend/internal/platform/oidc/{doc.go, verifier.go, verifier_test.go}` を
  新規追加。`coreos/go-oidc/v3` で tenant / admin 2 issuer 分の Provider + IDTokenVerifier
  を構築し、go-oidc 内蔵 aud 検証は `&oidc.Config{ClientID: "", SkipClientIDCheck: true}` で
  明示的に切ったうえで、`finalize()` 内で aud と tenant / admin の client_id を排他一致
  検証する 2 層構造に整理した。`failure_kind` は `failureKind string` 型の sentinel error
  として Cause チェーンに含め、`errors.As` / `errors.Is` で Service / Logger 側から識別
  できる経路を確立した（NFR 4.1）。
- **重要な判断**:
  - **同一 issuer URL を tenant / admin で共有する構成への対応**: Keycloak の典型運用
    （1 realm 内に tenant-console / admin-console の 2 client を登録）では tenant / admin の
    issuer URL が同一になる。当初 tenant verifier と admin verifier の両方が成功した場合を
    `aud_ambiguous` 扱いにする実装にしていたが、これだと同一 issuer 構成で正常 token も
    必ず ambiguous になってしまうため、戦略を「両 verifier を順に試し、最初に成功した方の
    IDToken を `finalize` に渡す」に変更し、aud の排他判定は `finalize` 側 client_id 一致で
    一元化した。これにより別 issuer 構成と同一 issuer 構成の両方が同じコードパスで動作する。
  - **kid 不在の failure_kind 判定の限界**: `coreos/go-oidc` の RemoteKeySet は kid 不一致と
    署名検証失敗の双方で `"failed to verify id token signature"` を返し、エラー文字列で両者を
    区別できない（`go.sum` 上の go-oidc v3.10.0 / `jwks.go:179` で確認）。`isKidNotFound`
    helper は文字列マッチで best-effort に分類するが、現状はほぼ全ケースで `invalid_sig` に
    倒れる挙動になる。Req 1.10 は「拒否する」までを保証し、failure_kind の細分化は best-effort
    扱いとした。テスト (h) では `invalid_kid` / `invalid_sig` の **いずれか**を許容する
    permissive な assert で「reject されること」を最優先で守っている。
  - **AuthStyleInHeader の明示上書き**: `Provider.Endpoint()` の戻り値は `AuthStyle=0`
    （`AuthStyleAutoDetect`）で、`golang.org/x/oauth2` v0.21.0 は初回 token endpoint reject を
    受けて `client_secret_post` にフォールバックする非決定的挙動を取る。`buildConsoleVerifier`
    内で `endpoint.AuthStyle = oauth2.AuthStyleInHeader` を明示してから返却し、後続 task 5.1
    の `oauth2.Config` 構築で Basic 固定を契約として強制する。単体テスト
    `TestVerifier_EndpointAuthStyle_IsInHeader` で tenant / admin 両 endpoint の
    `AuthStyle == oauth2.AuthStyleInHeader` を回帰的に守る。
  - **`SkipClientIDCheck: true` の明示が必須**: go-oidc v3 は `ClientID == "" &&
    !SkipClientIDCheck` の組合せを `"invalid configuration, clientID must be provided or
    SkipClientIDCheck must be set"` で reject する仕様（`oidc/verify.go:280`）。`ClientID` を
    空にするだけでは設定不正になるため `SkipClientIDCheck: true` を必ず併設する。
  - **nonce reject は Service 層の責務**: tasks.md (j) の指示通り、Verifier は `nonce`
    クレームを `Claims.Nonce` に surface するだけで一致確認は行わない（不在は空文字を返す）。
    OIDC `nonce` と `StatePayload.OIDCNonce` の constant-time 比較は後続 task 5.1
    `HandleCallback` の責務として境界分離した（観点ごとに 1 ヶ所で検証する原則）。
  - **機密値非埋込契約**: `*errors.Error.Message` および Cause メッセージ本文に raw JWT /
    client_secret / state MAC 鍵を **文字列補間しない**。go-oidc の error は `joinFailureKind`
    で `failure_kind` sentinel と join して wrap するに留め、追加 context は console / issuer
    URL（既に公開情報）のみ。`TestVerifyIDToken_NoSensitiveValueInErrorMessage` で 4 失敗
    種別について `err.Error()` に raw JWT が含まれないことを assert し、回帰耐性を確保した。
  - **モック IdP 設計**: `httptest.NewServer` の URL に `/realms/test` を suffix して issuer
    にし、discovery / JWKS handler を **issuer 配下のパス**（`testIssuerPath + "/.well-known/..."`
    等）に register する必要がある（go-oidc は `<issuer>/.well-known/openid-configuration` に
    GET するため）。rotation テストでは `idpServer.rotateKey()` で JWKS を差し替え、`jwksHits`
    atomic counter で再 fetch の発生を確認する設計。
  - **依存方向**: `internal/platform/oidc` は `internal/errors` / `internal/config` のみを
    internal package として import し、上位 domain（auth / db / httpserver）には依存しない
    （`doc.go` に依存方向ルールを godoc 化）。`internal/depspin/depspin.go` の `coreos-go-oidc`
    blank import は本 task では削除せず、後続 task 7.1 で扱う（spec の指示通り）。
- **残存課題**:
  - 後続 task 5.1 `auth.Service.HandleCallback` で `verifier.VerifyIDToken` の戻り値
    `Claims.Nonce` と `StatePayload.OIDCNonce` を `subtle.ConstantTimeCompare` で照合する
    nonce binding（Req 2.9 / OIDC Core 1.0 §3.1.2.7）を実装する。Verifier 側で nonce 一致
    判定を行わない境界はテスト (j) で固定済み。
  - 後続 task 6.3 `cmd/api/main.go` bootstrap で `oidc.NewVerifier(ctx, cfg)` を呼び出し、
    失敗時に `os.Exit(1)` で fail-closed bootstrap を成立させる（NFR 3.2）。本 task で
    `*errors.Error{Code: CodeUnavailable, failure_kind: oidc_discovery}` を返す経路は
    `TestNewVerifier_DiscoveryFailure_ReturnsUnavailable` で回帰的に守る。
  - 後続 task 7.1 で `depspin.go` から `coreos-go-oidc` の blank import を削除する
    （本 task で direct dependency 化したため、blank import の役目が果たされた）。
  - `invalid_kid` failure_kind の細分化は best-effort 実装に留まる。実 IdP / 実 Keycloak で
    kid rotation エラー文言が固定化される場合、後続 Issue で `RemoteKeySet` ラッパを実装して
    failure_kind を厳密化する余地がある（本 Issue 範囲外）。

### Task 2.1 — Verify 実行結果

- `go build ./...`: PASS
- `go vet ./...`: PASS
- `go test ./...`: 全 package PASS（`./internal/platform/oidc` 含む。実行時間
  約 3.5s、`TestVerifyIDToken_*` を含む 13 トップレベルテスト + nonce サブテスト 2 件が
  全 PASS）
- `go mod tidy`: `coreos/go-oidc/v3` と `golang.org/x/oauth2` を direct dependency に昇格
  （go-jose/go-jose/v4 も test の直接 import により direct 化）
- DB-backed verify: 本 task は OIDC Verifier のユニットテストのみで完結し、DB を要求しない
  ため対象外。tasks.md L774〜L802 の DB-backed verify 義務は後続 task 4.1 / 6.4 で
  integration test として実施される予定。

### Task 3

- **採用方針**: タスク `3` は umbrella header（`_Requirements:_` / `_Boundary:_` を持たない親 task）
  であり、本起動では直接の実装は無く、子 task 3.1（auth.types + state cookie helper + 単体テスト）/
  3.2（auth.session helper + 単体テスト）が後続 fresh iteration で順次実装される前提として
  `## Implementation Notes` 構造のみ整備する（先行する `### Task 1` / `### Task 2` の umbrella
  処理パターンを踏襲）。
- **重要な判断**:
  - 親 task は `tasks.md` 上で `### Task 3` の learning スロットを成立させるためのプレースホルダ
    に留め、`backend/internal/auth/` 配下のコード追加（types.go / clock.go / state.go / session.go /
    doc.go）・テスト追加（state_test.go / session_test.go）は本 iteration では行わない（実装本体は
    3.1 / 3.2 の fresh iteration が担当する設計 / `### Task 1` / `### Task 2` と同パターン）。
  - per-task ループ規約「1 commit = 1 task ID」に従い、本 iteration の marker commit は
    `docs(tasks): mark 3 as done` 単一の subject で `tasks.md` のみを含める。impl-notes.md への
    `### Task 3` 追加は marker commit と分離した別 commit に積む。
- **残存課題**: 子 task 3.1（`backend/internal/auth/{types.go, clock.go, state.go, doc.go}` +
  `state_test.go` の新規追加、`StatePayload` の `Nonce` / `OIDCNonce` 分離 + HMAC-SHA256 MAC +
  `__Host-ae_mdm_state` cookie + `ExpireCookieAttributes()` 削除 helper の実装）/ 3.2
  （`backend/internal/auth/session.go` + `session_test.go` の新規追加、`crypto/rand` 32 byte
  base64url session token + SHA-256 hex hash + `__Host-ae_mdm_session` cookie + `MaxAge < 0`
  削除 helper + `HashPrefix` 8 文字 helper の実装）は後続 fresh iteration で消化する。子 task
  全完了時の親 task `3` の昇格は本 iteration で完了済みのため、auto-promotion 規約は no-op
  として扱う。

### Task 3.1

- **採用方針**: `backend/internal/auth/{types.go, clock.go, state.go, doc.go, state_test.go}`
  の 5 ファイルを新規追加。`Identity` / `Session` のドメイン型は design.md L460〜L490 の構造
  そのままに `Console` のみ `internal/platform/oidc.Console` を再利用する形に整理し、`Clock`
  interface + `SystemClock` で時刻 DI 境界を確立。state cookie の Sign / Verify は HMAC-SHA256
  + base64url no-padding（`base64.RawURLEncoding`） + `subtle.ConstantTimeCompare` の 3 点で
  CSRF / replay / 偽造を防御し、失敗種別を `state_invalid` / `state_expired` /
  `state_mismatch` の 3 sentinel に分類した（Req 2.1〜2.9 / NFR 4.1）。
- **重要な判断**:
  - **`failureKind` sentinel を auth package に独立再定義した理由**: `internal/platform/oidc`
    にも同名 type が存在するが、当該型は **unexported**（package private）であり外部から
    `errors.Is` で参照できない。本 task 3.1 で auth domain の `state_invalid` / `state_expired` /
    `state_mismatch` を分類するには、auth package で同一パターン（`type failureKind string` +
    `Error() string` メソッド + 公開定数）を再定義する必要があった。oidc 側の `FailureKind*`
    定数を import すると auth → oidc → 何らかの逆方向依存を作る誘惑が生じるため、両 package
    で同型を独立に持つ方が依存方向ルール（doc.go 記載）を機械的に守れる。tasks.md L295 の
    「auth package 用に独立に再定義する」指示と整合。
  - **`state.ExpireCookieAttributes` の `MaxAge = -1` 選択理由**: Go の `http.Cookie` の
    Set-Cookie 出力規約では `MaxAge < 0` のみが「削除 cookie」として扱われ、ヘッダに
    `Max-Age=0` 属性 + 過去日付の `Expires` 属性を出力する。`MaxAge = 0` は Max-Age 属性
    自体が省略されて session cookie 化（ブラウザを閉じるまで保持）するため、callback 完了時
    の即時無効化（Req 2.8）が成立しない。design.md L542〜L548 / tasks.md L283〜L290 の指示
    通り具体値 `-1` を採用し、テスト (h) で `MaxAge < 0` および `MaxAge == -1` の双方を
    assert することで「`= 0` への退行」を回帰耐性で守る形にした。
  - **Verify の検証順序**: 空文字 → フォーマット split → base64 decode → MAC 検証 → payload
    unmarshal → TTL → queryState の順に並べ、**payload unmarshal を MAC 検証通過後**に行う
    ことで「攻撃者が任意の JSON を unmarshal させる経路」を物理的に断った。TTL 境界は **`>`**
    の strict 不等式とし、`now == IssuedAt + ttl` は受理する設計（テスト (c2) で boundary
    挙動を回帰耐性で固定）。
  - **機密値の非埋込契約**: error wrap の `Message` 引数には固定文字列（`"state cookie missing"`
    等）のみを渡し、secret / cookie 生値 / queryState を `fmt.Sprintf` 経由で文字列補間しない。
    `TestStateCookie_VerifyError_DoesNotEmbedSensitiveValues` と `_NoSensitiveLeak_AllFailureKinds`
    で 4 失敗種別について `err.Error()` に当該値が含まれないことを assert し、NFR 1.1 / NFR 4.2
    を回帰的に守る。
  - **`payload.Nonce` の重複検証経路**: query state vs cookie 内 Nonce の不一致判定は
    `subtle.ConstantTimeCompare` を使い、長さ差・内容差ともに副チャネル耐性を確保した。
    平易な `!=` 比較だと早期 return のタイミング差から長さ漏洩が起こり得るため `crypto/subtle`
    を採用した（NFR 4.x 系の標準的な安全実装）。
- **残存課題**:
  - 後続 task 3.2: `backend/internal/auth/session.go` で `New()` / `HashToken` /
    `CookieAttributes(ttl)` / `ExpireCookieAttributes()` / `HashPrefix` を実装する。本 task で
    確立した `failureKind` パターンと doc.go の依存方向ルールを継承し、`__Host-ae_mdm_session`
    cookie 名 + `MaxAge = -1` 削除 helper を session 側でも同パターンで実装する。
  - 後続 task 4.1: `Repository.Create` で `Session` struct を pgxpool で INSERT する経路、
    `Repository.Get` で `Session` + `Identity` を join 取得する経路で本 task の型を利用する。
    `Identity.TenantID == uuid.Nil` 表現も Repository 側で SuperAdmin context 解決時に
    そのまま使う。
  - 後続 task 5.1: `auth.Service.BeginLogin` / `HandleCallback` で本 task の `Sign` /
    `Verify` / `CookieAttributes` / `ExpireCookieAttributes` および `StatePayload` の
    `Nonce` / `OIDCNonce` 分離を活用する。`oauth2.AuthCodeURL(payload.Nonce, SetAuthURLParam("nonce",
    payload.OIDCNonce))` の呼び出しで OAuth `state` と OIDC `nonce` を別パラメータとして
    IdP に渡す前提（OIDC Core 1.0 §3.1.2.1）に本 task の型設計が直接対応している。

### Task 3.2

- **採用方針**: `backend/internal/auth/{session.go, session_test.go}` の 2 ファイル新規追加 +
  `doc.go` 構成リスト 1 行更新。`New()` は `crypto/rand.Read` で 32 byte を読み出し
  `base64.RawURLEncoding`（no-padding）で 43 文字の文字列に整形、`HashToken` は SHA-256 hex
  64 文字を返す。cookie 属性 helper は state.go の同名関数（`CookieAttributes` /
  `ExpireCookieAttributes`）と Go の同 package 内 redeclaration エラーを起こすため
  **`SessionCookieAttributes` / `SessionExpireCookieAttributes`** に rename して衝突回避した
  （契約・属性値は tasks.md / design.md の指示通り維持）。
- **重要な判断**:
  - **命名衝突回避の選択**: design.md / tasks.md の散文は `state.ExpireCookieAttributes()` /
    `session.ExpireCookieAttributes()` のように sub-package 風名前空間を想定するが、task 3.1
    で auth を **フラット package** として確定済み（state.go / types.go / clock.go / doc.go が
    `package auth` 直下）。retroactive な refactor 禁止規約に従い、session 側を Session
    プレフィックスで disambiguate する選択を採用した。`New` / `HashToken` / `HashPrefix` は
    state.go と衝突しないためそのままの名前を維持する。後続 task 5.1 / 5.2 / 6.1 の Service /
    Handler / Middleware は、state cookie 削除には `auth.ExpireCookieAttributes()`、session
    cookie 削除には `auth.SessionExpireCookieAttributes()` を呼び分ける必要がある（**確認事項**
    に明示）。
  - **`MaxAge = -1` の選択理由**: Go の `http.Cookie` 規約では `MaxAge < 0` のみが削除 cookie
    として `Set-Cookie` ヘッダに `Max-Age=0` 属性 + 過去日付 `Expires` 属性を出力する。
    `MaxAge = 0` だと Max-Age 属性が省略されて session cookie 化（ブラウザを閉じるまで保持）
    して即時削除が成立しない。state.go と同じ規約を踏襲し、テスト (f) で `MaxAge < 0` と
    `MaxAge == -1` の両方を assert して回帰耐性を確保した。
  - **`base64.RawURLEncoding` 採用理由**: Cookie value の文字種制約（[A-Za-z0-9-_]+）と
    URL-safe（`+` / `/` の禁止）+ no-padding（`=` の禁止）を両立できる encoding は
    base64url の no-padding 表記のみ。state.go の MAC encoding でも同 encoding を使っており、
    auth package 全体で base64url no-padding に統一する。
  - **`crypto/rand.Read` err の fail-closed**: design.md NFR 3.1 / Req 3.5 の指示通り、
    `rand.Read` 失敗時は `*errors.Error{Code: CodeInternal}` を返し fixed message のみで
    wrap する（partial buffer 内容を補間しない / NFR 1.1 / NFR 4.2）。呼び出し側（後続 task
    5.1 `auth.Service`）は 5xx に倒すことで session 発行不能を明示する責務を持つ。
  - **`HashPrefix` の defensive 境界**: 8 文字未満の入力（空文字 / 短い hash）が誤って渡された
    場合は丸ごと返す設計とした（slice out-of-range panic を起こさない）。本来 `HashToken` の
    戻り値（64 文字）が前提だが、テスト用 fixture や bug 経路で短い値が渡る可能性を考慮した
    defensive 実装。テスト (g2) / (g3) で空文字 / 7 文字入力の挙動を回帰耐性で固定。
  - **Red→Green の踏み方**: session.go 追加前に session_test.go を先に書いて compile error
    （`undefined: New` 等）で fail を観測してから session.go 実装で全 PASS に到達する手順を
    取った（一発で書き上げて green で始まるテストは観点不備を疑う原則 / CLAUDE.md「テスト
    規約 / 共通」節）。
- **残存課題**:
  - 後続 task 4.1 `Repository.Create` / `Repository.Get` / `Repository.Touch` /
    `Repository.Revoke` で本 task の `HashToken(rawToken)` を呼んで `Session.TokenHash`
    field（task 3.1 で定義済み）に格納する経路で利用される。生 token は永続ストアに
    記録しない（Req 3.6 / 3.7 / NFR 1.2）。
  - 後続 task 5.1 `auth.Service.HandleCallback` で `auth.New()` → cookie value 設定 →
    `auth.SessionCookieAttributes(cfg.SessionAbsoluteTimeout)` → `Repository.Create` の経路
    を組み立てる。`SessionCookieAttributes` の `ttl` 引数には `cfg.SessionAbsoluteTimeout`
    （task 1.1 で `Config` に追加済み）を渡す契約。
  - 後続 task 5.2 / 6.1 Handler / Middleware の logout / 失効検出パスで
    `auth.SessionExpireCookieAttributes()` を呼ぶ。**state cookie 削除には `auth.
    ExpireCookieAttributes()`、session cookie 削除には `auth.SessionExpireCookieAttributes()`
    を使う**点を本 task で確立した命名規約として後続 task で守る必要がある（state cookie /
    session cookie の cleanup 経路で取り違えないこと）。
  - 後続 task 4.1 / 5.1 の structured log では `HashPrefix(session.TokenHash)` の戻り値を
    `session_hash_prefix` field key で出力する（Req 3.8）。生 token / 全 hash は出力しない
    （task 1.4 で `state_cookie` / `session_cookie` の redaction allowlist 追加済みだが、
    一次防御として実装側で fixed field key + prefix のみを出力する責務が残る）。

### Task 4

- **採用方針**: タスク `4` は umbrella header（`_Requirements:_` / `_Boundary:_` を持たない親 task）
  であり、本起動では直接の実装は無く、子 task 4.1（Repository 実装 + integration テスト）が
  後続 fresh iteration で実装される前提として `### Task 4` learning スロットのみ整備する
  （先行する `### Task 1` / `### Task 2` / `### Task 3` の umbrella 処理パターンを踏襲）。
- **重要な判断**:
  - 親 task は `tasks.md` 上で `### Task 4` の learning スロットを成立させるためのプレースホルダ
    に留め、`backend/internal/auth/repository.go` の新規追加・`pgxpool.Pool` ベース CRUD
    （`Create` / `Get` / `Touch` / `Revoke` / `ConsumeStateNonce` / `ResolveAdminUser`）の実装・
    SuperAdmin context での `db.BeginTxFunc` 経路の確立・integration テスト追加は本 iteration
    では行わない（実装本体は 4.1 の fresh iteration が担当する設計 / `### Task 1` / `### Task 2` /
    `### Task 3` と同パターン）。
  - per-task ループ規約「1 commit = 1 task ID」に従い、本 iteration の marker commit は
    `docs(tasks): mark 4 as done` 単一の subject で `tasks.md` のみを含める。impl-notes.md への
    `### Task 4` 追加は marker commit と分離した別 commit に積む。
- **残存課題**: 子 task 4.1（`backend/internal/auth/repository.go` の新規追加、`Repository`
  interface + `pgxpool.Pool` ベース実装、`ConsumeStateNonce` / `ResolveAdminUser` を含む
  すべての CRUD を `db.BeginTxFunc` 経由で SuperAdmin context（`db.WithTenantContext(ctx,
  db.TenantContext{IsSuperAdmin: true})` 前置）下で実行する経路、`Get` 0 行時の
  `*errors.Error{Code: CodeUnauthenticated, failure_kind: session_tamper}` 返却、`Touch`/
  `Revoke` の冪等 UPDATE、`ConsumeStateNonce` の unique_violation (`23505`) 分類、
  `ResolveAdminUser` の `(oidc_issuer, oidc_subject)` 複合 UNIQUE lookup、integration test
  整備）は後続 fresh iteration で消化する。子 task 全完了時の親 task `4` の昇格は本
  iteration で完了済みのため、auto-promotion 規約は no-op として扱う。

### Task 4.1

- **採用方針**: `backend/internal/auth/{repository.go, repository_failure_kinds.go}` の
  2 ファイル + `backend/test/integration/auth_repository_test.go` を新規追加し、`doc.go` 構成
  リストを task 4.1 時点に更新。Repository interface（`ConsumeStateNonce` / `ResolveAdminUser` /
  `Create` / `Get` / `Touch` / `Revoke`）の **全メソッド** で `superAdminContext(ctx)` →
  `db.BeginTxFunc` の 2 段 wrap を共通化し、callback handler / middleware が
  `TenantContextMiddleware` の外側で動作するという A2 制約を Repository 内に閉じ込めた。
  failure_kind sentinel（`state_replay` / `admin_user_not_provisioned` / `session_tamper`）は
  state.go の `failureKind` 型を再利用しつつ、責務分離のため別ファイル化した。
- **重要な判断**:
  - **`pgerrcode` を direct dependency に昇格**: A2 では indirect 依存だった
    `github.com/jackc/pgerrcode` を `repository.go` で直接 import し、`pgerrcode.UniqueViolation`
    （`"23505"`）と `*pgconn.PgError.Code` の照合で UNIQUE 違反を分類する経路を確立した
    （Req 2.9 の state_replay マッピング）。`go mod tidy` で indirect → direct への昇格を
    確定。SQLState 文字列リテラルを直接埋め込む案も検討したが、pgerrcode は constants
    package で typo を回避でき、A2 design.md 確認事項とも整合するため採用した。
  - **failure_kind sentinel を別ファイル化した理由**: state.go は state cookie helper
    （Sign / Verify / CookieAttributes / ExpireCookieAttributes）の責務に閉じる必要があり、
    Repository 固有の sentinel（state_replay / admin_user_not_provisioned / session_tamper）を
    state.go に追記すると state cookie helper の責務境界がぼやける。`failureKind` 型本体
    （`type failureKind string` + `Error() string`）は state.go から再利用するため
    `package auth` 内に閉じ、定数のみを `repository_failure_kinds.go` に配置する分割を採用した
    （doc.go の構成リストも task 4.1 時点に更新）。
  - **`ResolveAdminUser` の `FOR UPDATE` 行ロック採用**: read-modify-write の競合経路
    （同一 OIDC subject が並行 callback で 2 回 ResolveAdminUser を呼ぶケース）で email
    UPDATE のロストアップデートを防ぐため、`SELECT ... FOR UPDATE` で行ロックを取得してから
    email UPDATE する設計を採用した（design.md L646〜L664 の Repository interface 散文と
    tasks.md L365 の指示の両方を満たす）。
  - **`Identity.TenantID == uuid.Nil` の表現**: `admin_users.tenant_id` は nullable
    （SuperAdmin の場合 NULL / A2 既存スキーマ）のため、Scan 先は `*uuid.UUID` で受け、nil なら
    `Identity.TenantID` を `uuid.Nil` のままにする実装にした（types.go の godoc「SuperAdmin の
    場合は uuid.Nil」と整合）。これにより Service 層は `identity.TenantID == uuid.Nil` で
    SuperAdmin を判定できる（A2 の TenantContext 規約と一貫）。
  - **`admin_role_assignments` の role 集約クエリは `role::text` 経由**: `admin_role` 型は
    `CREATE TYPE admin_role AS ENUM (...)`（0002_create_admin_users_and_roles.up.sql L16）で
    定義された PostgreSQL ENUM 型。pgx の `Scan(&string)` で ENUM を string に取り出すには
    明示的に `::text` キャストする必要がある（直接 binary decoding は CodecDB の type 登録が
    必要で、本 task ではキャスト経路の方が簡潔で安全）。
  - **`Get` 0 行と `ResolveAdminUser` 0 行で Code が異なる**: 前者は `CodeUnauthenticated`
    （cookie 改竄 = 認証経路の失敗 / Req 5.4 → 401）、後者は `CodeForbidden`
    （事前 provisioning が無い管理者の login = 認可経路の失敗 / 403）。design.md
    L646〜L664 と整合。Service 層は両者の Code をそのまま HTTP status にマッピングする。
  - **integration test の SuperAdmin GUC 設定**: 既存 `helpers_test.go.seedDummyData` は
    `app.is_superadmin=true` のみを set するが、本 task で追加した read 系 helper
    （`fetchSession` / `fetchAdminUserEmail`）は事前に Repository が `db.BeginTxFunc` を
    使った tx に続いて呼ばれるパターンで、connection 上で `app.tenant_id` が空文字のまま
    cast `''::uuid` で reject される事故が再現した。本 task の helper では
    `set_config('app.tenant_id', uuid.Nil.String(), true)` + `set_config('app.is_superadmin',
    'true', true)` の 2 段 set を採用した（RLS policy の OR 右辺 `is_superadmin` だけでは
    短絡評価が保証されないため、`app.tenant_id` の有効値 set が必須）。既存 `seedDummyData`
    の現状は INSERT のみで動作するため変更しない（影響範囲外）。
  - **DB-backed verify を実施**: 本 task は migration 0013〜0015 / RLS / SuperAdmin context /
    UNIQUE 違反マッピングの全経路を DB 接続経由でしか検証できないため、本 iteration 内で
    docker compose 経由の Postgres 16 + migrate up + integration test 全件 pass を確認した
    （詳細は `### Task 4.1 — Verify 実行結果` 節）。
- **残存課題**:
  - 後続 task 5.1 `auth.Service.HandleCallback` で本 task の `Repository` 各メソッドを
    DI 注入する。`ConsumeStateNonce` は state.Verify 直後 + token 交換より **前** に呼ぶ
    （tasks.md L430〜L436 / replay 攻撃が IdP token endpoint に到達する前に弾く / attack
    surface 最小化）。`ResolveAdminUser` の 403 は Service が `errors.As` で
    `FailureKindAdminUserNotProvisioned` を識別して 403 を return する経路を組む。
  - 後続 task 6.1 `auth.Middleware` で `Repository.Get` の 0 行 → `FailureKindSessionTamper`
    → 401 + cookie 削除の経路を組む。本 task の Get は `Session` + `Identity` を **同一 tx 内で**
    join 取得しており、Service / Middleware 側の追加 lookup は不要（NFR 3.1 の fail-closed
    と整合）。
  - 後続 task 6.4 の integration test（auth_login_callback_test.go 等）では本 task で確立した
    Repository 経路に対し HTTP 経路を被せた e2e 検証を行う前提。本 task の repository test は
    Repository 単体（Service / Handler を介さない）の境界網羅に閉じる。
  - 後続 task 7.1 で `impl-notes.md` の「DB-backed verify 実行結果」節を最終確定する
    （本 task の `### Task 4.1 — Verify 実行結果` 節は本 task scope の暫定記録）。
  - `admin_role_assignments` の RLS は `tenant_isolation_admin_role_assignments` で
    tenant 分離されており、SuperAdmin context では全行可視（A2 0011_enable_rls.up.sql L48〜L58）。
    本 task の `ResolveAdminUser` / `Get` は SuperAdmin context 配下なので全行を取得でき、
    RBAC の解釈は後続 Issue で実装する責務（本 task は集約のみ）。

### Task 4.1 — Verify 実行結果

- `cd backend && go build ./...`: PASS
- `cd backend && go vet ./...`: PASS
- `cd backend && go test ./... -count=1`: 全 package PASS（`internal/auth` 含む。
  `test/integration` は DATABASE_URL 未設定経路で skip 動作を確認）
- DB-backed verify（docker compose postgres + migrate up + integration test）:
  - 環境: `docker compose up -d postgres`（POSTGRES_HOST_PORT=15433 / POSTGRES_PASSWORD=test_repo_4_1
    / Postgres 16-alpine）+ `db-init-roles` 適用（migration_user / app_user 作成）+
    migrate up 0001〜0015（全 15 migration 適用 / state_nonces まで含む）
  - コマンド: `INTEGRATION_TEST_DATABASE_URL="postgres://app_user:app_pass@localhost:15433/ae_mdm?sslmode=disable"
    INTEGRATION_TEST_MIGRATE_URL="postgres://migration_user:migration_pass@localhost:15433/ae_mdm?sslmode=disable"
    go test ./test/integration/... -count=1`
  - 実行結果: **PASS**（`auth_repository_test.go` 8 テスト関数すべて pass / 既存 integration
    test も全 pass / 合計 約 2.8s）。シナリオ (a)〜(k) の 11 シナリオを 8 テスト関数で網羅
    （ResolveAdminUser 3 関数 / Create_Get_Touch_Revoke 1 関数で d/f/g/h を統合 /
    Get_HashMismatch_SessionTamper 1 関数 / ConsumeStateNonce 3 関数）
  - 実行日時: 2026-06-26

### Task 5

- **採用方針**: タスク `5` は umbrella header（`_Requirements:_` / `_Boundary:_` を持たない親 task）
  であり、本起動では直接の実装は無く、子 task 5.1（auth.Service 4 ユースケース + 単体テスト）/
  5.2（auth.Handler HTTP 6 endpoints + httptest 単体テスト）が後続 fresh iteration で順次実装
  される前提として `## Implementation Notes` 構造のみ整備する（先行する `### Task 1` /
  `### Task 2` / `### Task 3` / `### Task 4` の umbrella 処理パターンを踏襲）。
- **重要な判断**:
  - 親 task は `tasks.md` 上で `### Task 5` の learning スロットを成立させるためのプレースホルダ
    に留め、`backend/internal/auth/{service.go, handler.go}` 等のコード追加・テスト追加は本
    iteration では行わない（実装本体は 5.1 / 5.2 の fresh iteration が担当する設計 /
    `### Task 1` / `### Task 2` / `### Task 3` / `### Task 4` と同パターン）。
  - per-task ループ規約「1 commit = 1 task ID」に従い、本 iteration の marker commit は
    `docs(tasks): mark 5 as done` 単一の subject で `tasks.md` のみを含める。impl-notes.md への
    `### Task 5` 追加は marker commit と分離した別 commit に積む。
- **残存課題**: 子 task 5.1（`backend/internal/auth/service.go` の新規追加、`Service` interface
  `BeginLogin` / `HandleCallback` / `LookupAndRefresh` / `Logout` の 4 ユースケース実装、
  `returnTo` 正規化 + `Nonce` / `OIDCNonce` 独立 16 byte 生成 + state cookie 発行 +
  `ConsumeStateNonce` を token 交換**前**に実行する replay 防御 + OIDC nonce 一致確認 +
  `ResolveAdminUser` 403 マッピング + idle / absolute timeout 判定 + Logout 経路）/ 5.2
  （`backend/internal/auth/handler.go` の新規追加、HTTP 6 endpoints `GET /api/auth/login` /
  `GET /api/auth/callback` / `GET /api/auth/me` / `POST /api/auth/logout` + tenant / admin
  console の 2 系統対応 + httptest による单体テスト + Service interface のモック注入）は
  後続 fresh iteration で消化する。子 task 全完了時の親 task `5` の昇格は本 iteration で
  完了済みのため、auto-promotion 規約は no-op として扱う。

### Task 5.1

- **採用方針**: `backend/internal/auth/{service.go, service_failure_kinds.go, service_test.go}`
  の 3 ファイルを新規追加し、`doc.go` の構成リストを task 5.1 時点に更新。`Service` interface
  は `BeginLogin` / `HandleCallback` / `LookupAndRefresh` / `Logout` の 4 ユースケースを提供し、
  Verifier / Repository / Clock / TokenGenerator / oauth2.Config を DI で受け取る形に整理した。
  fake oauth2 token endpoint は `httptest.NewServer` で構築し、id_token 返却 / 欠落 / 5xx の
  3 パターンを切り替えられる helper を service_test.go に集約。
- **重要な判断**:
  - **Nonce / OIDCNonce の独立生成**: tasks.md L408〜L409 / design.md L518〜L525 の指示通り、
    `crypto/rand.Read` で 16 byte ずつ **別々に**生成する `generateNonce()` を 2 回呼ぶ
    実装にした。同値を使い回す案 (1 nonce を state と OIDC nonce で共用) を採用すると
    state と OIDC nonce の意味分離が崩れる（OAuth `state` は CSRF 防止、OIDC `nonce` は
    authorization code injection 防止で別目的 / RFC OIDC Core 1.0 §3.1.2.1）。テスト
    `TestBeginLogin_ValidReturnTo_ReturnsRedirectAndCookie` で `state != nonce` を assertion
    して退行を防ぐ。
  - **`ConsumeStateNonce` を token 交換より前に呼ぶ**: tasks.md L430〜L436 / design.md L682〜L685
    の指示通り、state.Verify 直後 + payload.Console 照合直後 + token 交換**前**に
    `repo.ConsumeStateNonce` を呼ぶ実装にした。これにより replay 攻撃が IdP token endpoint /
    DB session INSERT に到達する前に弾ける（attack surface 最小化 / Req 2.9）。テスト
    `TestHandleCallback_StateReplay_Returns401AndDoesNotExchange` で fake repo が state_replay
    を返した時に `verifier.VerifyIDToken` / `ResolveAdminUser` / `Create` のいずれも呼ばれない
    ことを assertion している。
  - **`token.Extra("id_token").(string)` の two-value form 必須**: tasks.md L440〜L443 /
    design.md L686〜L689 の指示通り、`rawIDToken, ok := token.Extra("id_token").(string)` で
    取り出し、`!ok || rawIDToken == ""` なら `upstream_oidc_token` (502) を返す。直接
    `token.Extra("id_token").(string)` の **single-value form** だと token endpoint が
    `id_token` を返さない / 文字列でない場合に panic を起こす（NFR 3.1 fail-closed 違反）。
    テスト `TestHandleCallback_NoIDToken_ReturnsUpstreamFailure` で id_token 欠落経路を回帰
    的に守る。
  - **`Claims.Nonce` 一致確認は Service 層、Verifier 側では行わない**: 設計の境界分離
    （task 2.1 の判断と整合）。Service の HandleCallback 内で
    `subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(payload.OIDCNonce)) != 1` で
    照合し、不一致は `nonce_mismatch` (401) で reject。`claims.Nonce == ""`（IdP が nonce を
    要求どおりに返さなかった場合）も `payload.OIDCNonce` が 16 byte ランダムなので
    constant-time 比較は **不一致**を返し、同じ `nonce_mismatch` 経路で fail-closed する。
    テスト `TestHandleCallback_NonceMismatch_Returns401` + `TestHandleCallback_EmptyIDTokenNonce_Returns401`
    で 2 経路を回帰的に守る。
  - **`TokenGenerator` DI で csprng_failure を回帰**: tasks.md L457〜L463 / design.md L717〜L722
    の指示通り、`NewService` の引数に `tokenGen TokenGenerator` を明示し、本番は `session.New`
    を渡す、テストは `func() (string, error) { return "", err }` を渡せる構造にした。
    テスト `TestHandleCallback_TokenGenError_ReturnsCSPRNGFailure` で fake fn が err を
    返した時に `repo.Create` が呼ばれず 500 になることを assertion している（Req 3.5 / NFR 3.1
    fail-closed の回帰保証）。
  - **失効判定順序 console → absolute → revoked → idle**: tasks.md L478〜L483 / design.md
    L795〜L797 の指示通り、`LookupAndRefresh` で 4 段の順に判定する。`absolute → revoked → idle`
    の順は「絶対拘束 → 明示 logout → idle」の優先度で、console_mismatch を最先頭に置くのは
    漏洩した tenant session が `/api/admin` に提示された場合に即座に reject する経路を
    成立させるため（design.md L799）。失効時は `revokeOnExpire` で `repo.Revoke` を冪等に
    呼ぶ（Req 4.6）が、Revoke 自体のエラーは failure_kind 経路を上書きしないようログのみに
    残す設計とした（元の failure_kind の伝播を優先 / NFR 3.1 fail-closed）。
  - **`return_to` 正規化**: tasks.md L404〜L408 / design.md 確認事項 3 の指示通り
    「同一オリジン内相対パスのみ許容」を採用。空文字 → `/`（default）、`//` で始まる
    scheme-relative URL / `http://` / `https://` を含む absolute URL / `dashboard` のような
    `/` で始まらない相対パスはすべて 400 で reject。`url.Parse` で `u.Host != "" || u.Scheme != ""`
    の二段防御を追加し、`//` prefix の見落としや想定外の scheme を弾く。
  - **`failure_kind` ログ field の明示出力**: tasks.md L486〜L494 / design.md L1090〜L1097 の
    指示通り、各失敗パスで `log.Warn("auth failure", "failure_kind", kind, "console", ...,
    "session_hash_prefix", ...)` の形式で **明示的に** field を出す。`logWarnFailure` /
    `logWarnSession` の 2 helper に集約し、auth package の `failureKind` は `errors.As` で
    抽出、oidc package の `failureKind` は unexported type なので `Error()` 戻り値の文字列を
    Cause チェーンから走査して既知リストと突き合わせる best-effort 経路を `extractFailureKind`
    に実装。テスト `TestService_LogFailureKindOnFailure` / `TestBeginLogin_LogsFailureKindOnReturnToInvalid`
    で fake logger に field が乗ることを assertion している（NFR 4.1 の実装契約を回帰）。
  - **機密値非埋込契約**: tasks.md L495〜L502 / design.md L1099〜L1117 の指示通り、`*errors.Error.Message`
    / `Cause` メッセージに `cfg.StateMACSecret` / `cfg.OIDC*ClientSecret` / state cookie 生値 /
    session cookie 生値 / id_token raw JWT を **文字列補間しない**。oauth2 / state.Verify /
    repository の error はそのまま wrap し、追加 context は `failure_kind` / `console` /
    `session_hash_prefix` 等の非機密値のみとする。テスト
    `TestHandleCallback_SensitiveValuesNotEmbeddedInError` /
    `TestBeginLogin_SensitiveValuesNotEmbeddedInError` /
    `TestLookupAndRefresh_SensitiveValuesNotEmbeddedInError` の 3 系統で 5 種の機密値
    （state secret / client secret / raw id_token / raw session token / full session hash）が
    `err.Error()` に含まれないことを assertion し、回帰耐性を確保した（NFR 1.1 / NFR 4.2 の
    一次防御）。
  - **`service_failure_kinds.go` の別ファイル化**: state.go / repository_failure_kinds.go の
    パターンを踏襲し、Service 固有の 10 sentinel（state_console_mismatch / invalid_aud /
    nonce_mismatch / csprng_failure / upstream_oidc_token / console_mismatch / session_expired /
    session_revoked / session_idle / return_to_invalid）を別ファイルに分離した。service.go の
    責務（4 ユースケースの集約）を膨張させずに failure_kind 追加を一箇所に集約できる
    （doc.go の構成リストも task 5.1 時点に更新）。
  - **state / session cookie 削除 helper の使い分け**: 本 task の HandleCallback 戻り値は
    `(rawSessionToken, sessionCookie, returnTo, err)` のみで、state cookie の削除は **Handler
    の責務**（後続 task 5.2 が `state.ExpireCookieAttributes()` を発行する）。task 3.2 の確認
    事項で確立した「state cookie 削除 = `auth.ExpireCookieAttributes()`、session cookie 削除
    = `auth.SessionExpireCookieAttributes()`」の使い分けを Service レイヤでも維持する
    （Service はどちらの cookie 属性 helper も呼び出さず、Handler が経路ごとに使い分ける）。
- **残存課題**:
  - 後続 task 5.2 `auth.Handler` で Service の 4 メソッドを HTTP / cookie I/O に橋渡しする。
    `/api/auth/callback` 入口で `code` / `state` クエリ欠落判定を 400 `invalid_request` で
    行う責務は Handler 側（本 Service はクエリ欠落を仮定しない契約）。
  - 後続 task 6.1 `auth.Middleware` で `LookupAndRefresh` を呼び、`httpserver.AuthClaims` を
    ctx に注入する。本 Service の `LookupAndRefresh` 戻り値 `(Identity, Session, error)` を
    そのまま使い、middleware は ctx 注入と Set-Cookie 発行のみを担う。
  - 後続 task 6.3 `cmd/api/main.go` bootstrap で `auth.NewService(cfg, verifier, repo,
    map[oidc.Console]*oauth2.Config{...}, auth.SystemClock{}, auth.TokenGenerator(session.New),
    log)` を呼んで本番値で構築する。`oauth2.Config.Scopes` には `goidc.ScopeOpenID` /
    `"email"` / `"profile"` を必ず含める（id_token 発行のため / 本 Service テストは
    fake verifier を使うので scope 不在の経路は Service 単体では検出できず、後続 task 6.4
    の integration test で実 IdP mock 経由で検証する）。
  - 確認事項: 本 task では `failureKind` 型を `extractFailureKind` で oidc package のものも
    含めて統一的に文字列化する `best-effort` 経路を採用したが、oidc package の `failureKind`
    が unexported のため文字列マッチに依存している。`Error()` の戻り値が変化した場合
    （go-oidc upgrade で文言が変わる等）に best-effort fallback が黙って失敗する可能性
    がある。将来的に oidc.FailureKind* 定数を auth package から `errors.As` で直接識別
    できるよう **公開化を検討**する余地がある（本 task 範囲外、後続 Issue / refactor で扱う）。

### Task 5.1 — Verify 実行結果

- `cd backend && go build ./...`: PASS
- `cd backend && go vet ./...`: PASS
- `cd backend && go test ./... -count=1`: 全 package PASS（`internal/auth` の Service 関連
  テスト 30 ケース超 + 既存テスト全件 / 約 4 秒）。`internal/platform/oidc` も含めて
  影響範囲全件で regression なし。
- DB-backed verify: 本 task は Service 単体（fake Verifier / Repository / Clock / Logger /
  oauth2 mock）で完結し、DB を要求しない。HTTP 経路を被せた e2e 検証は後続 task 6.4 の
  `auth_login_callback_test.go` 等の integration test で実施される予定（tasks.md L716〜L750
  の DB-backed verify 義務）。
- 実行日時: 2026-06-26

### Task 5.2

- **採用方針**: `backend/internal/auth/{handler.go, handler_test.go}` の 2 ファイルを新規追加し、
  `service_failure_kinds.go` に Handler 入口判定用の `FailureKindInvalidRequest` を 1 件追加、
  `doc.go` の構成リストを task 5.2 時点に更新。`Handler` struct は `Service` interface を DI で
  受け取り、`Mount(r chi.Router, consolePrefix string, console oidc.Console)` で sub-router
  経由（`r.Route(consolePrefix, func(sub chi.Router){...})`）に `/login` GET / `/callback` GET /
  `/logout` POST の 3 route を登録する。`console` は Mount 時点で closure に固定し HTTP
  request からは読み取らない（Req 6.2 / 6.4 の path-based クライアント分離強制）。
- **重要な判断**:
  - **`r.Route` 経由 sub-router 構築の必須**: tasks.md L556〜L566 / L566「**`r.Route(consolePrefix,
    ...)` 経由で sub-router を作る**」の指示通り、Mount 内で `r.Get("/login", ...)` を root 相対
    で登録すると `/api/auth/login` ではなく `/login` に登録されて Req 6.2 path-based 分離が成立
    しなくなる。`r.Route(consolePrefix, func(sub chi.Router){...})` で sub-router を構築し、
    その中で `sub.Get("/login", ...)` のように相対 path で登録する形を採用した。テスト
    `TestMount_RegistersAllSixEndpoints` が tenant + admin の 6 endpoint に到達できる
    ことを assertion して回帰を防ぐ。
  - **callback 入口での欠落判定を Handler 側に置く**: tasks.md L574〜L580 / design.md API
    Contract `/api/auth/callback` Errors 列「400（return_to が不正 URL / `code` 欠落 / `state`
    欠落）」の指示通り、`code` / `state` のいずれか欠落で 400 `invalid_request` を Service
    呼び出し前に返す（fake Service が呼ばれないことを assert）。Service 内に流すと
    `state.Verify` で `state_invalid` (401) や `oauth2.Exchange` で 502 に化けて契約と矛盾
    するため、必ず Handler 入口で判定する。`FailureKindInvalidRequest` を新規 sentinel として
    `service_failure_kinds.go` に追加し、`errors.Is` で識別可能にした（Service 側の sentinel と
    並列に配置 / responsibility separation）。
  - **state cookie 削除 helper の使い分け契約継承**: task 3.2 の確認事項で確立した
    「state cookie 削除 = `auth.ExpireCookieAttributes()`、session cookie 削除 =
    `auth.SessionExpireCookieAttributes()`」を Handler 経路でも厳格に守る。callback 成功時
    （session 発行 + state cookie 削除）/ callback エラー時（state cookie 即時削除）/
    callback 入口欠落判定（state cookie 即時削除）の **3 経路すべて**で `ExpireCookieAttributes()`
    を呼ぶ（state.go の helper）。logout 成功時のみ `SessionExpireCookieAttributes()` を呼ぶ
    （session.go の helper）。誤って取り違えると Req 2.8（state cookie 即時無効化）/ Req 5.1
    （logout 時 session cookie 削除）の片方が壊れるため、コメントに helper 名と削除対象を
    明示して維持容易性を確保した。
  - **logout cookie 不在で 401**: tasks.md L606「cookie 不在で 401」の指示通り、cookie 不在 /
    cookie value 空文字 のいずれも `FailureKindSessionTamper` で 401 を返す。`Service.Logout` は
    呼ばない（cookie 不在の場合は logout する対象が無いため）。tasks.md / requirements.md 上で
    Req 5.2 の応答コードは「401」と確定（test (d) の assertion と整合）。
  - **`console` 引数の `logout` 経路での扱い**: tasks.md は `h.logout(console)` シグネチャを
    指示しているが、現状の logout 経路では console を使う場面が無い（session token は console を
    持ち越さない opaque 値、`Service.Logout` も console を要求しない signature）。closure 引数
    として保持しつつ `_ = console` で意図的に未使用化し、`unused parameter` を vet で検出しない
    形にした。将来 logout 経路に console-aware なログ field（`console=tenant-console` 等）を
    追加する余地は残してある。
  - **httptest 単体テスト構造**: `fakeService` で Service interface 4 メソッドを mock し、
    `chi.NewRouter()` + `Handler.Mount` を tenant / admin の 2 度呼び出して 6 endpoint を
    同一 router 上に配線した。`httptest.NewRecorder()` で各 endpoint への request を実行し、
    `rec.Result().Cookies()` で Set-Cookie ヘッダ列を取り出して name / Value / MaxAge を
    assert する流儀を採用（既存 service_test.go の fake パターンと一貫）。callback 入口
    欠落判定では `fake.calls.handleCallback != 0` で Service が呼ばれていないことを直接
    assert している。
  - **機密値非埋込契約**: tasks.md L567〜L571 の指示通り、error wrap / `errors.WriteHTTP`
    経由の JSON body / response header に state cookie 生値・session cookie 生値・id_token
    raw JWT・OIDC client secret を文字列補間しない。テスト
    `TestCallback_ErrorResponse_DoesNotLeakSensitiveValues` で response body / header に
    state cookie 生値が含まれないことを assert（一次防御の回帰耐性 / NFR 1.1 / NFR 4.2）。
- **残存課題**:
  - 後続 task 6.1 `auth.Middleware` で `Service.LookupAndRefresh` を呼び出して
    `httpserver.AuthClaims` を ctx に注入する。本 Handler の `/api/auth/*` / `/api/admin/auth/*`
    は **TenantContextMiddleware の外側**で動作するため、後続 task 6.2 の `NewServer` 配線で
    `authMount(r, "/api/auth", ConsoleTenant)` / `authMount(r, "/api/admin/auth", ConsoleAdmin)`
    を root router 直下（TenantContextMiddleware より前）に Mount する責務がある。
  - 後続 task 6.4 の integration test（`auth_login_callback_test.go`）で実 IdP mock 経由の
    `/api/auth/login` → 302 / `/api/auth/callback` → 302 + session cookie 発行 + state cookie
    削除 / state replay → 401 + state cookie 削除 等を end-to-end で検証する。本 task の
    httptest 単体テストは fake Service 経由の Handler 境界網羅に閉じる（Service との結合は
    本 task のスコープ外）。
  - **logout 経路の console-aware ログ field 追加**: 現状の `logout` handler は `_ = console`
    で console 引数を意図的に未使用としているが、将来 `log.Info("logout", "console", string(console))`
    のような observability 改善を入れる余地がある（本 task 範囲外、後続 Issue で扱う）。
  - 確認事項: tasks.md L606 は logout cookie 不在の応答コードを「401」と明示しているが、
    一般的な REST API design では「204 No Content / 既に logout 済み扱い」「200 OK / no-op」
    にする選択肢もある。本 Issue では明示的に「401」を採用（攻撃検出可能性 + Req 5.2 の
    fail-closed 解釈と整合）。

### Task 5.2 — Verify 実行結果

- `cd backend && go build ./...`: PASS
- `cd backend && go vet ./...`: PASS
- `cd backend && go test ./... -count=1`: 全 package PASS。`internal/auth` の Handler 関連
  テスト 19 ケース（login 3 / callback 7 / logout 5 / Mount sweep 1 / 機密値非埋込 1 +
  TestMount 内 sub-test 6）+ 既存テスト全件 / 約 3 秒。`internal/platform/oidc` 等
  影響範囲全件で regression なし。
- DB-backed verify: 本 task は Handler 単体（fake Service / chi router / httptest.NewRecorder）
  で完結し、DB を要求しない。HTTP 経路を実 Service + DB に被せた e2e 検証は後続 task 6.4 の
  `auth_login_callback_test.go` 等の integration test で実施される予定（tasks.md L716〜L750
  の DB-backed verify 義務）。
- 実行日時: 2026-06-26

### Task 6

- **採用方針**: タスク `6` は umbrella header（`_Requirements:_` / `_Boundary:_` を持たない親 task）
  であり、本起動では直接の実装は無く、子 task 6.1（auth.Middleware 実装 + 単体テスト）/
  6.2（`httpserver.NewServer` に auth middleware + auth エンドポイント配線）/ 6.3
  （`cmd/api` bootstrap に OIDC Verifier / Auth 配線追加）/ 6.4（結合テスト auth 全フロー）が
  後続 fresh iteration で順次実装される前提として `### Task 6` learning スロットのみ整備する
  （先行する `### Task 1` / `### Task 2` / `### Task 3` / `### Task 4` / `### Task 5` の
  umbrella 処理パターンを踏襲）。
- **重要な判断**:
  - 親 task は `tasks.md` 上で `### Task 6` の learning スロットを成立させるためのプレースホルダ
    に留め、`backend/internal/auth/middleware.go` / `backend/internal/platform/httpserver/server.go` /
    `backend/cmd/api/main.go` / `backend/test/integration/auth_*_test.go` 等のコード追加・テスト
    追加は本 iteration では行わない（実装本体は 6.1 / 6.2 / 6.3 / 6.4 の fresh iteration が
    担当する設計 / `### Task 1` / `### Task 2` / `### Task 3` / `### Task 4` / `### Task 5` と
    同パターン）。
  - per-task ループ規約「1 commit = 1 task ID」に従い、本 iteration の marker commit は
    `docs(tasks): mark 6 as done` 単一の subject で `tasks.md` のみを含める。impl-notes.md への
    `### Task 6` 追加は marker commit と分離した別 commit に積む。
- **残存課題**: 子 task 6.1（`backend/internal/auth/middleware.go` + `middleware_test.go` の
  新規追加、`NewMiddleware(svc Service, expectedConsole oidc.Console, log logger.Logger,
  clock Clock) func(http.Handler) http.Handler` を tenant / admin 別インスタンスで構築可能に
  する構造、`__Host-ae_mdm_session` cookie 不在 → 401 + cookie 削除 + `failure_kind:
  session_tamper` ログ経路、`service.LookupAndRefresh(ctx, raw, expectedConsole, clock.Now())`
  呼び出しで console 照合 → absolute → revoked → idle の順で失効判定、`console_mismatch` 経路
  で tenant 系 cookie が admin route に提示された場合に 401 + cookie 削除 + `failure_kind:
  console_mismatch` ログ field 出力、成功時に `httpserver.AuthClaims{TenantID, AdminUserID,
  Roles, IsSuperAdmin}` を `httpserver.WithAuthClaims(ctx, ...)` で ctx 注入、機密値非埋込
  契約で `session_hash_prefix` / `failure_kind` / `console` のみを field 化）/ 6.2
  （`backend/internal/platform/httpserver/server.go` の `NewServer` シグネチャ拡張で `authMWTenant
  func(http.Handler) http.Handler` / `authMWAdmin func(http.Handler) http.Handler` / `authMount
  func(r chi.Router, consolePrefix string, console oidc.Console)` 3 引数を追加、`/api/auth` を
  root router 直下に `authMount(r, "/api/auth", ConsoleTenant)` で Mount + `/api/admin/auth` を
  `authMount(r, "/api/admin/auth", ConsoleAdmin)` で Mount、`apiRouter` の `Use(...)` に
  `authMWTenant` / `adminRouter` の `Use(...)` に `authMWAdmin` を `TenantContextMiddleware` の
  前段として挿入、nil 許容 fallback で既存 server_test.go の 401 default deny 経路を維持、
  cross-console reject 経路の server_test.go 追加）/ 6.3（`backend/cmd/api/main.go` bootstrap
  に `oidc.NewVerifier(ctx, cfg)` → 失敗時 exit 1 (NFR 3.2) → `auth.NewRepository(pool)` →
  `auth.NewService(cfg, verifier, repo, oauth2Configs, clock, auth.TokenGenerator(session.New),
  log)` → `authMWTenant := auth.NewMiddleware(svc, oidc.ConsoleTenant, log, clock)` /
  `authMWAdmin := auth.NewMiddleware(svc, oidc.ConsoleAdmin, log, clock)` 構築 →
  `httpserver.NewServer(cfg, log, pool, authMWTenant, authMWAdmin, authMount)` 注入、
  `oauth2Configs` は `map[oidc.Console]*oauth2.Config` で tenant / admin 各 `ClientID` /
  `ClientSecret` / `RedirectURL` / `Scopes: []string{goidc.ScopeOpenID, "email", "profile"}` /
  `Endpoint: verifier.TenantEndpoint()` で構築、`AuthStyle == oauth2.AuthStyleInHeader` の
  契約をユニットテストで回帰的に守る、`cmd/api/main_test.go` の bootstrap smoke test 更新）/
  6.4（`backend/test/integration/auth_login_callback_test.go` / `auth_session_lookup_test.go` /
  `auth_logout_revoke_test.go` の 3 ファイル新規追加、`docker compose up -d postgres` + test
  用 RSA private key + `httptest.NewServer` 経由の OIDC IdP mock で discovery / JWKS / token
  endpoint を提供、login → 302、callback → 302 + sessions テーブル 1 行 + cookie 生値 / DB
  hash + state cookie 削除、state cookie 改竄 → 401 + `failure_kind=state_invalid`、ID トークン
  aud 不一致 → 401 + `failure_kind=invalid_aud`、未 provisioning な (oidc_issuer, oidc_subject)
  → 403 + `failure_kind=admin_user_not_provisioned`、state replay → 401 +
  `failure_kind=state_replay` + state cookie 削除、idle 31 分後 → 401 + revoked_at 更新、
  absolute 8h+1s 後 → 401、tenant 用 session を `/api/admin/...` に提示 → 401 +
  `failure_kind=console_mismatch` + cookie 削除、logout 後の cookie 再提示 → 401、改竄 cookie
  → 401、DATABASE_URL 未設定時の `t.Skip` 経路）は後続 fresh iteration で消化する。子 task
  全完了時の親 task `6` の昇格は本 iteration で完了済みのため、auto-promotion 規約は no-op
  として扱う。

## 確認事項

本セクションは `requirements.md` / `design.md` / `tasks.md` 本文の書き換えを伴わずに、実装フェーズ
で気付いた spec との矛盾点・人間判断が必要なポイントを集約する場である。

- **task 3.2: cookie 属性 helper の命名衝突と rename 採用**: design.md L588 / L595 と tasks.md
  L318 / L321〜L324 は session cookie 属性 helper の名前を `CookieAttributes(ttl)` /
  `ExpireCookieAttributes()` と指示しているが、task 3.1 で同名の top-level 関数が
  `backend/internal/auth/state.go` に既に実装されており（state.go L151 / L170）、Go 同
  package 内 redeclaration エラーが発生する。design.md / tasks.md の散文では
  `state.ExpireCookieAttributes()` / `session.ExpireCookieAttributes()` のような sub-package
  風名前空間を想定していると読めるが、task 3.1 で auth を **フラット package** として確定
  しており retroactive な refactor は per-task ループ規約で禁止されている。本 task では
  session 側の cookie 属性 helper を **`SessionCookieAttributes` /
  `SessionExpireCookieAttributes`** に rename して disambiguate する判断を採用した（関数の
  責務・属性値・契約は spec 通り維持 / 他 helper `New` / `HashToken` / `HashPrefix` は state.go
  と衝突しないためそのまま）。後続 task 5.1 / 5.2 / 6.1 の Service / Handler / Middleware は、
  state cookie 削除 = `auth.ExpireCookieAttributes()` / session cookie 削除 =
  `auth.SessionExpireCookieAttributes()` を **明示的に呼び分ける**必要がある。
- なし（task 1.2 では tasks.md L99〜L173 の指示通りに migration / test fixture を更新でき、
  design 本文との矛盾点は発生していない）。
