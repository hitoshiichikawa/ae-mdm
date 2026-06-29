# 実装メモ（Issue #52 / #38 follow-up: Tenant Bind の競合制御強化）

per-task ループで実装を進める。本ファイルは各 task の learning と AC Traceability を追記していく。

## AC Traceability

| Requirement | 担保 task / テスト |
|---|---|
| 4.1（status 4 値化） | task 1（migration 0017 で `binding` 追加）/ DB 層の最終証跡は task 7.1 の reversible test |
| 3.1（signup_url_name 永続化） | task 1（migration 0017 で `signup_url_name` 列追加）/ 永続化往復は task 7.1 |

> 上表は task 進行に伴い追記する（本 task 1 が担保する AC のみ記載）。

## Implementation Notes

### Task 1（migration 0017: tenant_status 4 値化 + signup_url_name 列）

- **採用方針**: 0016 と同じ「ALTER のみ・IF NOT EXISTS / IF EXISTS で冪等」パターンに揃え、
  `ALTER TYPE tenant_status ADD VALUE IF NOT EXISTS 'binding'` と
  `ALTER TABLE tenants ADD COLUMN IF NOT EXISTS signup_url_name text` を up に置いた。
- **重要な判断**:
  - enum 値 `binding` は PostgreSQL に `ALTER TYPE ... DROP VALUE` 構文が無いため down で個別削除
    できない。0017 down では削除を試みず no-op（SQL コメントで明示）とし、可逆性は全 down
    シーケンスで 0001 down の `DROP TYPE IF EXISTS tenant_status` が enum 型ごと削除 → 再 up で
    0001 が 3 値再作成 → 0017 が `binding` を再 ADD VALUE することで担保される（design L402-409 の
    方針に一致）。
  - `ADD VALUE` は PostgreSQL 12+ で tx 内実行可能。0017 は追加値を同 migration 内で参照しない
    （DML を含まない）ため tx 内制約に抵触しない。
  - `signup_url_name` は NULL 許容（既存行・create 前整合のため）。Repository 側の NULL→空文字
    写像は後続 task 2.1 / 3.1 の責務（本 task は列追加のみ）。
- **検証**: 実装ファイルは Go コードではないが、回帰がないことを stage-a-verify ブロック
  （`go build ./... && go vet ./... && go test ./internal/tenant/...`）で確認し全 pass。
  `test/integration`（reversible test 含む）も compile + 自己 skip（DB env 未設定）で pass。
  実 PostgreSQL を要する reversible test の green 確認は task 7.1（_Boundary: tenant_repository_test.go_）に
  deferred されている（tasks.md L90 / 本 task は「テスト追加ではなく既存テストの非破壊検証」）。
- **残存課題**: なし（migration ファイル追加のみ。後続 task 2.1 で types.go の `StatusBinding` /
  `SignupURLName` フィールドを追加する前提）。

## 確認事項

- 現時点で spec（requirements.md / design.md / tasks.md）と実装の間に矛盾は検出していない。
