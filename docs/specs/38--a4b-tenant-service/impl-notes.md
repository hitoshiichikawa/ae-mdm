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

## 確認事項

- **design.md「Modified Files」と現状の差異（task 1 では影響なし）**: design.md は「本 Issue 時点で `cmd/api/main.go` は未存在のため bootstrap 配線は行わない」と記すが、現コードベースには既に `cmd/api` パッケージが存在する（`go build ./...` / `go test ./...` 緑）。task 1 の scaffold（types + audit port）には影響しないが、後続 task 6（`Handler.Mount`）で DI bootstrap の既存状況を再確認すること。`design.md` / `tasks.md` の書き換えは行っていない（実装 PR では spec を書き換えない規約に準拠）。

STATUS: complete
