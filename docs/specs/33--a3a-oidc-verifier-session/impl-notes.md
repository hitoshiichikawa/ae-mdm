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

## 確認事項

本セクションは `requirements.md` / `design.md` / `tasks.md` 本文の書き換えを伴わずに、実装フェーズ
で気付いた spec との矛盾点・人間判断が必要なポイントを集約する場である。

- なし（task 1.2 では tasks.md L99〜L173 の指示通りに migration / test fixture を更新でき、
  design 本文との矛盾点は発生していない）。
