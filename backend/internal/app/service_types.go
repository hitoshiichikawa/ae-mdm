package app

import (
	"time"

	"github.com/google/uuid"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// 本ファイルは App の application 層（Service / Repository / Handler）が用いる DTO・sentinel
// error を定義する（design.md「Data Models」節 / internal/policy/service_types.go に倣う）。
//
// JSON tag を付与した View / Request 型は Handler が直接シリアライズ / デシリアライズできる
// 形にする。DB 層型（TenantAppRow）は JSON tag を持たず、View への写像を経由して外部へ返す。

// PlayTokenRequest は iframe 表示用 webToken の発行要求入力 DTO（POST /api/play-tokens / Req 1.1/1.2）。
type PlayTokenRequest struct {
	// ParentFrameURL は Managed Google Play 承認 iframe を埋め込む親フレームの URL。
	// 空 / 欠落は Service / Handler が入力不正（400）として弾く（Req 1.2）。
	ParentFrameURL string `json:"parent_frame_url"`
}

// PlayTokenView は発行済み webToken を返す API レスポンス表現（POST /api/play-tokens 応答 / Req 1.1）。
//
// **秘匿値（NFR 1.1）**: Value は Managed Google Play iframe 埋め込み用の短命トークンであり、
// PlayTokenView 以外へ流さず、構造化ログ・監査 Detail に平文出力しない。Handler も含めログ経路に
// 載せてはならない。
type PlayTokenView struct {
	// Value は iframe 埋め込み用 webToken の値（秘匿・ログ非出力）。
	Value string `json:"value"`
}

// SyncApp は同期対象アプリ 1 件の入力 DTO（承認結果として frontend が relay する / Req 3.1）。
type SyncApp struct {
	// PackageName は Android アプリのパッケージ名（例: "com.example.app"）。空は 400（後続 Service task）。
	PackageName string `json:"package_name"`
	// Title はアプリ表示名（tenant_apps.title は NOT NULL のため空は 400）。
	Title string `json:"title"`
	// IconURL はアプリアイコン URL（nullable / tenant_apps.icon_url は NULL 許可）。
	IconURL *string `json:"icon_url,omitempty"`
}

// SyncRequest は承認結果のカタログ同期要求入力 DTO（POST /api/apps/sync / Req 3.1）。
type SyncRequest struct {
	// Apps は同期対象アプリのリスト。空リストは反映件数 0 の正常応答になる（Req 3.4）。
	Apps []SyncApp `json:"apps"`
}

// SyncResult は同期完了時に返す反映件数と同期時刻（POST /api/apps/sync 応答 / Req 3.3）。
type SyncResult struct {
	// SyncedAt は同期実行時刻（応答用 / 非永続 / now()）。
	SyncedAt time.Time `json:"synced_at"`
	// Count は tenant_apps へ反映した件数（新規 + 更新）。空リストは 0（Req 3.4）。
	Count int `json:"count"`
}

// TenantAppRow は `tenant_apps` テーブルの 1 行に対応する DB 層のドメイン型（NFR 2.1）。
//
// Repository が SELECT で列ごとに型付き scan する。列構成は migration 0008（id, tenant_id,
// package_name, title, icon_url, approved_at）に一致させる。`IconURL` は nullable（NULL は
// アイコン未設定）のため `*string` とし、Repository が NULL を nil へ写像する。DB 層型のため
// JSON tag は付与しない（外部応答は TenantAppView を経由する）。
type TenantAppRow struct {
	// ID は tenant_apps の primary key（0008 の id 列に DEFAULT は無く Upsert 時に採番する）。
	ID uuid.UUID
	// TenantID は所有テナントの id。RLS / UNIQUE(tenant_id, package_name) のテナント分離キー（Req 2.3 / 4.x）。
	TenantID uuid.UUID
	// PackageName は Android アプリのパッケージ名。
	PackageName string
	// Title はアプリ表示名。
	Title string
	// IconURL はアプリアイコン URL。未設定（NULL）の行では nil。
	IconURL *string
	// ApprovedAt は承認（最終反映）時刻。Upsert 時に now() へ更新される。
	ApprovedAt time.Time
}

// TenantAppView は承認済みカタログ 1 件を返す API レスポンス表現（GET /api/apps 応答 / Req 2.1）。
//
// JSON tag を付与し、Handler が直接シリアライズできる形にする。`IconURL` は nullable のため
// null をそのまま返せる `*string`（`omitempty` は付けず、未設定を明示的に null で返す）。
type TenantAppView struct {
	// PackageName は Android アプリのパッケージ名。
	PackageName string `json:"package_name"`
	// Title はアプリ表示名。
	Title string `json:"title"`
	// IconURL はアプリアイコン URL（未設定は null）。
	IconURL *string `json:"icon_url"`
	// ApprovedAt は承認（最終反映）時刻。
	ApprovedAt time.Time `json:"approved_at"`
}

// ErrAppNotApproved はポリシー紐付け対象アプリが自テナントの承認済みカタログに存在しないこと
// を示す sentinel error。CheckAppsApproved（後続 task 4）が未承認アプリの紐付けを拒否する際に
// 返す（HTTP 422 / Req 5.2）。
//
// 利用側（Service）はこれを read-only に扱い（再代入しない）、必要に応じて errors.Wrap で
// cause を付与した新規 *errors.Error を返す。internal/policy/service_types.go の sentinel と
// 同じ利用契約に従う。
var ErrAppNotApproved = pkgerrors.New(pkgerrors.CodeBusinessRule, "app is not in the approved catalog")
