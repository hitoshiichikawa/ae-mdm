package amapi

import (
	"context"

	androidmanagement "google.golang.org/api/androidmanagement/v1"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// CreateWebToken は AMAPI の enterprises.webTokens.create を呼び出す（Req 1.7）。
//
// 戻り値の WebToken.Value（iframe 埋め込み時に使う短命 token）は秘密値であり、構造化ログには
// 出さない（NFR 1.2）。
func (c *realClient) CreateWebToken(ctx context.Context, enterpriseName string, parentFrameURL string) (WebToken, error) {
	if err := requireEnterpriseName(enterpriseName); err != nil {
		return WebToken{}, err
	}
	if parentFrameURL == "" {
		return WebToken{}, pkgerrors.New(pkgerrors.CodeInvalidRequest,
			"parentFrameURL is required")
	}

	body := &androidmanagement.WebToken{
		ParentFrameUrl: parentFrameURL,
	}
	var out *androidmanagement.WebToken
	err := c.doWithRetry(ctx, "enterprises.webTokens.create", enterpriseName, func() error {
		var doErr error
		out, doErr = c.svc.Enterprises.WebTokens.Create(enterpriseName, body).Context(ctx).Do()
		return doErr
	})
	if err != nil {
		return WebToken{}, err
	}
	if out == nil {
		return WebToken{}, pkgerrors.New(pkgerrors.CodeUpstream, "amapi returned empty WebToken")
	}
	return WebToken{
		Name:  out.Name,
		Value: out.Value,
	}, nil
}
