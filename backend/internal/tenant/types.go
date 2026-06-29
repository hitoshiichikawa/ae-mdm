package tenant

import (
	"context"
	"time"

	"github.com/google/uuid"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// Status はテナントのライフサイクル状態を表す機械可読な enum（NFR 1.1）。
//
// テナント状態は常に以下 4 値のいずれか 1 つを取る（#52 で binding を追加 / Req 4.1）:
//
//   - StatusPendingBind: テナント作成済みだが Enterprise 未バインド
//   - StatusBinding:      バインド予約中（pending_bind→binding 原子遷移の勝者のみが
//     CreateEnterprise に進む中間状態。enterprise_name は未確定 / NFR 1.2）
//   - StatusBound:        Enterprise バインド済み（enterprise_name 確定）
//   - StatusDisabled:     無効化済み（終端 / 本 Issue では再有効化遷移を持たない）
//
// DB 側 `tenant_status` enum（migration 0001 + 0017 で binding 追加）と文字列値を一致させる。
type Status string

const (
	// StatusPendingBind はバインド未完了状態（テナント作成直後の初期状態）。
	StatusPendingBind Status = "pending_bind"
	// StatusBinding はバインド予約中状態（pending_bind→binding 予約に成功した勝者が
	// CreateEnterprise を待つ中間状態 / Req 1.1）。enterprise_name は未確定（NFR 1.2）であり、
	// 再 bind 可能・disable 可能だが bound とはみなさない（Req 2.3）。
	StatusBinding Status = "binding"
	// StatusBound はバインド済み状態（enterprise_name が確定している）。
	StatusBound Status = "bound"
	// StatusDisabled は無効化状態（終端状態 / Req 3.1）。
	StatusDisabled Status = "disabled"
)

// Valid は Status が定義済みの 4 値のいずれかであるとき true を返す（NFR 1.1 / Req 4.1）。
// 未定義値（空文字 / 想定外文字列 / 大文字小文字違い）に対しては false を返す。
func (s Status) Valid() bool {
	switch s {
	case StatusPendingBind, StatusBinding, StatusBound, StatusDisabled:
		return true
	default:
		return false
	}
}

// String は Status の文字列表現を返す（fmt / ログ用）。
func (s Status) String() string {
	return string(s)
}

// ParseStatus は文字列を Status へ変換する（NFR 1.1 / Req 4.1）。
//
// 入力が定義済みの 4 値（pending_bind / binding / bound / disabled）のいずれかであれば対応する
// Status を返し、それ以外（空文字を含む未定義値）は `*errors.Error{Code: CodeInvalidRequest}`
// を返す。DB から読み出した status 文字列の正当性検証や、外部入力のバリデーションに用いる。
func ParseStatus(s string) (Status, error) {
	candidate := Status(s)
	if candidate.Valid() {
		return candidate, nil
	}
	return "", pkgerrors.New(pkgerrors.CodeInvalidRequest, "invalid tenant status")
}

// TenantRow は `tenants` テーブルの 1 行に対応する DB 層のドメイン型。
//
// Repository（後続 task 3.1）が SELECT / INSERT / UPDATE で読み書きする。
// EnterpriseName は **bound 時のみ非空** であり、pending_bind / binding / disabled では空文字
// となる（nullable カラムを最も素直な「空文字 = 未設定」で表現する設計判断。`*string` ではなく
// `string` を採用し、Repository は NULL を空文字へ写像する。これにより View 変換時の
// nil ポインタ分岐を避け、`EnterpriseName == ""` で未バインドを判定できる）。
// SignupURLName も同じ「NULL→空文字」写像パターンで扱う（Repository 実装は後続 task 3.1）。
// DisabledAt / DisabledBy は無効化された行でのみ非 nil（NFR 2.1 の補助 / 0016 監査列）。
type TenantRow struct {
	// ID はテナントの primary key。
	ID uuid.UUID
	// Name はテナント名（顧客企業名）。空は許容しない（Req 1.3 で Service / Handler が弾く）。
	Name string
	// Status は現在のライフサイクル状態（常に 4 値のいずれか / NFR 1.1 / Req 4.1）。
	Status Status
	// EnterpriseName は AMAPI Enterprise 識別子。bound 時のみ非空、それ以外は空文字。
	EnterpriseName string
	// SignupURLName は CreateSignupURL が返した識別子（発行元テナントに束縛される正本 / Req 3.1）。
	// create 時に永続化し、bind 時の CreateEnterprise 引数に用いる。未発行 / NULL は空文字へ写像する
	// （Repository は NULL→空文字。Repository 永続化実装は後続 task 3.1）。ログ / 監査には出さない（NFR 3.2）。
	SignupURLName string
	// CreatedAt はレコード生成時刻。
	CreatedAt time.Time
	// UpdatedAt は最終更新時刻。
	UpdatedAt time.Time
	// DisabledAt は無効化時刻。無効化されていない行では nil（0016 監査列 / NFR 2.1）。
	DisabledAt *time.Time
	// DisabledBy は無効化実行者の admin_users.id。無効化されていない行では nil（0016 監査列）。
	DisabledBy *uuid.UUID
}

// TenantView は API レスポンスとして呼び出し側へ返すテナント表現（Req 4.1 / 4.2 / 4.4）。
//
// JSON tag を付与し、Handler（後続 task 6.1）が直接シリアライズできる形にする。
// EnterpriseName は bound 時のみ存在するため `omitempty` を付与し、pending_bind / binding /
// disabled の応答には enterprise_name フィールドを含めない（Req 6.5 の存在差非露出にも整合）。
type TenantView struct {
	// ID はテナントの primary key。
	ID uuid.UUID `json:"id"`
	// Name はテナント名。
	Name string `json:"name"`
	// Status は現在のライフサイクル状態（pending_bind / binding / bound / disabled / Req 4.4）。
	Status Status `json:"status"`
	// EnterpriseName は AMAPI Enterprise 識別子（bound 時のみ。それ以外は省略）。
	EnterpriseName string `json:"enterprise_name,omitempty"`
}

// ViewFromRow は TenantRow を API 応答用の TenantView へ変換する。
// Handler / Service が DB 行から応答を組み立てる際に用いる共通変換ヘルパ。
//
// enterprise_name は **bound 時のみ** View に載せる（TenantView 契約 / Req 4.2）。
// 無効化された行は監査目的で DB 上 enterprise_name を保持し続ける（UpdateDisabled は
// status のみ更新する）が、disabled / pending_bind / binding の GET / List 応答に
// enterprise_name を漏らさない（status!=bound の応答に enterprise 識別子を露出しない /
// Req 4.2 の条件付き返却・Req 6.5 の存在差非露出と整合。binding は予約中で enterprise_name
// 未確定 / NFR 1.2 のため status は 4 値で返しつつ識別子は載せない / Req 4.4）。
func ViewFromRow(row TenantRow) TenantView {
	view := TenantView{
		ID:     row.ID,
		Name:   row.Name,
		Status: row.Status,
	}
	if row.Status == StatusBound {
		view.EnterpriseName = row.EnterpriseName
	}
	return view
}

// CreateInput はテナント作成要求の入力 DTO（Req 1.1 / 1.3）。
// `POST /api/admin/tenants` の request body（Name のみ）に対応する。
type CreateInput struct {
	// Name は作成するテナント名。空白 trim 後に空であれば Service / Handler が弾く（Req 1.3）。
	Name string `json:"name"`
	// SignupURLName は CreateSignupURL が返した識別子（発行元束縛の正本 / Req 3.1）。
	// HTTP request body 由来ではなく Service が CreateSignupURL 成功後に充填し Repository.Insert へ
	// 永続化させる内部フィールドのため `json:"-"`（body から受け取らない）。Service / Repository
	// 側の永続化配線は後続 task 5.1 / 3.1 の責務（本 task はフィールド追加のみ）。
	SignupURLName string `json:"-"`
}

// BindInput は Enterprise バインド要求の入力 DTO（Req 2.1）。
// `POST /api/admin/tenants/{id}/bind` の request body に対応する。
type BindInput struct {
	// SignupURLName は CreateSignupURL が返した識別子（CreateEnterprise の引数）。
	// admin が訪れる signupURL 自体ではなく、後続 Bind で使う識別子の方を渡す。
	SignupURLName string `json:"signup_url_name"`
}

// DisableInput はテナント無効化要求の入力 DTO（Req 3.1 / 3.2）。
// `DELETE /api/admin/tenants/{id}` の request body に対応する。
//
// 二段階確認は確認テキスト方式（design.md API Contract）を採用し、Confirmation に対象
// テナントの name を再入力させ、Service が `tenants.name` との完全一致を検証する。
type DisableInput struct {
	// Confirmation は二段階確認の入力値（対象テナント name の再入力）。
	// 一致しなければ確認未完了として弾く（Req 3.2）。
	Confirmation string `json:"confirmation"`
}

// SignupURL はテナント作成時に生成されるサインアップ URL の DTO（Req 1.2）。
//
// AMAPI `CreateSignupURL` は admin が訪れる URL（URL）と後続 Bind で使う識別子（Name）の
// 2 値を返すため、両方を保持する。URL のみ HTTP レスポンス body（`signup_url`）に載せ、
// 構造化ログ / 監査 Event には載せない（NFR 2.3 / design.md Security Considerations）。
type SignupURL struct {
	// URL は admin が訪れて Google サインアップを行う URL（HTTP 応答 `signup_url`）。
	URL string `json:"signup_url"`
	// Name は後続 Bind（CreateEnterprise）で使う signupURLName 識別子。
	// ログ / 監査 Event には出さない。
	Name string `json:"-"`
}

// Operation は監査対象のテナント操作種別（NFR 2.1）。
type Operation string

const (
	// OperationCreate はテナント作成操作。
	OperationCreate Operation = "create"
	// OperationBind は Enterprise バインド操作。
	OperationBind Operation = "bind"
	// OperationDisable はテナント無効化操作。
	OperationDisable Operation = "disable"
	// OperationRecover は中断したバインド予約（binding 行）の回収操作（Req 2.4）。
	// RecoverStaleBindings が binding→pending_bind 回収時の Record に用いる（Service 配線は後続 task 5.2）。
	OperationRecover Operation = "recover"
)

// Result は監査対象操作の結果（成功 / 失敗）（NFR 2.1）。
type Result string

const (
	// ResultSuccess は操作成功。
	ResultSuccess Result = "success"
	// ResultFailure は操作失敗（拒否含む）。
	ResultFailure Result = "failure"
)

// Event は監査ログ記録の対象となる 1 件のテナント操作イベント（NFR 2.1）。
//
// **機密値（サインアップ URL の秘密パラメータ・SA 資格情報・OAuth トークンの生値）を
// フィールドとして一切保持しない**（NFR 2.3）。これにより EventRecorder の実装が何を出力
// しても機密値が surface しない一次防御を構造的に成立させる。
type Event struct {
	// Actor は操作実行者の admin_users.id（NFR 2.1）。
	Actor uuid.UUID
	// TenantID は操作対象テナントの id（NFR 2.1）。
	TenantID uuid.UUID
	// Operation は操作種別（create / bind / disable / recover）（NFR 2.1 / Req 2.4）。
	Operation Operation
	// Result は操作結果（success / failure）（NFR 2.1）。
	Result Result
	// ConfirmationCompleted は二段階確認の完了有無（無効化操作で用いる / Req 3.5）。
	ConfirmationCompleted bool
	// DenyReason は拒否時の理由（権限不足・不正状態遷移・確認未完了等 / NFR 2.2）。
	// 機密値は含めない（人間可読な短い拒否理由のみ）。空文字は理由なし（success 等）。
	DenyReason string
}

// EventRecorder はテナント操作の監査イベントを Audit Service へ渡すためのポート（NFR 2.1）。
//
// 投機的抽象化を避け、port は最小（Record 1 本）に限定する。Audit Service 実装後に当該
// Service の Recorder へ差し替える（DI 配線は将来 main の責務 / design.md
// 「tenant.EventRecorder」節）。戻り値 error は、将来の Audit Service 実装が永続化失敗を
// 呼び出し側へ返せるよう拡張余地として持つ（interim の logger 実装は常に nil を返す）。
type EventRecorder interface {
	// Record は 1 件の監査イベントを記録する。
	Record(ctx context.Context, e Event) error
}

// 以下の sentinel error は Service / Repository（後続 task）が `errors.Is` 比較や wrap 元
// として用いる package-level な read-only 変数。design.md「Error Handling」/ State 節の
// Code 写像に従い、それぞれ対応する HTTP ステータスへ写像される。
//
// **存在露出なし（Req 6.5）**: message は対象テナントの存在有無を露出しない汎用文言とする。
// 利用側はこれらを read-only に扱い（再代入しない）、必要に応じて errors.Wrap で cause を
// 付与した新規 *errors.Error を返す。
var (
	// ErrConflict は競合（bound 済みテナントへの重複 bind / 二重無効化）。HTTP 409（Req 2.5 / 3.4）。
	ErrConflict = pkgerrors.New(pkgerrors.CodeConflict, "tenant operation conflicts with current state")
	// ErrInvalidState は未定義 / 不正な状態遷移（disabled へ bind 等）。HTTP 422（Req 2.6 / NFR 1.2）。
	ErrInvalidState = pkgerrors.New(pkgerrors.CodeBusinessRule, "tenant state does not permit this operation")
	// ErrConfirmationRequired は二段階確認未完了（無効化）。HTTP 422（Req 3.2）。
	ErrConfirmationRequired = pkgerrors.New(pkgerrors.CodeBusinessRule, "two-step confirmation is required")
	// ErrNotBound は未バインドテナントへの enterprise 識別子要求。HTTP 422（Req 5.2）。
	ErrNotBound = pkgerrors.New(pkgerrors.CodeBusinessRule, "tenant is not bound to an enterprise")
	// ErrTenantDisabled は無効化テナントへの業務操作要求。HTTP 422（Req 5.3）。
	ErrTenantDisabled = pkgerrors.New(pkgerrors.CodeBusinessRule, "tenant is disabled")
	// ErrTenantNotFound はテナント不在。HTTP 404（Req 4.3 / 6.5 で存在差を露出しない汎用文言）。
	ErrTenantNotFound = pkgerrors.New(pkgerrors.CodeNotFound, "tenant not found")
)
