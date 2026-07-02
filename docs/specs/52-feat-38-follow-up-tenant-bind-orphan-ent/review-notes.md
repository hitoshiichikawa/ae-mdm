# Review Notes

<!-- idd-claude:review round=1 model=claude-opus-4.8 timestamp=2026-07-02T00:00:00Z -->

## Reviewed Scope

- Branch: claude/issue-52-impl-feat-38-follow-up-tenant-bind-orphan-ent
- HEAD commit: 8f9b9f73549520550182430ef0d37ab6d578e680
- Compared to: develop..HEAD
- 補足: `develop..HEAD`（two-dot）は develop 側の先行分（`notification/` モジュール・`39-` spec の
  削除表示）を含むため、実際のブランチ変更は `develop...HEAD`（three-dot / merge-base
  `dec415a`）で評価した。実変更は `internal/tenant/`・migration 0017・spec dir に限定され、
  notification 削除はレビュー対象外（develop の advance による表示上の差分）。
- 本レビューは full HEAD review（prompt に per-task range 指定なし）。Feature Flag Protocol は
  CLAUDE.md で `opt-out` のため flag 観点は適用せず、通常 3 カテゴリ判定のみ実施。

## Verified Requirements

- 1.1 — repository.go `ReserveBinding`（`UPDATE ... status='binding' WHERE status='pending_bind'`）/ service.go step 4 / service_test「ReserveBinding 勝者 + CreateEnterprise 成功 + UpdateBound(affected=1)…bound」/ integration `TestTenantRepository_ReserveBinding_ConcurrentWinnerAndLoser`
- 1.2 — service.go `affected==0 → 409`（CreateEnterprise 未呼出）/ service_test「ReserveBinding 敗者(affected=0) のとき 409…CreateEnterprise・UpdateBound を呼ばない（orphan 防止の核）」/ integration (1)
- 1.3 — service.go `StatusBinding/StatusBound → ErrConflict` / service_test「binding 状態への新規 bind…409…ReserveBinding を呼ばない」「bound 状態への再 bind…409」
- 1.4 — repository.go `UpdateBound` WHERE を `status='binding'` へ変更 / service.go step 6 / integration「UpdateBound WHERE status='binding'（pending_bind 行は affected=0）」
- 1.5 — service.go `releaseBindingBestEffort` + repository.go `ReleaseBinding` / service_test「CreateEnterprise が失敗したとき ReleaseBinding を呼び UpdateBound 未呼出」「空 enterprise_name…ReleaseBinding」/ integration `TestTenantRepository_ReleaseBinding_AffectedAndIdempotent`
- 1.6 — service.go `Disable` の前提状態 switch に `StatusBinding` 追加 / service_test「binding テナントの確認一致のとき disabled へ遷移できる」/ integration「binding↔disable（UpdateDisabled affected=1 後 UpdateBound affected=0）」
- 1.7 — service.go 各経路（予約失敗 / 作成失敗 / 確定成功）で `Record(bind, …)` / service_test で各ケースの Record 発火を検証
- 2.1 — repository.go `RecoverStaleBindings`（sweep）+ service.go ユースケース + handler.go `POST /tenants/recover-bindings` / service_test『TestService_RecoverStaleBindings』/ handler_test『TestRecoverBindings_ReturnsRecoveredCount』/ integration `TestTenantRepository_RecoverStaleBindings_RecoversOnlyStale`
- 2.2 — service_test「回収後の再 bind が新たな ReserveBinding を通り二重作成しない（CreateEnterprise 1 回のみ）」
- 2.3 — service.go `EnterpriseNameForTenant` の `StatusBinding → ErrNotBound` / service_test「binding のとき CodeBusinessRule（未バインド扱い）」
- 2.4 — types.go `OperationRecover` + service.go `Record(recover, success)` + 構造化ログ / service_test で actor/tenant_id/operation/result を検証 / types_test『TestOperationValues』
- 3.1 — migration 0017 `signup_url_name` 列 + types.go `SignupURLName` + repository.go Insert/Get/List 配線 + service.go Create 順序変更（CreateSignupURL→Insert）/ service_test「成功時…signup_url_name が Insert 行に永続化」/ repository_test『TestNullableString_*』/ integration「signup_url_name 永続化往復」
- 3.2 — service.go `row.SignupURLName` を正本に CreateEnterprise へ渡す + types.go `BindInput` 空 struct 化 + handler.go `createResponse` から signup_url_name 除去 / service_test「永続値が CreateEnterprise 引数に渡り body 値は無視」/ handler_test「create 応答に signup_url_name が含まれない」
- 3.3 — handler.go bind が `decodeJSONAllowEmpty` で body の signup_url_name を無視 / handler_test「body の別テナント signup_url_name を無視」「空 body でも永続値経路で bind」
- 3.4 — service.go 永続 signup_url_name 空で fail-closed 422（ReserveBinding/CreateEnterprise 未呼出）/ service_test「永続 signup_url_name が空白 trim 後に空のとき 422…呼ばない」
- 4.1 — migration 0017 `ALTER TYPE ... ADD VALUE 'binding'` + types.go `StatusBinding`/`Valid()`/`ParseStatus` 4 値化 / types_test『TestStatusValid』『TestParseStatus』/ 既存 migrations_reversible_test（0017 込み）
- 4.2 — service.go disabled→422 / 定義外 status→fail-closed 422 / service_test「disabled 状態への bind…422」「定義外 status…422」
- 4.3 — service.go CreateEnterprise 失敗時 ReleaseBinding で binding を残さず UpdateBound 未呼出 / service_test で検証
- 4.4 — types.go `ViewFromRow`（binding を status に載せ enterprise_name 非露出）/ types_test「binding 行は status を binding として載せ enterprise_name を露出しない」
- NFR 1.1 / 1.2 — Status 4 値保持 / enterprise_name は bound のみ非空（types.go doc + ViewFromRow + integration 往復）
- NFR 2.1 / 2.2 — 条件付き UPDATE（ReserveBinding/UpdateBound/UpdateDisabled）で lost update 防止 / CreateEnterprise を tx 外に維持（design シーケンス踏襲）
- NFR 3.1 / 3.2 — 回収時の `log.Info`（tenant id のみ）+ Record(recover) / Event・ログに signup_url_name 等の機密生値を載せない

## Findings

なし（approve）。

補足（reject 対象ではない観察事項）:
- `backend/test/integration/tenant_admin_guard_test.go`（9 行）は全 task の `_Boundary:_` にも
  design File Structure Plan にも列挙されていないが、変更内容は新規公開 IF
  `Service.RecoverStaleBindings`（Req 2.1 / task 6.1 で authorized）追加に伴う
  `fakeTenantService` の interface 充足スタブ（0 件・nil を返す no-op）に限定される。build 必須の
  fixture 追従であり scope creep でも挙動変更でもないため boundary 逸脱としない。
- `service_test.go` の Create テスト assertion 更新（URL 失敗時「Insert 呼出」→「Insert 未呼出」）
  および `tenant_repository_test.go` の既存 UpdateBound テスト ARRANGE への `ReserveBinding` 追加は、
  design 確定済み挙動（`UpdateBound WHERE binding` / create 順序変更 = 確認事項 4）への追従であり、
  assert は緩めていない（テスト弱体化に該当しない）。
- impl-notes.md「確認事項」記載の post-marker fixture commit（`efcabae`）は per-task ループの
  marker 位置 mechanics の論点であり、本 full HEAD review（develop..HEAD 全体）では全 commit が
  レビュー範囲内。コード自体は build/vet/`go test ./internal/tenant/...` green で検証済み。

## Summary

全 numeric AC（Req 1.1〜4.4 / NFR 1.1〜3.2）が実装とテストの両面でカバーされ、変更は宣言済み
task 境界（migration 0017 / types / repository / service / handler + 各テスト + integration）に
収まっている。`go build ./...` / `go vet ./internal/tenant/...` / `go test ./internal/tenant/...`
は green。境界外 1 ファイルは authorized interface 追加に伴う build 必須の fixture スタブのみ。

RESULT: approve
