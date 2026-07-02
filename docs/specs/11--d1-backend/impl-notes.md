# 実装ノート（#11 D1 アプリ配信 backend）

## AC Traceability（task 1 が担保する AC）

| AC | 内容 | 担保テスト（`internal/app/repository_test.go`） |
|----|------|-------------------------------------------------|
| 2.2 | List 0 件は非 nil 空 slice | `TestCollectTenantAppRows_EmptyReturnsNonNilSlice` / `List` が `make([]TenantAppRow,0)` 起点 |
| 2.3 | 他テナント行を返さない（tenant-scoped / 昇格しない） | `TestListSQL_TenantPredicate` / `TestUpsertSQL_OnConflictDoUpdate` / `TestApprovedPackagesSQL_TenantPredicate` / `TestAllSQL_NoPrivilegeEscalation` |
| 3.2 | 重複 package は更新（重複作成しない） | `TestUpsertSQL_OnConflictDoUpdate`（ON CONFLICT ... DO UPDATE / `(tenant_id, package_name)`） |
| 3.4 | 空リスト同期は反映件数 0 | `TestUpsert_EmptyList_ReturnsZeroWithoutPool`（nil pool で pool 非接触を確認） |
| 5.1 | 承認済み package 集合の lookup | `TestCollectPackageSet_Multiple` / `TestApprovedPackages_EmptyInput_ReturnsEmptyMapWithoutPool` / `TestApprovedPackagesSQL_TenantPredicate` |

補助テスト: scan 写像（`TestScanTenantAppRow_AllColumns`）/ icon_url NULL 境界
（`TestScanTenantAppRow_NullIconURL`）/ scan error 伝播（`TestScanTenantAppRow_ScanError`）/
行イテレーション DB error→CodeUnavailable（`TestCollect*_IterationError` / `_ScanError`）。

## 検証結果（サマリ）

- `cd backend && go build ./...` — PASS
- `cd backend && go vet ./...` — PASS
- `cd backend && go test ./...` — PASS（全パッケージ ok）
- `go test ./internal/app/...` — PASS（20 テストケース）
- Red→Green 確認: Upsert 早期 return 除去 / `collect` の nil slice 化 / ON CONFLICT 除去の
  各ミューテーションで対応テストが FAIL することを確認済（revert 済）。

## Implementation Notes

### Task 1

- 採用方針: `internal/policy/`（doc/service_types/repository + co-located test）と同一レイヤ規約で
  `tenant_apps`（migration 0008 / RLS 0011）の永続化層を実装。新規 migration は作らない。
- 重要な判断:
  - **rowsScanner seam の新設**: policy の単一行 `rowScanner` に加え、List/ApprovedPackages の
    行イテレーションを実 DB 非依存にテストするため複数行 seam `rowsScanner`（Next/Scan/Err/Close、
    pgx.Rows が満たす）を導入し、本体を `collectTenantAppRows` / `collectPackageSet` へ委譲した。
    `var _ rowsScanner = (pgx.Rows)(nil)` の compile-time check で配線乖離を build で検出する。
  - **uuid 採番**: 0008 の id 列に DEFAULT が無いため Upsert の新規行 id は `uuid.New()` で採番。
    ON CONFLICT 時は EXCLUDED.id は使われず既存行が更新される（重複作成なし / Req 3.2）。
  - **空入力の早期 return**: Upsert(空) は tx を開かず `(0, nil)`、ApprovedPackages(空) は非 nil 空 map を
    早期 return（不要な DB 往復回避 / Req 3.4）。nil pool を渡すテストで pool 非接触を確認。
  - **アトミック性**: Upsert は単一 tx 内で全件を順次 upsert し RowsAffected を積算。1 件でも失敗すれば
    BeginTxFunc が rollback し `(0, CodeUnavailable)` を伝達（部分反映を残さない / Req 3.6）。
  - **SQL の named const 化**: policy の `selectPolicyColumns` に倣い List/Upsert/ApprovedPackages の
    SQL を package-level const にし、tenant_id 述語・ON CONFLICT・非昇格を静的検証可能にした。
- 残存課題: なし（service.go / handler.go / cmd 配線は後続 task 2〜6 の責務）。
  Upsert の反映件数は `RowsAffected` 積算（ON CONFLICT DO UPDATE は 1/行）で算出する方針を後続
  task 3（SyncApps）が前提にする。

### Task 2

- 採用方針: `internal/policy.Service`（段階拡張 interface / consumer-defines-interface / NewService DI /
  logDeny）と同一規約で App Service の webToken 発行（CreatePlayToken）とカタログ参照（ListApps）を実装。
- 重要な判断:
  - **interface を 2 メソッドに限定**: policy が task 間で interface を段階拡張したのと同方針で、
    SyncApps（task 3）/ CheckAppsApproved（task 4）は先取り定義せず本 task では CreatePlayToken /
    ListApps のみ宣言。`var _ Service = (*service)(nil)` の compile-time check で乖離を検出する。
  - **consumer interface の置き場所**: `webTokenClient`（CreateWebToken のみ）/ `enterpriseResolver`
    （EnterpriseNameForTenant のみ）を service.go に最小 port として定義。amapi.Client / tenant.Service が
    満たす。`eventRecorder`（audit）は先取りせず task 3 で NewService に追加する（本 task では未導入）。
  - **NewService の deps 順**: `(repo, client, tenants, log)`。policy 最終形 `(repo, client, recorder,
    tenants, log)` から recorder を除いた形にし、task 3 が recorder を client と tenants の間へ自然に
    挿入できるようにした。
  - **Value ログ非出力（NFR 1.1）**: 成功パスは一切ログを出さず PlayTokenView 経由でのみ Value を返す。
    エラーパスは logDeny で deny_reason のみ記録（Value を渡さない）。fake logger の `leaks()` で全ログ
    エントリ（msg + field 値）に Value が surface しないことを assert。Red→Green は success 経路へ
    `s.log.Info("...", "value", token.Value)` を混ぜると FAIL することを確認済（revert 済）。
  - **row→view 写像の置き場所**: `rowToView`（service.go の非公開関数）で TenantAppRow→TenantAppView。
    id / tenant_id を外部露出せず package_name / title / icon_url（nullable は null 保持）/ approved_at のみ返す。
  - **parent_frame_url 空検査**: `strings.TrimSpace(...) == ""` で空 + 空白のみを 400 に倒す（amapi
    requireEnterpriseName / policy name 検証と同じ TrimSpace 方針）。この経路では resolver / AMAPI を
    呼ばないことを fake の callCount で確認（Req 1.2）。Red→Green は条件を無効化すると Req 1.2 テストが
    FAIL することを確認済（revert 済）。
- 残存課題: task 3（SyncApps）で NewService へ `eventRecorder` を追加し interface を SyncApps へ拡張する
  想定。task 5（Handler）は本 Service の 2 メソッドを own-tenant RBAC 配下で呼ぶ（actor は claims 由来を渡す）。
  CreatePlayToken の `actor` 引数は現状 logDeny のみで使用（監査は webToken 発行では行わない / design 確認事項 4）。

### Task 3

- 採用方針: policy.Service（段階拡張 interface / consumer-defines-interface / record helper）と同一規約で
  App Service にカタログ同期 `SyncApps` と `app_sync` 監査を追加。NewService を task 2 learning どおり
  `(repo, client, recorder, tenants, log)`（policy 最終形と同順）へ拡張し recorder を挿入。
- 重要な判断:
  - **監査 helper は単一 `record`**: policy は event 種別が複数（create/update/delete/assign）で record/emitAudit を
    分割するが、App は `app_sync` 単一種別のため helper を 1 本に集約（投機的に emitAudit を分けない）。
  - **SyncedAt は `time.Now()` 直接**: app package に clock seam は無く、seam 新設は本 task `_Boundary`（service.go /
    service_test.go）外の新規ファイルになるため避けた。テストは `IsZero()` の tolerant assert で決定的に検証。
  - **enterprise_name は bind gate 専用**: SyncApps は client-relayed 承認結果を Upsert するため resolver の
    戻り値（enterprise_name）は使わず破棄。未バインドのみを弾く gate として機能させ、Detail/ログにも載せない（NFR 3.2）。
  - **空 package_name/title は 400 ガード**: `_Requirements:_` に紐付く numeric AC は無いが、tenant_apps.title の
    NOT NULL 制約違反を未然に防ぐ入力ガードとして実装（Req 3.1 の「有効な承認結果を反映」の異常系）。Upsert 未呼び出し + 失敗監査。
- AC 担保テスト（`internal/app/service_test.go` / `TestService_SyncApps`）:
  - Req 3.1: 「正常同期…Upsert へ委譲」（apps relay + count）/ 「package_name が空→400」「title が空→400」（入力ガード異常系）
  - Req 3.3: 「正常同期…反映件数と非 zero の同期時刻」/「正常同期…成功監査」
  - Req 3.4: 「空リスト同期…count 0 を正常応答」
  - Req 3.5: 「未バインド…Upsert を呼ばず error 伝達 + 失敗監査」
  - Req 3.6: 「Repository が error…count 0 で伝達 + 失敗監査」
  - NFR 3.1: 「成功監査記録」/「Record 失敗でも成功結果を覆さず WARN」（成否両経路で record）
  - NFR 3.2: 「監査 Detail に enterprise_name 等の機密値を含めない」（Detail key は count/result のみ）
  - Red→Green: bind gate 除去 / 空検査除去 / Record 失敗時 WARN 抑止の各ミューテーションで対応テスト FAIL を確認済（revert 済）。
- 残存課題: task 4（CheckAppsApproved）で interface を第 4 メソッドへ拡張する想定（本 task では `var _ Service` は 3 メソッド）。
  Handler（task 5）は SyncApps を ActionUpdate + own-tenant RBAC 配下で呼び、`{synced_at, count}` を返す。

### Task 4

- 採用方針: policy.Service の段階拡張 interface と同方針で App Service に承認済みアプリ read seam
  `CheckAppsApproved` を追加し、interface を最終形（4 メソッド）へ確定した。
- 重要な判断:
  - **空入力の早期 return**: `len(packageNames)==0` は検証対象なしとして Repository を呼ばず `nil` を返す
    （実 Repository も空入力で pool へ触れない契約と整合 / 不要な DB 往復回避）。fake の呼び出し捕捉で 0 回を assert。
  - **越境 package の未承認扱い**: Repository.ApprovedPackages は tenant-scoped（RLS + tenant_id 述語）で
    自テナント承認済みのみを返すため、越境 package は集合に現れず、走査時の欠落判定で自動的に未承認扱いになる
    （越境検出用の特別分岐を持たない / Req 4.x / 存在有無を露出しない Req 4.2）。
  - **DB error を握り潰さない**: ApprovedPackages の error（CodeUnavailable/503 等）はそのまま伝達し、
    承認判定不能を ErrAppNotApproved(422) へ化けさせない。専用テストで 503 保持を確認。
  - **sentinel は read-only**: `ErrAppNotApproved`（service_types.go 定義）を再代入せず直接返す（422 / Req 5.2）。
  - **read seam のため監査なし**: ListApps と同じ read 方針で record/logDeny を呼ばない。
  - **interface 最終形化**: interface doc コメントと `var _ Service = (*service)(nil)` の compile-time check
    コメントを「段階拡張 / 本 task 3 では 3 メソッド」から 4 メソッド最終形へ更新した（service.go 内 = boundary 内）。
- AC 担保テスト（`internal/app/service_test.go` / `TestService_CheckAppsApproved`）:
  - Req 5.1: 「全 package が承認済み→nil / ApprovedPackages へ委譲（packageNames 捕捉）」/「DB error を握り潰さず伝達」
  - Req 5.2: 「1 件未承認→ErrAppNotApproved(422)」/「越境 package→未承認扱いで 422」
  - 境界値: 「空入力（nil / 空 slice）→nil かつ Repository 非呼び出し」
  - Red→Green: 未承認検出ロジック無効化で未承認/越境テスト FAIL、空入力早期 return 無効化で空入力テスト FAIL を確認済（revert 済）。
- 残存課題: task 5（Handler）/ task 6（cmd 配線）は本 Issue 内。CheckAppsApproved の enforcement 配線
  （Policy Service #40 が本 seam を呼ぶ改修）は本 Issue スコープ外（design 確認事項 3）。

### Task 5

- 採用方針: `policy.Handler`（authorize gate / decodeJSON / WriteHTTP 写像 / co-located test の fake +
  httptest）と `tenant.Handler`（`Mount(r chi.Router)` route 登録）を組み合わせた hybrid で App Handler を実装。
- 重要な判断:
  - **Mount パターンの選択**: 3 endpoint が別々のトップレベルパス（`/play-tokens` / `/apps` / `/apps/sync`）を
    持つため、policy の「embedded router + `Mount("/prefix", h)`」ではなく tenant の `Mount(r chi.Router)` を採用し、
    渡された router へ root 相対で直接登録。task 6 が `appHandler.Mount(routers.API)` で `/api` chain へ配線する。
  - **svc は package の Service（4 メソッド）を受ける**: policy と同じく package-level `Service` interface を
    受け、Handler 用の subset port は新設しない（余計な interface 面を増やさない）。CheckAppsApproved は
    endpoint へ配線しない read seam のため fake でのみ実装（呼び出されない）。
  - **RBAC 対応**: play-tokens=ActionRead / apps=ActionRead / apps/sync=ActionUpdate（Resource=ResourceApp）。
    既存 `authz/permissions.go` マトリクスに整合（Operator/Viewer は app:update 無 → sync 403、TenantAdmin は許可、
    Viewer は read 系許可）。マトリクスは変更しない。
  - **NFR 1.2 の Handler 境界検証**: 成功パスは無ログ。fake logger の `leaks()` で webToken.Value が
    どのログエントリにも surface しないことを assert。
- AC 担保テスト（`internal/app/handler_test.go`）:
  - Req 1.1: `TestHandler_CreatePlayToken_Viewer_Returns200`（value 返却 + actor/tenant 委譲）/ `_NotBound_Returns422` / `_UpstreamError_Returns502`
  - Req 1.2: `TestHandler_CreatePlayToken_MalformedJSON_Returns400`（svc 未呼出）
  - Req 2.1 / 2.3 / 4.2: `TestHandler_ListApps_Viewer_Returns200OwnTenant`（own-tenant 委譲 + 一覧返却）
  - Req 3.3: `TestHandler_SyncApps_TenantAdmin_Returns200`（`{synced_at,count}` + actor/tenant 委譲）
  - Req 4.1: `TestHandler_SyncApps_Operator_Returns403` / `_SyncApps_Viewer_Returns403` / `_ListApps_NoClaims_Returns401` / TenantAdmin sync 許可（f）/ Viewer read 許可（a,e）
  - NFR 1.2: `TestHandler_CreatePlayToken_DoesNotLogTokenValue`
  - Red→Green: authorize deny 分岐無効化で sync 403 テスト FAIL、CreatePlayToken の WriteHTTP 除去で 422/502 テスト FAIL を確認済（revert 済）。
- 残存課題: task 6（cmd/api 配線 + `buildAppHandler` 回帰検知）/ task 6.1（real PG RLS 統合テスト）は本 Issue 内の後続。

### Task 6

- 採用方針: `buildPolicyHandler` / `buildTenantRecorder` の testability 方針をそのまま踏襲し、
  `cmd/api/main.go` に app domain bootstrap ブロックと `buildAppHandler` helper を追加、`main_test.go` に
  本番配線の型レベル退行検知テストを追記した。
- 重要な判断:
  - **共有インスタンス再利用**: `buildAppHandler(pool, amapiClient, auditSvc, authorizer, tenantSvc, log)` は
    (7)(8) で構築済みの共有ラッパをそのまま受け取り新規構築しない。`app.NewService(repo, amapiClient,
    auditSvc, tenantSvc, log)` は task 2/3 learning の deps 順（repo, client, recorder, tenants, log）に一致し、
    呼び出し形は `policy.NewService` と同一（NFR 2.1: AMAPI 反映は共有ラッパ経由）。
  - **Mount パターン**: App Handler は 3 endpoint が別々のトップレベルパスを持つため `appHandler.Mount(routers.API)`
    で root 相対に登録（policy の `Mount("/prefix", h)` ではなく tenant.Handler.Mount 型 / task 5 learning と整合）。
    `routers.API` は policy/audit が Mount する /api chain（TenantContextMiddleware = RLS tenant-scoped）と同一（Req 4.1）。
  - **bootstrap 番号**: 既存 (7)〜(10) domain + (11) ListenAndServe の連番に app を挿入するため、app を (11)、
    ListenAndServe を (12) へ繰り上げた（tasks.md 本文は「(12) app domain」と表記するが、ListenAndServe より
    後段に配置すると server ループ後で到達不能になるため、sequential 順で app を先に置いた。確認事項参照）。
- AC 担保テスト（`cmd/api/main_test.go` / `TestBuildAppHandler_WiresAppDomainNotStub`）:
  - Req 4.1 / NFR 2.1: `buildAppHandler` を本番と同じ実型（amapi.Client / audit.Service / *authz.Authorizer /
    tenant.Service）で呼び、戻り値が非 nil の `*app.Handler` であることを assert（App domain 配線が stub/interim へ
    退行していないことを型レベルで回帰検知）。Red→Green: `buildAppHandler` を `return nil`（DI 欠落）へ変えると
    `h == nil` fatal で FAIL することを確認済（revert 済）。
- 残存課題: task 6.1（real PG RLS 統合テスト / list・sync・approved-check の越境不可視 + Upsert 冪等性）は
  deferrable（`- [ ]*`）でスコープ外。本 task で HTTP 経路の実配線が完了したため 6.1 の統合テストが配線後 API を叩ける。

## 確認事項

- design.md / requirements.md との矛盾は認めなかった。design の確認事項（Option A の
  カタログ取得経路 / webToken 有効期限 / Req5 enforcement 配線 / 監査粒度）は人間レビュー済みの
  前提で、本 task 1（永続化層）のスコープには影響しない。

STATUS: complete
