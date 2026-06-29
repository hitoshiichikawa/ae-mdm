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

// upsertClient は Service が AMAPI 反映と反映済み version 取得に用いる最小ポート
// （consumer-defines-interface）。
//
// amapi.Client の全 IF ではなく本 task が使う UpsertPolicy / GetPolicy のみに限定し、テストで
// fake を差し込みやすくする（design「Service deps」節）。GetPolicy は upsert 後に AMAPI 反映済み
// snapshot version を読み戻すために用いる（PolicyRow.Version の契約 = AMAPI 反映済み version /
// Req 1.3 / 1.5）。
type upsertClient interface {
	// UpsertPolicy は Policy を AMAPI へ upsert する。policyName には短い policyId を渡す
	// （AMAPI Client が enterpriseName + "/policies/" + policyName で resourceName を組み立てる）。
	UpsertPolicy(ctx context.Context, enterpriseName, policyName string, body amapi.PolicyBody) error

	// GetPolicy は AMAPI 反映済み Policy を取得する。upsert 直後の反映済み version 読み戻しに用いる。
	GetPolicy(ctx context.Context, enterpriseName, policyName string) (amapi.PolicyBody, error)
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
	// 1. 検証ゲート（name 必須 + mapper 変換 + Validate 結合 / 全件提示 / 早期 return）。
	//    検証失敗時は AMAPI / Repository を一切呼ばない（Req 2.5）。
	if verr := s.validate(in); verr != nil {
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

	// 4b. AMAPI 反映済み snapshot version を読み戻す（PolicyRow.Version の契約 = AMAPI 反映済み
	//     version / Req 1.3 / 1.5）。新規 policyId のため並行 writer は存在せず、自身の upsert の
	//     version が確実に読める。read-back 失敗時は AMAPI 反映自体は成功しているため処理を継続し、
	//     fallback version 0 で永続化する（reflectedVersion 内で WARN ログ）。
	version := s.reflectedVersion(ctx, enterpriseName, policyID, 0)

	// 5. AMAPI 反映成功後に snapshot を永続化する（Req 1.3 / 1.5）。送信した raw body を pass-through。
	row := PolicyRow{
		ID:              id,
		TenantID:        tenantID,
		Name:            in.Name,
		AMAPIPolicyName: amapiPolicyName,
		Body:            in.Body,
		Version:         version,
		UpdatedBy:       &actor,
	}
	persisted, err := s.repo.Insert(ctx, row)
	if err != nil {
		// AMAPI には反映済みだが DB snapshot 永続化に失敗 = AMAPI と DB の乖離（orphan AMAPI
		// policy）。運用者が reconcile できるよう構造化 ERROR ログを出す（NFR 3.1 / design 確認事項:
		// MVP は AMAPI-first 順序で補償トランザクションを持たない / Req 1.3）。
		s.logInconsistency(actor, tenantID, id, amapiPolicyName, "amapi upsert succeeded but snapshot insert failed")
		s.record(ctx, actor, tenantID, id, eventTypePolicyCreate, audit.ResultFailure, in.Name)
		return PolicyView{}, err
	}

	// 6. 作成成功を監査記録する（Req 5.1）。Detail は安全 field のみ（raw body を載せない）。
	s.record(ctx, actor, tenantID, id, eventTypePolicyCreate, audit.ResultSuccess, in.Name)

	// DB 採番の timestamps を充填した persisted から PolicyView を組み立てる（CreatedAt /
	// UpdatedAt を zero time にしない / API 契約と整合）。
	return rowToView(persisted), nil
}

// Update は Service.Update の実装。
func (s *service) Update(ctx context.Context, actor, tenantID, policyID uuid.UUID, in PolicyRequest) (PolicyView, error) {
	// 1. 検証ゲート（Create と同一 / 早期 return / Req 2.5）。
	if verr := s.validate(in); verr != nil {
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

	// 4+5. AMAPI 反映と snapshot 更新を per-policy advisory lock 保持下の単一 critical section で
	//      直列化する（同一 policy への並行更新で AMAPI 反映順と DB 書込順が逆転しない / Req 1.5）。
	//      reflect は lock 保持中に AMAPI へ upsert し、反映済み version を解決して返す。
	//      version は AMAPI 反映済み snapshot version を読み戻して充填する（既存 version を据え置か
	//      ない / Req 1.2 / 1.5 / 出力契約）。read-back 失敗時は AMAPI 反映自体は成功しているため
	//      既存 version を fallback にして処理を継続する（reflectedVersion 内で WARN ログ）。
	amapiPolicyID := policyIDFromAMAPIName(existing.AMAPIPolicyName, policyID)
	body := BuildPolicyBody(existing.AMAPIPolicyName, in.Body)
	row := PolicyRow{
		ID:              policyID,
		TenantID:        tenantID,
		Name:            in.Name,
		AMAPIPolicyName: existing.AMAPIPolicyName,
		Body:            in.Body,
		UpdatedBy:       &actor,
		// Version は reflect が反映済み version を解決して充填する。
	}
	amapiReflected := false
	reflect := func(rctx context.Context) (int64, error) {
		if uerr := s.amapi.UpsertPolicy(rctx, enterpriseName, amapiPolicyID, body); uerr != nil {
			// 再試行不可エラーは snapshot を確定保存せずそのまま伝達する（Req 1.4 / 1.5）。
			return 0, uerr
		}
		amapiReflected = true
		return s.reflectedVersion(rctx, enterpriseName, amapiPolicyID, existing.Version), nil
	}
	persisted, affected, err := s.repo.UpdateSnapshotSerialized(ctx, row, reflect)
	if err != nil {
		if amapiReflected {
			// AMAPI には反映済みだが DB snapshot 更新に失敗 = AMAPI と DB の乖離。運用者が reconcile
			// できるよう構造化 ERROR ログを出す（NFR 3.1 / design 確認事項: MVP は補償なし / Req 1.5）。
			s.logInconsistency(actor, tenantID, policyID, existing.AMAPIPolicyName, "amapi upsert succeeded but snapshot update failed")
		} else {
			// AMAPI 反映自体が失敗（snapshot 未確定 / Req 1.4）。拒否理由を構造化ログに残す（NFR 3.1）。
			s.logDeny(actor, tenantID, policyID, "amapi upsert failed")
		}
		s.record(ctx, actor, tenantID, policyID, eventTypePolicyUpdate, audit.ResultFailure, in.Name)
		return PolicyView{}, err
	}
	if affected == 0 {
		// 対象行が不在（事前 Get 後の削除競合 / 他テナント越境）。Repository は reflect の前に
		// FOR UPDATE で存在を再確認し、不在なら AMAPI を呼ばずに affected=0 を返すため、通常この
		// 経路では AMAPI と DB の乖離は生じない（amapiReflected=false）。存在差を露出しない NotFound を
		// 返す（Req 4.4 / 4.5）。
		if amapiReflected {
			// 想定外（FOR UPDATE の行ロックを越えて反映後に行が消失）の防御。AMAPI だけ更新済みの
			// 乖離を構造化 ERROR ログに残し運用者が reconcile できるようにする（NFR 3.1）。
			s.logInconsistency(actor, tenantID, policyID, existing.AMAPIPolicyName, "amapi upsert succeeded but target row missing on update")
		}
		s.logDeny(actor, tenantID, policyID, "policy update affected no rows")
		s.record(ctx, actor, tenantID, policyID, eventTypePolicyUpdate, audit.ResultFailure, in.Name)
		return PolicyView{}, ErrPolicyNotFound
	}

	// 6. 更新成功を監査記録する（Req 5.2）。
	s.record(ctx, actor, tenantID, policyID, eventTypePolicyUpdate, audit.ResultSuccess, in.Name)

	// DB 更新後の timestamps を充填した persisted から PolicyView を組み立てる（UpdatedAt を
	// zero time にしない / API 契約と整合）。
	return rowToView(persisted), nil
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
		// のため、そのまま伝達する（Req 3.2 / 4.2 / 4.5）。拒否対象 device_id も構造化ログに残す（NFR 3.1）。
		s.logDenyAssign(actor, tenantID, deviceID, policyID, "policy assign failed")
		s.recordAssign(ctx, actor, tenantID, deviceID, policyID, audit.ResultFailure)
		return err
	}
	if affected == 0 {
		// 他テナント device 指定（affected=0 / err==nil）→ 存在差非露出の NotFound（Req 3.3 / 4.3）。
		// 拒否対象 device_id も構造化ログに残す（NFR 3.1）。
		s.logDenyAssign(actor, tenantID, deviceID, policyID, "policy assign affected no rows")
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

// validate は入力 DTO（name 必須 + raw body）を検証する（Req 2.1 / 2.2 / 2.3 / 2.4 / 2.5）。
//
// 検証は (1) top-level `name` の空文字拒否（service_types.go の入力契約「空は Service / Handler が
// 弾く」を満たす / Req 2.3）、(2) mapper による raw body → PolicyInput 変換不能（型不整合）、
// (3) Validator の検証不正、の 3 種を結合して全件提示する（Req 2.4）。
//
// 検証エラーが 0 件のときは nil を返す（AMAPI / Repository へ進んでよい）。1 件以上のときは
// 全件を保持した *ValidationFailedError を返す。raw body の生値はエラーへ載せない（Req 5.4）。
func (s *service) validate(in PolicyRequest) *ValidationFailedError {
	var combined []ValidationError

	// (1) name 必須（空 / 空白のみは invalid field として拒否する / Req 2.3）。
	if strings.TrimSpace(in.Name) == "" {
		combined = append(combined, ValidationError{
			Domain:  domainPolicyMetadata,
			Field:   "name",
			Kind:    KindInvalidField,
			Message: "ポリシー名は必須です",
		})
	}

	// (2)+(3) mapper 変換不能（KindInvalidField）と Validator 検証不正を結合して全件提示する（Req 2.4）。
	pin, convErrs := RawToPolicyInput(in.Body)
	result := Validate(pin)
	combined = append(combined, convErrs...)
	combined = append(combined, result.Errors...)

	if len(combined) == 0 {
		return nil
	}
	return newValidationFailedError(dedupeFieldErrors(combined))
}

// dedupeFieldErrors は同一 (Domain, Field) に対する重複 ValidationError を 1 件へ畳む（出現順は保持）。
//
// mapper の変換不能エラー（convErrs）は Validator の検証不正より先に combined へ append されるため、
// 同一フィールドで「型不整合（root cause）」と「必須欠落（型不整合の下流症状）」が重なった場合は
// 先頭の mapper エラーを残し、Validator 側の重複を捨てる。Req 2.4 の「全件提示」は distinct な
// (Domain, Field) 単位で維持され、同一フィールドへの重複ノイズのみを排除する。
func dedupeFieldErrors(errs []ValidationError) []ValidationError {
	type fieldKey struct {
		domain Domain
		field  string
	}
	seen := make(map[fieldKey]struct{}, len(errs))
	deduped := make([]ValidationError, 0, len(errs))
	for _, e := range errs {
		key := fieldKey{domain: e.Domain, field: e.Field}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		deduped = append(deduped, e)
	}
	return deduped
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

// logDenyAssign は割当拒否の構造化ログを出力する（NFR 3.1）。割当操作の target resource は policy と
// device の双方であるため、logDeny の属性に加えて device_id も載せ、拒否対象（他テナント device /
// 他テナント policy）を事後追跡できるようにする。機密値は含めない（Req 5.4 / NFR 3.2）。
func (s *service) logDenyAssign(actor, tenantID, deviceID, policyID uuid.UUID, reason string) {
	fields := []any{
		logger.ActorID(actor),
		logger.TenantID(tenantID),
		"deny_reason", reason,
	}
	if policyID != uuid.Nil {
		fields = append(fields, "policy_id", policyID.String())
	}
	if deviceID != uuid.Nil {
		fields = append(fields, "device_id", deviceID.String())
	}
	s.log.Warn("policy operation denied", fields...)
}

// logInconsistency は AMAPI 反映成功後に DB snapshot 永続化が失敗した（AMAPI と DB snapshot が
// 乖離した）ことを構造化 ERROR ログに出す（NFR 3.1）。
//
// MVP の upsert は design 確認事項どおり AMAPI-first 順序で補償トランザクションを持たないため、
// この乖離は運用者が手動で reconcile（orphan AMAPI policy の削除 / 再同期）する必要がある。
// reconcile に必要な属性（実行者・テナント・policy_id・amapi_policy_name）と
// `requires_reconciliation=true` を載せる。amapi_policy_name は resource path であり機密値では
// ないため reconcile 用に載せる（raw body / 資格情報 / トークン生値は載せない / Req 5.4 / NFR 3.2）。
func (s *service) logInconsistency(actor, tenantID, policyID uuid.UUID, amapiPolicyName, reason string) {
	s.log.Error("policy amapi/db inconsistency",
		logger.ActorID(actor),
		logger.TenantID(tenantID),
		"policy_id", policyID.String(),
		"amapi_policy_name", amapiPolicyName,
		"inconsistency_reason", reason,
		"requires_reconciliation", true,
	)
}

// reflectedVersion は AMAPI 反映済み policy の version を GetPolicy で読み戻す（Req 1.3 / 1.5）。
//
// PolicyRow.Version の契約は「AMAPI 反映済み snapshot version」であり、upsert 直後に GetPolicy で
// 反映済み version を取得して充填する。GetPolicy が失敗した場合は **AMAPI 反映自体は成功している**
// ため処理を中断せず、fallback version を用いて snapshot 本体（body）の一致を優先する（Req 1.5 の
// body 一致を version metadata の正確性より優先 / 構造化 WARN ログで version 劣化を可視化する）。
// 機密値（資格情報・トークン生値）はログに含めない（Req 5.4 / NFR 3.2）。
func (s *service) reflectedVersion(ctx context.Context, enterpriseName, amapiPolicyID string, fallback int64) int64 {
	reflected, err := s.amapi.GetPolicy(ctx, enterpriseName, amapiPolicyID)
	if err != nil {
		s.log.Warn("policy reflected version read-back failed; using fallback version",
			"amapi_policy_name", buildAMAPIPolicyName(enterpriseName, amapiPolicyID),
			"fallback_version", fallback,
		)
		return fallback
	}
	return reflected.Version
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
