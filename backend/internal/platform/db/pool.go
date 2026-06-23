// Package db は ae-mdm の DB 接続プール・トランザクション境界・RLS GUC 発行・
// テナント context 強制を提供する Platform Layer のモジュール。
//
// 本パッケージは requirements.md Req 4.1〜4.5 / NFR 1.1 / NFR 1.2 / NFR 3.1 / NFR 3.2 /
// design.md Components: DB Connection Pool / Tenant Context Middleware / TxManager + RLS Helper
// 節に対応する。
//
// 依存方向: config / errors / logger を import 可。逆方向（config / errors / logger →
// platform/db）は禁止（design.md「Architecture Pattern & Boundary Map」と整合）。
package db

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	internalerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// NewPool は config から pgxpool を構築し、起動時 Ping で疎通を検証する。
//
// requirements.md Req 4.1 / NFR 3.1 / NFR 3.2 と design.md Components: DB Connection Pool
// 節に対応する。失敗時（ParseConfig 失敗 / NewWithConfig 失敗 / Ping 失敗）は
// *errors.Error{Code: CodeUnavailable} を返し、cmd/api / cmd/worker の bootstrap で
// exit code != 0 + 構造化 ERROR ログを出す invariant を成立させる。
//
// 戻り値の *pgxpool.Pool は呼び出し側が Close() する責務を持つ（通常は cmd/api の
// graceful shutdown 経路で defer Close する）。
func NewPool(ctx context.Context, cfg config.Config) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, internalerrors.Wrap(
			internalerrors.CodeUnavailable,
			"db: 接続文字列の解析に失敗",
			err,
		)
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, internalerrors.Wrap(
			internalerrors.CodeUnavailable,
			"db: 接続プールの構築に失敗",
			err,
		)
	}

	if err := pool.Ping(ctx); err != nil {
		// Ping 失敗時は pool を閉じてから返す（leak 防止）。
		pool.Close()
		return nil, internalerrors.Wrap(
			internalerrors.CodeUnavailable,
			"db: 起動時 Ping に失敗",
			err,
		)
	}

	return pool, nil
}
