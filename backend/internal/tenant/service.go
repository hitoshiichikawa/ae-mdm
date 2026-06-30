package tenant

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/amapi"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
)

// Service は Tenant ライフサイクル（作成 / バインド / 無効化 / 参照 / 前提ガード）と
// 状態機械の単一所有者（design.md「tenant.Service」節 / tasks.md task 4.1 / 5.1）。
//
// Create / Get / List / EnterpriseNameForTenant（task 4）に加え、状態機械の遷移系である
// Bind / Disable（task 5）を実装する。状態遷移は定義済み遷移のみ成功し（NFR 1.2）、未定義
// 状態は fail-closed で拒否する。
//
// 主責務はユースケース単位で「前提状態判定（状態機械）→ AMAPI オーケストレーション →
// Repository 永続化 → 監査記録」を行うこと。actor（操作実行者の admin_users.id）は Handler が
// `httpserver.AuthClaimsFromContext` で取得し、監査イベント用に Service へ明示的に渡す
// （本 package は `httpserver` を import しない依存方向ルール / doc.go のため、ctx 経由ではなく
// 引数で受け取る）。
type Service interface {
	// Create は新規テナントを作成する（Req 1.1〜1.5 / 3.1）。
	//
	//   - in.Name を空白 trim 後に空なら CodeInvalidRequest を返し、永続化も AMAPI 呼び出しも
	//     行わない（Req 1.3）。
	//   - amapi.CreateSignupURL でサインアップ URL を先に発行する（Req 1.2）。続いて
	//     Repository.Insert で signup_url_name を含めた pending_bind 行を作成する（Req 1.1 / 3.1）。
	//     signup_url_name は ② の戻り値であり、発行元テナントへ束縛する正本として永続化するため
	//     URL 発行成功後に Insert する（#52 で create 順序を CreateSignupURL→Insert へ変更 / Req 3.1）。
	//   - URL 生成が失敗（または空応答）したときは Insert せず pending_bind 行を作らない。当該
	//     エラーを伝達する（#52 確認事項 4: #38 Req 1.4「URL 生成失敗時 pending_bind 維持」を
	//     「URL 生成失敗時は pending_bind 行を作らない」へ解釈変更）。
	//   - 成功時は SignupURL（URL と後続 bind 用の signupURLName）を返す。
	//   - 成否を EventRecorder.Record に渡す（Req 1.5 / NFR 2.1）。拒否経路は構造化ログを出す
	//     （NFR 2.2）。
	//
	// actor は監査イベントの実行者識別子（admin_users.id）。
	Create(ctx context.Context, actor uuid.UUID, in CreateInput) (TenantView, SignupURL, error)

	// Bind は pending_bind テナントへ Enterprise を予約状態経由の 2 段確定でバインドする
	// （Req 1.x / 3.x / 4.2 / 4.3）。orphan Enterprise（DB に紐付かない AMAPI Enterprise）を
	// 構造的に作らないため、CreateEnterprise の前に pending_bind→binding の原子予約を挟む。
	//
	//   - Repository.Get で現状態を取得する。pending_bind 以外は予約も AMAPI 呼び出しもせず
	//     拒否する: binding / bound への新規 bind は CodeConflict（Req 1.2 / 1.3、新規
	//     Enterprise を作らない）、disabled への bind は CodeBusinessRule（Req 4.2）、定義外
	//     status は fail-closed で CodeBusinessRule（NFR 1.2）、不在は CodeNotFound（Req 4.3）。
	//   - 永続 row.SignupURLName を検証する。空（未永続化）なら fail-closed で CodeBusinessRule
	//     （422）を返し、ReserveBinding も CreateEnterprise も呼ばない（Req 3.4）。
	//   - Repository.ReserveBinding で pending_bind→binding を原子遷移する。affected=0（並行
	//     敗者 / 既に遷移済み）は CodeConflict（Req 1.2）で CreateEnterprise を呼ばない。
	//   - 予約勝者（affected=1）のみ amapi.CreateEnterprise を呼ぶ。**永続 row.SignupURLName** を
	//     渡す（発行元テナントへの束縛 / body の値は使わない / Req 3.2 / 3.3）。失敗（または空
	//     応答）時は Repository.ReleaseBinding で binding→pending_bind へ解放し再 bind 可能化
	//     する（Req 1.5 / 4.3）。AMAPI error は再分類しない。
	//   - 成功時 Repository.UpdateBound（WHERE status='binding'）で bound 確定する。affected=0 は
	//     回収 / disable との競合とみなし CodeConflict（Req 1.4）。UpdateBound 失敗 / affected=0
	//     では ReleaseBinding しない（sweep が後追い回収する / Req 2.1）。
	//   - 成否を EventRecorder.Record に渡す（Req 1.7）。拒否経路は構造化ログを出す（NFR 2.2）。
	//
	// in は後方互換のため維持するが、signup_url_name は永続値を正本に使うため読まない（Req 3.2）。
	// actor は監査イベントの実行者識別子（admin_users.id）。
	Bind(ctx context.Context, actor uuid.UUID, id uuid.UUID, in BindInput) (TenantView, error)

	// Disable はテナントを無効化する（Req 3.x）。disabled は終端状態（再有効化遷移なし）。
	//
	//   - Repository.Get で対象を取得する。不在は CodeNotFound、既に disabled は二重無効化
	//     として CodeConflict（Req 3.4）。
	//   - 二段階確認テキスト方式（design.md API Contract）: in.Confirmation が対象 row.Name と
	//     完全一致しなければ確認未完了として CodeBusinessRule（Req 3.2）。
	//   - 確認 OK で Repository.UpdateDisabled する。affected=0 は二重無効化競合として CodeConflict
	//     （Req 3.4）。
	//   - 成否を EventRecorder.Record に渡す（成功時 ConfirmationCompleted=true / Req 3.1 / 3.3 / 3.5）。
	//     拒否経路は構造化ログを出す（NFR 2.2）。
	//
	// actor は監査イベントの実行者識別子であり、無効化監査列（disabled_by）にも記録される。
	Disable(ctx context.Context, actor uuid.UUID, id uuid.UUID, in DisableInput) (TenantView, error)

	// Get は id でテナント詳細を取得し TenantView に変換する（Req 4.2）。
	// 不在は Repository 由来の CodeNotFound を伝達する（Req 4.3）。
	Get(ctx context.Context, id uuid.UUID) (TenantView, error)

	// List は全テナントを TenantView の slice として返す（Req 4.1）。
	// 0 件のときは非 nil の空 slice を返す（Req 4.4）。
	List(ctx context.Context) ([]TenantView, error)

	// EnterpriseNameForTenant は他ドメインの業務操作前提ガード（Req 5.1〜5.3）。
	//
	//   - bound: enterprise_name + nil を返す。ただし bound 行の enterprise_name が空（データ
	//     不整合）の場合は fail-closed で CodeBusinessRule（ErrInvalidState）を返す（NFR 1.1 防御）。
	//   - pending_bind: 未バインドとして CodeBusinessRule（ErrNotBound）を返す（Req 5.2）。
	//   - disabled: 無効化として CodeBusinessRule（ErrTenantDisabled）を返す（Req 5.3）。
	//   - 不在: CodeNotFound（ErrTenantNotFound）を返す（Req 5.1）。
	//
	// テナント分離（Req 6.5）は fail-closed で適用する。Repository は SuperAdmin context へ昇格し
	// 全 tenants 行を可視化するため、本メソッドが唯一の越境防止点である:
	//   - TenantContext 未確立: 認可文脈なしとして ErrTenantNotFound で拒否する（呼び出し側の
	//     context 設定漏れで任意 tenant の enterprise_name が漏れる経路を塞ぐ。SuperAdmin の内部
	//     経路は明示的に SuperAdmin TenantContext を確立してから呼ぶこと）。
	//   - tenant-scoped（IsSuperAdmin=false）が自テナント以外の id を要求: ErrTenantNotFound。
	//   - SuperAdmin context: 全 tenant 横断参照を許可。
	// 拒否時は存在差を露出しない ErrTenantNotFound で統一し、構造化ログを出す（NFR 2.2）。
	EnterpriseNameForTenant(ctx context.Context, id uuid.UUID) (string, error)
}

// service は Service interface の本番実装。
//
// deps は design.md L208-210 の Outbound 依存に対応する:
//   - repo:     Repository（永続化）
//   - amapi:    amapi.Client（Enterprise 作成代行 / 本 task では CreateSignupURL のみ使用）
//   - recorder: EventRecorder（監査記録）
//   - cfg:      config.Config（AMAPIProjectID。本 task では未使用だが Bind/task 5 が CreateEnterprise
//     の projectID 引数に使うため deps に含める / design.md L209）
//   - log:      logger.Logger（拒否経路の構造化ログ / NFR 2.2）
type service struct {
	repo     Repository
	amapi    amapi.Client
	recorder EventRecorder
	cfg      config.Config
	log      logger.Logger
}

// NewService は本番用 Service を構築する（`internal/auth.NewService` と同方式の deps 注入）。
//
// log が nil の場合は logger.Default()（未配線時は no-op）を採用し、DI 未配線でも構造化ログ
// 呼び出しで panic させない（`amapi.NewClient` / `NewLoggerRecorder` の nil-log フォールバックと
// 同方針）。
func NewService(
	repo Repository,
	client amapi.Client,
	recorder EventRecorder,
	cfg config.Config,
	log logger.Logger,
) Service {
	if log == nil {
		log = logger.Default()
	}
	return &service{
		repo:     repo,
		amapi:    client,
		recorder: recorder,
		cfg:      cfg,
		log:      log,
	}
}

// Create は Service.Create の実装。
func (s *service) Create(ctx context.Context, actor uuid.UUID, in CreateInput) (TenantView, SignupURL, error) {
	// 1. name 空白 trim 後の空入力を拒否（Req 1.3）。永続化・AMAPI 呼び出しは一切行わない。
	name := strings.TrimSpace(in.Name)
	if name == "" {
		err := pkgerrors.New(pkgerrors.CodeInvalidRequest, "tenant name is required")
		// 拒否経路の構造化ログ（NFR 2.2）+ 失敗監査（Req 1.5 / NFR 2.1）。対象テナントは未採番のため Nil。
		s.logDeny(actor, uuid.Nil, "tenant name is empty")
		s.record(ctx, actor, uuid.Nil, OperationCreate, ResultFailure, false, "tenant name is empty")
		return TenantView{}, SignupURL{}, err
	}

	// 2. サインアップ URL を先に発行する（Req 1.2）。signup_url_name は ② の戻り値であり、
	//    発行元テナントへ束縛する正本として Insert で永続化する必要があるため（Req 3.1）、
	//    Insert より前に呼ぶ（#52 で create 順序を CreateSignupURL→Insert へ変更）。URL 生成が
	//    失敗したときは Insert せず pending_bind 行を作らず当該エラーを伝達する（#52 確認事項 4）。
	//    AMAPI 由来 error は #34 が Code 正規化済みのため再分類しない。テナント id は Insert 前
	//    のため未採番だが、失敗監査の対象テナントは Nil で記録する。
	id := uuid.New()
	signupURL, signupURLName, err := s.amapi.CreateSignupURL(ctx)
	if err != nil {
		s.record(ctx, actor, uuid.Nil, OperationCreate, ResultFailure, false, "signup url creation failed")
		return TenantView{}, SignupURL{}, err
	}

	// 2b. AMAPI が成功扱い（err=nil）で空の URL / signup url name を返した場合は後続 bind の前提
	//     （Req 3.1 の signup_url_name 永続化・Req 2.1 の bind 入力）が壊れるため、上流の異常応答
	//     として CodeUpstream（502）を返し Insert しない（bind 側の空 enterprise_name ガードと対称）。
	if strings.TrimSpace(signupURL) == "" || strings.TrimSpace(signupURLName) == "" {
		err := pkgerrors.New(pkgerrors.CodeUpstream, "amapi returned an empty signup url")
		s.logDeny(actor, uuid.Nil, "amapi returned empty signup url")
		s.record(ctx, actor, uuid.Nil, OperationCreate, ResultFailure, false, "empty signup url from amapi")
		return TenantView{}, SignupURL{}, err
	}

	// 3. signup_url_name を含めた pending_bind 行を永続化する（Req 1.1 / NFR 1.1 / 3.1）。
	//    signup_url_name は発行元テナントへ束縛される正本であり、bind 時の CreateEnterprise 引数
	//    として永続値を使うため Insert で書き込む（Req 3.1）。
	row := TenantRow{
		ID:            id,
		Name:          name,
		Status:        StatusPendingBind,
		SignupURLName: signupURLName,
	}
	if err := s.repo.Insert(ctx, row); err != nil {
		s.record(ctx, actor, id, OperationCreate, ResultFailure, false, "tenant persistence failed")
		return TenantView{}, SignupURL{}, err
	}

	// 4. 作成成功を監査記録（Req 1.5 / NFR 2.1）。
	s.record(ctx, actor, id, OperationCreate, ResultSuccess, false, "")

	view := TenantView{
		ID:     id,
		Name:   name,
		Status: StatusPendingBind,
	}
	su := SignupURL{
		URL:  signupURL,
		Name: signupURLName,
	}
	return view, su, nil
}

// Bind は Service.Bind の実装（Req 1.x / 3.x / 4.2 / 4.3 / NFR 1.2 / 2.1）。
//
// 予約状態経由の 2 段確定で orphan Enterprise を防止する: 「Get で現状態確認 → 永続
// signup_url_name 検証 → ReserveBinding（pending_bind→binding 原子予約）→ 勝者のみ
// CreateEnterprise（永続値を渡す）→ 成功 UpdateBound（WHERE binding）/ 失敗 ReleaseBinding」の
// 順で行い、AMAPI I/O は tx の外に置く（design.md Bind シーケンス）。in.SignupURLName は読まず、
// 永続 row.SignupURLName を正本に使う（Req 3.2 / 3.3）。
func (s *service) Bind(ctx context.Context, actor uuid.UUID, id uuid.UUID, _ BindInput) (TenantView, error) {
	// 1. 現状態を取得（不在は CodeNotFound をそのまま伝達 → 404 / Req 4.3）。取得失敗も bind の
	//    失敗経路として監査記録する（Req 1.7 / NFR 2.1）。
	row, err := s.repo.Get(ctx, id)
	if err != nil {
		s.record(ctx, actor, id, OperationBind, ResultFailure, false, "tenant lookup failed")
		return TenantView{}, err
	}

	// 2. 前提状態判定（状態機械）。pending_bind 以外は予約も AMAPI も呼ばず拒否する。
	switch row.Status {
	case StatusPendingBind:
		// 正常な遷移可能状態。以降の signup_url_name 検証 → ReserveBinding へ進む。
	case StatusBinding:
		// 既に予約中のテナントへの新規 bind は競合。新規 Enterprise を作らない（Req 1.3）。
		s.logDeny(actor, id, "tenant binding is already in progress")
		s.record(ctx, actor, id, OperationBind, ResultFailure, false, "tenant already binding")
		return TenantView{}, ErrConflict
	case StatusBound:
		// bound 済みテナントへの再 bind は競合。新規 Enterprise を作らない（Req 1.2 / 1.3）。
		s.logDeny(actor, id, "tenant is already bound")
		s.record(ctx, actor, id, OperationBind, ResultFailure, false, "tenant already bound")
		return TenantView{}, ErrConflict
	case StatusDisabled:
		// 無効化テナントへの bind は前提状態違反（Req 4.2 / #38 Req 2.6 継続）。
		s.logDeny(actor, id, "tenant is disabled")
		s.record(ctx, actor, id, OperationBind, ResultFailure, false, "tenant disabled")
		return TenantView{}, ErrInvalidState
	default:
		// 定義外 status は fail-closed で不正状態として拒否（NFR 1.2）。
		s.logDeny(actor, id, "tenant state is invalid")
		s.record(ctx, actor, id, OperationBind, ResultFailure, false, "tenant state invalid")
		return TenantView{}, ErrInvalidState
	}

	// 3. 永続 signup_url_name を発行元束縛の正本として検証する（Req 3.2 / 3.3 / 3.4）。
	//    永続化されていない（空 / NULL→空文字）テナントの bind は fail-closed で 422 拒否し、
	//    ReserveBinding も CreateEnterprise も呼ばない。body の signup_url_name は読まない
	//    （他テナント値の混入経路を構造的に排除する / Req 3.3）。
	signupURLName := strings.TrimSpace(row.SignupURLName)
	if signupURLName == "" {
		err := pkgerrors.New(pkgerrors.CodeBusinessRule, "tenant signup url name is not provisioned")
		s.logDeny(actor, id, "tenant signup url name is not provisioned")
		s.record(ctx, actor, id, OperationBind, ResultFailure, false, "signup url name not provisioned")
		return TenantView{}, err
	}

	// 4. pending_bind→binding を原子予約する（Req 1.1）。並行 bind の勝者のみ affected=1 となり、
	//    敗者（affected=0）は CreateEnterprise を呼ばずに 409 競合で拒否する（Req 1.2 orphan 防止の核）。
	affected, err := s.repo.ReserveBinding(ctx, id)
	if err != nil {
		s.record(ctx, actor, id, OperationBind, ResultFailure, false, "binding reservation failed")
		return TenantView{}, err
	}
	if affected == 0 {
		// 並行敗者 / 既に遷移済み。CreateEnterprise を呼ばない（Req 1.2 / 1.3）。
		s.logDeny(actor, id, "tenant binding reservation lost to a concurrent request")
		s.record(ctx, actor, id, OperationBind, ResultFailure, false, "binding reservation conflict")
		return TenantView{}, ErrConflict
	}

	// 5. 予約勝者のみ Enterprise を作成（AMAPI）。**永続 signup_url_name** を渡す（Req 3.2）。
	//    失敗 / 空応答時は ReleaseBinding で binding→pending_bind へ解放し再 bind 可能化する
	//    （Req 1.5 / 4.3）。AMAPI 由来 error は #34 が Code 正規化済みのため再分類しない。
	enterpriseName, err := s.amapi.CreateEnterprise(ctx, signupURLName, s.cfg.AMAPIProjectID)
	if err != nil {
		s.releaseBindingBestEffort(ctx, id)
		s.record(ctx, actor, id, OperationBind, ResultFailure, false, "enterprise creation failed")
		return TenantView{}, err
	}

	// 5b. AMAPI が成功扱いで空の enterprise 識別子を返した場合は bound へ進めない（Req 1.5 / NFR 1.2）。
	//     enterprise_name は bound の不変条件（bound 時のみ非空 / NFR 1.1）であり、空のまま UpdateBound
	//     すると status=bound かつ enterprise 識別子なしの不整合行を作り、後続の業務操作前提ガード
	//     （Req 5.1）が壊れる。上流の異常応答として CodeUpstream（502）を返し binding を解放する。
	if strings.TrimSpace(enterpriseName) == "" {
		err := pkgerrors.New(pkgerrors.CodeUpstream, "amapi returned an empty enterprise name")
		s.releaseBindingBestEffort(ctx, id)
		s.logDeny(actor, id, "amapi returned empty enterprise name")
		s.record(ctx, actor, id, OperationBind, ResultFailure, false, "empty enterprise name from amapi")
		return TenantView{}, err
	}

	// 6. 条件付き UPDATE（WHERE status='binding'）で bound 確定（Req 1.4）。affected=0 / error 時は
	//    ReleaseBinding しない（回収 / disable と競合した敗者であり、sweep が後追い回収する / Req 2.1）。
	boundAffected, err := s.repo.UpdateBound(ctx, id, enterpriseName)
	if err != nil {
		// 部分一意 index 違反（23505）は Repository が CodeConflict へ写像済み。そのまま伝達する。
		s.record(ctx, actor, id, OperationBind, ResultFailure, false, "bind persistence failed")
		return TenantView{}, err
	}
	if boundAffected == 0 {
		// 回収 / disable との競合（既に binding でない）。新規 Enterprise を作っても行は更新されない（Req 1.4）。
		s.logDeny(actor, id, "tenant bind conflicts with current state")
		s.record(ctx, actor, id, OperationBind, ResultFailure, false, "bind conflict")
		return TenantView{}, ErrConflict
	}

	// 7. バインド成功を監査記録（Req 1.7 / NFR 2.1）。
	s.record(ctx, actor, id, OperationBind, ResultSuccess, false, "")

	return TenantView{
		ID:             id,
		Name:           row.Name,
		Status:         StatusBound,
		EnterpriseName: enterpriseName,
	}, nil
}

// releaseBindingBestEffort は CreateEnterprise 失敗 / 空応答時に binding 行を pending_bind へ
// best-effort で解放する（Req 1.5 / 4.3）。ReleaseBinding 自体が失敗した場合は元の bind error を
// 優先伝達するため戻り値を返さず、解放失敗を構造化ログに残す（sweep が後追い回収する保険 /
// design.md Error Strategy）。
func (s *service) releaseBindingBestEffort(ctx context.Context, id uuid.UUID) {
	if _, err := s.repo.ReleaseBinding(ctx, id); err != nil {
		s.log.Warn("tenant binding release failed after enterprise creation failure",
			logger.TenantID(id),
		)
	}
}

// Disable は Service.Disable の実装（Req 3.x）。
//
// 二段階確認テキスト方式で対象テナント名の再入力一致を検証し、条件付き UPDATE
// （WHERE status!='disabled'）で disabled へ遷移させる。disabled は終端状態。
func (s *service) Disable(ctx context.Context, actor uuid.UUID, id uuid.UUID, in DisableInput) (TenantView, error) {
	// 1. 対象を取得（不在は CodeNotFound → 404）。確認テキスト比較のため name が必要。取得失敗も
	//    disable の失敗経路として監査記録する（Req 3.5 / NFR 2.1）。
	row, err := s.repo.Get(ctx, id)
	if err != nil {
		s.record(ctx, actor, id, OperationDisable, ResultFailure, false, "tenant lookup failed")
		return TenantView{}, err
	}

	// 2. 前提状態判定（状態機械）。pending_bind / bound のみ無効化可能。
	switch row.Status {
	case StatusPendingBind, StatusBound:
		// 無効化可能な状態。以降の確認テキスト検証 → UpdateDisabled へ進む。
	case StatusDisabled:
		// 既に disabled なら二重無効化として拒否（Req 3.4）。disabled は終端で再遷移しない。
		s.logDeny(actor, id, "tenant is already disabled")
		s.record(ctx, actor, id, OperationDisable, ResultFailure, false, "tenant already disabled")
		return TenantView{}, ErrConflict
	default:
		// 定義外 status は fail-closed で不正状態として拒否し、disabled へ遷移させない（NFR 1.2 /
		// Bind 側の default 分岐と対称）。
		s.logDeny(actor, id, "tenant state is invalid")
		s.record(ctx, actor, id, OperationDisable, ResultFailure, false, "tenant state invalid")
		return TenantView{}, ErrInvalidState
	}

	// 3. 二段階確認テキスト検証（対象テナント name との完全一致 / Req 3.2）。
	//    不一致は確認未完了として拒否し、永続化しない。
	if in.Confirmation != row.Name {
		s.logDeny(actor, id, "two-step confirmation text does not match")
		s.record(ctx, actor, id, OperationDisable, ResultFailure, false, "confirmation required")
		return TenantView{}, ErrConfirmationRequired
	}

	// 4. 条件付き UPDATE（WHERE status!='disabled'）で無効化 + 監査列記録（Req 3.1）。
	affected, err := s.repo.UpdateDisabled(ctx, id, actor)
	if err != nil {
		s.record(ctx, actor, id, OperationDisable, ResultFailure, true, "disable persistence failed")
		return TenantView{}, err
	}
	if affected == 0 {
		// 確認後に他要求が先に無効化した競合（二重無効化 / Req 3.4）。
		s.logDeny(actor, id, "tenant disable conflicts with current state")
		s.record(ctx, actor, id, OperationDisable, ResultFailure, true, "disable conflict")
		return TenantView{}, ErrConflict
	}

	// 5. 無効化成功を監査記録（確認完了済みのため ConfirmationCompleted=true / Req 3.1 / 3.3 / 3.5）。
	s.record(ctx, actor, id, OperationDisable, ResultSuccess, true, "")

	// disabled view を返す。enterprise_name は disabled では露出しない（omitempty で省略 / Req 6.5）。
	return TenantView{
		ID:     id,
		Name:   row.Name,
		Status: StatusDisabled,
	}, nil
}

// Get は Service.Get の実装。Repository へ委譲し TenantView へ変換する（Req 4.2 / 4.3）。
func (s *service) Get(ctx context.Context, id uuid.UUID) (TenantView, error) {
	row, err := s.repo.Get(ctx, id)
	if err != nil {
		// 不在（CodeNotFound）はそのまま伝達する（Req 4.3 / 存在差は Repository が汎用 message で隠蔽済み）。
		return TenantView{}, err
	}
	return ViewFromRow(row), nil
}

// List は Service.List の実装。Repository へ委譲し各行を TenantView へ変換する（Req 4.1 / 4.4）。
func (s *service) List(ctx context.Context) ([]TenantView, error) {
	rows, err := s.repo.List(ctx)
	if err != nil {
		return nil, err
	}
	// 0 件でも非 nil の空 slice を返す（Req 4.4）。Repository も空 slice を返すが View 側でも保証する。
	views := make([]TenantView, 0, len(rows))
	for _, row := range rows {
		views = append(views, ViewFromRow(row))
	}
	return views, nil
}

// EnterpriseNameForTenant は Service.EnterpriseNameForTenant の実装（Req 5.1〜5.3）。
//
// 他ドメインの業務操作前提ガード。bound のみ enterprise_name を返し、pending_bind / disabled は
// 拒否、不在は CodeNotFound を返す。拒否時は構造化ログを出す（NFR 2.2）。
func (s *service) EnterpriseNameForTenant(ctx context.Context, id uuid.UUID) (string, error) {
	// テナント分離ガード（Req 6.5 / テナント分離）を fail-closed で適用する。本 IF は他ドメインの
	// tenant-scoped 文脈から呼ばれ、続く Repository は全メソッドで SuperAdmin context へ昇格して
	// 全 tenants 行を可視化する。したがって本メソッドが唯一の越境防止点であり、認可文脈を確認
	// できない呼び出しは fail-open にせずすべて拒否する:
	//   - TenantContext 未確立（呼び出し側の context 設定漏れ等）: ErrTenantNotFound（fail-closed）。
	//     これにより Repository の SuperAdmin 昇格に委ねて任意 tenant の enterprise_name を返して
	//     しまう経路を構造的に塞ぐ。SuperAdmin の内部経路も明示的に SuperAdmin TenantContext を
	//     確立してから呼ぶこと。
	//   - tenant-scoped（IsSuperAdmin=false）かつ自テナント以外の id 要求: ErrTenantNotFound。
	//   - SuperAdmin context: 全 tenant 横断参照を許可（admin 経路）。
	// 拒否時は存在差を露出しない ErrTenantNotFound（404）で統一し、構造化ログを出す（NFR 2.2）。
	tc, ctxErr := db.FromContext(ctx)
	if ctxErr != nil {
		s.logDeny(uuid.Nil, id, "tenant context is required for enterprise name access")
		return "", ErrTenantNotFound
	}
	if !tc.IsSuperAdmin && tc.TenantID != id {
		s.logDeny(tc.AdminUserID, id, "cross-tenant enterprise name access denied")
		return "", ErrTenantNotFound
	}

	row, err := s.repo.Get(ctx, id)
	if err != nil {
		// 不在は CodeNotFound（Req 5.1）。Repository が ErrTenantNotFound 相当を wrap 済み。
		return "", err
	}

	switch row.Status {
	case StatusBound:
		// bound だが enterprise_name が空 = データ不整合（DDL は bound 行の enterprise_name 非空を
		// 強制しない）。空の識別子を下流 AMAPI 操作へ渡すと前提ガード（Req 5.1）が壊れるため、
		// fail-closed で不正状態として拒否する（NFR 1.1 invariant 防御 / Bind 側 3b の空応答ガードと対称）。
		if strings.TrimSpace(row.EnterpriseName) == "" {
			s.logDeny(tc.AdminUserID, id, "bound tenant has empty enterprise name")
			return "", ErrInvalidState
		}
		return row.EnterpriseName, nil
	case StatusPendingBind:
		// 未バインドテナントへの enterprise 識別子要求は拒否（Req 5.2）。
		s.logDeny(tc.AdminUserID, id, "tenant is not bound to an enterprise")
		return "", ErrNotBound
	case StatusDisabled:
		// 無効化テナントへの業務操作要求は拒否（Req 5.3）。
		s.logDeny(tc.AdminUserID, id, "tenant is disabled")
		return "", ErrTenantDisabled
	default:
		// 定義外 status は fail-closed で不正状態として拒否（NFR 1.1 / 1.2 の防御）。
		s.logDeny(tc.AdminUserID, id, "tenant state is invalid")
		return "", ErrInvalidState
	}
}

// record は監査イベントを EventRecorder へ渡す helper（Req 1.5 / NFR 2.1）。
//
// Record のエラーは監査記録の暫定実装（logger）では発生しないが、将来の Audit Service 実装で
// 永続化失敗が起き得るため、失敗時は観測ログのみ残してユースケース本体のエラー経路を上書き
// しない（auth の revokeOnExpire と同方針）。
func (s *service) record(ctx context.Context, actor, tenantID uuid.UUID, op Operation, result Result, confirmed bool, denyReason string) {
	e := Event{
		Actor:                 actor,
		TenantID:              tenantID,
		Operation:             op,
		Result:                result,
		ConfirmationCompleted: confirmed,
		DenyReason:            denyReason,
	}
	if err := s.recorder.Record(ctx, e); err != nil {
		s.log.Warn("tenant audit record failed",
			"operation", string(op),
			logger.ActorID(actor),
			logger.TenantID(tenantID),
		)
	}
}

// logDeny は拒否された操作の原因分析属性（実行者・対象テナント・拒否理由）を構造化ログとして
// 出力する（NFR 2.2）。機密値は含めない（reason は人間可読な短い拒否理由のみ）。
//
// actor には実行者識別子（admin_users.id）を渡す。Create/Bind/Disable は Handler から受け取った
// actor を、EnterpriseNameForTenant の前提ガード拒否は確立済み TenantContext の `AdminUserID` を
// 渡す（NFR 2.2 の「実行者」を満たすため）。実行者が真に不明な経路（TenantContext 未確立で
// fail-closed 拒否する場合のみ）に限り uuid.Nil を渡す。
func (s *service) logDeny(actor, tenantID uuid.UUID, reason string) {
	s.log.Warn("tenant operation denied",
		logger.ActorID(actor),
		logger.TenantID(tenantID),
		"deny_reason", reason,
	)
}

// 型 assertion 用に service が Service interface を満たすことを compile-time で確認する。
var _ Service = (*service)(nil)
