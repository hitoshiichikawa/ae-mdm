package auth

import (
	"context"
	stderrors "errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/db"
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/oidc"
)

// Repository は sessions / admin_users / state_nonces に対する CRUD を集約する。
//
// design.md「Auth Repository」節および tasks.md task 4.1 と整合。すべてのメソッドは
// 内部で SuperAdmin context（`db.TenantContext{IsSuperAdmin: true}`）を確立した上で
// `db.BeginTxFunc` 経由でトランザクションを開く（callback ハンドラ / middleware は
// A2 の TenantContextMiddleware の外側で動作するため、Repository が自身で SuperAdmin
// context を確立する責務を負う）。
type Repository interface {
	// ConsumeStateNonce は state_nonces テーブルに 1 行 INSERT する。
	//
	// 同一 nonce が既に消費済みの場合、PRIMARY KEY UNIQUE 制約違反（pgerrcode 23505）
	// が発生し、`*errors.Error{Code: CodeUnauthenticated, failure_kind: state_replay}`
	// を返す（Req 2.9 の物理的拒否）。それ以外の DB エラーは `CodeUnavailable` で wrap する。
	//
	// expiresAt は元 state cookie の絶対期限（payload.IssuedAt + cfg.StateCookieTTL）で、
	// 後続 sweeper task がこの値を基準に GC する前提（本 Issue 範囲外）。
	ConsumeStateNonce(ctx context.Context, nonce string, console oidc.Console, expiresAt time.Time) error

	// ResolveAdminUser は (oidc_issuer, oidc_subject) 複合キーで admin_users を解決する。
	//
	// 事前 provisioning 必須の read-modify-write:
	//   1. SuperAdmin context で `SELECT id, tenant_id FROM admin_users WHERE oidc_issuer = $1 AND oidc_subject = $2 FOR UPDATE`
	//   2. 0 行 → `*errors.Error{Code: CodeForbidden, failure_kind: admin_user_not_provisioned}`
	//   3. 1 行 → email 列を IdP 側値で UPDATE（id / tenant_id / oidc_issuer / oidc_subject は不変）
	//   4. admin_role_assignments を join して Roles / IsSuperAdmin を集約し Identity を返す
	//
	// 新規 INSERT は行わない（admin-seed CLI / 管理 UI 経由でのみ provisioning される / design.md）。
	ResolveAdminUser(ctx context.Context, issuer, sub, email string, console oidc.Console) (Identity, error)

	// Create は SuperAdmin context で sessions に 1 行 INSERT する。
	Create(ctx context.Context, s Session) error

	// Get は token_hash で sessions ↔ admin_users を join 取得する。
	//
	// 0 行（hash 不一致 = 改竄）の場合、`*errors.Error{Code: CodeUnauthenticated, failure_kind: session_tamper}`
	// を返す（Req 5.4）。`Identity.Roles` / `IsSuperAdmin` は admin_role_assignments
	// の集約結果で populate される。
	Get(ctx context.Context, tokenHash string) (Session, Identity, error)

	// Touch は last_seen_at を now にセットする（Req 4.3 / 4.8 / expires_at は変更しない）。
	Touch(ctx context.Context, tokenHash string, now time.Time) error

	// Revoke は revoked_at IS NULL の場合のみ now をセットする（Req 5.1 / 冪等）。
	Revoke(ctx context.Context, tokenHash string, now time.Time) error
}

// repository は Repository の pgxpool ベース実装。
type repository struct {
	pool *pgxpool.Pool
}

// NewRepository は Repository を構築する。pool は callback handler / middleware が
// 共有する pgxpool.Pool を渡す（A2 cmd/api/main.go bootstrap で構築される）。
func NewRepository(pool *pgxpool.Pool) Repository {
	return &repository{pool: pool}
}

// superAdminContext は ctx に SuperAdmin TenantContext を埋め込む。
//
// callback handler / auth middleware は A2 の TenantContextMiddleware の外側で動作するため、
// Repository が自身で SuperAdmin context を確立する責務を負う（design.md「Auth Repository」節）。
// TenantContext 未確立のまま BeginTxFunc を呼ぶと A2 の panic ガードに引っかかる。
func superAdminContext(ctx context.Context) context.Context {
	return db.WithTenantContext(ctx, db.TenantContext{
		TenantID:     uuid.Nil,
		IsSuperAdmin: true,
	})
}

// ConsumeStateNonce は Repository.ConsumeStateNonce の実装。
func (r *repository) ConsumeStateNonce(ctx context.Context, nonce string, console oidc.Console, expiresAt time.Time) error {
	ctx = superAdminContext(ctx)
	return db.BeginTxFunc(ctx, r.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO state_nonces (nonce, console, expires_at) VALUES ($1, $2, $3)`,
			nonce, string(console), expiresAt,
		)
		if err == nil {
			return nil
		}
		// PRIMARY KEY UNIQUE 違反は state_replay にマッピング（Req 2.9）。
		// 機密値（nonce 生値）は wrap message に補間しない（NFR 1.1 / NFR 4.2）。
		var pgErr *pgconn.PgError
		if stderrors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation {
			return pkgerrors.Wrap(
				pkgerrors.CodeUnauthenticated,
				"state nonce replay detected",
				FailureKindStateReplay,
			)
		}
		// それ以外の DB エラーは Unavailable で wrap（fail-closed / fixed message）。
		return pkgerrors.Wrap(
			pkgerrors.CodeUnavailable,
			"state nonce insert failed",
			err,
		)
	})
}

// ResolveAdminUser は Repository.ResolveAdminUser の実装。
func (r *repository) ResolveAdminUser(ctx context.Context, issuer, sub, email string, console oidc.Console) (Identity, error) {
	ctx = superAdminContext(ctx)
	var identity Identity
	identity.OIDCSubject = sub

	err := db.BeginTxFunc(ctx, r.pool, func(tx pgx.Tx) error {
		// 1. (oidc_issuer, oidc_subject) 複合キーで FOR UPDATE 行ロック取得
		var (
			adminUserID uuid.UUID
			tenantIDDB  *uuid.UUID
		)
		err := tx.QueryRow(ctx,
			`SELECT id, tenant_id FROM admin_users
			 WHERE oidc_issuer = $1 AND oidc_subject = $2
			 FOR UPDATE`,
			issuer, sub,
		).Scan(&adminUserID, &tenantIDDB)
		if err != nil {
			if stderrors.Is(err, pgx.ErrNoRows) {
				// 事前 provisioning 必須経路。新規 INSERT は行わない（design.md）。
				// 機密値（issuer / sub / email）は wrap message に補間しない。
				return pkgerrors.Wrap(
					pkgerrors.CodeForbidden,
					"admin user not provisioned",
					FailureKindAdminUserNotProvisioned,
				)
			}
			return pkgerrors.Wrap(
				pkgerrors.CodeUnavailable,
				"admin user lookup failed",
				err,
			)
		}

		// 2. email を IdP 側値で UPDATE（id / tenant_id / oidc_issuer / oidc_subject は不変）
		if _, err := tx.Exec(ctx,
			`UPDATE admin_users SET email = $1 WHERE id = $2`,
			email, adminUserID,
		); err != nil {
			return pkgerrors.Wrap(
				pkgerrors.CodeUnavailable,
				"admin user email update failed",
				err,
			)
		}

		// 3. admin_role_assignments を join して Roles / IsSuperAdmin を集約
		rows, err := tx.Query(ctx,
			`SELECT role::text FROM admin_role_assignments WHERE admin_user_id = $1`,
			adminUserID,
		)
		if err != nil {
			return pkgerrors.Wrap(
				pkgerrors.CodeUnavailable,
				"admin role assignments lookup failed",
				err,
			)
		}
		defer rows.Close()
		roles := make([]string, 0)
		isSuperAdmin := false
		for rows.Next() {
			var role string
			if err := rows.Scan(&role); err != nil {
				return pkgerrors.Wrap(
					pkgerrors.CodeUnavailable,
					"admin role assignment scan failed",
					err,
				)
			}
			roles = append(roles, role)
			if role == "SuperAdmin" {
				isSuperAdmin = true
			}
		}
		if err := rows.Err(); err != nil {
			return pkgerrors.Wrap(
				pkgerrors.CodeUnavailable,
				"admin role assignments iteration failed",
				err,
			)
		}

		identity.AdminUserID = adminUserID
		identity.Email = email
		if tenantIDDB != nil {
			identity.TenantID = *tenantIDDB
		}
		identity.Roles = roles
		identity.IsSuperAdmin = isSuperAdmin
		return nil
	})
	if err != nil {
		return Identity{}, err
	}
	return identity, nil
}

// Create は Repository.Create の実装。
func (r *repository) Create(ctx context.Context, s Session) error {
	ctx = superAdminContext(ctx)
	return db.BeginTxFunc(ctx, r.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO sessions
			   (token_hash, admin_user_id, issued_at, last_seen_at, expires_at, revoked_at, console)
			 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			s.TokenHash, s.AdminUserID, s.IssuedAt, s.LastSeenAt, s.ExpiresAt, s.RevokedAt, string(s.Console),
		)
		if err != nil {
			return pkgerrors.Wrap(
				pkgerrors.CodeUnavailable,
				"session insert failed",
				err,
			)
		}
		return nil
	})
}

// Get は Repository.Get の実装。
func (r *repository) Get(ctx context.Context, tokenHash string) (Session, Identity, error) {
	ctx = superAdminContext(ctx)
	var (
		session  Session
		identity Identity
	)
	err := db.BeginTxFunc(ctx, r.pool, func(tx pgx.Tx) error {
		var (
			adminUserID uuid.UUID
			issuedAt    time.Time
			lastSeenAt  time.Time
			expiresAt   time.Time
			revokedAt   *time.Time
			consoleStr  string
			oidcSubject string
			email       string
			tenantIDDB  *uuid.UUID
		)
		err := tx.QueryRow(ctx,
			`SELECT s.admin_user_id, s.issued_at, s.last_seen_at, s.expires_at, s.revoked_at, s.console,
			        u.oidc_subject, u.email, u.tenant_id
			 FROM sessions s
			 JOIN admin_users u ON u.id = s.admin_user_id
			 WHERE s.token_hash = $1`,
			tokenHash,
		).Scan(
			&adminUserID, &issuedAt, &lastSeenAt, &expiresAt, &revokedAt, &consoleStr,
			&oidcSubject, &email, &tenantIDDB,
		)
		if err != nil {
			if stderrors.Is(err, pgx.ErrNoRows) {
				// 0 行 = hash 不一致（改竄）/ Req 5.4 / 機密値（tokenHash 生値）は補間しない。
				return pkgerrors.Wrap(
					pkgerrors.CodeUnauthenticated,
					"session not found",
					FailureKindSessionTamper,
				)
			}
			return pkgerrors.Wrap(
				pkgerrors.CodeUnavailable,
				"session lookup failed",
				err,
			)
		}
		session = Session{
			TokenHash:   tokenHash,
			AdminUserID: adminUserID,
			Console:     oidc.Console(consoleStr),
			IssuedAt:    issuedAt,
			LastSeenAt:  lastSeenAt,
			ExpiresAt:   expiresAt,
			RevokedAt:   revokedAt,
		}

		// Roles / IsSuperAdmin を admin_role_assignments から集約
		rows, err := tx.Query(ctx,
			`SELECT role::text FROM admin_role_assignments WHERE admin_user_id = $1`,
			adminUserID,
		)
		if err != nil {
			return pkgerrors.Wrap(
				pkgerrors.CodeUnavailable,
				"admin role assignments lookup failed",
				err,
			)
		}
		defer rows.Close()
		roles := make([]string, 0)
		isSuperAdmin := false
		for rows.Next() {
			var role string
			if err := rows.Scan(&role); err != nil {
				return pkgerrors.Wrap(
					pkgerrors.CodeUnavailable,
					"admin role assignment scan failed",
					err,
				)
			}
			roles = append(roles, role)
			if role == "SuperAdmin" {
				isSuperAdmin = true
			}
		}
		if err := rows.Err(); err != nil {
			return pkgerrors.Wrap(
				pkgerrors.CodeUnavailable,
				"admin role assignments iteration failed",
				err,
			)
		}

		identity = Identity{
			AdminUserID:  adminUserID,
			OIDCSubject:  oidcSubject,
			Email:        email,
			Roles:        roles,
			IsSuperAdmin: isSuperAdmin,
		}
		if tenantIDDB != nil {
			identity.TenantID = *tenantIDDB
		}
		return nil
	})
	if err != nil {
		return Session{}, Identity{}, err
	}
	return session, identity, nil
}

// Touch は Repository.Touch の実装。
//
// `revoked_at IS NULL` を WHERE 条件に含めることで、`Get` と `Touch` の間に並走した
// `Revoke` が成功した行（= 直後にログアウトされた session）の `last_seen_at` を
// 更新しないことを保証する（Req 5.1 / 5.3 / PR #42 round-1 review 由来）。条件に
// マッチしなくても本メソッドは error を返さない（呼び出し側で `LookupAndRefresh` の
// 次回呼び出しが `Get` 段階で revoked_at を検知し失効と判定する経路に倒れる / 冪等）。
func (r *repository) Touch(ctx context.Context, tokenHash string, now time.Time) error {
	ctx = superAdminContext(ctx)
	return db.BeginTxFunc(ctx, r.pool, func(tx pgx.Tx) error {
		// Req 4.8: expires_at は変更しない。last_seen_at のみを now にセット。
		// Req 5.1 / 5.3: revoked_at が立っている行（logout / revokeOnExpire 済）は更新しない。
		if _, err := tx.Exec(ctx,
			`UPDATE sessions SET last_seen_at = $1
			 WHERE token_hash = $2 AND revoked_at IS NULL`,
			now, tokenHash,
		); err != nil {
			return pkgerrors.Wrap(
				pkgerrors.CodeUnavailable,
				"session touch failed",
				err,
			)
		}
		return nil
	})
}

// Revoke は Repository.Revoke の実装。
func (r *repository) Revoke(ctx context.Context, tokenHash string, now time.Time) error {
	ctx = superAdminContext(ctx)
	return db.BeginTxFunc(ctx, r.pool, func(tx pgx.Tx) error {
		// Req 5.1: 冪等性（既に revoked_at が立っている行は更新しない）。
		if _, err := tx.Exec(ctx,
			`UPDATE sessions SET revoked_at = $1 WHERE token_hash = $2 AND revoked_at IS NULL`,
			now, tokenHash,
		); err != nil {
			return pkgerrors.Wrap(
				pkgerrors.CodeUnavailable,
				"session revoke failed",
				err,
			)
		}
		return nil
	})
}
