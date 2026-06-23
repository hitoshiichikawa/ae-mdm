---
paths:
  - ".claude/rules/feature-flag.md"
---
<!-- 条件ロード（#327）: 上記 paths に触れるセッションにのみ自動付与される。frontmatter を削除すると全コンテキスト常時ロードに戻るため注意。 -->

# Feature Flag Protocol（規約詳細）

> **このファイルは opt-in 宣言したプロジェクトでのみエージェントが Read します。**
> 採否宣言は対象 repo の `CLAUDE.md` の `## Feature Flag Protocol` 節を参照してください。
> 宣言が無い／値が `opt-in` 以外なら、本ファイルは読み込まれず、エージェントは通常の単一実装パスで動作します。

## 概要

未完成機能を main にマージしても既存挙動を壊さないようにする実装パターンとして、
`if (flag) { 新挙動 } else { 旧挙動 }` 形式でコードを 2 系統持たせ、flag を初期値 false で
配置する規約です。本規約は **idd-claude が提示する書き方の取り決め**（規約）であって、
**LaunchDarkly / Unleash / GrowthBook 等の外部 Feature Flag SaaS との連携を扱う仕組みでは
ありません**（後述の Non-Goals 参照）。

メリット: 段階的な機能リリース・リスク隔離・main 上で機能完成を待たずに細かく PR を merge できる
デメリット: flag 残存による技術債、両系統テストのメンテナンスコスト

## 採否宣言の書式（Req 2.2）

対象 repo の `CLAUDE.md` に以下の専用節を 1 つ置きます（h2 固定）:

```markdown
## Feature Flag Protocol

> **デフォルトは opt-out です**

**採否**: opt-out

<!-- 採用する場合は上の行を `**採否**: opt-in` に変更し、本ファイル(.claude/rules/feature-flag.md)を確認 -->
<!-- idd-claude:feature-flag-protocol opt-out -->
```

- 宣言行は `**採否**: opt-in` または `**採否**: opt-out` の **1 行のみ**（lowercase, ハイフン区切り）
- マーカーコメント `<!-- idd-claude:feature-flag-protocol opt-in -->` は任意（grep 抽出用）
- 節が存在しない / 値が `opt-in` 以外（`opt-out` / 空 / 不正値 / typo）→ **opt-out として解釈**（Req 1.3）
- `enabled` / `Opt-In` / `opt_in` 等の typo は無効。**lowercase の `opt-in` のみが opt-in 扱い**

## Flag 命名と初期値（Req 2.3）

- **命名方針**: `<feature-name>_enabled`（例: `new_checkout_flow_enabled`）
  - lower_snake_case を推奨。プロジェクトの言語慣習に合わせて lowerCamelCase / kebab-case でも可
  - 動詞の能動形（`use_new_pricing`）よりも形容詞・状態形（`new_pricing_enabled`）を推奨
- **初期値**: `false`（既定で旧パスが選択される）
- **有効化条件の記述場所**: 環境変数 / 設定ファイル / プロジェクト固有の機構など
  - 言語非依存。ハードコード定数 / `process.env` / `os.getenv` / `viper` / `dotenv` 等
  - flag を導入したら **その flag 名と初期値を `impl-notes.md` または `README.md` に列挙**

## Implementer が満たすべき要件（Req 2.4 / 3.1 / 3.2 / 3.3）

opt-in プロジェクトの実装では、各タスクで以下のチェックリストを満たすこと:

- [ ] 旧パスを **削除せず温存** する（`if (flag) { 新挙動 } else { 旧挙動 }`）
- [ ] flag-on / flag-off の両パスが **同一テストスイート** で実行できる（テストケースは flag 値で枝分かれしてよい）
- [ ] flag-off パスの挙動は **本機能導入前と同一**（差分等価。リファクタ・型変更は可、挙動変更は不可）
- [ ] flag 名と初期値を `impl-notes.md` または `README.md` に列挙する
- [ ] 新規挙動の有効化条件（どの環境変数で true にするか等）を明文化する

## 両系統テスト（Req 5.1 / 5.2 / 5.3）

- 同一テストスイートを **flag-on / flag-off の 2 通りで実行する**
- いずれか 1 系統でも失敗したら **全体結果を失敗** として扱う
- 実行責務はプロジェクトが選択する（idd-claude 側で実行機構は提供しない）:
  - **(a) ローカル実行**: 開発者が `FLAG=on npm test && FLAG=off npm test` 等を都度実行
  - **(b) CI 実行**: CI matrix / job で 2 系統を並列実行
  - **(c) 規約上の指針提示にとどめ、各プロジェクトが自由に選択**

推奨: 機能が main に同居する期間が 2 週間を超えるなら CI 化を検討する。
短期間（数日以内に flag 削除 PR を出す前提）ならローカル実行で十分。

## クリーンアップ責務（Req 6.1 / 6.2 / 6.3）

- 全タスク（umbrella Issue 単位）完了後、**flag 定義と `if (flag)` 分岐を除去する別 PR を作成する義務** がある
- **起票責務**: **人間が umbrella Issue 完了時に手動で起票する**
  - 理由: Implementer エージェントは単一 Issue 文脈しか持たず、umbrella の完了判定が不可能
  - 推奨フロー: umbrella Issue に `cleanup: feature-flag-XXX` というサブ Issue を立て、auto-dev に流す
- **残存 flag の棚卸し閾値**: **同時に active flag が 5 個を超えたら棚卸し Issue を起票する**
  - 5 個は経験則。プロジェクト規模に応じて調整可
  - 「active flag 一覧」を `README.md` に列挙する運用を推奨（grep で件数を機械的に確認できる）

## Non-Goals（Req 2.5）

本規約は以下を扱いません。これらが必要なプロジェクトは、本規約とは別に専用ツールを導入してください:

- **LaunchDarkly / Unleash / GrowthBook 等の外部 Feature Flag SaaS との連携・移行**
- **Flag 値の動的変更**（A/B テスト・段階リリース・ユーザー属性別出し分け・カナリアリリース）
- **Flag テレメトリの自動収集**(採用率・有効化日時の自動集計）

これらの機能は本規約のスコープ外です。idd-claude 側でも提供しません（言語・基盤非依存性
のため。Req NFR 3.1）。

## 採用宣言サンプル（Req NFR 2.2）

### opt-in 例（規約を採用するプロジェクト）

```markdown
## Feature Flag Protocol

> **デフォルトは opt-out です**。本節を opt-in に変更すると、
> Implementer / Reviewer エージェントが flag 裏実装の規約に従って動作します。

**採否**: opt-in

<!-- idd-claude:feature-flag-protocol opt-in -->
<!-- 規約詳細: .claude/rules/feature-flag.md -->

### この規約を採用するメリット
- 未完成機能を main にマージしても既存挙動を壊さない
- 段階的な機能リリースが可能

### この規約を採用するデメリット
- flag 残存による技術債の管理コスト
- 両系統テストのメンテナンスコスト
```

### opt-out 例（規約を採用しないプロジェクト）

```markdown
## Feature Flag Protocol

> **デフォルトは opt-out です**。本節を削除する／値を `opt-in` 以外にする場合、
> 通常の単一実装パスで動作します。

**採否**: opt-out

<!-- idd-claude:feature-flag-protocol opt-out -->
```

## FAQ

- **Q: `enabled` と書いてしまった**
  A: opt-out として解釈されます。lowercase の `opt-in` のみが有効です（typo 安全側に倒す設計）
- **Q: 既存プロジェクトはどうなる？**
  A: 本節を追加していなければ従来通り opt-out 扱い。後方互換性を保証します（NFR 1.1）
- **Q: 言語固有の例は？**
  A: 本規約は言語非依存（NFR 3.1）。具体的な flag 機構（環境変数 / 設定ファイル / DI 等）は
  各プロジェクトの言語慣習に合わせてください

## 参考

- 採否宣言の節: 対象 repo の `CLAUDE.md` `## Feature Flag Protocol`
- Implementer / Reviewer のフロー追記: `.claude/agents/developer.md` / `.claude/agents/reviewer.md`
