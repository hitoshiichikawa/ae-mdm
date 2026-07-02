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
  - **既存テストは追記のみ・非改変**: prompt の「追記のみ / assertion 非改変」に従い、既存
    `TestTenantRepository_*` は一切書き換えていない（下記残存課題の #38 テスト破損もこの制約により
    本 task では修正せず flag に留めた）。
- **残存課題（要 Architect / PM 判断・flag）**: task 3.1 が `UpdateBound` の WHERE を
  `status='pending_bind'`→`status='binding'` へ変更した結果、#38 由来の既存 integration test
  `TestTenantRepository_UpdateBound_AffectedAndDoubleBind`（L144）と
  `TestTenantRepository_UpdateBound_DuplicateEnterpriseName_Conflict`（L186）が **実 DB では破損**する
  （いずれも `Insert`（pending_bind）直後に `UpdateBound` を呼び affected=1 / bound を期待するが、
  新 WHERE では affected=0 となり両テストが fail する）。本 task の `_Boundary:_` は当ファイルだが
  prompt が「既存テストの assertion を弱めたり書き換えたりしない（追記のみ）」を明示するため、
  ARRANGE 修正（`ReserveBinding` を挟んで binding 行にする fixture 追従）を本 task では実施していない。
  自動 verify（DB env 未設定で self-skip）は green だが実 DB CI では red になるため、これら 2 件の
  ARRANGE を新契約（binding 起点）へ追従させる修正の要否を Architect / PM / Reviewer に確認したい
  （新 `TestTenantRepository_UpdateBound_BindingRowConfirmsBound` が正しい binding→bound 正常系を
  既にカバーしており、旧 2 件は fixture 追従 or 統廃合の判断対象）。spec 本文（tasks.md / design.md /
  requirements.md）は書き換えていない。

## 確認事項

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
- 上記 deferral を除き、現時点で spec（requirements.md / design.md / tasks.md）と実装の間に
  その他の矛盾は検出していない。

STATUS: complete
