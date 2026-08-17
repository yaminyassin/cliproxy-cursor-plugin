package auth

import (
	"context"
	"fmt"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/router-for-me/cliproxy-cursor-plugin/internal/account"
)

const providerIdentifier = "cursor"

// Provider implements pluginapi.AuthProvider for Cursor, wiring the
// PKCE login flow (login.go/poll.go), token refresh (refresh.go), and
// storage encoding (storage.go) into the host-facing interface, and is
// the sole writer of internal/account's Store per the ralplan-approved
// plan's account-state ownership design.
type Provider struct {
	poller    *Poller
	refresher *Refresher
	accounts  *account.Store
}

// NewProvider creates a Cursor auth Provider backed by the given account
// store. httpClient may be nil to use package defaults.
func NewProvider(accounts *account.Store, httpClient *http.Client) *Provider {
	return &Provider{
		poller:    NewPoller(httpClient),
		refresher: NewRefresher(httpClient),
		accounts:  accounts,
	}
}

// Identifier implements pluginapi.AuthProvider.
func (p *Provider) Identifier() string {
	return providerIdentifier
}

// ParseAuth implements pluginapi.AuthProvider: decodes a persisted
// StorageJSON payload into pluginapi.AuthData.
func (p *Provider) ParseAuth(_ context.Context, req pluginapi.AuthParseRequest) (pluginapi.AuthParseResponse, error) {
	storage, err := ParseTokenStorage(req.RawJSON)
	if err != nil {
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}
	if storage.Type != "cursor" {
		return pluginapi.AuthParseResponse{Handled: false}, nil
	}

	authData := pluginapi.AuthData{
		Provider:         providerIdentifier,
		ID:               providerIdentifier,
		FileName:         req.FileName,
		Label:            "Cursor",
		StorageJSON:      req.RawJSON,
		NextRefreshAfter: storage.ExpiresAt(),
	}

	p.accounts.Set(authData.ID, account.State{
		AccessToken:  storage.AccessToken,
		RefreshToken: storage.RefreshToken,
		ExpiresAt:    storage.ExpiresAt(),
		Status:       account.StatusActive,
	})

	return pluginapi.AuthParseResponse{Handled: true, Auth: authData}, nil
}

// StartLogin implements pluginapi.AuthProvider: begins a new PKCE login
// attempt and returns its login URL and poll state (the Cursor login
// UUID).
func (p *Provider) StartLogin(_ context.Context, _ pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
	params, deadline, err := p.poller.StartLogin()
	if err != nil {
		return pluginapi.AuthLoginStartResponse{}, fmt.Errorf("cursor: failed to start login: %w", err)
	}
	return pluginapi.AuthLoginStartResponse{
		Provider:  providerIdentifier,
		URL:       params.LoginURL,
		State:     params.UUID,
		ExpiresAt: deadline,
	}, nil
}

// PollLogin implements pluginapi.AuthProvider: checks one login attempt's
// status and, on success, records the resulting account in the shared
// account.Store (per the account-state ownership design, internal/auth is
// the sole writer).
func (p *Provider) PollLogin(ctx context.Context, req pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
	result := p.poller.Poll(ctx, req.State)

	if result.Status != pluginapi.AuthLoginStatusSuccess {
		return pluginapi.AuthLoginPollResponse{
			Status:  result.Status,
			Message: result.Message,
		}, nil
	}

	expiresAt := getTokenExpiry(result.AccessToken)
	storage := NewTokenStorage(result.AccessToken, result.RefreshToken, expiresAt)
	storageJSON, err := storage.Marshal()
	if err != nil {
		return pluginapi.AuthLoginPollResponse{}, fmt.Errorf("cursor: failed to marshal token storage: %w", err)
	}

	authData := pluginapi.AuthData{
		Provider:         providerIdentifier,
		ID:               providerIdentifier,
		FileName:         providerIdentifier + ".json",
		Label:            "Cursor",
		StorageJSON:      storageJSON,
		NextRefreshAfter: expiresAt,
	}

	p.accounts.Set(authData.ID, account.State{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
		ExpiresAt:    expiresAt,
		Status:       account.StatusActive,
	})

	return pluginapi.AuthLoginPollResponse{
		Status: pluginapi.AuthLoginStatusSuccess,
		Auth:   authData,
	}, nil
}

// RefreshAuth implements pluginapi.AuthProvider: refreshes a stored
// account's tokens, deduplicated per account id, and marks the account
// degraded in the shared account.Store on failure rather than silently
// dropping it (fact-r6-model-refresh-policy).
func (p *Provider) RefreshAuth(ctx context.Context, req pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
	storage, err := ParseTokenStorage(req.StorageJSON)
	if err != nil {
		p.accounts.MarkDegraded(req.AuthID, account.StatusNeedsReauth, fmt.Sprintf("failed to parse stored auth: %v", err))
		return pluginapi.AuthRefreshResponse{}, fmt.Errorf("cursor: failed to parse stored auth for refresh: %w", err)
	}

	tokenToRefresh := storage.RefreshToken
	if tokenToRefresh == "" {
		tokenToRefresh = storage.AccessToken
	}

	result, errRefresh := p.refresher.Refresh(ctx, req.AuthID, tokenToRefresh)
	if errRefresh != nil {
		p.accounts.MarkDegraded(req.AuthID, account.StatusNeedsReauth, errRefresh.Error())
		return pluginapi.AuthRefreshResponse{}, fmt.Errorf("cursor: refresh failed: %w", errRefresh)
	}

	newStorage := NewTokenStorage(result.AccessToken, result.RefreshToken, result.ExpiresAt)
	storageJSON, errMarshal := newStorage.Marshal()
	if errMarshal != nil {
		p.accounts.MarkDegraded(req.AuthID, account.StatusDegraded, fmt.Sprintf("failed to marshal refreshed auth: %v", errMarshal))
		return pluginapi.AuthRefreshResponse{}, fmt.Errorf("cursor: failed to marshal refreshed token storage: %w", errMarshal)
	}

	authData := pluginapi.AuthData{
		Provider:         providerIdentifier,
		ID:               req.AuthID,
		Label:            "Cursor",
		StorageJSON:      storageJSON,
		Metadata:         req.Metadata,
		Attributes:       req.Attributes,
		NextRefreshAfter: result.ExpiresAt,
	}

	p.accounts.Set(req.AuthID, account.State{
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
		ExpiresAt:    result.ExpiresAt,
		Status:       account.StatusActive,
	})

	return pluginapi.AuthRefreshResponse{
		Auth:             authData,
		NextRefreshAfter: result.ExpiresAt,
	}, nil
}
