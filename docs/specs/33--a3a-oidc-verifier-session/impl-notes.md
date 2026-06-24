# Implementation Notes

本ファイルは Issue #33（A3a: OIDC Verifier + Session 管理）の per-task 実装ループにおける
learning を `### Task <id>` 単位で追記する。`docs/specs/33--a3a-oidc-verifier-session/` 配下の
`requirements.md` / `design.md` / `tasks.md` は実装 PR では書き換えず、矛盾は本ファイル末尾の
「確認事項」節に記録する。

## Implementation Notes

### Task 1

- **採用方針**: タスク `1` は umbrella header（`_Requirements:_` / `_Boundary:_` を持たない親 task）
  であり、本起動では直接の実装は無く、子 task 1.1〜1.4（Config 拡張 / migration / httpserver 公開化 /
  logger redaction）が後続 iteration で順次実装される前提として `## Implementation Notes` 構造のみ
  整備する。
- **重要な判断**:
  - 親 task は `tasks.md` 上で `### Task 1` の learning スロットを成立させるためのプレースホルダ
    に留め、コード変更・migration 追加・テスト追加は本 iteration では行わない（実装本体は 1.1〜
    1.4 の各 fresh iteration が担当する設計）。
  - per-task ループ規約「1 commit = 1 task ID」に従い、本 iteration の marker commit は `docs(tasks):
    mark 1 as done` 単一の subject で `tasks.md` のみを含める。impl-notes.md セットアップは marker
    commit と分離した別 commit に積む。
- **残存課題**: 子 task 1.1（Config）/ 1.2（migration）/ 1.3（httpserver 公開化）/ 1.4（logger
  redaction）の各実装は後続 fresh iteration で消化する。子 task 全完了時に親 task `1` の昇格は
  既に本 iteration で完了済みのため、子完了時の auto-promotion 規約は no-op として扱う。

## 確認事項

本セクションは `requirements.md` / `design.md` / `tasks.md` 本文の書き換えを伴わずに、実装フェーズ
で気付いた spec との矛盾点・人間判断が必要なポイントを集約する場である。

- なし（本 iteration は親 task の administrative iteration のみで、design 本文との矛盾点は発生して
  いない）。
