# Requirements Document

## Introduction

管理者が自テナントの端末について、現況・コンプライアンス状態・同期遅延を把握できないと、
紛失・非準拠・管理下離脱といったリスクへの対応が遅れる。本 Issue は、端末インベントリの
一覧・詳細・コンプライアンス分類・同期遅延を可視化する読み取り系機能と、SaaS 運営者向けの
全テナント横断サマリ、そして STATUS_REPORT 通知を契機とした端末属性の更新を対象とする。
端末属性は HTTP からの直接書込みではなく通知駆動でのみ反映される点を不変条件とする。
本要件は umbrella spec（`docs/specs/24-android-enterprise-emm-mvp/`）の Requirement 5・
NFR 1.2 / 3.2 のバックエンド実装スライスであり、それらと矛盾しないよう numeric ID で再構成する。

## 関連

- Parent: #24
- Depends on: #39

## Requirements

### Requirement 1: 端末一覧（自テナント限定・フィルタ・ページング）

**Objective:** As a TenantAdmin / Operator / Viewer, I want 自テナントの端末を絞り込み・ページング付きで一覧取得したい, so that 管理対象端末の現況を効率的に把握できる

#### Acceptance Criteria

1. When 管理者が端末一覧を要求したとき, the Device Service shall 自テナントに属する端末のみを一覧として返す（umbrella Req 5.1）
2. When 管理者がコンプライアンス分類でのフィルタ（準拠 / 非準拠 / 未確認 / サポート対象外 のいずれか）を指定して一覧を要求したとき, the Device Service shall 指定分類に一致する自テナント端末のみを抽出して返す（umbrella Req 5.4）
3. When 管理者が管理モードでのフィルタ（Fully Managed / Dedicated のいずれか）を指定して一覧を要求したとき, the Device Service shall 指定モードに一致する自テナント端末のみを抽出して返す
4. When 管理者が同期遅延でのフィルタを指定して一覧を要求したとき, the Device Service shall 同期遅延と判定される自テナント端末のみを抽出して返す（umbrella Req 5.6）
5. When 管理者がページング指定（ページ番号または取得件数）を伴って一覧を要求したとき, the Device Service shall 指定範囲に収まる端末のみを返す
6. If 管理者が未定義のフィルタ値（許可されないコンプライアンス分類・管理モード・同期遅延の指定値）を指定したとき, the Device Service shall 当該要求を不正として拒否し、端末一覧を返さない
7. While 自テナントに端末が 1 件も存在しないとき, when 管理者が端末一覧を要求したとき, the Device Service shall エラーとせず空の一覧を返す

### Requirement 2: 端末詳細（属性表示）

**Objective:** As a TenantAdmin / Operator / Viewer, I want 各端末の詳細属性を参照したい, so that 個別端末の構成とリスクを把握できる

#### Acceptance Criteria

1. When 管理者が自テナントの端末詳細を要求したとき, the Device Service shall 当該端末の管理モード・適用中ポリシー・ハードウェア情報・ソフトウェア情報・最終同期時刻・コンプライアンス状態を返す（umbrella Req 5.2）
2. When 管理者が自テナントの端末詳細を要求したとき, the Device Service shall 当該端末にインストール済みの管理対象アプリ一覧を返す（umbrella Req 7.5）
3. While 端末が同期遅延と判定されるとき, when 管理者が当該端末の詳細を要求したとき, the Device Service shall 同期遅延を示すフラグを付与して返す（umbrella Req 5.6, NFR 3.2）
4. If 管理者が自テナントに存在しない端末識別子を指定して詳細を要求したとき, the Device Service shall 404 相当の応答を返し、端末詳細を返さない
5. While 端末の一部属性（ハードウェア情報・ソフトウェア情報・インストール済みアプリ）が未取得で空であるとき, when 管理者が当該端末の詳細を要求したとき, the Device Service shall 当該属性を空として詳細を返し、エラーとしない

### Requirement 3: コンプライアンス分類

**Objective:** As a TenantAdmin / Operator / Viewer, I want 各端末のコンプライアンス状態が定義済みの分類で提示される, so that リスクの高い端末を識別できる

#### Acceptance Criteria

1. The Device Service shall 各端末のコンプライアンス状態を「準拠 / 非準拠 / 未確認 / サポート対象外」のいずれか 1 つに分類して返す（umbrella Req 5.3, NFR 1.2）
2. When 端末のコンプライアンス状態が非準拠であるとき, the Device Service shall 当該端末の非準拠理由を併記して返す（umbrella Req 5.3）
3. While 端末のコンプライアンス判定材料となる状態通知がまだ観測されていないとき, the Device Service shall 当該端末を「未確認」として分類する（umbrella Req 5.3）
4. Where 端末が「サポート対象外」として記録されているとき, the Device Service shall 当該端末を第 4 の分類値「サポート対象外」として返す（umbrella NFR 1.2）

### Requirement 4: 同期遅延の可視化

**Objective:** As a TenantAdmin / Operator / Viewer, I want 一定期間状態通知が無い端末を「同期遅延」として識別したい, so that 通信不能・管理下離脱の端末を早期に発見できる

#### Acceptance Criteria

1. While ある端末の最終同期時刻から現在時刻までの経過が設定された閾値を超えているとき, the Device Service shall 当該端末を「同期遅延」として可視化する（umbrella Req 5.6, NFR 3.2）
2. The Device Service shall 同期遅延判定の閾値を設定値（既定 24 時間）から取得する（umbrella NFR 3.2）
3. While ある端末の最終同期時刻から現在時刻までの経過が閾値ちょうど（閾値と等しい）であるとき, the Device Service shall 当該端末を「同期遅延」として扱わない

### Requirement 5: 他テナント端末の参照拒否（存在秘匿）

**Objective:** As a SaaS 運営者, I want 他テナント端末への詳細参照要求を存在ごと秘匿して拒否したい, so that テナント間のデータ分離と情報漏洩防止を担保できる

#### Acceptance Criteria

1. If 管理者が自テナント以外の端末識別子を指定して詳細を要求したとき, the Device Service shall 当該要求を拒否し、端末の存在有無を区別させない 404 相当の応答を返す（umbrella Req 5.5, NFR 2.1）
2. The Device Service shall 存在しない端末識別子への詳細要求と他テナント端末識別子への詳細要求に対して、区別できない同一の応答を返す（umbrella Req 5.5）

### Requirement 6: 全テナント横断 overview（SuperAdmin 限定）

**Objective:** As a SuperAdmin, I want 全テナント横断の端末サマリを集計・取得したい, so that SaaS 全体の端末現況とリスクを俯瞰できる

#### Acceptance Criteria

1. When SuperAdmin が横断 overview を要求したとき, the Device Service shall 全テナントを対象にテナント別の端末数とコンプライアンス内訳を集計して返す（umbrella NFR 2.1）
2. If SuperAdmin 以外のロール（TenantAdmin / Operator / Viewer）が横断 overview を要求したとき, the Device Service shall 当該要求を権限不足として拒否する（umbrella NFR 2.1）
3. When SuperAdmin が特定テナントに絞り込んだ overview を要求したとき, the Device Service shall 当該テナントのサマリのみを集計して返す
4. While 集計対象に端末が 1 件も存在しないとき, when SuperAdmin が横断 overview を要求したとき, the Device Service shall エラーとせず端末数 0 のサマリを返す

### Requirement 7: STATUS_REPORT 通知による端末属性更新

**Objective:** As a SaaS 運営者, I want STATUS_REPORT 通知を契機に端末属性が最新化される, so that ポーリングなしで端末の現況が反映される

#### Acceptance Criteria

1. When STATUS_REPORT 通知を受信したとき, the Device Status Handler shall 該当端末の最終同期時刻・コンプライアンス状態・適用中ポリシー・非準拠理由・ハードウェア情報・ソフトウェア情報・インストール済みアプリを通知内容に基づき更新する（umbrella Req 8.3）
2. If STATUS_REPORT 通知の payload に一部フィールド（非準拠理由・ハードウェア情報・インストール済みアプリなど）が欠落しているとき, the Device Status Handler shall 当該通知を破棄せず、欠落フィールドを破壊せずに処理を完了する
3. The Device Service shall 端末属性の更新を STATUS_REPORT 通知経由でのみ許容し、HTTP 経由で端末属性を直接書き換えるインターフェースを提供しない（umbrella Req 8.5）

## Non-Functional Requirements

### NFR 1: 一覧応答のパフォーマンス

1. The Device Service shall 1 テナントあたり 5,000 端末規模において、コンプライアンス分類でのフィルタ付き端末一覧要求の p95 応答時間を 1 秒未満に保つ

### NFR 2: 通知駆動反映の即時性

1. When STATUS_REPORT 通知を受信したとき, the Device Status Handler shall 受信時点から 60 秒以内に対応する端末属性の更新を参照結果へ反映する（umbrella NFR 3.1）

### NFR 3: テナント分離の恒常性

1. The Device Service shall 任意のテナントの管理者に対する一覧・詳細・横断集計のいずれの応答でも、他テナントの端末データを返さない状態を恒常的に保つ（umbrella NFR 2.1）

## Out of Scope

- デバイス関連の Web UI（一覧・詳細・overview の画面実装）（#13）
- リモートコマンド発行（LOCK / WIPE / REBOOT）（#10）
- 端末またはエンロールメントトークンへのポリシー割当（端末詳細からのポリシー変更操作）
- ENROLLMENT 通知処理および端末の新規登録（エンロールハンドラの責務）
- COMMAND 通知処理（コマンド状態遷移）
- 「サポート対象外（unsupported）」への書込み契機（Android バージョン判定・エンロール拒否検知）。本 Issue では分類値の読み取り・返却のみを対象とし、書込み契機は Open Questions を参照
- 通知の冪等処理・テナント解決・未割当退避（#39 Notification Dispatcher で実装済み）
- 同期遅延・通知欠落を管理者へ能動通知するアラート機構（閾値超過時の push 通知・メール等）

## Open Questions

- 「サポート対象外」への書込み契機（STATUS_REPORT で Android バージョンを判定するか、エンロールハンドラ側で判定するか）が本 Issue スコープに含まれるか未確定。読み取り側（第 4 分類値の返却）はスコープ内で確定している
- 一度も STATUS_REPORT を受信しておらず最終同期時刻が存在しない端末について、同期遅延と扱うか / コンプライアンスを「未確認」に留めるかが未確定（Requirement 4 は最終同期時刻の存在を前提に閾値超過を判定している）
- STATUS_REPORT が端末インベントリに未登録の端末に対して到達した場合の扱い（新規作成する / ENROLLMENT 未処理として保留する / 破棄する）が未確定
- 順序逆転して到達した STATUS_REPORT（新しいレポートの後に古いレポートが届く）の扱い。最終同期時刻やレポート時刻の比較で stale 更新を抑止すべきか未確定
- 横断 overview に端末数 0 のテナントを一覧として含めるか、集計対象から除外するかが未確定（Requirement 6.4 は全体で端末 0 件の空集計のみを規定）
- 端末一覧のページングにおける既定ページサイズ・最大取得件数の上限が未定義
