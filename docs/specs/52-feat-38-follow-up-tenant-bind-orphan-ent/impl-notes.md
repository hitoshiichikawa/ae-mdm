# 実装メモ（Issue #52 / #38 follow-up: Tenant Bind の競合制御強化）

per-task ループで実装を進める。本ファイルは各 task の learning と AC Traceability を追記していく。

## AC Traceability

| Requirement | 担保 task / テスト |
|---|---|
| 4.1（status 4 値化） | task 1（migration 0017 で `binding` 追加）+ task 2.1（types.go `StatusBinding` 追加 / `TestStatusValid`「binding を受理」/ `TestParseStatus` 4 値変換）/ DB 層の最終証跡は task 7.1 の reversible test |
| 4.4（4 値 View 返却） | task 2.1（`TestViewFromRow`「binding 行は status を binding として載せ enterprise_name を露出しない」） |
| 2.4（recover 操作の監査対象化の型基盤） | task 2.1（`OperationRecover` 追加 / `TestOperationValues`「recover 操作が追加され…」）。Service 配線・Record(recover) 発火は task 5.2 |
| 3.2（signup_url_name 永続値正本化） | task 2.1（`TenantRow`/`CreateInput` に `SignupURLName` 追加）。`BindInput.SignupURLName` 除去と CreateEnterprise への永続値引き渡しは task 4.1/6.1（後述「確認事項」の deferral 参照） |
| 3.1（signup_url_name 永続化） | task 1（migration 0017 で `signup_url_name` 列追加）+ task 2.1（型フィールド追加）+ task 3.1（Insert/Get/List の SQL に列組込、`nullableString` の NULL 写像を `TestNullableString_*` で単体検証）/ 永続化往復（Insert→Get 一致）は task 7.1 |
| 1.1（pending_bind→binding 原子予約） | task 3.1（`ReserveBinding` 追加）。affected rows 実挙動（並行 1/0）は `_Requirements_partial:_` 明示で task 7.1 の integration test へ deferred。Service 配線は task 4.1 |
| 1.4（UpdateBound 起点を binding へ） | task 3.1（`UpdateBound` の WHERE を `pending_bind`→`binding` へ変更）。affected=0 実挙動（pending_bind 行）は partial 明示で task 7.1 へ deferred |
| 1.5（CreateEnterprise 失敗時の再 bind 可能化） | task 3.1（`ReleaseBinding` 追加、binding→pending_bind）。affected rows 実挙動は partial 明示で task 7.1 へ deferred。Service 配線は task 4.1 |
| 2.1（中断予約の回収手段） | task 3.2（`RecoverStaleBindings` sweep クエリ追加）。RLS 下挙動・しきい値境界は全て partial 明示で task 7.1 へ deferred。Service ユースケース配線は task 5.2 |

> 上表は task 進行に伴い追記する（本 task 3 までが担保する AC を記載）。

## Implementation Notes

### Task 1（migration 0017: tenant_status 4 値化 + signup_url_name 列）

- **採用方針**: 0016 と同じ「ALTER のみ・IF NOT EXISTS / IF EXISTS で冪等」パターンに揃え、
  `ALTER TYPE tenant_status ADD VALUE IF NOT EXISTS 'binding'` と
  `ALTER TABLE tenants ADD COLUMN IF NOT EXISTS signup_url_name text` を up に置いた。
- **重要な判断**:
  - enum 値 `binding` は PostgreSQL に `ALTER TYPE ... DROP VALUE` 構文が無いため down で個別削除
    できない。0017 down では削除を試みず no-op（SQL コメントで明示）とし、可逆性は全 down
    シーケンスで 0001 down の `DROP TYPE IF EXISTS tenant_status` が enum 型ごと削除 → 再 up で
    0001 が 3 値再作成 → 0017 が `binding` を再 ADD VALUE することで担保される（design L402-409 の
    方針に一致）。
  - `ADD VALUE` は PostgreSQL 12+ で tx 内実行可能。0017 は追加値を同 migration 内で参照しない
    （DML を含まない）ため tx 内制約に抵触しない。
  - `signup_url_name` は NULL 許容（既存行・create 前整合のため）。Repository 側の NULL→空文字
    写像は後続 task 2.1 / 3.1 の責務（本 task は列追加のみ）。
- **検証**: 実装ファイルは Go コードではないが、回帰がないことを stage-a-verify ブロック
  （`go build ./... && go vet ./... && go test ./internal/tenant/...`）で確認し全 pass。
  `test/integration`（reversible test 含む）も compile + 自己 skip（DB env 未設定）で pass。
  実 PostgreSQL を要する reversible test の green 確認は task 7.1（_Boundary: tenant_repository_test.go_）に
  deferred されている（tasks.md L90 / 本 task は「テスト追加ではなく既存テストの非破壊検証」）。
- **残存課題**: なし（migration ファイル追加のみ。後続 task 2.1 で types.go の `StatusBinding` /
  `SignupURLName` フィールドを追加する前提）。

### Task 2（types.go の 4 値化と signup_url_name フィールド追加）

- **採用方針**: additive かつ build-safe な型変更のみを本 task で完遂し、build を壊す破壊的変更
  （`BindInput.SignupURLName` 削除）は参照コードと同 task（4.1/6.1）へ deferred した。
- **重要な判断**:
  - `StatusBinding="binding"` を追加し `Valid()`/`ParseStatus` を 4 値対応化（Req 4.1 / NFR 1.1）。
    enum 値文字列は migration 0017 / DB enum の `binding` と一致させた。
  - `TenantRow.SignupURLName` は既存 `EnterpriseName` の「NULL→空文字」写像パターンを踏襲（doc 明記）。
    `CreateInput.SignupURLName` は HTTP body 由来でなく `CreateSignupURL` 戻り値を Service が充填する
    内部フィールドのため `json:"-"` とし、create body 契約（Name のみ）を変えない判断とした。
  - **`BindInput.SignupURLName` 削除は本 task で行わず task 4.1/6.1 へ deferred**（理由は「確認事項」
    に詳述）。本 task は型の additive 変更に閉じ、stage-a-verify gate を green に保った。
  - doc コメントの「3 値」記述を Status / TenantRow / TenantView / ViewFromRow / Event.Operation で
    4 値（pending_bind / binding / bound / disabled）へ整合更新（in-boundary、挙動変更なし）。
- **残存課題（次 task への影響）**: (1) `BindInput.SignupURLName` 削除と CreateEnterprise への永続値
  引き渡しは task 4.1（service.go）/ 6.1（handler.go・各 test）で完了する。(2) `signup_url_name` の
  NULL→空文字写像・Insert/Get/List 配線は task 3.1、create 順序変更での永続化は task 5.1。
  (3) `OperationRecover` を用いた Record(recover) 発火は task 5.2。

### Task 3（repository.go の予約・解放・回収メソッドと signup_url_name 永続化）

- **採用方針**: 既存 5 メソッド（`superAdminContext` + `db.BeginTxFunc` + `RowsAffected()` /
  `List` の rows iterate）と同型に予約・解放・回収を追加し、`signup_url_name` を既存
  `enterprise_name` の「NULL→空文字」写像パターンへ統合した。
- **重要な判断**:
  - **interface deferral（build-safe）**: `ReserveBinding` / `ReleaseBinding` /
    `RecoverStaleBindings` は具象 `*repository` メソッドとしてのみ追加し、`Repository` interface
    への宣言追加は **task 4.1（消費側 Service）へ deferred** した。interface に今宣言すると
    `service_test.go` の `fakeRepository`（task 3 の `_Boundary: repository.go_` 外）が interface を
    満たさなくなり stage-a-verify gate（`go test ./internal/tenant/...`）が compile error で必ず壊れる。
    Go は interface 未所属の具象メソッドを build/vet エラーにしないため build green を保てる
    （task 2.1 の `BindInput.SignupURLName` 削除 deferral と同方針）。
  - **`nullableString` 一般化**: 既存 `nullableEnterpriseName` を汎用 `nullableString(s string) *string`
    へ一般化し、enterprise_name / signup_url_name 双方で再利用（DRY・単一責務）。これにより DB
    非依存の単体テスト seam を作り、`TestNullableString_*` で NULL 写像（空文字→nil / 非空→値ポインタ）を検証。
  - **`make_interval(secs => $1)` によるしきい値**: `RecoverStaleBindings` のしきい値は DB 側
    `now()` 基準で評価（design L289-293 の意図＝アプリ/DB クロック乖離回避）。`olderThan.Seconds()`
    を float64 で `make_interval` に渡し、`now() - make_interval(secs => $1)` で比較する。
  - **scan 順序整合**: `signup_url_name` を SELECT 列順では `enterprise_name` の直後に固定し、
    `scanTenantRow` の Scan 引数順も同位置に合わせた（Get / List 双方の SELECT を同順で更新）。
- **残存課題（次 task への影響）**: (1) `ReserveBinding`/`ReleaseBinding`/`RecoverStaleBindings` の
  `Repository` interface 宣言は task 4.1（service.go・service_test.go 同時書換）で完了。
  (2) `UpdateBound` の WHERE を `pending_bind`→`binding` へ変更したため、pending_bind 行への
  `UpdateBound` が affected=0 になる integration 回帰は task 7.1（`_Boundary: tenant_repository_test.go_` /
  tasks.md L87）で解消。 (3) `ReserveBinding`/`ReleaseBinding`/`RecoverStaleBindings` の affected
  rows・sweep 実挙動テスト（Req 1.1/1.4/1.5/2.1）は `_Requirements_partial:_` 明示済みで task 7.1 へ deferred。

## 確認事項

- **`BindInput.SignupURLName` 削除を task 4.1 / 6.1 へ deferred（本 task 2.1 では実施せず）**:
  tasks.md L16 は task 2.1 の作業項目に「`BindInput` の `SignupURLName` フィールド削除（Req 3.2/3.3）」を
  含むが、当該フィールドは task 2.1 の `_Boundary: types.go_` 外の以下から参照されている:
  `service.go:207`（Bind フローの `in.SignupURLName` 読み取り / task 4.1 boundary）、
  `service_test.go` 約 11 箇所（`BindInput{SignupURLName: ...}` / task 4.1 のテスト書き換え対象）、
  `handler_test.go:588-589`（`fake.lastBindIn.SignupURLName` / task 6.1 対象）。
  本 task でフィールドを削除すると `go build ./...` / `go test ./internal/tenant/...`（= stage-a-verify
  gate）が必ず壊れ、修正が boundary 外へ大きく波及する（CLAUDE.md「既存テストを壊さない」制約に抵触）。
  そのため削除は参照コードを同時に書き換える task 4.1（service.go・service_test.go）/ 6.1
  （handler.go・handler_test.go）へ deferred した。Req 3.2 の DTO 除去スライスは当該 task で完了する。
  これは tasks.md の書き換えではなく実装上の deferral 記録であり、spec 本文は変更していない。
- **task 3 の interface deferral（`ReserveBinding`/`ReleaseBinding`/`RecoverStaleBindings`）**:
  これら 3 メソッドは具象 `*repository` メソッドとしてのみ追加し、`Repository` interface 宣言は
  task 4.1（service.go・service_test.go の `fakeRepository` を同時に書き換える task）へ deferred した。
  task 3 の `_Boundary: repository.go_` 外の `service_test.go` を触らず stage-a-verify gate を green に
  保つための build-safe deferral であり、tasks.md は書き換えていない。
- **UpdateBound の WHERE 変更（`pending_bind`→`binding` / Req 1.4）の integration 回帰移譲**:
  本変更により pending_bind 行への `UpdateBound` が実 DB 上 affected=0 になる。当該 integration 回帰
  （tasks.md L87「pending_bind 行への UpdateBound affected=0」）は task 7.1
  （`_Boundary: tenant_repository_test.go_`）の責務であり、本 task では既存 integration test を触らない。
- 上記 deferral を除き、現時点で spec（requirements.md / design.md / tasks.md）と実装の間に
  その他の矛盾は検出していない。

STATUS: complete
