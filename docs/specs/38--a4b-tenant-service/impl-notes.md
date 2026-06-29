# 実装ノート（Issue #38 / A4b Tenant ドメイン Service）

per-task ループで各 task の learning を追記する補足ノート。`requirements.md` /
`design.md` / `tasks.md` の本文は書き換えず、実装上の判断と申し送りのみを記録する。

## Implementation Notes

### Task 1

- **採用方針**: `internal/tenant` パッケージを `internal/auth` と同パターンで scaffold し、ドメイン型・DTO・監査記録ポート（`EventRecorder`）と logger 実装（interim）を `types.go` / `audit_log.go` に分離して追加した（`_Boundary: tenant.types, tenant.EventRecorder_` に厳密に限定。service/repository/handler/migration は後続 task へ）。
- **重要な判断**:
  - `TenantRow.EnterpriseName` は `*string` ではなく `string`（空文字 = 未設定）で表現し、Repository（task 3）が NULL→空文字へ写像する前提とした。`TenantView` は `enterprise_name,omitempty` で bound 時のみ field 出力し、nil ポインタ分岐を回避。
  - NFR 2.3 を **構造的に**担保: `Event` 構造体に機密値フィールドを一切持たせないことで、logger 側 redact allowlist に依存せず秘密値非出力を成立させた（`LoggerRecorder.Record` は operation/result/actor_id/tenant_id/confirmation_completed と拒否時 deny_reason のみ emit）。
  - sentinel error は `*errors.Error`（Code 付き）の read-only package-level var として定義。Code 写像は `ErrConflict`→409 / `ErrInvalidState`・`ErrConfirmationRequired`・`ErrNotBound`・`ErrTenantDisabled`→422 / `ErrTenantNotFound`→404。message は Req 6.5（存在差非露出）に従い汎用文言。
  - `EventRecorder.Record` は将来の Audit Service（永続化失敗あり得る）拡張に備え `error` を返す形。interim の logger 実装は常に nil。
- **残存課題（後続 task への申し送り）**:
  - sentinel error 群は task 4/5（Service）で `errors.Is` 比較 / `errors.Wrap` の cause 付与に利用する。
  - `SignupURL.Name`（`json:"-"`）は task 5（Bind）で `CreateEnterprise` の引数として使用する。
  - `TenantRow.EnterpriseName` の NULL→空文字写像は task 3（Repository）の責務。

### Task 2

- **採用方針**: マイグレーション 0016 は `tenants` 本体を再作成せず ALTER のみで、無効化監査列 `disabled_at timestamptz` / `disabled_by uuid` を `ADD COLUMN IF NOT EXISTS` で冪等追加し、`enterprise_name` の部分一意 index `uq_tenants_enterprise_name`（`WHERE enterprise_name IS NOT NULL`）を `CREATE UNIQUE INDEX IF NOT EXISTS` で追加した（0001 / 0011 は不変 / NFR 3.1）。
- **重要な判断**:
  - **部分一意 index 採用理由**: `enterprise_name` は pending_bind 行で NULL のため、通常の UNIQUE 制約だと未バインド行同士が一意衝突する。部分 index（`WHERE enterprise_name IS NOT NULL`）にすることで bound 済みの Enterprise 名のみを一意化し、二重バインドを DB 層で防止（Req 2.1 invariant 補強）。
  - **down の DROP 順序**: index → 列の順で落とし、列は up の追加と逆順（disabled_by → disabled_at）で揃えた。golang-migrate は down を逆順実行するため 0016 down は列・index のみ落とし、tenants テーブルや tenant_status enum は触らない（0001 down の責務）。
  - **新規テスト不要**: 本 task の往復検証は既存 `backend/test/integration/migrations_reversible_test.go` が up→down→up と冪等 no-op で担う。0016 は `tenants` への ALTER のみ（テーブル新設なし）のため `primaryTables` 変更不要。enterprise_name 一意制約違反（23505）の実 DB 検証は後続 task 3 の integration test の責務であり本 task では追加しない（tasks.md L22 / design.md「テスト配置の根拠」が正典）。
- **残存課題（後続 task への申し送り）**:
  - task 3（Repository）の `UpdateBound` は、別 tenant への同一 enterprise_name 投入時に本 index 違反（pgerrcode 23505）を `CodeConflict` へ写像する前提（tasks.md L33）。
  - task 3 / task 5 の `UpdateDisabled` が `disabled_at=now()` / `disabled_by=$` を書き込む列は本 0016 で用意済み。
  - `disabled_at` / `disabled_by` は NULL 許容で bound/pending_bind 行は NULL のまま。TenantRow への写像（task 1 で `EnterpriseName` を `string` で表現した方針）と整合する形で task 3 が読み出す。

### Task 3

- **採用方針**: `internal/tenant/repository.go` に `Repository` interface（Insert/Get/List/UpdateBound/UpdateDisabled）と pgxpool 実装を追加。`internal/auth/repository.go` の `superAdminContext` + `db.BeginTxFunc` + `pgx.ErrNoRows`/`pgconn.PgError`+`pgerrcode` 判定 + `pkgerrors.Wrap` 写像パターンを忠実に踏襲した。結合テストは `backend/test/integration/tenant_repository_test.go`（`helpers_test.go` 流用 / DB env 未設定なら t.Skip）。
- **重要な判断**:
  - **NULL ⇔ 空文字写像**: `Insert` は `nullableEnterpriseName`（空文字→nil）で enterprise_name を NULL 投入し、`Get`/`List` は `*string` で受けて NULL→空文字へ写像する（task 1 の `TenantRow.EnterpriseName string` 方針と整合）。`scanTenantRow` を Get/List で共有し走査ロジックを単一化。
  - **競合は条件付き UPDATE の affected で表現**: `UpdateBound`(WHERE status='pending_bind') / `UpdateDisabled`(WHERE status!='disabled') は affected を返すのみ。二重 bind / 二重 disable は affected=0 で表現し、409/422 への HTTP 写像は後続 Service task の責務（本 task は affected=int64 を返すところまで）。`updated_at = now()` も併せて更新。
  - **23505 のみ CodeConflict、他 DB エラーは CodeUnavailable**: `UpdateBound` の部分一意 index 違反（`uq_tenants_enterprise_name`）のみ `pgerrcode.UniqueViolation` で `CodeConflict` に写像。それ以外の DB エラーは fail-closed で `CodeUnavailable` に wrap。機密値は message に補間しない（auth の慣習）。sentinel error（types.go の `ErrTenantNotFound` 等）は本 task では直接返さず、auth と同じく `pkgerrors.Wrap` で同 Code を返す wrap 派を採用（cause を保持して errors.As で Code 判定可能）。
  - **テナント分離テストの検証方法**: Repository は内部で常に SuperAdmin context を確立する設計のため、非 SuperAdmin の可視性は pool 直叩き（`platformdb.BeginTxFunc` + 別 tenant 文脈）で `SELECT COUNT(*) FROM tenants WHERE id=$` が 0 件になることを RLS 経由で検証した（`db_tenant_isolation_test.go` の既存パターン流用）。
- **残存課題（後続 task 4 Service への申し送り）**:
  - `Get` の不在は `CodeNotFound`、`UpdateBound` の 23505 は `CodeConflict` を Repository が既に写像済み。Service は affected=0（bind/disable）を 409/422 へ写像する責務を負う（bound 重複=409 / disabled へ bind=422 等は Service の状態判定で分岐）。
  - `UpdateBound`/`UpdateDisabled` は affected を返すのみで、現状態の判定（bound/disabled/不在の区別）は Service が `Get` 経由で行う設計（design.md L244-265 Bind シーケンス図と整合）。
  - `List` は created_at 昇順固定・全件返却（ページングなし。requirements Open Question「テナント一覧のページング」は MVP 全件で据え置き）。

### Task 4

- **採用方針**: `internal/tenant/service.go` に Tenant Service を `internal/auth.NewService` と同型の「interface 公開 + 本番実装 struct + deps 注入 + コンストラクタ」パターンで実装し、本 task のスコープである Create / Get / List / EnterpriseNameForTenant の 4 メソッドのみを `Service` interface に宣言・実装した。`amapi.StubClient` + fake Repository/Recorder/Logger を使う in-package 単体テスト（`service_test.go`）を同 commit に含めた。
- **重要な判断**:
  - **interface に Bind/Disable を含めるか**: prompt の推奨どおり **後者（task 4 では 4 メソッドのみ interface 宣言）** を採用した。Bind/Disable は task 5 で interface に追記・実装する。これによりスタブのプレースホルダ（`CodeInternal` 等を返す未実装メソッド）を本番実装 struct に置かずに済み、「task 4 では実装しない」スコープと一致してクリーン。**task 5 への申し送り**: task 5 は `Service` interface に `Bind(ctx, id, BindInput) (TenantView, error)` / `Disable(ctx, id, DisableInput) (TenantView, error)` を追記し、`service` struct にメソッドを実装する（design.md L221-222 のシグネチャに従う）。
  - **actor の受け渡し**: design.md の Service 疑似シグネチャ（`Create(ctx, in CreateInput)`）は actor 引数を持たないが、`tenant` package は doc.go の依存方向ルールで `httpserver` import を **禁止**（許可は amapi/db/logger/errors/config のみ）しているため、Service が ctx 経由で `httpserver.AuthClaimsFromContext` を呼ぶことは不可。よって監査 Event の実行者は **`Create(ctx, actor uuid.UUID, in CreateInput)` の明示引数**として受け取る形にした（Handler が `AuthClaimsFromContext` で取得して渡す / Repository の `UpdateDisabled(ctx, id, actor)` が既に explicit actor を取る前例と整合）。これは design 疑似シグネチャからの最小の逸脱であり、確認事項にも記載した。**task 5 への申し送り**: Bind/Disable も同様に actor を明示引数で受け取る設計が一貫する（Disable は Repository.UpdateDisabled に actor を渡す必要があるため必須）。
  - **AMAPI error はそのまま伝播**: `CreateSignupURL` の error は #34 が Code 正規化済み（4xx=非 transient / 429・5xx=CodeUpstream+transient）のため Service では再分類せずそのまま返す（design.md Error Strategy / Req 1.4）。`Repository.Insert`(pending_bind) を `CreateSignupURL` より**先**に実行するため、URL 生成が失敗（または空応答）してもテナントは pending_bind 行のまま残り、Req 1.4 の「バインド未完了状態のまま保持」を満たす（後続レビュー指摘 #51 を受けて Insert 先行の順序へ確定。Create 実装の実順序と本ノートを整合させた）。
  - **拒否経路の構造化ログ（NFR 2.2）**: name 空 / pending_bind ガード / disabled ガードで `s.log.Warn("tenant operation denied", actor_id, tenant_id, deny_reason)` を出す。EnterpriseNameForTenant の前提ガードは actor を引き回さない契約（design 疑似シグネチャに actor 無し）のため actor は `uuid.Nil` を渡す。
  - **EnterpriseNameForTenant の default 分岐**: 定義外 status は fail-closed で `ErrInvalidState`（422）を返す（NFR 1.1 / 1.2 の防御。Repository は正規 3 値しか書かないため通常到達しないが安全側）。
- **残存課題（後続 task 5 への申し送り）**:
  - `Service` interface への Bind/Disable 追記と実装は task 5 の責務（上記）。
  - actor の明示引数化は Bind/Disable でも踏襲すること。
  - `config.Config.AMAPIProjectID` は本 task では未使用だが Service struct の deps に含めた（Bind/task 5 が `CreateEnterprise(signupURLName, cfg.AMAPIProjectID)` で使用 / design.md L209）。

### Task 5

- **採用方針**: `internal/tenant/service.go` の `Service` interface に `Bind(ctx, actor, id, in)` / `Disable(ctx, actor, id, in)` を追記し、`service` struct へ状態機械の遷移系を実装した（task 4 と同パターン）。同 commit で `service_test.go` の `fakeRepository` に `UpdateBound`/`UpdateDisabled` の affected 行数・error・呼出記録の制御を追加し、Bind/Disable の単体テストを拡充した。
- **重要な判断**:
  - **actor 引数順は Create と一貫**（`ctx, actor, id, in`）。design.md L221-222 の疑似シグネチャは actor を持たないが、task 4 で actor 明示引数化が確定済み（`httpserver` import 禁止の依存方向ルールのため ctx 経由不可）。本 task もそれを踏襲した。新たな逸脱ではないため確認事項は追記しない。
  - **Bind は AMAPI I/O を tx 外**: `Get`(現状態) → pending_bind のみ `CreateEnterprise` → `UpdateBound`(WHERE status='pending_bind') の順。CreateEnterprise 失敗時は永続化前に return し pending_bind を維持（NFR 1.3）。pending_bind 以外（bound→409 / disabled→422 / 定義外→fail-closed 422）は AMAPI を一切呼ばず拒否し、`CreateEnterprise` 未呼出を保証（Req 2.5 / 2.6 / NFR 1.2）。`UpdateBound` affected=0 は競合として 409 に写像（Req 2.5）、Repository 由来の 23505→CodeConflict はそのまま伝達。
  - **Disable の確認テキスト比較は `in.Confirmation == row.Name` の完全一致**。name 取得のため不一致でも先に `Get` が必要。既に disabled は Get 直後に二重無効化 409、確認不一致は永続化前に 422（`ErrConfirmationRequired`）で拒否。`UpdateDisabled` affected=0 も二重無効化競合として 409（Req 3.4）。成功時 View は `Status=disabled` で enterprise_name を露出しない（TenantView の omitempty / Req 6.5）。
  - **監査 Event の confirmed 意味付け**: Disable のみ `ConfirmationCompleted` を渡す（成功・competition 失敗とも確認テキストを通過した経路では true、確認未完了拒否では false）。Bind は confirmed=false 固定。失敗/拒否経路でも `record` + `logDeny` を Create の前例に合わせて出力。
- **残存課題（後続 task 6 Handler への申し送り）**: Handler は `httpserver.AuthClaimsFromContext` で actor を取得し `Bind(ctx, actor, id, in)` / `Disable(ctx, actor, id, in)` に渡す（Create と同様）。`POST /tenants/{id}/bind` の body は `{signup_url_name}`、`DELETE /tenants/{id}` の body は `{confirmation}`（対象 name 再入力）。Disable 成功時の HTTP 応答は 204（design.md API Contract）であり、Service が返す disabled TenantView は Handler が必要に応じて利用する。

### Task 6

- **採用方針**: `internal/tenant/handler.go` に `Handler` struct（deps: `Service` / `logger.Logger`）+ `NewHandler` + `Mount(r chi.Router)` を `internal/auth/handler.go` の sub-route 方式で実装し、`/tenants` プレフィックス配下に 5 endpoint（POST `/` / GET `/` / GET `/{id}` / POST `/{id}/bind` / DELETE `/{id}`）を登録した。in-package の httptest + fake Service（`fakeTenantService`）による単体テストを同 commit に含めた。
- **重要な判断**:
  - **actor 取得は `httpserver.AuthClaimsFromContext(r.Context()).AdminUserID`**。Task 4/5 申し送りどおり Handler が actor を明示引数で Service へ渡す。claims 不在（guard 未適用の異常経路）は `uuid.Nil` を渡し、認可は guard 層の責務として Handler 本体は止めない（fail-safe）。
  - **HTTP status の組み立てを Handler に持たせない**: 入口の path param UUID parse 失敗 / JSON decode 失敗のみ Handler が `CodeInvalidRequest`(400) を生成し、それ以外は Service/Repository が返す `*errors.Error` を `errors.WriteHTTP` で写像する。404/競合 body は sentinel error の固定 message に委ね、対象 ID を露出しない（Req 6.5）。DELETE 成功は 204、POST `/tenants` 成功は 201（`{id,name,status,signup_url,signup_url_name}` 合成）。`signup_url_name` は後続 `POST /tenants/{id}/bind` の入力（`CreateEnterprise` 引数 / Req 2.1）に必要かつ DB に保存しないため、応答 body に含めて呼び出し側へ返す（後続レビュー指摘 #51 を受けて `json:"-"` 非露出から応答露出へ確定。NFR 2.3 はログ出力への制約であり API 応答への本識別子露出は対象外で、より機微な `signup_url` 本体も応答 body に載せている）。
  - **`tenant` package が `httpserver` を import するのは handler.go（presentation 層）のみ**。design.md L327-328 が「Handler は presentation 層なので `AuthClaimsFromContext` を使う」と明示しているため設計指示を優先して実装した。当初の doc.go 依存方向ルールは `httpserver` を許可リスト外としていたが、#51 round3 で doc.go の許可 import リストに `internal/platform/httpserver`（handler.go 限定）を追記し、現行コードと整合させた（doc.go L20-22）。Service / Repository 層は引き続き httpserver 非依存を維持する（task 4 で actor を明示引数化した理由）。
- **残存課題（後続 task 7 への申し送り）**: 認可ガード継承（未認証 401 / tenant-console aud 403 / 非 SuperAdmin 403 / admin-console+SuperAdmin 2xx）の検証は task 7 の `test/integration` 結合テスト（admin chain 経由）の責務。本 task の handler_test.go は Mount 後の route 解決と Handler 入出力契約のみを検証しており、guard 継承は未カバー（design.md「テスト配置の根拠」と整合）。

### Task 7

- **採用方針**: `backend/test/integration/tenant_admin_guard_test.go`（`package integration_test`）に、`NewServer`（`authMWAdmin=nil` で admin chain を `[TenantContextMiddleware, RequireAdminConsoleAndSuperAdmin]` の 2 段のみ）+ `tenant.Handler.Mount(routers.Admin)` を実物で組み立て、`/api/admin/tenants` に対する認可ガード継承を `httptest.NewRecorder` + `srv.Handler.ServeHTTP` + `WithAuthClaims` 直接注入で検証した（既存 `http_subrouter_mount_test.go` test (j) 方式を AuthClaims 注入版に拡張）。
- **重要な判断**:
  - **claims 注入は HTTP 境界越えできないため `httptest.NewServer` + `http.Get` を使わず `ServeHTTP` 直叩き**にした。401（Req 6.4）は TenantContextMiddleware の default deny、403（Req 6.2 audience 不一致 / Req 6.3 非 SuperAdmin）は RequireAdminConsoleAndSuperAdmin が担い、各ケースが意図した middleware 段で止まることを status の差（401/403/200）で識別している。
  - **fake Service 到達の検証**: integration_test は別パッケージで `tenant` 内 unexported fake を再利用できないため、`tenant.Service`（6 メソッド）を満たす最小 fake をファイル内に定義。正常系は `GET /api/admin/tenants`（→ `Service.List`）で 200 を assert し、`listCalls()` 呼出数でガード通過後の Handler 到達を確認。拒否系は `listCalls()==0` で Service が一切呼ばれないこと（認可は guard 層で完結 / 存在露出防止）も併せて検証した。DB 不要（pool=nil + fake）で無条件に走る。
  - ガード継承の一様性確認のため、別 endpoint（`POST /api/admin/tenants` = create）でも 401/403 が一様に継承されることを table 駆動 subtests で補完した（過剰にせず拒否系のみ）。
- **残存課題**: なし（task 7 完了。Req 6.2 / 6.3 / 6.4 の正常系・拒否系を結合テストで担保）。

## 確認事項

- **Task 4: Service の actor 受け渡しが design 疑似シグネチャから逸脱（実装上の判断）**: design.md L218-226 の `Service` 疑似シグネチャは `Create(ctx, in CreateInput)` のように actor 引数を持たず、L228 Preconditions で「ctx に AuthClaims が確立済み」と記す。しかし `tenant` package の doc.go 依存方向ルールは `httpserver` import を禁止（許可: amapi/db/logger/errors/config）しており、Service が ctx から `httpserver.AuthClaimsFromContext` で actor を取り出すことができない。本実装は監査 Event の実行者を **`Create(ctx, actor uuid.UUID, in CreateInput)` の明示引数**で受け取る形に確定した（Repository の `UpdateDisabled(ctx, id, actor)` が既に explicit actor を取る前例と整合し、最小の逸脱）。Handler（task 6）が `AuthClaimsFromContext` で actor を取得して Service へ渡す。design.md / tasks.md は書き換えていない（実装 PR では spec を書き換えない規約に準拠）。Architect 判断が必要なら本点を差し戻し対象として検討されたい。

- **design.md「Modified Files」と現状の差異（task 1 では影響なし）**: design.md は「本 Issue 時点で `cmd/api/main.go` は未存在のため bootstrap 配線は行わない」と記すが、現コードベースには既に `cmd/api` パッケージが存在する（`go build ./...` / `go test ./...` 緑）。task 1 の scaffold（types + audit port）には影響しないが、後続 task 6（`Handler.Mount`）で DI bootstrap の既存状況を再確認すること。`design.md` / `tasks.md` の書き換えは行っていない（実装 PR では spec を書き換えない規約に準拠）。

- **Task 6: Handler が `httpserver` を import（doc.go 依存方向ルールとの整合は round3 で解消済み）**: design.md L327-328 は「actor_id は `httpserver.AuthClaimsFromContext`（`middleware.go:104`）で取得し Service の監査イベントに渡す」「Handler は presentation 層」と明示しており、Handler が actor を取得するには `httpserver` import が必須となる。本実装は design.md の指示を優先し `handler.go` で `internal/platform/httpserver` を import した（`go build` / `go vet` / `go test` 緑、import cycle なし＝ httpserver は tenant を import しないため一方向）。当初 doc.go の許可 import リストは `httpserver` を含んでいなかったが、**#51 round3 で doc.go（コード側ドキュメント）の許可 import リストに `internal/platform/httpserver`（handler.go 限定）を追記し現行コードと整合済み**（doc.go L20-22。実装 PR で書き換え可能なのはコードとコードコメントであり、`design.md` / `tasks.md` の spec 本文は書き換えていない）。これは Service 層の `httpserver` import 禁止（task 4 で actor 明示引数化した理由）とは別レイヤの判断であり、Service は引き続き httpserver 非依存を維持している。design.md 側の依存方向記述の追補が必要かは Architect 判断に委ねるが、コード／doc.go レベルの不整合は解消済み。

STATUS: complete
