package enrollment

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/hitoshiichikawa/ae-mdm/internal/audit"
	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/amapi"
)

// allowPersonalUsageDisallowed は AMAPI allowPersonalUsage の固定値。FULLY_MANAGED / DEDICATED の
// 双方でこの値を固定し、個人利用不可のエンロールメントトークンを発行する（Req 1.1 / 1.2）。
const allowPersonalUsageDisallowed = "PERSONAL_USAGE_DISALLOWED"

// eventTypeEnrollmentTokenIssue はトークン発行の監査イベント種別（Req 5.1 / tasks.md task 2 指定）。
const eventTypeEnrollmentTokenIssue audit.EventType = "enrollment_token_issue"

// Clock は現在時刻を提供する DI 境界（ListTokens の status 派生をテストで決定的にするため）。
//
// 専用 clock パッケージは持たず、各 domain が最小 Clock を持つ既存作法に倣う（手本: audit.Clock）。
// ListTokens は DeriveStatus(row.ExpiresAt, clock.Now()) の now を本 Clock から取得する。
type Clock interface {
	Now() time.Time
}

// SystemClock は time.Now() を呼び出す本番用の Clock 実装（値型 / ゼロ値で利用可）。
type SystemClock struct{}

// Now は time.Now() の戻り値をそのまま返す。
func (SystemClock) Now() time.Time { return time.Now() }

// enrollmentClient は Service が AMAPI トークン発行に用いる最小ポート（consumer-defines-interface）。
//
// amapi.Client の全 IF ではなく本 task が使う CreateEnrollmentToken のみに限定し、テストで
// amapi.StubClient を差し込みやすくする（policy.upsertClient を手本）。amapi.Client / amapi.StubClient の
// いずれも本ポートを structural typing で満たす。
type enrollmentClient interface {
	CreateEnrollmentToken(ctx context.Context, enterpriseName string, req amapi.EnrollmentTokenRequest) (amapi.EnrollmentToken, error)
}

// Service はトークン発行ユースケース（IssueToken）と一覧参照（ListTokens）の単一所有者
// （design.md「enrollment.Service」節 / Req 1.x / 4.1 / 5.x / NFR 3.1）。
type Service interface {
	// IssueToken はモード別トークンを発行し、QR 表示用データ（Value / QRCode）を含む TokenView を返す。
	//
	//   - validateMode（不正/未指定 → ErrInvalidMode で AMAPI 非呼出 / Req 1.4）。
	//   - DEDICATED は policyChecker.ResolveOwnedPolicy で自テナント policy を検証し、AMAPI policy id を
	//     PolicyName に設定する（未指定 → ErrPolicyRequired / 不在 → NotFound 伝達 / いずれも AMAPI 非呼出 / Req 1.5）。
	//   - enterpriseResolver.EnterpriseNameForTenant（bound のみ成功 / 越境は fail-closed / Req 2.3 前提）。
	//   - AllowPersonalUsage=PERSONAL_USAGE_DISALLOWED 固定で AMAPI へ発行する（Req 1.1 / 1.2）。
	//   - AMAPI エラーは snapshot を永続化せず伝達する（Req 1.6）。永続化失敗は inconsistency ERROR ログ
	//     （AMAPI-first / policy.Service 踏襲 / MVP は補償なし）。
	//   - 成否いずれの経路でも発行イベントを監査記録する（Req 5.1）。Detail は安全 field のみで秘密値を
	//     載せない（Req 5.2 / NFR 3.1）。秘密値（Value / QRCode）は TokenView（HTTP 応答）で一度だけ返す。
	//
	// actor は監査イベントの実行者識別子（admin_users.id）。tenantID は claims 由来の所有テナント。
	IssueToken(ctx context.Context, actor, tenantID uuid.UUID, in IssueRequest) (TokenView, error)

	// ListTokens は自テナントの発行済みトークン snapshot を expires_at 由来 status 付きで返す（Req 4.1）。
	//
	// 秘密値（Value / QRCode）は snapshot 列に無いため一覧に載らない（NFR 3.1）。read 操作のため監査記録は行わない。
	ListTokens(ctx context.Context, tenantID uuid.UUID) ([]TokenSummary, error)
}

// service は Service interface の本番実装。
//
// deps は design.md「enrollment.Service」節に対応する最小依存（すべて consumer-defines-interface）:
//   - repo:     TokenRepository（snapshot 永続化 / 一覧参照）
//   - client:   enrollmentClient（AMAPI 発行の最小ポート / amapi.Client が満たす）
//   - recorder: eventRecorder（監査記録の最小ポート / audit.Service が満たす）
//   - tenants:  enterpriseResolver（enterprise_name 解決 / tenant.Service が満たす）
//   - policies: policyChecker（DEDICATED policy 検証 / cmd/api アダプタが policy.Service を包んで満たす）
//   - clock:    Clock（ListTokens の status 派生の now 供給）
//   - log:      logger.Logger（拒否 / 不整合経路の構造化ログ / NFR 3.1）
//
// authorizer は持たない（authz は Handler = task 3 の責務 / design Components）。
type service struct {
	repo     TokenRepository
	client   enrollmentClient
	recorder eventRecorder
	tenants  enterpriseResolver
	policies policyChecker
	clock    Clock
	log      logger.Logger
}

// NewService は本番用 Service を構築する（policy.NewService と同方式の deps 注入）。
//
// log が nil の場合は logger.Default()（未配線時は no-op）、clock が nil の場合は SystemClock{} を
// 採用し、DI 未配線でも構造化ログ・時刻取得で panic させない（amapi.NewClient / policy.NewService の
// nil フォールバックと同方針）。
func NewService(
	repo TokenRepository,
	client enrollmentClient,
	recorder eventRecorder,
	tenants enterpriseResolver,
	policies policyChecker,
	clock Clock,
	log logger.Logger,
) Service {
	if log == nil {
		log = logger.Default()
	}
	if clock == nil {
		clock = SystemClock{}
	}
	return &service{
		repo:     repo,
		client:   client,
		recorder: recorder,
		tenants:  tenants,
		policies: policies,
		clock:    clock,
		log:      log,
	}
}

// IssueToken は Service.IssueToken の実装。
func (s *service) IssueToken(ctx context.Context, actor, tenantID uuid.UUID, in IssueRequest) (TokenView, error) {
	// 1. モード検証（不正/未指定 → ErrInvalidMode / AMAPI 非呼出 / Req 1.4）。
	if !in.Mode.Valid() {
		s.logDeny(actor, tenantID, "invalid enrollment mode")
		s.recordIssue(ctx, actor, tenantID, uuid.Nil, in.Mode, time.Time{}, audit.ResultFailure)
		return TokenView{}, ErrInvalidMode
	}

	// 2. DEDICATED は自テナント Kiosk policy を検証（未指定 → ErrPolicyRequired / 不在 → NotFound 伝達 /
	//    いずれも AMAPI 非呼出 / Req 1.5）。amapiPolicyName は FULLY_MANAGED では空（PolicyName 未設定）。
	var policyID *uuid.UUID
	amapiPolicyName := ""
	if in.Mode == ModeDedicated {
		if in.PolicyID == nil || *in.PolicyID == uuid.Nil {
			s.logDeny(actor, tenantID, "kiosk policy is required for dedicated mode")
			s.recordIssue(ctx, actor, tenantID, uuid.Nil, in.Mode, time.Time{}, audit.ResultFailure)
			return TokenView{}, ErrPolicyRequired
		}
		resolved, err := s.policies.ResolveOwnedPolicy(ctx, tenantID, *in.PolicyID)
		if err != nil {
			// 不在 / 越境は policyChecker が存在差非露出の NotFound を返す。そのまま伝達する（Req 1.5 / 2.3）。
			s.logDeny(actor, tenantID, "kiosk policy resolution failed")
			s.recordIssue(ctx, actor, tenantID, uuid.Nil, in.Mode, time.Time{}, audit.ResultFailure)
			return TokenView{}, err
		}
		amapiPolicyName = resolved
		policyID = in.PolicyID
	}

	// 3. enterprise_name 解決（bound のみ成功 / 越境は fail-closed NotFound / Req 2.3 前提）。
	enterpriseName, err := s.tenants.EnterpriseNameForTenant(ctx, tenantID)
	if err != nil {
		s.logDeny(actor, tenantID, "enterprise name resolution failed")
		s.recordIssue(ctx, actor, tenantID, uuid.Nil, in.Mode, time.Time{}, audit.ResultFailure)
		return TokenView{}, err
	}

	// 4. additionalData 組立（tenant_id / issued_by / mode / Req 1.3）。秘密値は含めない（NFR 3.1）。
	additionalData, err := AdditionalData{TenantID: tenantID, IssuedBy: actor, Mode: in.Mode}.Marshal()
	if err != nil {
		s.logDeny(actor, tenantID, "additional data marshal failed")
		s.recordIssue(ctx, actor, tenantID, uuid.Nil, in.Mode, time.Time{}, audit.ResultFailure)
		return TokenView{}, err
	}

	// 5. AMAPI 発行（AllowPersonalUsage=DISALLOWED 固定 / DEDICATED は PolicyName 設定 / Req 1.1 / 1.2）。
	tok, err := s.client.CreateEnrollmentToken(ctx, enterpriseName, amapi.EnrollmentTokenRequest{
		PolicyName:         amapiPolicyName,
		Duration:           in.Duration,
		AdditionalData:     additionalData,
		AllowPersonalUsage: allowPersonalUsageDisallowed,
	})
	if err != nil {
		// 再試行不可含む AMAPI エラーは snapshot を永続化せず伝達する（Req 1.6）+ 失敗監査（Req 5.1）。
		s.logDeny(actor, tenantID, "amapi enrollment token creation failed")
		s.recordIssue(ctx, actor, tenantID, uuid.Nil, in.Mode, time.Time{}, audit.ResultFailure)
		return TokenView{}, err
	}

	// 6. token id 採番 + expiration parse（AMAPI は RFC3339 文字列で ExpirationTime を返す）。
	id := uuid.New()
	expiresAt, perr := time.Parse(time.RFC3339, tok.ExpirationTime)
	if perr != nil {
		// AMAPI 発行済みだが expiration が解釈不能 = snapshot を確定できない上流契約違反。orphan AMAPI
		// token として運用者が reconcile できるよう inconsistency ERROR ログ + 失敗監査 + upstream 伝達。
		s.logInconsistency(actor, tenantID, id, tok.Name, "amapi issued token but expiration time is unparseable")
		s.recordIssue(ctx, actor, tenantID, id, in.Mode, time.Time{}, audit.ResultFailure)
		return TokenView{}, pkgerrors.Wrap(pkgerrors.CodeUpstream, "amapi returned unparseable enrollment token expiration time", perr)
	}

	// 7. snapshot 永続化（AMAPI-first / Value・QRCode は列を持たず保存しない / NFR 3.1）。
	row := TokenRow{
		ID:             id,
		TenantID:       tenantID,
		AMAPITokenName: tok.Name,
		Mode:           in.Mode,
		PolicyID:       policyID,
		AdditionalData: additionalData,
		ExpiresAt:      expiresAt,
		IssuedBy:       actor,
	}
	if err := s.repo.Insert(ctx, row); err != nil {
		// AMAPI 反映済みだが snapshot 永続化に失敗 = AMAPI との乖離（orphan AMAPI token）。運用者が
		// reconcile できるよう inconsistency ERROR ログを出す（policy.Service 踏襲 / MVP は補償なし）。
		// 呼び出し側へは ErrTokenPersist(503) を伝達する（Req 1.6 の AMAPI-first 乖離）。
		s.logInconsistency(actor, tenantID, id, tok.Name, "amapi issued token but snapshot insert failed")
		s.recordIssue(ctx, actor, tenantID, id, in.Mode, expiresAt, audit.ResultFailure)
		return TokenView{}, ErrTokenPersist
	}

	// 8. 発行成功を監査記録する（発行者 / テナント / モード / 有効期限 / 結果 / Req 5.1）。秘密値非混入（Req 5.2）。
	s.recordIssue(ctx, actor, tenantID, id, in.Mode, expiresAt, audit.ResultSuccess)

	// 9. QR 表示用データ（Value / QRCode）を HTTP 応答で一度だけ返す（NFR 3.1 / Req 1.1 / 1.2）。
	return TokenView{
		ID:         id,
		Mode:       in.Mode,
		ExpiresAt:  expiresAt,
		Value:      tok.Value,
		QRCodeData: tok.QRCode,
	}, nil
}

// ListTokens は Service.ListTokens の実装。
func (s *service) ListTokens(ctx context.Context, tenantID uuid.UUID) ([]TokenSummary, error) {
	// tenant-scoped Repository へ委譲（RLS で自テナント行のみ / Req 4.1）。read のため監査なし。
	rows, err := s.repo.List(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	now := s.clock.Now()
	summaries := make([]TokenSummary, 0, len(rows))
	for _, row := range rows {
		summaries = append(summaries, TokenSummary{
			ID:        row.ID,
			Mode:      row.Mode,
			PolicyID:  row.PolicyID,
			ExpiresAt: row.ExpiresAt,
			Status:    DeriveStatus(row.ExpiresAt, now),
		})
	}
	return summaries, nil
}

// recordIssue はトークン発行イベントを eventRecorder へ渡す helper（Req 5.1 / 5.2）。
//
// Detail は安全 field（mode / expires_at / result）のみで、秘密値（Value / QRCode）を一切載せない
// （Req 5.2 / NFR 3.1）。expires_at が zero（発行前失敗）や mode が空（不正モード）の場合は当該 field を
// 省略する。Record の失敗は WARN ログに留めユースケース本体の業務結果を覆さない（policy.record と同方針 /
// 成否いずれの経路でも Record を呼ぶ Req 5.1）。
func (s *service) recordIssue(ctx context.Context, actor, tenantID, tokenID uuid.UUID, mode Mode, expiresAt time.Time, result audit.ResultType) {
	detail := map[string]any{
		"result": string(result),
	}
	if mode != "" {
		detail["mode"] = string(mode)
	}
	if !expiresAt.IsZero() {
		detail["expires_at"] = expiresAt.UTC().Format(time.RFC3339)
	}
	resourceID := ""
	if tokenID != uuid.Nil {
		resourceID = tokenID.String()
		detail["token_id"] = resourceID
	}
	ev := audit.Event{
		TenantID:   tenantID,
		ActorID:    actor,
		EventType:  eventTypeEnrollmentTokenIssue,
		ResourceID: resourceID,
		Detail:     detail,
		Result:     result,
	}
	if err := s.recorder.Record(ctx, ev); err != nil {
		s.log.Warn("enrollment audit record failed",
			"event_type", string(eventTypeEnrollmentTokenIssue),
			logger.ActorID(actor),
			logger.TenantID(tenantID),
		)
	}
}

// logDeny は発行拒否の原因分析属性（実行者・対象テナント・拒否理由）を構造化ログに出力する（NFR 3.1）。
// reason は人間可読な短い拒否理由のみで、秘密値（Value / QRCode）は含めない（Req 5.2 / NFR 3.1）。
func (s *service) logDeny(actor, tenantID uuid.UUID, reason string) {
	s.log.Warn("enrollment token issue denied",
		logger.ActorID(actor),
		logger.TenantID(tenantID),
		"deny_reason", reason,
	)
}

// logInconsistency は AMAPI 発行成功後に snapshot を確定できなかった（AMAPI と DB snapshot が乖離した）
// ことを構造化 ERROR ログに出力する（NFR 3.1）。
//
// MVP の発行は AMAPI-first 順序で補償トランザクションを持たない（policy.Service 踏襲）ため、この乖離は
// 運用者が手動で reconcile（orphan AMAPI token の削除等）する必要がある。reconcile に必要な属性
// （実行者・テナント・token_id・amapi_token_name）と requires_reconciliation=true を載せる。
// amapi_token_name は AMAPI resource 名であり秘密値ではないため reconcile 用に載せる。秘密値
// （Value / QRCode）は一切載せない（NFR 3.1）。
func (s *service) logInconsistency(actor, tenantID, tokenID uuid.UUID, amapiTokenName, reason string) {
	s.log.Error("enrollment amapi/db inconsistency",
		logger.ActorID(actor),
		logger.TenantID(tenantID),
		"token_id", tokenID.String(),
		"amapi_token_name", amapiTokenName,
		"inconsistency_reason", reason,
		"requires_reconciliation", true,
	)
}

// 型 assertion 用に service が Service interface（IssueToken / ListTokens）を満たすことを
// compile-time で確認する。
var _ Service = (*service)(nil)
