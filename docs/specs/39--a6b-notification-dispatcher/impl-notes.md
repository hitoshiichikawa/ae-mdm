# 実装ノート（Issue #39 / Notification Dispatcher）

## Implementation Notes

### Task 1

- **採用方針**: design.md 採用案 a に従い、`EnterpriseNameForTenant` の対称メソッドとして
  tenant パッケージに `TenantIDByEnterpriseName(ctx, name) (uuid.UUID, bool, error)` を追加
  （既存 `superAdminContext` + `BeginTxFunc` パターンを踏襲、既存 IF を破壊しない追加メソッド）。
- **重要な判断**:
  - 空 / 空白 enterprise_name は Service 層で DB を叩かず `found=false` を即返す退避経路
    （Req 3.4）+ 拒否ログ（NFR 3.1）。非空は Repository へ委譲。
  - Repository は SuperAdmin context で `WHERE enterprise_name=$1 AND status='bound'` を実行。
    0 件（`pgx.ErrNoRows`）は `found=false`（非エラー / Req 3.2）、それ以外の DB 失敗は
    `*errors.Error{CodeUnavailable, IsTransient:true}` で wrap（Req 1.4 経路 / worker nack）。
  - `IsTransient:true` は既存 `Wrap` が設定しないため、`amapi` / `pubsub` と同様に
    `&errors.Error{...}` をリテラル構築した。
  - 後続の notification [[Task 2]] が要求する `TenantResolver` 最小 IF（逆引き 1 メソッド）の
    契約を満たす Service メソッドを提供済み。
- **残存課題**: なし（notification パッケージ・dispatcher・admin_handler・結合テストは
  task 2 以降の責務で本 task では未着手）。

### Task 2

- **採用方針**: 新規 `internal/notification` パッケージに doc.go / types.go / verifier.go /
  dedupe.go を追加（audit/tenant の per-domain package パターンを踏襲）。
- **重要な判断**:
  - **wire-format 解釈**: spec は AMAPI Pub/Sub 通知の wire-format を確定していないため、
    種別は attribute `notificationType` から、enterprise_name は payload JSON のリソース名
    `name`（`enterprises/{id}/...`）の `enterprises/{id}` 接頭辞から抽出する解釈を採った
    （AMAPI 公式仕様の標準形 / 確認事項参照。task 3/5 がこの契約を消費する）。空 payload /
    種別 attribute 欠落 / JSON 不正は破棄 ack 相当 `*errors.Error{IsTransient:false}`
    （Code=CodeBusinessRule）で分類、enterprise_name 不明は非エラーで空文字通過（退避判定は
    Dispatcher / Req 3.4）。
  - **dedupe の test seam**: DB 依存の実挙動は task 5 結合テストに委ね、unit では pure helper
    3 つ（`wrapDedupePersistErr` の transient 写像 / `mapIsProcessedScan` の bool 写像 /
    `markProcessedSQL`・`isProcessedSQL` の文字列契約 assert）を fake error で検証した
    （audit の SQL 文字列 assert / tenant の fake repo 手法と同型）。
  - **IsTransient リテラル構築**: `errors.Wrap` は IsTransient を立てないため、task 1 の
    `TenantIDByEnterpriseName` 同様 `&errors.Error{Code:CodeUnavailable, IsTransient:true}` を
    リテラル構築した。
- **残存課題**: task 3（UnassignedQueue / Dispatcher）・task 4（admin_handler / cmd/api 配線）・
  task 5（結合テスト）が未着手。dedupe / verifier の実 DB 挙動（ON CONFLICT 冪等・既処理判定の
  実値）は task 5 で検証する。

### Task 3

- **採用方針**: notification パッケージに unassigned.go（UnassignedQueue）/ dispatcher.go
  （Dispatcher）を追加。退避リポジトリは audit.buildSelectQuery / dedupe の test seam を、
  Dispatcher は design.md フロー図（L76〜）と error 写像（ShouldAck の IsTransient 規約）を踏襲した。
- **重要な判断**:
  - **退避経路の dedupe 記録タイミング**: design フロー図の Unassigned 段は「INSERT + dedupe
    記録 + ack」と読めるため、handler 成功後と同様に **退避 INSERT 成功後**に MarkProcessed する
    実装にした（退避 INSERT に冪等制約を置かない本 Issue では、これにより再配信時の二重退避を
    dedupe で抑止する / Req 3.3 と 1.1 の整合）。退避 INSERT 失敗・退避後の dedupe 記録失敗は
    いずれも transient で nack 保持（Req 1.4 / NFR 2.2 = 取りこぼし防止）。
  - **enterprise_name 空の早期退避**: Verifier が空文字で通す wire-format 前提（task 2）に従い、
    Dispatcher は env.EnterpriseName=="" のとき逆引きを **行わず** found=false 相当で退避経路へ
    倒す（Req 3.4 / DB 非アクセス / TenantResolver.err があっても呼ばれないことをテストで担保）。
  - **tenant context 確立**: 解決成功時のみ `db.WithTenantContext(ctx, {TenantID})` を確立して
    から handler を呼ぶ（Req 3.1 / NFR 2.1）。退避経路・未登録種別・既処理は tenant context を
    確立せず、いずれのテナントリソースも更新しない（NFR 2.2）。
  - **jsonb NOT NULL 列**: migration 0010 は `payload jsonb NOT NULL DEFAULT '{}'` / `enterprise_name
    text NOT NULL`。空 payload は payloadOrEmptyJSON で `{}` に、空 enterprise_name は空文字を
    そのまま bind する（audit.emptyJSONObject と同方針 / Req 3.5）。
  - **DB 依存挙動の単体化方針**: dedupe_test.go と同様、実 DB 挙動は task 5 結合テストに委ね、
    unassigned は pure helper（SQL 文字列契約 / buildUnassignedListQuery の WHERE 句構築 /
    payload・transient 写像）を、Dispatcher は mock 5 種（verifier/dedupe/unassigned/resolver/
    handler）で段階分岐 × ack/nack を表駆動検証。ack/nack 判定は errors.ShouldAck を通した。
- **残存課題**: task 4（admin_handler / cmd/api 配線）・task 5（結合テスト）が未着手。
  Dispatcher の subscriber への wire-in は #35 scope 外（暫定 handler 据え置き / design Risk）。
  UnassignedQueue の実 DB 挙動（退避 INSERT / Filter SELECT の実値・received_at 降順）は
  task 5 で検証する。Dispatcher は cmd/worker へ未配線（構築・注入可能な形で提供するに留める）。

### Task 4

- **採用方針**: `internal/notification/admin_handler.go` に chi.Router 内包の `AdminHandler`
  （`NewAdminHandler(queue UnassignedQueue, log)`）を audit.AdminHandler と同型で追加し、
  cmd/api の (9) ブロックで `routers.Admin.Mount("/notifications/unassigned", h)` 配線した。
- **重要な判断**:
  - **authorizer / tenant_id query を持たない**: audit.AdminHandler は cross-tenant の
    `tenant_id` query を authz.Authorizer で二重防御するが、本 endpoint は task 4.1 の
    署名指定（`NewAdminHandler(queue, log)`）どおり authorizer を取らず、401/403 を
    RequireAdminConsoleAndSuperAdmin middleware に全面委譲する（Req 4.3 / 4.4 / NFR 3.1）。
    filter は from/to/type のみで `tenant_id` 句を持たない（design.md API Contract と一致）。
  - **type 許可値検証**: type は `ENROLLMENT/STATUS_REPORT/COMMAND` の完全一致のみ許可し、
    許可外（`USAGE_LOGS_UPLOADED` 等）/ 大小文字違い（`enrollment`）は 400 + `invalid type`
    で不正項目を提示する（Req 4.5）。from/to は audit.parseRFC3339Query と同方針の
    `invalid from` / `invalid to` 固定文言で 400（query 生値を補間しない / NFR 3.1）。
  - **payload の二重 encode 回避**: UnassignedNotificationDTO.Payload は `json.RawMessage`
    として jsonb 生 bytes を透過し（string への二重 encode を避ける）、空は
    payloadOrEmptyJSON で `{}` に倒す（unassigned.go と整合）。received_at は RFC3339。
  - **503 写像**: UnassignedQueue.List の DB 失敗（`CodeUnavailable`）は WriteHTTP の
    EffectiveHTTPStatus で 503 になる。failure_kind=query_error を構造化 WARN に出す
    （audit と識別軸を揃える / NFR 3.1）。
- **残存課題**: 配線 smoke（cmd/api への Mount 経路）と退避 → 閲覧の往復は task 5 の結合
  テスト（Req 6.4）でカバーする（本 task は handler 単体 + 配線コードのみ / mock queue で検証）。
  Dispatcher 本体の worker 経路 wire-in は #35 scope 外で据え置き。

### Task 5

- **採用方針**: `backend/test/integration/notification_dispatch_test.go` を追加。subscriber /
  Pub/Sub emulator を介さず `Dispatcher.Handle` を直接駆動し、実 `NewDedupe` / `NewUnassignedQueue` +
  実 `tenant.NewService`（逆引き）を結線、種別 handler は呼び出し回数を数える mock で dispatch
  経路を回帰検証（design.md Testing Strategy / Risk「worker wire-in scope 外」と整合）。
- **重要な判断**:
  - **bound テナント fixture は Insert→UpdateBound で採番**: `seedDummyData` は status='bound' でも
    enterprise_name を設定しないため逆引きが成立しない。`tenant_repository_test.go` の
    Insert→UpdateBound パターンで enterprise_name を保存した bound テナントを作り、payload の
    `name` 接頭辞（`enterprises/{id}`）を tenant の enterprise_name と一致させて Verifier の
    wire-format 解釈（task 2）に厳密に合わせた。
  - **Req 6.4 は閲覧 API 経由を優先**: admin_handler を test server に Mount し、SuperAdmin
    TenantContext を注入する middleware を被せて HTTP GET で取得・絞り込みを検証した（401/403 は
    RequireAdminConsoleAndSuperAdmin の責務のため、`audit_test.go` の injectAuthClaimsMW と同方針で
    ガードを bypass し閲覧経路に集中）。from/to は RFC3339、type は許可値のみ。
  - **冪等性は実 DB の ON CONFLICT で検証**: 同一 MessageID を 2 回 Handle し handler 呼び出しが
    1 回のみ + dedupe 記録で 2 回目が即 ack されることを実 notification_dedupe で確認（mock では
    なく実 PK + ON CONFLICT 経路 / Req 6.1）。
  - **DB 不在は t.Skip**: `requireDBURLs` で DATABASE_URL 未設定環境は全テスト skip。build には
    必ず含まれ、go vet / golangci-lint も pass する（false-fail させない）。
- **残存課題**: なし（task 5.1 で本 spec の全タスク完了 / 5 も昇格完了）。Dispatcher の cmd/worker
  への wire-in は #35 scope 外で据え置き（暫定 handler のまま）。実 DB での pass 確認は本環境に
  PostgreSQL が無いため未実施だが、skip 経路・build・vet・lint は green。

## 確認事項

- **逆引き DB テストの配置（spec 表記との差異 / 非ブロッキング）**: tasks.md 1.1 は
  「`internal/tenant/repository_test.go` に逆引きテストを追加」と記すが、本リポジトリでは
  repository 層の DB 依存テストは `internal/tenant/repository_test.go` が存在せず
  `backend/test/integration/` に集約する既存慣習のため、逆引きの実 DB テストは
  `backend/test/integration/tenant_repository_test.go` に追加した（`requireDBURLs` で
  DATABASE_URL 未設定時 skip）。Service 層の境界ケース（空入力で DB 非アクセス /
  DB 失敗 transient 伝達）は `internal/tenant/service_test.go` に fake repository で単体化済み。
  spec 本文は書き換えていない（既存テスト配置パターンの踏襲を優先した判断）。

- **AMAPI Pub/Sub wire-format の前提（task 2 / 仕様ギャップ・要 task 3/5 整合）**:
  requirements / design は通知の wire-format（種別と enterprise_name をどこから読むか）を
  確定していない。Verifier は AMAPI 公式仕様の標準形に基づき **種別 = attribute
  `notificationType`**、**enterprise_name = payload JSON のリソース名 `name`
  （`enterprises/{id}/...`）の `enterprises/{id}` 接頭辞** を読む解釈で実装した。task 3
  （Dispatcher）/ task 5（結合テスト）はこの Envelope 契約を前提に組むこと。実 AMAPI の
  attribute/payload 形式が異なる場合は本前提の見直しが必要（spec 本文は書き換えていない）。
- **stray tool 残骸（非ブロッキング）**: 本ファイル末尾に task 1 の write 由来とみられる
  `</content>` / `</invoke>` 行が `## 確認事項` セクション外に残っているが、本 task では
  触らず放置した（malformed tool 残骸であり確認事項本文ではない）。

## PR Iteration round 1（PR #59 / round-2 review 対応 / #39）

round-2 review（sha `2e321e5`）の 3 指摘への対応。`requirements.md` / `design.md` /
`tasks.md` は impl PR のため書き換えていない（矛盾は本セクション末尾「確認事項（PR iteration）」と
PR 返信で提起）。

- **[high] dispatcher.go 退避経路の取りこぼし窓 → 退避を原子化**:
  未割当退避を `dedupe.Claim`（別 tx）→ `Enqueue`（別 tx）→ 失敗時 `Release` の 3 操作から、
  `UnassignedQueue.Enqueue` 内で **claim（`claimSQL`）+ 退避 INSERT を同一 tx** で実行する原子操作に
  変更（`unassigned.go`）。claim 後・退避前の停止や退避失敗時の orphan claim が構造的に発生しなくなり、
  退避 INSERT 失敗時は claim も同 tx で rollback されて再配信で再退避できる（Req 5.1 / NFR 2.2）。
  並行 / 再配信の同一 MessageID は退避キューに重複行を作らない（Req 1.3 / 3.3）。`Dispatcher` 側は
  `enqueueUnassigned` が `Enqueue` の ack/nack 写像のみを担う形に簡素化した（`Release` 呼び出しを廃止）。
- **[high] dispatch（handler）経路の残存リスクは構造的制約として明示**:
  handler 経路は claim を handler 実行の前に取り、handler 失敗時に `Release` する claim-first を維持
  （Req 1.3 の並行直列化と結合テスト `ConcurrentDuplicateMessageID_DispatchedOnce` を満たすため。
  record-after-success へ戻すと並行同一 MessageID の二重実行を許し Req 1.3 / 6.1 を破る）。
  claim 永続化後・handler 成功記録前の worker 停止、または `Release` 自体の DB 失敗で orphan claim が
  残り喪失しうる窓は **既存スキーマ（`notification_dedupe` は processed 状態のみで in-progress / lease を
  持てない）では原理的に塞げない**。`status` 列追加による reclaim-after-timeout は本 Issue Out of Scope
  （新規マイグレーション禁止）であり、退避経路（handler 非関与で単一 tx 化可能）とは異なり handler が
  IF 経由の外部依存（design.md L462-468 の非原子性 Risk）のため同 tx に閉じられない。**`status` 列導入の
  follow-up Issue を起票して根治する**ことを推奨（当該経路は `releaseClaim` が ERROR ログで事後追跡可能）。
- **[medium] 未対応/未登録種別が退避キューを汚す → 種別判定を退避より前へ**:
  `Handle` の handler 登録確認（Req 2.4）を tenant 解決（Req 3.2 退避判定）の **前** に移動
  （`dispatcher.go`）。未登録種別（例: AMAPI がトピック作成時に送る `notificationType=test`）は
  enterprise_name 未解決でも退避されず取りこぼさず ack 完了扱いになる（types.go「未知種別は破棄 ack」
  設計意図と整合）。回帰テスト: 単体 `dispatcher_test.go`（未登録種別 × tenant 未解決で enqueue=0）/
  結合 `TestNotificationDispatch_UnregisteredType_NotQuarantined`。
- **[low] tasks.md verify が `./test/integration` を含まない**:
  impl PR のため `tasks.md` は書き換えない。結合テストは
  `go test ./test/integration/... -run NotificationDispatch`（要 `INTEGRATION_TEST_DATABASE_URL` /
  `INTEGRATION_TEST_MIGRATE_URL`）で実行する。verify コマンドへの結合テスト追記は設計 PR iteration
  または follow-up での `tasks.md` 更新を推奨（PR 返信で提起）。

検証: 実 PostgreSQL（16）で `internal/notification` 単体（`-race`）+ `test/integration` 全件
（`-race`）green。`go build ./...` / `go vet ./...` / 単体全件 green。

### 確認事項（PR iteration）

- **design.md フロー図との順序差異（registration 判定の前出し）**: design.md L77-89 のフロー図は
  未登録種別判定（`未登録種別 --> DropAck`）を Handler ノードの下流（Resolve → SetCtx の後）に置くが、
  本修正は Req 2.4（未対応/未登録は取りこぼさずログ + 完了扱い）を退避 Req 3.2 より優先する読みと
  types.go の設計意図に合わせ、判定を Resolve の前へ移した。両 Req が同時成立（未登録 × tenant 未解決）
  する場合の優先順位がフロー図で未確定のため、設計 PR で図の更新を推奨する（impl PR では図を書き換えない）。

STATUS: complete
