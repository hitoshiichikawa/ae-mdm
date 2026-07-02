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

### Task 3

- **採用方針**: `policy.Handler` を手本に enrollment Handler（POST 発行 / GET 一覧）を追加し、cmd/api で
  enrollment domain を配線。policyChecker アダプタは cmd/api 層で policy.Service を包み cross-domain import を回避。
- **重要な判断**:
  - Handler は own-tenant RBAC のみを担い、actor/tenantID は claims 由来のみを Service へ渡す（越境発行を
    構造的に排除 / Req 2.3）。越境 policy 指定は policyChecker→policy.Service.Get が非露出 404 を返し、Handler は
    `pkgerrors.WriteHTTP` でそのまま写像（handler test では fakeService の CodeNotFound で再現）。
  - `policySvc` を `buildPolicyHandler` 内部から main レベルへ引き上げ（新設 `buildPolicyService`）、enrollment の
    policyChecker と **共有**。命名不変条件（policy の amapi policy id == DB uuid 文字列）に依拠し
    `ResolveOwnedPolicy` は `policyID.String()` を返す。既存 `TestBuildPolicyHandler_...` は新シグネチャ追従（assertion 不変）。
  - GET の `TokenSummary.Status` は `expires_at` 由来（active/expired）のみ。4.2（使用済み）は列不在で能動追跡せず
    Handler にコメント明示（design リスク 2 / observable「未登録」は notification path=task 6 が担保）。
  - handler_test は service_test.go の `fakeLogger` を流用し、Service は `fakeHandlerService` spy で差し替え。
- **残存課題（task 4/5/6 へ影響）**: なし（Handler/配線は完結。ENROLLMENT 通知経路の Registrar/handler は
  task 4/5、結合テストは task 6 で別途構築。worker wire-in は #36 で本 Issue scope 外）。

#### AC Traceability（task 3 範囲: 2.1 / 2.2 / 2.3 / 4.1）

| Req ID | 担保テスト |
|---|---|
| 2.1 | `TestHandler_Issue_TenantAdmin_Returns200`（actor/tenant/body 写像）/ `TestHandler_Issue_Operator_Returns200` |
| 2.2 | `TestHandler_Issue_Viewer_Returns403`（Create 未呼出 + deny WARN） |
| 2.3 | `TestHandler_Issue_CrossTenantPolicy_Returns404NonExposing`（非露出 404 + own-tenant 境界の tenantID 写像） |
| 4.1 | `TestHandler_List_ReturnsExpiresAtDerivedStatus`（active/expired 派生 + 自テナント scoped） |

> 補助テスト（AC 非紐付け / 入力検証・防御ガード）: `TestHandler_Issue_MalformedJSON_Returns400` /
> `TestHandler_Issue_NoClaims_Returns401`。cmd/api 配線回帰: `TestBuildEnrollmentHandler_WiresEnrollmentDomainNotStub`。
> **確認事項**: なし（design/tasks との矛盾は検出せず。spec 本文は未改変）。

### Task 4

- **採用方針**: `repository.go`（TokenRepository）を手本に `Registrar.UpsertEnrolledDevice` を追加し、
  ctx 由来 tenant_id を明示 bind した `INSERT ... ON CONFLICT DO UPDATE` で devices を冪等 upsert する。
- **重要な判断**:
  - upsert 実行を `deviceExecer` seam（`Exec(ctx, sql, args...) (pgconn.CommandTag, error)` / pgx.Tx が
    満たす）+ `withTx` 関数 seam に分離。本番は `db.BeginTxFunc` で tenant-scoped tx を開き RLS に分離を
    委ねる（SuperAdmin 昇格しない）。単体テストは fake execer / txRunner を注入し実 PostgreSQL 非接続で
    bind 値・SQL・ガードを固定（既存ファイルは触らず `registration.go` 内で seam を完結 / `_Boundary:_` 遵守）。
  - ctx 未確立は `db.FromContext` で先行ガードして DB へ触れず伝播（`BeginTxFunc` の panic 経路に載せない）。
    この経路は非 transient（CodeTenantCtxMissing = プログラミング前提違反）で越境更新を構造的に防ぐ（NFR 2.1）。
  - DB 失敗は exec 失敗地点で `&errors.Error{Code: CodeUnavailable, IsTransient: true}` をリテラル構築し、
    さらに `asTransientUpsertErr`（tenant.asTransientReverseLookupErr を手本）で BeginTx/Commit 由来の
    非 transient error も一律 transient へ正規化。DB 失敗を漏れなく nack（再処理保持 / Req 3.6）へ写像する。
    Message は機密値を補間しない固定文言（NFR 3.1）。
- **残存課題（task 5 / 6 への影響）**: `UpsertEnrolledDevice(ctx, amapiDeviceName, mode, complianceStatus string) error`
  は task 5 の `notification.EnrollmentRegistrar` port を primitive 型のみで満たす契約。実 upsert の冪等性
  （同一 amapi_device_name の 2 回通知で devices 件数 1 のまま）は本 task では単体で固定せず、task 6 の
  実 PostgreSQL 結合テストが所有する（design 明記）。`NewRegistrar(pool)` は task 6 が呼ぶ前提で公開済み。

#### AC Traceability（task 4 範囲: 3.1 / 3.4 / NFR 2.1）

| Req ID | 担保テスト |
|---|---|
| 3.1 | `TestRegistrar_UpsertEnrolledDevice_BindsTenantFromContext`（tenant_id=ctx 由来・amapi_device_name/mode/compliance の bind 写像）/ `_MapsModeAndComplianceToBind`（fully_managed/dedicated × unknown/unsupported の値域） |
| 3.4 | `TestRegistrar_UpsertEnrolledDevice_EmitsIdempotentOnConflictSQL`（`ON CONFLICT (tenant_id, amapi_device_name) DO UPDATE SET mode/compliance` が diff 内に存在。実冪等挙動は task 6） |
| NFR 2.1 | `_BindsTenantFromContext`（tenant_id を ctx 由来で明示 bind）/ `_MissingTenantContext_GuardsWithoutExec`（ctx 未確立時 tx 非開始・exec 非呼出でエラー返却） |

> 補助テスト（AC 非紐付け / Req 3.6・design 225 の写像根拠を固定）:
> `TestRegistrar_UpsertEnrolledDevice_ExecError_ReturnsTransientUnavailable`（exec 失敗→CodeUnavailable +
> IsTransient=true・Message に device 名非混入=NFR 3.1）/ `_NonTransientTxError_NormalizedToTransient`
> （BeginTx/Commit 由来の非 transient error も transient へ正規化）。
> **確認事項**: なし（design.md / tasks.md との矛盾は検出せず。spec 本文は未改変）。

### Task 5

- **採用方針**: `TenantResolver` を手本に consumer-defined port `EnrollmentRegistrar`（primitive 型）を
  notification package 内に宣言し、`EnrollmentNotificationHandler`（`NotificationHandler` 実装）で
  additionalData 突合 → 登録 / 未割当退避を振り分ける（enrollment を import しない / doc.go 遵守）。
- **重要な判断**:
  - `Handle` は payload parse → additionalData parse（tenant_id 欠落/parse 不能→退避 tenant_missing）→
    `db.FromContext`（未確立→退避 tenant_ctx_missing）→ `tenantMatches`（不一致→退避 tenant_mismatch）→
    一致時のみ `register` の順（design 手順 1〜4 と 1:1）。退避は `UnassignedQueue.Enqueue` を自ら呼び
    nil（ack）を返す採用案（Dispatcher 無改変）。関数を Handle/register/quarantine + 純粋 helper に分割し
    40 行以内・単一責務を維持。
  - `tenantMatches` は additionalData.tenant_id を `uuid.Parse` して ctx 由来 tenant と比較。parse 不能
    （非 uuid）も突合不能として安全側で退避（false→mismatch）。
  - **androidVersion パース不能フォールバックは `unknown`（登録継続）** を採用（design L269「それ以外は
    unknown」/ 判定不能で登録する場合の解釈と整合）。root cause: パース不能を `unsupported` に倒すと
    サポート端末を誤って対象外化しうるため、端末を登録して可視化する安全側（NFR 1.1）を選んだ。境界
    （9=unsupported / 10=unknown / 11.0.0=unknown / 空・非数値=unknown）を単体テストで固定。
  - amapi_device_name は payload `name`（`enterprises/.../devices/...`）を trim してそのまま bind
    （design「リソース名 → amapi_device_name」/ devices.amapi_device_name は text）。mode は
    additionalData.mode（fully_managed/dedicated の enum ラベル）を素通し。
  - WARN は message_id（`logger.MessageID`）+ enterprise_name / notification_type / quarantine_reason /
    failure_kind / compliance_status の**非機密 field のみ**。payload 生値・additionalData 生値・
    tenant_id 生値は補間しない（NFR 3.1 / NFR 4.1）。単体テストで機密マーカーの WARN 非混入を固定。
- **残存課題（task 6 への影響）**: 実 upsert の冪等性（同一 amapi_device_name の 2 回通知で devices
  1 件）・突合不一致の実退避・発行→通知→登録の動線は task 6 の実 PostgreSQL 結合テストが所有する。
  `NewEnrollmentHandler(enrollment.NewRegistrar(pool), NewUnassignedQueue(pool), log)` を in-test
  Dispatcher の handler map（`Enrollment` キー）へ登録する契約で公開済み。cmd/worker wire-in は #36。

#### AC Traceability（task 5 範囲: 3.1 / 3.2 / 3.3 / 3.5 / 3.6 / NFR 1.1 / NFR 2.1 / NFR 2.2 / NFR 4.1）

| Req ID | 担保テスト |
|---|---|
| 3.1 | `TestEnrollmentHandler_Handle_TenantMatch_RegistersDeviceWithoutQuarantine`（一致→Registrar 1 回・退避 0・device/mode/compliance 写像） |
| 3.2 / NFR 2.1 / NFR 2.2 | `_TenantMismatch_QuarantinesWithoutRegister`（不一致→退避のみ・Registrar 非呼出）/ `_MissingTenantContext_Quarantines`（ctx 未確立→退避） |
| 3.3 | `_TenantMissing_Quarantines`（tenant_id 欠落 / 空 / enrollmentTokenData 空 / additionalData JSON 不正 の 4 subcase→退避） |
| 3.5 / NFR 1.1 | `_AndroidVersion_MapsCompliance`（9/8.1.0→unsupported、10/11.0.0/空/非数値→unknown） |
| 3.6 | `_RegistrarTransientError_ReturnsNack`（transient 失敗→非 ack error / ShouldAck=false・退避 0） |
| NFR 4.1 | `_TenantMismatch_...`（quarantine_reason/enterprise_name の WARN 付与）/ `_WarnLogsDoNotLeakSecrets`（退避/サポート対象外/失敗の WARN に payload 生値・additionalData 生値・tenant_id 非混入 = NFR 3.1 も担保） |

> 補助テスト（防御ガード / AC は NFR 2.2 に紐付け）: `_MalformedPayload_Quarantines`（payload JSON 不正→安全側退避）。
> **確認事項**:
> - **context-map.md の discrepancy**: watcher 生成 context-map の Candidate files 欄に Mermaid ノード表記
>   `D[notification.Dispatcher]` が混入し不正確。実装対象は design.md File Structure Plan L100-104 の
>   `enrollment_handler.go`（新規）+ `enrollment_handler_test.go`（新規）であり、そちらを正本とした
>   （spec 本文は未改変）。
> - **androidVersion パース不能フォールバック**: 上記「重要な判断」の通り `unknown`（登録継続）を採用。
>   design L269 / requirements Req 3.5 / NFR 1.1 と矛盾しないことを確認済み（矛盾なし）。

### Task 6

- **採用方針**: notification_dispatch_test.go の setup 作法（migrate→truncate→app pool→SuperAdmin 検証）を
  踏襲しつつ、ENROLLMENT 種別に実ドメインハンドラ（`NewEnrollmentHandler` + `enrollment.NewRegistrar(pool)`）を
  登録した in-test Dispatcher を組み、発行→通知→登録の主動線を実 PostgreSQL で回帰検証する（cmd/worker 無変更 / design リスク 7）。
- **重要な判断**:
  - helpers_test.go の低レベルヘルパ（`requireDBURLs` / `applyMigrationsUp` / `truncateAll` / `newAppPool` /
    `connectTimeout` / `boundEnterpriseName`）を再利用。手本 `setupDispatch` は countingHandler mock を登録し
    pool を露出しないため、pool を露出し実 Registrar を注入する専用 `setupEnrollmentFlow` を新設した
    （helpers_test.go / notification_dispatch_test.go は無改変）。
  - additionalData は StubClient の `OnCreateEnrollmentToken` hook で発行時 `req.AdditionalData`（JSON 文字列）を
    **捕捉**し、そのまま通知 payload の `enrollmentTokenData` へ回送（`enrollment.AdditionalData.Marshal` と同一
    wire-format を保証 / design リスク 1）。不一致・無効ケースは `enrollment.AdditionalData{}.Marshal()` で直接組む。
  - 冪等検証は **MessageID を変え amapi_device_name を同一**にした 2 通知で実施（同一 MessageID だと 2 回目が
    dedupe fast-path で dispatch されず upsert 冪等性を検証できないため）。
  - devices / enrollment_tokens は tenant-scoped RLS のため、cross-tenant 検証クエリは SuperAdmin context
    （`platformdb.BeginTxFunc(saCtx, ...)`）で count/select する（手本の saCtx 検証作法と同型）。
  - IssueToken は tenant-scoped ctx（`WithTenantContext{TenantID, AdminUserID}`）で駆動し、`EnterpriseNameForTenant`
    の own-tenant ガードと enrollment_tokens / audit_logs の RLS INSERT を両立させる。`issued_by` / `actor_id` の FK を
    満たすため発行元テナント配下に admin_user を seed する。
- **残存課題（次 task に影響する事項）**: なし（task 6 が本 Issue 最終 task）。ENROLLMENT の本番稼働は #36 の
  worker 配線後（design リスク 7）。手本 `seedBoundTenant` の latent 回帰（下記確認事項）は Issue #39 の範囲。

#### AC Traceability（task 6 範囲: 6.1 / 6.2 / 6.3 / 3.4 / 4.2 / NFR 2.2）

| Req ID | 担保テスト |
|---|---|
| 6.1 / 3.1 | `TestEnrollmentFlow_IssueThenEnroll_RegistersDeviceForIssuingTenant`（IssueToken 発行 + snapshot 1 件 → 通知 Handle → devices 1 件 + mode=fully_managed / compliance=unknown） |
| 6.2 / 3.2 / NFR 2.2 | `TestEnrollmentFlow_TenantMismatchNotification_QuarantinedWithoutDeviceRegistration`（tenant_id 不一致 → unassigned 1 件退避 + 発行元 / 全 devices 0 件） |
| 6.3 / 3.4 | `TestEnrollmentFlow_DuplicateDeviceNotification_IdempotentSingleDevice`（同一 amapi_device_name の 2 通知で devices 件数 1 のまま） |
| 4.2 | `TestEnrollmentFlow_InvalidNotification_NoDeviceCreatedForTenant`（tenant_id 欠落 = 突合不能 → 当該テナントに端末未登録） |

> **確認事項**:
> - **手本 seedBoundTenant の latent 回帰（Issue #39 / #7 scope 外）**: notification_dispatch_test.go の
>   `seedBoundTenant` は `Insert`（pending_bind 生成）→ `UpdateBound` の順だが、#52 で `UpdateBound` の WHERE が
>   `status='binding'` に変更されたため pending_bind 行を bound 化できず（affected=0 を無視）、enterprise_name も
>   設定されない。実 DB で走らせると当該手本の bound 逆引き前提が崩れる（DATABASE_URL 未設定のため latent）。本 task は
>   影響を避けるため status='bound' + enterprise_name を 1 INSERT で確定する直接 seed を用いた。手本の修正は Issue #39 の
>   範囲であり spec 本文は未改変。
> - design / tasks / requirements 本文との矛盾は検出せず（本 task では未改変）。

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
