package enrollment

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
)

// TokenRepository は enrollment_tokens の snapshot 永続化（Insert）と一覧参照（List）を
// tenant-scoped RLS で集約する（design.md「enrollment.TokenRepository」節 / Req 1.3 / 4.1 / NFR 3.1）。
//
// すべてのメソッドは tenant-scoped な TenantContext（Handler が確立する自テナント文脈）のまま
// db.BeginTxFunc で tx を開き、RLS（自テナント限定）に分離を委ねる（policy.Repository を手本、
// SuperAdmin 昇格しない）。秘密値（Value / QRCode）列は持たず bind も scan もしない（NFR 3.1）。
type TokenRepository interface {
	// Insert は発行済みトークンの snapshot を 1 行 INSERT する（Req 1.3）。
	//
	// bind 列は (id, tenant_id, amapi_token_name, mode, policy_id, additional_data, expires_at,
	// issued_by)。policy_id は FULLY_MANAGED で NULL（*uuid.UUID の nil）。created_at は DB
	// DEFAULT now() に委ねる。永続化失敗（接続不通等）は *errors.Error{Code: CodeUnavailable} で wrap。
	Insert(ctx context.Context, row TokenRow) error

	// List は自テナントの発行済みトークン snapshot を created_at 昇順で返す（Req 4.1）。
	//
	// 0 件のときは非 nil の空 slice を返す。RLS により自テナント行のみが可視。DB 失敗は
	// CodeUnavailable へ wrap。Status（active / expired）派生は呼び出し側（Service）が DeriveStatus で行う。
	List(ctx context.Context, tenantID uuid.UUID) ([]TokenRow, error)
}

// tokenRepository は TokenRepository の pgxpool ベース実装。
type tokenRepository struct {
	pool *pgxpool.Pool
}

// NewTokenRepository は TokenRepository を構築する。pool は cmd/api bootstrap が構築する共有
// pgxpool.Pool を渡す（policy.NewRepository と同方式）。
func NewTokenRepository(pool *pgxpool.Pool) TokenRepository {
	return &tokenRepository{pool: pool}
}

// rowScanner は scanTokenRow が依存する最小 interface（pgx.Rows / pgx.Row が満たす）。
// 実 PostgreSQL に依存せず scanTokenRow を単体テストするための extension point
// （policy.rowScanner と同型）。
type rowScanner interface {
	Scan(dest ...any) error
}

// selectTokenColumns は SELECT 句の列順（scanTokenRow の Scan 順と一致させる / NFR 3.1）。
// 秘密値（Value / QRCode）列は存在しないため一切列挙しない。
const selectTokenColumns = `id, tenant_id, amapi_token_name, mode, policy_id, additional_data, expires_at, issued_by, created_at`

// scanTokenRow は enrollment_tokens の 1 行を列ごとに型付きで TokenRow へ走査する（List 共通）。
//
// mode enum は一旦 string へ scan してから Mode へ写像する（named string 型の scan 互換のため）。
// policy_id（nullable）は *uuid.UUID へ（NULL は nil）、additional_data（jsonb）は JSON 文字列へ
// scan する。Scan の error は wrap せずそのまま返し、呼び出し側（List）が Unavailable 写像に委ねる。
func scanTokenRow(row rowScanner) (TokenRow, error) {
	var t TokenRow
	var mode string
	if err := row.Scan(
		&t.ID,
		&t.TenantID,
		&t.AMAPITokenName,
		&mode,
		&t.PolicyID,
		&t.AdditionalData,
		&t.ExpiresAt,
		&t.IssuedBy,
		&t.CreatedAt,
	); err != nil {
		return TokenRow{}, err
	}
	t.Mode = Mode(mode)
	return t, nil
}

// Insert は TokenRepository.Insert の実装。
func (r *tokenRepository) Insert(ctx context.Context, row TokenRow) error {
	return db.BeginTxFunc(ctx, r.pool, func(tx pgx.Tx) error {
		// policy_id は *uuid.UUID をそのまま bind（nil は NULL）。mode は enum ラベル文字列で bind。
		// additional_data は JSON 文字列で bind（pgx JSONBCodec が string を受け付ける）。
		// Value / QRCode は列が無いため bind しない（NFR 3.1）。
		if _, err := tx.Exec(ctx,
			`INSERT INTO enrollment_tokens
			 (id, tenant_id, amapi_token_name, mode, policy_id, additional_data, expires_at, issued_by)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			row.ID, row.TenantID, row.AMAPITokenName, string(row.Mode), row.PolicyID,
			row.AdditionalData, row.ExpiresAt, row.IssuedBy,
		); err != nil {
			return pkgerrors.Wrap(pkgerrors.CodeUnavailable, "enrollment token insert failed", err)
		}
		return nil
	})
}

// List は TokenRepository.List の実装。
func (r *tokenRepository) List(ctx context.Context, tenantID uuid.UUID) ([]TokenRow, error) {
	// 0 件でも非 nil の空 slice を返す。
	rowsOut := make([]TokenRow, 0)
	err := db.BeginTxFunc(ctx, r.pool, func(tx pgx.Tx) error {
		// RLS により自テナント行のみ可視。tenant_id 条件は二重防御として明示する（NFR 2.1 相当）。
		rows, err := tx.Query(ctx,
			`SELECT `+selectTokenColumns+`
			 FROM enrollment_tokens
			 WHERE tenant_id = $1
			 ORDER BY created_at ASC`,
			tenantID,
		)
		if err != nil {
			return pkgerrors.Wrap(pkgerrors.CodeUnavailable, "enrollment token list query failed", err)
		}
		defer rows.Close()
		for rows.Next() {
			t, scanErr := scanTokenRow(rows)
			if scanErr != nil {
				return pkgerrors.Wrap(pkgerrors.CodeUnavailable, "enrollment token row scan failed", scanErr)
			}
			rowsOut = append(rowsOut, t)
		}
		if err := rows.Err(); err != nil {
			return pkgerrors.Wrap(pkgerrors.CodeUnavailable, "enrollment token list iteration failed", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return rowsOut, nil
}
