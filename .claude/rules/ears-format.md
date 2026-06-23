---
paths:
  - "docs/specs/**/requirements.md"
---
<!-- 条件ロード（#327）: 上記 paths に触れるセッションにのみ自動付与される。frontmatter を削除すると全コンテキスト常時ロードに戻るため注意。 -->

<!-- SPDX-License-Identifier: MIT -->
<!-- Adapted from cc-sdd (https://github.com/gotalab/cc-sdd), MIT License, Copyright (c) gotalab -->

# EARS 記法ガイド

idd-claude の受入基準（Acceptance Criteria）は **EARS**（Easy Approach to Requirements Syntax）
で記述します。曖昧性を排除し、機械検証可能な形式にするためです。

## 基本パターン

### 1. Event-Driven（イベント駆動）

- **Pattern**: `When [event], the [system] shall [response/action]`
- **Use Case**: 特定のイベント・トリガーへの応答
- **Example**: `When user clicks checkout button, the Checkout Service shall validate cart contents`

### 2. State-Driven（状態駆動）

- **Pattern**: `While [precondition], the [system] shall [response/action]`
- **Use Case**: 状態や前提条件に依存する挙動
- **Example**: `While payment is processing, the Checkout Service shall display a loading indicator`

### 3. Unwanted Behavior（異常系）

- **Pattern**: `If [trigger], the [system] shall [response/action]`
- **Use Case**: エラー・失敗・望ましくない状況への応答
- **Example**: `If an invalid credit card number is entered, the Checkout Service shall display an error message`

### 4. Optional Feature（条件付き機能）

- **Pattern**: `Where [feature is included], the [system] shall [response/action]`
- **Use Case**: オプション・条件付き機能の要件
- **Example**: `Where two-factor authentication is enabled, the Auth Service shall request a verification code`

### 5. Ubiquitous（普遍）

- **Pattern**: `The [system] shall [response/action]`
- **Use Case**: 常時有効な要件、システムの基本特性
- **Example**: `The session cookie shall have the HttpOnly flag set`

## 複合パターン

- `While [precondition], when [event], the [system] shall [response/action]`
- `When [event] and [additional condition], the [system] shall [response/action]`

## 記述ルール

- **トリガーキーワードは英語固定**: `When` / `If` / `While` / `Where` / `The [system] shall`
  - 日本語化するのは `[event]` `[precondition]` `[trigger]` `[feature]` `[response/action]` の可変部のみ
  - 例: `When ユーザーが登録ボタンを押下したとき, the User Service shall 確認メールを送信する`
  - トリガーや `shall` の部分に日本語を混ぜ込まない
- **subject の選び方**:
  - ソフトウェア: 具体的なサービス名・モジュール名（例: "Checkout Service", "User Auth Module"）
  - 業務プロセス: 責任を持つチーム・ロール（例: "Support Team", "Review Process"）
- **shall / should**:
  - `shall` — 必須の挙動
  - `should` — 推奨される挙動
  - `may` や曖昧語は避ける

## 品質基準

- **テスト可能・検証可能**であること（1 つの AC に対して 1 つ以上のテストケースを書ける）
- **1 AC = 1 挙動**。複数の挙動を 1 文に混ぜない
- **実装詳細を含めない**: データベース名・フレームワーク名・API パターン等は `design.md` の領分
- **曖昧語の具体化**: "fast" → "200ms 以内"、"robust" → "connection loss 時に 3 回まで自動リトライ" のように数値化

## 参考

- [cc-sdd `ears-format.md`](https://github.com/gotalab/cc-sdd/blob/main/.kiro/settings/rules/ears-format.md)
