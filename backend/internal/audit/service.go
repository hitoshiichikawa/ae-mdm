package audit

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/hitoshiichikawa/ae-mdm/internal/config"
	"github.com/hitoshiichikawa/ae-mdm/internal/logger"
)

// Repository は audit_logs への永続化操作を抽象化する DI 境界。
//
// append-only INSERT と保持期間下限付き SELECT のみを提供し、UPDATE / DELETE を発行する
// メソッドは **持たない**（Req 1.5 / 6.1 を IF レベルで担保）。
//
// 本 interface は consumer-defines-interface イディオムに従い、利用側である service.go 内で
// 宣言する。これにより task 1 完了時点で audit package が単独でビルド・テスト可能になる
// （task 2.1 の concrete 実装は本 interface を満たす struct を提供する。設計乖離の詳細は
// impl-notes.md「確認事項」を参照）。design.md L375-379 のシグネチャと一致させる。
type Repository interface {
	// Insert は Event を audit_logs へ 1 レコード追記する（Req 1.1 / 1.2）。
	Insert(ctx context.Context, ev Event) error
	// Select は effectiveFrom（保持下限）と Filter から動的 SQL を組み立て occurred_at 降順で取得する。
	Select(ctx context.Context, f Filter, effectiveFrom time.Time) ([]Event, error)
}

// Service は監査ログの記録（append-only）と閲覧（保持期間下限の算出を含む）のユースケースを
// 束ねる interface。
//
// update / delete の操作 IF は **一切公開しない**（Req 1.5）。記録済みレコードの不変性は
// 本 interface が update/delete を露出しないこと（IF レベル）と DB 層の append-only 強制
// （RLS + INSERT-only ロール / A2）で二重に担保する。
type Service interface {
	// Record は Event を append-only で 1 レコード追記する（Req 1.1〜1.4）。
	//
	// ev.ID が uuid.Nil なら uuid.New() を採番し、ev.OccurredAt が zero なら clock.Now() を
	// 補完してから Repository へ渡す（DB default に依らず app 側で確定値を持つことで監査の
	// 説明可能性とテストの決定性を担保 / Req 1.1）。永続化失敗時はエラーを呼び出し側へ伝播し、
	// 当該書込を成功として扱わない（Req 1.6 / fail-closed）。
	//
	// ev.Detail は素通しする。ID トークン本体・cookie 生値・パスワード等の機密値を Detail に
	// 入れないことは **呼び出し側の責務**（Req 1.7 / NFR 3.1）。
	//
	// 永続化失敗時は failure_kind=persist_error の構造化 WARN を出力する（NFR 3.2）。機密値
	// （detail 生値）はログ本文に補間しない（NFR 3.1）。
	Record(ctx context.Context, ev Event) error

	// List は Filter に一致する監査ログを occurred_at 降順で返す（Req 2.x / 3.x / 5.x）。
	//
	// 保持期間下限 effectiveFrom = max(retentionFloor, Filter.From) を内部で必ず付与する
	// （retentionFloor = clock.Now() - cfg.AuditLogRetentionDays / Req 5.1〜5.4 / NFR 1.1）。
	// 0 件時は空 slice + nil error を返す（Req 2.8 / 3.4）。テナント分離 / cross-tenant 可視は
	// 呼び出し側が確立した ctx の TenantContext + RLS が担う（Service は ctx を素通しする）。
	List(ctx context.Context, f Filter) ([]Event, error)
}

// service は Service interface の本番実装。
type service struct {
	cfg   config.Config
	repo  Repository
	clock Clock
	log   logger.Logger
}

// NewService は本番用 Service を構築する。
//
// cfg は保持期間（AuditLogRetentionDays）の参照に、repo は永続化に、clock は保持期間下限の
// 現在時刻算出に用いる（design.md「Audit Service」節 / task 1.2 と整合）。log は記録経路の
// 永続化失敗時に failure_kind=persist_error の構造化 WARN を出すために用いる（NFR 3.2）。
// log が nil の場合は WARN 出力を skip する（test fixture / 防御的経路）。
func NewService(cfg config.Config, repo Repository, clock Clock, log logger.Logger) Service {
	return &service{cfg: cfg, repo: repo, clock: clock, log: log}
}

// Record は Service.Record の実装。
//
// 永続化失敗時は failure_kind=persist_error の構造化 WARN を出してからエラーを呼び出し側へ
// 伝播し、当該書込を成功として扱わない（Req 1.6 / NFR 3.2 / fail-closed）。
func (s *service) Record(ctx context.Context, ev Event) error {
	if ev.ID == uuid.Nil {
		ev.ID = uuid.New()
	}
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = s.clock.Now()
	}
	if err := s.repo.Insert(ctx, ev); err != nil {
		s.warnPersistFailure(ev)
		return err
	}
	return nil
}

// warnPersistFailure は監査ログ追記（INSERT）の永続化失敗で failure_kind=persist_error の
// 構造化 WARN を出す（NFR 3.2）。
//
// Record は HTTP 経路外（各ドメイン Service）から呼ばれ request_id を持たないため、
// event_type / result の非機密 field のみ載せる。detail の生値・query 生値・トークン等の
// 機密値はログ本文に補間しない（NFR 3.1）。log が nil の場合は no-op。
func (s *service) warnPersistFailure(ev Event) {
	if s.log == nil {
		return
	}
	s.log.Warn("audit failure",
		"failure_kind", FailureKindPersistError,
		"event_type", string(ev.EventType),
		"result", string(ev.Result),
	)
}

// List は Service.List の実装。
func (s *service) List(ctx context.Context, f Filter) ([]Event, error) {
	effectiveFrom := s.effectiveFrom(f.From)
	return s.repo.Select(ctx, f, effectiveFrom)
}

// effectiveFrom は保持期間下限 retentionFloor と Filter.From のうち遅い方（= max）を返す。
//
// retentionFloor = clock.Now() - cfg.AuditLogRetentionDays（Req 5.1 / 5.2 / 5.4 / NFR 1.1）。
// from が retentionFloor より後（After）のときのみ from を採用し、それ以外（nil / 起点以前）は
// retentionFloor を採用する（Req 5.3 = 保持起点より前を含む期間条件は起点に丸める）。
func (s *service) effectiveFrom(from *time.Time) time.Time {
	retentionFloor := s.clock.Now().AddDate(0, 0, -s.cfg.AuditLogRetentionDays)
	if from != nil && from.After(retentionFloor) {
		return *from
	}
	return retentionFloor
}
