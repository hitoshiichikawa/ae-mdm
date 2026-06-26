package amapi

import (
	"context"
	"fmt"

	androidmanagement "google.golang.org/api/androidmanagement/v1"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// ListDevices は AMAPI の enterprises.devices.list を呼び出す（Req 1.4）。
//
// AMAPI のページング（nextPageToken）は本ラッパ内部で連結し、上位には全件結合済みの
// []Device を返す。pageSize は AMAPI の既定（100）を採用。
func (c *realClient) ListDevices(ctx context.Context, enterpriseName string) ([]Device, error) {
	if err := requireEnterpriseName(enterpriseName); err != nil {
		return nil, err
	}

	var all []Device
	pageToken := ""
	for {
		var resp *androidmanagement.ListDevicesResponse
		token := pageToken
		err := c.doWithRetry(ctx, "enterprises.devices.list", enterpriseName, func() error {
			call := c.svc.Enterprises.Devices.List(enterpriseName).Context(ctx)
			if token != "" {
				call = call.PageToken(token)
			}
			var doErr error
			resp, doErr = call.Do()
			return doErr
		})
		if err != nil {
			return nil, err
		}
		if resp == nil {
			return nil, pkgerrors.New(pkgerrors.CodeUpstream,
				"amapi returned empty ListDevicesResponse")
		}
		for _, d := range resp.Devices {
			all = append(all, deviceFromSDK(d))
		}
		if resp.NextPageToken == "" {
			break
		}
		pageToken = resp.NextPageToken
	}
	return all, nil
}

// GetDevice は AMAPI の enterprises.devices.get を呼び出す（Req 1.4）。
//
// deviceID は AMAPI の最終セグメント（"{deviceId}"）を受け取る。本ラッパ内で
// enterpriseName と組み合わせてリソースパスを組み立てる。
func (c *realClient) GetDevice(ctx context.Context, enterpriseName, deviceID string) (Device, error) {
	if err := requireEnterpriseName(enterpriseName); err != nil {
		return Device{}, err
	}
	if deviceID == "" {
		return Device{}, pkgerrors.New(pkgerrors.CodeInvalidRequest, "deviceID is required")
	}
	resourceName := fmt.Sprintf("%s/devices/%s", enterpriseName, deviceID)
	var out *androidmanagement.Device
	err := c.doWithRetry(ctx, "enterprises.devices.get", enterpriseName, func() error {
		var doErr error
		out, doErr = c.svc.Enterprises.Devices.Get(resourceName).Context(ctx).Do()
		return doErr
	})
	if err != nil {
		return Device{}, err
	}
	if out == nil {
		return Device{}, pkgerrors.New(pkgerrors.CodeUpstream, "amapi returned empty Device")
	}
	return deviceFromSDK(out), nil
}

// IssueCommand は AMAPI の enterprises.devices.issueCommand を呼び出す（Req 1.5）。
//
// 戻り値の commandID は AMAPI が払い出した Operation 名（"enterprises/.../operations/{id}"）の
// 末尾 ID 部分のみを返す方針も検討したが、MVP では Operation 名全体をそのまま返して
// domain Service 側で解釈させる（呼び出し側ドメインの責務）。
func (c *realClient) IssueCommand(ctx context.Context, enterpriseName, deviceID string, cmd CommandRequest) (string, error) {
	if err := requireEnterpriseName(enterpriseName); err != nil {
		return "", err
	}
	if deviceID == "" {
		return "", pkgerrors.New(pkgerrors.CodeInvalidRequest, "deviceID is required")
	}
	if cmd.Type == "" {
		return "", pkgerrors.New(pkgerrors.CodeInvalidRequest, "CommandRequest.Type is required")
	}
	resourceName := fmt.Sprintf("%s/devices/%s", enterpriseName, deviceID)
	body := &androidmanagement.Command{
		Type:        cmd.Type,
		Duration:    cmd.Duration,
		NewPassword: cmd.NewPassword,
	}
	var op *androidmanagement.Operation
	err := c.doWithRetry(ctx, "enterprises.devices.issueCommand", enterpriseName, func() error {
		var doErr error
		op, doErr = c.svc.Enterprises.Devices.IssueCommand(resourceName, body).Context(ctx).Do()
		return doErr
	})
	if err != nil {
		return "", err
	}
	if op == nil {
		return "", pkgerrors.New(pkgerrors.CodeUpstream, "amapi returned empty Operation")
	}
	return op.Name, nil
}

// deviceFromSDK は AMAPI SDK の Device を本ラッパ独自型に正規化する。
func deviceFromSDK(d *androidmanagement.Device) Device {
	if d == nil {
		return Device{}
	}
	return Device{
		Name:                 d.Name,
		State:                d.AppliedState,
		PolicyName:           d.AppliedPolicyName,
		LastStatusReportTime: d.LastStatusReportTime,
	}
}
