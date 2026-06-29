package policy

import (
	"github.com/hitoshiichikawa/ae-mdm/internal/platform/amapi"
)

// 本ファイルは Policy Service が用いる「Raw JSON(map[string]any) ↔ Validator ドメイン型
// (PolicyInput)」の変換層と、検証通過後に AMAPI へ pass-through する PolicyBody 組立 helper を
// 提供する（design.md「policy mapper」節 / Req 2.1 / 2.3 / NFR 1.1）。
//
// 設計方針（design 確認事項 4 推奨案）:
//   - Validator が検証する 5 領域（applications 件数 / passwordMinimumLength /
//     encryptionPolicy・passwordQuality / systemUpdate / kiosk）のみを抽出する。
//   - 抽出元キーが存在しない領域は PolicyInput のポインタを nil のままにし、Validator の
//     「nil 領域は検証 skip（部分更新許容）」契約に委ねる。
//   - 型不整合・必須キー欠落で変換不能なケースは invalid field 相当の ValidationError
//     （Kind: KindInvalidField）に写像し、Service が Validate と同じ経路で 400 に倒す。
//   - 残りの AMAPI フィールドは strongly-typed 化せず raw body のまま pass-through する。

// AMAPI Policy 本体 JSON の抽出元キー（design「policy mapper」節）。マジック文字列を
// 意味のある名前で共有する（CLAUDE.md コード規約）。
const (
	keyApplications     = "applications"
	keyEncryptionPolicy = "encryptionPolicy"
	keyPasswordPolicies = "passwordPolicies"
	keySystemUpdate     = "systemUpdate"

	keyInstallType       = "installType"
	keyPackageName       = "packageName"
	keyPwdMinimumLength  = "passwordMinimumLength"
	keyPasswordQuality   = "passwordQuality"
	keySystemUpdateType  = "type"
	keyStartMinutes      = "startMinutes"
	keyEndMinutes        = "endMinutes"
	installTypeKioskMark = "KIOSK"
)

// RawToPolicyInput は AMAPI Policy 本体 JSON（map[string]any）を Validator の PolicyInput へ
// 変換する（Req 2.1 / 2.3）。
//
// 各領域は抽出元キーが存在する場合のみ PolicyInput の対応ポインタを設定する（nil = 検証 skip）。
// 型不整合（数値であるべき項目が文字列 / オブジェクトであるべき項目が配列等）は変換不能として
// invalid field 相当の ValidationError に収集し、Service が 400 へ写像する（design 確認事項 4）。
// 返り値の ValidationError には raw body の生値（機密値）を載せない（Req 5.4 / NFR 3.2）。
func RawToPolicyInput(raw map[string]any) (PolicyInput, []ValidationError) {
	var in PolicyInput
	var errs []ValidationError

	mapApplications(raw, &in, &errs)
	mapPasswordAndSecurity(raw, &in, &errs)
	mapSystemUpdate(raw, &in, &errs)

	return in, errs
}

// mapApplications は applications 配列から App 件数と Kiosk パッケージ名集合を抽出する。
// applications が配列でない場合は型不整合として invalid field を収集する。
func mapApplications(raw map[string]any, in *PolicyInput, errs *[]ValidationError) {
	rawApps, ok := raw[keyApplications]
	if !ok {
		return
	}
	apps, ok := rawApps.([]any)
	if !ok {
		*errs = append(*errs, invalidField(DomainApp, keyApplications, "applications は配列である必要があります"))
		return
	}

	in.App = &AppPolicy{AppCount: len(apps)}

	// Kiosk: installType == "KIOSK" のアプリの packageName を Kiosk パッケージ名として収集する。
	// 配列要素がオブジェクトでない / packageName が文字列でない場合は invalid field とする。
	var kioskNames []string
	hasKiosk := false
	for i, rawApp := range apps {
		app, ok := rawApp.(map[string]any)
		if !ok {
			*errs = append(*errs, invalidField(DomainApp, keyApplications, "applications の要素はオブジェクトである必要があります"))
			continue
		}
		installType, _ := app[keyInstallType].(string)
		if installType != installTypeKioskMark {
			continue
		}
		hasKiosk = true
		name, ok := app[keyPackageName].(string)
		if !ok {
			*errs = append(*errs, invalidField(DomainKiosk, kioskFieldAt(i), "Kiosk アプリの packageName は文字列である必要があります"))
			continue
		}
		kioskNames = append(kioskNames, name)
	}
	if hasKiosk {
		in.Kiosk = &KioskPolicy{PackageNames: kioskNames}
	}
}

// mapPasswordAndSecurity は encryptionPolicy（top-level）と passwordPolicies[0] の
// passwordMinimumLength / passwordQuality を Password / Security 領域へ写像する。
func mapPasswordAndSecurity(raw map[string]any, in *PolicyInput, errs *[]ValidationError) {
	// encryptionPolicy（top-level 文字列）。存在すれば Security を初期化する。
	if rawEnc, ok := raw[keyEncryptionPolicy]; ok {
		enc, ok := rawEnc.(string)
		if !ok {
			*errs = append(*errs, invalidField(DomainSecurity, "EncryptionPolicy", "encryptionPolicy は文字列である必要があります"))
		} else {
			ensureSecurity(in).EncryptionPolicy = enc
		}
	}

	// passwordPolicies は配列。MVP では先頭要素から minimumLength / passwordQuality を抽出する。
	rawPwd, ok := raw[keyPasswordPolicies]
	if !ok {
		return
	}
	policies, ok := rawPwd.([]any)
	if !ok {
		*errs = append(*errs, invalidField(DomainPassword, keyPasswordPolicies, "passwordPolicies は配列である必要があります"))
		return
	}
	if len(policies) == 0 {
		return
	}
	first, ok := policies[0].(map[string]any)
	if !ok {
		*errs = append(*errs, invalidField(DomainPassword, keyPasswordPolicies, "passwordPolicies の要素はオブジェクトである必要があります"))
		return
	}

	if rawLen, ok := first[keyPwdMinimumLength]; ok {
		n, ok := toInt(rawLen)
		if !ok {
			*errs = append(*errs, invalidField(DomainPassword, "MinimumLength", "passwordMinimumLength は整数である必要があります"))
		} else {
			in.Password = &PasswordPolicy{MinimumLength: n}
		}
	}
	if rawQuality, ok := first[keyPasswordQuality]; ok {
		quality, ok := rawQuality.(string)
		if !ok {
			*errs = append(*errs, invalidField(DomainSecurity, "PasswordQuality", "passwordQuality は文字列である必要があります"))
		} else {
			ensureSecurity(in).PasswordQuality = quality
		}
	}
}

// mapSystemUpdate は systemUpdate オブジェクトを SystemUpdate 領域へ写像する。
func mapSystemUpdate(raw map[string]any, in *PolicyInput, errs *[]ValidationError) {
	rawSU, ok := raw[keySystemUpdate]
	if !ok {
		return
	}
	su, ok := rawSU.(map[string]any)
	if !ok {
		*errs = append(*errs, invalidField(DomainSystemUpdate, keySystemUpdate, "systemUpdate はオブジェクトである必要があります"))
		return
	}

	policy := &SystemUpdatePolicy{}
	if rawType, ok := su[keySystemUpdateType]; ok {
		t, ok := rawType.(string)
		if !ok {
			*errs = append(*errs, invalidField(DomainSystemUpdate, "Type", "systemUpdate.type は文字列である必要があります"))
		} else {
			policy.Type = t
		}
	}
	if rawStart, ok := su[keyStartMinutes]; ok {
		n, ok := toInt(rawStart)
		if !ok {
			*errs = append(*errs, invalidField(DomainSystemUpdate, "StartMinutes", "systemUpdate.startMinutes は整数である必要があります"))
		} else {
			policy.StartMinutes = n
		}
	}
	if rawEnd, ok := su[keyEndMinutes]; ok {
		n, ok := toInt(rawEnd)
		if !ok {
			*errs = append(*errs, invalidField(DomainSystemUpdate, "EndMinutes", "systemUpdate.endMinutes は整数である必要があります"))
		} else {
			policy.EndMinutes = n
		}
	}
	in.SystemUpdate = policy
}

// ensureSecurity は Security 領域ポインタを遅延初期化して返す（encryptionPolicy /
// passwordQuality いずれかが存在する場合のみ Security を作る）。
func ensureSecurity(in *PolicyInput) *SecurityPolicy {
	if in.Security == nil {
		in.Security = &SecurityPolicy{}
	}
	return in.Security
}

// toInt は JSON decode 由来の数値（float64）/ 整数を int へ変換する。JSON 標準 decode は
// 数値を float64 にするため float64 を主に扱うが、int 系も許容する。小数部を持つ float64 や
// 非数値型は変換不能（false）とする。
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		if n != float64(int(n)) {
			return 0, false
		}
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	default:
		return 0, false
	}
}

// invalidField は型不整合 / 必須キー欠落を invalid field 相当の ValidationError に写像する
// helper（Kind: KindInvalidField → 400）。Message には機密値（raw body の生値）を載せない。
func invalidField(domain Domain, field, message string) ValidationError {
	return ValidationError{
		Domain:  domain,
		Field:   field,
		Kind:    KindInvalidField,
		Message: message,
	}
}

// kioskFieldAt は不正な Kiosk アプリのフィールド名を applications 配列添字付きで返す
// （machine-readable に当該要素を特定できるようにする / validator.go の kioskField と整合）。
func kioskFieldAt(index int) string {
	return "applications[" + itoa(index) + "]"
}

// itoa は小さな非負添字を文字列化する軽量 helper（strconv 依存を避ける範囲の用途）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// BuildPolicyBody は検証通過後の raw body を AMAPI PolicyBody（Name + Raw）へ pass-through で
// 組み立てる（Req 2.1 / NFR 1.1）。
//
// raw body は strongly-typed 化せずそのまま PolicyBody.Raw に渡す。AMAPI Client (#34) 側で
// amapi/policies.go の ForceSendFields 機構により zero value（false / 0 / 空配列）も含めて
// patch（全フィールド更新）で送信される。NFR 1.1 の minimumApiLevel 等の追加フィールドも
// 本 pass-through 経由で透過する（mapper は 5 領域抽出のみで raw body を加工しない）。
func BuildPolicyBody(amapiPolicyName string, raw map[string]any) amapi.PolicyBody {
	return amapi.PolicyBody{
		Name: amapiPolicyName,
		Raw:  raw,
	}
}
