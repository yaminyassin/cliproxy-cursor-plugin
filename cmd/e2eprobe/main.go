// Command e2eprobe drives the real plugin code (internal/auth,
// internal/account, internal/executor, internal/discovery) directly
// against Cursor's real backend (api2.cursor.sh), without needing a full
// CLIProxyAPI host process. This is the tool for the one Acceptance
// Criteria item that cannot be verified by the mocked unit test suite:
// a real login + real chat request + real response from Cursor, using
// this plugin's actual production code paths, not a reimplementation.
//
// Usage:
//
//	go run ./cmd/e2eprobe login
//	go run ./cmd/e2eprobe chat "your message here"
//	go run ./cmd/e2eprobe models
//
// State (tokens) persists to .e2eprobe-state.json in the current
// directory between invocations, gitignored.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/router-for-me/cliproxy-cursor-plugin/internal/account"
	"github.com/router-for-me/cliproxy-cursor-plugin/internal/auth"
	"github.com/router-for-me/cliproxy-cursor-plugin/internal/discovery"
	"github.com/router-for-me/cliproxy-cursor-plugin/internal/executor"
)

const stateFile = ".e2eprobe-state.json"

// probeState is this tool's own on-disk persistence (not the plugin's
// production auth-file format), just enough to carry a poll attempt id
// across the login/wait invocations and the resulting AuthData across
// login/chat/models invocations.
type probeState struct {
	AuthID      string    `json:"auth_id"`
	AccessToken string    `json:"access_token"`
	StorageJSON string    `json:"storage_json"`
	ExpiresAt   time.Time `json:"expires_at"`
	PendingUUID string    `json:"pending_uuid,omitempty"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: e2eprobe <login|wait|chat|models> [args]")
		os.Exit(1)
	}

	accounts := account.NewStore()
	authProvider := auth.NewProvider(accounts, nil)
	agentClient := executor.NewAgentClient(accounts, nil, "", "")
	exec := executor.NewExecutor(agentClient)
	disc := discovery.NewDiscoverer(agentClient.Service, accounts)

	ctx := context.Background()

	switch os.Args[1] {
	case "login":
		cmdLogin(ctx, authProvider)
	case "wait":
		cmdWait(ctx, authProvider, accounts)
	case "chat":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: e2eprobe chat \"message\" [model-id]")
			os.Exit(1)
		}
		model := ""
		if len(os.Args) >= 4 {
			model = os.Args[3]
		}
		cmdChat(ctx, exec, accounts, os.Args[2], model)
	case "models":
		cmdModels(ctx, disc, accounts)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		os.Exit(1)
	}
}

func cmdLogin(ctx context.Context, authProvider *auth.Provider) {
	resp, err := authProvider.StartLogin(ctx, pluginapi.AuthLoginStartRequest{})
	if err != nil {
		fatalf("StartLogin failed: %v", err)
	}
	fmt.Println("Open this URL in a browser and complete the Cursor login:")
	fmt.Println()
	fmt.Println(resp.URL)
	fmt.Println()
	fmt.Printf("Login expires at: %s\n", resp.ExpiresAt.Format(time.RFC3339))
	fmt.Println()
	fmt.Println("Then run: go run ./cmd/e2eprobe wait")

	saveState(probeState{PendingUUID: resp.State})
}

func cmdWait(ctx context.Context, authProvider *auth.Provider, accounts *account.Store) {
	st := loadState()
	if st.PendingUUID == "" {
		fatalf("no pending login attempt found; run 'e2eprobe login' first")
	}

	fmt.Println("Polling Cursor for login completion (host-conformant: 5s interval, 15min ceiling)...")
	deadline := time.Now().Add(15 * time.Minute)
	for time.Now().Before(deadline) {
		resp, err := authProvider.PollLogin(ctx, pluginapi.AuthLoginPollRequest{State: st.PendingUUID})
		if err != nil {
			fatalf("PollLogin failed: %v", err)
		}
		switch resp.Status {
		case pluginapi.AuthLoginStatusPending:
			fmt.Print(".")
			time.Sleep(5 * time.Second)
			continue
		case pluginapi.AuthLoginStatusSuccess:
			fmt.Println()
			fmt.Println("Login succeeded.")
			authState, ok := accounts.Peek(resp.Auth.ID)
			if !ok {
				fatalf("login succeeded but account store has no entry for %q (real bug, please report)", resp.Auth.ID)
			}
			saveState(probeState{
				AuthID:      resp.Auth.ID,
				AccessToken: authState.AccessToken,
				StorageJSON: string(resp.Auth.StorageJSON),
				ExpiresAt:   authState.ExpiresAt,
			})
			fmt.Printf("Account id: %s\n", resp.Auth.ID)
			fmt.Printf("Token expires: %s\n", authState.ExpiresAt.Format(time.RFC3339))
			fmt.Println()
			fmt.Println("Now run: go run ./cmd/e2eprobe models")
			fmt.Println("     or: go run ./cmd/e2eprobe chat \"hello\"")
			return
		case pluginapi.AuthLoginStatusError:
			fatalf("login failed: %s", resp.Message)
		}
	}
	fatalf("login timed out after 15 minutes")
}

func cmdModels(ctx context.Context, disc *discovery.Discoverer, accounts *account.Store) {
	st := requireLoggedIn()
	restoreAccount(accounts, st)

	resp, err := disc.ModelsForAuth(ctx, pluginapi.AuthModelRequest{AuthID: st.AuthID})
	if err != nil {
		fatalf("ModelsForAuth failed (real Cursor backend call): %v", err)
	}
	fmt.Printf("Cursor returned %d usable models for this account:\n\n", len(resp.Models))
	for _, m := range resp.Models {
		fmt.Printf("  %-30s %s\n", m.ID, m.DisplayName)
	}
}

func cmdChat(ctx context.Context, exec *executor.Executor, accounts *account.Store, message, model string) {
	st := requireLoggedIn()
	restoreAccount(accounts, st)

	if model == "" {
		model = "auto"
	}

	payload := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": message},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		fatalf("failed to marshal request: %v", err)
	}

	fmt.Printf("Sending to Cursor (model=%s): %q\n\n", model, message)
	start := time.Now()
	resp, err := exec.Execute(ctx, pluginapi.ExecutorRequest{
		AuthID:  st.AuthID,
		Model:   model,
		Payload: raw,
	})
	elapsed := time.Since(start)
	if err != nil {
		fatalf("Execute failed (real Cursor backend call): %v", err)
	}

	fmt.Printf("Response received in %s:\n\n", elapsed.Round(time.Millisecond))
	fmt.Println(string(resp.Payload))
}

func requireLoggedIn() probeState {
	st := loadState()
	if st.AuthID == "" || st.AccessToken == "" {
		fatalf("not logged in; run 'e2eprobe login' then 'e2eprobe wait' first")
	}
	return st
}

func restoreAccount(accounts *account.Store, st probeState) {
	accounts.Set(st.AuthID, account.State{
		AccessToken: st.AccessToken,
		ExpiresAt:   st.ExpiresAt,
		Status:      account.StatusActive,
	})
}

func loadState() probeState {
	raw, err := os.ReadFile(stateFile)
	if err != nil {
		return probeState{}
	}
	var st probeState
	if err := json.Unmarshal(raw, &st); err != nil {
		return probeState{}
	}
	return st
}

func saveState(st probeState) {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		fatalf("failed to marshal state: %v", err)
	}
	if err := os.WriteFile(stateFile, raw, 0o600); err != nil {
		fatalf("failed to write state file: %v", err)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
