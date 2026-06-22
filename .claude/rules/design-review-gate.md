---
paths:
  - "docs/specs/**/design.md"
---
<!-- 条件ロード（#327）: 上記 paths に触れるセッションにのみ自動付与される。frontmatter を削除すると全コンテキスト常時ロードに戻るため注意。 -->

<!-- SPDX-License-Identifier: MIT -->
<!-- Adapted from cc-sdd (https://github.com/gotalab/cc-sdd), MIT License, Copyright (c) gotalab -->

# 設計書レビューゲート（Architect 自己レビュー）

Architect が `design.md` を書き終える前に、このゲートに従ってドラフトをレビューし、
問題があれば修正、問題なければ確定します。

## 要件カバレッジレビュー

- `requirements.md` の **すべての numeric requirement ID**（1, 1.1, 2, 2.1 ...）が design.md の
  Requirements Traceability マッピングに現れ、具体的なコンポーネント・契約・フロー・データモデル・
  運用判断のいずれかで裏打ちされているか
- 外部依存・連携点・ランタイム前提・マイグレーション・可観測性・セキュリティ・
  パフォーマンス目標が明示的に design.md に反映されているか
- カバー不足が draft 不完全さなら修正、**要件自体が曖昧なら requirements に戻す**
  （設計側で要件を勝手に発明しない）

## アーキテクチャ準備レビュー

- コンポーネント境界が、実装タスクの担当を推測せず割り当てられる程度に明示されているか
- Interfaces / Contracts / State transitions / 統合境界が、実装と検証のために十分具体的か
- build vs adopt の判断のうち、アーキテクチャに材料的に影響するものが design.md に記録されているか
- ランタイム前提・マイグレーション・ロールアウト制約・検証フック・障害モードのうち、
  実装順序やリスクに影響するものが可視化されているか

## 実行可能性レビュー

- 設計が、隠れた前提なしで**境界のあるタスク列**として実装可能か
- 並列実装を意図する箇所では parallel-safe な境界が見えているか
- 投機的抽象化（将来スコープのためだけに存在するコンポーネント／アダプタ／インターフェース）を排除しているか
- tasks.md で直接参照できない程に曖昧なセクションは、確定前に書き直す

## Task turn 予算 sanity check（過大 task 検出）

`tasks.md` ドラフトの確定直前に、Architect は各タスクが `DEV_MAX_TURNS`（既定 60）以内に
収まる粒度であるかを **判断レビューの一環として** 点検します。本観点は、`tasks-generation.md`
の「turn 予算ガイドライン（per-task Implementer ループ運用時の粒度指針）」で示した **タスク
生成段階の指針** に対し、`design-review-gate.md` 側で **自己レビュー時の検出観点** を補う形で
機能します（生成 vs レビューの役割分担）。

本観点は **推奨（指針）レベル** であり、reject 条件ではありません。Mechanical Checks 節には
属さず（後述の理由）、機械的な数値強制（CI / pre-commit / Mechanical Check による自動 reject）も
行いません。観点に該当する task を発見した場合、Architect は当該 task の分割または最上位昇格を
**検討** し、判断結果（分割するか据え置くか）を `design.md` / `tasks.md` に反映します。

### 背景: 層対称分割の落とし穴

frontend / UI / テストが重い責務は、backend / pure logic と比べて **turn コスト密度が高い**
（描画・状態・スタイル・スナップショット・visual regression 等のコンテキストを並行で抱える
ため）。「層ごとに対称に分割する」だけで安心していると、frontend 側のタスクだけが
`error_max_turns` を踏み抜く事故が発生します（例: 「API クライアント lib(+test) + 複数
component(+test)」を 1 タスクにまとめた事例）。

このため、Architect 自己レビュー時には **turn コスト密度を意識した分割** に視点を切り替え、
以下のシグナルで過大 task を点検することを推奨します。

### 検出シグナル

以下のシグナルのいずれかに該当する task は、過大 task の可能性を **点検対象** とします
（必ず分割せよという意味ではなく、判断レビューの俎上に乗せるための観点列挙）:

1. **異種責務の同居**: 1 つのタスクに「API クライアント lib(+test)」「複数 component(+test)」
   「状態管理 / Store(+test)」「スタイル / theme 変更」等の **異種責務** が混在していないか。
   同居していれば責務単位での分割を検討する
2. **兄弟比突出**: 同階層の兄弟タスクと比較して、当該タスクの **詳細項目数** または
   **想定新規ファイル数** が突出していないか。兄弟比の倍率や項目数の絶対値は **目安** であり
   厳密な閾値は定めない（Open Question (b) の通り、定量強制はしない）
3. **新規ファイル件数の目安**: 1 タスクで新規追加するファイル数が多い場合（**目安として 3 件
   以上**）、分割を検討する。なお 3 件はあくまで **目安** であり、ファイル粒度（小ユーティリティの
   集合体か / 大規模 component 1 件か）で実コストは大きく異なるため、絶対閾値としては運用しない
4. **重い子タスクの同居**: 重い子タスク（`1.1` / `1.2` …）が同一親の下に複数同居していないか。
   同居している場合、子を親から切り出して **最上位 task（`2` / `3` …）への昇格** を検討する
   （子は独立 commit 単位として消化されるため、昇格させた方が turn 予算管理が容易）
5. **turn コスト密度差**: frontend / UI / テスト重責務は backend より turn コスト密度が高い。
   層対称分割（frontend 1 タスク + backend 1 タスク等）ではなく、**turn コスト密度を意識した
   分割**（frontend を細かく刻み、backend を統合する等）が望ましい場面がある

### 是正方針: 責務不変の粒度分割

過大 task を検出した場合の是正は、**`design.md` の責務・コンポーネント構成を変えず、tasks の
切り方のみを調整する** ことを基本とします（責務不変の粒度分割）。具体的には:

- 異種責務同居タスクを **責務単位の独立タスク** に分割する（例: 「API lib + UI」 → 「API lib」
  と「UI」を別タスクに）
- 重い子タスクを **最上位 task に昇格** させる（親から切り出して `2` / `3` … として独立 commit
  単位にする）
- 兄弟比が突出しているタスクを **複数タスクに分解** する（責務単位 / ファイル単位の自然な
  境界で分ける）

`design.md` 側の責務再設計が必要と判断される場合のみ、judgment review に戻して再検討します
（観点違反を理由に design 側の責務を強制的に書き換える運用は採用しない）。

### Mechanical Checks 節に含めない理由

本観点を **Mechanical Checks に含めない** のは以下の理由です（Mechanical Checks は機械的に
判定可能な項目に限定するため）:

- タスクごとの turn 消費量は実装難度・既存コードベースの状態・テスト規模に依存し、設計段階で
  正確な事前見積もりが難しい
- ファイル数 / 兄弟比などのシグナルは **必要条件にも十分条件にもならない**（小ファイル 5 件 <
  大ファイル 1 件のケースが普通にある）。数値強制すると false-positive が頻発する
- `DEV_MAX_TURNS` 既定値（60）は将来変更され得る。機械 enforcement に紐付けると数値追従コストが
  発生する
- 推奨どまりにすることで Architect / 人間設計者が判断材料として活用しつつ、reject 判定の自動化は
  既存 Mechanical Checks（Requirements traceability / File Structure Plan 充填 / orphan component /
  Budget overflow / checkbox enforcement / verify block well-formed）に集約できる

### 既存規約との関係

- [`tasks-generation.md`](./tasks-generation.md) の「turn 予算ガイドライン（per-task Implementer
  ループ運用時の粒度指針）」節（#289 で追加）と **役割分担** します:
  - `tasks-generation.md` 側 = **タスク生成段階** の粒度指針（fresh session 仕様 / 粒度指針 /
    強度の項）
  - 本節 = **Architect 自己レビュー段階** の検出観点（5 シグナル / 是正方針）
- 既存「レビュー・ループ」節の **最大 2 パス** 規約および `/goal` 自動ループ運用節は変更しません
  （本観点は判断レビュー側の観点追加であり、ループ手順自体は変えない）
- Mechanical Checks 節（Requirements traceability / File Structure Plan 充填 / orphan component /
  Budget overflow / checkbox enforcement / verify block well-formed）の判定基準は変更しません
- 既に main に merge 済みの spec の `design.md` / `tasks.md` に対する **遡及的な違反検出は
  要求しません**（retrofit は本観点のスコープ外）

### 適用タイミング

Architect は `tasks.md` ドラフトの確定直前、判断レビュー（要件カバレッジ / アーキテクチャ準備 /
実行可能性）を通過し、Mechanical Checks（Budget overflow check など）が pass した後の段階で
本観点を点検することを推奨します。本観点に該当する task を発見した場合は、上記「是正方針」に
従って tasks の切り方を調整してから確定します。

## Mechanical Checks（自動確認項目）

判断レビューの前に、機械的に確認します:

- **Requirements traceability**: requirements.md から numeric ID を全抽出し、design.md のどこかに
  出現するか scan。未参照 ID を報告
- **File Structure Plan の充填**: File Structure Plan セクションに具体的なファイルパスが
  書かれているか（"TBD" やプレースホルダを検出）
- **orphan component なし**: design.md の Components セクションに挙がったコンポーネント名のうち、
  File Structure Plan に対応ファイルが無いものを検出
- **Budget overflow check**: tasks.md の最上位 numeric ID タスク件数を機械的にカウントし、
  閾値に応じて分岐する（後述「Budget overflow check（tasks.md 件数）」節を参照）
- **tasks.md checkbox enforcement check**: tasks.md のすべてのタスク行が checkbox 形式
  （`- [ ]` または `- [ ]*`）で開始することを機械的に確認する（後述「tasks.md checkbox
  enforcement check」節を参照）
- **verify block well-formed check**: tasks.md に構造化 verify ブロック（センチネル
  `<!-- stage-a-verify -->` + 直後 fence）がある場合、それが well-formed か（直後 fence /
  fence 閉じ / 中身非空）を機械的に確認する（後述「verify block well-formed check」節を参照）

### Budget overflow check（tasks.md 件数）

`tasks.md` の **最上位 numeric ID タスク**（`- [ ] 1.` / `- [ ] 2.` のように、リストネスト無しで
整数 ID `.` で始まる行）の件数を数えます。子タスク（`1.1`, `1.2` …）や `- [ ]*`（deferrable
テストタスク）は **本カウントでは数えません**（独立コミット単位での turn 消費が支配的なため）。

#### Count 抽出 regex（参照実装）

POSIX 互換の ERE で次のパターンに一致する行を 1 件として数えます:

```
^- \[ \]\*? [0-9]+\. 
```

意味: 行頭が `- [ ]` または `- [ ]*` で、続けて整数 ID + `.` + 半角スペースで始まる行
（例: `- [ ] 1. <タイトル>` / `- [ ] 12. <タイトル>` / `- [ ]* 3. <タイトル>`）。
**`1.1` のような小数階層 ID は子タスクなのでマッチしません**（`[0-9]+\.` の直後に空白が来る規約）。

> **正準とハーネスの相互参照（#216）**: 本 regex は本ルールが **正準** であり、harness 側の
> `local-watcher/bin/issue-watcher.sh` の `tc_count_tasks`（Tasks Count Gate #147）が同一 regex に
> 厳密一致させて同一件数を返します。harness 側を変更する場合は **本節を正準として先に**更新して
> ください（両者は別実行基盤のため共有コードを持てず、同一 regex の明記と相互参照でドリフトを
> 防いでいます）。

#### 閾値表（判定分岐）

| タスク件数 | 判定 | Architect への要求 |
|---|---|---|
| ≤ 10 件 | pass | 追加アクションなしで確定 |
| 11〜13 件 | consolidate を試行 | タスク統合を試行し、結果として 10 件以下になればそのまま pass。なお 11〜13 件のままなら `design.md` 末尾に `## Split Proposal` セクションを追加し、`needs-decisions` ラベルを Issue に付与 |
| ≥ 14 件 | forced split | consolidate を経由せず `design.md` 末尾に `## Split Proposal` セクションを追加し、`needs-decisions` ラベルを Issue に付与 |

#### Split Proposal セクションの誘導

11〜13 件で consolidate に失敗した場合、および 14 件以上の forced split の場合、
Architect は `design.md` の **末尾**（既存セクション群の後）に `## Split Proposal` セクションを
追加します。本セクションの構造とテンプレは
[`.claude/agents/architect.md`](../agents/architect.md) の「Budget overflow が検出された場合の対応」
節を参照してください。

#### 既存ガイドラインとの関係

`tasks-generation.md` の「3〜10 件目安」ガイドラインは引き続き **設計指針** として有効で、
本 Mechanical Check は同ガイドラインの 10 件上限を **機械的な enforcement boundary** として
扱う実装上の取り決めです（追加要件ではない）。10 件以下のケースでは本機能導入前と挙動は
変化しません。

#### 境界の参照 fixture

判定境界（10 / 11 / 13 / 14 件）の期待動作は、本リポジトリの開発時に以下の fixture と
スモークスクリプトで回帰確認しています:

- `docs/specs/131-feat-architect-tasks-md-budget-overflow/test-fixtures/tasks-10.md`（pass）
- `docs/specs/131-feat-architect-tasks-md-budget-overflow/test-fixtures/tasks-11.md`（consolidate → split）
- `docs/specs/131-feat-architect-tasks-md-budget-overflow/test-fixtures/tasks-13.md`（consolidate → split）
- `docs/specs/131-feat-architect-tasks-md-budget-overflow/test-fixtures/tasks-14.md`（forced split）
- `docs/specs/131-feat-architect-tasks-md-budget-overflow/test-count.sh` — count 抽出 regex の整合性検証

### tasks.md checkbox enforcement check

`tasks.md` の **すべてのタスク行** が checkbox 形式（`- [ ]` 未完了 / `- [ ]*` deferrable）で
開始することを機械的に確認します。本チェックは Developer の resume 機能
（`IMPL_RESUME_PROGRESS_TRACKING=true`、Issue #67 / #112 以降の既定）が markdown 上の
checkbox を進捗の **正本** として読む前提を、Architect 段階で確実に成立させるためのものです。

#### 判定パターン（参照実装）

タスク行と認識する POSIX 互換 ERE:

```
^- \[[ x]\]\*? [0-9]+(\.[0-9]+)*\.? 
```

意味: 行頭が `- [ ]` / `- [ ]*` / `- [x]` / `- [x]*` のいずれかで、続けて numeric 階層 ID
（`1` / `1.1` / `2.1.3` 等）+ 半角スペースで始まる行をタスク行と認識する（最上位タスクは
ID 末尾の `.` あり [`- [ ] 1. <名前>`]、子タスクは末尾の `.` なし [`- [ ] 1.1 <名前>`] が
既存表記。regex 末尾の `\.?` がこの差を吸収）。checkbox を持たないタスク表現
（例: `## T-01: タスク名` / `### Task 1` / `#### 1.1 子タスク` 等の markdown header だけで
タスクを表す行）は本パターンにマッチせず、**違反として報告**されます。

#### 検証手順

1. Architect は `tasks.md` ドラフトの確定直前に、上記判定パターンで全タスク行を抽出する
2. タスク本体を表す行が **markdown header のみ**（`^#{1,6} ` で始まり、リスト項目になって
   いない行）でタスクを表現している箇所が無いかを目視確認する（例: `## T-01: タスク名` /
   `### 子タスク`）
3. checkbox 不在のタスク行を 1 件でも検出した場合、該当行を `- [ ] <numeric ID>. <タスク名>`
   形式（または `- [ ]* <numeric ID> <タスク名>`）に修正してから確定する
4. [`tasks-generation.md`](./tasks-generation.md) の「Checkbox 形式の必須化」節と整合する
   ことを確認する（同節と本チェックは同一 checkbox 規約に依拠する）

#### Budget overflow check との関係

本 checkbox enforcement check と既存の **Budget overflow check** は、**同一の checkbox 規約**
（`- [ ]` / `- [ ]*` で始まるタスク行の認識）に依拠しています:

- Budget overflow check の count 抽出 regex `^- \[ \]\*? [0-9]+\. ` は **最上位 numeric ID
  タスク**のみを数える狭い判定（ID 末尾の `.` を必須化）
- checkbox enforcement check の判定パターン `^- \[[ x]\]\*? [0-9]+(\.[0-9]+)*\.? ` は
  **親タスク・子タスク・完了済みタスク** を含む広い判定（ID 末尾の `.` をオプショナル化）
- 両者は同じ「タスク行 = リスト項目 + checkbox + numeric ID」という規約を共有しており、
  本機能導入により Budget overflow check の判定境界（10 / 11 / 13 / 14 件）は **変化しません**
  （`docs/specs/131-feat-architect-tasks-md-budget-overflow/test-fixtures/` の 4 fixture が
  全て `- [ ]` checkbox を持つことで引き続き動作します）

#### 適用範囲（後方互換性）

- 本チェックの対象は **Architect が新規に生成・編集する `tasks.md`** に限定する
- 既に main に merge 済みの spec の `tasks.md` に対する **遡及的なルール違反検出は要求しない**
  （retrofit は本 rule のスコープ外）
- 既存 deferrable テストタスク表記 `- [ ]*` は有効な checkbox 形式として扱う（違反として
  報告しない）
- ≤ 10 件の正常ケースを含む Budget overflow check の挙動は変化しない

### verify block well-formed check

`tasks.md` に **構造化 verify ブロック**（stage-a-verify gate #125 / #224 の input 契約。
センチネル `<!-- stage-a-verify -->` + 直後 fenced code block）が含まれる場合、それが
well-formed であるかを機械的に確認します。本チェックは malformed なブロックが Developer
フェーズまで持ち越され、watcher の実行時に黙って fallback（env / heuristic / SKIPPED）へ
後退してしまう事故を、設計確定前に検出するためのものです（Req 5.1, 5.2）。

#### well-formed 判定（参照実装）

ブロックが well-formed であるとは、以下をすべて満たすことです（モジュール側 awk
`stage_a_verify_extract_verify_block`（`local-watcher/bin/modules/stage-a-verify.sh`）の
抽出基準と **同一**。両者は別実行基盤のため共有コードを持てず、同一基準の明記と相互参照で
ドリフトを防いでいます。判定基準を変更する場合は本節とモジュール側 awk の双方を同期更新する
こと）:

1. **センチネル存在**: 行を trim した結果が厳密に `<!-- stage-a-verify -->` に一致する
   アンカー行が存在する（前後空白許容、行内の他テキスト不可）
2. **直後性 / 直後 fence**: アンカー行の次行以降で空行を任意個スキップした後の最初の非空行が
   fence 開始（trim 後 ` ``` ` 始まり）である（fence 以外の非空行が先に来たら malformed）
3. **fence 閉じ**: fence が次の ` ``` ` 行で閉じている（EOF まで閉じなければ malformed）
4. **中身非空**: fence 内が trim 後すべて空ではない（空ブロックは malformed）
5. **複数ブロック**: 上記を満たす最初のアンカー + fence のみを採用（決定論）

書式規約の散文側正本は [`tasks-generation.md`](./tasks-generation.md) の「構造化 verify ブロック」
節です（本節と同一の well-formed 条件に依拠）。

#### 検証手順

1. Architect は `tasks.md` ドラフトの確定直前に、センチネル `<!-- stage-a-verify -->` の有無を
   走査する
2. センチネルが存在する場合、上記 well-formed 判定 1〜4 を目視 / 機械的に確認する
3. malformed（直後 fence なし / fence 未クローズ / 中身空）を検出した場合、**違反として報告**し、
   確定前に well-formed な書式へ修正する（既存ゲートと同じ最大 2 パス）
4. センチネルが存在しない（ブロックを宣言していない）spec は、本 well-formed 判定の対象外
   （後述 Req 5.3 の warn とは別レイヤ）

#### verify 対象あり + ブロック/env 両無の扱い（Req 5.3）

verify 対象（build/test/lint）を持つはずのプロジェクトで、構造化 verify ブロックも
`STAGE_A_VERIFY_COMMAND` も存在しない場合、本チェックは **warn 止まり**（reject しない）と
します。design-less impl（tasks.md 不在の #204 等）や verify 不要 spec（純ドキュメント変更等）を
誤って reject しないための安全側設計です。Architect は warn を受けて構造化ブロックの宣言を
検討しますが、宣言しないことを理由に確定をブロックしません。

> **design-less impl の SKIP は意図された仕様（#230）**: `design` モードを経由せず Architect が
> `tasks.md` を生成しない design-less impl（tasks.md 不在）では、watcher 実行時に stage-a-verify
> gate が verify コマンドを推測せず **SKIP** します。これは未実装の取りこぼしではなく
> 「watcher は verify コマンドを推測しない」設計思想（#224 / #228）に基づく意図された仕様です。
> design-less impl の regression は Developer が実行するテストと Reviewer の AC 判定で担保されます
> （詳細は README「Stage A Verify Gate (#125)」節を参照）。本 well-formed check はそもそも
> `tasks.md` を生成する Architect ルートのみが対象であり、design-less impl には適用されません。

#### 適用範囲（後方互換性）

- 本チェックの対象は **Architect が新規に生成・編集する `tasks.md`** に限定する
- 既に main に merge 済みで構造化 verify ブロックを持たない既存 spec を **遡及的な違反として
  報告しない**（Req 5.4、retrofit は本 rule のスコープ外）
- 構造化 verify ブロックを持たない spec は従来どおり env / ヒューリスティック / SKIPPED に
  fallback するため、本チェック導入により既存挙動は変化しない（NFR 1.1）

### `/goal` による自動ループ運用（Claude Code v2.1.139+）

Claude Code v2.1.139 以降では、上記 3 つの Mechanical Checks を `/goal` の完了条件として
宣言し、未達なら自動で次ターンを実行する運用が可能です。**v2.1.139 未満の環境では本節
全体をスキップし、後述の「レビュー・ループ」節の従来手順（Mechanical Checks → 判断レビュー
→ 最大 2 パス）をそのまま適用してください**（後方互換）。

#### 適用タイミング

Architect エージェントが `design.md` ドラフトを確定する直前、判断レビュー（要件カバレッジ
／アーキテクチャ準備／実行可能性）を通過した段階で `/goal` を発行します。順序は
「Mechanical Checks の `/goal` 自動ループ → 判断レビュー → 確定」を推奨します。

#### Architect 向け完了条件文字列テンプレ例

以下のいずれかを `/goal <条件>` の `<条件>` 部に貼り付けて発行します（自然言語の AND
結合で記述し、EARS トリガーキーワード `When` / `If` / `While` / `Where` / `shall` は混ぜない）:

```
requirements.md のすべての numeric requirement ID（1, 1.1, 2 等）が design.md のどこかに出現し、
かつ File Structure Plan セクションに具体的なファイルパスが書かれていて "TBD" やプレースホルダが残っておらず、
かつ Components セクションに挙がった全コンポーネント名が File Structure Plan に対応ファイルを持つ
```

短縮版:

```
design.md は全 numeric requirement ID を参照し、File Structure Plan に "TBD" が残置されず、全 Component に対応ファイルがある
```

#### ターン上限の併記

`/goal` 自動ループのターン上限は、後述「レビュー・ループ」節の **最大 2 パス**を流用します
（撤廃ではなく併記）。`/goal` が 2 ターン経過しても完了条件を満たさない場合は、自動ループ
を終了し、要件フェーズ戻し（requirements.md 側の不明点を PM に差し戻す）または人間
エスカレーション（Issue コメントで設計判断を仰ぐ）を選択します。

## レビュー・ループ

- Mechanical Checks → 判断レビューの順
- 問題が draft 内で閉じるなら修正して再レビュー
- **最大 2 パス**で確定するか、要件フェーズに戻す（無限ループを避ける）
  - Claude Code v2.1.139+ では上記「`/goal` による自動ループ運用」節の手順で Mechanical Checks 部分を自動収束させる
  - v2.1.139 未満では本節の手順をそのまま実行する（従来挙動と完全一致）
- ゲート通過後に `design.md` を確定させる

## 参考

- [cc-sdd `design-review-gate.md`](https://github.com/gotalab/cc-sdd/blob/main/.kiro/settings/rules/design-review-gate.md)
