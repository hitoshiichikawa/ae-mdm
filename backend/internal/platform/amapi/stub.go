package amapi

import (
	"context"
	"sync"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

// StubClient は domain Service の単体テスト用に Client interface を満たす in-memory 実装。
// Req 6.1 / 6.2 / 6.3（テスト用 stub を本ラッパと同一パッケージ境界内で提供 / 実 AMAPI 呼び出しを
// 発生させない）に対応する。
//
// 各メソッドにフック関数（On... フィールド）を差し込むことで、テストごとに任意の戻り値・error を
// 返せる。フック未設定時は録音のみ行い、デフォルト戻り値（zero value / 空文字）を返す。
//
// 内部状態（呼び出し履歴）はテスト goroutine 安全性のため Mutex で保護する。
type StubClient struct {
	mu sync.Mutex

	// Calls は受信した全呼び出しを順序付きで記録する（テストでの呼び出し回数・引数検証用）。
	Calls []StubCall

	// 各メソッドの hook 関数（任意）。未設定なら録音 + 零値の応答。
	OnCreateSignupURL       func(ctx context.Context) (signupURL, signupURLName string, err error)
	OnCreateEnterprise      func(ctx context.Context, signupURLName, projectID string) (enterpriseName string, err error)
	OnGetEnterprise         func(ctx context.Context, enterpriseName string) (Enterprise, error)
	OnUpsertPolicy          func(ctx context.Context, enterpriseName, policyName string, body PolicyBody) error
	OnGetPolicy             func(ctx context.Context, enterpriseName, policyName string) (PolicyBody, error)
	OnListDevices           func(ctx context.Context, enterpriseName string) ([]Device, error)
	OnGetDevice             func(ctx context.Context, enterpriseName, deviceID string) (Device, error)
	OnIssueCommand          func(ctx context.Context, enterpriseName, deviceID string, cmd CommandRequest) (string, error)
	OnCreateEnrollmentToken func(ctx context.Context, enterpriseName string, req EnrollmentTokenRequest) (EnrollmentToken, error)
	OnCreateWebToken        func(ctx context.Context, enterpriseName string, parentFrameURL string) (WebToken, error)
}

// StubCall は StubClient 上で発生した 1 件の呼び出し履歴を表す（テスト assertion 用）。
//
// Args は順序付き引数列を生のまま保持する（型 assertion で取り出す前提）。
type StubCall struct {
	Method         string
	EnterpriseName string
	Args           []any
}

// recordCall は呼び出し履歴を追記する（goroutine-safe）。
func (s *StubClient) recordCall(method, enterpriseName string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Calls = append(s.Calls, StubCall{Method: method, EnterpriseName: enterpriseName, Args: args})
}

// CallCount は指定メソッドの呼び出し回数を返す（テスト assertion 用 helper）。
func (s *StubClient) CallCount(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.Calls {
		if c.Method == method {
			n++
		}
	}
	return n
}

// CreateSignupURL は Client interface を満たす。
func (s *StubClient) CreateSignupURL(ctx context.Context) (string, string, error) {
	s.recordCall("CreateSignupURL", "")
	if s.OnCreateSignupURL != nil {
		return s.OnCreateSignupURL(ctx)
	}
	return "", "", nil
}

// CreateEnterprise は Client interface を満たす。
func (s *StubClient) CreateEnterprise(ctx context.Context, signupURLName, projectID string) (string, error) {
	s.recordCall("CreateEnterprise", "", signupURLName, projectID)
	if s.OnCreateEnterprise != nil {
		return s.OnCreateEnterprise(ctx, signupURLName, projectID)
	}
	return "", nil
}

// GetEnterprise は Client interface を満たす。enterpriseName 空値は CodeInvalidRequest を返す
// （本番実装 Client と同じ契約を stub にも持たせ、domain Service テストでガード違反を検出可能にする）。
func (s *StubClient) GetEnterprise(ctx context.Context, enterpriseName string) (Enterprise, error) {
	s.recordCall("GetEnterprise", enterpriseName)
	if err := requireEnterpriseName(enterpriseName); err != nil {
		return Enterprise{}, err
	}
	if s.OnGetEnterprise != nil {
		return s.OnGetEnterprise(ctx, enterpriseName)
	}
	return Enterprise{}, nil
}

// UpsertPolicy は Client interface を満たす。realClient と同じ contract で policyName 空値を
// 拒否する（domain Service テストで stub と real の挙動差を出さないため）。
func (s *StubClient) UpsertPolicy(ctx context.Context, enterpriseName, policyName string, body PolicyBody) error {
	s.recordCall("UpsertPolicy", enterpriseName, policyName, body)
	if err := requireEnterpriseName(enterpriseName); err != nil {
		return err
	}
	if policyName == "" {
		return pkgerrors.New(pkgerrors.CodeInvalidRequest, "policyName is required")
	}
	if s.OnUpsertPolicy != nil {
		return s.OnUpsertPolicy(ctx, enterpriseName, policyName, body)
	}
	return nil
}

// GetPolicy は Client interface を満たす。realClient と同じ contract で policyName 空値を拒否する。
func (s *StubClient) GetPolicy(ctx context.Context, enterpriseName, policyName string) (PolicyBody, error) {
	s.recordCall("GetPolicy", enterpriseName, policyName)
	if err := requireEnterpriseName(enterpriseName); err != nil {
		return PolicyBody{}, err
	}
	if policyName == "" {
		return PolicyBody{}, pkgerrors.New(pkgerrors.CodeInvalidRequest, "policyName is required")
	}
	if s.OnGetPolicy != nil {
		return s.OnGetPolicy(ctx, enterpriseName, policyName)
	}
	return PolicyBody{}, nil
}

// ListDevices は Client interface を満たす。
func (s *StubClient) ListDevices(ctx context.Context, enterpriseName string) ([]Device, error) {
	s.recordCall("ListDevices", enterpriseName)
	if err := requireEnterpriseName(enterpriseName); err != nil {
		return nil, err
	}
	if s.OnListDevices != nil {
		return s.OnListDevices(ctx, enterpriseName)
	}
	return nil, nil
}

// GetDevice は Client interface を満たす。realClient と同じ contract で deviceID 空値を拒否する。
func (s *StubClient) GetDevice(ctx context.Context, enterpriseName, deviceID string) (Device, error) {
	s.recordCall("GetDevice", enterpriseName, deviceID)
	if err := requireEnterpriseName(enterpriseName); err != nil {
		return Device{}, err
	}
	if deviceID == "" {
		return Device{}, pkgerrors.New(pkgerrors.CodeInvalidRequest, "deviceID is required")
	}
	if s.OnGetDevice != nil {
		return s.OnGetDevice(ctx, enterpriseName, deviceID)
	}
	return Device{}, nil
}

// IssueCommand は Client interface を満たす。realClient と同じ contract で deviceID と
// CommandRequest.Type の空値を拒否する。
func (s *StubClient) IssueCommand(ctx context.Context, enterpriseName, deviceID string, cmd CommandRequest) (string, error) {
	s.recordCall("IssueCommand", enterpriseName, deviceID, cmd)
	if err := requireEnterpriseName(enterpriseName); err != nil {
		return "", err
	}
	if deviceID == "" {
		return "", pkgerrors.New(pkgerrors.CodeInvalidRequest, "deviceID is required")
	}
	if cmd.Type == "" {
		return "", pkgerrors.New(pkgerrors.CodeInvalidRequest, "CommandRequest.Type is required")
	}
	if s.OnIssueCommand != nil {
		return s.OnIssueCommand(ctx, enterpriseName, deviceID, cmd)
	}
	return "", nil
}

// CreateEnrollmentToken は Client interface を満たす。
func (s *StubClient) CreateEnrollmentToken(ctx context.Context, enterpriseName string, req EnrollmentTokenRequest) (EnrollmentToken, error) {
	s.recordCall("CreateEnrollmentToken", enterpriseName, req)
	if err := requireEnterpriseName(enterpriseName); err != nil {
		return EnrollmentToken{}, err
	}
	if s.OnCreateEnrollmentToken != nil {
		return s.OnCreateEnrollmentToken(ctx, enterpriseName, req)
	}
	return EnrollmentToken{}, nil
}

// CreateWebToken は Client interface を満たす。realClient と同じ contract で parentFrameURL の
// 空値を拒否する。
func (s *StubClient) CreateWebToken(ctx context.Context, enterpriseName string, parentFrameURL string) (WebToken, error) {
	s.recordCall("CreateWebToken", enterpriseName, parentFrameURL)
	if err := requireEnterpriseName(enterpriseName); err != nil {
		return WebToken{}, err
	}
	if parentFrameURL == "" {
		return WebToken{}, pkgerrors.New(pkgerrors.CodeInvalidRequest, "parentFrameURL is required")
	}
	if s.OnCreateWebToken != nil {
		return s.OnCreateWebToken(ctx, enterpriseName, parentFrameURL)
	}
	return WebToken{}, nil
}

// 型 assertion 用に Client interface を満たすことを compile-time で確認する。
var _ Client = (*StubClient)(nil)

// 型 assertion 用に realClient が Client interface を満たすことを compile-time で確認する
// （per-resource ファイルでメソッドが分散するため、本ファイルでまとめて確認）。
var _ Client = (*realClient)(nil)
