package tenant

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/amapi"
)

// Service は Tenant ライフサイクル（作成 / 参照 / 前提ガード）と状態機械の単一所有者
// （design.md「tenant.Service」節 / tasks.md task 4.1）。
//
// 本 task（4）では Create / Get / List / EnterpriseNameForTenant の 4 メソッドを実装する。
// Bind / Disable（状態機械の遷移系）は後続 task 5 で本 interface に追記・実装するため、
// 本 task では interface に宣言しない（「task 4 では実装しない」スコープと一致させ、本番実装
// struct にスタブを置かずに済む選択。詳細は impl-notes.md Task 4 learning 参照）。
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
	//   - amapi.CreateSignupURL でサインアップ URL を発行し、非 transient error なら永続化せず
	//     エラーを伝達する（Req 1.4）。
	//   - Repository.Insert で pending_bind 行を作成し、SignupURL を返す（Req 1.1 / 1.2）。
	//   - 成否を EventRecorder.Record に渡す（Req 1.5 / NFR 2.1）。拒否経路は構造化ログを出す
	//     （NFR 2.2）。
	//
	// actor は監査イベントの実行者識別子（admin_users.id）。
	Create(ctx context.Context, actor uuid.UUID, in CreateInput) (TenantView, SignupURL, error)

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
		// 拒否経路の構造化ログ（NFR 2.2）。実行者・対象テナント（未採番のため Nil）・拒否理由を出す。
		s.logDeny(actor, uuid.Nil, "tenant name is empty")
		return TenantView{}, SignupURL{}, err
	}

	// 2. サインアップ URL を発行（Req 1.2）。非 transient error は永続化せずに伝達（Req 1.4）。
	//    AMAPI 由来 error は #34 が Code 正規化済みのためそのまま伝播する（再分類しない）。
	signupURL, signupURLName, err := s.amapi.CreateSignupURL(ctx)
	if err != nil {
		// 作成失敗を監査記録（Req 1.5 / NFR 2.1）。tenant は未採番のため Nil。
		s.record(ctx, actor, uuid.Nil, OperationCreate, ResultFailure, false, "signup url creation failed")
		return TenantView{}, SignupURL{}, err
	}

	// 3. pending_bind 行を採番して永続化（Req 1.1 / NFR 1.1 / 3.1）。
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
