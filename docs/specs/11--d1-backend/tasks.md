# Implementation Plan

> 本タスク群は umbrella #24 task 11.1（App Service）/ Requirement 7 を、本 spec の App backend
> 要件（Requirement 1〜5 + NFR）へ focus した実装単位である。`_Requirements:_` は本 spec
> （`docs/specs/11--d1-backend/requirements.md`）の numeric ID を指す。参照モデルは `internal/policy/`。
> 各タスクは対応する co-located `*_test.go` を同一タスク内に含む（テスト後続 deferred は 6.1 のみ）。

- [x] 1. App Repository とパッケージ雛形（tenant_apps 永続化層）
  - `internal/app/doc.go`: パッケージ doc + 依存方向ルール（`policy/doc.go` に倣い、cmd を import しない /
    Handler のみ httpserver を import / amapi・tenant・audit・authz・errors・logger のみ許可）
  - `internal/app/service_types.go`: `PlayTokenRequest{parent_frame_url}` / `PlayTokenView{value}` /
    `SyncRequest{apps}` / `SyncApp{package_name,title,icon_url?}` / `SyncResult{synced_at,count}` /
    `TenantAppRow` / `TenantAppView`（JSON tag 付き）、sentinel `ErrAppNotApproved`（CodeBusinessRule / 422）
  - `internal/app/repository.go`: `db.BeginTxFunc` で tenant-scoped context のまま操作し RLS に分離を委ねる
    （SuperAdmin 昇格しない / `policy/repository.go` と同型）。新規 migration は作らず 0008 の `tenant_apps` を使う
    - `List`: `WHERE tenant_id=$1 ORDER BY approved_at` で非 nil 空 slice を返す（Req 2.2）。RLS + tenant_id 述語で
      他テナント行を返さない（Req 2.3）
    - `Upsert`: 複数 `SyncApp` を **単一 tx** で `INSERT ... ON CONFLICT (tenant_id, package_name) DO UPDATE SET
      title, icon_url, approved_at=now()` し反映件数を返す（Req 3.2 重複更新 / Req 3.4 空リストは 0 件）
    - `ApprovedPackages`: packageNames のうち自テナントで承認済みの package 集合を返す（Req 5.1）
    - DB 失敗は `errors.Wrap(CodeUnavailable, ...)`（503）へ写像
  - co-located `repository_test.go`（`rowScanner` seam / `policy/repository_test.go` と同型）: scan 写像 /
    List が非 nil 空 slice（Req 2.2）/ Upsert SQL に `ON CONFLICT ... DO UPDATE` を含む（Req 3.2）/ Upsert([])→0 件
    （Req 3.4）/ 各クエリに tenant_id 述語を含み SuperAdmin 昇格しない（Req 2.3）/ ApprovedPackages 集合写像
    （Req 5.1）/ DB error→CodeUnavailable
  - _Requirements: 2.2, 2.3, 3.2, 3.4, 5.1_
  - _Boundary: AppRepository, AppService_

- [x] 2. App Service — webToken 発行とカタログ参照（CreatePlayToken / ListApps）
  - `internal/app/service.go`: `Service` interface（本タスクでは `CreatePlayToken` / `ListApps` を定義）+
    consumer-defines-interface（`webTokenClient`＝amapi.Client / `enterpriseResolver`＝tenant.Service）+ `NewService` DI。
    authz は持たない（Handler の責務 / `policy/service.go` と同方針）
  - `CreatePlayToken`: (1) parent_frame_url 空検査 → CodeInvalidRequest(400)（Req 1.2）、(2)
    `enterpriseResolver.EnterpriseNameForTenant` で enterprise 解決・未バインドは error 伝達（422 / Req 1.3）、
    (3) `webTokenClient.CreateWebToken` で発行し `PlayTokenView{value}` を返す（Req 1.1）。AMAPI 由来 error は
    #34 正規化済みをそのまま伝達（502 等 / NFR 2.1）。Value を構造化ログに出さない（NFR 1.1）
  - `ListApps`: `Repository.List` へ委譲し `[]TenantAppView` を返す（Req 2.1）。read 監査なし
  - co-located `service_test.go`: CreatePlayToken 正常（value 返却 / Req 1.1）/ 空 parent_frame_url→400（Req 1.2）/
    未バインド→error 伝達（Req 1.3）/ AMAPI error→伝達（NFR 2.1）/ fake logger で Value がログに出ないこと（NFR 1.1）/
    ListApps が委譲し空 slice を返す（Req 2.1）
  - _Requirements: 1.1, 1.2, 1.3, 2.1, NFR 1.1, NFR 2.1_
  - _Boundary: AppService_
  - _Depends: 1_

- [x] 3. App Service — カタログ同期（SyncApps + 監査）
  - `internal/app/service.go`: `Service` interface を `SyncApps` へ拡張（`policy` が task 間で interface を
    拡張したのと同方針）+ consumer-defines-interface `eventRecorder`（＝audit.Service）を追加
  - `SyncApps`: (1) `enterpriseResolver.EnterpriseNameForTenant` で bind gate・未バインドは upsert せず error 伝達
    （Req 3.5）、(2) 各 SyncApp の package_name/title 空検査（400）、(3) `Repository.Upsert`（単一 tx / client-relayed
    承認結果を反映 / Req 3.1）、(4) `SyncResult{synced_at: now(), count}` を返す（Req 3.3、空リストは count 0 / Req 3.4）
  - Req 3.6: Repository が error を返した場合は反映件数 0 のまま（部分反映を残さず）error を伝達する
  - NFR 3.1: 成否いずれの経路でも `eventRecorder.Record`（event_type=`app_sync`, Detail={count, result}、sync 実行単位の粒度）。
    NFR 3.2: Detail/ログに資格情報・OAuth トークン生値を載せない。Record 失敗は WARN に留めユースケース結果を覆さない
  - co-located `service_test.go`: 正常 count（Req 3.1/3.3）/ 空リスト→count 0（Req 3.4）/ 未バインド→Upsert 未呼び出し +
    error（Req 3.5）/ Repository error→count 0 + 伝達 + 失敗監査（Req 3.6）/ Record 失敗でも成功結果を覆さない
    （NFR 3.1）/ 監査 Detail に機密値を含まない（NFR 3.2）
  - _Requirements: 3.1, 3.3, 3.5, 3.6, NFR 3.1, NFR 3.2_
  - _Boundary: AppService_
  - _Depends: 2_

- [x] 4. App Service — 承認済みアプリ read seam（CheckAppsApproved / Requirement 5）
  - `internal/app/service.go`: `Service` interface を `CheckAppsApproved(ctx, tenantID, packageNames) error` へ拡張
  - `Repository.ApprovedPackages` で自テナント承認済み集合を取得し（Req 5.1）、packageNames に未承認が 1 件でも
    あれば `ErrAppNotApproved`（422）を返す（Req 5.2）。tenant-scoped（自テナント境界のみ / RLS）
  - Policy Service（#40）が消費する read seam。**enforcement 配線（Policy が本 seam を呼ぶ改修）は本 Issue 外**
    （design 確認事項 3）。本 seam は本タスク内で直接テストする公開契約とし投機的抽象化にしない
  - co-located `service_test.go`: 全承認→nil（Req 5.1）/ 1 件未承認→ErrAppNotApproved(422)（Req 5.2）/ 越境 package→
    未承認扱い / 空入力→nil
  - _Requirements: 5.1, 5.2_
  - _Boundary: AppService_
  - _Depends: 3_

- [x] 5. App Handler — 3 endpoint + RBAC + エラー写像
  - `internal/app/handler.go`: `Mount(r chi.Router)` で `POST /play-tokens`（ActionRead）/ `GET /apps`（ActionRead）/
    `POST /apps/sync`（ActionUpdate）を登録（`tenant.Handler.Mount` パターン）。`AuthClaimsFromContext` →
    `authz.Authorizer.AuthorizeAndLog`（`ResourceApp`, `AudienceTenantConsole`, `TargetTenantID=claims.TenantID`）で
    own-tenant 判定（`policy.Handler.authorize` と同方式）。claims 不在→401、deny→403（Req 4.1）
  - body decode は `decodeJSON`（malformed / 空→400）。Service の `*errors.Error`（tenant 422 / AMAPI 502 / DB 503）は
    `errors.WriteHTTP` で Code→HTTP 写像。存在差は sentinel/汎用 message に委ね露出しない（Req 4.2 / NFR 1.2）
  - Value をログに出さない（NFR 1.2 / NFR 1.1 と整合）
  - co-located `handler_test.go`: 各 endpoint の RBAC allow/deny（Operator の sync→403 / TenantAdmin の sync→許可 /
    Viewer の play-tokens・apps→許可 / Req 4.1）/ claims 不在→401 / body decode 400（Req 1.2）/ 未バインド 422・
    AMAPI 502 の status 写像（Req 1.1）/ `GET /apps` が Service へ own-tenant で委譲し一覧を返す（Req 2.1 / 2.3 / 4.2）/
    sync 成功で `{synced_at,count}` を返す（Req 3.3）
  - _Requirements: 1.1, 1.2, 2.1, 2.3, 3.3, 4.1, 4.2, NFR 1.2_
  - _Boundary: AppHandler_
  - _Depends: 4_

- [x] 6. cmd/api への配線と本番 DI 回帰検知
  - `backend/cmd/api/main.go`: bootstrap に (12) app domain ブロックを追加。`buildAppHandler(pool, amapiClient,
    auditSvc, authorizer, tenantSvc, log)` helper を新設（`buildPolicyHandler` に倣い、既存共有インスタンス
    〔amapiClient / auditSvc / authorizer / tenantSvc〕を **再利用** し新規構築しない）。AMAPI 反映は共有ラッパ経由（NFR 2.1）
  - `appHandler.Mount(routers.API)` で `/api/play-tokens` / `/api/apps` / `/api/apps/sync` を `/api` chain
    （TenantContextMiddleware による RLS tenant-scoped）へ配線（Req 4.1）
  - `backend/cmd/api/main_test.go`: `buildAppHandler` の本番配線退行を型レベルで回帰検知（`buildPolicyHandler` /
    `buildTenantRecorder` と同 testability 方針）
  - _Requirements: 4.1, NFR 2.1_
  - _Boundary: AppHandler, AppService_
  - _Depends: 5_

- [ ]* 6.1 RLS テナント分離と Upsert 冪等性の統合テスト（real PG / coverage 補完）
  - real PostgreSQL 上で越境テナントの `tenant_apps` が list/sync/approved-check で不可視（Req 4.1 / 4.2）を検証。
    task 1（tenant_id 述語）/ task 5（RBAC deny）の unit テストを real RLS で補完する coverage 補完タスク
  - 同一 package の二重同期で 1 行・title 更新（Req 3.2 の ON CONFLICT 冪等性）を実 DB で確認
  - E2E / 統合スコープに限定し、先行タスクの per-task 判定に影響しない範囲に留める
  - _Requirements: 3.2, 4.1, 4.2_
  - _Boundary: AppRepository_
  - _Depends: 6_

## Verify

本 spec の実装後、watcher（stage-a-verify gate）が再実行すべき build/test/lint コマンドを宣言する。
対象パスは `tasks.md` commit 時点で存在する module ルート（`backend/` の `./...`）に限定する（#364）。

<!-- stage-a-verify -->
```sh
cd backend && go build ./... && go vet ./... && go test ./...
```
