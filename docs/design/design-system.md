# Design System — トークン確定版

> `design-system.html` から抽出した実トークン。実装時に **`frontend/shared` の Tailwind テーマ**
> （`tailwind.config.ts` の `theme.extend.colors` + CSS 変数）へ落とす起点。
> テーマ切替は **`[data-theme="dark"]`** 属性。**ライト / ダーク両対応**。

## フォント

- Sans（UI 既定）: `"Noto Sans JP", system-ui, sans-serif`
- Mono（数値・ID 等）: `'IBM Plex Mono', monospace`

## カラートークン（light / dark）

### サーフェス・テキスト・ボーダー

| token | light | dark | 用途 |
|---|---|---|---|
| `--bg` | `#ffffff` | `#0d1017` | ページ背景 |
| `--surface` | `#f7f8fa` | `#151922` | サーフェス |
| `--surface-2` | `#eef0f4` | `#1b212c` | サーフェス（2 段目） |
| `--elevated` | `#ffffff` | `#171c25` | 浮いた面（カード / ポップオーバー） |
| `--border` | `#e4e7ec` | `#262d3a` | 既定ボーダー |
| `--border-strong` | `#d3d8e0` | `#323a49` | 強調ボーダー |
| `--text` | `#1b1f27` | `#e7eaf0` | 本文 |
| `--text-secondary` | `#5a616e` | `#9aa2b1` | 副次テキスト |
| `--text-muted` | `#6b7480` | `#8a909c` | 補助 / ヒント |

### Primary（インディゴ）

| token | light | dark |
|---|---|---|
| `--primary` | `#4f46e5` | `#6366f1` |
| `--primary-hover` | `#4338ca` | `#7c80f7` |
| `--primary-bg` | `#eef0fe` | `rgba(99,102,241,.16)` |
| `--primary-border` | `#c9cdfa` | `rgba(99,102,241,.42)` |
| `--primary-fg` | `#ffffff` | `#ffffff` |

### セマンティック（状態色）— ドメイン対応

| ramp | base (light / dark) | bg (light / dark) | fg (light / dark) | ドメイン用途 |
|---|---|---|---|---|
| `success` | `#16a34a` / `#22c55e` | `#e8f6ed` / `rgba(34,197,94,.14)` | `#15803d` / `#4ade80` | **準拠** |
| `danger` | `#dc2626` / `#ef4444` | `#fdeaea` / `rgba(239,68,68,.14)` | `#b91c1c` / `#f87171` | **非準拠** / WIPE 等の危険操作 |
| `warning` | `#d97706` / `#f59e0b` | `#fdf2e0` / `rgba(245,158,11,.14)` | `#b45309` / `#fbbf24` | **同期遅延** |
| `info` | `#2563eb` / `#3b82f6` | `#e8f0fe` / `rgba(59,130,246,.16)` | `#1d4ed8` / `#60a5fa` | 情報 |
| `neutral` | `#6b7280` / `#94a3b8` | `#eef0f3` / `rgba(148,163,184,.14)` | `#4b5563` / `#cbd5e1` | 未確認 / 中立 |
| `purple` | `#7c3aed` / `#8b5cf6` | `#f1ebfd` / `rgba(139,92,246,.16)` | `#6d28d9` / `#a78bfa` | 区分（管理モード等） |
| `pale` | — | `#f3f4f6` / `rgba(148,163,184,.07)` | `#7b8494` / `#9ca3af` | 最淡 / サポート対象外 |

### 効果・密度

- `--row-py: 7px` — テーブル行の縦 padding（**データ密 admin** の密度）
- `--shadow` — light: `0 1px 2px rgba(16,20,30,.04), 0 1px 3px rgba(16,20,30,.06)` / dark: `0 1px 2px rgba(0,0,0,.4)`
- `--shadow-pop` — light: `0 4px 16px rgba(16,20,30,.12), 0 1px 3px rgba(16,20,30,.08)` / dark: `0 8px 28px rgba(0,0,0,.55)`

## バッジ色の対応（ブリーフ §4.7 と一致）

| 種類 | 値 → ramp |
|---|---|
| コンプライアンス | 準拠=`success` / 非準拠=`danger` / 同期遅延=`warning` / 未確認=`neutral` / サポート対象外=`pale` |
| 管理モード | Fully Managed=`info` / Dedicated(Kiosk)=`purple` |
| コマンド状態 | 実行待ち=`neutral` / 成功=`success` / 失敗=`danger` / タイムアウト=`warning` |
| テナント状態 | バインド未完了=`warning` / 有効=`success` / 無効=`neutral` |

## 実装への落とし方（frontend/shared）

1. `:root`（light）+ `[data-theme="dark"]` で上記 CSS 変数を定義
2. `tailwind.config.ts` の `theme.extend.colors` を CSS 変数参照にマップ（例: `primary: 'var(--primary)'`）
3. shadcn/ui のテーマ変数（`background` / `foreground` / `primary` / `destructive` / `muted` 等）を上記にエイリアス
4. フォント: UI = Noto Sans JP、数値・ID = IBM Plex Mono

> 元データ: [design-system.html](design-system.html)（ブラウザで描画して確認可）
