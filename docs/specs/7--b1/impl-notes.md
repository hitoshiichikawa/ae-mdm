# 実装ノート（B1 エンロール / Issue #7）

## Implementation Notes

### Task 1

- **採用方針**: `policy` domain を手本に、enrollment domain の型基盤（types.go）と token snapshot
  repository（repository.go）を追加。cross-domain 依存は consumer-defines-interface（primitive 型
  ポート）で倒置し、実 Service 配線は cmd/api（task 3）に委ねる。
- **重要な判断**:
  - `DeriveStatus(expiresAt, now)` を clock を隠し持たない純粋関数化（`now` を引数で受ける）。
    判定境界は `now < expiresAt → active` / `now >= expiresAt → expired` に固定し、`now == expiresAt`
    は expired とした（有効期限「まで」有効）。境界（等時刻 / 直前 1ns / 直後 1ns）を単体テストで固定。
  - `AdditionalData.Marshal` は内部 `additionalDataJSON`（明示 json tag）へ写像してから marshal し、
    AMAPI additionalData と `enrollment_tokens.additional_data` snapshot で同一 wire-format を共有。
  - `TokenRow.AdditionalData` は JSON 文字列（`string`）で保持し pgx JSONBCodec に string を bind
    （`[]byte` は bytea 扱いになり jsonb に不適）。mode enum は scan 時に一旦 `string` を経由して
    `Mode` へ写像（named string 型の scan 互換）。`rowScanner` 拡張点で実 DB 非依存の scan テストが可能。
  - 依存倒置ポート（enterpriseResolver / policyChecker / eventRecorder）を design.md どおり types.go に
    宣言。`eventRecorder` は `audit.Event` を参照するため types.go は `audit` を import する（doc.go の
    許可リスト内）。**amapi は task 1 では import しない**（発行は service.go=task 2 の責務）。
  - `Value`/`QRCode` 相当の秘密値は `TokenRow` の列・field に一切持たせず、reflect テストで field 集合を
    固定（NFR 3.1）。秘密値は将来 `TokenView`（HTTP 応答一度きり）のみで返す。
- **残存課題（task 2 Service へ影響）**:
  - sentinel error（`ErrInvalidMode`=400 / `ErrPolicyRequired`=400 / `ErrTokenPersist`=503）と 3 ポートは
    宣言のみで未使用。task 2 の Service（validateMode → policyChecker → enterpriseResolver → amapi 発行
    → Insert → 監査）で消費する。
  - `TokenRow.AdditionalData` の string→jsonb bind の実 DB 検証は task 6 結合テストで固定する（task 1 は
    pure helper テストのみで DB 非接続）。
- **確認事項**: なし（design.md / tasks.md との矛盾は検出せず。orchestrator 補足の「audit を実 import
  しない」は、design.md の `eventRecorder` ポート契約（audit.Event 参照）と整合させ audit のみ import・
  amapi 非 import と解釈した。spec 本文は未改変）。

### Task 2

- **採用方針**: `policy.Service` を手本に enrollment Service（`IssueToken` / `ListTokens`）を実装。
  AMAPI-first + inconsistency ログ / 秘密値非記録 / 成否いずれも監査、を踏襲。
- **重要な判断**:
  - deps は consumer-defined ポートに限定し、`enrollmentClient`（`CreateEnrollmentToken` のみ）と
    最小 `Clock`（`Now()`）を service.go 内に宣言（amapi.StubClient / SystemClock が structural に満たす）。
    `NewService(repo, client, recorder, tenants, policies, clock, log)`。log==nil→Default / clock==nil→SystemClock。
  - `EnrollmentToken.ExpirationTime`（RFC3339）は `time.Parse` で変換。**parse 失敗は AMAPI 発行済みで
    snapshot 確定不能な上流契約違反**とし、inconsistency ERROR ログ + 失敗監査 + `CodeUpstream` 伝達（安全側）。
  - 検証失敗（不正モード / policy 未指定・不在 / enterprise 解決失敗 / AMAPI 失敗 / 永続化失敗）の
    **全経路で失敗監査を Record**（design.md:161「成否いずれの経路でも」）。Detail は `{mode, expires_at, result, token_id}`
    の安全 field のみで Value/QRCode を一切載せない。秘密値は `TokenView` の HTTP 応答一度きり。
  - 永続化失敗は sentinel `ErrTokenPersist`(503) を伝達し、DB 原因は inconsistency ログ（amapi_token_name /
    requires_reconciliation）へ退避（NFR 3.1 に抵触しない resource 名のみ記録）。
- **残存課題（task 3 Handler へ影響）**: Service は authorizer を持たない（RBAC は Handler の責務）。
  handler.go / cmd/api 配線 / policyChecker アダプタ（policy.Service.Get 包み）は task 3 で構築する。
  DEDICATED の `PolicyName` は policyChecker が返す amapi policy id をそのまま設定する契約。

#### AC Traceability（task 2 範囲: 1.1 / 1.2 / 1.3 / 1.4 / 1.5 / 1.6 / 5.1 / 5.2 / NFR 3.1）

| Req ID | 担保テスト |
|---|---|
| 1.1 | `TestService_IssueToken_FullyManaged_UsesDisallowedPersonalUsage` / `_ReturnsSecretQRDataInViewOnly` |
| 1.2 | `TestService_IssueToken_Dedicated_SetsResolvedPolicyName`（PolicyName=amapi policy id） |
| 1.3 | `TestService_IssueToken_FullyManaged_EmbedsTenantIssuerModeInAdditionalData` |
| 1.4 | `TestService_IssueToken_InvalidMode_ReturnsErrorWithoutCallingAMAPI`（空文字 / 未知値・AMAPI 非呼出） |
| 1.5 | `_DedicatedWithoutPolicy_ReturnsErrRequiredWithoutCallingAMAPI`（nil / uuid.Nil）/ `_DedicatedPolicyNotFound_PropagatesWithoutCallingAMAPI` |
| 1.6 | `TestService_IssueToken_AMAPIError_SkipsInsertAndRecordsFailureAudit`（Insert 非呼出）+ leak テストの Insert 失敗経路 |
| 5.1 | `_AMAPIError_SkipsInsertAndRecordsFailureAudit`（失敗）/ `_Success_RecordsAuditWithSafeFields`（成功: actor/tenant/mode/expires_at/result） |
| 5.2 / NFR 3.1 | `TestService_IssueToken_DoesNotLeakSecretToAuditOrLogs`（成功 + inconsistency ログ経路） |

> 補助テスト（AC 非紐付け / 品質補完）: `_ListTokens_DerivesStatusFromExpiresAt`（Req 4.1 の Service 経由確認）/
> `_ListTokens_RepositoryError_Propagates`（Repository エラー伝達）。

## AC Traceability（task 1 範囲: 1.3 / 4.1 / NFR 3.1）

| Req ID | 担保テスト |
|---|---|
| 1.3 | `TestAdditionalData_Marshal_WithValues_ContainsTenantIssuerMode` / `_NilUUIDs_EmitsZeroUUIDStrings`（tenant_id/issued_by/mode を含む JSON） |
| 4.1 | `TestDeriveStatus_*`（active/expired 派生 + 境界: 等時刻 / 直前 1ns / 直後 1ns） |
| NFR 3.1 | `TestTokenRow_HasNoSecretFields`（Value/QRCode 相当 field 不在 + field 集合固定）/ `TestAdditionalData_Marshal_DoesNotLeakSecretKeys` |

> 補助テスト（AC 非紐付け / task 2 が Req 1.4 を所有）: `TestMode_Valid` / `TestParseMode`（enum 判定健全性）。

## Verify 結果（task 1 時点）

- `go build ./...` PASS / `go vet ./...` PASS
- `go test ./...` PASS（全 20 パッケージ。既存テスト無破壊。結合テストは DB 未接続で t.Skip）

STATUS: complete
