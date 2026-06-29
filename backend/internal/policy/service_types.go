package policy

import (
	"time"

	"github.com/google/uuid"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// 本ファイルは Policy の application 層（Service / Repository / Handler / mapper）が用いる
// DTO・enum・sentinel error を定義する。Validator (#36) が占有する types.go とは命名衝突を
// 避けるため別ファイルに分離する（design.md File Structure Plan / Issue 制約）。
//
// 純粋関数層（types.go / validator.go）は本ファイルに依存しない。依存方向は
// application 層 → Validator の単方向であり、doc.go の依存方向ルールを維持する。

// PolicyRow は `policies` テーブルの 1 行に対応する DB 層のドメイン型（NFR 2.1）。
//
// Repository（後続 task 2.1）が SELECT / INSERT / UPDATE で列ごとに型付き scan する。
// 列構成は migration 0005（id, tenant_id, name, amapi_policy_name, body jsonb, version,
// updated_by, created_at, updated_at）に一致させる。`UpdatedBy` は nullable（NULL は無更新者）
// のため `*uuid.UUID` とし、Repository が NULL を nil へ写像する。`Version` は AMAPI 反映後の
// snapshot バージョンで、`amapi.PolicyBody.Version`（int64）と型を揃える。
type PolicyRow struct {
	// ID はポリシーの primary key（AMAPI policyId の seed にも用いる決定論的命名 / design）。
	ID uuid.UUID
	// TenantID は所有テナントの id。RLS / 複合 FK のテナント分離キー（Req 4.x）。
	TenantID uuid.UUID
	// Name はポリシー表示名（管理者が付与する人間可読名）。
	Name string
	// AMAPIPolicyName は AMAPI policyName（"enterprises/{eid}/policies/{policyId}" 形式）。
	AMAPIPolicyName string
	// Body はポリシー本体 JSON snapshot（AMAPI へ反映した raw body そのもの / pass-through）。
	// 機密パラメータを含み得るため、構造化ログ / 監査 Detail には載せない（Req 5.4 / NFR 3.2）。
	Body map[string]any
	// Version は AMAPI 反映後の snapshot バージョン（AMAPI 払い出し / amapi.PolicyBody.Version）。
	Version int64
	// UpdatedBy は最終更新者の admin_users.id。未設定（NULL）の行では nil。
	UpdatedBy *uuid.UUID
	// CreatedAt はレコード生成時刻。
	CreatedAt time.Time
	// UpdatedAt は最終更新時刻。
	UpdatedAt time.Time
}

// PolicyView は単一ポリシーの詳細を返す API レスポンス表現（GET / POST / PUT 応答 / Req 4.x）。
//
// JSON tag を付与し、Handler（後続 task 5.1）が直接シリアライズできる形にする。本体 JSON
// snapshot（`Body`）まで含めて返すことで、編集 UI が現在値を再取得できる（design API Contract）。
type PolicyView struct {
	// ID はポリシーの primary key。
	ID uuid.UUID `json:"id"`
	// Name はポリシー表示名。
	Name string `json:"name"`
	// Body はポリシー本体 JSON snapshot（pass-through した raw body）。
	Body map[string]any `json:"body"`
	// Version は AMAPI 反映後の snapshot バージョン。
	Version int64 `json:"version"`
	// CreatedAt はレコード生成時刻。
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt は最終更新時刻。
	UpdatedAt time.Time `json:"updated_at"`
}

// PolicySummary はポリシー一覧（GET /api/policies）の 1 件分の軽量表現（Req 4.4）。
//
// 一覧では本体 JSON snapshot（`Body`）を載せず、識別に必要な最小フィールドのみを返す
// （ペイロード削減と、一覧経路で本体機密値を広く露出させない設計 / Req 5.4 / NFR 3.2）。
type PolicySummary struct {
	// ID はポリシーの primary key。
	ID uuid.UUID `json:"id"`
	// Name はポリシー表示名。
	Name string `json:"name"`
	// Version は AMAPI 反映後の snapshot バージョン。
	Version int64 `json:"version"`
	// UpdatedAt は最終更新時刻。
	UpdatedAt time.Time `json:"updated_at"`
}

// PolicyRequest はポリシー作成・更新要求の入力 DTO（POST / PUT /api/policies / Req 1.x / 2.x）。
//
// `Body` は AMAPI Policy 本体そのものを `map[string]any` で受け取り、mapper が Validator の
// `PolicyInput` へ変換した上で検証する。検証通過後は raw body のまま AMAPI へ pass-through する
// （投機的に strongly-typed 化しない / design 確認事項 4）。
type PolicyRequest struct {
	// Name は作成・更新するポリシーの表示名。空は Service / Handler が弾く（Req 1.x）。
	Name string `json:"name"`
	// Body は AMAPI Policy 本体 JSON（任意 key-value）。mapper が 5 領域を抽出して検証する。
	Body map[string]any `json:"body"`
}

// AssignInput はポリシーの端末割当要求の入力 DTO（PUT /api/policies/{id}/assign / Req 3.x）。
type AssignInput struct {
	// DeviceID は割当先の端末 id（自テナントの devices.id）。自テナント不在は NotFound（Req 3.3）。
	DeviceID uuid.UUID `json:"device_id"`
}

// Operation は監査対象のポリシー操作種別（Req 5.x / NFR 3.1）。
type Operation string

const (
	// OperationCreate はポリシー作成操作。
	OperationCreate Operation = "create"
	// OperationUpdate はポリシー更新操作。
	OperationUpdate Operation = "update"
	// OperationDelete はポリシー削除操作。
	OperationDelete Operation = "delete"
	// OperationAssign はポリシーの端末割当操作。
	OperationAssign Operation = "assign"
)

// Result は監査対象操作の結果（成功 / 失敗）（Req 5.x / NFR 3.1）。
type Result string

const (
	// ResultSuccess は操作成功。
	ResultSuccess Result = "success"
	// ResultFailure は操作失敗（拒否含む）。
	ResultFailure Result = "failure"
)

// 以下の sentinel error は Service / Repository（後続 task）が `errors.Is` 比較や wrap 元として
// 用いる package-level な read-only 変数。design.md「Error Handling」の Code 写像に従い、それぞれ
// 対応する HTTP ステータスへ写像される。利用側はこれらを read-only に扱い（再代入しない）、
// 必要に応じて errors.Wrap で cause を付与した新規 *errors.Error を返す。
//
// **存在露出なし（Req 4.5）**: message は対象ポリシーの存在有無を露出しない汎用文言とする。
var (
	// ErrPolicyNotFound はポリシー不在 / 他テナント越境（RLS で 0 行）。HTTP 404
	// （Req 4.4 / 4.5 で存在差を露出しない汎用文言）。
	ErrPolicyNotFound = pkgerrors.New(pkgerrors.CodeNotFound, "policy not found")
	// ErrDeleteConflict は割当済み端末が存在するポリシーの削除競合。HTTP 409
	// （design 確認事項 3 推奨案 / Req 5.3）。
	ErrDeleteConflict = pkgerrors.New(pkgerrors.CodeConflict, "policy is assigned to one or more devices")
)
