# Requirements Document

## Introduction

テナント管理者が作成・更新する Android Management ポリシーは、AMAPI へ反映される前に各領域
（アプリ / パスワード / セキュリティ / システム更新 / Kiosk）の設定値が妥当であることを保証
する必要がある。不正な設定値（アプリ上限超過・パスワード桁数の範囲外・Kiosk パッケージ名の
形式不正・必須項目欠落など）がそのまま AMAPI に渡ると、不正なポリシーが端末に配信され業務影響を
生む。本 spec は umbrella #24 の Requirement 4 のうち、5 領域の入力検証ロジック（Policy
Validator）を **純粋関数層**として切り出して提供する。Validator は外部副作用を持たず、与えられた
5 領域の設定値を検証して、不正な領域・フィールドを機械可読に識別できる結果を返す。ポリシーの
永続化・割当・AMAPI patch・UI は本 spec の対象外であり、別 Issue（Policy Service / tenant-console）で
扱う。

## 関連

- Parent: #24
- Depends on: #2

## Requirements

### Requirement 1: アプリ領域のバリデーション（件数上限）

**Objective:** As a Policy Validator の呼び出し側（Policy Service）, I want 1 ポリシーあたりのアプリ件数が上限を超えていないか検証したい, so that 上限超過の不正ポリシーが AMAPI へ反映され端末に配信されるのを防げる

#### Acceptance Criteria

1. When アプリ件数が 3,000 件以下のポリシー設定値が渡されたとき, the Policy Validator shall 当該アプリ領域を妥当として受理する
2. If アプリ件数が 3,000 件を超えるポリシー設定値が渡されたとき, the Policy Validator shall 当該設定値を拒否し、アプリ件数上限超過である旨を機械可読に識別できる検証エラーを返す
3. When アプリ件数が境界値 2,999 / 3,000 / 3,001 件のいずれかのポリシー設定値が渡されたとき, the Policy Validator shall 2,999 と 3,000 を受理し、3,001 を上限超過として拒否する
4. When アプリが 0 件のポリシー設定値が渡されたとき, the Policy Validator shall アプリ件数上限の観点では当該設定値を受理する

### Requirement 2: パスワード領域のバリデーション（桁数の数値範囲）

**Objective:** As a Policy Validator の呼び出し側, I want パスワード最小桁数が許容される数値範囲に収まっているか検証したい, so that 範囲外の桁数設定による不正なパスワードポリシー配信を防げる

#### Acceptance Criteria

1. When パスワード最小桁数が許容範囲（下限以上・上限以下）の値で渡されたとき, the Policy Validator shall 当該パスワード領域を妥当として受理する
2. If パスワード最小桁数が許容下限を下回る値で渡されたとき, the Policy Validator shall 当該設定値を拒否し、パスワード桁数が範囲外である旨を機械可読に識別できる検証エラーを返す
3. If パスワード最小桁数が許容上限を上回る値で渡されたとき, the Policy Validator shall 当該設定値を拒否し、パスワード桁数が範囲外である旨を機械可読に識別できる検証エラーを返す
4. When パスワード最小桁数が許容範囲の下限値ちょうど・上限値ちょうど・下限直下・上限直上のいずれかで渡されたとき, the Policy Validator shall 下限値ちょうどと上限値ちょうどを受理し、下限直下と上限直上を範囲外として拒否する

### Requirement 3: セキュリティ領域のバリデーション（必須項目・enum 値）

**Objective:** As a Policy Validator の呼び出し側, I want セキュリティ領域の必須項目欠落と不正な選択肢値を検出したい, so that 欠落・不正値を含むセキュリティポリシーが端末へ配信されるのを防げる

#### Acceptance Criteria

1. When セキュリティ領域の必須項目がすべて充足され、選択肢項目が許容値集合内の値で渡されたとき, the Policy Validator shall 当該セキュリティ領域を妥当として受理する
2. If セキュリティ領域の必須項目が欠落した状態で渡されたとき, the Policy Validator shall 当該設定値を拒否し、欠落した必須項目を機械可読に識別できる検証エラーを返す
3. If セキュリティ領域の選択肢項目に許容値集合外の値が渡されたとき, the Policy Validator shall 当該設定値を拒否し、許容値外の項目を機械可読に識別できる検証エラーを返す

### Requirement 4: システム更新領域のバリデーション（必須項目・不正値）

**Objective:** As a Policy Validator の呼び出し側, I want システム更新領域の必須項目欠落と不正値を検出したい, so that 欠落・不正値を含む更新ポリシーが端末へ配信されるのを防げる

#### Acceptance Criteria

1. When システム更新領域の必須項目がすべて充足され、各項目値が妥当な範囲・形式で渡されたとき, the Policy Validator shall 当該システム更新領域を妥当として受理する
2. If システム更新領域の必須項目が欠落した状態で渡されたとき, the Policy Validator shall 当該設定値を拒否し、欠落した必須項目を機械可読に識別できる検証エラーを返す
3. If システム更新領域の項目に妥当な範囲・形式を外れた値が渡されたとき, the Policy Validator shall 当該設定値を拒否し、不正な項目を機械可読に識別できる検証エラーを返す

### Requirement 5: Kiosk 領域のバリデーション（パッケージ名形式）

**Objective:** As a Policy Validator の呼び出し側, I want Kiosk 指定アプリのパッケージ名が Android パッケージ名として妥当な形式か検証したい, so that 形式不正なパッケージ名による不正な Kiosk ポリシー配信を防げる

#### Acceptance Criteria

1. When Kiosk 指定アプリのパッケージ名がすべて妥当な Android パッケージ名形式で渡されたとき, the Policy Validator shall 当該 Kiosk 領域を妥当として受理する
2. If Kiosk 指定アプリのパッケージ名のいずれかが妥当な Android パッケージ名形式を満たさない状態で渡されたとき, the Policy Validator shall 当該設定値を拒否し、形式不正なパッケージ名を機械可読に識別できる検証エラーを返す
3. While Kiosk 指定アプリのパッケージ名を判定しているとき, the Policy Validator shall ドット区切りで 2 セグメント以上を持ち、各セグメントが英字始まりで英数字とアンダースコアのみからなる文字列のみを妥当な形式とみなす
4. If Kiosk 指定アプリのパッケージ名が空文字列で渡されたとき, the Policy Validator shall 当該設定値を拒否し、形式不正として識別できる検証エラーを返す

### Requirement 6: 検証エラーの報告（領域・フィールドの識別と種別区別）

**Objective:** As a Policy Validator の呼び出し側, I want 不正項目がどの領域・どのフィールドかと、上限超過か形式不正かの区別を機械可読に受け取りたい, so that 呼び出し側が適切な HTTP ステータス（422 / 400 相当）と詳細を管理者へ提示できる

#### Acceptance Criteria

1. When 検証エラーを返すとき, the Policy Validator shall 各エラーに対して不正な領域（アプリ / パスワード / セキュリティ / システム更新 / Kiosk）と対象フィールドを機械可読に含める
2. When 1 回の検証で複数の不正項目が存在するとき, the Policy Validator shall 検出したすべての不正項目を 1 つの検証結果として収集して返す
3. When アプリ件数上限超過による検証エラーを返すとき, the Policy Validator shall 当該エラーをビジネスルール違反（上限超過）として、項目フォーマット不正と区別できる種別で識別する
4. When 必須項目欠落・範囲外・形式不正による検証エラーを返すとき, the Policy Validator shall 当該エラーを不正入力（invalid field）として、上限超過と区別できる種別で識別する
5. When すべての領域の設定値が妥当なとき, the Policy Validator shall 検証エラーを 1 件も含まない成功結果を返す

## Traceability（umbrella #24 Requirement 4 への対応）

本 spec の各 Requirement は umbrella #24 `requirements.md` の Requirement 4（ポリシー管理）の
以下 AC に対応する。本 spec は当該 AC のうち **検証ロジック部分のみ**を切り出す。

| 本 spec | umbrella #24 の対応 AC | 対応する論理エラー（design.md） |
|---|---|---|
| Requirement 1（アプリ件数上限） | 4.2（5 領域の設定項目: アプリ）, 4.5（アプリ上限 3,000 件） | `policy_app_limit_exceeded`（business rule / 422） |
| Requirement 2（パスワード桁数範囲） | 4.2（パスワード）, 4.6（必須項目の不正値拒否） | `invalid_policy_field`（invalid field / 400） |
| Requirement 3（セキュリティ必須・enum） | 4.2（セキュリティ）, 4.6 | `invalid_policy_field` |
| Requirement 4（システム更新必須・不正値） | 4.2（システム更新）, 4.6 | `invalid_policy_field` |
| Requirement 5（Kiosk パッケージ名形式） | 4.2（Kiosk）, 4.6 | `invalid_policy_field` |
| Requirement 6（エラー報告・種別区別） | 4.5, 4.6（不正項目を管理者に提示） | 上限超過 / invalid field の区別 |

## Non-Functional Requirements

### NFR 1: 純粋性・テスト容易性

1. The Policy Validator shall 同一の入力設定値に対して常に同一の検証結果を返す（決定論的）
2. The Policy Validator shall データベース・HTTP・ファイル・システム時刻のいずれの外部副作用にも依存せず検証を完結する
3. The Policy Validator shall 各検証規則を入力（設定値）と期待結果（受理 / 拒否 + 不正項目）の対応として単体テスト可能な形で提供する

### NFR 2: 検証の網羅観点

1. The Policy Validator shall 5 領域それぞれについて、正常系（受理）・異常系（拒否）・境界値の各観点を単体テストで検証できる粒度の振る舞いを提供する

## Out of Scope

- Policy Service（ポリシーの upsert / 端末・トークンへの割当 / AMAPI `policies.patch` 反映）— 別 Issue（umbrella #24 task 8.2）
- ポリシー編集 UI / クライアント側検証（tenant-console の `features/policies`）— 別 Issue
- ポリシー変更の監査ログ記録（Req 4.8 相当）— Policy Service の責務
- 他テナントポリシー更新の拒否（Req 4.7 相当、認可 / RLS）— 認可基盤の責務
- AMAPI Policy リソースの完全な JSON schema 検証（本 Validator は 5 領域のドメイン入力検証に限定し、AMAPI 側 schema の網羅検証は対象外）
- 専用エラーコード定数（`policy_app_limit_exceeded` / `invalid_policy_field`）の追加可否などの実装手段の確定 — design.md / 実装の領分（本 spec は「上限超過と形式不正を機械可読に区別できる」振る舞いのみ要求する）

## Open Questions

- パスワード最小桁数の具体的な許容範囲（下限・上限の数値）が umbrella #24 では「数値範囲」とのみ規定され確定値が未定。AMAPI の `passwordMinimumLength` 仕様（一般に 1〜16 程度）に整合させる想定だが、本 spec で採用する確定値は design / 実装段階で AMAPI 公式仕様を一次情報として確認のうえ確定する
- セキュリティ領域・システム更新領域の「必須項目」「選択肢項目（enum）」の具体的なフィールド集合が umbrella #24 では代表項目までしか規定されていない。各領域で検証対象とする具体フィールドの確定は design 段階で AMAPI Policy schema を参照して定める（本 spec は「必須項目欠落」「許容値外」を検出する振る舞いを領域単位で要求するに留める）
