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

STATUS: complete
