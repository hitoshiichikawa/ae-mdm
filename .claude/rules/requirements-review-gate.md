---
paths:
  - "docs/specs/**/requirements.md"
---
<!-- 条件ロード（#327）: 上記 paths に触れるセッションにのみ自動付与される。frontmatter を削除すると全コンテキスト常時ロードに戻るため注意。 -->

<!-- SPDX-License-Identifier: MIT -->
<!-- Adapted from cc-sdd (https://github.com/gotalab/cc-sdd), MIT License, Copyright (c) gotalab -->

# 要件定義レビューゲート（PM 自己レビュー）

Product Manager が `requirements.md` を書き終える前に、このゲートに従ってドラフトをレビューし、
問題があれば修正、問題なければ確定します。

## スコープ・カバレッジレビュー

- 主要なユーザー動線、スコープ境界、主要なエラーケース、ユーザー／運用者から見える edge condition
  をカバーしているか
- 業務・ドメインルール、コンプライアンス制約、セキュリティ／プライバシー要件、運用制約のうち、
  ユーザー可視の挙動に影響するものが明示されているか
- カバー不足が draft の不完全さなら → ドラフトを修正して再レビュー
- カバー不足が Issue 記述・既存ドキュメントの曖昧さが原因なら → 推測せず `確認事項` に
  列挙して、Issue コメントでの人間エスカレーションを提案

## EARS・テスト可能性レビュー

- すべての AC が [`ears-format.md`](./ears-format.md) に準拠しているか
- すべての要件が testable / observable / specific か
- 実装詳細が紛れ込んでいないか（→ `design.md` の領分なので requirements から除外）
- 要件見出しが **numeric ID のみ** であること（`Requirement 1`, `1.1` など。英字 ID `Requirement A` は不可）

## 構造・品質レビュー

- 関連する挙動をまとまった要件エリアにグルーピングし、同じ義務を複数箇所に重複記述していないか
- スコープの包含・除外境界が誤読されない程度に明確か
- 非機能要件が user-observable または operator-observable な粒度か
  （技術選定・内部構造は design に委ねる）
- "fast" "robust" "secure" 等の曖昧語を具体化しているか（[`ears-format.md`](./ears-format.md) 参照）

## Mechanical Checks（自動的に確認できる項目）

判断レビューの前に、機械的にチェックします:

- **Numeric ID の確認**: すべての要件見出しに numeric ID がある（`1`, `1.1`, `2` など）。
  見出し ID 欠落を scan
- **AC の存在**: すべての要件に EARS 形式の AC が 1 つ以上ある
  （`When` / `If` / `While` / `Where` / `The <system> shall` のいずれかで始まる文が含まれる）
- **実装語彙の混入チェック**: DB 名・フレームワーク名・API パターン等の技術用語が混入していないか scan

### `/goal` による自動ループ運用（Claude Code v2.1.139+）

Claude Code v2.1.139 以降では、上記 3 つの Mechanical Checks を `/goal` の完了条件として
宣言し、未達なら自動で次ターンを実行する運用が可能です。**v2.1.139 未満の環境では本節
全体をスキップし、後述の「レビュー・ループ」節の従来手順（Mechanical Checks → 判断レビュー
→ 最大 2 パス）をそのまま適用してください**（後方互換）。

#### 適用タイミング

PM エージェントが `requirements.md` ドラフトを確定する直前、判断レビューを通過した段階で
`/goal` を発行します。順序は「Mechanical Checks の `/goal` 自動ループ → 判断レビュー → 確定」
を推奨します（判断レビューを先に手で済ませてから `/goal` で機械検出だけを締める運用も可）。

#### PM 向け完了条件文字列テンプレ例

以下のいずれかを `/goal <条件>` の `<条件>` 部に貼り付けて発行します（自然言語の AND
結合で記述し、EARS トリガーキーワード `When` / `If` / `While` / `Where` / `shall` は混ぜない）:

```
すべての requirement 見出しに numeric ID（1, 1.1, 2 等）があり、
かつ すべての requirement に EARS 形式 AC（When / If / While / Where / The <subject> shall のいずれかで開始）が 1 件以上あり、
かつ AC 本文に DB 名・フレームワーク名・API パターン等の実装語彙が混入していない
```

短縮版:

```
requirements.md の全要件見出しが numeric ID であり、各要件に EARS 形式 AC が 1 件以上あり、実装語彙の混入がない
```

#### ターン上限の併記

`/goal` 自動ループのターン上限は、後述「レビュー・ループ」節の **最大 2 パス**を流用します
（撤廃ではなく併記）。`/goal` が 2 ターン経過しても完了条件を満たさない場合は、自動ループ
を終了し、人間エスカレーション（Issue コメントで判断を仰ぐ）または要件フェーズ戻し
（Issue に差し戻して情報補完を依頼）を選択します。

## レビュー・ループ

- Mechanical Checks を先に実施、続いて判断レビュー
- 問題が draft 内で閉じるなら修正して再レビュー
- **最大 2 パス**で確定するか、人間エスカレーションを選ぶ（無限ループを避ける）
  - Claude Code v2.1.139+ では上記「`/goal` による自動ループ運用」節の手順で Mechanical Checks 部分を自動収束させる
  - v2.1.139 未満では本節の手順をそのまま実行する（従来挙動と完全一致）
- ゲート通過後に `requirements.md` を確定させる

## 参考

- [cc-sdd `requirements-review-gate.md`](https://github.com/gotalab/cc-sdd/blob/main/.kiro/settings/rules/requirements-review-gate.md)
