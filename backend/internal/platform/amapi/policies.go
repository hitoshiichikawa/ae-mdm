package amapi

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	androidmanagement "google.golang.org/api/androidmanagement/v1"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// UpsertPolicy は AMAPI の enterprises.policies.patch を呼び出す（Req 1.3）。
//
// AMAPI の patch は「存在しなければ作成、存在すれば全フィールド更新」のセマンティクスで動き、
// MVP では本ラッパもこれを upsert として透過する（umbrella #24 design.md「AMAPI Client」節）。
// body.Raw は AMAPI Policy の JSON 表現として encode / decode してから SDK 型へ流し込む。
func (c *realClient) UpsertPolicy(ctx context.Context, enterpriseName, policyName string, body PolicyBody) error {
	if err := requireEnterpriseName(enterpriseName); err != nil {
		return err
	}
	if policyName == "" {
		return pkgerrors.New(pkgerrors.CodeInvalidRequest, "policyName is required")
	}

	policy, err := convertRawToPolicy(body.Raw)
	if err != nil {
		return err
	}
	resourceName := fmt.Sprintf("%s/policies/%s", enterpriseName, policyName)
	return c.doWithRetry(ctx, "enterprises.policies.patch", enterpriseName, func() error {
		_, doErr := c.svc.Enterprises.Policies.Patch(resourceName, policy).Context(ctx).Do()
		return doErr
	})
}

// GetPolicy は AMAPI の enterprises.policies.get を呼び出す（Req 1.3）。
func (c *realClient) GetPolicy(ctx context.Context, enterpriseName, policyName string) (PolicyBody, error) {
	if err := requireEnterpriseName(enterpriseName); err != nil {
		return PolicyBody{}, err
	}
	if policyName == "" {
		return PolicyBody{}, pkgerrors.New(pkgerrors.CodeInvalidRequest, "policyName is required")
	}

	resourceName := fmt.Sprintf("%s/policies/%s", enterpriseName, policyName)
	var out *androidmanagement.Policy
	err := c.doWithRetry(ctx, "enterprises.policies.get", enterpriseName, func() error {
		var doErr error
		out, doErr = c.svc.Enterprises.Policies.Get(resourceName).Context(ctx).Do()
		return doErr
	})
	if err != nil {
		return PolicyBody{}, err
	}
	if out == nil {
		return PolicyBody{}, pkgerrors.New(pkgerrors.CodeUpstream, "amapi returned empty Policy")
	}
	raw, err := convertPolicyToRaw(out)
	if err != nil {
		return PolicyBody{}, err
	}
	return PolicyBody{
		Name:    out.Name,
		Version: out.Version,
		Raw:     raw,
	}, nil
}

// convertRawToPolicy は本ラッパの raw map を AMAPI SDK の Policy 型へ変換する。
// JSON marshal / unmarshal を経由することで AMAPI Policy の任意フィールドを pass-through し、
// 続いて raw map に含まれる全フィールドを `ForceSendFields` へ登録することで Google API Go
// client の `omitempty` 既定を打ち消し、`cameraDisabled:false` や空配列等の zero value も
// AMAPI patch（全フィールド更新）で確実に送信させる。
func convertRawToPolicy(raw map[string]any) (*androidmanagement.Policy, error) {
	if raw == nil {
		return &androidmanagement.Policy{}, nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, pkgerrors.Wrap(pkgerrors.CodeInvalidRequest,
			"failed to marshal PolicyBody.Raw", err)
	}
	p := &androidmanagement.Policy{}
	if err := json.Unmarshal(b, p); err != nil {
		return nil, pkgerrors.Wrap(pkgerrors.CodeInvalidRequest,
			"failed to unmarshal PolicyBody.Raw into Policy", err)
	}
	populateForceSendFields(reflect.ValueOf(p).Elem(), raw)
	return p, nil
}

// populateForceSendFields は raw map に出現したフィールドを SDK 構造体の `ForceSendFields`
// に再帰的に登録し、Google API Go client の `omitempty` 既定で zero value（false / 0 /
// 空配列）が落ちる挙動を打ち消す。patch（全フィールド更新）で raw の意図通りに送信させる
// ために必要（umbrella #24 design.md「AMAPI Client」節 / Req 1.3 の policy upsert 仕様）。
//
//nolint:gocyclo // reflect.Kind 分岐は table 風に並べた方が読みやすい
func populateForceSendFields(structVal reflect.Value, raw map[string]any) {
	if structVal.Kind() != reflect.Struct {
		return
	}
	structType := structVal.Type()
	var force []string
	for i := 0; i < structType.NumField(); i++ {
		field := structType.Field(i)
		if !field.IsExported() {
			continue
		}
		if field.Name == "ForceSendFields" || field.Name == "NullFields" {
			continue
		}
		jsonName := strings.Split(field.Tag.Get("json"), ",")[0]
		if jsonName == "" || jsonName == "-" {
			continue
		}
		rawVal, ok := raw[jsonName]
		if !ok {
			continue
		}
		force = append(force, field.Name)
		// nested struct / *struct への再帰: raw の同位置の map を辿って ForceSendFields を伝搬。
		fv := structVal.Field(i)
		nestedRaw, isMap := rawVal.(map[string]any)
		if !isMap {
			continue
		}
		switch fv.Kind() {
		case reflect.Struct:
			populateForceSendFields(fv, nestedRaw)
		case reflect.Ptr:
			if !fv.IsNil() && fv.Elem().Kind() == reflect.Struct {
				populateForceSendFields(fv.Elem(), nestedRaw)
			}
		}
	}
	if len(force) == 0 {
		return
	}
	fsf := structVal.FieldByName("ForceSendFields")
	if !fsf.IsValid() || !fsf.CanSet() || fsf.Kind() != reflect.Slice {
		return
	}
	existing, _ := fsf.Interface().([]string)
	fsf.Set(reflect.ValueOf(append(existing, force...)))
}

// convertPolicyToRaw は AMAPI SDK の Policy 型を本ラッパの raw map に変換する。
func convertPolicyToRaw(p *androidmanagement.Policy) (map[string]any, error) {
	if p == nil {
		return nil, nil
	}
	b, err := json.Marshal(p)
	if err != nil {
		return nil, pkgerrors.Wrap(pkgerrors.CodeUpstream,
			"failed to marshal AMAPI Policy", err)
	}
	out := map[string]any{}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, pkgerrors.Wrap(pkgerrors.CodeUpstream,
			"failed to unmarshal AMAPI Policy into raw map", err)
	}
	return out, nil
}
