package audit

import (
	"time"

	"github.com/google/uuid"
)

// EventType は監査イベントの種別を表す string ベースの enum。
//
// 正規セット（tenant_create / role_change / policy_change / command_wipe / token_issue 等）の
// 網羅確定は各ドメイン Issue 側の発火実装に依存するため、本 package は任意の string 値を
// 受け付ける（design.md「Audit Types」節 / requirements 未解決事項と整合）。
type EventType string

// ResultType は監査イベントの結果区分を表す string ベースの enum。
//
// 値は ResultSuccess / ResultFailure の 2 値固定（Req 1.3 / 1.4）。
type ResultType string

const (
	// ResultSuccess は操作が成功したことを表す結果区分（Req 1.3）。
	ResultSuccess ResultType = "success"
	// ResultFailure は操作が失敗したことを表す結果区分（Req 1.4）。
	ResultFailure ResultType = "failure"
)

// Event は audit_logs の 1 レコードに 1:1 で対応する値オブジェクト。
//
// 実行者・テナント・イベント種別・対象リソース・詳細・結果・発生時刻を保持する（Req 1.1）。
//
// TenantID == uuid.Nil は **NULL テナント**（特定テナントに帰属しない SuperAdmin 横断操作）を
// 意味する（Req 1.2）。Repository はこの場合に tenant_id を NULL として bind する。
//
// Detail には ID トークン本体・セッション cookie の生値・パスワード等の機密値そのものを
// 格納しないこと（Req 1.7 / NFR 3.1）。Service は Detail を素通しするため、機密値の sanitize は
// **呼び出し側（記録を要求するドメイン Service）の責務**である。
type Event struct {
	// ID は監査ログレコードの一意 ID。uuid.Nil の場合は Service が uuid.New() を採番する。
	ID uuid.UUID
	// TenantID は当該イベントが帰属するテナント。uuid.Nil は NULL テナント（Req 1.2）。
	TenantID uuid.UUID
	// ActorID は操作を行った実行者（admin_users.id）。
	ActorID uuid.UUID
	// EventType はイベント種別（任意の string 値を受け付ける）。
	EventType EventType
	// ResourceID は操作対象リソースの識別子（空文字は対象なし）。
	ResourceID string
	// Detail はイベント詳細。jsonb に直列化される。機密生値を含めない（Req 1.7 / NFR 3.1）。
	Detail map[string]any
	// Result は結果区分（ResultSuccess / ResultFailure / Req 1.3 / 1.4）。
	Result ResultType
	// OccurredAt は発生時刻。zero の場合は Service が clock.Now() を補完する。
	OccurredAt time.Time
}

// Filter は List のクエリ条件を表す値オブジェクト。
//
// ポインタ型は nil を、文字列型は空文字を「未指定（絞り込みなし）」として解釈する。
type Filter struct {
	// TenantID は admin 横断閲覧でのみ利用するテナント絞り込み条件。
	// nil は絞り込みなし（Req 3.2 / 3.5）。tenant 経路では設定せず RLS に委ねる。
	TenantID *uuid.UUID
	// EventType はイベント種別の絞り込み条件。空文字は絞り込みなし（Req 2.2 / 3.3）。
	EventType EventType
	// ActorID は実行者の絞り込み条件（uuid 文字列）。空文字は絞り込みなし（Req 2.3 / 3.3）。
	// parse 検証は handler の責務。
	ActorID string
	// ResourceID は対象リソースの絞り込み条件。空文字は絞り込みなし（Req 2.4 / 3.3）。
	ResourceID string
	// From は発生時刻の開始時刻（以降）の絞り込み条件。nil は未指定（Req 2.5 / 2.6）。
	From *time.Time
	// To は発生時刻の終了時刻（以前）の絞り込み条件。nil は未指定（Req 2.5 / 2.7）。
	To *time.Time
}
