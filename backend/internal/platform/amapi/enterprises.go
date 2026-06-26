package amapi

import (
	"context"

	androidmanagement "google.golang.org/api/androidmanagement/v1"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// CreateSignupURL は AMAPI の signupUrls.create を呼び出す（Req 1.1）。
//
// Enterprise 確定前のフローのため enterpriseName は不要（Req 2.2）。AMAPI が払い出した
// SignupUrl の Url と Name を本ラッパ独自型として返す。
func (c *realClient) CreateSignupURL(ctx context.Context) (string, string, error) {
	var out *androidmanagement.SignupUrl
	err := c.doWithRetry(ctx, "signupUrls.create", "", func() error {
		var doErr error
		out, doErr = c.svc.SignupUrls.Create().Context(ctx).Do()
		return doErr
	})
	if err != nil {
		return "", "", err
	}
	if out == nil {
		return "", "", pkgerrors.New(pkgerrors.CodeUpstream, "amapi returned empty SignupUrl")
	}
	return out.Url, out.Name, nil
}

// CreateEnterprise は AMAPI の enterprises.create を呼び出す（Req 1.2）。
//
// signupURLName / projectID は SignupUrl から受け取った値を呼び出し側が引き継ぐ。
// 戻り値は確定した Enterprise 一意名（"enterprises/{enterpriseId}" 形式）。
func (c *realClient) CreateEnterprise(ctx context.Context, signupURLName, projectID string) (string, error) {
	var out *androidmanagement.Enterprise
	err := c.doWithRetry(ctx, "enterprises.create", "", func() error {
		var doErr error
		out, doErr = c.svc.Enterprises.Create(&androidmanagement.Enterprise{}).
			ProjectId(projectID).
			SignupUrlName(signupURLName).
			Context(ctx).
			Do()
		return doErr
	})
	if err != nil {
		return "", err
	}
	if out == nil {
		return "", pkgerrors.New(pkgerrors.CodeUpstream, "amapi returned empty Enterprise")
	}
	return out.Name, nil
}

// GetEnterprise は AMAPI の enterprises.get を呼び出す（Req 1.2）。
//
// enterpriseName が空のときは CodeInvalidRequest を返す（Req 2.3）。
func (c *realClient) GetEnterprise(ctx context.Context, enterpriseName string) (Enterprise, error) {
	if err := requireEnterpriseName(enterpriseName); err != nil {
		return Enterprise{}, err
	}
	var out *androidmanagement.Enterprise
	err := c.doWithRetry(ctx, "enterprises.get", enterpriseName, func() error {
		var doErr error
		out, doErr = c.svc.Enterprises.Get(enterpriseName).Context(ctx).Do()
		return doErr
	})
	if err != nil {
		return Enterprise{}, err
	}
	if out == nil {
		return Enterprise{}, pkgerrors.New(pkgerrors.CodeUpstream, "amapi returned empty Enterprise")
	}
	return Enterprise{
		Name:        out.Name,
		DisplayName: out.EnterpriseDisplayName,
	}, nil
}
