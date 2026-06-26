package amapi

import (
	"context"

	androidmanagement "google.golang.org/api/androidmanagement/v1"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// CreateEnrollmentToken は AMAPI の enterprises.enrollmentTokens.create を呼び出す（Req 1.6）。
//
// 戻り値の EnrollmentToken.Value（QR / 手動入力で使う秘密値）は構造化ログには出さない（NFR 1.2）。
// 呼び出し側 domain Service は本値を audit log や永続化に乗せないこと。
func (c *realClient) CreateEnrollmentToken(ctx context.Context, enterpriseName string, req EnrollmentTokenRequest) (EnrollmentToken, error) {
	if err := requireEnterpriseName(enterpriseName); err != nil {
		return EnrollmentToken{}, err
	}

	body := &androidmanagement.EnrollmentToken{
		PolicyName:         req.PolicyName,
		Duration:           req.Duration,
		AdditionalData:     req.AdditionalData,
		AllowPersonalUsage: req.AllowPersonalUsage,
	}
	var out *androidmanagement.EnrollmentToken
	err := c.doWithRetry(ctx, "enterprises.enrollmentTokens.create", enterpriseName, func() error {
		var doErr error
		out, doErr = c.svc.Enterprises.EnrollmentTokens.Create(enterpriseName, body).Context(ctx).Do()
		return doErr
	})
	if err != nil {
		return EnrollmentToken{}, err
	}
	if out == nil {
		return EnrollmentToken{}, pkgerrors.New(pkgerrors.CodeUpstream,
			"amapi returned empty EnrollmentToken")
	}
	return EnrollmentToken{
		Name:           out.Name,
		Value:          out.Value,
		QRCode:         out.QrCode,
		ExpirationTime: out.ExpirationTimestamp,
	}, nil
}
