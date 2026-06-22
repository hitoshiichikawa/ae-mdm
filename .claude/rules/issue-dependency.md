---
paths:
  - ".claude/rules/issue-dependency.md"
---
<!-- 条件ロード（#327）: 上記 paths に触れるセッションにのみ自動付与される。frontmatter を削除すると全コンテキスト常時ロードに戻るため注意。 -->

# Issue 間依存関係ガイド

idd-claude では tasks.md 内のタスク間依存（`_Depends:_`）は
[`tasks-generation.md`](./tasks-generation.md) で正式ルール化されていますが、
**Issue 間 / cross-Issue の依存関係を Issue 本文で表現するフォーマット**は本ファイルで規定します。
PM agent の起票・分割・要件化、Triage agent のブロッキング判定、Architect / Developer の
前提理解、人間運用者の Issue 棚卸し、これらすべての参照点になることを目的とします。

## 目的

関係種別ごとに **canonical 記法** を 1 つに固定し、過去 Issue で混在していた alias 表記
（`前提依存:` / `Blocked by:` / `Umbrella:` / `分割元:` など）を **canonical と等価**として
扱うことで、表現の揺れを吸収しつつ将来の自動化（依存グラフ可視化、Triage プロンプト精査、
auto-dev ブロッキング判定など）に必要な機械可読性を確保します。

## 関係種別（canonical 5 種）

| 関係種別 | canonical 記法 | tool-parse | 意味 | ブロッキング性 |
|---|---|---|---|---|
| 前提依存 | `Depends on: #N` | **必須** | 当該 Issue を着手するために事前完了が必要な Issue | **あり**（依存先未解決 = ブロック状態） |
| 親子関係 | `Parent: #N` | **必須** | 当該 Issue を含む上位（umbrella を含む）の親 Issue | なし（参照用） |
| 分割元 | `Split from: #N` | 推奨 | 元 Issue を分割した結果として生まれたことを示す | なし（履歴用） |
| 兄弟関係 | `Sibling: #N` | informational | 同じ親に属する並列 Issue を相互参照する | なし |
| 関連参照 | `Related: #N` | informational | 直接依存ではない参考リンク（過去議論・類似 Issue 等） | なし |

> Note: `Parent` は umbrella Issue（複数 sub-task をまとめる管理 Issue）と通常の親子の
> 双方を含みます。区別が必要な場合は **本文中で説明**する運用とし、関係種別自体を
> 分離しません（過去の `Umbrella:` 表記は alias として等価扱い）。

## canonical 配置場所

Issue 本文に `## 関連` セクション（英語 repo では `## Related`）を 1 つ設け、その配下に
関係種別を列挙します。リポジトリ内の他セクションには記載しません。

```markdown
## 関連

- Depends on: #12 #34
- Parent: #5
- Related: #88
```

英語 repo 例:

```markdown
## Related

- Depends on: #12
- Parent: #5
- Split from: #100
```

## 複数値の表記

1 関係 = 1 行を canonical とし、複数の Issue 番号を続ける場合はスペース区切り、
カンマ区切りも許容します。

| 表記 | 採否 | 例 |
|---|---|---|
| スペース区切り | canonical | `Depends on: #1 #2 #3` |
| カンマ区切り | 許容 | `Depends on: #1, #2, #3` |
| 1 関係 = 複数行（同じキーを繰り返す） | 非推奨 | `Depends on: #1` を 2 行書く |

複数値の場合でも各番号は `#N`（GitHub Issue 番号記法）で書きます。owner/repo prefix が
必要なクロスリポ参照は `owner/repo#N` 形式で書いてよいですが、現状運用では本 repo 内
参照に限定する想定です。

## 互換 alias マッピング

既存 Issue で使われた alias 表記は canonical と等価扱いです。新規 Issue は canonical を
選択してください。

| alias 表記 | canonical | 備考 |
|---|---|---|
| `前提依存: #N` | `Depends on: #N` | 日本語 alias。既存 Issue で多用 |
| `Blocked by: #N` | `Depends on: #N` | GitHub 慣習表記。ブロッカ Issue を被ブロッキング側に書く |
| `親 Issue: #N` | `Parent: #N` | 日本語 alias |
| `Umbrella: #N` | `Parent: #N` | umbrella と Parent を統合（区別は本文で説明） |
| `分割元: #N` | `Split from: #N` | 日本語 alias |

## 逆ブロッキング（`Blocks: #N`）の扱い

「この Issue が他の Issue をブロックしている」という **逆方向の関係** を `Blocks: #N` で
書く運用は **canonical では採用しません**。代わりに被ブロッキング側の Issue 本文で
`Depends on: #N` として記述します。理由は以下:

- ブロッキング状態を判定するのは「依存される側」より「依存する側」が知る方が自然
  （新しい Issue が古い Issue に依存することの方が情報フローとして正しい）
- 双方向の同期が崩れたときに不整合が出るのを避ける
- Triage agent / auto-dev のブロッキング判定が片側のみを精査すれば済む

旧 Issue が `Blocks: #N` を本文に持つ場合は alias として informational に扱い、被ブロッキング
側を新規起票する際に `Depends on:` で書き直す運用とします（既存の retrofit は不要）。

## 適用範囲

- **新規 Issue 起票時に適用**: PM agent / 人間運用者は新規 Issue で canonical 記法を選択する
- **既存 Issue の retrofit は不要**: 過去に alias で書かれた Issue を canonical に書き換える
  作業は本ルール導入の必須前提としません（NFR 1.1 互換維持）
- **強制レベルは should（推奨）**: CI lint / pre-commit hook 等の自動チェックは導入していません。
  PM agent の指針（`product-manager.md`）が canonical 採用を促す形で運用されます

## 言語方針との整合

CLAUDE.md「言語方針」では Issue 本文・コメントは日本語ベースですが、本ルールで定める
canonical 記法（`Depends on:` / `Parent:` / `Split from:` / `Sibling:` / `Related:`）は
**識別子・コマンド名・env var 名と同じ枠**として英語固定とします。理由:

- 機械パース時の robustness（日本語の正規表現は文字幅・正規化の罠が多い）
- OSS としての汎用性（idd-claude を install した非日本語 repo でも同じ canonical が使える）
- 関係種別語彙は限定された語彙集合であり、自然文ではなく **キー** に近い

Issue 本文の説明文や項目間の補足は日本語ベースのままで構いません。canonical 記法の
**キー部分のみ**英語固定です。

## 例示

### canonical 表記

```markdown
## 関連

- Depends on: #150
- Parent: #146
```

### alias 表記（既存 Issue 互換）

```markdown
## 関連

- 前提依存: #150
- 親 Issue: #146
- Umbrella: #100
```

### 複数値

```markdown
## 関連

- Depends on: #12 #34 #56
- Sibling: #20, #21, #22
```

### 逆ブロッキング（非推奨。被ブロッキング側で `Depends on:` を使う）

```markdown
<!-- NG: Issue #100 本文に書く -->
## 関連

- Blocks: #150

<!-- OK: Issue #150 本文に書く -->
## 関連

- Depends on: #100
```

## 参考

- 関連ルール: tasks 内の依存表現は [`tasks-generation.md`](./tasks-generation.md) の
  `_Depends:_` アノテーション規約を参照（cross-Issue 依存と cross-task 依存は別レイヤ）
- 出典: 本ルールは cc-sdd 原典に対応物が存在せず、idd-claude 独自規約として制定したもの
