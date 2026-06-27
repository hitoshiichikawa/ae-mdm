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
	// Create は新規テナントを作成する（Req 1.1〜1.5）。
	//
	//   - in.Name を空白 trim 後に空なら CodeInvalidRequest を返し、永続化も AMAPI 呼び出しも
	//     行わない（Req 1.3）。
	//   - Repository.Insert で pending_bind 行を先に作成する（Req 1.1）。続いて
	//     amapi.CreateSignupURL でサインアップ URL を発行する（Req 1.2）。URL 生成が失敗（または
	//     空応答）してもテナントを pending_bind のまま保持し、当該エラーを伝達する（Req 1.4）。
	//   - 成功時は SignupURL（URL と後続 bind 用の signupURLName）を返す。
	//   - 成否を EventRecorder.Record に渡す（Req 1.5 / NFR 2.1）。拒否経路は構造化ログを出す
	//     （NFR 2.2）。
	//
	// actor は監査イベントの実行者識別子（admin_users.id）。
	Create(ctx context.Context, actor uuid.UUID, in CreateInput) (TenantView, SignupURL, error)

	// Bind は pending_bind テナントへ Enterprise をバインドする（Req 2.x / NFR 1.3）。
	//
	//   - Repository.Get で現状態を取得する。pending_bind 以外は永続化も AMAPI 呼び出しも
	//     せず拒否する: bound への再 bind は CodeConflict（Req 2.5、新規 Enterprise を作らない）、
	//     disabled への bind は CodeBusinessRule（Req 2.6）、定義外 status は fail-closed で
	//     CodeBusinessRule（NFR 1.2）、不在は CodeNotFound。
	//   - pending_bind のときのみ amapi.CreateEnterprise を呼ぶ。失敗時は永続化せずエラーを
	//     伝達し行を pending_bind に保つ（Req 2.4 / NFR 1.3）。AMAPI error は再分類しない。
	//   - 成功時 Repository.UpdateBound で bound 確定する。affected=0 は競合とみなし CodeConflict
	//     （Req 2.1 / 2.2 / 2.5）。
	//   - 成否を EventRecorder.Record に渡す（Req 2.7）。拒否経路は構造化ログを出す（NFR 2.2）。
	//
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
	//   - bound: enterprise_name + nil を返す。
	//   - pending_bind: 未バインドとして CodeBusinessRule（ErrNotBound）を返す（Req 5.2）。
	//   - disabled: 無効化として CodeBusinessRule（ErrTenantDisabled）を返す（Req 5.3）。
	//   - 不在: CodeNotFound（ErrTenantNotFound）を返す（Req 5.1）。
	//
	// tenant-scoped な呼び出し元（ctx の TenantContext が IsSuperAdmin=false）が自テナント以外の
	// id を要求した場合は、存在差を露出しない ErrTenantNotFound で拒否する（テナント分離 / Req 6.5）。
	// 拒否時は構造化ログを出す（NFR 2.2）。
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

	// 2. pending_bind 行を先に採番して永続化（Req 1.1 / NFR 1.1 / 3.1）。
	//    サインアップ URL 生成が失敗してもテナントを pending_bind のまま保持する Req 1.4 を満たすため、
	//    AMAPI 呼び出しの前に Insert する（永続化済みであれば URL 生成失敗後も pending_bind 行が残る）。
	id := uuid.New()
	row := TenantRow{
		ID:     id,
		Name:   name,
		Status: StatusPendingBind,
	}
	if err := s.repo.Insert(ctx, row); err != nil {
		s.record(ctx, actor, id, OperationCreate, ResultFailure, false, "tenant persistence failed")
		return TenantView{}, SignupURL{}, err
	}

	// 3. サインアップ URL を発行（Req 1.2）。失敗時はテナントを pending_bind のまま保持し、当該
	//    エラーを伝達する（Req 1.4）。AMAPI 由来 error は #34 が Code 正規化済みのため再分類しない。
	signupURL, signupURLName, err := s.amapi.CreateSignupURL(ctx)
	if err != nil {
		s.record(ctx, actor, id, OperationCreate, ResultFailure, false, "signup url creation failed")
		return TenantView{}, SignupURL{}, err
	}

	// 3b. AMAPI が成功扱い（err=nil）で空の URL / signup url name を返した場合は後続 bind の前提
	//     （Req 2.1 の signup_url_name 入力）が壊れるため、上流の異常応答として CodeUpstream（502）を
	//     返しテナントを pending_bind に保つ（Req 1.2 / 1.4。bind 側の空 enterprise_name ガードと対称）。
	if strings.TrimSpace(signupURL) == "" || strings.TrimSpace(signupURLName) == "" {
		err := pkgerrors.New(pkgerrors.CodeUpstream, "amapi returned an empty signup url")
		s.logDeny(actor, id, "amapi returned empty signup url")
		s.record(ctx, actor, id, OperationCreate, ResultFailure, false, "empty signup url from amapi")
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

// Bind は Service.Bind の実装（Req 2.x / NFR 1.2 / 1.3）。
//
// 状態遷移は「Get で現状態確認 → pending_bind のみ CreateEnterprise → 成功時のみ条件付き
// UpdateBound で bound 確定」の順で行い、AMAPI I/O は tx の外に置く（design.md Bind シーケンス）。
func (s *service) Bind(ctx context.Context, actor uuid.UUID, id uuid.UUID, in BindInput) (TenantView, error) {
	// 0. 入力検証: signup_url_name は空白 trim 後に非空であること（設計 bind API の 400 契約）。
	//    空のまま CreateEnterprise に渡すと入力検証を AMAPI 側の失敗に依存させてしまうため、
	//    AMAPI 呼び出し・永続化の前にローカルで CodeInvalidRequest（400）を返す（Req 2.1）。
	signupURLName := strings.TrimSpace(in.SignupURLName)
	if signupURLName == "" {
		err := pkgerrors.New(pkgerrors.CodeInvalidRequest, "signup_url_name is required")
		s.logDeny(actor, id, "signup_url_name is empty")
		s.record(ctx, actor, id, OperationBind, ResultFailure, false, "signup url name is empty")
		return TenantView{}, err
	}

	// 1. 現状態を取得（不在は CodeNotFound をそのまま伝達 → 404 / Req 4.3）。取得失敗も bind の
	//    失敗経路として監査記録する（Req 2.7 / NFR 2.1）。
	row, err := s.repo.Get(ctx, id)
	if err != nil {
		s.record(ctx, actor, id, OperationBind, ResultFailure, false, "tenant lookup failed")
		return TenantView{}, err
	}

	// 2. 前提状態判定（状態機械）。pending_bind 以外は永続化も AMAPI も呼ばず拒否する。
	switch row.Status {
	case StatusPendingBind:
		// 正常な遷移可能状態。以降の AMAPI → UpdateBound へ進む。
	case StatusBound:
		// bound 済みテナントへの再 bind は競合。新規 Enterprise を作らない（Req 2.5）。
		s.logDeny(actor, id, "tenant is already bound")
		s.record(ctx, actor, id, OperationBind, ResultFailure, false, "tenant already bound")
		return TenantView{}, ErrConflict
	case StatusDisabled:
		// 無効化テナントへの bind は前提状態違反（Req 2.6）。
		s.logDeny(actor, id, "tenant is disabled")
		s.record(ctx, actor, id, OperationBind, ResultFailure, false, "tenant disabled")
		return TenantView{}, ErrInvalidState
	default:
		// 定義外 status は fail-closed で不正状態として拒否（NFR 1.2）。
		s.logDeny(actor, id, "tenant state is invalid")
		s.record(ctx, actor, id, OperationBind, ResultFailure, false, "tenant state invalid")
		return TenantView{}, ErrInvalidState
	}

	// 3. Enterprise を作成（AMAPI）。失敗時は永続化せずエラーを伝達し pending_bind を保つ
	//    （Req 2.4 / NFR 1.3）。AMAPI 由来 error は #34 が Code 正規化済みのため再分類しない。
	enterpriseName, err := s.amapi.CreateEnterprise(ctx, signupURLName, s.cfg.AMAPIProjectID)
	if err != nil {
		s.record(ctx, actor, id, OperationBind, ResultFailure, false, "enterprise creation failed")
		return TenantView{}, err
	}

	// 3b. AMAPI が成功扱いで空の enterprise 識別子を返した場合は bound へ進めない（Req 2.1 / NFR 1.3）。
	//     enterprise_name は bound の不変条件（bound 時のみ非空 / NFR 1.1）であり、空のまま UpdateBound
	//     すると status=bound かつ enterprise 識別子なしの不整合行を作り、後続の業務操作前提ガード
	//     （Req 5.1）が壊れる。上流の異常応答として CodeUpstream（502）を返し pending_bind を保つ。
	if strings.TrimSpace(enterpriseName) == "" {
		err := pkgerrors.New(pkgerrors.CodeUpstream, "amapi returned an empty enterprise name")
		s.logDeny(actor, id, "amapi returned empty enterprise name")
		s.record(ctx, actor, id, OperationBind, ResultFailure, false, "empty enterprise name from amapi")
		return TenantView{}, err
	}

	// 4. 条件付き UPDATE（WHERE status='pending_bind'）で bound 確定（Req 2.1 / 2.2）。
	affected, err := s.repo.UpdateBound(ctx, id, enterpriseName)
	if err != nil {
		// 部分一意 index 違反（23505）は Repository が CodeConflict へ写像済み。そのまま伝達する。
		s.record(ctx, actor, id, OperationBind, ResultFailure, false, "bind persistence failed")
		return TenantView{}, err
	}
	if affected == 0 {
		// 他要求との競合（既に bound 等）。新規 Enterprise を作っても行は更新されない（Req 2.5）。
		s.logDeny(actor, id, "tenant bind conflicts with current state")
		s.record(ctx, actor, id, OperationBind, ResultFailure, false, "bind conflict")
		return TenantView{}, ErrConflict
	}

	// 5. バインド成功を監査記録（Req 2.7 / NFR 2.1）。
	s.record(ctx, actor, id, OperationBind, ResultSuccess, false, "")

	return TenantView{
		ID:             id,
		Name:           row.Name,
		Status:         StatusBound,
		EnterpriseName: enterpriseName,
	}, nil
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

	// 2. 既に disabled なら二重無効化として拒否（Req 3.4）。disabled は終端で再遷移しない。
	if row.Status == StatusDisabled {
		s.logDeny(actor, id, "tenant is already disabled")
		s.record(ctx, actor, id, OperationDisable, ResultFailure, false, "tenant already disabled")
		return TenantView{}, ErrConflict
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
	// テナント分離ガード（Req 6.5 / テナント分離）: 本 IF は他ドメインの tenant-scoped 文脈から
	// 呼ばれる。Repository は全メソッドで SuperAdmin context へ昇格し全 tenants 行を可視化するため、
	// tenant-scoped な呼び出し元（IsSuperAdmin=false）が自テナント以外の id を要求した場合は
	// enterprise 識別子を返さず、存在差を露出しない ErrTenantNotFound（404）で拒否する。
	// TenantContext 未確立（SuperAdmin の内部経路 / 単体テスト等）では本ガードを適用しない。
	if tc, ctxErr := db.FromContext(ctx); ctxErr == nil && !tc.IsSuperAdmin && tc.TenantID != id {
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
		return row.EnterpriseName, nil
	case StatusPendingBind:
		// 未バインドテナントへの enterprise 識別子要求は拒否（Req 5.2）。
		s.logDeny(uuid.Nil, id, "tenant is not bound to an enterprise")
		return "", ErrNotBound
	case StatusDisabled:
		// 無効化テナントへの業務操作要求は拒否（Req 5.3）。
		s.logDeny(uuid.Nil, id, "tenant is disabled")
		return "", ErrTenantDisabled
	default:
		// 定義外 status は fail-closed で不正状態として拒否（NFR 1.1 / 1.2 の防御）。
		s.logDeny(uuid.Nil, id, "tenant state is invalid")
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
// actor が不明な経路（前提ガード等、ctx の actor を引き回さない呼び出し）では Nil を渡す。
func (s *service) logDeny(actor, tenantID uuid.UUID, reason string) {
	s.log.Warn("tenant operation denied",
		logger.ActorID(actor),
		logger.TenantID(tenantID),
		"deny_reason", reason,
	)
}

// 型 assertion 用に service が Service interface を満たすことを compile-time で確認する。
var _ Service = (*service)(nil)
