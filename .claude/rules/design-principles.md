---
paths:
  - "docs/specs/**/design.md"
---
<!-- 条件ロード（#327）: 上記 paths に触れるセッションにのみ自動付与される。frontmatter を削除すると全コンテキスト常時ロードに戻るため注意。 -->

<!-- SPDX-License-Identifier: MIT -->
<!-- Adapted from cc-sdd (https://github.com/gotalab/cc-sdd), MIT License, Copyright (c) gotalab -->

# design.md 記述原則

Architect が `design.md` を書く際の全体方針と必須セクションの設計思想。

## 目的

異なる実装者の間で**実装の一貫性**を担保するのに十分な詳細を提供し、解釈のズレを防ぐこと。

## アプローチ

- 実装判断に直接影響する必須セクションを含める
- 実装ミスの防止に不可欠でない optional セクションは省略する
- 詳細度は機能の複雑度に合わせる
- 散文より **図・表** を優先する

## 外部仕様の不確実性を Web 検索で解消する

外部ツール・ライブラリ・API・CLI コマンドの仕様に依拠した設計判断は、モデル知識のカットオフや
細部仕様の曖昧さによって推測ベースで確定しやすく、Developer フェーズで仕様乖離が判明する
事故の温床になります。設計品質の取りこぼしを減らすため、Architect は不確実な外部仕様について
Web 検索で一次情報（公式ドキュメント・公式 GitHub README / issue・公式 changelog 等）を確認
してから設計判断を行ってください。

### 検索対象のスコープ

- **対象**: 外部ツール・ライブラリ・API・CLI コマンドの仕様（オプションフラグ・返り値・互換性・
  既知の制約等）
- **対象外**: idd-claude 内部の既存仕様・既存実装（既存ドキュメント・既存コード・既存テストの
  参照を優先）、些末な書式・命名選択など外部仕様に直接依拠しない判断

### 適用の最小化

「不明な場合・新規ツール導入時に限定」して必要最小限に留めます。既に確信が持てる仕様や、
内部完結する判断のためだけに Web 検索を起動しないでください。

### 検索結果のリンク記録（推奨止め）

検索結果のリンクを `design.md` の該当セクション本文または `## Supporting References` 等の
optional セクションに残すことを **推奨**します。記録時は短縮表記（例: `<https://example.com/docs/foo>`）
で十分です。ただし **義務化はしません**。リンクを記録するか否かは Architect の裁量に委ねます
（記録の有無を理由に design-review-gate で reject しません）。

## 分量バジェット（#331）

> 本節の「分量」は design.md の**行数**を指します。`design-review-gate.md` の
> **Budget overflow check**（tasks.md のタスク**件数**ゲート / #147・#216）とは別概念です。

design.md の行数は **書く時間より読まれる回数が支配的**であることを前提に、複雑度に応じた
目安へ収めます（Developer / Reviewer / PR iteration が繰り返し入力として読み込むため、
肥大は出力トークンと再読入力トークンの両面で多段に増幅される）:

| 複雑度 | 目安 | 例 |
|---|---|---|
| 軽微（1〜2 ファイル / 単一責務） | **≤ 150 行** | 単一関数の追加・小規模 opt-in env |
| 標準（単一モジュール + 配線） | **≤ 300 行** | 新規 module 1 つ + call site 接続 |
| 複雑（複数モジュール横断 / 状態機械変更） | **≤ 600 行** | stage 状態機械の変更・新 processor |

- 目安超過は即 reject 対象ではないが、超過する場合は **「なぜ短くできないか」を design.md
  冒頭（Overview 直後）に 1〜3 行で明記**する
- **1000 行を超えたら複雑すぎる**: 機能の複雑度が高すぎる可能性があります。設計の単純化、
  もしくは複数 spec への分割（複数 Issue に切り直し）を検討してください

### 簡潔化の規律（バジェット内に収める手段）

- **既存コード片の逐語転載をしない**: 既存コードは `file:line` 参照で指す（転載は乖離の温床）。
  新規コードの提示は契約（関数名・引数・戻り値・エラー値）までに留め、実装本文は Developer に
  委ねる
- **Requirements Traceability は 1 要件 1 行**（表形式。設計判断の説明を重複記載しない）
- 繰り返し構造は 1 回だけ記述し「同パターン」と参照する（File Structure Plan の既存方針を
  全セクションへ適用）
- 散文より図・表を優先（前述のアプローチを分量面からも徹底する）

## セクション順序の柔軟性

標準順序（Overview → Architecture → File Structure Plan → …）から逸脱して、
`Requirements Traceability` を前に配置したり、`Data Models` を `Architecture` の近くに
置いたりしても構いません。読みやすさのため調整してください。

各セクション内では **Summary → Scope → Decisions → Impacts/Risks** の流れを保ち、
レビュワーが一貫してスキャンできるようにします。

## 必須セクション

| セクション | 目的 | 備考 |
|---|---|---|
| Overview | 機能の狙い・ユーザー・インパクト | 2-3 段落で簡潔に |
| Goals / Non-Goals | 明示的な対象と除外 | スコープ境界を誤読されない |
| Architecture Pattern & Boundary Map | 採用パターンと境界 | 複雑機能では Mermaid 図必須 |
| Technology Stack | レイヤごとの技術選定 | 表形式 |
| File Structure Plan | ディレクトリと責務 | **tasks.md の `_Boundary:_` を駆動する** |
| Requirements Traceability | req ID → 設計要素 | 複雑機能のみ（単純 1:1 なら Components で代替可） |
| Components and Interfaces | コンポーネント詳細 / 契約 | Service / API / Event / Batch / State の契約を明示 |
| Data Models | ドメイン / 論理 / 物理モデル | 該当する範囲のみ |
| Error Handling | エラー戦略・カテゴリ・監視 | 具体的な例を含める |
| Testing Strategy | 単体／結合／E2E／性能 | 各 3-5 項目 |

## Optional セクション（必要時のみ）

- **Security Considerations**: 認証・機密情報を扱う機能、外部連携、ユーザー権限が絡む機能
- **Performance & Scalability**: 性能目標・高負荷・スケール懸念がある機能
- **Migration Strategy**: スキーマ・データ移動を伴う場合。Mermaid flowchart で段階を可視化
- **Supporting References**: 長大な型定義・ベンダー比較表など、本文で読みにくい付随情報

## File Structure Plan の書き方

- **小機能**: 個別ファイル単位で責務を列挙
- **大機能**: ディレクトリレベル + per-domain パターンを記述、非自明なファイルのみ個別列挙

例:

```
src/
├── domain-a/              # Domain A responsibility
│   ├── controller.ts      # Endpoint handlers
│   ├── service.ts         # Business logic
│   └── types.ts           # Domain types
├── domain-b/              # Domain B (same pattern as domain-a)
└── shared/
    └── cross-cutting.ts   # Non-obvious: why this exists
```

- 繰り返し構造の場合は、パターンを 1 回だけ記述（"domain-b follows same pattern as domain-a"）
- 個別ファイルはその責務がパスから自明でない場合のみ個別列挙

## 参考

- [cc-sdd `design.md` テンプレート](https://github.com/gotalab/cc-sdd/blob/main/.kiro/settings/templates/specs/design.md)
