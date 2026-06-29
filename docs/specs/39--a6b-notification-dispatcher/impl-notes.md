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
