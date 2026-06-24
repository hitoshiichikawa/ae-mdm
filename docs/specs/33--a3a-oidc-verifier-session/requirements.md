# Requirements Document

## Introduction

本要件は ae-mdm（Android Enterprise EMM SaaS）MVP の **認証基盤（A3a）** を確立するためのものである。
Umbrella Issue #24 の Requirement 2.1 / 2.2 / 2.8 および NFR 5.1 / 5.2 / 5.3 のうち、
**OIDC ID トークン検証** と **管理者セッション管理**（cookie 発行・タイムアウト・state 保護）を
1 Issue に切り出し、tenant-console / admin-console の 2 つの OIDC クライアントから発行された
ID トークンを正しく識別・検証し、安全なセッション cookie を発行する責務を担う。

本基盤は後続 Issue（RBAC 許可マトリクスや `/api/admin` ガード、画面側のロール出し分け等）が依拠する
**前提層**である。認可（permission check）は本 Issue のスコープ外とし、本 Issue では「認証済みであるか」
「どの aud のコンソールから来たか」「セッションが有効か」を判定し、後続層が利用できる
TenantContext 相当の情報（管理者識別子・aud 区別・ロールクレーム）を確立する所まで責任を持つ。

なお、Issue #2（A2）で提供される config / logger / errors / DB / RLS / `/api` & `/api/admin` の
2 サブルータ群 mount を前提とし、本 Issue ではその上に OIDC Verifier と Session Manager、および
OIDC callback / login / logout の HTTP エンドポイントを追加する。

## 関連

- Depends on: #2
- Parent: #24

## Requirements

### Requirement 1: OIDC ID トークン検証

**Objective:** As a 認証基盤の利用側（auth handler）, I want OIDC IdP が発行した ID トークンの真正性・有効性・想定オーディエンス整合を一元的に検証できる, so that 偽造・期限切れ・誤送付された ID トークンによるセッション発行を阻止できる

#### Acceptance Criteria

1. When auth handler が ID トークンを受領したとき, the OIDC Verifier shall IdP の JWKS から取得した公開鍵で当該トークンの署名を検証する
2. When ID トークンを検証するとき, the OIDC Verifier shall 当該トークンの `iss` クレームを設定値として与えられた信頼発行者と完全一致で照合する
3. When ID トークンを検証するとき, the OIDC Verifier shall 当該トークンの `aud` クレームが `tenant-console` または `admin-console` のいずれかと一致することを確認し、どちらに一致したかを呼び出し側に返却する
4. If ID トークンの `aud` クレームが `tenant-console` / `admin-console` のいずれにも一致しないとき, the OIDC Verifier shall 当該トークンを拒否し、セッション発行を行わない
5. If ID トークンの `aud` クレームが配列形式で `tenant-console` / `admin-console` の両方を含むとき, the OIDC Verifier shall 当該トークンを曖昧として拒否し、セッション発行を行わない
6. When ID トークンを検証するとき, the OIDC Verifier shall 当該トークンの `exp` クレームと検証時刻を比較し、`exp` が検証時刻以前であれば当該トークンを拒否する
7. If ID トークンの署名検証が失敗したとき, the OIDC Verifier shall 当該トークンを拒否し、セッション発行を行わない
8. If ID トークンの `iss` クレームが設定値と一致しないとき, the OIDC Verifier shall 当該トークンを拒否し、セッション発行を行わない
9. While IdP が署名鍵をローテーションしたとき, the OIDC Verifier shall 新しい鍵 ID（kid）に対応する公開鍵を JWKS から取得し直して検証を継続する
10. If ID トークンの kid に対応する公開鍵が JWKS から取得できないとき, the OIDC Verifier shall 当該トークンを拒否し、セッション発行を行わない
11. The OIDC Verifier shall ID トークンの生値（raw JWT）をログまたは永続ストアに平文で記録しない

### Requirement 2: OIDC 認可コードフローと state による CSRF / replay 防止

**Objective:** As a 管理者, I want ログイン開始から callback までの間に第三者が state を横取り・差し替えできない安全な認可コードフロー, so that CSRF や state replay 攻撃でセッションを乗っ取られない

#### Acceptance Criteria

1. When 管理者が `/api/auth/login` 相当のログイン開始エンドポイントに到達したとき, the Auth Service shall 推測困難な乱数を生成して OIDC `state` 値とし、IdP の認可エンドポイントへのリダイレクト URL に当該 `state` を付与する
2. When ログイン開始時に `state` を生成したとき, the Auth Service shall 当該 `state` を MAC（Message Authentication Code）で保護した短寿命 cookie として管理者のブラウザに発行する
3. The state 保護 cookie shall HttpOnly 属性・Secure 属性・SameSite=Lax 属性を設定する
4. The state 保護 cookie shall ログイン開始時刻から 10 分以内に有効期限が切れる短寿命の有効期限を設定する
5. When OIDC callback エンドポイントが IdP からのリダイレクトを受領したとき, the Auth Service shall リクエストクエリの `state` 値と state 保護 cookie の値が MAC 検証込みで一致することを確認する
6. If state 保護 cookie が存在しない・有効期限が切れている・MAC 検証に失敗するいずれかの状態で callback が到達したとき, the Auth Service shall 認可コードを処理せずログインを失敗として扱う
7. If リクエストクエリの `state` 値と state 保護 cookie の値が一致しないとき, the Auth Service shall 認可コードを処理せずログインを失敗として扱う
8. When callback で state 検証が完了したとき, the Auth Service shall state 保護 cookie を即時に無効化（最大経過時間 0 等）して再利用を防止する
9. Where ログイン開始時に発行された state 保護 cookie が、別のブラウザセッションまたは別の IdP リダイレクトに紐づく callback で提示されたとき, the Auth Service shall 当該 callback を拒否する

### Requirement 3: セッション cookie 発行と属性

**Objective:** As a 管理者, I want OIDC 認証成功後にセッション cookie が発行され、当該 cookie が XSS・MITM・CSRF に対する標準的な保護属性を備える, so that ログイン後のリクエストで毎回 ID トークンを送らずに安全に操作を継続できる

#### Acceptance Criteria

1. When OIDC callback で ID トークン検証および state 検証が成功したとき, the Session Manager shall 管理者向けのセッション識別子を新規発行し、当該識別子をセッション cookie として管理者のブラウザに返却する
2. The セッション cookie shall HttpOnly 属性を設定する
3. The セッション cookie shall Secure 属性を設定する
4. The セッション cookie shall SameSite=Lax 属性を設定する
5. When セッション cookie を発行するとき, the Session Manager shall 当該 cookie の値として推測困難な乱数（暗号学的に安全な擬似乱数）を用いる
6. The Session Manager shall セッション識別子の生値（cookie に格納する値）をログまたは永続ストアに平文で記録しない
7. When セッション識別子を永続ストアに記録するとき, the Session Manager shall 当該識別子の SHA-256 ハッシュを格納する
8. When セッション識別子を構造化ログに出力するとき, the Session Manager shall 当該識別子の SHA-256 ハッシュまたはそれを短縮した識別子のみを出力する
9. When ID トークンの `aud` 検証で識別したコンソール種別（tenant-console / admin-console）が確定したとき, the Session Manager shall 当該コンソール種別をセッションに紐付け、後続のセッション検証時に呼び出し側へ返却可能にする

### Requirement 4: セッションのアイドル / 絶対タイムアウト

**Objective:** As a 運用者, I want 長時間放置されたセッションや、発行から長時間経過したセッションを自動的に失効させる, so that 端末紛失や離席に起因するセッション盗用のリスクを抑制できる

#### Acceptance Criteria

1. When セッション cookie を発行するとき, the Session Manager shall 当該セッションのアイドルタイムアウトの基準時刻として「最終操作時刻」を記録する
2. When セッション cookie を発行するとき, the Session Manager shall 当該セッションの絶対有効期限として「発行時刻 + 8 時間」を記録する
3. When 認証済みリクエストがセッション cookie を提示したとき, the Session Manager shall 当該セッションの最終操作時刻を現在時刻に更新する
4. While セッションの最終操作時刻からの経過時間が 30 分を超えているとき, the Session Manager shall 当該セッションを失効として扱い、後続リクエストの認証を拒否する
5. While セッションの発行時刻からの経過時間が 8 時間を超えているとき, the Session Manager shall 当該セッションを失効として扱い、後続リクエストの認証を拒否する
6. If 失効したセッション cookie が提示されたとき, the Session Manager shall 当該セッションを永続ストア上でも失効状態として扱い、再ログインを要求する
7. When セッションが失効と判定されたとき, the Session Manager shall 当該セッション cookie をブラウザから削除するためのレスポンスを返す
8. When セッションの最終操作時刻が更新されるとき, the Session Manager shall 当該更新後も絶対有効期限を延長しない（絶対有効期限は発行時刻からの固定値とする）

### Requirement 5: セッション失効・ログアウト

**Objective:** As a 管理者, I want 任意のタイミングでログアウトでき、ログアウト後は当該セッション cookie で再認証が成立しない, so that 共用端末等で意図した時点でアクセスを断てる

#### Acceptance Criteria

1. When 管理者がログアウトを要求したとき, the Session Manager shall 当該セッションを永続ストア上で失効状態として記録する
2. When ログアウトが完了したとき, the Session Manager shall セッション cookie をブラウザから削除するためのレスポンスを返す
3. If ログアウト後に同一のセッション cookie が提示されたとき, the Session Manager shall 当該セッションを失効として扱い、後続リクエストの認証を拒否する
4. If 改竄された（ハッシュ値が永続ストア上のいずれの記録とも一致しない）セッション cookie が提示されたとき, the Session Manager shall 当該リクエストの認証を拒否する

### Requirement 6: 2 コンソール OIDC クライアントの分離

**Objective:** As a セキュリティ運用者, I want tenant-console と admin-console から発行された ID トークンが、対応するルート群でのみ受理される, so that 一方のコンソールで漏洩した ID トークンを他方のコンソールに転用される攻撃を物理的に防げる

#### Acceptance Criteria

1. The OIDC Verifier shall tenant-console と admin-console の 2 つの OIDC クライアント設定（client_id / 信頼 aud / callback URL）を独立に保持できる
2. When OIDC callback エンドポイントが認可コードと state を受領したとき, the Auth Service shall 当該 callback が tenant-console / admin-console のどちらに紐づくかを URL パスまたは設定値から確定し、対応する OIDC クライアントで処理する
3. When セッションを発行するとき, the Session Manager shall 当該セッションに紐付けたコンソール種別（aud）を後続のセッション検証で参照可能な属性として保持する
4. The OIDC Verifier shall tenant-console 用の信頼 aud と admin-console 用の信頼 aud を異なる値として設定可能にする

## Non-Functional Requirements

### NFR 1: 機密値の取り扱い

1. The Auth Service shall ID トークン本体・セッション cookie の生値・state 保護 cookie の MAC 鍵・OIDC client secret のいずれもログに平文で出力しない
2. The Auth Service shall セッション識別子を永続ストアに格納する際、SHA-256 ハッシュとして格納し、cookie 経由でブラウザから提示された生値とハッシュ計算結果が一致した場合に限り当該セッションを有効と判定する

### NFR 2: タイムアウト値の運用変更容易性

1. The Auth Service shall セッションのアイドルタイムアウト（既定 30 分）と絶対有効期限（既定 8 時間）と state 保護 cookie の有効期限（既定 10 分）を環境変数または設定値から変更可能にする
2. While アプリケーションが稼働中であるとき, the Auth Service shall タイムアウト値を不変として扱い、ランタイムでの上書きを許可しない

### NFR 3: fail-closed

1. If OIDC 検証・state 検証・セッション検証のいずれかが想定外の例外で失敗したとき, the Auth Service shall 当該リクエストの認証を拒否し、認証成功として扱わない
2. If 起動時に OIDC discovery（issuer の `.well-known/openid-configuration` または JWKS）の取得に失敗したとき, the Auth Service shall プロセスを起動失敗として扱い、認証エンドポイントを公開しない

### NFR 4: 観測性

1. When ID トークン検証・state 検証・セッション検証のいずれかが失敗したとき, the Auth Service shall 失敗種別（署名不正 / iss 不一致 / aud 不一致 / exp 切れ / state 不一致 / state 期限切れ / session 期限切れ / session 改竄 等）を識別可能な構造化ログを WARN レベルで記録する
2. The Auth Service shall 構造化ログにセッション cookie の生値・ID トークン本体・state cookie の MAC 鍵を含めない（ハッシュ化済み識別子・aud 種別・タイムスタンプ・失敗種別のみを含める）

## Out of Scope

- RBAC（ロール×操作の許可マトリクス）の実装、および各エンドポイントでの permission check
- `/api/admin` 配下に挟む SuperAdmin 必須ガードの判定ロジック（Issue #2 で提供済みのフック点に乗る形は本 Issue の範囲だが、ロール判定そのものは後続 Issue）
- 画面側のロール出し分け（tenant-console / admin-console UI 側の非活性化・非表示制御）
- 管理者ユーザーの管理画面（admin_users / admin_role_assignments の CRUD UI）
- OIDC ログアウト時の IdP 側 single logout（RP-initiated logout）対応
- 多要素認証（MFA）の追加
- 監査ログ自体の出力フォーマット（NFR 4.1 で「構造化ログに記録する」事象を要件化するが、`audit_logs` テーブルへの監査イベント記録は umbrella Req 2.9 等の後続 Issue で扱う）
- セッション固定（session fixation）攻撃に対する追加的な ID 再発行ロジックの強化（cookie 属性と乱数 ID により最低限の保護は満たすが、追加施策は別 Issue）

## Open Questions

- Issue #33 本文では「absolute 8 時間で失効（NFR 3.2）」と記載されているが、umbrella requirements.md の NFR 3.2 は「端末からの STATUS_REPORT 未受信 24 時間で同期遅延として可視化する」という別件の要件である。absolute 8 時間という値そのものは umbrella design.md（`docs/specs/24-android-enterprise-emm-mvp/design.md` の OIDC Verifier / Session Manager 節および Security Considerations 節）に記載されているため本要件では「絶対有効期限 = 発行から 8 時間」を採用したが、umbrella requirements.md 側の NFR として明文化されていないため、表記揺れを是正するかは umbrella 起票者（または PjM）の確認を要する。
- state 保護 cookie の MAC 鍵は環境変数で注入する想定（Issue #2 の config モジュールに新規キー追加）だが、鍵のローテーション戦略（旧鍵を一定期間並行検証するか、即時切替で進行中ログインを破棄するか）は本 Issue 時点では未確定。MVP では「即時切替・進行中ログインは再ログインで救済」を前提として要件化したが、想定運用と一致するかを確認したい。
- Issue 本文に明示されていないが、ログアウトエンドポイント（Requirement 5）は MVP 必須として要件化した。tenant-console / admin-console UI のリリース時期との兼ね合いで本 Issue から外す判断もありうるため、判断を仰ぎたい。
- セッション cookie の Domain / Path 属性（同一オリジン前提か、サブドメイン共有が必要か）は本 Issue では明示していない。tenant-console と admin-console を別オリジンで配信する場合の cookie scope 設計は別途確認が必要。
