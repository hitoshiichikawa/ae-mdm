// Package enrollment は ae-mdm の端末エンロール（B1 / Issue #7）のドメイン層を提供する。
//
// TenantAdmin / Operator が業務端末を QR コードで AMAPI 管理下に置くための
// (1) エンロールメントトークン発行（Fully Managed / Dedicated モード）と QR 表示用データ生成、
// (2) ENROLLMENT 通知を契機とした発行元テナントの端末インベントリ登録、
// を担う。umbrella #24 Requirement 3 のうち enrollment 実装単位を切り出したものである。
//
// # レイヤ構成（design.md File Structure Plan）
//
//   - doc.go          : 本 package ドキュメント（依存方向ルール）
//   - types.go        : Mode enum・DTO（IssueRequest / TokenView / TokenSummary / TokenRow）・
//     AdditionalData 値オブジェクト + marshal helper・sentinel error・
//     依存倒置ポート（enterpriseResolver / policyChecker / eventRecorder）
//   - repository.go   : TokenRepository（enrollment_tokens の Insert / List、tenant-scoped RLS）
//   - service.go      : Service（IssueToken / ListTokens）。検証→policy 検証→enterprise 解決→
//     AMAPI 発行→snapshot 永続化→監査記録（後続 task）
//   - registration.go : Registrar（devices への冪等 upsert）（後続 task）
//   - handler.go      : /api/enrollment-tokens Handler（POST 発行 / GET 一覧）+ authz（後続 task）
//
// # 秘密値の非記録（NFR 3.1）
//
// AMAPI が払い出す EnrollmentToken.Value / QRCode は秘密値であり、snapshot 永続化列を持たず、
// 監査 Detail・構造化ログにも載せない。HTTP 応答（TokenView）で発行時に一度だけ返す。
//
// # 依存方向ルール
//
// 本 package が import してよいのは以下のみ。他 domain（tenant / notification 等）および cmd への
// 直接 import は行わず、cross-domain 参照は consumer-defines-interface（primitive 型ポート）で
// 依存倒置する（policy/doc.go を手本 / notification は enrollment.Registrar を structural typing で満たす）:
//
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/platform/amapi（トークン発行の最小 port / Service）
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/platform/db（tenant-scoped tx / RLS）
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/audit（監査記録の最小 port が参照する Event 型）
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/logger（拒否・不整合経路の構造化ログ）
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/errors（Code 付きエラー写像）
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/config（設定値の参照）
//   - 許可: github.com/hitoshiichikawa/ae-mdm/internal/platform/authz / .../httpserver（own-tenant RBAC / claims 取得 / Handler のみ）
//   - 禁止: 他 domain（tenant / notification / policy 等）の struct への直接 import / cmd への import
package enrollment
