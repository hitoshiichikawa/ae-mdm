---
paths:
  - "docs/specs/**/tasks.md"
  - "docs/specs/**/design.md"
---
<!-- 条件ロード（#327）: 上記 paths に触れるセッションにのみ自動付与される。frontmatter を削除すると全コンテキスト常時ロードに戻るため注意。 -->

<!-- SPDX-License-Identifier: MIT -->
<!-- Adapted from cc-sdd (https://github.com/gotalab/cc-sdd), MIT License, Copyright (c) gotalab -->

# tasks.md 生成ルール

Architect が出力する `tasks.md` は、Developer が迷わず実装を進められる粒度と、
トレーサビリティを持つアノテーションを持たせます。

## 基本フォーマット

### 単純タスクのみの場合

```markdown
- [ ] 1. <タスクの要約>
  - <詳細項目（必要な場合のみ）>
  - _Requirements: 1.1, 2.3_
```

### 親タスクと子タスクの構造を取る場合

```markdown
- [ ] 1. <親タスクの要約>
- [ ] 1.1 <子タスクの記述> (P)
  - <詳細項目 1>
  - <詳細項目 2>
  - _Requirements: 1.1, 1.2_
  - _Boundary: UserService, AuthController_
  - _Depends: 2.1_
```

## Checkbox 形式の必須化

`tasks.md` の **すべての実装タスク行**は、行頭が `- [ ]`（未完了）または `- [ ]*`（deferrable
印、後述）の checkbox 形式で開始することを **必須** とします。これは Developer の resume
機能（`IMPL_RESUME_PROGRESS_TRACKING=true`、Issue #67 / #112 以降の既定）が `- [ ]` → `- [x]`
の markdown checkbox 編集を進捗の **正本** として読む前提を確実に成立させるためです。

- **親タスク行・子タスク行のいずれにも checkbox を付与すること**
  （例: `- [ ] 1. ...` / `- [ ] 1.1 ...` のように親も子もリスト項目 + checkbox で書く）
- **markdown header のみ**（例: `## T-01: タスク名` / `### Task 1` / `#### 1.1 子タスク`）で
  タスクを表現することは **禁止**。タスク行は必ずリスト項目 (`- [ ]`) で書くこと
- 詳細項目（`_Requirements:_` 等のアノテーション行や説明箇条書き）は checkbox を持たない
  通常のリスト項目で構わない（タスクそのものを表現する行のみが checkbox 必須）
- 判定パターン（POSIX 互換 ERE）: `^- \[[ x]\]\*? [0-9]+(\.[0-9]+)*\.? ` — 行頭が `- [ ]`
  / `- [ ]*` / `- [x]` / `- [x]*` のいずれかで、続けて numeric 階層 ID（`1` / `1.1` /
  `2.1.3` 等）+ 半角スペースで始まる行をタスク行と認識する（最上位タスクは ID 末尾の
  `.` あり [`- [ ] 1. <名前>`]、子タスクは末尾の `.` なし [`- [ ] 1.1 <名前>`] が既存表記）

> **Mechanical Check との対応**: 上記必須化は Architect の自己レビュー時に
> [`design-review-gate.md`](./design-review-gate.md) の **tasks.md checkbox enforcement check**
> Mechanical Check が機械的に検証します（checkbox 不在のタスク行を 1 件でも検出した場合は
> 違反として報告し、Architect が `- [ ] <numeric ID>. <タスク名>` 形式に修正してから確定する）。

## アノテーション

| キー | 必須? | 用途 |
|---|---|---|
| `_Requirements:_` | **必須** | 対応する requirement ID を列挙（numeric のみ、例: `1.1, 2.3`）。説明や括弧書きは付けない |
| `_Requirements_partial:_` | per-task Reviewer 運用で deferred test を伴う場合のみ | 当該 task では実装のみ行い、対応テスト追加を後続 task に deferred している AC numeric ID を列挙。詳細は後述「task-test 境界整合の規約」節を参照 |
| `_Boundary:_` | 並列可タスク `(P)` でのみ必須 | 担当するコンポーネント名を列挙（design.md の Components 名と一致） |
| `_Depends:_` | 非自明な cross-boundary 依存のみ | 先行するタスク ID を列挙。自明な順序依存は省略 |

## 並列マーカー `(P)`

- **並列実行可能**なタスクのみ末尾に ` (P)` を付ける
- 並列実行できないタスク（順序依存のあるタスク）には付けない（デフォルト=直列）
- `(P)` を付けるなら `_Boundary:_` を必須とする（並列時の競合境界を明示するため）

## ID 規則

- **numeric 階層 ID** のみ使用: `1`, `1.1`, `1.2`, `2`, `2.1` ...
- `T-01` や `FR-01` 形式の英字 ID は使わない（requirements.md の numeric ID と揃えるため）

## task-test 境界整合の規約（per-task Reviewer 運用時 / Issue #303）

`PER_TASK_LOOP_ENABLED=true` の per-task Reviewer ループ運用では、各 task 完了時に Reviewer
が当該 task の `_Requirements:_` で宣言された AC numeric ID について「対応テストが当該 task の
diff range 内にあるか」を `missing test` カテゴリで判定します。Architect が「実行時挙動の変更」と
「対応 regression / failure-path / safety-fallback テスト追加」を異なる task に分割し、なお
先行 task の `_Requirements:_` に当該テスト側 AC を残したままにすると、per-task Reviewer は
テスト未追加の状態で AC 紐付けを評価し、`missing test` で reject する事故が発生します
（idd-codex 側で複数回観測。本節は Architect 段階での予防規約です）。

本節は **per-task Reviewer ループ運用時の Architect / Developer / Reviewer 共通の参照点**
（task-boundary contract）として機能します。`PER_TASK_LOOP_ENABLED` 未指定 / `=true` 以外
（既定）の運用環境では本節は **適用されず**、既存単一 Developer 一括実装フローの挙動は
変化しません（NFR 1.1 / 1.2 / 1.3）。

### Architect への要求（Req 1.1〜1.3, 1.5）

Architect は `tasks.md` 生成時に **task 単位**で以下を決定すること:

1. **同一 task 内に対応テストを含める（default）**: 各 task の `_Requirements:_` に列挙した
   AC について、対応テスト追加作業を **当該 task の詳細項目に明記** することを default
   とする。task の詳細項目に「テスト追加」「regression test 追加」「shell-level fixture
   検証」等の具体的な作業項目を 1 つ以上含めること
2. **behavior-changing task は最低限の regression test を同 task 内に含める**: 当該 task が
   実行時挙動を変える（behavior-changing）場合、当該 task 内に最低限の regression /
   shell-level test 追加を含めること（テストが完全に他 task へ deferred されているのは
   後述 partial 明示が必須）
3. **特に同 task 内テスト必須となる AC カテゴリ**: 当該 task の `_Requirements:_` に
   regression coverage / failure path / API・parse failure handling / stale data safety /
   safety-side fallback の AC numeric ID が含まれる場合、対応テスト追加作業を **必ず**
   同 task 内に含める（後続 task への deferred は不可）

### partial 明示の canonical 記法（Req 1.4, 1.5 / NFR 2.3）

対応テストを後続 task に deferred する場合、Architect は先行 task で当該 AC を
**partial 明示**するか、`_Requirements:_` から除外する必要があります。partial 明示の
canonical 記法は **以下の独立アノテーション方式 1 つに固定** します（複数表記の混在を生まない /
NFR 2.3）:

```markdown
- [ ] 2.1 新エラーハンドリング実装（テストは task 3 で追加）
  - 詳細項目
  - _Requirements: 2.1, 2.2_
  - _Requirements_partial: 2.2_
  - _Boundary: ErrorHandler_
```

意味解釈:

- `_Requirements: 2.1, 2.2_` — 当該 task が AC 2.1, 2.2 に対する **実装**を持つ
- `_Requirements_partial: 2.2_` — そのうち 2.2 は **テスト追加が deferred** されており、当該
  task の per-task Reviewer 判定で `missing test` の reject 対象外とする
- partial 明示された AC は、後続のいずれかの task（dedicated regression test task 等）で
  対応テスト追加が完了する必要がある（Architect は deferred 先 task を明示する責務を持つ）

書式規約:

- 1 行 = 1 アノテーション。`_Requirements_partial:_` の値は numeric 階層 ID のスペース /
  カンマ区切り（`_Requirements:_` と同じ書式）
- `_Requirements_partial:_` に列挙する numeric ID は **必ず**同 task の `_Requirements:_` に
  含まれる subset でなければならない（`_Requirements:_` に存在しない ID を partial 宣言する
  ことはできない）
- partial 宣言が無い AC は「同 task 内にテスト追加されている」とみなされ、per-task Reviewer は
  通常通り `missing test` 判定対象として扱う

代替記法（**禁止**。canonical 化のため）:

- 行内サフィックス方式（例: `_Requirements: 1.1 (partial), 1.2_`）は `_Requirements:_` の
  既存パース規約（numeric ID 列挙、説明や括弧書き禁止）と矛盾するため採用しない
- task タイトル中の散文（例: `タスク名 (partial)`）は機械パース不能のため不可
- `<!-- partial: 1.1 -->` 等の HTML コメント方式は markdown checkbox enforcement と
  非干渉だが、Reviewer が同一ルールで解釈できないため不採用

### dedicated regression test task の境界制約（Req 2.1〜2.3）

Architect が dedicated regression test task（テストのみを目的とする後続 task）を切り出す
場合、当該 test task は以下の境界制約に従うこと:

1. **`_Requirements:_` の重複制御**: 当該 test task の `_Requirements:_` は、先行
   behavior-changing task の `_Requirements:_` と重複させない、または **partial 解消関係**
   （先行 task で `_Requirements_partial:_` 明示された AC を当該 test task でカバーする
   関係）であることを task の詳細項目に明示する
2. **スコープ限定**: dedicated regression test task のスコープは **E2E / 統合テスト /
   coverage 補完等**、先行 task の per-task Reviewer 判定に影響しない範囲に限定する。
   単体テストの追加が先行 task の AC に直接紐づく場合は、後続 test task に切り出さず
   先行 task 内に含めること
3. **partial 解消の責務**: 先行 task で `_Requirements_partial:_` 明示された AC は、
   後続のいずれかの test task で対応テスト追加が完了する必要がある（Architect は
   deferred 先 task を tasks.md 上で明示すること。task 名 / 詳細項目に「task N の
   deferred test を解消する」旨を含める）

### Developer / Reviewer の参照（Req 3.1〜3.5）

本節は Architect / Developer / Reviewer が **同一の task-boundary contract** として参照
します。各エージェントの責務:

- **Developer**: 当該 task の `_Requirements:_` に列挙された AC のうち
  `_Requirements_partial:_` に含まれない ID については、当該 task 内で対応テストを実装する
  責務を負う。実装できないと判断した場合は `tasks.md` を書き換えず、PR 本文「確認事項」
  または Issue コメントで Architect への差し戻しを提案する（詳細は
  [`developer.md`](../agents/developer.md) の per-task ループ節）
- **Reviewer**: per-task Reviewer 起動時、当該 task の `_Requirements:_` 列挙 AC に対応する
  テスト追加が当該 task diff range 内にあるかを `missing test` カテゴリで判定する。
  `_Requirements_partial:_` で明示された AC については、当該 task の `missing test` reject
  理由としない（partial 解消は後続 task で確認される）。詳細は
  [`reviewer.md`](../agents/reviewer.md) の per-task ループ節

### 後方互換性（NFR 1.1〜1.3, 既存運用との関係 / Req 5.1〜5.3）

- `PER_TASK_LOOP_ENABLED` 未指定 / `=true` 以外の運用では本節は適用されない（既存単一
  Developer 一括実装フローの挙動を変化させない）
- 既に main に merge 済みの `tasks.md` に対する **遡及的な書き換えは要求しない**（retrofit
  は本 rule のスコープ外）
- 既存の `_Requirements:_` / `_Boundary:_` / `_Depends:_` / `(P)` / `- [ ]*` の各アノテーション
  規約を破壊的に変更しない。`_Requirements_partial:_` は **新規追加**の optional
  アノテーションであり、既存 spec が宣言しない場合は従来通り「全 AC が同 task 内テスト
  必須」とみなされる
- 既存 Mechanical Checks（Budget overflow check / checkbox enforcement check / verify block
  well-formed check）の判定ロジックは変更しない。`_Requirements_partial:_` 行は
  checkbox enforcement check の判定パターン（`^- \[[ x]\]\*? [0-9]+(\.[0-9]+)*\.? `）に
  マッチしないため、タスク件数カウント・checkbox 判定への影響はない

## Optional なテストタスク

deferrable なテスト追加タスクは checkbox を `- [ ]*`（アスタリスク付き）と記述し、詳細項目で
対応する AC を説明します。**`- [ ]*` も checkbox 形式の一種**として扱われ、上記
「Checkbox 形式の必須化」節および Mechanical Check の判定で違反として報告されません:

```markdown
- [ ]* 1.3 統合テスト追加
  - 対応する受入基準のうち、現時点でカバレッジが不足する項目を補完
  - _Requirements: 1.1, 1.2_
```

## ガイドライン

- 各タスクは **1 commit 単位**で独立に完了可能な粒度にする
- 合計タスク数は **3〜10 件を目安**（多すぎる場合は design の File Structure Plan が大きすぎる可能性）
- 対応する `_Requirements:_` を必ず明示（トレーサビリティ確保）
- 親タスクに対する子タスクは、実装順序に沿って並べる

> **件数 enforcement との関係**: 上記「3〜10 件目安」は設計指針として有効ですが、Architect の
> 自己レビュー時に [`design-review-gate.md`](./design-review-gate.md) の **Budget overflow check**
> Mechanical Check が同じ件数を機械的に判定します（≤10 件 pass / 11〜13 件 consolidate→split /
> ≥14 件 forced split）。10 件以下の正常ケースで挙動は変化しません。カウントは **最上位
> numeric ID タスク**（`- [ ] 1.` / `- [ ] 2.` …）のみが対象で、子タスク（`1.1` 等）や deferrable
> テストタスク（`- [ ]*`）は数えません。

## turn 予算ガイドライン（per-task Implementer ループ運用時の粒度指針）

`PER_TASK_LOOP_ENABLED=true` の per-task Implementer ループ運用では、`tasks.md` の **1 タスクごと**に
fresh な Claude session で Implementer が起動され、各タスクの turn 数は `DEV_MAX_TURNS`（既定 60）
を上限として **タスク間で独立に消費**されます。前のタスクで余った turn を次のタスクへ繰り越す
ことはできません。

### fresh session 仕様（前提）

- per-task Implementer は **タスクごとに新規 Claude session で起動**する。session 状態（過去の
  reasoning / context）はタスク間で持ち越されず、turn カウンタも各タスクで 0 から始まる
- 一度 `error_max_turns`（`DEV_MAX_TURNS` 到達による Claude CLI 自発 exit）で失敗したタスクは、
  再試行時も **同一タスク内で再び 0 turn から開始**する（過去 turn は再利用できない）
- したがって「タスク間の turn 累積を増やす」「Issue 全体の turn 枠を引き上げる」といった発想は
  per-task ループ運用上 **無効**であり、turn 予算は常に「1 タスクが `DEV_MAX_TURNS` に収まるか」
  だけが効く

### 粒度指針（推奨）

- **1 タスクは `DEV_MAX_TURNS`（既定 60）以内に収まる粒度を目安とする**。設計段階で「実装 + テスト +
  軽い refactor が 1 commit で完了する」程度の小ささに刻んでおくと、`error_max_turns` の発生確率を
  運用前に下げられる
- **frontend / UI / テストが重い責務は細かく切る**:
  - 「UI = 1 component + 1 test = 1 task」を目安とする（複数 component を 1 タスクに束ねない）
  - UI / frontend は描画・状態・スタイル・テストのコンテキストを並行で抱えるため turn 消費が
    膨らみやすく、1 タスクに複数 component を束ねると `DEV_MAX_TURNS` 到達リスクが顕著に上がる
  - Visual regression / snapshot 系テストの追加が必要な場合は別タスクに分離してよい
- **重い子タスクは親に束ねず、トップレベル task に昇格させる**:
  - 子タスク（`1.1` / `1.2` …）の合計 turn 見積もりが親の 1 つあたり目安を上回りそうなら、子を
    親から切り出して別の最上位 task（`2` / `3` …）に昇格させる
  - 最上位 task は独立 commit 単位として消化されるため、昇格させた方が turn 予算管理が容易
- **既存ガイドライン（「3〜10 件目安」「checkbox 必須化」）との関係**: 本指針は上記ガイドラインの
  **上位ではなく補助**として機能する。件数上限（≤10 件）に収まる範囲で、各タスクの turn 予算を
  さらに意識する形で適用すること

### 強度（推奨どまり / Mechanical Check 不在）

本節のガイドラインは **推奨（指針）レベル**であり、`design-review-gate.md` の Mechanical Checks
（Budget overflow check / checkbox enforcement check / verify block well-formed check）のような
機械的な reject 条件としては宣言しません。理由は以下:

- タスクごとの turn 消費量は実装難度・既存コードベースの状態・テスト規模に依存し、設計段階で
  正確な事前見積もりが難しい
- 数値（`DEV_MAX_TURNS=60`）は将来変更され得るため、機械 enforcement に紐付けると追従コストが
  発生する
- 推奨に留めることで Architect / 人間設計者が判断材料として活用しつつ、reject 判定の自動化は
  既存 Mechanical Checks に集約できる

> **根拠**: per-task ループは fresh session 仕様により turn 数がタスク単位で独立消費されるため、
> 設計段階で 1 タスクの turn 予算を意識することが、運用時の `error_max_turns` 発生確率を直接
> 下げる最も効果の高い手段になる。`DEV_MAX_TURNS` の恒久引き上げ（後述 README の
> Troubleshooting 節参照）はタスク粒度の不適合を覆い隠す対症療法であり、根本的にはタスク粒度の
> 是正で対処することが推奨される（詳細な対応優先順は README「`per-task-implementer-failed` /
> `error_max_turns` 対応」節を参照）。

### Architect 自己レビュー時の検出観点との相互参照（#292）

本節（タスク生成段階の粒度指針）と対になる **Architect 自己レビュー段階の検出観点** は、
[`design-review-gate.md`](./design-review-gate.md) の「Task turn 予算 sanity check（過大 task
検出）」節を参照してください。同節では本節の粒度指針を踏まえた上で、`tasks.md` 確定直前に
点検すべき 5 つの検出シグナル（異種責務同居 / 兄弟比突出 / 新規ファイル件数の目安 / 重い子タスク
同居 / turn コスト密度差）と是正方針（責務不変の粒度分割）を観点として列挙しています。生成
（本節）と自己レビュー検出（`design-review-gate.md` 側）の双方を参照することで、過大 task の
発生確率を運用前に下げられます。

## 構造化 verify ブロック（stage-a-verify gate の input 契約）

stage-a-verify gate (#125) は Stage A（Developer 実装）完了直前に、`tasks.md` 中の
build/test/lint コマンドを watcher が独立再実行し、Developer の自己申告だけで build 不通が
Stage A を通過するのを防ぐゲートです。従来はコマンド特定を「verify keyword を含む行＝コマンド」
とみなすヒューリスティック抽出で行っていたため、ツール名で始まる散文（例: `- shellcheck 警告
ゼロを確認`）を誤ってコマンドとして実行する誤発火が繰り返し発生していました（#160 / #219 / #221）。

これを避けるため、Architect は再実行させたい verify コマンドを **センチネル付きの構造化ブロック**で
`tasks.md` に明示宣言できます（#224）。構造化ブロックがあると watcher はヒューリスティック推測を
行わず、ブロック内のコマンドのみを決定論的に実行対象として解決します。

### canonical 書式

センチネルコメント `<!-- stage-a-verify -->` の **直後**に fenced code block を 1 つ置き、
その中身に実行するコマンドを書きます:

```markdown
<!-- stage-a-verify -->
```sh
shellcheck local-watcher/bin/modules/*.sh && bash docs/specs/<番号>-<slug>/test-fixtures/test-extract.sh
```
```

書式規約（well-formed 条件）:

- **センチネル**: 行を trim した結果が厳密に `<!-- stage-a-verify -->` に一致する行。前後の空白は
  許容するが、行内に他テキストを混ぜないこと
- **直後性**: センチネル行の次行以降で空行を任意個スキップした後の **最初の非空行が fence 開始**
  （trim 後 ` ``` ` 始まり）であること。fence 以外の非空行が先に来ると malformed として扱われる
- **fence 言語タグ**: ` ```sh ` / ` ```bash ` 等の言語タグは許容（タグ自体はコマンド中身に含まれない）
- **fence 終了**: 次に現れる ` ``` ` 行で閉じる。EOF まで閉じないと malformed
- **中身**: fence 内が trim 後すべて空だと malformed（空ブロック）。非空なら元の改行・インデントを
  保持してそのまま実行される

### 中身は散文ではなく実行可能コマンド

ブロックの中身は **実行可能なコマンドそのもの**として記述します（散文・説明箇条書き・タスク記述を
書かない）。複数行コマンドや `&&` / `||` / `;` 連結を含められます。watcher は中身を `bash -c` に
**そのまま**渡し、連結記号を watcher 側で解釈しません:

```markdown
<!-- stage-a-verify -->
```sh
shellcheck install.sh setup.sh &&
  actionlint .github/workflows/*.yml
```
```

### 既存 checkbox 規約・numeric ID 階層規約との非干渉

構造化 verify ブロックは **タスク行ではなく補助ブロック**です。本ファイル「Checkbox 形式の
必須化」節および [`design-review-gate.md`](./design-review-gate.md) の Budget overflow check /
checkbox enforcement check の判定パターンは、いずれも行頭 `- [ ]` / `- [ ]*` + numeric ID で
始まるタスク行を対象とします。センチネル行（`<!-- ... -->`）も fence 行（` ``` `）も fence 中身も
これらの判定パターンに **マッチしない**ため、ブロックを追加してもタスク件数カウント・checkbox
enforcement は一切影響を受けません。

### 配置場所

ブロックの配置場所は任意です（パースはセンチネル基準で見出しに依存しません）。推奨は `tasks.md`
末尾の `## Verify` 見出し配下にまとめる形ですが、`## Verify` 見出し自体は必須ではありません:

```markdown
## Verify

本 spec の実装後、watcher が再実行すべき verify コマンドを以下の構造化ブロックで宣言する。

<!-- stage-a-verify -->
```sh
npm test && npm run lint && npm run build
```
```

### パス存在前提（存在しないパスへの diff を含めない / #364）

verify ブロックに記述する **すべてのコマンド対象パスは、`tasks.md` commit 時点の作業ツリーに
存在すること**を必須要件とします。存在しないパスを `diff` / `cat` / `shellcheck` 等に渡すと、
watcher が REPO_DIR で独立再実行したときに「コード品質失敗」と区別の付かない非 0 exit を返し、
clean な実装でも Stage A が false-fail して `claude-failed` まで escalate する事故が起きます
（#364 = idd-codex 由来 #14 と同根の事象 / #362 で実害）。本制約は **構造化 verify ブロック**
（`<!-- stage-a-verify -->` + 直後 fence）と、構造化ブロックを持たない場合の **ヒューリスティック
抽出**（行頭 keyword 一致）の双方に等しく適用されます（同じ「watcher が `bash -c` に渡す
コマンド」という入力面を共有しているため）。

#### idd-claude 特有の注意: `local-watcher/` は `repo-template/` 配下にミラーされない

idd-claude self-hosting 環境では、`local-watcher/bin/*.sh` / `local-watcher/bin/modules/*.sh` は
`install.sh` 経由でユーザーホーム（`$HOME/bin/`）へ配布する構造を取っており、
**`repo-template/local-watcher/` というパスは存在しません**（`install.sh` の配置先と
`repo-template/` の配置先が別系統）。そのため、idd-claude の `tasks.md` に
`diff -r local-watcher/bin repo-template/local-watcher/bin` 等の「`repo-template/local-watcher/`
配下を比較対象にする」コマンドを verify に含めると、`diff` は exit=2 + stderr に
`No such file or directory` を返し verify が false-fail します。当該パターンの
コマンドは verify に **含めてはなりません**。

#### idd-claude における root ↔ repo-template 同期 diff の canonical 対象

idd-claude では root と `repo-template/` 配下が byte 一致で同期される対象は **`.claude/agents` と
`.claude/rules` の 2 系統のみ**です（CLAUDE.md「機能追加ガイドライン §4」の規約と整合）。
したがって idd-claude self-hosting で verify に置く同期 diff の canonical 形は以下に限定されます:

```sh
diff -r .claude/agents repo-template/.claude/agents
diff -r .claude/rules repo-template/.claude/rules
```

`workflow` / `labels` script / `CLAUDE.md` / `README.md` 等は **byte 一致対象外**（consumer 固有 /
別 PR で個別同期 / 構造的にミラーされない）であり、これらの `diff -r` を verify に含めても
意味のある検証にはなりません。

#### 存在の不確定なディレクトリへの diff には存在ガードを置く

他リポジトリの `tasks.md` で「2 つのディレクトリのうち一方が cleanup PR 等で消える可能性がある」
ような不確定パスを参照する必要がある場合、**`[ -d <path> ] && diff -r ...` 形のパス存在ガード**を
置く書式を canonical とします（パス不在を WARN 降格の対象ではなく **無音 skip** として扱える
ため、verify 全体の意図が明確になります）:

```sh
[ -d packages/old-module ] && [ -d packages/new-module ] && \
  diff -r packages/old-module packages/new-module
```

`stage-a-verify` 側にも「`diff` の exit=2 + `No such file or directory`」は WARN 降格として
扱う defense-in-depth（#364 Req 2）が入っていますが、これは **Architect 側のミス（存在しない
パスを verify に含めた）を実害化させないための救済ネット**であり、Architect は本節の制約で
そもそもパス不在を含めない責務を負います。WARN 降格に依存せず、verify 時点で確実に存在する
パスのみを書くこと。

### verify 対象が無い spec はブロックを省略できる

verify すべき build/test/lint コマンドが存在しない spec（純ドキュメント変更等）では、構造化ブロックを
省略して構いません。その場合 watcher は `STAGE_A_VERIFY_COMMAND` env → ヒューリスティック抽出 →
SKIPPED の順に fallback します（解決順序の詳細は README「Stage A Verify Gate (#125)」節を参照）。
構造化ブロックを持たない既存 spec は従来どおりヒューリスティック / env 経路で動作します（後方互換）。

なお、`design` モードを経由せず Architect が `tasks.md` を生成しない **design-less impl**
（tasks.md 不在。#204 等）は、構造化ブロック / ヒューリスティック抽出の入力となる `tasks.md`
自体が存在しないため stage-a-verify gate の **対象外（SKIP）**となります。これは未実装の
取りこぼしではなく「watcher は verify コマンドを推測しない」設計思想（#224 / #228 / #230）に
基づく **意図された仕様**であり、design-less impl の regression は Developer が実行するテストと
Reviewer の AC 判定で担保します（詳細は README「Stage A Verify Gate (#125)」節の
「design-less impl（tasks.md 不在）は gate 対象外」を参照）。

## 参考

- [cc-sdd `tasks.md` テンプレート](https://github.com/gotalab/cc-sdd/blob/main/.kiro/settings/templates/specs/tasks.md)
