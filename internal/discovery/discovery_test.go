package discovery

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/router-for-me/cliproxy-cursor-plugin/internal/account"
	"github.com/router-for-me/cliproxy-cursor-plugin/internal/cursorpb/gen"
	"github.com/router-for-me/cliproxy-cursor-plugin/internal/cursorpb/gen/genconnect"
)

// fakeAgentServiceClient implements genconnect.AgentServiceClient with a
// scriptable GetUsableModels behavior, so this test exercises the real
// normalizeModel translation logic without a live Cursor backend.
type fakeAgentServiceClient struct {
	getUsableModelsFunc func(ctx context.Context, req *connect.Request[gen.GetUsableModelsRequest]) (*connect.Response[gen.GetUsableModelsResponse], error)
}

func (f *fakeAgentServiceClient) Run(ctx context.Context, req *connect.Request[gen.AgentClientMessage]) (*connect.Response[gen.AgentServerMessage], error) {
	return nil, errors.New("not used in this test")
}
func (f *fakeAgentServiceClient) RunSSE(ctx context.Context, req *connect.Request[gen.BidiRequestId]) (*connect.Response[gen.AgentServerMessage], error) {
	return nil, errors.New("not used in this test")
}
func (f *fakeAgentServiceClient) NameAgent(ctx context.Context, req *connect.Request[gen.NameAgentRequest]) (*connect.Response[gen.NameAgentResponse], error) {
	return nil, errors.New("not used in this test")
}
func (f *fakeAgentServiceClient) GetUsableModels(ctx context.Context, req *connect.Request[gen.GetUsableModelsRequest]) (*connect.Response[gen.GetUsableModelsResponse], error) {
	return f.getUsableModelsFunc(ctx, req)
}
func (f *fakeAgentServiceClient) GetDefaultModelForCli(ctx context.Context, req *connect.Request[gen.GetDefaultModelForCliRequest]) (*connect.Response[gen.GetDefaultModelForCliResponse], error) {
	return nil, errors.New("not used in this test")
}
func (f *fakeAgentServiceClient) GetAllowedModelIntents(ctx context.Context, req *connect.Request[gen.GetAllowedModelIntentsRequest]) (*connect.Response[gen.GetAllowedModelIntentsResponse], error) {
	return nil, errors.New("not used in this test")
}

var _ genconnect.AgentServiceClient = (*fakeAgentServiceClient)(nil)

func TestModelsForAuth_NormalizesGetUsableModelsResponse(t *testing.T) {
	fake := &fakeAgentServiceClient{
		getUsableModelsFunc: func(ctx context.Context, req *connect.Request[gen.GetUsableModelsRequest]) (*connect.Response[gen.GetUsableModelsResponse], error) {
			return connect.NewResponse(&gen.GetUsableModelsResponse{
				Models: []*gen.ModelDetails{
					{ModelId: "cursor-fast", DisplayName: "Cursor Fast"},
					{ModelId: "cursor-max", DisplayModelId: "Cursor Max (display id fallback)"},
				},
			}), nil
		},
	}

	accounts := account.NewStore()
	accounts.Set("cursor", account.State{
		AccessToken: "test-access-token",
		ExpiresAt:   time.Now().Add(time.Hour),
		Status:      account.StatusActive,
	})

	discoverer := NewDiscoverer(fake, accounts)

	resp, err := discoverer.ModelsForAuth(context.Background(), pluginapi.AuthModelRequest{AuthID: "cursor"})
	if err != nil {
		t.Fatalf("ModelsForAuth failed: %v", err)
	}
	if resp.Provider != "cursor" {
		t.Errorf("provider = %q, want cursor", resp.Provider)
	}
	if len(resp.Models) != 2 {
		t.Fatalf("expected 2 normalized models, got %d", len(resp.Models))
	}
	if resp.Models[0].ID != "cursor-fast" || resp.Models[0].DisplayName != "Cursor Fast" {
		t.Errorf("model[0] = %+v, want id=cursor-fast displayName=Cursor Fast", resp.Models[0])
	}
	if resp.Models[1].ID != "cursor-max" || resp.Models[1].DisplayName != "Cursor Max (display id fallback)" {
		t.Errorf("model[1] = %+v, want DisplayModelId fallback used as DisplayName", resp.Models[1])
	}
	for _, m := range resp.Models {
		if m.OwnedBy != "cursor" || m.Object != "model" {
			t.Errorf("model %+v missing expected OwnedBy/Object defaults", m)
		}
	}
}

func TestModelsForAuth_DegradedAccount_FailsFastWithoutCallingCursor(t *testing.T) {
	called := false
	fake := &fakeAgentServiceClient{
		getUsableModelsFunc: func(ctx context.Context, req *connect.Request[gen.GetUsableModelsRequest]) (*connect.Response[gen.GetUsableModelsResponse], error) {
			called = true
			return nil, errors.New("should not be called")
		},
	}

	accounts := account.NewStore()
	accounts.Set("cursor", account.State{Status: account.StatusActive})
	accounts.MarkDegraded("cursor", account.StatusNeedsReauth, "refresh token rejected")

	discoverer := NewDiscoverer(fake, accounts)

	_, err := discoverer.ModelsForAuth(context.Background(), pluginapi.AuthModelRequest{AuthID: "cursor"})
	if err == nil {
		t.Fatalf("expected ModelsForAuth to fail fast on a degraded account")
	}
	if called {
		t.Errorf("expected GetUsableModels to never be called for a degraded account, but it was called")
	}
}
