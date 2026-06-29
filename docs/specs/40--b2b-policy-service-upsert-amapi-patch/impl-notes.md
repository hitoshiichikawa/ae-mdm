# Implementation Notes — Issue #40 Policy Service

本ファイルは per-task ループ下の各 task 完了 learning を `## Implementation Notes` 配下に追記する。

## AC Traceability（本 task=1 が担保した AC）

| Requirement | 担保テスト |
|-------------|-----------|
| 4.5（拒否時に存在差を露出しない汎用 message） | `service_types_test.go` `TestPolicySentinelErrors` / `TestErrPolicyNotFound_NoExistenceLeak` |
| 2.3（必須項目不正→不正項目提示 / 型不整合→invalid field） | `mapper_test.go` `TestRawToPolicyInput_TypeMismatch` / `TestRawToPolicyInput_NoRawValueLeak` |
| 2.1（反映/保存前の検証ゲート前提となる Raw→PolicyInput 変換） | `mapper_test.go` `TestRawToPolicyInput_FullMapping` / `TestRawToPolicyInput_PartialMapping` / `TestRawToPolicyInput_EmptyBody` |
| NFR 1.1（追加フィールドの pass-through 透過） | `mapper_test.go` `TestBuildPolicyBody_PassThrough` |

> Req 2.x / 4.x の Service / Repository / Handler 側の挙動（検証ゲート発火・RLS 分離・HTTP 写像）は
> 後続 task 2〜5 の責務。本 task は型定義と変換層の単体保証に限定する。

## AC Traceability（本 task=2 が担保した AC / repository_test.go）

| Requirement | 担保テスト |
|-------------|-----------|
| 1.3 / 1.5（snapshot 永続化の列型付き Insert/Update） | `TestScanPolicyRow_AllColumns` / `TestNullableUpdatedBy`（Insert/Update の引数化 + scan 往復） |
| 3.2 / 4.2（他テナント policy 割当→FK 違反を NotFound 写像） | `TestMapAssignError_FKViolation` |
| 3.3（他テナント device→affected=0 を Service が NotFound 判定する戻り値） | `TestMapAssignError_Nil`（err==nil で (0,nil) 経路を担保） |
| 4.4 / 4.5（0 行→存在差非露出の NotFound） | `TestMapGetError_NoRows` / `TestMapGetError_OtherDBError` |
| 5.3（割当済み端末ありの削除→FK 違反を 409 Conflict 写像） | `TestMapDeleteError_FKViolation` / `TestMapDeleteError_OtherDBError` / `TestMapDeleteError_Nil` |
| NFR 2.1（列ごと型付き scan / body jsonb→map / updated_by NULL→nil） | `TestScanPolicyRow_AllColumns` / `TestScanPolicyRow_NullUpdatedBy` / `TestScanPolicyRow_ScanError` |
| 3.1（割当の applied_policy_id UPDATE 経路） | `AssignPolicyToDevice` 実装 + `TestMapAssignError_*`（DB tx 自体は db パッケージ fake pool で別途担保） |

> Req 4.3（他テナント device の割当先指定拒否）は affected=0 経路（`TestMapAssignError_Nil` が戻り値契約を担保）と
> 複合 FK 経路（`TestMapAssignError_FKViolation`）の双方で Service が NotFound 写像できる戻り値を保証する。

## AC Traceability（本 task=5 が担保した AC / handler_test.go）

| Requirement | 担保テスト |
|-------------|-----------|
| 4.1（RBAC: deny は 403 / 構造化 WARN） | `TestHandler_Create_Viewer_Returns403`（Viewer POST→403 + deny_reason WARN）/ `TestHandler_Create_TenantAdmin_Returns200`（TenantAdmin POST→200） |
| 4.5（存在差非露出の NotFound / 汎用 message） | `TestHandler_Get_NotFound_Returns404`（不在 GET→404 + body に id 非露出）/ `TestHandler_Assign_NotFound_Returns404` |
| 2.2（3000 件超→422 + 上限超過提示） | `TestHandler_Create_BusinessRuleViolation_Returns422`（business rule のみ→422 + details 全件） |
| 2.3（必須項目不正→400 + 不正項目提示） | `TestHandler_Create_InvalidField_Returns400WithAllDetails`（invalid field 混在→400 + 全件 details） |
| NFR 3.1（拒否操作の原因属性を構造化ログ） | `TestHandler_Create_Viewer_Returns403`（deny_reason=authz denied）/ `TestHandler_List_NoClaims_Returns401`（deny_reason=missing auth claims） |

> 補助カバレッジ（task 5 内の周辺 HTTP 写像保証 / 上記 AC の派生）: malformed JSON→400
> （`TestHandler_Create_MalformedJSON_Returns400`）/ 不正 path id→400（`TestHandler_Get_InvalidID_Returns400`）/
> DELETE 204・409（`TestHandler_Delete_*`）/ assign 204（`TestHandler_Assign_TenantAdmin_Returns204`）/
> 自テナント一覧 200（`TestHandler_List_TenantAdmin_Returns200OwnTenant` / Req 4.4 の HTTP 経路）/
> AMAPI 上流 502（`TestHandler_Create_UpstreamError_Returns502`）。

## Implementation Notes

### Task 1

- 採用方針: application 層 DTO・sentinel error を `service_types.go` に、Raw JSON↔PolicyInput 変換を `mapper.go` に新規追加。既存 Validator (#36) の `types.go` / `validator.go` は不変。
- 重要な判断:
  - 変換契約（design 確認事項 4 推奨案）に従い、型不整合・必須キー欠落は invalid field 相当の `ValidationError{Kind: KindInvalidField}` に写像し、Service が `Validate` と同じ経路で 400 へ倒せるようにした。`ValidationError.Message` に raw 生値（機密値）を載せない（Req 5.4 / NFR 3.2 を先取り）。
  - AMAPI 本体 JSON のフィールドパス対応は、各領域の抽出元キーが存在する場合のみ `PolicyInput` の対応ポインタを設定し、欠落領域は nil のまま（Validator の「nil = 検証 skip / 部分更新許容」契約に委ねる）とした。
  - `BuildPolicyBody` は raw body を strongly-typed 化せず `amapi.PolicyBody.Raw` へ pass-through。zero value / 追加フィールド（minimumApiLevel 等）は AMAPI Client (#34) の ForceSendFields 機構に委ねる（NFR 1.1）。
  - `PolicyRow.Version` は AMAPI 払い出しの snapshot バージョンと型を揃えるため `int64`（DB 列は integer だが Repository 側 task 2.1 で scan 整合を取る）。`UpdatedBy` は nullable 列のため `*uuid.UUID`。
- 残存課題（次 task=2 以降に影響する事項）:
  - **確認事項（変換層のフィールドパス）**: AMAPI Policy 本体での password / kiosk のフィールドパスは設計上「passwordMinimumLength / encryptionPolicy・passwordQuality / kiosk」と概略指定に留まる。本実装は AMAPI スキーマに沿って (a) password は `passwordPolicies[0].passwordMinimumLength` / `passwordPolicies[0].passwordQuality`、(b) encryptionPolicy は top-level 文字列、(c) Kiosk パッケージ名は `applications[]` のうち `installType=="KIOSK"` の `packageName` 集合、と解釈した。`passwordPolicies` の複数要素・別経路（`passwordRequirements` 等）は MVP 範囲外として先頭要素のみ扱う。Service / Handler 実装時にこの抽出経路で UI 入力契約と齟齬が無いか PR レビューで確認されたい（推測で確定せず記録に留める）。
  - task 2.1（Repository）は `PolicyRow` の列順（id, tenant_id, name, amapi_policy_name, body jsonb, version, updated_by, created_at, updated_at / migration 0005）に合わせて列型付き scan する。`body jsonb` → `map[string]any`、`updated_by` NULL → nil 写像が前提。
  - task 3.1（Service.Create/Update）は `RawToPolicyInput` の `[]ValidationError`（変換不能）と `policy.Validate` の `ValidationResult.Errors`（検証不正）を統合して全件提示する（Req 2.4）。両者とも `Kind.Code()` で 400/422 へ写像する。

### Task 2

- 採用方針: `repository.go`（policies CRUD + devices.applied_policy_id UPDATE）を `db.BeginTxFunc(ctx, pool, ...)` + raw pgx で実装。tenant-scoped context のまま RLS に分離を委ね SuperAdmin 昇格しない（tenant/auth.Repository とは対照的）。テスト容易性は audit.Repository の確立パターン（純粋 helper の切り出し + `rowScanner` interface）に倣う。
- 重要な判断:
  - **FK violation の Code 写像の使い分け**: `Delete` の FK 違反（pgerrcode 23503 = 割当済み端末あり）は `ErrDeleteConflict`（409）へ、`AssignPolicyToDevice` の複合 FK 違反（他テナント policy 指定）は `ErrPolicyNotFound`（404 / 存在差非露出）へ写像。同じ 23503 でも操作意味が異なるため `mapDeleteError` / `mapAssignError` を分離した（design 確認事項 3 / Req 3.2・4.2・5.3）。
  - **Assign の affected=0 vs FK violation の使い分け**: 他テナント device 指定は `WHERE id AND tenant_id` の affected=0 → `(0, nil)` で返し Service が NotFound 判定（Req 3.3）。他テナント policy 指定は複合 FK 違反 → Repository 内で `ErrPolicyNotFound` 写像（Req 4.2）。前者は error にせず後者は error にする、という設計 interface の戻り値契約に厳密に従った。
  - **sentinel の errors.Is 到達性**: `*errors.Error` の Cause は単一だが、`service_types.go` 契約が「Service が `errors.Is(err, ErrPolicyNotFound/ErrDeleteConflict)` で写像」を要求するため、`wrapSentinel` helper で `fmt.Errorf("%w: %w", sentinel, dbErr)`（Go 1.22 多重 wrap）を Cause に据え、sentinel と DB cause の双方を `errors.Is` で辿れるようにした。
  - **scan の列型束縛**（NFR 2.1）: `scanPolicyRow` で `body jsonb`→`*map[string]any`、`updated_by`→`**uuid.UUID`（NULL→nil）を列ごとに明示束縛。`rowScanner` interface 経由で `fakeRow` を注入し実 DB 非依存で単体テストした。
  - DB tx orchestration（BeginTxFunc の panic ガード / rollback / commit）は `internal/platform/db` の fake pool テストで既に担保済みのため、Repository 側は純粋 helper（scan / 3 種 error 写像 / nullable bind）の単体テストに集中した。
- 残存課題（次 task=3 Service に影響する事項）:
  - **Repository 戻り値契約 → Service 写像**: Service は (a) `Get` の `ErrPolicyNotFound`（既に Code 付き）をそのまま伝達、(b) `Update`/`Delete` の `affected==0` を `ErrPolicyNotFound`(404) へ、(c) `AssignPolicyToDevice` の `affected==0` を `ErrPolicyNotFound`(404) へ Service 側で写像する責務を負う。`Delete` の FK 違反（409）/ `Assign` の FK 違反（404）は Repository が既に Code 付き `*errors.Error` で返すため Service はそのまま伝達してよい。
  - `Insert`/`Update` は `body` を `map[string]any` のまま bind（pgx が jsonb へ encode）。Service は AMAPI 反映成功後の raw body snapshot をそのまま `PolicyRow.Body` に詰めて渡す前提。
  - `version` は DB 列が integer だが `PolicyRow.Version int64` で往復。AMAPI 払い出し version をそのまま使う想定で、Repository 側で範囲チェックはしない（Service / AMAPI が値の妥当性を担保）。

### Task 3

- 採用方針: `service.go`（upsert ユースケース Create / Update）+ `service_test.go` を新規追加。順序は「mapper 変換 + Validate 結合の検証ゲート（早期 return）→ enterprise_name 解決 → AMAPI UpsertPolicy → 成功後 snapshot 永続化 → 監査記録」で固定し、検証失敗・AMAPI 失敗時は snapshot を確定保存しない（Req 1.4 / 2.5）。tenant.Service を手本に `logDeny` / `record` helper と nil-log フォールバックを踏襲。
- 重要な判断:
  - **インクリメンタル interface（設計どおり）**: `Service` interface は本 task の `Create` / `Update` の 2 メソッドのみ宣言・実装し、`var _ Service = (*service)(nil)` を 2 メソッドで成立させた。design.md の 6 メソッド版は最終形であり、Get/List/Delete/Assign は task 4.1 が同 interface へ追加する。これにより policy package が本 task 単独で build / test 可能。
  - **検証エラー全件搬送（`ValidationFailedError`）**: `errors.Error` に details が無いため、policy 固有の `ValidationFailedError{Errors []ValidationError}` を service.go に定義。mapper の変換不能 `[]ValidationError` と `Validate` の `ValidationResult.Errors` を結合して全件保持（Req 2.4）。top-level Code 規則は「KindInvalidField が 1 件でもあれば 400 / 全件 KindBusinessRule なら 422」とし、`Unwrap` で top-level Code 付き `*errors.Error` を公開して `errors.As` / `WriteHTTP` が HTTP status を解決できるようにした（Handler=task 5 が型 assertion で details を展開する想定）。Message に raw body 生値は載せない（Req 5.4）。
  - **consumer-defines-interface の最小 port**: Service deps は `policy.Repository` / `upsertClient`（UpsertPolicy 1 本）/ `eventRecorder`（Record 1 本 / audit.Service が満たす）/ `enterpriseResolver`（EnterpriseNameForTenant 1 本 / tenant.Service が満たす）/ `logger.Logger` のみ。**authorizer は Service に持たせない**（authz は Handler=task 5 の責務 / design Components）。
  - **AMAPI policyName**: Create は `uuid.New()` を採番し短い policyId（`id.String()`）を `UpsertPolicy` へ渡し、`PolicyRow.AMAPIPolicyName` には full path（`enterpriseName + "/policies/" + id`）を保存（enterpriseName を二重連結しない）。Update は既存行を `Repository.Get` で取得後、既存 `AMAPIPolicyName` の末尾 policyId を再利用して反映する。
- 残存課題 / 確認事項（PR レビューで人間判断を仰ぐ。spec は書き換えていない）:
  - **version の供給元が未明示**: `amapi.Client.UpsertPolicy` は version を返さず、tasks.md 3.1 の順序にも `GetPolicy` は含まれない。MVP の合理的既定として **Create=初期値 0 / Update=既存行 version 据え置き** を採った。AMAPI 払い出し version の同期が必要なら別 task（Get/List 実装時の `GetPolicy` 反映等）で扱うべきで、PR レビューで供給元方針を確認されたい。
  - **tasks.md 6.1 の authorizer 齟齬**: tasks.md 6.1 の配線記述は `NewService(repo, amapiClient, auditSvc, authorizer, tenantSvc, log)` と authorizer を含むが、design.md は authz を Handler に置く（Service は持たない）。本実装は design.md に従い `NewService(repo, client, recorder, tenants, log)` とした。task 6.1 実装時に配線シグネチャの齟齬を解消する必要があるため記録に留める（tasks.md は書き換えない）。
  - **インクリメンタル interface**: 上記のとおり本 task では Service を 2 メソッドで確定。task 4.1 が同 interface に 4 メソッドを追加し最終形（design.md 6 メソッド）へ拡張する前提。

### Task 4

- 採用方針: `service.go` の `Service` interface に `Get` / `List` / `Delete` / `Assign` の 4 メソッドを追加し、design.md の最終形（6 メソッド）へ拡張。Repository（`Get` / `List` / `Delete` / `AssignPolicyToDevice`）は task 2 で実装済みのため **呼び出すのみ**。`service_test.go` に各メソッドの正常系・異常系・境界値テストを追記（既存テストは不変）。
- 重要な判断:
  - **Repository 戻り値 → Service 写像の使い分け（impl-notes task 2 残存課題に厳密準拠）**: `Get` の `ErrPolicyNotFound`（Code 付き 404）は **そのまま伝達**。`Delete` / `Assign` の `affected==0` は Service 側で `ErrPolicyNotFound`（404）へ写像。`Delete` の `ErrDeleteConflict`（409）/ `Assign` の他テナント policy 複合 FK 違反（Repository が `ErrPolicyNotFound` 404 へ写像済み）は **そのまま伝達**。これにより他テナント device（affected=0）と他テナント policy（FK 違反）の双方が存在差非露出の NotFound に倒れる（Req 3.2 / 3.3 / 4.2 / 4.3 / 4.5）。
  - **read 系は監査なし**: `Get` / `List` は監査 `record` を呼ばない（Req 5.x は作成・更新・削除・割当のみが対象 / Req 4.4 は read のみ）。変更系（`Delete` / `Assign`）は成否いずれの経路でも監査する（既存 Create/Update と同方針 / Req 5.3）。
  - **新規 EventType + record helper の汎用化**: `eventTypePolicyDelete` / `eventTypePolicyAssign` を追加（design Data Models の命名に一致）。既存 `record` を name 空文字時に name field を省略する形へ小改修（Create/Update は常に name 非空のため挙動不変）。割当は device_id を Detail に載せる必要があるため `recordAssign` を追加し、共通の `emitAudit` helper へ集約。監査 Detail / logDeny に raw body 機密値を載せない（Req 5.4 / NFR 3.1 / 3.2）。
  - **authorizer 非保持・AMAPI device patch なし**: Service は authorizer を持たず actor / tenantID を引数で受ける（authz は Handler=task 5 の責務 / design Components）。`Assign` は `devices.applied_policy_id` の DB 更新までで AMAPI device patch は行わない（design 確認事項 1 推奨案 / テストでも AMAPI 非呼び出しを担保）。
  - **row→view 写像**: `Get` 用に `rowToView`（CreatedAt / UpdatedAt / Version を row から充填）/ `List` 用に `rowToSummary`（Body を載せず軽量化 / Req 5.4）を追加。`var _ Service = (*service)(nil)` の compile-time チェックが 6 メソッドで成立。
- 残存課題（次 task=5 Handler に影響する事項）:
  - Handler（task 5）が `ValidationFailedError` の details 展開 / `authz.AuthorizeAndLog`（RBAC）/ `errors.WriteHTTP`（存在差非露出の HTTP 写像）/ JSON decode / path param parse / actor・tenantID の claims 取得を担う（Service は `httpserver` を import しない）。
  - **tasks.md 6.1 の NewService 配線シグネチャ齟齬は task 3 で既出**: tasks.md 6.1 は `NewService(repo, amapiClient, auditSvc, authorizer, tenantSvc, log)` と authorizer を含むが、design.md は authz を Handler に置く（本実装の `NewService(repo, client, recorder, tenants, log)` には authorizer 無し）。task 6.1 実装時に配線シグネチャの齟齬を解消する必要がある（spec は書き換えていない）。
  - **確認事項**: 割当 endpoint の所有を Policy / Device どちらに置くか（design 確認事項 1）は本 task では DB 更新までに限定する暫定実装。Device Service 実装時の移設可否は PR レビューで人間判断を仰ぐ（spec は書き換えていない）。

### Task 5

- 採用方針: `handler.go`（/api/policies 6 endpoint + RBAC + HTTP 写像）+ `handler_test.go` を新規追加。`audit.Handler` を手本に内包 `chi.Router` + `ServeHTTP` を持たせ、`routers.API.Mount("/policies", h)`（tasks.md 6.1 の配線）で chi.Mount 互換に稼働する形にした。actor / tenantID は `httpserver.AuthClaimsFromContext` で取得し Service へ引数で渡す（`httpserver` import は Handler のみ / design Components）。
- 重要な判断:
  - **Mount 方式（audit 型を採用）**: tasks.md 5.1 は `Mount(r chi.Router)`（tenant.Handler 型）、6.1 は `routers.API.Mount("/policies", policyHandler)`（chi.Mount = audit.Handler 型）と記述が割れている。両者を同時に満たすため、tasks.md 5.1 が「`audit/handler.go` が手本」と明記している点を優先し audit 型（内包 router + ServeHTTP + root 相対登録）を採った。これにより 6.1 の `Mount("/policies", h)` がそのまま成立する。下記「確認事項」に記録。
  - **RBAC 軸の対応付け**: GET/List/Get=ActionRead、POST=ActionCreate、PUT=ActionUpdate、DELETE=ActionDelete、`PUT {id}/assign`=ActionUpdate（policy 行ではなく devices.applied_policy_id を更新する変更操作のため policy:update 権限と同一視 / permissionMatrix に assign 専用 Action は無い）。`TargetTenantID` は `claims.TenantID`（own-tenant）を渡し RBAC を自テナント境界に閉じる。
  - **検証エラー全件提示の HTTP 展開（Req 2.2/2.3/2.4）**: `ValidationFailedError` を `errors.As` で捕捉し、top-level Code（KindInvalidField 混在→400 / 全件 BusinessRule→422）に応じた status を `EffectiveHTTPStatus` で決定。`details[]`（domain/field/kind/message のみ）を JSON body に載せ、raw body 生値は載せない（Req 5.4 / NFR 3.2）。それ以外の `*errors.Error`（NotFound 404 / Conflict 409 / Upstream 502）は `errors.WriteHTTP` 一任。
  - **拒否経路の二重構造化ログ**: deny は platform 層 `AuthorizeAndLog`（authz_deny_reason）に加え、policy ドメインの `logDeny`（deny_reason / action / path / method）を出す（audit.Handler の failure_kind と同じく識別軸を揃える / NFR 3.1）。claims 不在は 401 + `missing auth claims` の WARN。機密値（raw body）はログに補間しない。
  - `decodeJSON` / `parseID` / `writeJSON` は tenant.Handler 同方式（単一 JSON document のみ許容 / 後続トークンも 400）を policy package 内に複製（package 間で共有 helper を持たない既存慣習に従う）。
- 残存課題 / 確認事項（次 task=6 DI 配線に影響 / spec は書き換えていない）:
  - **Mount 方式の記述揺れ（task 6 配線で要確認）**: 上記のとおり tasks.md 5.1（`Mount(r chi.Router)`）と 6.1（`routers.API.Mount("/policies", policyHandler)`）が割れている。本実装は audit 型（`Mount("/policies", h)`）を採ったため、task 6.1 では `routers.API.Mount("/policies", policyHandler)` で配線でき齟齬は解消する。tenant 型の `h.Mount(routers.API)` を期待していた場合は配線記述が一致しないため、PR レビューで配線方式を確認されたい。
  - **NewService 配線シグネチャ齟齬は task 3/4 で既出のまま**: tasks.md 6.1 は `NewService(repo, amapiClient, auditSvc, authorizer, tenantSvc, log)` と authorizer を含むが、design.md / 実装は authz を Handler に置く（`NewService(repo, client, recorder, tenants, log)` に authorizer 無し / `NewHandler(svc, authorizer, log)` に authorizer を渡す）。task 6.1 実装時に Service ではなく Handler へ authorizer を渡す配線へ修正する必要がある（spec は書き換えていない）。
  - `NewHandler(svc Service, authorizer *authz.Authorizer, log logger.Logger)` のシグネチャで構築する。main.go は既存の `authz.New()` 相当の authorizer インスタンス（または audit.Handler に渡している authorizer）を再利用すること。

STATUS: complete
