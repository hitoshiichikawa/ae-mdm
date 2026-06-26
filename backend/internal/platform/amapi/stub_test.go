package amapi

import (
	"context"
	stdErrors "errors"
	"testing"

	pkgerrors "github.com/hitoshiichikawa/ae-mdm/internal/errors"
)

func TestStubClient_RecordsCallsAndUsesHooks(t *testing.T) {
	// Arrange
	want := EnrollmentToken{Name: "enterprises/X/enrollmentTokens/t1", Value: "secret"}
	s := &StubClient{
		OnCreateEnrollmentToken: func(ctx context.Context, enterpriseName string, req EnrollmentTokenRequest) (EnrollmentToken, error) {
			return want, nil
		},
	}

	// Act
	got, err := s.CreateEnrollmentToken(context.Background(), "enterprises/X", EnrollmentTokenRequest{
		Duration: "3600s",
	})

	// Assert
	if err != nil {
		t.Fatalf("CreateEnrollmentToken = %v", err)
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if s.CallCount("CreateEnrollmentToken") != 1 {
		t.Fatalf("CallCount = %d, want 1", s.CallCount("CreateEnrollmentToken"))
	}
	if s.Calls[0].EnterpriseName != "enterprises/X" {
		t.Fatalf("Calls[0].EnterpriseName = %q", s.Calls[0].EnterpriseName)
	}
}

func TestStubClient_EnforcesEnterpriseNameGuard(t *testing.T) {
	// Arrange
	s := &StubClient{
		OnGetEnterprise: func(ctx context.Context, enterpriseName string) (Enterprise, error) {
			t.Errorf("hook should not be called when guard rejects")
			return Enterprise{}, nil
		},
	}

	// Act
	_, err := s.GetEnterprise(context.Background(), "")

	// Assert: stub も本番と同じ contract で空 enterpriseName を拒否
	if err == nil {
		t.Fatalf("expected error")
	}
	var de *pkgerrors.Error
	if !stdErrors.As(err, &de) || de.Code != pkgerrors.CodeInvalidRequest {
		t.Fatalf("want CodeInvalidRequest, got %v", err)
	}
}

func TestStubClient_NoHookReturnsZeroValue(t *testing.T) {
	// Arrange
	s := &StubClient{}

	// Act
	url, name, err := s.CreateSignupURL(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if url != "" || name != "" {
		t.Fatalf("expected zero values, got url=%q name=%q", url, name)
	}
	if s.CallCount("CreateSignupURL") != 1 {
		t.Fatalf("CallCount = %d", s.CallCount("CreateSignupURL"))
	}
}

func TestStubClient_HookReturnsErrorPropagates(t *testing.T) {
	// Arrange
	wantErr := pkgerrors.New(pkgerrors.CodeForbidden, "stubbed denial")
	s := &StubClient{
		OnIssueCommand: func(ctx context.Context, enterpriseName, deviceID string, cmd CommandRequest) (string, error) {
			return "", wantErr
		},
	}

	// Act
	_, err := s.IssueCommand(context.Background(), "enterprises/X", "D1", CommandRequest{Type: "LOCK"})

	// Assert
	if err == nil {
		t.Fatalf("expected error")
	}
	if !stdErrors.Is(err, wantErr) {
		t.Fatalf("errors.Is(err, wantErr) = false, want true")
	}
}

func TestStubClient_EnforcesPerMethodGuards(t *testing.T) {
	// stub も real client と同じ contract を守る（domain Service テストで本番との挙動差を出さない）。
	cases := []struct {
		name string
		call func(s *StubClient) error
	}{
		{"UpsertPolicy_emptyPolicyName", func(s *StubClient) error {
			return s.UpsertPolicy(context.Background(), "enterprises/X", "", PolicyBody{})
		}},
		{"GetPolicy_emptyPolicyName", func(s *StubClient) error {
			_, err := s.GetPolicy(context.Background(), "enterprises/X", "")
			return err
		}},
		{"GetDevice_emptyDeviceID", func(s *StubClient) error {
			_, err := s.GetDevice(context.Background(), "enterprises/X", "")
			return err
		}},
		{"IssueCommand_emptyDeviceID", func(s *StubClient) error {
			_, err := s.IssueCommand(context.Background(), "enterprises/X", "", CommandRequest{Type: "LOCK"})
			return err
		}},
		{"IssueCommand_emptyType", func(s *StubClient) error {
			_, err := s.IssueCommand(context.Background(), "enterprises/X", "D1", CommandRequest{Type: ""})
			return err
		}},
		{"CreateWebToken_emptyParentFrameURL", func(s *StubClient) error {
			_, err := s.CreateWebToken(context.Background(), "enterprises/X", "")
			return err
		}},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			err := c.call(&StubClient{})
			if err == nil {
				t.Fatalf("expected CodeInvalidRequest, got nil")
			}
			var de *pkgerrors.Error
			if !stdErrors.As(err, &de) || de.Code != pkgerrors.CodeInvalidRequest {
				t.Fatalf("want CodeInvalidRequest, got %v", err)
			}
		})
	}
}

func TestStubClient_RecordsAllMethods(t *testing.T) {
	// Arrange
	s := &StubClient{}
	ctx := context.Background()

	// Act: 各メソッドを 1 回ずつ呼ぶ
	_, _, _ = s.CreateSignupURL(ctx)
	_, _ = s.CreateEnterprise(ctx, "signupUrls/x", "p")
	_, _ = s.GetEnterprise(ctx, "enterprises/X")
	_ = s.UpsertPolicy(ctx, "enterprises/X", "default", PolicyBody{})
	_, _ = s.GetPolicy(ctx, "enterprises/X", "default")
	_, _ = s.ListDevices(ctx, "enterprises/X")
	_, _ = s.GetDevice(ctx, "enterprises/X", "D1")
	_, _ = s.IssueCommand(ctx, "enterprises/X", "D1", CommandRequest{Type: "LOCK"})
	_, _ = s.CreateEnrollmentToken(ctx, "enterprises/X", EnrollmentTokenRequest{})
	_, _ = s.CreateWebToken(ctx, "enterprises/X", "https://x")

	// Assert
	if len(s.Calls) != 10 {
		t.Fatalf("Calls = %d, want 10", len(s.Calls))
	}
	wantMethods := []string{
		"CreateSignupURL", "CreateEnterprise", "GetEnterprise", "UpsertPolicy", "GetPolicy",
		"ListDevices", "GetDevice", "IssueCommand", "CreateEnrollmentToken", "CreateWebToken",
	}
	for i, m := range wantMethods {
		if s.Calls[i].Method != m {
			t.Fatalf("Calls[%d].Method = %q, want %q", i, s.Calls[i].Method, m)
		}
	}
}
