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

## 確認事項

- design.md / requirements.md との矛盾は認めなかった。design の確認事項（Option A の
  カタログ取得経路 / webToken 有効期限 / Req5 enforcement 配線 / 監査粒度）は人間レビュー済みの
  前提で、本 task 1（永続化層）のスコープには影響しない。

STATUS: complete
