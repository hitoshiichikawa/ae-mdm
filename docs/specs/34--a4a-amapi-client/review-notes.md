# Review Notes

<!-- idd-claude:review round=1 model=claude-sonnet-4-5 timestamp=2026-06-26T00:00:00Z -->

## Reviewed Scope

- Branch: claude/issue-34-impl--a4a-amapi-client
- HEAD commit: 9ab90814b4b779f275b5c95f18fed85f69a3e622
- Compared to: develop..HEAD
- 注記: Issue #34 spec ディレクトリには `tasks.md` / `design.md` が存在しない（impl-notes
  の冒頭で「本サイクルでは Architect 経由の tasks.md が無い」と明記）。境界判定は umbrella
  `docs/specs/24-android-enterprise-emm-mvp/tasks.md` の task 4.1 (`_Boundary: AMAPIClient_`,
  `_Requirements: 1.1, 1.2, 3.1, 3.2, 4.1, 4.3, 6.1, 6.2, 7.1, NFR 1.1_` — umbrella の AC ID
  空間) と umbrella `design.md` line 127 の File Structure Plan
  (`backend/internal/platform/amapi/`) を参照した。AC 判定は Issue #34 の
  `requirements.md` を正本として実施。

## Verified Requirements

- 1.1 — `enterprises.go`:`CreateSignupURL` / `enterprises_test.go`:`TestCreateSignupURL_OK_ReturnsURLAndName`
- 1.2 — `enterprises.go`:`CreateEnterprise`/`GetEnterprise` /
  `TestCreateEnterprise_OK_ReturnsEnterpriseName`, `TestGetEnterprise_OK_NormalizesResponse`
- 1.3 — `policies.go`:`UpsertPolicy`/`GetPolicy` /
  `TestUpsertPolicy_OK_PatchesResource`, `TestGetPolicy_OK_NormalizesResponse`
- 1.4 — `devices.go`:`ListDevices`/`GetDevice` /
  `TestListDevices_PagesConcatenated`, `TestGetDevice_OK_NormalizesResponse`
- 1.5 — `devices.go`:`IssueCommand` / `TestIssueCommand_OK_ReturnsOperationName`
- 1.6 — `enrollment_tokens.go`:`CreateEnrollmentToken` /
  `TestCreateEnrollmentToken_OK_NormalizesResponse`
- 1.7 — `webtokens.go`:`CreateWebToken` / `TestCreateWebToken_OK_ReturnsValue`
- 1.8 — `types.go` の独自型（`Enterprise`/`PolicyBody`/`Device`/`EnrollmentToken`/`WebToken`）
  に正規化。`TestGetEnterprise_OK_NormalizesResponse`,
  `TestGetDevice_OK_NormalizesResponse`, `TestGetPolicy_OK_NormalizesResponse`,
  `TestCreateEnrollmentToken_OK_NormalizesResponse` で検証
- 1.9 — `*androidmanagement.Service` を保持する `realClient.svc` を unexported とし、
  外部から SDK を抽出する公開 API を提供しない（パッケージ境界による物理担保）
- 2.1 — `requireEnterpriseName` を該当全メソッド（`GetEnterprise`/`UpsertPolicy`/`GetPolicy`/
  `ListDevices`/`GetDevice`/`IssueCommand`/`CreateEnrollmentToken`/`CreateWebToken`）の
  先頭で呼び出し
- 2.2 — `CreateSignupURL`/`CreateEnterprise` のシグネチャに `enterpriseName` 引数なし
- 2.3 — `TestGetEnterprise_EmptyEnterpriseName_ReturnsInvalidRequest`,
  `TestUpsertPolicy_EmptyEnterpriseName_ReturnsInvalidRequest`,
  `TestListDevices_EmptyEnterpriseName_ReturnsInvalidRequest`,
  `TestCreateEnrollmentToken_EmptyEnterpriseName_ReturnsInvalidRequest`,
  `TestCreateWebToken_EmptyEnterpriseName_ReturnsInvalidRequest`,
  `TestStubClient_EnforcesEnterpriseNameGuard`
- 2.4 — `TestGetEnterprise_ResourcePathReflectsArgument` で受信パスに引数値そのまま反映
- 3.1 — `NewClient` で `option.WithCredentialsFile(cfg.GoogleApplicationCredentials)`
- 3.2 — `config.Config.GoogleApplicationCredentials`（既存 env 経路 `GOOGLE_APPLICATION_CREDENTIALS`
  から注入。`config/env.go` line 135 で `requiredStr` 経由読込）
- 3.3 — `TestNewClient_GoogleApplicationCredentialsEmpty_ReturnsConfigInvalid`
- 3.4 — ソース内に API Key リテラルなし。logger.Err() に raw 値を直接渡さない設計
- 3.5 — Google SDK 内部の `htransport` / `cloud.google.com/go/auth` に OAuth トークン
  キャッシュを委譲（impl-notes に明記）
- 4.1 — `TestMapAMAPIError_GoogleAPIError_Mapping`（400/401/403/404/409/422 各 case で
  IsTransient=false） / `TestDoWithRetry_DoesNotRetryOn4xx`
- 4.2 — `TestMapAMAPIError_GoogleAPIError_Mapping`（500/502/503/504/599 で IsTransient=true）/
  `TestDoWithRetry_GivesUpAfter4Attempts`
- 4.3 — `TestMapAMAPIError_GoogleAPIError_Mapping/status_403`（`CodeForbidden`）
- 4.4 — `TestMapAMAPIError_GoogleAPIError_Mapping/status_404`,
  `TestGetEnterprise_NotFound_ReturnsCodeNotFound`
- 4.5 — `TestMapAMAPIError_GoogleAPIError_Mapping/status_409`,
  `TestIssueCommand_409Conflict_NotRetried`
- 4.6 — `TestMapAMAPIError_NetworkError_IsTransientUnavailable`,
  `TestDoWithRetry_NetworkError_IsRetriedThenExhausted`
- 4.7 — `mapGoogleAPIError` が `gerr` を Cause として wrap。
  `TestMapAMAPIError_GoogleAPIError_Mapping` 内で `stdErrors.Is(out, gerr) == true` を検証
- 5.1 — `TestDoWithRetry_RecoversAfter429`（2x 429 → 3 回目成功）/
  `TestDoWithRetry_GivesUpAfter4Attempts`（defaultMaxRetries=3、計 4 試行）
- 5.2 — `TestDoWithRetry_GivesUpAfter4Attempts`（`CodeUpstream` + `IsTransient=true` 検証）
- 5.3 — `TestDoWithRetry_DoesNotRetryOn4xx`（403 で server calls=1）
- 5.4 — `TestDoWithRetry_RespectsContextCancel`（Sleep 中 cancel で `CodeUnavailable` 返却）
- 6.1 — `client.go` の `Client` interface 定義。`var _ Client = (*StubClient)(nil)` /
  `(*realClient)(nil)` の compile-time check（`stub.go` 末尾）
- 6.2 — `stub.go`:`StubClient` を `amapi` パッケージ内に同梱
- 6.3 — `TestStubClient_*`（stub_test.go）は httptest.Server を一切立てない。
  `StubClient` は SDK を構築せず record + フック呼び出しのみ
- NFR 1.1 — `realClient.logOutcome` が `operation`/`enterprise_name`/`duration_ms`/
  `attempts`/`outcome` の 5 field を 1 行で出力（`client.go` の `logOutcome`）
- NFR 1.2 — `EnrollmentToken.Value` / `WebToken.Value` を構造化 field 化しない設計
- NFR 1.3 — `doWithRetry` の `Warn` ログに `attempt`/`next_attempt`/`cause_kind`。
  `TestClassifyCause_429Or5xxOrNetwork` で network/429/5xx 分類を検証
- NFR 2.1 — `cfg.GoogleApplicationCredentials` のみを credentials source とし、リポジトリ内
  固定パス・ハードコード値を一切参照しない
- NFR 2.2 — AMAPI SDK の固定 BasePath（`https://androidmanagement.googleapis.com/`）に従う。
  本番ビルドで上書き手段を導入しないため HTTPS 必須が成立
- NFR 3.1 — `config.Config` / `internal/errors` の `Code` 体系 / `logger.Logger` interface に
  破壊的変更なし（diff 対象に `backend/internal/{config,errors,logger}/` 配下なし）

## Findings

なし

## Summary

Issue #34 (A4a: AMAPI Client 共有ラッパ) の全 AC（Req 1.1〜1.9 / 2.1〜2.4 / 3.1〜3.5 /
4.1〜4.7 / 5.1〜5.4 / 6.1〜6.3 / NFR 1.1〜1.3 / NFR 2.1〜2.2 / NFR 3.1）が
`backend/internal/platform/amapi/` 配下の実装と対応テストでカバーされている。
変更範囲は umbrella tasks 4.1 の `_Boundary: AMAPIClient_` と design.md File Structure Plan
（`amapi/`）に完全に閉じており、boundary 逸脱なし。AC と test の trace は impl-notes の
AC Traceability 表とも整合している。

RESULT: approve
