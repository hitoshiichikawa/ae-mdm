# Requirements Document

## Introduction

本要件は ae-mdm（Android Enterprise EMM SaaS）MVP における認証認可基盤（A3）を構築する
ためのものである。Umbrella Issue #24（`docs/specs/24-android-enterprise-emm-mvp/`）の
Requirement 2（管理者認証・認可）と NFR 5（認証・セッションのセキュリティ）を本 spec で
切り出して実装する。具体的には、ジェネリック OIDC プロバイダ（ローカル開発では Keycloak）を
用いた管理者ログインフロー、tenant-console / admin-console の **2 OIDC クライアント分離**、
ID トークン検証、opaque session token の発行と idle 30 分でのセッション失効、
SuperAdmin / TenantAdmin / Operator / Viewer の 4 ロールによる表駆動 RBAC、
および `/api/admin/*` 配下を SuperAdmin 専用に保護するルートガードを backend 側で提供する。

本 spec は umbrella の Req 2 / NFR 5 のうち **backend 側の OIDC 検証・セッション管理・
RBAC 判定・/api/admin ガードの責務範囲のみ**を扱う。tenant-console / admin-console の
フロントエンド SPA における UI ロール出し分け（ボタン非活性化・画面非表示）は本 spec の
対象外であり、umbrella tasks 12.x / 13.x（後続の Issue #12 以降）で実装する。
A2（Issue #2、`docs/specs/2--a2-config-logger-errors-db-rls/`）が提供する config / logger /
errors / DB トランザクション境界 / `/api` と `/api/admin` の 2 サブルータ mount 機構を
前提として、本 spec はその上に認証・認可ミドルウェアと auth エンドポイント群を実装する。

## 関連

- Depends on: #2
- Parent: #24

## Requirements

### Requirement 1: OIDC ログインフロー（tenant-console / admin-console の 2 クライアント分離）

**Objective:** As a 管理者, I want 所属する SPA（tenant-console または admin-console）に応じた OIDC クライアントで認証を開始し、コールバックで ID トークン検証とセッション発行が完了する, so that 顧客企業の業務管理者と SaaS 運用者のログイン経路を物理的に分離した上でブラウザ操作を始められる

#### Acceptance Criteria

1. The EMM Console shall tenant-console SPA 用の OIDC ログイン開始エンドポイントと、admin-console SPA 用の OIDC ログイン開始エンドポイントを別経路で提供する
2. When 未認証の管理者が tenant-console SPA 用のログイン開始エンドポイントにアクセスしたとき, the EMM Console shall OIDC プロバイダの認可エンドポイントへ `client_id=tenant-console` でリダイレクトする
3. When 未認証の管理者が admin-console SPA 用のログイン開始エンドポイントにアクセスしたとき, the EMM Console shall OIDC プロバイダの認可エンドポイントへ `client_id=admin-console` でリダイレクトする
4. When OIDC プロバイダから認可コードが付与されたコールバックを受信したとき, the EMM Console shall 当該認可コードを ID トークンに交換し、ID トークン検証（Requirement 2 参照）に成功した場合に限りセッションを発行する
5. When セッション発行が完了したとき, the EMM Console shall 当該セッションに対応する管理者が属する SPA（tenant-console または admin-console）の URL に管理者を戻す
6. When 認証済み管理者がログアウトを要求したとき, the EMM Console shall 当該セッションを失効させ、以後の API 呼び出しを未認証として扱う
7. If OIDC プロバイダから取得した認可コードの交換に失敗したとき, the EMM Console shall セッションを発行せず、ログイン失敗を示すエラー応答を返す

### Requirement 2: ID トークン検証（署名・iss・aud・exp）

**Objective:** As a SaaS 運営者, I want ID トークンの署名・発行者・有効期限・対象クライアント（aud）を必ず検証してからセッションを発行する, so that 改竄・期限切れ・他クライアント宛のトークンによる成りすましログインを防げる

#### Acceptance Criteria

1. When ID トークンを検証するとき, the EMM Console shall OIDC プロバイダの JWKS で署名を検証する
2. When ID トークンを検証するとき, the EMM Console shall 当該トークンの `iss` クレームが設定済み OIDC プロバイダの発行者識別子と一致することを確認する
3. When ID トークンを検証するとき, the EMM Console shall 当該トークンの `aud` クレームが `tenant-console` または `admin-console` のいずれか 1 つに一致することを確認する
4. When ID トークンを検証するとき, the EMM Console shall 当該トークンの `exp` クレームが現在時刻を過ぎていないことを確認する
5. When ID トークン検証に成功したとき, the EMM Console shall 検証時に確認した `aud` 値（tenant-console / admin-console のいずれか）を当該セッションのメタデータとして保持する
6. If ID トークンの署名検証に失敗したとき, the EMM Console shall セッションを発行せず、認証失敗として応答する
7. If ID トークンの `iss` が設定済み発行者識別子と一致しないとき, the EMM Console shall セッションを発行せず、認証失敗として応答する
8. If ID トークンの `aud` が `tenant-console` および `admin-console` のいずれにも一致しないとき, the EMM Console shall セッションを発行せず、認証失敗として応答する
9. If ID トークンの `exp` が現在時刻を過ぎているとき, the EMM Console shall セッションを発行せず、認証失敗として応答する

### Requirement 3: セッション管理（opaque token / cookie 属性 / idle timeout）

**Objective:** As a 管理者, I want ログイン後のセッションが安全な cookie で維持され、一定時間操作がない場合は自動失効する, so that ブラウザ放置や cookie 漏洩のリスクを最小化できる

#### Acceptance Criteria

1. When セッションを発行するとき, the EMM Console shall opaque な session token を生成し、ID トークン本体を cookie として送出しない
2. The EMM Console shall セッション cookie に `HttpOnly` 属性を設定する
3. The EMM Console shall セッション cookie に `Secure` 属性を設定する
4. The EMM Console shall セッション cookie に `SameSite=Lax` 属性を設定する
5. When 認証済みリクエストが処理されたとき, the EMM Console shall 当該セッションの最終アクセス時刻を更新する
6. While 認証済みセッションが直近の操作から 30 分以上アイドル状態であるとき, the EMM Console shall 当該セッションを失効させる
7. While 認証済みセッションの発行から 8 時間以上が経過しているとき, the EMM Console shall 当該セッションを失効させる（absolute timeout）
8. If 失効済みまたは未知のセッション token を伴うリクエストが到達したとき, the EMM Console shall 当該リクエストを 401 として拒否し、再ログインを要求する
9. When 管理者がログアウトを要求したとき, the EMM Console shall 対応するサーバ側セッションレコードを失効させ、cookie の削除指示を応答に含める

### Requirement 4: RBAC ロール定義と表駆動許可マトリクス

**Objective:** As a SaaS 運営者, I want 4 ロール（SuperAdmin / TenantAdmin / Operator / Viewer）と action × resource の許可マトリクスを表駆動で定義し、認可判定を 1 箇所に集約する, so that 後続のドメイン handler が個別に ad-hoc な if 分岐を書かずに一貫した認可判定を行える

#### Acceptance Criteria

1. The EMM Console shall 管理者ロールを SuperAdmin / TenantAdmin / Operator / Viewer の 4 種類として定義する
2. When OIDC ID トークンの groups または roles クレームから管理者ロールを解決するとき, the EMM Console shall 当該クレーム値を 4 ロールのいずれかに写像する
3. The EMM Console shall ロール × action × resource の許可マトリクスを表駆動データとして保持し、認可判定をすべて当該マトリクス経由で行う
4. When 認可判定が呼び出されたとき, the EMM Console shall 許可マトリクスを参照して許可または拒否を返す
5. Where 管理者のロールが Viewer であるとき, the EMM Console shall 端末・ポリシー・テナント設定・アプリカタログに対する読み取り操作のみを許可する
6. Where 管理者のロールが Viewer であるとき, the EMM Console shall 端末・ポリシー・テナント設定・アプリカタログに対する作成・更新・削除操作およびリモートコマンド発行を拒否する
7. Where 管理者のロールが Operator であるとき, the EMM Console shall 自テナントの端末に対する LOCK および REBOOT コマンドの発行とポリシーの参照を許可する
8. Where 管理者のロールが Operator であるとき, the EMM Console shall 自テナントの端末に対する WIPE コマンドの発行、ポリシー作成・更新、テナント設定変更、管理者の追加・削除を拒否する
9. Where 管理者のロールが TenantAdmin であるとき, the EMM Console shall 自テナント配下のすべての操作（WIPE を含む破壊的コマンド、ポリシー作成・更新・削除、自テナント内の管理者の追加・削除・ロール変更）を許可する
10. Where 管理者のロールが TenantAdmin であるとき, the EMM Console shall 他テナントのリソース参照・操作およびテナント自体の作成・削除を拒否する
11. Where 管理者のロールが SuperAdmin であるとき, the EMM Console shall テナントの作成・削除、全テナント横断の参照、admin-console 配下の運用系操作を許可する
12. If ID トークンの groups または roles クレームに 4 ロールのいずれにも写像できない未知の値のみが含まれるとき, the EMM Console shall セッションを発行せず、認可失敗として応答する

#### Acceptance Matrix

ロール × action × resource の表駆動許可マトリクス（行: ロール、列: action、各セルが
対応する AC 番号と許可/拒否を示す）。各 action はリソース種別（device / policy / tenant /
admin-user / audit-log）に対する代表動詞であり、本マトリクスは Requirement 4 の AC で
形式宣言されている内容を一覧化したものである。

| Role / Action | read device | create/update policy | issue LOCK / REBOOT | issue WIPE | manage admin-user (same tenant) | create / delete tenant | read cross-tenant audit log |
|---|---|---|---|---|---|---|---|
| Viewer | 許可 (AC 4.5) | 拒否 (AC 4.6) | 拒否 (AC 4.6) | 拒否 (AC 4.6) | 拒否 (AC 4.6) | 拒否 (AC 4.6) | 拒否 (AC 4.6) |
| Operator | 許可 (AC 4.7) | 拒否 (AC 4.8) | 許可 (AC 4.7) | 拒否 (AC 4.8) | 拒否 (AC 4.8) | 拒否 (AC 4.8) | 拒否 (AC 4.8) |
| TenantAdmin | 許可 (AC 4.9) | 許可 (AC 4.9) | 許可 (AC 4.9) | 許可 (AC 4.9) | 許可 (AC 4.9) | 拒否 (AC 4.10) | 拒否 (AC 4.10) |
| SuperAdmin | 許可 (AC 4.11) | 許可 (AC 4.11) | 許可 (AC 4.11) | 許可 (AC 4.11) | 許可 (AC 4.11) | 許可 (AC 4.11) | 許可 (AC 4.11) |

注: 「read cross-tenant audit log」は admin-console 経由の全テナント横断監査ログ閲覧を指す
（umbrella Req 9.5）。自テナントの監査ログ閲覧は別 AC（umbrella Req 9.4 / 本 spec の AC 4.5）
で扱う。

### Requirement 5: `/api/admin/*` ルートの SuperAdmin 専用ガード

**Objective:** As a SaaS 運営者, I want `/api/admin/*` 配下の運用系エンドポイントを、aud=admin-console かつ SuperAdmin ロールを持つ管理者のみに通過させる, so that tenant-console から漏洩した token や非 SuperAdmin の管理者が運用系操作（テナント作成・削除・横断監査閲覧）を実行することを防げる

#### Acceptance Criteria

1. The EMM Console shall `/api/admin/*` 配下のすべてのリクエスト経路に対して SuperAdmin 必須ガードをミドルウェアチェーンに挟む（ただし `/api/admin/auth/login` / `/api/admin/auth/callback` / `/api/admin/auth/logout` / `/api/admin/auth/session` の auth エンドポイント群は、ログイン未完了状態で到達する経路のため SuperAdmin ガード対象外とする。これら auth 経路自身は AC 1.3 に従い admin-console 用の OIDC ログイン経路として機能する）
2. When 認証済みリクエストが `/api/admin/*` 配下のエンドポイントに到達したとき, the EMM Console shall 当該セッションの `aud` メタデータが `admin-console` であることを確認する
3. When 認証済みリクエストが `/api/admin/*` 配下のエンドポイントに到達したとき, the EMM Console shall 当該管理者のロールが SuperAdmin であることを確認する
4. If 認証済みリクエストの `aud` メタデータが `tenant-console` であるまま `/api/admin/*` 配下のエンドポイントに到達したとき, the EMM Console shall 当該リクエストを 403 で拒否する
5. If `/api/admin/*` 配下のエンドポイントに SuperAdmin 以外のロール（TenantAdmin / Operator / Viewer）を持つ管理者からのリクエストが到達したとき, the EMM Console shall 当該リクエストを 403 で拒否する
6. If 未認証のリクエストが `/api/admin/*` 配下のエンドポイントに到達したとき, the EMM Console shall 当該リクエストを 401 で拒否する
7. When `/api/admin/*` 配下のリクエストを 403 で拒否するとき, the EMM Console shall 対象リソースの存在自体を露出しない応答内容を返す
8. While aud=admin-console のセッションが他テナントのリソースを `/api` 配下から参照しようとしているとき, the EMM Console shall SuperAdmin のみ cross-tenant 操作として許可する

### Requirement 6: 認可拒否時の応答方針（存在の非露出）

**Objective:** As a SaaS 運営者, I want 他テナント・上位権限リソースへのアクセス拒否時に、当該リソースの存在自体を応答から推測されない, so that エンドポイント走査による他テナントリソース ID の推定を防げる

#### Acceptance Criteria

1. While 認証済み管理者が自テナント以外のリソース識別子を指定したリクエストを送ったとき, the EMM Console shall 当該リクエストを 403 相当として拒否し、応答ボディに当該リソースの内部状態（存在有無・属性）を含めない
2. When 認可ミドルウェアが拒否を決定したとき, the EMM Console shall 拒否理由を機械可読なエラーコードとして応答に含めるが、対象リソース ID やテナント識別子を応答に含めない
3. The EMM Console shall 認可拒否時の応答を、対象リソースが存在する場合と存在しない場合とで同一形式・同一ステータスコードに統一する

### Requirement 7: ロール変更時の監査イベント発火

**Objective:** As a 監査・コンプライアンス担当, I want 管理者のロール変更が必ず監査ログに記録される, so that 権限昇格・降格の経緯を後追いできる

#### Acceptance Criteria

1. When 管理者のロールが変更されたとき, the EMM Console shall 変更者・対象管理者・変更前ロール・変更後ロール・変更時刻を含む監査イベントを発火する
2. The EMM Console shall ロール変更時の監査イベント発火を、認証認可ドメインの責務として宣言する（実際の永続化は audit ドメインに委譲する）
3. If 監査イベントの発火に失敗したとき, the EMM Console shall ロール変更そのものを成功として確定させず、当該変更操作を失敗として応答する

## Non-Functional Requirements

### NFR 1: セッション cookie のセキュリティ属性

1. The EMM Console shall セッション cookie に `HttpOnly` 属性を必ず付与し、ブラウザ JavaScript からの読み取りを不可能にする
2. The EMM Console shall セッション cookie に `Secure` 属性を必ず付与し、HTTPS 接続でのみ cookie が送出されるようにする
3. The EMM Console shall セッション cookie に `SameSite=Lax` 属性を必ず付与する

### NFR 2: ID トークン検証の必須化

1. The EMM Console shall ID トークン検証（署名・iss・aud・exp）をすべて成功した場合に限りセッションを発行し、いずれか 1 つでも失敗したセッション発行経路を許可しない
2. The EMM Console shall OIDC プロバイダの JWKS をキャッシュしつつも、鍵ローテーションを検出可能な頻度で再取得する

### NFR 3: セッション失効の即時性

1. While 認証済みセッションが直近の操作から 30 分以上アイドル状態であるとき, the EMM Console shall 30 分到達直後の次回 API 呼び出しを 401 として拒否する
2. While 認証済みセッションの発行から 8 時間以上が経過しているとき, the EMM Console shall 8 時間到達直後の次回 API 呼び出しを 401 として拒否する
3. When 管理者がログアウトを実行したとき, the EMM Console shall 当該セッションを 1 秒以内に失効させ、その後の API 呼び出しを 401 として拒否する

### NFR 4: 認可判定の一元化

1. The EMM Console shall ロール × action × resource の許可判定をすべて単一の許可マトリクス経由で行い、ドメイン handler 内の ad-hoc な if 分岐による認可判定を許可しない
2. The EMM Console shall 許可マトリクスの定義を、表駆動データとしてテストから検証可能な形で公開する

### NFR 5: 機密情報のログ非露出

1. While ログ出力が行われているとき, the EMM Console shall ID トークン本体・session token 値・OIDC client secret を平文でログに出力しない
2. When 認可拒否を構造化ログに記録するとき, the EMM Console shall 拒否理由・管理者識別子・パスを記録するが、対象リソース ID 全体を露出する場合は SuperAdmin 操作に限定する

## Out of Scope

- tenant-console / admin-console SPA におけるフロントエンド UI ロール出し分け（ボタン非活性化・画面非表示・RoleGate コンポーネント等）。umbrella tasks 12.x / 13.x（Issue #12 以降）で実装する
- OIDC プロバイダ（Keycloak 等）自体の構築・realm 定義・管理者ユーザのプロビジョニング。ローカル開発用の realm 雛形は Issue #1（A1 Docker Compose）でカバー済み
- 外部 IdP（Azure AD / Okta / Google Workspace 等）固有のクレームマッピング拡張。本 spec は OIDC 標準クレーム（sub / iss / aud / exp / groups または roles）のみを扱う
- 多要素認証（TOTP / WebAuthn / SMS OTP）の独自実装。MFA は OIDC プロバイダ側で要求し、本 spec は ID トークン受領後の検証のみを扱う
- 管理者のセルフサインアップ・パスワードリセット・招待メール送信フロー。管理者プロビジョニングは OIDC プロバイダ側で完結する
- 監査ログ書き込みの永続化実装（audit ドメインの責務、後続 Issue で扱う）。本 spec はロール変更時の監査イベント発火責務までを宣言する
- API トークン認証・サービスアカウント認証（M2M）。本 spec はブラウザログイン経由の管理者セッションのみを扱う
- セッション固定攻撃対策としての CSRF トークン発行と検証（`SameSite=Lax` で基本防御は提供するが、追加の CSRF token 機構は本 spec の対象外）

## Dependencies

- **#2（A2 共通基盤 config/logger/errors + DB/RLS、merge 済み）**: 本 spec は A2 が提供する config（OIDC クライアント情報・セッション秘密鍵・session timeout 設定）、logger（認証イベントの構造化ログ）、errors（独自 Error 型）、DB トランザクション境界（sessions テーブルへの永続化）、HTTP サブルータ mount 機構（`/api` と `/api/admin` の分離）、`/api/admin` 配下に SuperAdmin ガードを挟むフック点に依存する
- **#24（umbrella MVP spec）**: 本 spec は umbrella Req 2 / NFR 5 を child spec として切り出したものであり、umbrella の Logical Data Model に定義された `admin_users` / `admin_role_assignments` / `sessions` テーブルを利用する
- **umbrella tasks 3.1〜3.3**: 本 spec の実装対象タスクは umbrella tasks 3.1（OIDC Verifier + Session Manager）、3.2（RBAC Authorizer + /api/admin guard）、3.3（Auth Service 単体テスト）に対応する

## Traceability to Umbrella Spec

本 spec の各 Requirement / AC が umbrella `docs/specs/24-android-enterprise-emm-mvp/requirements.md`
の Req 2.x / NFR 5.x のどれに対応するかを以下の表に示す。umbrella の AC を child spec の
スコープ（backend 側責務のみ）に合わせて再記述しているため、1 対 1 対応ではなく
1 対多または多対 1 対応となる箇所がある。

| 本 spec 要件 | 対応する umbrella 要件 | 備考 |
|---|---|---|
| Req 1（OIDC ログインフロー、2 クライアント分離）| umbrella Req 2.1 | umbrella Req 2.1 を tenant-console / admin-console の 2 クライアントに分割して再記述 |
| Req 2（ID トークン検証）| umbrella Req 2.2, NFR 5.2 | 検証項目（署名・iss・aud・exp）を具体化 |
| Req 3（セッション管理）| umbrella Req 2.1, 2.8, NFR 5.1, NFR 5.3 | opaque token / cookie 属性 / idle 30 分 / absolute 8 時間 |
| Req 4（RBAC 4 ロール表駆動）| umbrella Req 2.3, 2.4, 2.5, 2.6, 2.7 | 4 ロールの許可マトリクスを表駆動として宣言 |
| Req 5（/api/admin SuperAdmin ガード）| umbrella Req 2.7, 9.3 | aud=tenant-console token の 403、SuperAdmin 以外の 403 |
| Req 6（拒否時の存在非露出）| umbrella Req 1.5, 5.5 | 403 応答での内部状態非露出 |
| Req 7（ロール変更時の監査イベント発火）| umbrella Req 2.9 | 認証認可ドメインからの監査イベント発火責務 |
| NFR 1（cookie セキュリティ属性）| umbrella NFR 5.1 | HttpOnly / Secure / SameSite=Lax |
| NFR 2（ID トークン検証の必須化）| umbrella NFR 5.2 | JWKS キャッシュと鍵ローテ |
| NFR 3（セッション失効の即時性）| umbrella NFR 5.3 | idle 30 分 / absolute 8 時間 / logout 1 秒 |
| NFR 4（認可判定の一元化）| umbrella Req 2.3〜2.7 | 表駆動マトリクスへの集約 |
| NFR 5（機密情報のログ非露出）| umbrella NFR 5.1, NFR 5.2（暗黙）| umbrella NFR 5 全体のセキュリティ精神を具体化 |

## Open Questions

- 4 ロールへの写像対象とする OIDC クレーム名（`groups` 配列か `roles` 配列か、または provider 固有の custom claim か）。MVP では Keycloak の `groups` クレームを想定するが、本番運用先 IdP が確定した時点で AC 4.2 の実装詳細を再確認する必要がある
- absolute session timeout の値として 8 時間を本 spec で仮置きしているが、umbrella spec 側では明示されていない（umbrella NFR 5.3 は idle 30 分のみ規定）。長時間操作が想定される運用シナリオ（夜間バッチ監視等）が現れる場合は再調整の余地がある
- 4 ロールのいずれにも写像できない未知のロールクレームを持つ管理者をどう扱うか。AC 4.12 では「セッションを発行せず認証失敗として応答」と定めたが、暫定的に Viewer に降格させる運用方針もあり得る（運用ポリシーの確認が必要）
- `/api/admin/*` 配下に対する 403 拒否時の応答ボディフォーマット（umbrella spec では「存在を露出しない」とだけ規定）。エラーコード体系の統一は A2 の独自 Error 型と整合させて design.md で詳細化する
