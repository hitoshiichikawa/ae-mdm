# Review Notes

<!-- idd-claude:review round=1 model=claude-opus-4.8 timestamp=2026-06-28T00:30:00Z -->

## Reviewed Scope

- Branch: claude/issue-36-impl--b2a-policy-validator-5
- Round 1 review HEAD: ece807efbcc8fe537abea2179ad3238138b74c26（この commit 時点の検証記録）
- Compared to: develop...HEAD（merge-base 基準の追加分のみ。develop 先行分は除外）
- 変更構成（現在のブランチ）: 全 7 ファイル / 約 1,200 insertions / 0 deletions（新規追加のみ。既存コード改変なし）
  - `backend/internal/policy/{doc.go,types.go,validator.go,validator_test.go}`（新規 package）
  - `docs/specs/36--b2a-policy-validator-5/{requirements.md,impl-notes.md,review-notes.md}`（本 review-notes.md 自身も差分に含まれるため 6→7 ファイル）
- Round 1 以降の follow-up commit（PR iteration で適用、本検証記録もこれらに整合させて更新）:
  - `fbecf46` / `344cba1` — AMAPI passwordQuality enum 補完・Kiosk エラー機械可読化・パスワード最小桁数の下限を AMAPI 仕様準拠で 0 に修正
  - `5118447` — アプリ件数の負値を invalid field として拒否（Req 1 の下限検証追加）
- 注記: 本 Issue は `tasks.md` / `design.md` 不在の単一実装パス（Architect 非起動）。
  `_Boundary:_` アノテーションは存在しないが、差分は新規隔離 package と spec docs に限定され
  既存コンポーネントへの変更が 0 のため boundary 観点の懸念なし。
- Feature Flag Protocol: CLAUDE.md `採否: opt-out` のため flag 観点の細目は適用せず（通常 3 カテゴリ判定）。

## Verified Requirements

- 1.1 — `checkAppCount`（範囲 0〜MaxAppCount を受理、>3000 は business rule 拒否・<0 は invalid field 拒否） / `TestValidate_App_CountLimit`（count=1 受理）
- 1.2 — `checkAppCount`（3001 拒否 + `KindBusinessRule`） / `TestValidate_App_CountLimit`（3001 拒否 + Kind 検証）
- 1.3 — `TestValidate_App_CountLimit`（2999 受理 / 3000 受理 / 3001 拒否の境界網羅）
- 1.4 — `TestValidate_App_CountLimit`（count=0 受理 / 下限直下 -1・負値 -100 を invalid field 拒否）
- 2.1 — `checkPasswordLength`（範囲内受理） / `TestValidate_Password_LengthRange`（length=8 受理）
- 2.2 — `checkPasswordLength`（下限未満拒否 + `KindInvalidField`） / 同テスト（-1 拒否 + Kind 検証。0 は AMAPI 仕様で「制限なし」のため受理、下限直下は -1）
- 2.3 — `checkPasswordLength`（上限超過拒否） / 同テスト（17 拒否）
- 2.4 — `TestValidate_Password_LengthRange`（下限ちょうど 0・上限ちょうど 16 受理 / 下限直下 -1・上限直上 17 拒否）
- 3.1 — `checkSecurity`（必須充足 + enum 許容値で受理） / `TestValidate_Security_RequiredAndEnum`（正常系）
- 3.2 — `checkSecurity`（EncryptionPolicy 空文字を欠落拒否 + Field 識別） / 同テスト（欠落異常系）
- 3.3 — `checkSecurity`（EncryptionPolicy / PasswordQuality 許容値外拒否） / 同テスト（許容値外 2 ケース）
- 4.1 — `checkSystemUpdate`（必須充足 + 範囲内受理） / `TestValidate_SystemUpdate_RequiredAndRange`（AUTOMATIC / WINDOWED 0-1439 受理）
- 4.2 — `checkSystemUpdate`（Type 空文字を欠落拒否 + Field 識別） / 同テスト（欠落異常系）
- 4.3 — `checkSystemUpdate`（enum 外拒否 + WINDOWED 分範囲 0〜1439 外拒否） / 同テスト（UNKNOWN_TYPE / StartMinutes=1440 / EndMinutes=-1）
- 5.1 — `checkKiosk`/`isValidPackageName`（妥当形式受理） / `TestValidate_Kiosk_PackageNameFormat`（com.example 等受理 + 空集合受理）
- 5.2 — `checkKiosk`（形式不正拒否） / 同テスト（example 等拒否）
- 5.3 — `isValidPackageName`（2 セグメント以上 + 各セグメント英字始まり英数字/`_`） / 同テスト（1seg・数字始まり・ハイフン・末尾dot 拒否）
- 5.4 — `isValidPackageName`（空文字拒否） / 同テスト（空文字列拒否）
- 6.1 — `ValidationError.Domain`/`Field` 保持 / 全テストの `hasError`(domain, field) 検証
- 6.2 — `Validate`（全 rule を収集 append） / `TestValidate_CollectsAllErrors`（4 領域同時不正→4 件収集）
- 6.3 — `KindBusinessRule` + `Code()`→`CodeBusinessRule`(422) / `TestValidate_DistinguishesErrorKinds`（app=business rule/422）
- 6.4 — `KindInvalidField` + `Code()`→`CodeInvalidRequest`(400) / 同テスト（password=invalid field/400）
- 6.5 — `ValidationResult.IsValid` / `TestValidate_AllValid_ReturnsSuccess`（5 領域妥当→0 件成功）
- NFR 1.1 — `TestValidate_Deterministic`（2 回呼出で件数・各項目一致）
- NFR 1.2 — `validator.go` は `regexp` のみ import（DB/HTTP/file/時刻 非依存）。`doc.go` に依存方向ルール明記
- NFR 1.3 — 各 rule が入力↔期待結果の table-driven テストで単体検証可能
- NFR 2.1 — 5 領域すべてに正常系 / 異常系 / 境界値テストを `t.Run` で網羅

## Findings

なし

## Summary

5 領域すべての numeric AC（Req 1〜6）と NFR 1/2 が実装・テスト両面でカバーされ、`go test -count=1 ./internal/policy/...` が green。差分は新規隔離 package（`internal/errors` のみ依存）+ spec docs に限定され既存コード改変ゼロのため boundary 逸脱なし。missing test も検出されず。

RESULT: approve
