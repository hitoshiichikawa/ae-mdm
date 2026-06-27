# Implementation Notes — #36 [B2a] Policy Validator（5領域）

## 実装方針

- 新規 package `internal/policy`（純粋関数層 / NFR 1）。`internal/errors` のみを import し、
  amapi / 上位 service / cmd には依存しない（`doc.go` に依存方向ルールを明記）。
- **ドメイン入力型**（`PolicyInput` + 5 領域 struct）を定義し、`amapi.PolicyBody`（Raw map）には
  依存しない。Raw map → ドメイン型の変換は Policy Service の責務（本 Issue 対象外）。各領域は
  ポインタ（nil=未指定）で部分更新を許容し、nil 領域は検証 skip する。
- **table-driven rule struct**（`validator.go` の `rule` / `rules`）。各領域の規則を 1 エントリで
  宣言し、`Validate` が `applicable` → `check` の順で適用。検証規則の追加は `rules` への 1 行追加で済む。
- **エラー収集モデル**: `ValidationResult.Errors []ValidationError` に全不正項目を収集（Req 6.2）。
  各 `ValidationError` は `Domain` / `Field` / `Kind`（`KindBusinessRule` / `KindInvalidField`）/
  `Message` を機械可読に保持。`ErrorKind.Code()` で `errors.CodeBusinessRule`(422) /
  `errors.CodeInvalidRequest`(400) へマッピングし、呼び出し側の HTTP ステータス決定を支援（Req 6.1/6.3/6.4）。

ファイル: `backend/internal/policy/doc.go` / `types.go` / `validator.go` / `validator_test.go`。

## Open Questions の確定値と根拠

1. **パスワード最小桁数の許容範囲**: `MinPasswordLength=0` / `MaxPasswordLength=16`
   （`validator.go`）。同梱 AMAPI SDK（`google.golang.org/api@v0.186.0`
   `androidmanagement/v1` `PasswordRequirements.PasswordMinimumLength`）の field doc
   "The minimum allowed password length. A value of 0 means there is no restriction." を
   一次情報として確認し、`0` を「制限なし」を表す有効値として受理する下限 0 を採用。負値のみを
   範囲外として拒否する。上限 16 は AMAPI 公式 schema に明示上限が無いためアプリ層の保守的
   上限として据え置き（requirements.md Open Question の latitude 内 / Policy Service 統合時に再確認）。
2. **セキュリティ領域の検証フィールド**:
   - 必須 + enum: `EncryptionPolicy`（AMAPI `encryptionPolicy` 相当。許容値
     `ENABLED_WITHOUT_PASSWORD` / `ENABLED_WITH_PASSWORD`）。空文字を欠落として拒否（Req 3.2）。
   - 任意 + enum: `PasswordQuality`（AMAPI `passwordQuality` 相当）。指定時のみ許容値検証（Req 3.3）。
3. **システム更新領域の検証フィールド**:
   - 必須 + enum: `Type`（AMAPI `systemUpdate.type` 相当。許容値 `AUTOMATIC`/`WINDOWED`/`POSTPONE`）。
   - 範囲: `Type=="WINDOWED"` のとき `StartMinutes`/`EndMinutes` を 0〜1439（1 日の分数）で検証（Req 4.3）。

## AC ↔ テスト Traceability（1 要件 1 行）

| AC | 担保テスト |
|---|---|
| Req 1.1/1.2/1.3/1.4 | `TestValidate_App_CountLimit`（0/1/2999/3000=受理, 3001=拒否+business rule） |
| Req 2.1/2.2/2.3/2.4 | `TestValidate_Password_LengthRange`（8/1=受理, -1/-5/17=拒否, 0=境界下受理(AMAPI 制限なし)/16=境界上受理） |
| Req 3.1/3.2/3.3 | `TestValidate_Security_RequiredAndEnum`（充足受理 / 欠落・許容値外拒否） |
| Req 4.1/4.2/4.3 | `TestValidate_SystemUpdate_RequiredAndRange`（受理 / 欠落・enum外・範囲外拒否） |
| Req 5.1/5.2/5.3/5.4 | `TestValidate_Kiosk_PackageNameFormat`（妥当受理 / 1seg・数字始まり・ハイフン・末尾dot・空文字拒否） |
| Req 6.1 | 全テストの `Domain`/`Field` 検証（`hasError` helper） |
| Req 6.2 | `TestValidate_CollectsAllErrors`（4領域同時不正→4件収集） |
| Req 6.3 | `TestValidate_DistinguishesErrorKinds`（app=business rule, Code()=422） |
| Req 6.4 | `TestValidate_DistinguishesErrorKinds`（password=invalid field, Code()=400） |
| Req 6.5 | `TestValidate_AllValid_ReturnsSuccess`（5領域妥当→0件成功） |
| NFR 1.1 | `TestValidate_Deterministic`（2回呼出で同一結果） |
| NFR 1.2 | 設計上 DB/HTTP/ファイル/時刻 非依存（`Validate` は純粋関数。テスト内で外部副作用無し） |
| NFR 1.3/2.1 | 全テストが table-driven（入力↔期待結果）+ 5領域 正常/異常/境界 を `t.Run` で網羅 |

## 検証コマンド実行結果

- `gofmt -l internal/policy/`: 差分なし（PASS）
- `cd backend && go vet ./...`: PASS（警告なし）
- `cd backend && go build ./...`: PASS
- `cd backend && go test ./internal/policy/...`: PASS（7 関数 / サブテスト全 pass）
- `cd backend && go test ./...`: PASS（既存 auth / authz / amapi / integration 等すべて green）

（Go コマンドは `GOTOOLCHAIN=local` 付きで実行。Makefile の規約と整合）

## 確認事項

- パスワード桁数の **下限** は同梱 AMAPI SDK（`google.golang.org/api@v0.186.0`）の
  `PasswordRequirements.PasswordMinimumLength` field doc を一次情報として確認し、`0`=「制限なし」を
  受理する下限 0 に確定した（PR #48 round 2 レビュー反映）。**上限 16** は AMAPI 公式 schema に明示
  上限が無く、各領域の enum 許容値集合と併せて Policy Service 統合時（task 8.2）に AMAPI 公式
  schema と突き合わせて再確認することを推奨。値は `validator.go` の定数 / `allowed*` map に集約して
  おり、変更は局所的に可能。
- `EncryptionPolicy` の許容値に `ENCRYPTION_POLICY_UNSPECIFIED` を含めていない（明示指定を必須と
  みなす保守的判断）。AMAPI 仕様で UNSPECIFIED を有効値として扱う場合は `allowedEncryptionPolicies`
  への追加で対応可能。

STATUS: complete
