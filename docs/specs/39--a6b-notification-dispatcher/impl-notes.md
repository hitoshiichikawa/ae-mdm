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

## 確認事項

- **逆引き DB テストの配置（spec 表記との差異 / 非ブロッキング）**: tasks.md 1.1 は
  「`internal/tenant/repository_test.go` に逆引きテストを追加」と記すが、本リポジトリでは
  repository 層の DB 依存テストは `internal/tenant/repository_test.go` が存在せず
  `backend/test/integration/` に集約する既存慣習のため、逆引きの実 DB テストは
  `backend/test/integration/tenant_repository_test.go` に追加した（`requireDBURLs` で
  DATABASE_URL 未設定時 skip）。Service 層の境界ケース（空入力で DB 非アクセス /
  DB 失敗 transient 伝達）は `internal/tenant/service_test.go` に fake repository で単体化済み。
  spec 本文は書き換えていない（既存テスト配置パターンの踏襲を優先した判断）。
</content>
</invoke>
