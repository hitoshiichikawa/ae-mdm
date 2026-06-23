# Implementation Notes

## Implementation Notes

### Task 1

- **採用方針**: errors → logger → config の一方向依存で 3 package を新規実装し、
  cmd/api / cmd/worker / db / httpserver から共通 import 可能な基盤を確立する。
- **重要な判断**
  - `internal/errors` は他 internal package を import しない（`ErrLogger` interface を
    パッケージ内で定義し、`logger.Logger` の Warn / Error が structural typing で暗黙的に
    満たす設計）。Go の import cycle を物理的に回避し、design.md「Components and Interfaces」
    の依存方向と完全一致させた。
  - `logger.Err(err error)` は `*errors.Error` のときに `zap.Inline` 内で
    `error_code` / `error_message` / `error_cause` を構造化し、独自型外 error は標準の
    `zap.Error` にフォールバックする。logger 側のテスト
    （`TestLogger_SatisfiesErrLoggerInterface`）で compile-time に interface 整合を固定。
  - redaction は field 名のサブストリング一致（lowercase）で `redactFields` を kv ペアに
    適用する方式を採用。`set-cookie` のような派生キーも `cookie` が部分一致するため
    自動的に対象になり、design.md Security Considerations の「session_secret / id_token /
    refresh_token / cookie / google_application_credentials / sa_json / private_key /
    password」を網羅する。なお zap.Field 直渡しの値（型変換済み）は redaction されないため、
    機密値はかならず kv ペア（"id_token", token）で渡す運用とする。
  - `config.Load()` は値型 Config を返し、`loadFrom(envGetter)` を内部に切り出すことで
    `t.Setenv` を使う公開 API smoke と、決定論的な map ベース env テストの両立を実現。
    Req 1.4（immutable）は「pointer を返さない」「公開セッターを置かない」で担保。
  - `WriteHTTP` の called-once 契約は (a) request context 経由 sentinel と
    (b) writer ポインタを key にした `sync.Map` の二重で実装。HTTP ハンドラ defer 終了時
    に `ClearWriter` を呼ぶことで sentinel エントリのリークを防ぐ前提（後続 task 4.1 の
    recover middleware で利用する）。
- **残存課題（次 task への申し送り）**
  - `internal/platform/db` 配下（task 2.x）で `BeginTxFunc` から `errors.New(CodeTenantCtxMissing, ...)`
    を panic する際、recover 後の `WriteHTTP` 経路で `ClearWriter` を defer 呼び出しする
    こと。Issue は無いが期待動作として明記。
  - logger の `fileOpener` は init 時 1 回だけ呼ぶ前提で、ファイルローテーション
    （lumberjack 等）は本 task では未対応。NFR 4.2 を満たすコンテナ運用では通常 stderr /
    stdout で十分のため見送り。
  - `logger.SetDefault` / `Default` は process global の便宜上の API。本 Issue 後続 task
    （cmd/api / cmd/worker）で main から SetDefault を 1 回だけ呼ぶ運用前提。

## AC Traceability（Req 1 / 2 / 3）

| Requirement | テスト |
|---|---|
| 1.1 構造体としての env 読み込み | `config.TestLoad_AllRequiredPresent_ReturnsConfig` / `TestLoad_FromOSEnv_Smoke` |
| 1.2 必要 env を網羅 | `config.TestLoad_AllRequiredPresent_ReturnsConfig`（13 件の required を carry / `validEnv` fixture） |
| 1.3 必須欠落・不正フォーマットで fail-fast | `config.TestLoad_MissingRequired_ReturnsConfigInvalid` / `TestLoad_InvalidIntFormat_ReturnsConfigInvalid` / `TestLoad_SessionSecretTooShort_ReturnsConfigInvalid` |
| 1.4 immutable | 値型返却（pointer 共有しない設計）+ `TestLoad_AllRequiredPresent_ReturnsConfig` の Config 値型確認 |
| 1.5 api / worker / CLI 共通 import 可 | `internal/config` 配置（cmd/api / cmd/worker の bootstrap は task 5.x で配線） |
| 2.1 構造化ログファクトリ | `logger.TestNewLogger_AppliesConfigDefaults` |
| 2.2 tenant_id / request_id / message_id field helper | `logger.TestLogger_AddsFieldsHelpers` |
| 2.3 エラー観測時 WARN / ERROR 記録 | `errors.TestWriteHTTP_DomainError` / `errors.TestShouldAck_TransientDomainError_NacksWithWarn` / `errors.TestShouldAck_PermanentDomainError_AcksWithError` |
| 2.4 レベル / フォーマット / 出力先を env から | `logger.TestLogger_LevelThreshold_FiltersBelow` / `logger.TestNewLogger_RejectsInvalidLevel` / `TestNewLogger_RejectsInvalidFormat` / `TestNewLogger_FileOutput` |
| 2.5 機密の平文出力禁止 | `logger.TestRedactFields_RedactsSecretsBySubstring`（11 ケース）/ `TestLogger_RedactsSecretFieldValues` |
| 3.1 Code / Message / Cause を持つ Error 型 | `errors.TestError_ErrorMessage` |
| 3.2 errors.Is / As 互換 | `errors.TestError_IsAsCompatibility` |
| 3.3 HTTP ステータスマッピング | `errors.TestDefaultHTTPStatus_Mapping`（12 ケース）/ `TestWriteHTTP_DomainError` |
| 3.4 worker 最外層 ack / nack 判定 | `errors.TestShouldAck_NilError_AcksAndNoLog` / `TestShouldAck_TransientDomainError_NacksWithWarn` / `TestShouldAck_PermanentDomainError_AcksWithError` / `TestShouldAck_UnknownErrorType_NacksWithWarn`（4 ケースマトリクス） |
| 3.5 予期しないエラーは 5xx + 構造化ログ | `errors.TestWriteHTTP_NonDomainError_DefaultsTo500` |

Req 4 / 5 / 6 / 7 / NFR は後続 task（2.x / 3.x / 4.x / 5.x / 6.x）の責務であり、本 task では未対応。

## Verify

```sh
cd backend && go build ./... && go vet ./... && go test ./...
```

- `go build ./...`: PASS
- `go vet ./...`: PASS
- `go test ./...`:
  - `internal/config`: PASS
  - `internal/errors`: PASS
  - `internal/logger`: PASS
  - `cmd/api` / `cmd/worker` / `internal/depspin`: no test files（本 task 範囲外）

## 確認事項

なし（本 task 範囲では design.md / requirements.md / tasks.md と矛盾無し）。

STATUS: complete
