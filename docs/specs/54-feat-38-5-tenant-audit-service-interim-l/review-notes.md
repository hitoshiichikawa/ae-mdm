# Review Notes

<!-- idd-claude:review round=1 model=claude-opus-4.8 timestamp=2026-06-29T00:00:00Z -->

## Reviewed Scope

- Branch: claude/issue-54-impl-feat-38-5-tenant-audit-service-interim-l
- HEAD commit: 2042ca4d5d3cdb6662d6c7ad3b7804e584a88fab
- Compared to: develop..HEAD
- 変更ファイル: `backend/cmd/api/main.go`（DI 配線差し替え） / `backend/internal/tenantaudit/recorder.go`（新規アダプタ） / `backend/internal/tenantaudit/recorder_test.go`（新規ユニットテスト） / spec docs（`impl-notes.md` / `requirements.md`）
- 補足: 本 Issue は Architect 段を経ない単一実装パスで `tasks.md` / `design.md` が不在。`_Boundary:_` アノテーションが無いため、境界判定は requirements.md の Out of Scope 節を基準に実施した。

## Verified Requirements

- 1.1 — `main.go` で `auditSvc` を注入した `tenantaudit.NewRecorder` が `tenant.Service` に配線され、成功イベントが `audit.Service.Record` へ委譲される / `TestRecorder_Record_OperationMapping`（create/bind 成功ケースで recordCalls==1 を検証）
- 1.2 — `mapResult` が `tenant.ResultFailure` を `audit.ResultFailure` へ写像し失敗区分で記録要求 / `TestRecorder_Record_OperationMapping`（disable 失敗ケース wantResult=ResultFailure）
- 1.3 — 同一 `auditSvc` が `/api/admin/audit-logs` 閲覧経路の backing であり、アダプタ委譲により永続化＝閲覧可能化が成立（既存 #5/#37 実装）/ `main.go` 配線 + 委譲で観測可能
- 1.4 — `tenant.NewLoggerRecorder(log)` → `tenantaudit.NewRecorder(auditSvc, log)` への差し替え（`main.go` diff）
- 2.1 — `recorder.go` `ActorID: e.Actor` / `TestRecorder_Record_OperationMapping`（ActorID 検証）
- 2.2 — `TenantID: e.TenantID` / 同上（TenantID 検証）
- 2.3 — `mapResult`（success/failure 双方）/ `TestRecorder_Record_OperationMapping` + `TestRecorder_Record_ResultSuccessMapping`
- 2.4 — `mapEventType` create→`tenant_create` / `TestRecorder_Record_OperationMapping`
- 2.5 — bind→`tenant_bind` / 同上
- 2.6 — disable→`tenant_disable` / 同上
- 2.7 — `mapEventType` default→`EventTypeUnknown`（無音破棄せず委譲）/ `TestRecorder_Record_UnknownOperation_MapsToIdentifiableType`（recordCalls==1 + EventTypeUnknown 検証）
- 2.8 — `ResourceID: e.TenantID.String()` / `TestRecorder_Record_OperationMapping`（ResourceID 検証）
- 2.9 — `audit.Event.ID` / `OccurredAt` を zero 据置で委譲 / `TestRecorder_Record_DelegatesIDAndOccurredAt`
- 3.1 — `Record` が `r.svc.Record` の error をそのまま return（境界で握りつぶさない）/ `TestRecorder_Record_PropagatesPersistError`
- 3.2 — 同上（error 伝播 = 成功扱いしない）/ 同テスト（`errors.Is` で wantErr 一致を検証）
- 3.3 — `internal/tenant/service.go` の `record()` helper 契約は本差分で未変更（diff name-only に不在）
- 3.4 — `failure_kind=persist_error` 構造化ログは AC subject の「監査ログ Service」責務であり既存 `internal/audit/service.go` `warnPersistFailure`（`FailureKindPersistError="persist_error"`）が担う
- 4.1 — `buildDetail` が `confirmation_completed` / `deny_reason` のみ載せ機密生値を持ち込まない / `TestRecorder_Record_DetailHasNoSensitiveKeys`（許可鍵リスト検証）
- 4.2 — 非機密フィールドを機械可読鍵名で写像 / `TestRecorder_Record_DetailCarriesNonSensitiveFields`
- 4.3 — `tenant.Event` が機密フィールドを構造的に持たない一次防御に依拠（新規持ち込み経路なし）/ `TestRecorder_Record_DetailHasNoSensitiveKeys`
- 4.4 — `buildDetail` の `len(detail)==0 → nil` 防御ガード（現 `tenant.Event` 構造では `confirmation_completed` が常時 loadable な非機密フィールドのため前提が空集合＝発火しない conditional だが、規定挙動は実装済み）/ 到達可能挙動は `TestRecorder_Record_DenyReasonEmpty_OmitsDenyReasonKey` でカバー
- 5.1 — `var _ tenant.EventRecorder = (*Recorder)(nil)` 静的アサーション（ポート契約充足）
- 5.2 — `recorder.go` は変換＋委譲のみで再試行・抑制・フィルタリングロジック不在（コードレビュー）
- 5.3 — 起動時 DI（`main.go`）で interim logger でなくアダプタ注入 / `go build ./...` exit 0
- NFR 1.1 — `service.go` 未変更（ユースケース本体の成功・失敗判定不変）
- NFR 1.2 — `EventType` 定数の固定文字列値 / `TestEventTypeConstants_FixedStringValues`
- NFR 2.1 — 機密非含有の構造化ログは既存 audit Service が担う（`warnPersistFailure` 等）

## Findings

なし

## Summary

全 numeric AC（Req 1〜5 / NFR 1〜2）が新規アダプタ・DI 配線・既存 audit Service 実装のいずれかで観測可能にカバーされ、対応ユニットテストも 1:1 で追加されている。`go build ./...`（exit 0）と `internal/tenantaudit` / `internal/tenant` テストが green。差分は `main.go` 配線・新規 `tenantaudit` パッケージ・spec docs に限定され、`service.go` / audit ロジック / interim logger 撤去等の Out of Scope へは波及していない。3 カテゴリいずれにも該当する問題なし。

RESULT: approve
