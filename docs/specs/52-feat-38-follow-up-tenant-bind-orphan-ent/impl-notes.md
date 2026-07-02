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

| 1.2（並行敗者の orphan 防止） | task 4.1（Bind 敗者 `reserveBindingAffected=0` → 409 + `CallCount("CreateEnterprise")==0` + UpdateBound 未呼出）。DB 層 affected 1/0 は task 7.1 |
| 1.3（binding/bound への新規 bind 拒否） | task 4.1（`TestService_Bind`「binding 状態への新規 bind…409」「bound 状態への再 bind…409」）|
| 1.4（UpdateBound 起点を binding へ / Service 配線） | task 4.1（勝者 `updateBoundAffected=1` で bound 確定 / `affected=0` で 409）。WHERE 変更自体は task 3.1、DB 回帰は task 7.1 |
| 1.5（CreateEnterprise 失敗時の再 bind 可能化 / Service 配線） | task 4.1（`TestService_Bind`「CreateEnterprise 失敗…ReleaseBinding 呼出」「空 enterprise_name…ReleaseBinding 呼出」「解放失敗時も元 error 優先」）。ReleaseBinding 具象は task 3.1 |
| 1.7（bind イベント監査対象化） | task 4.1（各経路で `Record(bind, success/failure)` 発火を全ケースで検証） |
| 3.2（永続値を CreateEnterprise へ / body 除去の Service 側） | task 4.1（`TestService_Bind`「永続値が CreateEnterprise 引数に渡り body の値は無視される」）。DTO `BindInput.SignupURLName` 除去・create 応答整理は task 6.1 |
| 3.4（未永続化 signup_url_name の fail-closed 拒否） | task 4.1（`TestService_Bind`「永続 signup_url_name 空…422 + ReserveBinding/CreateEnterprise 未呼出」）|
| 4.2（disabled への bind 拒否継続） | task 4.1（`TestService_Bind`「disabled 状態への bind…422」）|
| 4.3（不在 404 / 部分遷移を残さない） | task 4.1（不在 `CodeNotFound` / CreateEnterprise 失敗時 ReleaseBinding で binding を残さない）|

| 3.1（signup_url_name 永続化 / create 順序変更） | task 5.1（`Create` を CreateSignupURL→Insert 順へ変更し戻り値の signup_url_name を Insert 行に積む。`TestService_Create`「成功時…signup_url_name が Insert 行に永続化される」/「CreateSignupURL 失敗・空応答時に Insert しない」）。永続化往復（Insert→Get 一致）は task 7.1 |
| 2.1（中断予約の回収 / Service 配線） | task 5.2（`RecoverStaleBindings` ユースケース：`TestService_RecoverStaleBindings`「古い binding 行が回収され Record(recover) 発火し件数を返す」/「0 件」/「Repository 失敗伝達」）。sweep の RLS 下挙動は task 7.1 |
| 2.2（回収後の再 bind が二重作成しない） | task 5.2（`TestService_RecoverStaleBindings`「回収後の再 bind が新たな ReserveBinding を通り CreateEnterprise を 1 回のみ呼ぶ」）|
| 2.3（binding を未バインド扱い） | task 5.2（`EnterpriseNameForTenant` の status 分岐に binding 追加。`TestService_EnterpriseNameForTenant`「binding のとき CodeBusinessRule（ErrNotBound）を返す」）|
| 2.4（recover イベントの監査対象化 / Service 配線） | task 5.2（`RecoverStaleBindings` が各回収 id に `Record(recover, success)` 発火。`TestService_RecoverStaleBindings` で actor/tenant_id/operation/result を検証）。`OperationRecover` 型は task 2.1 |
| 4.4（binding を 4 値 View / ガードで返す） | task 5.2（`EnterpriseNameForTenant` が binding を未バインド扱いで拒否。View 返却自体は task 2.1） |
| 1.6（binding↔disable 競合制御） | task 5.3（`Disable` の前提状態判定に binding 追加。`TestService_Disable`「binding テナントの確認一致のとき disabled へ遷移できる」）。binding↔disable の DB 層 affected 競合回帰（UpdateBound affected=0）は task 7.1 |

| 3.2（signup_url_name 永続値正本化 / DTO・応答除去） | task 6.1（`BindInput.SignupURLName` 削除で body 混入経路を型除去、`createResponse` から signup_url_name 除去。`TestCreate_Success_ReturnsPendingBindAndSignupURL`「応答に signup_url_name が含まれない」）。CreateEnterprise への永続値引き渡しは task 4.1 |
| 3.3（他テナント値で bind 不可） | task 6.1（bind handler が `decodeJSONAllowEmpty` で body の signup_url_name を無視。`TestBind_Success_ReturnsBoundView`「body の別テナント signup_url_name を無視し bound」/ `TestBind_EmptyBody_UsesPersistedSignupURLName`「空 body でも永続値経路で bind」）|
| 2.1（中断予約の回収 / handler 露出） | task 6.1（`Service` interface へ `RecoverStaleBindings` 宣言 + `POST /tenants/recover-bindings` endpoint 追加。`TestRecoverBindings_ReturnsRecoveredCount`「件数 JSON を返す + 既定 olderThan を渡す」/ `TestRecoverBindings_ServiceError_MapsToHTTPStatus`「DB 失敗を 503 へ写像」）。Service ユースケースは task 5.2、DB sweep は task 7.1 |

> 上表は task 進行に伴い追記する（本 task 6 までが担保する AC を記載）。

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

### Task 4（service.go の Bind 2 段確定フロー / orphan 防止の核）

- **採用方針**: `Bind` を「Get → 永続 signup_url_name 検証 → ReserveBinding → 勝者のみ
  CreateEnterprise（永続値）→ UpdateBound(WHERE binding) / 失敗時 ReleaseBinding」の 2 段確定へ
  書き換え、CreateEnterprise を予約勝者の 1 要求に限定して orphan を構造的に排除した。
- **重要な判断**:
  - **task 3 の interface deferral を本 task で完了**: `ReserveBinding` / `ReleaseBinding` を
    `Repository` interface に宣言追加（具象は task 3.1 実装済み）。消費側 Service が interface
    経由で呼ぶため compile に必須。`RecoverStaleBindings` は本 task で消費しないため interface に
    **追加せず** task 5.2 へ残した（具象メソッドのみ存続）。
  - **body の signup_url_name は読まない**: 永続 `row.SignupURLName` を正本に使い、`Bind` の
    signature 第 4 引数 `in BindInput` は `_` で受けて未使用化（Req 3.2 / 3.3）。`BindInput` 型・
    `BindInput.SignupURLName` フィールド削除・handler / create 応答整理は **task 6.1 へ残す**
    （types.go / handler.go boundary 外で削除すると build が壊れる / task 2 deferral と同系）。
  - **ReleaseBinding は CreateEnterprise 失敗・空応答時のみ**: `releaseBindingBestEffort` helper で
    binding→pending_bind を best-effort 解放し、ReleaseBinding 自体の失敗は元 bind error を優先
    伝達して WARN ログに残す（design L453-454）。**UpdateBound 失敗 / affected=0 では
    ReleaseBinding しない**（回収 / disable 競合の敗者であり sweep が後追い回収する / Req 2.1）。
  - **未永続化検証は Get の後**: 旧フロー（body 値を Get 前に 400 で弾く）を新仕様（永続値を Get
    後に検証し未永続化なら 422 `CodeBusinessRule` インライン error）へ置換。新 sentinel を types.go
    に足すと boundary 外のためインライン error に留めた（Req 3.4）。
- **残存課題（次 task への影響）**: (1) `BindInput.SignupURLName` フィールド削除 / handler が body を
  読まない化 / create 応答からの `signup_url_name` 除去 / recover-bindings endpoint は task 6.1。
  (2) `RecoverStaleBindings` の Service ユースケース配線 + interface 追加 + `EnterpriseNameForTenant`
  の binding 整合は task 5.2、`Create` の順序変更（CreateSignupURL→Insert）は task 5.1、`Disable` の
  binding 整合は task 5.3。(3) ReserveBinding/ReleaseBinding の affected rows 実挙動・UpdateBound の
  WHERE binding 回帰は task 7.1 の integration test。

### Task 5（service.go の create 順序変更・回収・状態整合）

- **採用方針**: 既存 service.go の record/logDeny helper・doc コメント様式・状態 switch パターンを
  踏襲し、Create 順序変更（5.1）・RecoverStaleBindings ユースケース + EnterpriseNameForTenant の
  binding 整合（5.2）・Disable の binding 整合（5.3）を追加した。
- **重要な判断**:
  - **Create 順序変更で旧 Req 1.4 解釈を design 確定方針へ追従（5.1）**: `Create` を
    「name 検証 → CreateSignupURL → 空応答チェック → Insert(pending_bind, signup_url_name) → record」
    順へ変更し、signup_url_name（CreateSignupURL 戻り値）を発行元束縛の正本として Insert で永続化
    （Req 3.1）。URL 生成失敗 / 空応答時は Insert せず pending_bind 行を作らない（design L443 /
    確認事項 4 の新解釈。#38 Req 1.4「URL 生成失敗時 pending_bind 維持」を design が更新）。これに
    伴い既存 Create テストの「URL 失敗時 Insert 呼出（pending_bind 維持）」assertion を新挙動
    （Insert 未呼出）へ更新した（テストを弱めるのではなく design 確定済み挙動への追従）。
  - **Service interface への RecoverStaleBindings 宣言を task 6.1 へ build-safe deferral（5.2）**:
    具象 `*service.RecoverStaleBindings` のみ追加し、`Service` interface 宣言は task 6.1 へ残した。
    今 interface に宣言すると handler_test.go の `fakeTenantService`（`var _ Service` assertion 付き /
    task 5 boundary 外）が interface を満たさず stage-a-verify gate（`go test ./internal/tenant/...`）が
    compile error で壊れるため。task 3→4 の Repository interface deferral と完全に対称。単体テストは
    white-box（`package tenant`）から `h.svc.(*service).RecoverStaleBindings(...)` で具象を呼ぶ。
    Repository interface への RecoverStaleBindings 宣言追加 + fakeRepository 追従は本 task で完了した
    （task 4 が ReserveBinding/ReleaseBinding を追加したのと同じ consumer-task 追加）。
  - **回収は status のみ戻し再 bind は通常経路（5.2）**: `RecoverStaleBindings` は
    Repository へ委譲し回収各 id に Record(recover)+構造化ログ（tenant id のみ / NFR 3.2）を発火。
    回収は enterprise_name を書かないため再 bind は通常の ReserveBinding 経路を通り二重作成しない
    （Req 2.2）。EnterpriseNameForTenant は binding を pending_bind と同じ ErrNotBound（422）で拒否
    （Req 2.3 / 4.4）。
  - **Disable の binding 整合は Repository 変更不要（5.3）**: 前提状態 switch に binding を追加する
    のみ。既存 UpdateDisabled の `WHERE status!='disabled'` が binding 行も対象に取るため SQL 変更不要。
- **残存課題（次 task への影響）**: (1) Service interface への RecoverStaleBindings 宣言 + handler の
  recover-bindings endpoint 配線 + handler_test.go の fakeTenantService 更新 + bind body 除去 +
  create 応答整理 = task 6.1。 (2) ReserveBinding/ReleaseBinding/RecoverStaleBindings の affected rows・
  sweep 実挙動、UpdateBound WHERE binding 回帰、binding↔disable の DB 層 affected 競合
  （binding 行 UpdateDisabled affected=1 後 UpdateBound affected=0）、signup_url_name 永続化往復
  （Insert→Get 一致）の integration 回帰 = task 7.1。

### Task 6（handler.go の bind 入力契約変更・recover endpoint・create 応答整理）

- **採用方針**: 永続 signup_url_name を正本とする新契約を Handler 層で完結させ、先行 task
  2.1 / 4.1 / 5.2 が build-safe deferral として 6.1 へ残した cross-file cleanup（BindInput
  フィールド削除 / Service interface 宣言）をまとめて解消した。
- **重要な判断**:
  - **BindInput フィールド削除の cross-file 波及**: `BindInput.SignupURLName` を除去し空 struct 化。
    consumer（`service_test.go:1364` の `BindInput{...}`・`handler_test.go` の
    `lastBindIn.SignupURLName` 参照・`test/integration` の fakeTenantService）を同 commit で追従。
    `Service.Bind` シグネチャは維持（design L211）し、body 値の混入経路を型レベルで排除（Req 3.3）。
  - **bind は `decodeJSONAllowEmpty` へ**: body から signup_url_name を読まない。余分フィールド
    無視 + 空 body 許容（単一 JSON document 検証は維持 / design L307-309）。
  - **Service interface deferral 完了**: `RecoverStaleBindings` を interface へ宣言（Req 2.1）。
    handler_test / integration の fakeTenantService が `var _ Service` を満たすようメソッド追加。
  - **recover endpoint の olderThan 既定値**: `POST /tenants/recover-bindings`（静的 route。
    chi では `/{id}/bind` と衝突しない）を追加。body を読まず package const
    `defaultStaleBindingThreshold = 15 * time.Minute`（暫定値 / design Open Q 3・確認事項 3）を
    Service へ渡す。DB 失敗は `errors.WriteHTTP` の Code 写像で 503（design API Contract L321）。
  - **createResponse からの signup_url_name 除去**: 永続化済みで bind body 不要になったため除去
    （Req 3.2）。`signup_url`（admin 訪問用）は維持。既存 create テストを「signup_url_name キーが
    含まれない」検証へ変更（assert を緩めず新契約を検証）。
- **残存課題（task 7/7.1 への影響）**: ReserveBinding/ReleaseBinding/RecoverStaleBindings の
  affected rows・sweep 実挙動、UpdateBound WHERE binding 回帰、binding↔disable の DB 層 affected
  競合、signup_url_name 永続化往復（Insert→Get 一致）の integration 回帰は task 7.1 へ残る。
  recover-bindings の olderThan config 化（暫定 15 分の確定）は運用要件（design 確認事項 3）。

### Task 7（integration test: RLS 下の予約・解放・回収・束縛・可逆性）

- **採用方針**: 先行 task 3.1（Req 1.1/1.4/1.5）・3.2（Req 2.1）が `_Requirements_partial:_` で
  deferred した「実 PostgreSQL を要する affected rows / sweep 挙動」を、既存
  `tenant_repository_test.go` と同スタイルで 7 関数追記して解消した（DB env 未設定時は既存
  `requireDBURLs` が self-skip）。
- **重要な判断**:
  - **updated_at のエイジングは pool 直叩き**: `RecoverStaleBindings` の「古い binding のみ回収」を
    検証するため、`tenants.updated_at`（自動更新トリガー無し / Repository に過去化メソッド無し）を
    isolation test と同じ `platformdb.BeginTxFunc(saCtx, pool, ...)` + SuperAdmin ctx で
    `UPDATE ... updated_at = now() - make_interval(secs => $1)` により過去へ寄せた。しきい値評価は
    DB 側 `now()` 基準（実装踏襲）なので stale を 1h 前・しきい値 30m とし margin を確保。
    `setupTenantRepo` は repo しか返さないため当該テストのみ個別 setup を組み helper シグネチャは不変。
  - **Req 4.1（enum 4 値 + signup_url_name 列の可逆性）は重複追加しない**: 既存
    `migrations_reversible_test`（0017 込みの全 down→up / task 7.1 boundary 外・変更禁止）が担保済み。
    本ファイルの各テストが 0017 適用済みスキーマ上で `binding` enum 値と `signup_url_name` 列を
    実際に往復させることで Req 4.1 の実挙動証跡を兼ねる（tasks.md L90 の整理）。
  - **既存 UpdateBound テストの fixture 追従（coordinator 承認済み）**: task 3.1 の
    `UpdateBound` WHERE 変更（`pending_bind`→`binding`）で破損する #38 由来の既存 2 件を、
    coordinator 承認のもと boundary（`tenant_repository_test.go`）内で新契約へ追従させた
    （下記残存課題参照）。assert 本体は非改変で ARRANGE への `ReserveBinding` 追加のみ。
- **残存課題（対応済み）**: task 3.1 が `UpdateBound` の WHERE を
  `status='pending_bind'`→`status='binding'` へ変更した結果、#38 由来の既存 integration test
  `TestTenantRepository_UpdateBound_AffectedAndDoubleBind` と
  `TestTenantRepository_UpdateBound_DuplicateEnterpriseName_Conflict` が実 DB で破損する
  （`Insert`（pending_bind）直後に `UpdateBound` を呼び affected=1 / bound / 23505 を期待するが、
  新 WHERE では affected=0 となり fail する）ことを検出し、当初は「追記のみ」制約により flag に
  留めていた。**coordinator の承認（design.md で `UpdateBound WHERE status='binding'` は確定済みであり、
  fixture 追従は task 5.1 が Create テストを新挙動へ追従させた前例と同じ扱い）を受け、boundary 内で
  fixture 追従を完了**した:
  - `AffectedAndDoubleBind`: 初回 UpdateBound の前に `ReserveBinding`（affected=1）を挟み binding 行に
    してから確定。2 回目 UpdateBound は既に bound（binding でない）で affected=0（既存 assert 維持）。
  - `DuplicateEnterpriseName_Conflict`: A・B 双方に `ReserveBinding`（各 affected=1）を挟む。B を予約
    しないと UpdateBound B が no-op(affected=0) で 23505 に到達しないため両方を binding にしてから
    重複 enterprise_name 投入 → CodeConflict（既存 assert 維持）。
  いずれも assert は緩めず ARRANGE への `ReserveBinding` 追加のみ（＋起点変更を示す inline / doc コメント
  更新）。旧 2 件は「二重 bind idempotency」「重複 enterprise_name→23505/CodeConflict」という新規
  `BindingRowConfirmsBound` では代替できない観点を持つため統廃合せず温存。spec 本文（tasks.md /
  design.md / requirements.md）は書き換えていない。

## 確認事項

- **【要人間判断 / task 7.1】fixture 修正 commit が marker 後方に積まれた（post-marker / 非 docs）**:
  task 7.1 の marker（`docs(tasks): mark 7.1 as done` = `0492658`）確定後に、既存 UpdateBound
  integration test の fixture 追従（`efcabae` test(#52): `tenant_repository_test.go` = **非 docs**）と
  その記録更新（`350c7c5` docs(impl-notes)）を追加 commit した。これは fixture 破損を marker 確定
  **後**に検出し、per-task 制約（`git reset` / `git rebase` 禁止 = 既存 commit 温存）により commit 順序を
  後付けで是正できなかったため（`既存 commit と矛盾する変更 → 追加 commit or 確認事項で人間判断` の
  後者に該当）。結果、watcher の `pt_resolve_diff_range(7.1)` は review range 終端を `0492658` に固定し、
  post-marker 群 {`9e87504`,`eabd04a`,`efcabae`,`350c7c5`} のうち `efcabae` が
  `POST_MARKER_DOCS_ALLOWLIST`（`**/impl-notes.md,docs/specs/**/*.md`）外の非 docs 変更のため
  `pt_classify_post_marker_paths` が `mixed` 判定 → **docs-only-auto-refresh は発火せず**、既定
  `POST_MARKER_RECOVERY_MODE=fail-with-diagnostic` では task 7.1 の per-task Reviewer 起動前に
  rc=5（silent range truncation 検出）で escalate し得る。
  - **修正内容自体は正しく検証済み**（design.md L138/L151/L251 で `UpdateBound WHERE status='binding'` は
    確定 / assert 非緩和・ARRANGE への `ReserveBinding` 追加のみ / `go build`・`go test ./internal/tenant/...`・
    `./test/integration/...` は green [integration は DB env 未設定で self-skip]）。mechanics（marker 位置）の
    問題であってレビュー対象コードの欠陥ではない。
  - **推奨解消策（いずれか / 人間判断）**: (1) task 7.1 の per-task Reviewer 起動を
    `POST_MARKER_RECOVERY_MODE=extend-range`（review range を HEAD まで拡張し `efcabae` を含める documented
    opt-in）で回す、(2) 人間が marker を HEAD へ refresh する、(3) diagnostic を受けて手動レビュー。
    orchestrator は reset/rebase 禁止・pushed 済みブランチのため history 書き換えは選択しなかった。

- **task 6.1 の boundary 拡張（handler.go 外への意図的な cross-file cleanup 完了）**: task 6.1 の
  `_Boundary:_` は `handler.go` のみだが、`types.go`（`BindInput.SignupURLName` 削除）/
  `service.go`（`Service` interface への `RecoverStaleBindings` 宣言）/ `service_test.go`
  （`BindInput{}` への追従）/ `handler_test.go`（新契約テスト）/ `test/integration/tenant_admin_guard_test.go`
  （fakeTenantService への `RecoverStaleBindings` 追加 = 新規公開 IF による既存テスト fixture 追従 / Issue #410）
  にも触れた。これは scope creep ではなく、**先行 task 2.1 / 4.1 / 5.2 が build-safe deferral として
  明示的に 6.1 へ残した cross-file cleanup の完了**である（各 task の impl-notes learning に deferred 先=6.1
  と記録済み。design File Structure Plan L109-114 も同 spec 内の変更ファイルとして列挙）。tasks.md /
  design.md 本文は書き換えていない（marker のみ）。
- **recover-bindings の olderThan 既定 15 分は暫定値**: `defaultStaleBindingThreshold = 15 * time.Minute`
  は design Open Q 3（L509-511）/ 確認事項 3 の「既定値 + 将来 config 化の余地。具体値は運用要件として
  要確認」に基づく暫定採用値であり、確定値ではない。運用要件確定後に config 化 / 値見直しが必要
  （派生タスク候補）。requirements.md（Req 2.1）は「回収手段の存在」のみを規定し具体しきい値を固定
  していないため spec 本文との矛盾はない。

- **Service interface への `RecoverStaleBindings` 宣言を task 6.1 へ deferred（本 task 5.2 では実施せず）**:
  具象 `*service.RecoverStaleBindings(ctx, actor, olderThan) (int, error)` のみ追加し、`Service`
  interface への宣言追加は task 6.1 へ残した。理由: `handler_test.go` の `fakeTenantService`
  （`var _ Service` 型 assertion 付き / task 5 の `_Boundary: service.go_` 外）が interface を満たさ
  なくなり stage-a-verify gate（`go test ./internal/tenant/...`）が compile error で壊れるため。これは
  task 3→4 の Repository interface deferral と完全に対称な build-safe deferral であり、Service interface
  宣言 + handler 配線 + fakeTenantService 更新は consumer task 6.1 の責務。tasks.md は書き換えていない
  （実装上の deferral 記録）。
- **Create 順序変更（5.1）に伴う既存テスト更新（design 確定済み挙動への追従）**: `Create` を
  CreateSignupURL→Insert 順へ変更し URL 生成失敗 / 空応答時は Insert しない仕様（design L440-444 /
  確認事項 4）へ追従するため、既存 Create テスト 2 件の assertion を「Insert 呼出（pending_bind 維持 /
  旧 Req 1.4）」から「Insert 未呼出（新 Req 3.1 解釈）」へ更新した。これは assert を緩めるのではなく
  design が確定した挙動変更への追従であり、URL 生成失敗時に pending_bind 行を作らない新仕様を検証する。
  なお requirements.md（Req 3.1）と design.md（L440-444 / 確認事項 4）は本挙動を確定済みであり spec 本文
  との矛盾は検出していない（spec 本文は変更していない）。

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
### PR #62 レビュー（codex round 3 / 4 findings legitimate）への対応区分

PR #62 の codex レビュー（`aa1fb50` に対する 4 指摘 / 自動裁定 legitimate=4）を精読し、以下の
4 件を「設計確定済み（対応不要）」2 件と「設計イテレーション要（impl PR スコープ外）」2 件に
区分した。impl PR では design.md / tasks.md / requirements.md を書き換えないため（設計 PR で
人間レビュー済み）、後者 2 件は本 確認事項に escalate し、返信本文でも「設計と矛盾/未確定のため
本 impl PR では取り込まず設計イテレーション or 別 Issue 再提起を推奨」と明記した。

- **[finding 1 / high・設計確定済み] `service.go:328` UpdateBound affected=0 経路の orphan**:
  disable / RecoverStaleBindings が binding を先取りして勝つと、勝者の CreateEnterprise 成功済み
  Enterprise が AMAPI 上 orphan として残る指摘。これは design.md「回収方式の決定」(L366-368) /
  「binding↔disable の競合制御」(L379-383) と requirements Out of Scope（AMAPI 既存 orphan の能動削除は
  対象外）で **明示的に受容したトレードオフ**であり、AMAPI ラッパに削除・逆引き IF が無いため能動補償は
  構造的に不可能。既に NFR 3.1 の構造化ログ（`service.go:339-343`、enterprise_name 付き）で運用者が
  手動棚卸しできる形に可観測化済み。**対応不要**（設計と矛盾するため取り込まない）。

- **[finding 2 / high・設計イテレーション要] `repository.go:267`/`348` 予約に所有者/世代が無い（ABA lost update）**:
  ReserveBinding→CreateEnterprise 実行中に RecoverStaleBindings が当該 binding を pending_bind へ解放し、
  別リクエストが再予約（binding）した後、古い in-flight 要求の `UpdateBound WHERE status='binding'`（または
  失敗経路の `ReleaseBinding WHERE status='binding'`）が **別リクエストの予約を誤確定/誤解放**し得る。
  これは NFR 2.1「lost update を発生させない」に対する実挙動の反例であり、指摘は妥当。
  - **根本原因**: binding 予約に所有者/世代（generation）を持たせていないため、`WHERE status='binding'` だけ
    では「自分の予約」と「回収後に張られた別予約」を区別できない。
  - **推奨修正（設計イテレーション / 別 Issue で確定すべき data-model・contract 変更）**:
    - 案A（列追加）: `binding_token uuid`（または `binding_generation bigint`）列を migration 0018 で追加。
      ReserveBinding が採番して `RETURNING` し、`UpdateBound` / `ReleaseBinding` の WHERE に
      `AND binding_token = $token` を足す。堅牢だが design.md Data Models / Repository 契約の更新を伴う。
    - 案B（既存列の楽観ロック化）: 既存 `updated_at`（または system column `xmin`）を世代トークンとして使い、
      ReserveBinding が `RETURNING updated_at`、確定/解放を `AND updated_at = $reservedAt` で guard する。
      migration 不要だが timestamptz / xid の round-trip 完全一致に依存するため **実 PostgreSQL での
      integration 検証が必須**。
  - **本 impl PR で fix しない理由**: (1) いずれの案も ReserveBinding/UpdateBound/ReleaseBinding の
    **戻り値・引数（Repository 契約）と Data Models を変更**するため、設計 PR で人間レビュー済みの
    design.md（L285-296 の現行シグネチャ）を impl PR が単独で書き換える範囲を超える。(2) 本 impl 環境に
    実 PostgreSQL が無く（integration test は self-skip / 単体は fake repository）、SQL 層の並行 guard を
    **検証不能**。guard 述語を誤ると happy path の UpdateBound が全件 affected=0 となり全 bind が壊れる
    fail-broken リスクがあり、未検証のまま盲目 push はしない（CLAUDE.md「既存挙動を壊さない」/ 「Red→Green」）。
  - **到達条件（重大度の補足）**: 本 ABA は CreateEnterprise が stale しきい値（暫定 15 分 / 確定 15 分）を
    超えて in-flight し続け、その間に recover が走り、さらに別 bind が再予約する狭い極端 race。実害は
    低確率だが NFR 2.1 反例のため設計イテレーションで generation を入れることを推奨。finding 1 の
    「disable/recover が正当に勝った単一要求 orphan」は generation 導入後も受容トレードオフとして残る
    （generation はクロス要求の誤確定=finding 2 のみを消す）。

- **[finding 3 / medium・設計確定済み] `service.go:250` binding への再 bind が常に 409（Req 2.2 の冪等再 bind 未実装）**:
  Req 2.2「binding のまま中断したテナントへの再 bind を冪等に扱い進行または完了させる」を Bind 単体で
  実装していない指摘。design.md は Req 2.2 を **recover→再予約経路**で満たす設計（L156 / 「回収方式の決定」
  L353-370）であり、「binding 行への再 bind 時に既存 Enterprise を照会して紐付け直す」冪等化は
  **AMAPI に signup_url_name からの逆引き IF が無いため実現不可能**（L369-370 / 確認事項1）と設計判断済み。
  Bind 内で binding 行を inline 回収すると、in-flight の元 CreateEnterprise がなお成功し得るため二重作成の
  リスクがある。しきい値ベースの recover が意図的な機構。**対応不要**（設計と矛盾するため取り込まない /
  逆引き IF が実在するなら設計変更余地ありは設計 PR 確認事項1 のまま）。

- **[finding 4 / medium・設計イテレーション要] `repository.go:384` recover が signup_url_name を再発行しない**:
  RecoverStaleBindings は status を pending_bind へ戻すのみで signup_url_name を更新せず、その後の Bind
  （`service.go:301`）が同じ永続値を再利用するため、元の CreateEnterprise がタイムアウト後に成功して
  signup URL を consume 済みだった場合、回収後の再 bind が恒久的に成立しない（塩漬け）指摘。妥当。
  - **設計との関係**: design.md「回収方式の決定」(c)（L362-363）は「回収後の再 bind は **新しい signup_url**
    から再予約する（古い signup_url_name は consume 済みの可能性があり再利用しない）」と **意図を明記**して
    いるが、その新 signup_url を取得する機構（recover 内での再発行 / 別 re-provision endpoint）が design の
    API Contract（L318-322）に未定義であり、impl は旧値再利用のまま = **設計意図と実装の乖離 + 設計の
    機構未確定**。
  - **推奨修正（設計イテレーション / 別 Issue で機構を確定）**:
    - 案A: Service.RecoverStaleBindings が回収各テナントへ `amapi.CreateSignupURL` で新 signup_url を再発行し
      新設 `Repository.UpdateSignupURLName` で永続化（sweep 内 AMAPI 呼び出し + 新 repo 契約）。
    - 案B: 別 re-provision endpoint（`POST /tenants/{id}/reissue-signup-url` 等）を追加し、再 bind 前に admin が
      新 signup_url を採り直す。
  - **本 impl PR で fix しない理由**: いずれも design が gesture のみで未確定の機構（sweep への AMAPI 追加 /
    新 repo・API 契約）の追加であり、Developer が impl PR で仕様を追加・解釈する範囲を超える（PM/Architect
    差し戻し相当）。加えて AMAPI + PostgreSQL を要し本環境で検証不能。設計イテレーションで案A/Bを確定
    することを推奨。

- 上記 4 件（設計確定済み 2 / 設計イテレーション要 2）を除き、現時点で spec（requirements.md /
  design.md / tasks.md）と実装の間にその他の矛盾は検出していない。

### PR #62 レビュー（codex round 5 / 3 findings legitimate）への対応区分

PR #62 の codex レビュー（`d3bdc75` に対する 3 指摘 / 自動裁定 legitimate=3）を精読した。3 件とも
**確定済み design.md の記述と矛盾する**か、**design が機構を未確定にした論点**であり、impl PR では
design.md / requirements.md / tasks.md を書き換えない（設計 PR で人間レビュー済み）ため、いずれも
behavior change を積まず返信本文で「設計と矛盾/未確定のため本 impl PR では取り込まず設計イテレーション
or 別 Issue 再提起を推奨」と明記した。round 3 の finding 1 / finding 4 の再提起 2 件に加え、round 5 で
新規に出た `UpdateDisabled` の `enterprise_name` 保持（finding 3）を以下に区分する。

- **[round5 finding 1 / high・設計確定済み・round3 finding 1 の再提起] `service.go:322` UpdateBound
  affected=0 経路の orphan**: disable / RecoverStaleBindings が binding を先取りして勝つと勝者の
  CreateEnterprise 成功済み Enterprise が AMAPI 上 orphan として残る指摘。design.md「回収方式の決定」
  (L364-368) /「binding↔disable の競合制御」(L379-383) と requirements Out of Scope（AMAPI 既存 orphan の
  能動削除は対象外）で **明示的に受容したトレードオフ**であり、AMAPI ラッパに削除・逆引き IF が無いため
  能動補償は構造的に不可能。UpdateBound affected=0 の敗者が disabled/pending_bind へ enterprise_name を
  書くのは未定義遷移（NFR 1.2 / Req 4.2）で不可。既に NFR 3.1 の構造化ログ（`service.go:339-343`、
  enterprise_name 付き）で運用者が手動棚卸しできる形に可観測化済み。**behavior change 不要**（Req 1 の
  objective 文言「bind 中の無効化でも残さない」と design 受容トレードオフの緊張は設計 PR で確定済み /
  再検討するなら設計イテレーション）。

- **[round5 finding 2 / high・設計イテレーション要・round3 finding 4 の再提起（medium→high 昇格）]
  `repository.go:384` recover が signup_url_name を再発行しない**: RecoverStaleBindings は status を
  pending_bind へ戻すのみで signup_url_name を更新せず、Bind（`service.go:301`）が同じ永続値を再利用する
  ため、元 CreateEnterprise がタイムアウト後に成功し signup URL を consume 済みだった場合、回収後の再 bind
  が成功しない（tenant は pending_bind のため disable は可能だが bind は成立しない）。design.md「回収方式の
  決定」(c)(L361-363) は「回収後の再 bind は **新しい signup_url** から再予約する」と意図を明記しつつ、
  その新 signup_url を得る機構（recover 内での再発行 / 別 re-provision endpoint）が design の API Contract
  (L318-322) に未定義。**設計意図と実装の乖離 + 機構未確定**であり、修正には (案A) Service.RecoverStaleBindings
  が回収各行へ `amapi.CreateSignupURL` で再発行し新設 `Repository.UpdateSignupURLName` で永続化、または
  (案B) 別 re-provision endpoint 追加、のいずれか新規契約が要る。Developer が impl PR で仕様を追加・解釈する
  範囲を超え（PM/Architect 差し戻し相当）、加えて AMAPI signup URL の single-use 意味論は design 確認事項 1
  (L500-504) で未検証・本環境（AMAPI + PostgreSQL 不在）で検証不能。**設計イテレーションで案A/Bを確定推奨**。

- **[round5 finding 3 / medium・設計確定済み（新規）] `repository.go:432` UpdateDisabled が
  enterprise_name を残す**: bound→disabled で `UpdateDisabled` が status のみ更新し enterprise_name を
  クリアしないのは NFR 1.2「enterprise 識別子を bound でのみ非空として保持」の保存不変条件に反する、という
  指摘。これは design.md「binding↔disable の競合制御」(L382-383) が「disabled 行に enterprise_name が残るのは
  #38 既存挙動… **意図的な選択**」と、`types.go:119-121` が「無効化された行は **監査目的で** DB 上
  enterprise_name を保持し続ける（が disabled 応答には漏らさない）」と、**監査保全のための意図的な設計判断**
  として明記済み。NFR 1.2 を **behavioral invariant**（disabled では enterprise_name を決して露出しない）で
  解釈しており、`ViewFromRow`（`types.go:130-132`：bound のみ載せる）・`EnterpriseNameForTenant`
  （`service.go:548-551`：disabled は ErrTenantDisabled 返却で保存値を読まない）が既にこれを満たす。指摘は
  NFR 1.2 を **storage invariant**（保存値そのものを NULL 化）で解釈しており、design の behavioral 解釈と
  対立する。impl PR で保存値をクリアすると (1) design L382-383 の意図的選択を単独で覆し、(2) どの Enterprise に
  bound していたかの監査情報を破壊し、(3) 部分一意 index（`uq_tenants_enterprise_name`）のスロットを解放する
  behavior change になる。**behavior change 不要**（NFR 1.2 の storage/behavioral 解釈の確定は設計イテレーション
  で design L382-383・types.go 監査保全方針と併せて判断すべき論点）。

- 上記 3 件はいずれも確定済み design と矛盾/未確定であり behavior change を積まない。round 5 時点で spec
  （requirements.md / design.md / tasks.md）と実装の間に、上記論点以外の新たな矛盾は検出していない。

STATUS: complete
