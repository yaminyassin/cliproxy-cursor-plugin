// Package discovery implements the model_registrar/model_provider
// capability: host-conformant dynamic Cursor model discovery via
// GetUsableModels, normalized into pluginapi.ModelInfo entries, per
// fact-r6-model-refresh-policy.
package discovery

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/router-for-me/cliproxy-cursor-plugin/internal/account"
	"github.com/router-for-me/cliproxy-cursor-plugin/internal/cursorpb/gen"
	"github.com/router-for-me/cliproxy-cursor-plugin/internal/cursorpb/gen/genconnect"
)

// Discoverer implements dynamic Cursor model discovery.
type Discoverer struct {
	Service  genconnect.AgentServiceClient
	Accounts *account.Store
}

// NewDiscoverer creates a Discoverer backed by the given Connect-RPC
// service client and account store.
func NewDiscoverer(service genconnect.AgentServiceClient, accounts *account.Store) *Discoverer {
	return &Discoverer{Service: service, Accounts: accounts}
}

// ModelsForAuth implements pluginapi.ModelProvider.ModelsForAuth: fetches
// the current usable-model catalog for the given account and normalizes
// it into pluginapi.ModelInfo entries.
func (d *Discoverer) ModelsForAuth(ctx context.Context, req pluginapi.AuthModelRequest) (pluginapi.ModelResponse, error) {
	accountState, err := d.Accounts.Get(req.AuthID)
	if err != nil {
		return pluginapi.ModelResponse{}, fmt.Errorf("cursor: model discovery requires an active account: %w", err)
	}

	connectReq := connect.NewRequest(&gen.GetUsableModelsRequest{})
	connectReq.Header().Set("authorization", "Bearer "+accountState.AccessToken)

	resp, errCall := d.Service.GetUsableModels(ctx, connectReq)
	if errCall != nil {
		return pluginapi.ModelResponse{}, fmt.Errorf("cursor: GetUsableModels failed: %w", errCall)
	}

	models := make([]pluginapi.ModelInfo, 0, len(resp.Msg.GetModels()))
	for _, m := range resp.Msg.GetModels() {
		models = append(models, normalizeModel(m))
	}

	return pluginapi.ModelResponse{Provider: "cursor", Models: models}, nil
}

// StaticModels implements pluginapi.ModelProvider.StaticModels. Cursor's
// catalog is entirely OAuth-account-scoped (fact-r6-model-refresh-policy:
// dynamic discovery mirrors the host's model_registrar pattern), so there
// is no static, account-independent model list to offer.
func (d *Discoverer) StaticModels(_ context.Context, _ pluginapi.StaticModelRequest) (pluginapi.ModelResponse, error) {
	return pluginapi.ModelResponse{Provider: "cursor", Models: nil}, nil
}

func normalizeModel(m *gen.ModelDetails) pluginapi.ModelInfo {
	if m == nil {
		return pluginapi.ModelInfo{}
	}
	displayName := m.GetDisplayName()
	if displayName == "" {
		displayName = m.GetDisplayModelId()
	}
	if displayName == "" {
		displayName = m.GetModelId()
	}
	return pluginapi.ModelInfo{
		ID:                         m.GetModelId(),
		Object:                     "model",
		OwnedBy:                    "cursor",
		DisplayName:                displayName,
		SupportedGenerationMethods: []string{"chat"},
	}
}
