# Implementation Plan

- [x] 1. migration 0017（tenant_status enum 4 値化 + signup_url_name 列）
- [x] 1.1 0017 up/down SQL を追加する
  - `0017_tenant_binding_state_and_signup_url.up.sql`: `ALTER TYPE tenant_status ADD VALUE IF NOT EXISTS 'binding'` と `ALTER TABLE tenants ADD COLUMN IF NOT EXISTS signup_url_name text` を冪等に記述（0016 と同じ ALTER のみパターン）
  - `0017_*.down.sql`: `signup_url_name` 列を `DROP COLUMN IF EXISTS` で巻き戻し。enum 値 `binding` は PostgreSQL で直接削除できないため no-op を SQL コメントで明示（0001 down の `DROP TYPE` が全 down シーケンスで enum 型ごと削除する旨を記述）
  - 既存 `migrations_reversible_test`（全 down→up + ErrNoChange）が pass することをローカル DB で確認する（テスト追加ではなく既存テストの非破壊検証）
  - _Requirements: 4.1, 3.1_
  - _Boundary: migration 0017_

- [x] 2. types.go の 4 値化と signup_url_name フィールド追加
- [x] 2.1 Status enum 4 値化・DTO 拡張・sentinel 補強
  - `StatusBinding Status = "binding"` を追加し、`Valid()` / `ParseStatus` を 4 値対応にする（NFR 1.1）
  - `TenantRow` / `CreateInput` に `SignupURLName string` を追加（Repository は NULL を空文字へ写像する既存 enterprise_name パターン踏襲）
  - `Operation` enum に `OperationRecover Operation = "recover"` を追加（Req 2.4）
  - `BindInput` の `SignupURLName` フィールドを削除（永続値を正本に使うため body から除去 / Req 3.2・3.3）
  - 上記変更の単体テスト追加: `Valid()`/`ParseStatus` が binding を受理し未定義値を拒否する / `ViewFromRow` が binding を status に載せ enterprise_name を露出しない（Req 4.4）
  - _Requirements: 4.1, 4.4, 2.4, 3.2_
  - _Boundary: types.go_
  - _Depends: 1.1_

- [x] 3. repository.go の予約・解放・回収メソッドと signup_url_name 永続化
- [x] 3.1 ReserveBinding / ReleaseBinding / UpdateBound(WHERE 変更) / Insert 永続化
  - `ReserveBinding`（`UPDATE ... status='binding' WHERE id=$ AND status='pending_bind'`、affected 返却 / Req 1.1）と `ReleaseBinding`（`status='pending_bind' WHERE id=$ AND status='binding'` / Req 1.5）を追加（既存 `superAdminContext`+`BeginTxFunc`+affected rows パターン踏襲）
  - `UpdateBound` の WHERE を `status='pending_bind'` から `status='binding'` へ変更（Req 1.4）。uq_tenants_enterprise_name の 23505→CodeConflict 写像は維持
  - `Insert` / `Get` / `List` の SQL に `signup_url_name` を組み込む（Insert は NULL 写像、scan は NULL→空文字 / Req 3.1）
  - in-package 単体テストは DB 非依存ロジック（NULL 写像等）に留め、affected rows 実挙動の検証は task 7.1 の integration test へ deferred する（partial 明示。既存 #38 の「repository の実 SQL 挙動は integration、service/handler は fake で in-package」方針に整合）
  - _Requirements: 1.1, 1.4, 1.5, 3.1_
  - _Requirements_partial: 1.1, 1.4, 1.5_
  - _Boundary: repository.go_
  - _Depends: 2.1_
- [x] 3.2 RecoverStaleBindings sweep クエリ (P)
  - `RecoverStaleBindings(ctx, olderThan)`（`UPDATE ... status='pending_bind' WHERE status='binding' AND updated_at < now()-$olderThan RETURNING id`、回収 id 群返却 / Req 2.1）を追加
  - sweep の RLS 下挙動・しきい値境界の検証は task 7.1 の integration test へ deferred する（partial 明示）
  - _Requirements: 2.1_
  - _Requirements_partial: 2.1_
  - _Boundary: repository.go_
  - _Depends: 2.1_

- [x] 4. service.go の Bind 2 段確定フロー（orphan 防止の核）
- [x] 4.1 Bind を予約状態経由の 2 段確定へ変更
  - `Bind` を新フローへ変更: Get→（signup_url_name 永続値検証、空なら fail-closed 422 / Req 3.4）→`ReserveBinding`（affected=0 は 409 で CreateEnterprise 未呼出 / Req 1.2・1.3）→勝者のみ `CreateEnterprise`（**永続 signup_url_name** を渡す / Req 3.2）→成功 `UpdateBound`（Req 1.4）/ 失敗 `ReleaseBinding`（Req 1.5・4.3）
  - 現状態 binding/bound への新規 bind を 409、disabled を 422 で拒否（Req 1.3 / 4.2）
  - 各経路で `Record`（予約失敗 / 作成失敗 / 確定成功 / Req 1.7）と拒否ログを発火
  - 同タスク内に単体テスト追加（failure path / safety fallback のため同 task 内必須）: 勝者が CreateEnterprise を 1 回呼び bound 確定 / 敗者(ReserveBinding affected=0)が `CallCount("CreateEnterprise")==0` で 409（Req 1.2 orphan 防止の核）/ CreateEnterprise 失敗で ReleaseBinding 呼出 + pending_bind 維持 + UpdateBound 未呼出（Req 1.5/4.3）/ 永続 signup_url_name 空で 422 かつ ReserveBinding・CreateEnterprise 未呼出（Req 3.4）/ 永続値が CreateEnterprise 引数に渡る（Req 3.2）
  - _Requirements: 1.1, 1.2, 1.3, 1.4, 1.5, 1.7, 3.2, 3.4, 4.2, 4.3_
  - _Boundary: service.go_
  - _Depends: 3.1_

- [x] 5. service.go の create 順序変更・回収・状態整合
- [x] 5.1 Create 順序変更と signup_url_name 永続化
  - `Create` を「CreateSignupURL → Insert(pending_bind, signup_url_name)」順へ変更し signup_url_name を永続化（Req 3.1）。URL 生成失敗時は Insert しない（確認事項 4 の解釈に基づく）
  - 同タスク内に単体テスト追加: 成功時 signup_url_name が Insert 引数に渡る / CreateSignupURL 失敗時に Insert 未呼出（Req 3.1）
  - _Requirements: 3.1_
  - _Boundary: service.go_
  - _Depends: 4.1_
- [x] 5.2 RecoverStaleBindings ユースケースと EnterpriseNameForTenant の binding 整合
  - `RecoverStaleBindings(ctx, actor, olderThan)` を追加: Repository.RecoverStaleBindings を呼び、回収各行を `Record(recover)` + 構造化ログ（Req 2.1・2.4 / NFR 3.1）。回収後の再 bind は新 signup_url から再予約できる（二重作成しない / Req 2.2）
  - `EnterpriseNameForTenant` の status 分岐に `binding` を追加（未バインド扱いで 422 / Req 2.3・4.4）
  - 同タスク内に単体テスト追加（safety fallback のため同 task 内必須）: binding 行が回収され Record(recover) 発火（Req 2.1/2.4）/ EnterpriseNameForTenant が binding を 422 で拒否（Req 2.3/4.4）/ 回収→再 bind が新たな ReserveBinding を通る（Req 2.2）
  - _Requirements: 2.1, 2.2, 2.3, 2.4, 4.4_
  - _Boundary: service.go_
  - _Depends: 5.1_
- [x] 5.3 Disable の binding 整合（binding↔disable 競合制御）
  - `Disable` の前提状態判定に `binding` を無効化可能として追加（pending_bind/bound/binding が無効化可、disabled のみ二重無効化拒否 / Req 1.6）。UpdateDisabled の `WHERE status!='disabled'` で binding 行も対象に入ることを利用
  - 同タスク内に単体テスト追加（failure path のため同 task 内必須）: binding 行の Disable が成功し disabled へ遷移 / その後 UpdateBound 相当が競合（affected=0）で 409（Req 1.6 のいずれか一方のみ確定）
  - _Requirements: 1.6_
  - _Boundary: service.go_
  - _Depends: 5.2_

- [x] 6. handler.go の bind 入力契約変更・recover endpoint・create 応答整理
- [x] 6.1 bind body 除去・recover-bindings endpoint・create 応答変更
  - `bind` handler を body から `signup_url_name` を読まない形へ変更（余分フィールドは無視 / 空 body 許容 / Req 3.3）。`Service.Bind` を id + actor で呼ぶ
  - `POST /tenants/recover-bindings` を `Mount` の sub-route に追加し `Service.RecoverStaleBindings` を呼んで `{recovered:<件数>}` を返す（Req 2.1）。olderThan は既定値
  - `createResponse` から `signup_url_name` フィールドを除去（永続化済みで bind body 不要のため / Req 3.2）。`signup_url` は維持
  - 同タスク内に単体テスト追加: bind が body の signup_url_name を無視し永続値経路で動く（fake Service で検証 / Req 3.3）/ recover-bindings endpoint が件数 JSON を返す（Req 2.1）/ create 応答に signup_url_name が含まれない（Req 3.2）
  - _Requirements: 3.2, 3.3, 2.1_
  - _Boundary: handler.go_
  - _Depends: 5.2_

- [x] 7. integration test（RLS 下の予約・解放・回収・束縛・可逆性）
- [x] 7.1 tenant_repository_test.go に予約/解放/回収/署名束縛の競合テストを追加
  - 本 task は先行 task 3.1（Req 1.1/1.4/1.5）・task 3.2（Req 2.1）で `_Requirements_partial:_` 明示した deferred test を解消する dedicated regression test task（スコープは実 PostgreSQL を要する affected rows / sweep 挙動の integration test に限定）
  - ReserveBinding 並行: 同一 id 2 回で affected 1/0（Req 1.1 / NFR 2.1、orphan 防止の DB 層証跡）
  - ReleaseBinding: binding→pending_bind affected=1、pending_bind 行は affected=0（Req 1.5）
  - RecoverStaleBindings: 古い binding（updated_at 過去）のみ回収、新しい binding 据え置き（Req 2.1）
  - UpdateBound WHERE status='binding': pending_bind 行への UpdateBound affected=0（Req 1.4 の WHERE 変更回帰）
  - binding↔disable: binding 行に UpdateDisabled affected=1、その後 UpdateBound affected=0（Req 1.6）
  - signup_url_name 永続化往復: Insert→Get で signup_url_name 一致（Req 3.1）
  - 既存 `migrations_reversible_test` が enum 4 値 + signup_url_name 列込みで全 down→up pass（Req 4.1）
  - _Requirements: 1.1, 1.4, 1.5, 1.6, 2.1, 3.1, 4.1_
  - _Boundary: tenant_repository_test.go_
  - _Depends: 6.1_

## Verify

本 spec の実装後、watcher（stage-a-verify gate）が再実行すべき verify コマンドを宣言する。
integration test は DB env 未設定時に自身で t.Skip するため、`go build` / `go vet` / DB 非依存の
`go test` で回帰を担保する。

<!-- stage-a-verify -->
```sh
cd backend && go build ./... && go vet ./... && go test ./internal/tenant/...
```
