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
</invoke>

STATUS: complete
