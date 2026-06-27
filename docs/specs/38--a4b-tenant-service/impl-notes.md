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

## 確認事項

- **design.md「Modified Files」と現状の差異（task 1 では影響なし）**: design.md は「本 Issue 時点で `cmd/api/main.go` は未存在のため bootstrap 配線は行わない」と記すが、現コードベースには既に `cmd/api` パッケージが存在する（`go build ./...` / `go test ./...` 緑）。task 1 の scaffold（types + audit port）には影響しないが、後続 task 6（`Handler.Mount`）で DI bootstrap の既存状況を再確認すること。`design.md` / `tasks.md` の書き換えは行っていない（実装 PR では spec を書き換えない規約に準拠）。
