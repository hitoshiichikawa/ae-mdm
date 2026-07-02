package app

import (
	"context"
	"strings"

	"github.com/google/uuid"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/amapi"
)

// Service は App ドメインのユースケース（webToken 発行 / カタログ参照・同期 / 承認済み read seam）の
// 単一所有者（design.md「App Service」節 / internal/policy.Service に倣う）。
//
// **段階拡張 interface（本 task 2 では 2 メソッド）**: 本 task では webToken 発行（CreatePlayToken）と
// カタログ参照（ListApps）のみを定義する。カタログ同期（SyncApps / task 3）と承認済み read seam
// （CheckAppsApproved / task 4）は後続 task で本 interface へ追加する（policy が task 間で interface を
// 段階拡張したのと同方針。投機的に先取り定義しない）。
//
// authz は持たない: 認可（own-tenant RBAC）は Handler（task 5）の責務（design Components / policy と同方針）。
type Service interface {
	// CreatePlayToken は Managed Google Play 承認 UI を iframe 表示するための webToken を発行する
	// （Req 1.1 / 1.2 / 1.3 / NFR 1.1 / 2.1）。
	//
	//   - parent_frame_url が空 / 空白のみのときは webToken を発行せず CodeInvalidRequest（400）を
	//     返す。この場合 enterprise 解決も AMAPI 呼び出しも行わない（Req 1.2）。
	//   - enterpriseResolver.EnterpriseNameForTenant で自テナントの enterprise_name を解決する。
	//     未バインド / disabled 等は resolver が Code 付き error を返すため、そのまま伝達する（422 / Req 1.3）。
	//   - webTokenClient.CreateWebToken で発行し PlayTokenView{Value} を返す（Req 1.1）。AMAPI 由来
	//     error は #34 が正規化済み（CodeUpstream=502 等）でそのまま伝達する（NFR 2.1）。
	//
	// webToken.Value は秘匿値であり、成功・失敗いずれの経路でも構造化ログに出さない（NFR 1.1）。
	// actor は監査 / 拒否ログの実行者識別子、tenantID は claims 由来の所有テナント。
	CreatePlayToken(ctx context.Context, actor, tenantID uuid.UUID, in PlayTokenRequest) (PlayTokenView, error)

	// ListApps は自テナントの承認済みアプリカタログを返す（Req 2.1 / 2.2 / 2.3）。
	//
	//   - tenant-scoped Repository.List へ委譲し、RLS + tenant_id 述語で自テナント行のみを返す（Req 2.3）。
	//   - TenantAppRow を API 表現 TenantAppView（package_name / title / icon_url / approved_at）へ写像する。
	//   - 0 件のときは非 nil の空 slice を返す（Handler が `[]` をそのままシリアライズできる / Req 2.2）。
	//   - read 操作のため監査記録は行わない。
	ListApps(ctx context.Context, tenantID uuid.UUID) ([]TenantAppView, error)
}

// webTokenClient は Service が webToken 発行に用いる最小ポート（consumer-defines-interface / NFR 2.1）。
//
// amapi.Client の全 IF ではなく本 task が使う CreateWebToken のみに限定し、テストで fake を差し込み
// やすくする。amapi.Client が満たす。AMAPI 認証・再試行・エラー写像は #34 に閉じており再実装しない。
type webTokenClient interface {
	// CreateWebToken は enterprise 配下で iframe 埋め込み用の short-lived webToken を発行する。
	CreateWebToken(ctx context.Context, enterpriseName, parentFrameURL string) (amapi.WebToken, error)
}

// enterpriseResolver は tenantID から AMAPI enterprise_name を解決する最小ポート（Req 1.3）。
//
// tenant.Service.EnterpriseNameForTenant を満たす。bound テナントのみ enterprise_name + nil を返し、
// 未 bind / disabled / 不在 / 越境は Code 付き error を返す（tenant.Service 契約 / policy と同型の port）。
type enterpriseResolver interface {
	// EnterpriseNameForTenant は tenant-scoped context で enterprise_name を解決する。
	EnterpriseNameForTenant(ctx context.Context, id uuid.UUID) (string, error)
}

// service は Service interface の本番実装。
//
// deps は design.md「App Service」節の consumer-defines-interface に対応する最小依存:
//   - repo:    app.Repository（tenant_apps のカタログ参照 / task 1 で実装済み）
//   - amapi:   webTokenClient（webToken 発行の最小ポート / amapi.Client が満たす）
//   - tenants: enterpriseResolver（enterprise_name 解決 / bind gate / tenant.Service が満たす）
//   - log:     logger.Logger（拒否経路の構造化ログ / NFR 1.1 は Value を載せない）
//
// **eventRecorder は持たない**: 同期監査は SyncApps（task 3）で追加する責務であり本 task では導入しない
// （投機的に先取りしない）。**authorizer も持たない**: authz は Handler（task 5）の責務（design Components）。
type service struct {
	repo    Repository
	amapi   webTokenClient
	tenants enterpriseResolver
	log     logger.Logger
}

// NewService は本番用 Service を構築する（policy.NewService / tenant.NewService と同方式の deps 注入）。
//
// log が nil の場合は logger.Default()（未配線時は no-op）を採用し、DI 未配線でも構造化ログ呼び出しで
// panic させない（amapi.NewClient / policy.NewService の nil-log フォールバックと同方針）。
//
// 後続 task 3（SyncApps）で eventRecorder が deps に追加される（本 task では未導入 / 段階拡張）。
func NewService(
	repo Repository,
	client webTokenClient,
	tenants enterpriseResolver,
	log logger.Logger,
) Service {
	if log == nil {
		log = logger.Default()
	}
	return &service{
		repo:    repo,
		amapi:   client,
		tenants: tenants,
		log:     log,
	}
}

// CreatePlayToken は Service.CreatePlayToken の実装。
func (s *service) CreatePlayToken(ctx context.Context, actor, tenantID uuid.UUID, in PlayTokenRequest) (PlayTokenView, error) {
	// 1. parent_frame_url 空検査（Req 1.2）。空 / 空白のみは入力不正として 400 で早期 return し、
	//    enterprise 解決・AMAPI 呼び出しを一切行わない。
	if strings.TrimSpace(in.ParentFrameURL) == "" {
		s.logDeny(actor, tenantID, "parent_frame_url is required")
		return PlayTokenView{}, pkgerrors.New(pkgerrors.CodeInvalidRequest, "parent_frame_url is required")
	}

	// 2. enterprise_name 解決（tenant-scoped context / bound のみ成功 / Req 1.3）。
	//    未バインド / disabled 等は resolver が Code 付き error を返すため、そのまま伝達する（422）。
	enterpriseName, err := s.tenants.EnterpriseNameForTenant(ctx, tenantID)
	if err != nil {
		s.logDeny(actor, tenantID, "enterprise name resolution failed")
		return PlayTokenView{}, err
	}

	// 3. webToken 発行（Req 1.1 / NFR 2.1）。AMAPI 由来 error は #34 正規化済みをそのまま伝達する。
	token, err := s.amapi.CreateWebToken(ctx, enterpriseName, in.ParentFrameURL)
	if err != nil {
		s.logDeny(actor, tenantID, "amapi web token creation failed")
		return PlayTokenView{}, err
	}

	// token.Value は秘匿値（NFR 1.1）。ログ・監査に載せず PlayTokenView 経由でのみ呼び出し側へ返す。
	return PlayTokenView{Value: token.Value}, nil
}

// ListApps は Service.ListApps の実装。
func (s *service) ListApps(ctx context.Context, tenantID uuid.UUID) ([]TenantAppView, error) {
	// tenant-scoped Repository へ委譲（RLS + tenant_id 述語で自テナント行のみ / Req 2.1 / 2.3）。
	// read のため監査なし。DB 失敗（CodeUnavailable / 503）は Repository の写像のまま伝達する。
	rows, err := s.repo.List(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	// 0 件でも非 nil の空 slice を返す（Handler が `[]` をシリアライズできる / Req 2.2）。
	views := make([]TenantAppView, 0, len(rows))
	for _, row := range rows {
		views = append(views, rowToView(row))
	}
	return views, nil
}

// rowToView は tenant_apps の DB 行（TenantAppRow）を API 表現（TenantAppView）へ写像する
// （List 経路 / Req 2.1）。id / tenant_id 等の内部 field は外部へ露出せず、package_name / title /
// icon_url（nullable は null のまま）/ approved_at のみを返す。
func rowToView(row TenantAppRow) TenantAppView {
	return TenantAppView{
		PackageName: row.PackageName,
		Title:       row.Title,
		IconURL:     row.IconURL,
		ApprovedAt:  row.ApprovedAt,
	}
}

// logDeny は拒否 / 失敗経路の原因分析属性（実行者・対象テナント・拒否理由）を構造化ログとして出力する。
//
// reason は人間可読な短い拒否理由のみで、webToken.Value 等の秘匿値は一切含めない（NFR 1.1 / policy.logDeny
// と同方針）。
func (s *service) logDeny(actor, tenantID uuid.UUID, reason string) {
	s.log.Warn("app operation denied",
		logger.ActorID(actor),
		logger.TenantID(tenantID),
		"deny_reason", reason,
	)
}

// 型 assertion 用に service が Service interface（本 task 2 の 2 メソッド: CreatePlayToken / ListApps）を
// 満たすことを compile-time で確認する。後続 task で interface が拡張されると本 check が乖離を検出する。
var _ Service = (*service)(nil)
