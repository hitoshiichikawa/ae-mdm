# Design assets（Interface design / HTML リファレンス）

Claude Design で作成した **HTML デザインの正**（`*.dc.html` 由来）。各画面の HTML をコンソール別に配置。

> **位置づけ**: これは「デザインの参照資料」であり「フロント実装そのものではない」。
> 実装フェーズ（フロント Issue #12〜#21）で **React + shadcn/ui + Tailwind に移植**する。
> したがって `frontend/` には置かず、本ディレクトリで参照資料として管理する。
> HTML はブラウザで描画して確認でき（各フォルダの `support.js` 同梱）、Claude Code / Developer は Read できる。

## 構成

```
docs/design/
├── design-system.html     # 横断: 色/タイポ/余白トークン + 共通コンポーネント
├── design-system.md       # 上記から抽出したトークン確定版（実装の起点）
├── support.js
├── tenant-console/        # 顧客 IT 管理者向け（10 画面）+ support.js
└── admin-console/         # SaaS 運用者向け（5 画面）+ support.js
```

## 画面マッピング（配置済み・全画面 OK）

### tenant-console（10）

| 画面 | ファイル | 対応 Issue | brief |
|---|---|---|---|
| ログイン | `tenant-console/login.html` | #12 | §7.1 |
| ダッシュボード | `tenant-console/dashboard.html` | #13 | §7.12 |
| デバイス一覧 | `tenant-console/devices.html` | #13 | §7.3 |
| デバイス詳細（コマンド UI 内包） | `tenant-console/device-detail.html` | #13 / #14 | §7.4 |
| エンロール | `tenant-console/enrollment.html` | #15 | §7.5 |
| ポリシー一覧 | `tenant-console/policies.html` | #16 | §7.6 |
| ポリシー編集（5 領域タブ） | `tenant-console/policy-editor.html` | #16 | §7.7 |
| アプリ配信 | `tenant-console/apps.html` | #17 | §7.8 |
| 監査ログ | `tenant-console/audit.html` | #18 | §7.9 |
| 管理者 & ロール | `tenant-console/admins.html` | #18 | §7.10 |

### admin-console（5）

| 画面 | ファイル | 対応 Issue | brief |
|---|---|---|---|
| アプリシェル | `admin-console/shell.html` | #19 | §4.1(admin) |
| テナント管理 | `admin-console/tenants.html` | #20 | §7.11 |
| 横断ダッシュボード | `admin-console/overview.html` | #21 | §7.13 |
| 未割当（退避キュー） | `admin-console/unassigned.html` | #21 | §7.13 |
| 横断監査ログ | `admin-console/audit.html` | #21 | §7.13 |

> 補足: **コマンド UI（#14）** は `device-detail.html` に内包。**admin ログイン**は tenant の `login.html` パターンを流用想定。

## 実装時の使い方

- トークンは [design-system.md](design-system.md) を `frontend/shared` の Tailwind テーマへ落とす
- 各 Issue（#12〜#21）の「参考資料」から該当 HTML を参照 → React + shadcn/ui へ移植
