# Requirements Document

## Introduction

Android Enterprise（Android Management API、以下 AMAPI）を用いた独自 EMM
（Enterprise Mobility Management）SaaS の MVP（P0 相当）を構築するための要件を定義する。
自前 DPC（Device Policy Controller）は開発せず、Google 製の Android Device Policy にポリシーを
配信する構成を採る。MVP の主目的は、顧客企業（テナント）の管理者が Web コンソール上で、
Fully Managed / Dedicated(Kiosk) モードの Android 端末を QR コードでエンロールし、ポリシー配信・
アプリ配信・基本的なリモートコマンド（LOCK / WIPE / REBOOT）・端末状態の可視化を行えるように
することにある。テナント間の完全なデータ分離、ロールベースアクセス制御、不可逆操作に対する
監査ログ記録など、SaaS としての最低限のガバナンスを担保する。

## Requirements

### Requirement 1: テナント管理（マルチテナント / Enterprise バインド）

**Objective:** As a SaaS 運営者（SuperAdmin）, I want 顧客企業ごとに独立したテナント環境を作成・管理できる, so that 複数顧客の Android Enterprise 環境を 1 つの SaaS 上で分離して提供できる

#### Acceptance Criteria

1. When SuperAdmin が新規テナントを作成する操作を実行したとき, the EMM Console shall テナントレコードを生成し、当該テナント専用の Android Enterprise を AMAPI 経由で作成・バインドする
2. When テナントの Android Enterprise バインドが完了したとき, the EMM Console shall 当該テナントに紐づく enterprise 識別子を保存し、以後の AMAPI 呼び出しで当該識別子を用いる
3. If Android Enterprise の作成またはバインド処理が失敗したとき, the EMM Console shall テナントを「バインド未完了」状態に保ち、当該テナントに対する端末・ポリシー操作を一切受け付けない
4. The EMM Console shall 各テナントに対して、端末・ポリシー・エンロールメントトークン・アプリ承認情報・監査ログをテナント識別子で分離して保持する
5. While 認証済み管理者が自テナント以外のリソース（端末・ポリシー・enterprise 設定）にアクセスを試みているとき, the EMM Console shall 403 相当の権限拒否を返し、当該リソースの存在自体を露出しない

### Requirement 2: 管理者認証・認可（OIDC / RBAC）

**Objective:** As a テナント管理者, I want OIDC で安全にログインし、役割に応じて操作範囲が限定される, so that 権限を持たない操作（破壊的操作・他テナント参照など）を未然に防げる

#### Acceptance Criteria

1. When 管理者が EMM Console にログインを試みたとき, the EMM Console shall OIDC プロバイダによる認証フローを開始し、認証成功後にセッションを発行する
2. If OIDC 認証が失敗したまたは ID トークンの検証に失敗したとき, the EMM Console shall セッションを発行せず、ログイン画面に認証失敗メッセージを表示する
3. The EMM Console shall 各管理者に対して SuperAdmin / TenantAdmin / Operator / Viewer のいずれか 1 つ以上のロールを割り当てる
4. Where 管理者のロールが Viewer であるとき, the EMM Console shall 端末・ポリシー・テナント設定の参照操作のみを許可し、作成・更新・削除・コマンド発行操作を拒否する
5. Where 管理者のロールが Operator であるとき, the EMM Console shall 端末コマンド（LOCK / REBOOT）の発行とポリシーの参照を許可するが、WIPE の発行、テナント設定変更、管理者の追加・削除は拒否する
6. Where 管理者のロールが TenantAdmin であるとき, the EMM Console shall 自テナント配下のすべての操作（WIPE を含む破壊的コマンド、ポリシー作成・更新、管理者の追加・削除）を許可するが、他テナントのリソース参照・操作は拒否する
7. Where 管理者のロールが SuperAdmin であるとき, the EMM Console shall テナントの作成・削除を含む全テナント横断の管理操作を許可する
8. If 認証セッションが期限切れまたは無効化されているとき, the EMM Console shall 後続の操作要求を拒否し、再ログインを要求する
9. When 管理者のロールが変更されたとき, the EMM Console shall 当該変更内容（変更者・対象管理者・変更前後のロール）を監査ログに記録する

### Requirement 3: 端末エンロール（QR コード / Fully Managed・Dedicated）

**Objective:** As a TenantAdmin または Operator, I want Fully Managed および Dedicated(Kiosk) モードの Android 端末を QR コードで簡単にエンロールできる, so that 業務端末を最小限の手順で管理下に置ける

#### Acceptance Criteria

1. When 管理者がエンロールメントトークンの発行を要求し、Fully Managed モードを指定したとき, the EMM Console shall AMAPI を介して個人利用不可（personal usage 不許可）のエンロールメントトークンを生成し、対応する QR コード表示用データを返す
2. When 管理者がエンロールメントトークンの発行を要求し、Dedicated(Kiosk) モードを指定したとき, the EMM Console shall AMAPI を介して個人利用不可かつ Kiosk ポリシーが適用される初期ポリシーを紐づけたエンロールメントトークンを生成し、対応する QR コード表示用データを返す
3. The EMM Console shall エンロールメントトークン発行時に、当該トークンを発行したテナント識別子と発行管理者を内部メタデータとして付与する
4. When 端末側で QR コードを読み取り、エンロールが完了したとき, the EMM Console shall AMAPI からの ENROLLMENT 通知を契機に、当該端末を発行元テナント配下の端末インベントリに登録する
5. If 受信した ENROLLMENT 通知が、内部メタデータから特定したテナントと一致しないまたは特定不能であるとき, the EMM Console shall 当該通知を破棄せずに「未割当」状態で記録し、運用者に可視化する
6. If エンロールメントトークンが有効期限切れまたは使用済みである状態でエンロールが試行されたとき, the EMM Console shall AMAPI からのエラーを管理者画面に伝達し、当該端末を端末インベントリに登録しない
7. The EMM Console shall エンロールメントトークンの発行ごとに、発行者・テナント・モード（Fully Managed か Dedicated か）・有効期限を監査ログに記録する

### Requirement 4: ポリシー管理（作成・更新・割当）

**Objective:** As a TenantAdmin, I want アプリ管理・パスワード・セキュリティ・システム更新・Kiosk の各領域に対するポリシーを作成・更新し、端末群に割り当てたい, so that テナント内の端末を一貫した管理ルール下に置ける

#### Acceptance Criteria

1. When TenantAdmin が新規ポリシーを作成し保存したとき, the EMM Console shall AMAPI を介して自テナントの enterprise 配下にポリシーリソースを upsert する
2. The EMM Console shall ポリシー編集画面でアプリ管理・パスワード・セキュリティ・システム更新・Kiosk の 5 領域の設定項目を提供する
3. When TenantAdmin が既存ポリシーを更新したとき, the EMM Console shall 更新内容を AMAPI に反映し、当該ポリシーが割り当て済みの端末に対して新ポリシーが配信されるようにする
4. When TenantAdmin が端末またはエンロールメントトークンに対してポリシーを割り当てたとき, the EMM Console shall 当該ポリシーを Android Device Policy 経由で配信対象とする
5. If 管理者がポリシー内で 1 ポリシーあたりのアプリ上限（3,000 件）を超えるアプリを設定しようとしたとき, the EMM Console shall 保存を拒否し、上限超過である旨を管理者に提示する
6. If 必須項目（パスワード最小桁数の数値範囲、Kiosk 指定アプリのパッケージ名など）が不正な値で送信されたとき, the EMM Console shall 保存を拒否し、不正項目を管理者に提示する
7. If TenantAdmin が他テナントのポリシーに対する更新を試みたとき, the EMM Console shall 当該操作を拒否する
8. When ポリシーが作成・更新・削除されたとき, the EMM Console shall 変更者・テナント・ポリシー識別子・変更時刻を監査ログに記録する

### Requirement 5: デバイス管理（一覧・詳細・コンプライアンス可視化）

**Objective:** As a TenantAdmin / Operator / Viewer, I want 自テナントの端末一覧と各端末の詳細・コンプライアンス状態を把握したい, so that 管理対象の現況とリスクを把握できる

#### Acceptance Criteria

1. When 管理者が端末一覧画面を表示したとき, the EMM Console shall 自テナントに属する端末のみを一覧として返す
2. The EMM Console shall 端末詳細画面で、当該端末の管理モード・適用中ポリシー・ハードウェア／ソフトウェア情報・最終同期時刻・コンプライアンス状態を表示する
3. The EMM Console shall 各端末のコンプライアンス状態を「準拠 / 非準拠 / 未確認」のいずれかに分類して表示し、非準拠の場合は非準拠理由（AMAPI の nonComplianceDetails 相当）を併記する
4. When 管理者が非準拠端末のみをフィルタする要求を行ったとき, the EMM Console shall 自テナント内の非準拠端末のみを抽出して返す
5. If 管理者が他テナントの端末識別子を指定して詳細を要求したとき, the EMM Console shall 当該操作を拒否し、端末の存在自体を露出しない
6. While 端末から最新の状態通知が一定期間（後述の NFR 3.2 で定義）受信できていないとき, the EMM Console shall 当該端末を「同期遅延」として可視化する

### Requirement 6: デバイスコマンド（LOCK / WIPE / REBOOT）

**Objective:** As a TenantAdmin（WIPE）または Operator（LOCK / REBOOT）, I want 端末に対してリモートで LOCK / WIPE / REBOOT を発行できる, so that 紛失・盗難・トラブル対応をリモートで完結できる

#### Acceptance Criteria

1. When Operator または TenantAdmin が LOCK コマンドを端末に対して発行したとき, the EMM Console shall AMAPI を介して当該端末に LOCK コマンドを発行する
2. When Operator または TenantAdmin が REBOOT コマンドを端末に対して発行したとき, the EMM Console shall AMAPI を介して当該端末に REBOOT コマンドを発行する
3. When TenantAdmin が WIPE コマンドを発行する操作を行ったとき, the EMM Console shall 二段階確認（明示的な二度目の確認入力）を要求し、両方の確認が完了するまで AMAPI への WIPE 発行を行わない
4. If Operator が WIPE コマンドを発行しようとしたとき, the EMM Console shall 当該操作を権限不足として拒否する
5. If 管理者が自テナント以外の端末に対するコマンド発行を試みたとき, the EMM Console shall 当該操作を拒否する
6. When コマンド発行が AMAPI に受理されたとき, the EMM Console shall 当該コマンドを「発行済み（実行待ち）」状態として記録する
7. When AMAPI から当該コマンドの完了通知（COMMAND 通知）を受信したとき, the EMM Console shall コマンド状態を「成功」または「失敗」に更新し、コンソール上で結果を可視化する
8. The EMM Console shall すべての WIPE コマンドの発行（発行者・テナント・対象端末・確認時刻・実行結果）を監査ログに記録する
9. The EMM Console shall LOCK / REBOOT を含むすべてのリモートコマンドの発行（発行者・テナント・対象端末・コマンド種別・結果）を監査ログに記録する

### Requirement 7: アプリ配信（Managed Google Play 連携）

**Objective:** As a TenantAdmin, I want Managed Google Play 上でアプリを承認し、自テナントの端末群に配信したい, so that 業務アプリを各端末に統一的に展開できる

#### Acceptance Criteria

1. When TenantAdmin が Managed Google Play 上でアプリを承認したとき, the EMM Console shall 承認結果を自テナントのアプリカタログに反映する
2. When TenantAdmin が承認済みアプリを特定のポリシーに対して「強制インストール（FORCE_INSTALLED）」として設定したとき, the EMM Console shall 当該ポリシーが適用された端末群に対してアプリが配信される構成を AMAPI に反映する
3. When TenantAdmin が承認済みアプリを「任意インストール（AVAILABLE）」として設定したとき, the EMM Console shall 当該ポリシーが適用された端末群の Managed Google Play 上で当該アプリが選択可能となるよう AMAPI に反映する
4. If TenantAdmin が他テナントの承認済みアプリリストを操作しようとしたとき, the EMM Console shall 当該操作を拒否する
5. The EMM Console shall 各端末詳細画面で、当該端末にインストール済みの管理対象アプリの一覧（パッケージ名とバージョン）を表示する

### Requirement 8: 監視・通知（Pub/Sub 通知駆動の状態同期）

**Objective:** As a SaaS 運営者および TenantAdmin, I want 端末状態・エンロール・コマンド結果が通知駆動で最新化される, so that ポーリング負荷なく端末の現況を把握できる

#### Acceptance Criteria

1. The EMM Console shall AMAPI からの ENROLLMENT / STATUS_REPORT / COMMAND 通知を受信できる受信エンドポイントを提供する
2. When AMAPI から ENROLLMENT 通知を受信したとき, the EMM Console shall 該当端末を当該テナントの端末インベントリに登録または更新する
3. When AMAPI から STATUS_REPORT 通知を受信したとき, the EMM Console shall 該当端末のコンプライアンス状態と関連属性（適用中ポリシー名・適用状態・非準拠詳細など）を更新する
4. When AMAPI から COMMAND 通知を受信したとき, the EMM Console shall 該当コマンドの状態を「成功」または「失敗」に更新する
5. The EMM Console shall 端末状態の更新を恒常的なポーリングではなく、通知受信を主たる契機として行う
6. If 受信した通知の検証（送信元・整合性）に失敗したとき, the EMM Console shall 当該通知を破棄し、運用者が事後追跡できる形でログを残す
7. If 通知が一時的に処理失敗したとき, the EMM Console shall 当該通知を再処理の対象として保持し、永久に喪失することがない

### Requirement 9: 管理 Web コンソール（操作起点としての UI）

**Objective:** As a 管理者, I want ブラウザから本要件で定義された操作（テナント・管理者・端末・ポリシー・コマンド・アプリ配信・監査ログ閲覧）を実行できる, so that CLI や API 直叩きなしで日常運用が完結する

> **Note（2 コンソール構成）**: 本システムの Web UI は **2 つのコンソール**で構成する — 顧客企業の管理者向け **tenant-console**（TenantAdmin / Operator / Viewer）と、SaaS 運用者向け **admin-console**（SuperAdmin: テナント作成・Enterprise バインド・横断監視・未割当退避）。Requirement 1・9.5 と NFR 2.3 は主に admin-console、その他の操作系は tenant-console が担う。AC は両コンソールに跨って適用され、UI の分割方式そのものは design.md の領分とする。

#### Acceptance Criteria

1. The EMM Console shall 本要件 1〜8 で定義された各操作を、Web ブラウザ上の UI から実行可能にする
2. The EMM Console shall ログイン後に、当該管理者のロールで実行不可能な操作を UI 上で非活性化または非表示にする
3. While 管理者がブラウザ操作を行っているとき, the EMM Console shall 自テナント以外のテナント識別子を URL や API パラメータで指定された場合でも、当該リソースを返さない
4. The EMM Console shall 監査ログ閲覧画面で、自テナントの監査ログを時系列に表示し、操作種別・実行者・対象リソースで絞り込めるようにする
5. Where 管理者のロールが SuperAdmin であるとき, the EMM Console shall 全テナント横断の監査ログ閲覧を許可する

## Non-Functional Requirements

### NFR 1: 対応 Android OS とプラットフォーム互換性

1. The EMM Platform shall Android 10 以上を実行する端末のエンロール・ポリシー配信・LOCK / WIPE / REBOOT コマンド発行をサポートする
2. If 管理者が Android 10 未満の端末をエンロールしようとした結果、AMAPI または ADP がそれを拒否したとき, the EMM Console shall その旨を端末の状態または通知として可視化し、当該端末をインベントリに「サポート対象外」として残す

### NFR 2: テナント分離（データ・操作・通知）

1. The EMM Platform shall 任意のテナント A の管理者が、テナント B の端末・ポリシー・enterprise 設定・エンロールメントトークン・監査ログ・アプリカタログを参照・操作できない状態を恒常的に保つ
2. The EMM Platform shall AMAPI からの通知をテナント識別子に紐付けて処理し、テナント A 宛の通知でテナント B のリソースが更新されない
3. If 通知に紐づく enterprise 識別子からテナントが一意に特定できないとき, the EMM Platform shall 当該通知を「未割当」キューに退避し、他テナントのリソースを更新しない

### NFR 3: 通知駆動の即時性と同期遅延の可視化

1. The EMM Platform shall AMAPI からの通知を受信した時点から 60 秒以内に、対応する端末インベントリ・コマンド状態の更新をコンソールに反映する
2. While ある端末から AMAPI 側で 24 時間以上 STATUS_REPORT 通知が観測されていないとき, the EMM Console shall 当該端末を「同期遅延」として可視化する

### NFR 4: 不可逆操作の二段階確認と監査ログ

1. The EMM Console shall WIPE およびテナント削除を含む不可逆操作に対して、明示的な二段階確認（確認ダイアログまたは確認入力）を要求する
2. The EMM Console shall WIPE・テナント削除・管理者のロール変更・ポリシーの作成・更新・削除の各操作を、実行者・テナント・対象リソース・実行時刻・結果とともに監査ログに記録し、180 日以上保持する
3. The EMM Console shall 監査ログレコードを、記録後に管理者が改竄・削除できない形で保持する

### NFR 5: 認証・セッションのセキュリティ

1. The EMM Console shall 管理者セッションの cookie に HttpOnly および Secure 属性を設定する
2. The EMM Console shall ID トークンの署名・発行者・有効期限を検証し、検証失敗時はログインを許可しない
3. While 管理者セッションが 30 分以上アイドル状態であるとき, the EMM Console shall 当該セッションを失効させ、次回操作時に再認証を要求する

## Out of Scope

- zero-touch enrollment / Knox Mobile Enrollment / NFC プロビジョニング
- Work Profile (BYOD) / COPE 管理モードの対応
- AMAPI SDK 拡張アプリ（コンパニオンアプリ）・ローカルコマンド・Play を介さない直接 APK 配信
- Lost Mode（START_LOST_MODE / STOP_LOST_MODE）および eSIM 管理（ADD_ESIM / REMOVE_ESIM / REQUEST_DEVICE_INFO）
- compliance 違反時の自動段階エスカレーション（複数段階の自動 enforcement、自動 WIPE 等の高度な強制制御）
- 限定公開／プライベートアプリの自社開発フローおよび Web アプリ（PWA ショートカット）配信
- RESET_PASSWORD / RELINQUISH_OWNERSHIP / CLEAR_APP_DATA など LOCK / WIPE / REBOOT 以外のリモートコマンド
- 詳細な使用ログ（USAGE_LOGS 通知）の収集・分析

## Open Questions

- テナント新規作成時の Android Enterprise 作成フロー（SignupUrl を顧客管理者にメール提示するか、運営者がオペレーション代行するか）が未確定。MVP では SuperAdmin 主導の代行運用か、テナント管理者自身のセルフサインアップか、いずれの体験にすべきか
- 監査ログの保持期間として NFR 4.2 で「180 日以上」を仮置きしているが、想定顧客の契約・規制要件に合わせて要見直し（例: 1 年・7 年など）
- 同期遅延の閾値として NFR 3.2 で「24 時間」を仮置きしているが、実際の statusReportingSettings の構成と運用想定（業務時間帯のみ稼働する端末など）に応じて短縮・延長の余地あり
- Viewer ロールが監査ログを閲覧可能とするか、TenantAdmin 以上に限定するかが未確定（要件 9.4 では絞り込み条件のみを定義）
- 通知欠落・遅延に対する管理者向けアラート（例: 「N 時間以上同期がない端末が X 台あります」）の要否と閾値
- MVP 時点でテナント間で共通のシステム標準ポリシー雛形を提供するか否か。提供する場合、雛形の編集権限を SuperAdmin に限定するかの方針も合わせて確認が必要
