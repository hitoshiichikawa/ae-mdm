# Implementation Notes

本ファイルは Issue #34（A4a: AMAPI Client 共有ラッパ）の実装 learning を記録する。
`docs/specs/34--a4a-amapi-client/requirements.md` および umbrella `docs/specs/24-android-enterprise-emm-mvp/`
配下の `design.md` / `tasks.md` は実装 PR では書き換えず、矛盾や設計上の判断は本ファイル末尾
「確認事項」節に記録する（本サイクルでは Architect 経由の `tasks.md` が無いため、per-task
loop ではなく Issue 全体一括での implementation）。

## Implementation Notes

### 採用方針

- **パッケージ配置**: `backend/internal/platform/amapi/`（umbrella tasks 4.1 の指示通り）。
  - `client.go` / `enterprises.go` / `policies.go` / `devices.go` / `enrollment_tokens.go` /
    `webtokens.go` / `stub.go` / `types.go` の 8 ファイルに分割。
- **`Client` interface のシグネチャ**: umbrella `design.md` line 578-590 の `AMAPIClient` interface を
  そのまま採用（引数名・順序・戻り値の名前を一切変更しない）。
- **AMAPI 呼び出しの実装**: Google 公式 SDK
  `google.golang.org/api/androidmanagement/v1` をそのまま利用。go.mod / go.sum の差分は出ず、
  既存依存（pubsub 経由）から transitive に解決済み。`go mod tidy` 実施 → 差分 0 を確認済み。
- **認証**: `option.WithCredentialsFile(cfg.GoogleApplicationCredentials)` で service account JSON
  を SDK に渡す。OAuth アクセストークンの取得・キャッシュ・リフレッシュは SDK 内部（`htransport`
  / `cloud.google.com/go/auth`）に委ねる（Req 3.5）。
- **AMAPI レスポンスの正規化**: `Enterprise` / `PolicyBody` / `Device` / `CommandRequest` /
  `EnrollmentToken` / `EnrollmentTokenRequest` / `WebToken` を `types.go` に集約。SDK 型を
  そのまま漏らさず、MVP に必要な最小フィールドのみ公開（Req 1.8）。
- **`enterpriseName` 必須化**: 該当全メソッド先頭で `requireEnterpriseName(...)` ガードを呼ぶ
  （Req 2.3 / 物理担保）。`StubClient` 側にも同 guard を入れることで、domain Service 単体テストでも
  本番と同じ contract を再現する。
- **再試行戦略**: `doWithRetry` helper に集約。base=100ms、係数=2、最大 3 回（=4 試行）の
  exponential backoff（Req 5.1）。429 / 5xx / network / context cancel を分類し、context
  cancel は backoff 中でも即時中断（Req 5.4）。
- **エラーマッピング**: `mapAMAPIError` / `mapGoogleAPIError` に集約。HTTP status →
  ドメイン `Code` を 1 表で対応（400→InvalidRequest / 401→Unauthenticated / 403→Forbidden /
  404→NotFound / 409→Conflict / 422→BusinessRule / 429・5xx→CodeUpstream+transient /
  network→CodeUnavailable+transient / context cancel→CodeUnavailable+transient / 不明→
  CodeUpstream+transient「保守側」）。
- **構造化ログ**: `logger.Logger` を `NewClient` に DI。操作ごとに `operation`,
  `enterprise_name`, `duration_ms`, `attempts`, `outcome` を `Info` / `Warn`。再試行は
  `Warn` で `attempt`, `next_attempt`, `cause_kind`（429 / 5xx / network）を構造化 field 化
  （NFR 1.1 / 1.3）。トークン / WebToken の値そのものは log field に乗せない（NFR 1.2）。
- **テスト戦略**: AMAPI SDK が固定 BasePath を持つため、`endpointRewriter` RoundTripper で
  リクエスト URL を `httptest.Server` へ書き換える。`option.WithoutAuthentication()` と
  `option.WithHTTPClient(...)` を併用することで、SDK 内部の認証を短絡しつつ実機相当の HTTP 経路で
  挙動テスト可能。Sleep は Options で no-op に差し替えて再試行テストを決定論かつ高速化。

### 重要な判断

- **既存依存だけで androidmanagement SDK を解決**: go.sum に v0.186.0 が既に存在（pubsub
  経由の transitive）。`go mod tidy` 後も go.mod / go.sum に diff は出なかった。よって新しい
  依存追加コミットは不要。
- **`Options` の試験用フックは最小限**: `HTTPClient` / `MaxRetries` / `BaseBackoff` / `Now` /
  `Sleep` の 5 つに絞る。`Retry-After` ヘッダ対応・jitter・per-method backoff オーバーライド等は
  MVP では入れず、確認事項へ記載。
- **`UpsertPolicy` の Raw → SDK 型変換**: `encoding/json` を経由した round-trip で実現。これに
  より AMAPI Policy の任意フィールド（数百項目）を domain Service 側で raw map で組み立てれば
  そのまま pass-through できる。MVP では型安全を犠牲にしつつ、開発速度を取った。
- **`ListDevices` のページング**: AMAPI 既定 pageSize を採用し、内部で nextPageToken 連結。
  呼び出し側 domain（device service）はページング懸念を持たず全件処理に集中できる。
- **`IssueCommand` の戻り値**: AMAPI が返す `Operation.Name`（"enterprises/.../operations/{id}"）
  をそのまま `commandID` として返す。末尾 ID 部分だけ切り出して返す案も検討したが、Operation の
  完了状態取得などは domain Service の責務で、本ラッパでは pass-through が誤解を生まないと判断。
- **`StubClient` も guard を実装**: テスト用 stub であっても `requireEnterpriseName` を内部で
  呼び、`OnXxx` フックの前にガードする。これにより、domain Service 単体テストで「うっかり
  enterpriseName=\"\" で呼んでいた」バグを stub で検出できる。
- **`net.Error` を含む型不明エラーは保守側（transient）に倒す**: 起源不明エラーを worker 側で
  恒久エラーとして ack してしまうリスクを避けるため。

### 残存課題

- **`Retry-After` ヘッダ未対応**: 429 でレート制限ヘッダ（`Retry-After`）の値に従う最適化は
  MVP では入れていない（exponential backoff のみ）。AMAPI が高頻度に 429 を返す状況が観測
  された場合、`mapGoogleAPIError` 周辺で `googleapi.Error.Header["Retry-After"]` を読み取って
  `doWithRetry` の delay を上書きする拡張を検討。
- **OAuth token のキャッシュ統計**: 現在 SDK 内部のキャッシュをそのまま利用しており、
  token 取得失敗時のリトライ・期限切れ検出のメトリクスは出していない。可観測性強化が
  必要になった段階で `internal/auth` の token redact 規約と合わせて追加する。
- **`UpsertPolicy.Raw` の型安全強化**: 当面は map のままだが、umbrella 後続 Issue で
  domain `policy.Service` 側に policy schema 検証を入れる際、本ラッパ側にも厳密型の
  alternative API（例: `UpsertPolicyTyped(ctx, ..., body *androidmanagement.Policy)`）の追加を
  検討する。
- **HTTPS 強制（NFR 2.2）の物理担保**: 本実装は AMAPI SDK の固定 BasePath
  `https://androidmanagement.googleapis.com/` に従うため HTTPS 必須が成立する。テスト時の
  endpointRewriter は test-only 経路で本番ビルドには影響しない。本番 build で BasePath を
  上書きする env var を導入しない方針なら NFR 2.2 は満たされる（本サイクルでは導入しない）。
- **integration テスト不在**: 本実装は SDK の Call.Do() を `httptest.Server` でモックする
  単体テストのみ。実際の AMAPI（GCP project）への通通信は umbrella 後続の statging
  smoke test で検証する想定。

### AC Traceability（受入基準 → テスト対応）

| Req | AC | カバーするテスト |
|---|---|---|
| 1.1 | CreateSignupURL を提供 | `TestCreateSignupURL_OK_ReturnsURLAndName` |
| 1.2 | CreateEnterprise / GetEnterprise を提供 | `TestCreateEnterprise_OK_ReturnsEnterpriseName`, `TestGetEnterprise_OK_NormalizesResponse` |
| 1.3 | UpsertPolicy / GetPolicy を提供 | `TestUpsertPolicy_OK_PatchesResource`, `TestGetPolicy_OK_NormalizesResponse` |
| 1.4 | ListDevices / GetDevice を提供 | `TestListDevices_PagesConcatenated`, `TestGetDevice_OK_NormalizesResponse` |
| 1.5 | IssueCommand を提供 | `TestIssueCommand_OK_ReturnsOperationName` |
| 1.6 | CreateEnrollmentToken を提供 | `TestCreateEnrollmentToken_OK_NormalizesResponse` |
| 1.7 | CreateWebToken を提供 | `TestCreateWebToken_OK_ReturnsValue` |
| 1.8 | レスポンスを本ラッパ独自型へ正規化 | `TestGetEnterprise_OK_NormalizesResponse`, `TestGetDevice_OK_NormalizesResponse`, `TestGetPolicy_OK_NormalizesResponse`, `TestCreateEnrollmentToken_OK_NormalizesResponse` |
| 1.9 | SDK 直接呼び出しの経路を許容しない | `internal/platform/amapi` パッケージ外部から `*androidmanagement.Service` を抽出する公開 API を提供しないことで物理担保。test 用 `endpointRewriter` も本パッケージ内に閉じる |
| 2.1 | enterpriseName を全関連操作で必須化 | `requireEnterpriseName` を全該当メソッド先頭で呼ぶ実装 + `TestRequireEnterpriseName_*` |
| 2.2 | CreateSignupURL / CreateEnterprise は例外 | 両 API シグネチャに `enterpriseName` 引数を持たない設計 |
| 2.3 | 空 enterpriseName を不正引数で拒否 | `TestGetEnterprise_EmptyEnterpriseName_ReturnsInvalidRequest`, `TestUpsertPolicy_EmptyEnterpriseName_ReturnsInvalidRequest`, `TestListDevices_EmptyEnterpriseName_ReturnsInvalidRequest`, `TestCreateEnrollmentToken_EmptyEnterpriseName_ReturnsInvalidRequest`, `TestCreateWebToken_EmptyEnterpriseName_ReturnsInvalidRequest`, `TestStubClient_EnforcesEnterpriseNameGuard` |
| 2.4 | enterpriseName を内部で置換しない | `TestGetEnterprise_ResourcePathReflectsArgument`（受信パスに引数値がそのまま反映） |
| 3.1 | service account 資格情報で AMAPI 呼び出し | `NewClient` で `option.WithCredentialsFile` 利用 |
| 3.2 | 環境変数経由で資格情報読み込み | `config.Config.GoogleApplicationCredentials`（既存 env 経路から注入）|
| 3.3 | 資格情報未設定で操作呼び出しエラー | `TestNewClient_GoogleApplicationCredentialsEmpty_ReturnsConfigInvalid` |
| 3.4 | 秘密値の埋め込み禁止 | コードに API Key 文字列リテラル無し（grep で確認可能）。logger の `Err()` は値を redact 経由で出力 |
| 3.5 | OAuth トークン キャッシュ | Google SDK 内部の `htransport` / `cloud.google.com/go/auth` に委譲（実装委譲）|
| 4.1 | 4xx → 再試行不可ドメインエラー | `TestMapAMAPIError_GoogleAPIError_Mapping`（400/401/403/404/409/422 各 case）/ `TestDoWithRetry_DoesNotRetryOn4xx` |
| 4.2 | 5xx → 再試行可能エラー | `TestMapAMAPIError_GoogleAPIError_Mapping`（500/502/503/504/599） / `TestDoWithRetry_GivesUpAfter4Attempts` |
| 4.3 | 403 → 「権限不足」識別可能 | `TestMapAMAPIError_GoogleAPIError_Mapping/status_403`（CodeForbidden） |
| 4.4 | 404 → 「未検出」識別可能 | `TestMapAMAPIError_GoogleAPIError_Mapping/status_404`, `TestGetEnterprise_NotFound_ReturnsCodeNotFound` |
| 4.5 | 409 → 「競合」識別可能 | `TestMapAMAPIError_GoogleAPIError_Mapping/status_409`, `TestIssueCommand_409Conflict_NotRetried` |
| 4.6 | network 失敗 → 再試行可能 | `TestMapAMAPIError_NetworkError_IsTransientUnavailable`, `TestDoWithRetry_NetworkError_IsRetriedThenExhausted` |
| 4.7 | エラーに HTTP status / メッセージ要約を保持 | `mapGoogleAPIError` が `gerr` を Cause として wrap。`errors.Is(out, gerr) == true` を `TestMapAMAPIError_GoogleAPIError_Mapping` で検証 |
| 5.1 | 429 / 5xx で最大 3 回 exponential backoff | `TestDoWithRetry_RecoversAfter429`（2x 429 → 3 回目成功）, `TestDoWithRetry_GivesUpAfter4Attempts` |
| 5.2 | 再試行枯渇後に再試行可能エラーで返す | `TestDoWithRetry_GivesUpAfter4Attempts`（CodeUpstream + IsTransient=true） |
| 5.3 | 4xx は再試行しない | `TestDoWithRetry_DoesNotRetryOn4xx`（403 で calls=1） |
| 5.4 | context cancel で再試行中断 | `TestDoWithRetry_RespectsContextCancel` |
| 6.1 | 公開 IF が interface（差し替え可能）| `Client` interface 定義 + `var _ Client = (*StubClient)(nil)` / `(*realClient)(nil)` の compile-time check |
| 6.2 | stub 実装を同一パッケージ内で提供 | `stub.go` の `StubClient`、`TestStubClient_*` |
| 6.3 | stub 利用時に実 AMAPI 呼び出し発生しない | `StubClient` は SDK を構築せず内部で `recordCall` + フックのみ。`TestStubClient_*` は server を一切立てない |
| NFR 1.1 | 操作・enterpriseName・所要時間・最終結果をログ | `realClient.logOutcome` の field 構成（`TestDoWithRetry_RecoversAfter429` で `warn_calls >= 2` を assertion） |
| NFR 1.2 | 秘密値の値そのものを記録しない | EnrollmentToken.Value / WebToken.Value を構造化 field 化しない実装。logger.Err() に raw 値を渡さない |
| NFR 1.3 | 再試行回数と原因種別をログ | `classifyCause` を `Warn` field `cause_kind` に出力。`TestClassifyCause_429Or5xxOrNetwork` で network/429/5xx 分類検証 |
| NFR 2.1 | 環境変数 / 環境変数で示されたパスの資格情報のみ | `cfg.GoogleApplicationCredentials` のみを credentials source とする実装 |
| NFR 2.2 | HTTPS 以外で呼び出さない | AMAPI SDK の固定 BasePath が HTTPS。本番ビルドで上書き手段を提供しない |
| NFR 3.1 | 既存共通基盤に破壊的変更を加えない | `config.Config` の追加無し（既存 `AMAPIProjectID` / `GoogleApplicationCredentials` をそのまま利用）/ `internal/errors` の Code 追加無し / `logger.Logger` の interface 変更無し |

### 確認事項

- **`config.Config.AMAPIProjectID` 未使用**: 本実装では `CreateEnterprise` の `projectID` は引数
  経由（umbrella design 通り）で受け取り、ラッパ側の `Config.AMAPIProjectID` は直接参照しない。
  domain Service（Tenant Service）側がこの `Config.AMAPIProjectID` を引数として渡す責務を持つ
  ことが必要。tasks 4.2 系 PR で配線時に確認すること。
- **`Service Interface` 抜粋（umbrella design line 578）と一部用語の整合**:
  signature の `CreateWebToken` 第 3 引数を `parentFrameURL` としたが、design の元コードでは
  `parentFrameURL string` と表記。本ラッパも `parentFrameURL` で実装した。AMAPI SDK 側は
  `parentFrameUrl`（小文字 url）。HTTP body は SDK が自動でケース変換。
- **`logger.Logger` の DI 経路**: 本 Issue 範囲では DI 配線（cmd/api.go から `NewClient` への
  Logger 注入）は実施せず、`Client` interface とそのテストのみ完成させた。後続 Issue で
  Tenant Service / Policy Service が `NewClient` を bootstrap する際、`logger.Default()` または
  process logger を注入する経路を確立する必要がある。
- **Mock 用 IF の API 安定性**: `StubClient.OnXxx` のフックシグネチャは本ラッパ独自型に依存
  するため、後続で `EnrollmentTokenRequest` 等のフィールド追加時にテスト側を更新する必要が
  ある。型を `domain.Service` 側ではなく `amapi` パッケージで定義する設計は、AMAPI スコープを
  domain 側から隠す観点で正解。

STATUS: complete
