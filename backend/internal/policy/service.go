package policy

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/hitoshiichikawa/ae-mdm/internal/audit"
	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/amapi"
)

// Service は Policy upsert / 割当 / 参照 / 削除のユースケースと検証ゲート・AMAPI
// オーケストレーション・監査記録の単一所有者（design.md「policy.Service」節）。
//
// **最終形 interface（task 4.1 で 6 メソッドへ拡張済み）**: task 3 では upsert 系の
// Create / Update のみだったが、task 4.1 で参照系（Get / List）と変更系（Delete / Assign）の
// 4 メソッドを追加し、design.md の最終形（6 メソッド）に揃えた。
type Service interface {
	// Create はポリシーを新規作成し、検証ゲート → AMAPI upsert → snapshot 永続化 → 監査記録の
	// 順で実行する（Req 1.1 / 1.3 / 2.x / 5.1）。
	//
	//   - mapper で raw body を PolicyInput へ変換し、変換不能（型不整合 / 必須キー欠落）と
	//     `policy.Validate` の検証不正を結合して全件提示する。1 件でも不正があれば AMAPI 反映・
	//     snapshot 保存を行わず早期 return する（Req 2.2〜2.5）。
	//   - 検証通過後に enterprise_name を解決し、`amapi.Client.UpsertPolicy` へ反映する。AMAPI が
	//     再試行不可エラーを返した場合は snapshot を確定保存せずそのまま伝達する（Req 1.4 / NFR 2.2）。
	//   - AMAPI 反映成功後に Repository へ snapshot（送信した raw body）を永続化する（Req 1.3 / 1.5）。
	//   - 成否を監査記録の対象として渡す（Req 5.1 / 5.2）。監査 Detail / 構造化ログに raw body の
	//     機密値を載せない（Req 5.4 / NFR 3.1 / 3.2）。
	//
	// actor は監査イベントの実行者識別子（admin_users.id）。tenantID は claims 由来の所有テナント。
	Create(ctx context.Context, actor, tenantID uuid.UUID, in PolicyRequest) (PolicyView, error)

	// Update は既存ポリシーを更新し、検証ゲート → 既存行確認 → AMAPI 反映 → snapshot 更新 →
	// 監査記録の順で実行する（Req 1.2 / 1.5 / 2.x / 4.1 / 5.2）。
	//
	//   - 検証ゲートは Create と同一。検証失敗時は AMAPI 反映・snapshot 保存を行わない（Req 2.5）。
	//   - 対象 policyID の既存行を Repository.Get で取得し、自テナント不在は NotFound を伝達する
	//     （Req 4.1 / 4.4 / 4.5）。
	//   - 既存行の amapi_policy_name へ AMAPI 反映し、成功後に snapshot を更新する（Req 1.2 / 1.5）。
	//
	// actor / tenantID は Create と同義。policyID は更新対象のポリシー id。
	Update(ctx context.Context, actor, tenantID, policyID uuid.UUID, in PolicyRequest) (PolicyView, error)

	// List は自テナントのポリシー一覧を返す（Req 4.4）。
	//
	//   - tenant-scoped Repository.List へ委譲し、RLS により自テナント行のみを返す。
	//   - 0 件のときは非 nil の空 slice を返す（Handler が `[]` をそのままシリアライズできる）。
	//   - read 操作のため監査記録は行わない（Req 5.x は作成・更新・削除・割当のみが対象）。
	List(ctx context.Context, tenantID uuid.UUID) ([]PolicySummary, error)

	// Get は自テナントのポリシー詳細を返す。不在は NotFound（Req 4.4 / 4.5）。
	//
	//   - tenant-scoped Repository.Get へ委譲し、RLS で他テナント行は 0 行 → 存在差を露出しない
	//     ErrPolicyNotFound（404）に写像済みのまま伝達する（Req 4.5）。
	//   - read 操作のため監査記録は行わない。
	Get(ctx context.Context, tenantID, policyID uuid.UUID) (PolicyView, error)

	// Delete はポリシーを削除し、削除イベントを監査する（Req 5.3）。
	//
	//   - Repository.Delete へ委譲する。affected=0（不在 / 他テナント越境）は存在差を露出しない
	//     ErrPolicyNotFound（404）へ Service 側で写像する（Req 4.4 / 4.5）。
	//   - 割当済み端末が存在する policy の削除は Repository が ErrDeleteConflict（409）を Code 付きで
	//     返すため、そのまま伝達する（design 確認事項 3 推奨案 / Req 5.3）。
	//   - 成否いずれの経路でも削除イベントを監査記録する（Req 5.3）。監査 Detail に機密値を載せない
	//     （Req 5.4）。
	//
	// actor は監査イベントの実行者識別子。tenantID は claims 由来の所有テナント。
	Delete(ctx context.Context, actor, tenantID, policyID uuid.UUID) error

	// Assign は端末の適用対象ポリシーを確定する（Req 3.x / 4.2 / 4.3）。
	//
	//   - Repository.AssignPolicyToDevice へ委譲し devices.applied_policy_id を UPDATE する。
	//     割当は DB 更新までで AMAPI への device patch は行わない（design 確認事項 1 推奨案）。
	//   - 他テナント device 指定は affected=0（err==nil）で返るため Service が ErrPolicyNotFound
	//     （404）へ写像する（Req 3.3）。他テナント policy 指定は複合 FK 違反を Repository が
	//     ErrPolicyNotFound（404 / 存在差非露出）へ写像済みのまま伝達する（Req 3.2 / 4.2 / 4.5）。
	//   - 成否いずれの経路でも割当イベントを監査記録する。監査 Detail に機密値を載せない（Req 5.4）。
	//
	// actor は実行者識別子。tenantID は claims 由来。deviceID は割当先端末、policyID は割当ポリシー。
	Assign(ctx context.Context, actor, tenantID, deviceID, policyID uuid.UUID) error
}

// upsertClient は Service が AMAPI 反映に用いる最小ポート（consumer-defines-interface）。
//
// amapi.Client の全 IF ではなく本 task が使う UpsertPolicy のみに限定し、テストで fake を
// 差し込みやすくする（tenant が amapi.Client 全体を持つのとは対照的に、Policy upsert は
// UpsertPolicy のみで完結するため最小ポートにする / design「Service deps」節）。
type upsertClient interface {
	// UpsertPolicy は Policy を AMAPI へ upsert する。policyName には短い policyId を渡す
	// （AMAPI Client が enterpriseName + "/policies/" + policyName で resourceName を組み立てる）。
	UpsertPolicy(ctx context.Context, enterpriseName, policyName string, body amapi.PolicyBody) error
}

// eventRecorder はポリシー操作の監査イベントを Audit Service へ渡す最小ポート（Req 5.x）。
//
// tenant.EventRecorder と同型の最小 port（Record 1 本）。audit.Service が満たす。投機的に
// 全 audit.Service IF を要求せず、本 task が使う Record のみに限定する。
type eventRecorder interface {
	// Record は 1 件の監査イベントを記録する。永続化失敗は呼び出し側へ返り得る。
	Record(ctx context.Context, ev audit.Event) error
}

// enterpriseResolver は tenantID から AMAPI enterprise_name を解決する最小ポート（Req 1.1）。
//
// tenant.Service.EnterpriseNameForTenant を満たす。bound テナントのみ enterprise_name + nil を
// 返し、未 bind / disabled / 不在 / 越境は Code 付き error を返す（tenant.Service 契約）。
type enterpriseResolver interface {
	// EnterpriseNameForTenant は tenant-scoped context で enterprise_name を解決する。
	EnterpriseNameForTenant(ctx context.Context, id uuid.UUID) (string, error)
}

// ValidationFailedError は mapper の変換不能 + Validator の検証不正を結合した全件を保持する
// policy 固有のエラー型（Req 2.2 / 2.3 / 2.4）。
//
// `errors.Error` には details フィールドが無いため、検証エラー全件の搬送には本型を用いる。
// Handler（task 5）が型 assertion して details を HTTP body へ展開する想定。本型は全件を
// 保持しつつ `*errors.Error`（top-level Code 付き）を Unwrap で公開し、`errors.As`/`WriteHTTP`
// が top-level HTTP status を解決できるようにする。
//
// **top-level Code 規則**: 結合後の Errors に KindInvalidField が 1 件でもあれば 400
// （CodeInvalidRequest）、全件が KindBusinessRule なら 422（CodeBusinessRule）。
//
// Message には raw body の生値（機密値）を載せない（Req 5.4 / NFR 3.2）。
type ValidationFailedError struct {
	// Errors は検出されたすべての不正項目（mapper 変換不能 + Validator 検証不正の結合）。
	Errors []ValidationError
	// wrapped は top-level Code を保持する *errors.Error。Unwrap で公開する。
	wrapped *pkgerrors.Error
}

// Error は error interface を満たす。機密値（raw body 生値）は含めず、件数のみを示す。
func (e *ValidationFailedError) Error() string {
	if e == nil || e.wrapped == nil {
		return "policy validation failed"
	}
	return e.wrapped.Error()
}

// Unwrap は top-level Code を持つ *errors.Error を公開し、errors.As / errors.Is /
// errors.WriteHTTP が HTTP status を解決できるようにする。
func (e *ValidationFailedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.wrapped
}

// Code は top-level の errors.Code を返す（KindInvalidField 混在→400 / 全件 BusinessRule→422）。
func (e *ValidationFailedError) Code() pkgerrors.Code {
	if e == nil || e.wrapped == nil {
		return pkgerrors.CodeInvalidRequest
	}
	return e.wrapped.Code
}

// newValidationFailedError は結合済みの ValidationError 全件から top-level Code を決定して
// ValidationFailedError を構築する（Req 2.2 / 2.3 / 2.4）。
//
// top-level Code 規則: KindInvalidField が 1 件でもあれば 400（invalid field を優先して提示し、
// 入力フォーマット不正を business rule より先に直させる）、全件が KindBusinessRule なら 422。
func newValidationFailedError(errs []ValidationError) *ValidationFailedError {
	code := pkgerrors.CodeBusinessRule
	for _, e := range errs {
		if e.Kind == KindInvalidField {
			code = pkgerrors.CodeInvalidRequest
			break
		}
	}
	return &ValidationFailedError{
		Errors:  errs,
		wrapped: pkgerrors.New(code, "policy validation failed"),
	}
}

// 監査イベント種別（design Data Models）。EventType は任意 string（audit.EventType）。
const (
	// eventTypePolicyCreate はポリシー作成監査イベント種別。
	eventTypePolicyCreate audit.EventType = "policy_create"
	// eventTypePolicyUpdate はポリシー更新監査イベント種別。
	eventTypePolicyUpdate audit.EventType = "policy_update"
	// eventTypePolicyDelete はポリシー削除監査イベント種別（Req 5.3）。
	eventTypePolicyDelete audit.EventType = "policy_delete"
	// eventTypePolicyAssign はポリシーの端末割当監査イベント種別。
	eventTypePolicyAssign audit.EventType = "policy_assign"
)

// service は Service interface の本番実装。
//
// deps は design.md「Service deps（consumer-defines-interface）」に対応する最小依存:
//   - repo:     policy.Repository（snapshot 永続化 / 既存行確認）
//   - amapi:    upsertClient（AMAPI 反映の最小ポート / amapi.Client が満たす）
//   - recorder: eventRecorder（監査記録の最小ポート / audit.Service が満たす）
//   - tenants:  enterpriseResolver（enterprise_name 解決 / tenant.Service が満たす）
//   - log:      logger.Logger（拒否経路の構造化ログ / NFR 3.1）
//
// **authorizer は持たない**: authz は Handler（task 5）の責務（design Components）。
type service struct {
	repo     Repository
	amapi    upsertClient
	recorder eventRecorder
	tenants  enterpriseResolver
	log      logger.Logger
}

// NewService は本番用 Service を構築する（tenant.NewService と同方式の deps 注入）。
//
// log が nil の場合は logger.Default()（未配線時は no-op）を採用し、DI 未配線でも構造化ログ
// 呼び出しで panic させない（amapi.NewClient / tenant.NewService の nil-log フォールバックと同方針）。
func NewService(
	repo Repository,
	client upsertClient,
	recorder eventRecorder,
	tenants enterpriseResolver,
	log logger.Logger,
) Service {
	if log == nil {
		log = logger.Default()
	}
	return &service{
		repo:     repo,
		amapi:    client,
		recorder: recorder,
		tenants:  tenants,
		log:      log,
	}
}

// Create は Service.Create の実装。
func (s *service) Create(ctx context.Context, actor, tenantID uuid.UUID, in PolicyRequest) (PolicyView, error) {
	// 1. 検証ゲート（mapper 変換 + Validate 結合 / 全件提示 / 早期 return）。
	//    検証失敗時は AMAPI / Repository を一切呼ばない（Req 2.5）。
	if verr := s.validate(in.Body); verr != nil {
		s.logDeny(actor, tenantID, uuid.Nil, "policy validation failed")
		s.record(ctx, actor, tenantID, uuid.Nil, eventTypePolicyCreate, audit.ResultFailure, in.Name)
		return PolicyView{}, verr
	}

	// 2. enterprise_name 解決（tenant-scoped context / bound のみ成功 / Req 1.1）。
	enterpriseName, err := s.tenants.EnterpriseNameForTenant(ctx, tenantID)
	if err != nil {
		s.logDeny(actor, tenantID, uuid.Nil, "enterprise name resolution failed")
		s.record(ctx, actor, tenantID, uuid.Nil, eventTypePolicyCreate, audit.ResultFailure, in.Name)
		return PolicyView{}, err
	}

	// 3. id 採番（policyId seed として決定論的命名に用いる / design Data Models）。
	id := uuid.New()
	policyID := id.String()
	amapiPolicyName := buildAMAPIPolicyName(enterpriseName, policyID)

	// 4. AMAPI 反映（UpsertPolicy には短い policyId を渡す。enterpriseName を二重連結しない）。
	//    再試行不可エラーは snapshot を確定保存せずそのまま伝達する（Req 1.4 / NFR 2.2）。
	body := BuildPolicyBody(amapiPolicyName, in.Body)
	if err := s.amapi.UpsertPolicy(ctx, enterpriseName, policyID, body); err != nil {
		s.logDeny(actor, tenantID, id, "amapi upsert failed")
		s.record(ctx, actor, tenantID, id, eventTypePolicyCreate, audit.ResultFailure, in.Name)
		return PolicyView{}, err
	}

	// 5. AMAPI 反映成功後に snapshot を永続化する（Req 1.3 / 1.5）。送信した raw body を pass-through。
	//    Create の version は初期値 0（AMAPI が version を返さない MVP 制約 / impl-notes 確認事項参照）。
	row := PolicyRow{
		ID:              id,
		TenantID:        tenantID,
		Name:            in.Name,
		AMAPIPolicyName: amapiPolicyName,
		Body:            in.Body,
		Version:         0,
		UpdatedBy:       &actor,
	}
	if err := s.repo.Insert(ctx, row); err != nil {
		s.record(ctx, actor, tenantID, id, eventTypePolicyCreate, audit.ResultFailure, in.Name)
		return PolicyView{}, err
	}

	// 6. 作成成功を監査記録する（Req 5.1）。Detail は安全 field のみ（raw body を載せない）。
	s.record(ctx, actor, tenantID, id, eventTypePolicyCreate, audit.ResultSuccess, in.Name)

	return PolicyView{
		ID:      id,
		Name:    in.Name,
		Body:    in.Body,
		Version: row.Version,
	}, nil
}

// Update は Service.Update の実装。
func (s *service) Update(ctx context.Context, actor, tenantID, policyID uuid.UUID, in PolicyRequest) (PolicyView, error) {
	// 1. 検証ゲート（Create と同一 / 早期 return / Req 2.5）。
	if verr := s.validate(in.Body); verr != nil {
		s.logDeny(actor, tenantID, policyID, "policy validation failed")
		s.record(ctx, actor, tenantID, policyID, eventTypePolicyUpdate, audit.ResultFailure, in.Name)
		return PolicyView{}, verr
	}

	// 2. 既存行を取得（自テナント存在確認 / 不在は NotFound 伝達 / Req 4.1 / 4.4 / 4.5）。
	existing, err := s.repo.Get(ctx, tenantID, policyID)
	if err != nil {
		s.logDeny(actor, tenantID, policyID, "policy lookup failed")
		s.record(ctx, actor, tenantID, policyID, eventTypePolicyUpdate, audit.ResultFailure, in.Name)
		return PolicyView{}, err
	}

	// 3. enterprise_name 解決（Req 1.1）。
	enterpriseName, err := s.tenants.EnterpriseNameForTenant(ctx, tenantID)
	if err != nil {
		s.logDeny(actor, tenantID, policyID, "enterprise name resolution failed")
		s.record(ctx, actor, tenantID, policyID, eventTypePolicyUpdate, audit.ResultFailure, in.Name)
		return PolicyView{}, err
	}

	// 4. AMAPI 反映（既存 amapi_policy_name の末尾 policyId を再利用 / Req 1.2）。
	//    再試行不可エラーは snapshot を確定保存せずそのまま伝達（Req 1.4 / 1.5）。
	amapiPolicyID := policyIDFromAMAPIName(existing.AMAPIPolicyName, policyID)
	body := BuildPolicyBody(existing.AMAPIPolicyName, in.Body)
	if err := s.amapi.UpsertPolicy(ctx, enterpriseName, amapiPolicyID, body); err != nil {
		s.logDeny(actor, tenantID, policyID, "amapi upsert failed")
		s.record(ctx, actor, tenantID, policyID, eventTypePolicyUpdate, audit.ResultFailure, in.Name)
		return PolicyView{}, err
	}

	// 5. AMAPI 反映成功後に snapshot を更新する（Req 1.2 / 1.5）。
	//    version は既存行の version を据え置く（AMAPI が version を返さない MVP 制約 / 確認事項参照）。
	row := PolicyRow{
		ID:              policyID,
		TenantID:        tenantID,
		Name:            in.Name,
		AMAPIPolicyName: existing.AMAPIPolicyName,
		Body:            in.Body,
		Version:         existing.Version,
		UpdatedBy:       &actor,
	}
	affected, err := s.repo.Update(ctx, row)
	if err != nil {
		s.record(ctx, actor, tenantID, policyID, eventTypePolicyUpdate, audit.ResultFailure, in.Name)
		return PolicyView{}, err
	}
	if affected == 0 {
		// 反映後に他要求が先に削除した競合等。存在差を露出しない NotFound を返す（Req 4.4 / 4.5）。
		s.logDeny(actor, tenantID, policyID, "policy update affected no rows")
		s.record(ctx, actor, tenantID, policyID, eventTypePolicyUpdate, audit.ResultFailure, in.Name)
		return PolicyView{}, ErrPolicyNotFound
	}

	// 6. 更新成功を監査記録する（Req 5.2）。
	s.record(ctx, actor, tenantID, policyID, eventTypePolicyUpdate, audit.ResultSuccess, in.Name)

	return PolicyView{
		ID:      policyID,
		Name:    in.Name,
		Body:    in.Body,
		Version: row.Version,
	}, nil
}

// List は Service.List の実装。
func (s *service) List(ctx context.Context, tenantID uuid.UUID) ([]PolicySummary, error) {
	// tenant-scoped Repository へ委譲（RLS で自テナント行のみ / Req 4.4）。read のため監査なし。
	rows, err := s.repo.List(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	summaries := make([]PolicySummary, 0, len(rows))
	for _, row := range rows {
		summaries = append(summaries, rowToSummary(row))
	}
	return summaries, nil
}

// Get は Service.Get の実装。
func (s *service) Get(ctx context.Context, tenantID, policyID uuid.UUID) (PolicyView, error) {
	// tenant-scoped Repository へ委譲。不在 / 他テナント越境は Repository が ErrPolicyNotFound
	// （存在差非露出 / 404）に写像済みのため、そのまま伝達する（Req 4.4 / 4.5）。read のため監査なし。
	row, err := s.repo.Get(ctx, tenantID, policyID)
	if err != nil {
		return PolicyView{}, err
	}
	return rowToView(row), nil
}

// Delete は Service.Delete の実装。
func (s *service) Delete(ctx context.Context, actor, tenantID, policyID uuid.UUID) error {
	// Repository.Delete へ委譲する。割当済み端末ありの policy は Repository が ErrDeleteConflict
	// （409）を Code 付きで返すため、そのまま伝達する（design 確認事項 3 / Req 5.3）。
	affected, err := s.repo.Delete(ctx, tenantID, policyID)
	if err != nil {
		s.logDeny(actor, tenantID, policyID, "policy delete failed")
		s.record(ctx, actor, tenantID, policyID, eventTypePolicyDelete, audit.ResultFailure, "")
		return err
	}
	if affected == 0 {
		// 不在 / 他テナント越境（RLS / WHERE tenant_id で 0 行）→ 存在差非露出の NotFound（Req 4.4 / 4.5）。
		s.logDeny(actor, tenantID, policyID, "policy delete affected no rows")
		s.record(ctx, actor, tenantID, policyID, eventTypePolicyDelete, audit.ResultFailure, "")
		return ErrPolicyNotFound
	}

	// 削除成功を監査記録する（Req 5.3）。Detail は安全 field のみ（機密値を載せない / Req 5.4）。
	s.record(ctx, actor, tenantID, policyID, eventTypePolicyDelete, audit.ResultSuccess, "")
	return nil
}

// Assign は Service.Assign の実装。
func (s *service) Assign(ctx context.Context, actor, tenantID, deviceID, policyID uuid.UUID) error {
	// Repository.AssignPolicyToDevice へ委譲し devices.applied_policy_id を UPDATE する。
	// 割当は DB 更新までで AMAPI device patch は行わない（design 確認事項 1 推奨案）。
	affected, err := s.repo.AssignPolicyToDevice(ctx, tenantID, deviceID, policyID)
	if err != nil {
		// 他テナント policy 指定（複合 FK 違反）は Repository が ErrPolicyNotFound（404）へ写像済み
		// のため、そのまま伝達する（Req 3.2 / 4.2 / 4.5）。
		s.logDeny(actor, tenantID, policyID, "policy assign failed")
		s.recordAssign(ctx, actor, tenantID, deviceID, policyID, audit.ResultFailure)
		return err
	}
	if affected == 0 {
		// 他テナント device 指定（affected=0 / err==nil）→ 存在差非露出の NotFound（Req 3.3 / 4.3）。
		s.logDeny(actor, tenantID, policyID, "policy assign affected no rows")
		s.recordAssign(ctx, actor, tenantID, deviceID, policyID, audit.ResultFailure)
		return ErrPolicyNotFound
	}

	// 割当成功を監査記録する。Detail は安全 field（policy_id / device_id / result）のみ（Req 5.4）。
	s.recordAssign(ctx, actor, tenantID, deviceID, policyID, audit.ResultSuccess)
	return nil
}

// rowToView は PolicyRow を API 詳細レスポンス（PolicyView）へ変換する（Get / Create / Update 共通の
// 写像。CreatedAt / UpdatedAt / Version を row から充填する）。
func rowToView(row PolicyRow) PolicyView {
	return PolicyView{
		ID:        row.ID,
		Name:      row.Name,
		Body:      row.Body,
		Version:   row.Version,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}
}

// rowToSummary は PolicyRow を一覧レスポンス（PolicySummary）へ変換する。本体 JSON snapshot
// （Body）は載せず、識別に必要な最小 field のみを返す（Req 5.4 / NFR 3.2 / List 経路）。
func rowToSummary(row PolicyRow) PolicySummary {
	return PolicySummary{
		ID:        row.ID,
		Name:      row.Name,
		Version:   row.Version,
		UpdatedAt: row.UpdatedAt,
	}
}

// validate は raw body を mapper で PolicyInput へ変換し、変換不能（型不整合）と Validator の
// 検証不正を結合して全件提示する（Req 2.1 / 2.2 / 2.3 / 2.4 / 2.5）。
//
// 検証エラーが 0 件のときは nil を返す（AMAPI / Repository へ進んでよい）。1 件以上のときは
// 全件を保持した *ValidationFailedError を返す。raw body の生値はエラーへ載せない（Req 5.4）。
func (s *service) validate(rawBody map[string]any) *ValidationFailedError {
	in, convErrs := RawToPolicyInput(rawBody)
	result := Validate(in)

	// mapper 変換不能（KindInvalidField）と Validator 検証不正を結合して全件提示する（Req 2.4）。
	combined := make([]ValidationError, 0, len(convErrs)+len(result.Errors))
	combined = append(combined, convErrs...)
	combined = append(combined, result.Errors...)

	if len(combined) == 0 {
		return nil
	}
	return newValidationFailedError(combined)
}

// record は監査イベントを eventRecorder へ渡す helper（Req 5.1 / 5.2）。
//
// Detail には安全 field のみ（policy_id / name / result）を載せ、raw body の機密値を載せない
// （Req 5.4 / NFR 3.2）。Record の失敗は WARN ログに留め、ユースケース本体の業務結果を覆さない
// （tenant.record と同方針 / 成否いずれの経路でも Record を呼ぶ Req 5.1 / 5.2）。
func (s *service) record(ctx context.Context, actor, tenantID, policyID uuid.UUID, eventType audit.EventType, result audit.ResultType, name string) {
	detail := map[string]any{
		"result": string(result),
	}
	// name を持たない操作（Delete / Assign）では name field を省略する（冗長な空 name を載せない）。
	if name != "" {
		detail["name"] = name
	}
	resourceID := ""
	if policyID != uuid.Nil {
		resourceID = policyID.String()
		detail["policy_id"] = resourceID
	}
	s.emitAudit(ctx, actor, tenantID, eventType, resourceID, result, detail)
}

// recordAssign は割当イベントの監査記録を行う（Req 5.x）。policy_assign は ResourceID=policy_id とし、
// Detail に割当先 device_id も安全 field として載せる（design Data Models）。raw body の機密値は
// 一切載せない（Req 5.4 / NFR 3.2）。
func (s *service) recordAssign(ctx context.Context, actor, tenantID, deviceID, policyID uuid.UUID, result audit.ResultType) {
	detail := map[string]any{
		"result": string(result),
	}
	resourceID := ""
	if policyID != uuid.Nil {
		resourceID = policyID.String()
		detail["policy_id"] = resourceID
	}
	if deviceID != uuid.Nil {
		detail["device_id"] = deviceID.String()
	}
	s.emitAudit(ctx, actor, tenantID, eventTypePolicyAssign, resourceID, result, detail)
}

// emitAudit は組み立て済みの監査 Detail から audit.Event を構築して eventRecorder へ渡す共通 helper。
// Record の失敗は WARN ログに留め、ユースケース本体の業務結果を覆さない（tenant.record と同方針 /
// 成否いずれの経路でも Record を呼ぶ Req 5.1 / 5.2 / 5.3）。
func (s *service) emitAudit(ctx context.Context, actor, tenantID uuid.UUID, eventType audit.EventType, resourceID string, result audit.ResultType, detail map[string]any) {
	ev := audit.Event{
		TenantID:   tenantID,
		ActorID:    actor,
		EventType:  eventType,
		ResourceID: resourceID,
		Detail:     detail,
		Result:     result,
	}
	if err := s.recorder.Record(ctx, ev); err != nil {
		s.log.Warn("policy audit record failed",
			"event_type", string(eventType),
			logger.ActorID(actor),
			logger.TenantID(tenantID),
		)
	}
}

// logDeny は拒否された操作の原因分析属性（実行者・対象テナント・対象 policy・拒否理由）を
// 構造化ログとして出力する（NFR 3.1）。reason は人間可読な短い拒否理由のみで、機密値
// （raw body の生値）は含めない（Req 5.4 / NFR 3.2）。
//
// policyID が uuid.Nil（採番前の Create 検証失敗等）の場合は policy_id field を省略する。
func (s *service) logDeny(actor, tenantID, policyID uuid.UUID, reason string) {
	fields := []any{
		logger.ActorID(actor),
		logger.TenantID(tenantID),
		"deny_reason", reason,
	}
	if policyID != uuid.Nil {
		fields = append(fields, "policy_id", policyID.String())
	}
	s.log.Warn("policy operation denied", fields...)
}

// buildAMAPIPolicyName は enterpriseName と policyId から AMAPI policyName
// （"enterprises/{eid}/policies/{policyId}" 形式）を組み立てる（design Data Models）。
func buildAMAPIPolicyName(enterpriseName, policyID string) string {
	return enterpriseName + "/policies/" + policyID
}

// policyIDFromAMAPIName は AMAPI policyName 末尾の短い policyId を取り出す。
//
// `UpsertPolicy` には enterpriseName を二重連結しない短い policyId を渡すため、既存行の
// amapi_policy_name から末尾セグメントを取り出す。"/policies/" を含まない異常データの場合は
// fallback として policyID（DB id）の文字列を返す（安全側 / データ不整合でも反映を試みる）。
func policyIDFromAMAPIName(amapiPolicyName string, policyID uuid.UUID) string {
	const marker = "/policies/"
	if idx := strings.LastIndex(amapiPolicyName, marker); idx >= 0 {
		short := amapiPolicyName[idx+len(marker):]
		if short != "" {
			return short
		}
	}
	return policyID.String()
}

// 型 assertion 用に service が Service interface（最終形 6 メソッド: Create / Update / List / Get /
// Delete / Assign）を満たすことを compile-time で確認する。
var _ Service = (*service)(nil)
