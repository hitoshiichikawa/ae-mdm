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

STATUS: complete
