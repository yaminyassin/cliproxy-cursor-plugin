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
//	go run ./cmd/e2eprobe conv [model-id]
//	go run ./cmd/e2eprobe tool [model-id]
//	go run ./cmd/e2eprobe models
//
// login blocks in a single process until the browser login completes or
// times out - internal/auth.Poller's pending-attempt state (the PKCE
// verifier) is intentionally process-lifetime/in-memory only, matching
// production (a real host process stays running across StartLogin ->
// PollLogin calls), so login and the poll loop that follows it MUST run
// in the same process; a prior version of this tool split them into
// separate `go run` invocations, which cannot work by the same design
// that makes the production code correct - not a bug in the plugin.
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
// production auth-file format) for the resulting tokens, carried across
// separate chat/models invocations after a completed login.
type probeState struct {
	AuthID      string    `json:"auth_id"`
	AccessToken string    `json:"access_token"`
	StorageJSON string    `json:"storage_json"`
	ExpiresAt   time.Time `json:"expires_at"`
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: e2eprobe <login|chat|models> [args]")
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
		cmdLogin(ctx, authProvider, accounts)
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
	case "conv":
		model := ""
		if len(os.Args) >= 3 {
			model = os.Args[2]
		}
		cmdConv(ctx, exec, accounts, model)
	case "tool":
		model := ""
		if len(os.Args) >= 3 {
			model = os.Args[2]
		}
		cmdTool(ctx, exec, accounts, model)
	case "models":
		cmdModels(ctx, disc, accounts)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		os.Exit(1)
	}
}

// cmdLogin runs the real StartLogin -> poll loop -> PollLogin(success)
// sequence entirely within this one process invocation, using the
// plugin's actual production auth.Provider code. This blocks (printing a
// progress dot every 5s, matching the host-conformant poll interval)
// until the browser login completes, times out at the 15-minute
// host-conformant ceiling, or Cursor rejects the attempt.
func cmdLogin(ctx context.Context, authProvider *auth.Provider, accounts *account.Store) {
	startResp, err := authProvider.StartLogin(ctx, pluginapi.AuthLoginStartRequest{})
	if err != nil {
		fatalf("StartLogin failed: %v", err)
	}
	fmt.Println("Open this URL in a browser and complete the Cursor login:")
	fmt.Println()
	fmt.Println(startResp.URL)
	fmt.Println()
	fmt.Printf("Login expires at: %s\n", startResp.ExpiresAt.Format(time.RFC3339))
	fmt.Println()
	fmt.Println("Waiting for login completion (host-conformant: 5s interval, 15min ceiling)...")

	deadline := time.Now().Add(15 * time.Minute)
	for time.Now().Before(deadline) {
		resp, errPoll := authProvider.PollLogin(ctx, pluginapi.AuthLoginPollRequest{State: startResp.State})
		if errPoll != nil {
			fatalf("PollLogin failed: %v", errPoll)
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

// cmdConv sends a hardcoded multi-turn conversation (user, assistant,
// user) in ONE Execute call, proving rootPromptMessagesJson genuinely
// reaches Cursor's model prompt construction within a single request -
// unlike cmdChat's independent single-message calls, this tests whether
// the model actually uses prior turns supplied in the same
// AgentRunRequest, matching what
// TestBuildAgentRunRequest_RootPromptMessagesJson verifies at the wire
// level but cannot prove against the real backend.
func cmdConv(ctx context.Context, exec *executor.Executor, accounts *account.Store, model string) {
	st := requireLoggedIn()
	restoreAccount(accounts, st)

	if model == "" {
		model = "gpt-5-mini"
	}

	payload := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": "My favorite number is 7. Just acknowledge it in one short sentence."},
			{"role": "assistant", "content": "Got it — your favorite number is 7."},
			{"role": "user", "content": "What is my favorite number plus 100?"},
		},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		fatalf("failed to marshal request: %v", err)
	}

	fmt.Printf("Sending a 3-message conversation to Cursor (model=%s) in ONE request:\n", model)
	fmt.Println(`  1. user: "My favorite number is 7. Just acknowledge it in one short sentence."`)
	fmt.Println(`  2. assistant: "Got it — your favorite number is 7."`)
	fmt.Println(`  3. user: "What is my favorite number plus 100?"`)
	fmt.Println()

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
	fmt.Println()
	fmt.Println("If the response correctly says 107 (or otherwise references 7 from turn 1), rootPromptMessagesJson genuinely reached Cursor's model prompt construction within this single request.")
}

// cmdTool sends a message that naturally prompts Cursor's model to
// request a native tool (e.g. listing/reading files) and prints whether
// the response surfaced a chat-completions tool_calls entry, per
// fact-r5-tool-roundtrip: this plugin must surface the tool request to
// the local client (as tool_calls), never execute it in-plugin itself.
// This is the one Acceptance Criteria path the mocked tests
// (TestExecute_SingleToolCallRoundTrip et al.) can only prove at the
// translation-logic level, not against Cursor's real tool-triggering
// behavior end to end.
func cmdTool(ctx context.Context, exec *executor.Executor, accounts *account.Store, model string) {
	st := requireLoggedIn()
	restoreAccount(accounts, st)

	if model == "" {
		model = "gpt-5-mini"
	}

	message := "List the files in the current working directory using your shell/ls tool, then tell me how many there are."

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
	fmt.Println()

	var decoded struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if errDecode := json.Unmarshal(resp.Payload, &decoded); errDecode != nil || len(decoded.Choices) == 0 {
		fmt.Println("(could not decode response for tool_calls inspection)")
		return
	}

	choice := decoded.Choices[0]
	if len(choice.Message.ToolCalls) == 0 {
		fmt.Println("No tool_calls in the response (Cursor answered in plain text this turn, or picked a different model behavior).")
		fmt.Println("If this repeats, try re-running - tool invocation is model-decided, not deterministic per call.")
		return
	}

	fmt.Printf("SUCCESS: Cursor requested %d native tool call(s), surfaced as chat-completions tool_calls (never executed in-plugin, per fact-r5-tool-roundtrip):\n\n", len(choice.Message.ToolCalls))
	for i, tc := range choice.Message.ToolCalls {
		fmt.Printf("  [%d] id=%s type=%s\n", i, tc.ID, tc.Type)
		fmt.Printf("      function.name: %s\n", tc.Function.Name)
		fmt.Printf("      function.arguments: %s\n", tc.Function.Arguments)
	}
	fmt.Printf("\nfinish_reason: %s (expected \"tool_calls\")\n", choice.FinishReason)
	fmt.Println()
	fmt.Println("This proves Cursor's real ExecServerMessage/InteractionUpdate.ToolCallCompleted path was translated into a standard chat-completions tool_calls entry against the live backend, not a mock.")
}

func requireLoggedIn() probeState {
	st := loadState()
	if st.AuthID == "" || st.AccessToken == "" {
		fatalf("not logged in; run 'e2eprobe login' first")
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
