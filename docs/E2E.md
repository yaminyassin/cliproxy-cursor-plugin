# Real End-to-End Verification

The mocked unit test suite (`make build && go test ./...`) proves every
capability's logic against fake/mocked Cursor backends without a live
account. Per the ralplan-approved plan's two-tier verification strategy
(fact-r3-acceptance), the plugin is only considered fully done once it
has also been checked once against a **real** Cursor account through a
**real** CLIProxyAPI instance. This document is that manual checklist.

## Protocol implementation notes (post-boundary-review rework)

An initial implementation of the executor used connect-go's generated
**unary** `Run` client. Two rounds of terminal review verified against
`../gajae-code/packages/ai/src/providers/cursor.ts` that Cursor's real
`Run` exchange is **not** a simple request/response call: it is a
bidirectional HTTP/2 stream where the server can send multiple top-level
messages, can request the client to resolve content-addressed blob IDs
mid-exchange (`KvServerMessage`/`KvClientMessage`
`getBlobArgs`/`setBlobArgs`), and can request live execution context
(`ExecServerMessage`, e.g. `requestContextArgs`) that requires a
synchronous reply on the same stream before the exchange continues. The
unary-only implementation silently dropped conversation history,
mis-encoded tool results, only ever consumed the first server message,
and could hang indefinitely on any exec-request message.

The executor (`internal/executor/stream.go`) now performs the real
exchange: a genuine HTTP/2 duplex request, Cursor's own Connect streaming
frame format, a content-addressed blob store
(`internal/executor/blobstore.go`), the `rootPromptMessagesJson` field
(which is what Cursor's server actually reads to build the model prompt
- `turns[]` is a UI-history view only), and a generic
`ExecServerMessage` responder (`internal/executor/execmessage.go`) that
answers `requestContextArgs` with a minimal real context and answers
every execution-type request (`readArgs`, `lsArgs`, `shellArgs`, ...)
with that result type's "rejected" case - this plugin never executes
tools in-plugin (fact-r5-tool-roundtrip), so declining rather than
executing is the correct behavior, and declining still lets Cursor's
exchange complete cleanly instead of hanging. This is covered by tests
using a real HTTP/2 test server (`h2c`), not a substituted unary mock -
see `internal/executor/executor_test.go`, `stream_test.go`, and
`execmessage_test.go`.

`x_cursor_client_version` is a plugin `ConfigField` and now has real
runtime effect: `plugin.register`/`plugin.reconfigure`'s `config_yaml`
payload (the host's real lifecycle request shape, confirmed against
`internal/pluginhost/rpc_schema.go` in the downloaded
`github.com/router-for-me/CLIProxyAPI/v7` module) is parsed and applied
to the process-global Cursor HTTP client
(`internal/dispatch/dispatch.go`'s `applyLifecycleConfig`), verified by a
test that captures the actual outbound header on a real HTTP request
after a reconfigure call. The compiled-in default is kept in sync with
gajae-code's `CURSOR_CLIENT_VERSION` at the time of writing
(`internal/executor/client.go`) and should be re-verified/updated
periodically, since Cursor gates backend features on this header.

**Known remaining gap - client-facing streaming is still buffered, not
incremental:** the exchange **between this plugin and Cursor** is now
the real, fully bidirectional multi-message protocol described above.
However, the exchange **between this plugin and the local
chat-completions client** (CLIProxyAPI's own downstream consumer) is
still fully buffered: `Executor.ExecuteStream`
(`internal/executor/executor.go`) waits for the entire Cursor turn to
finish (or fail) before emitting anything, then returns exactly one
chunk containing a complete `chat.completion`-shaped payload, not a
sequence of incremental `chat.completion.chunk`/SSE delta events. A
downstream client that expects true token-by-token streaming will
instead see one long pause followed by the full response at once. This
is a real, load-bearing scope limitation, not a cosmetic detail -
verify it explicitly in Step 3 below rather than assuming the executor's
internal protocol fix also fixed client-facing streaming (it did not;
they are two separate layers).

## Prerequisites

- A built plugin binary: `make build` (produces `bin/cursor.dylib` /
  `.so` / `.dll` depending on platform).
- A running CLIProxyAPI instance (see the
  [CLIProxyAPI docs](https://help.router-for.me/)) with the plugin wired
  in via `plugins.configs.cursor` in `config.yaml` (see
  `config/plugins-config-example.yaml`), and `bin/cursor.<ext>` copied or
  symlinked into CLIProxyAPI's configured `plugins.dir`.
- A real Cursor account (any active subscription/account able to log in
  at cursor.com).

## Steps

### 1. Build and install

```sh
make build
cp bin/cursor.dylib <cliproxyapi-dir>/plugins/   # adjust extension for your platform
```

Confirm CLIProxyAPI starts without error and logs the plugin registering
with capabilities `auth_provider`, `executor`, `model_registrar`,
`model_provider` (check CLIProxyAPI's own startup logs / Management API
plugin list).

### 2. Log in with a real Cursor account

Through CLIProxyAPI's management login flow (see CLIProxyAPI's
Management API docs for the auth-provider login UI/CLI path), start a
Cursor login. Confirm:

- A `cursor.com/loginDeepControl?...` URL is returned and opens
  correctly in a browser.
- After completing the browser login, the account transitions to
  `success` status within the host-conformant timeout window (up to 15
  minutes; typically seconds).
- The account does **not** get stuck in `pending` or silently disappear.
- Logging in with a second, different Cursor account produces a second,
  independent account entry (not overwriting the first) — this was a
  real bug found and fixed during boundary review
  (`TestPollLogin_TwoAccounts_DoNotCollide`); spot-check it against a
  real pair of accounts if you have access to more than one.

### 3. Send a chat request through the local endpoint

Send a chat-completions request to a Cursor model through CLIProxyAPI's
local OpenAI-compatible endpoint (e.g. `POST /v1/chat/completions` with
`"model": "<a cursor model id>"`, using whichever model id
`model.for_auth` discovery surfaced for your account — check
CLIProxyAPI's model list Management API or logs).

Confirm:

- The request succeeds and returns real assistant text from Cursor, not
  an error or empty response.
- The response shape matches standard chat-completions (`choices[0]
  .message.content`).
- If the exchange required a Cursor-initiated blob or exec-context
  request (this can happen depending on conversation length/complexity),
  confirm the response still comes back successfully rather than hanging
  or erroring — this validates the real KV/exec handshakes against
  Cursor's actual backend, not just the local `h2c` test server.
- With `"stream": true`, confirm the client-facing response arrives as a
  single delayed payload rather than incremental chunks (this is the
  documented buffered-streaming limitation above, not a bug — just
  confirm it matches the documented behavior rather than silently
  assuming true incremental streaming works).

### 4. Multi-turn tool-using conversation

Have a conversation that causes Cursor to emit a native tool call (for
example, asking it to list files or read a file in some connected
context, if your CLIProxyAPI client surface supports tool use with this
provider).

Confirm:

- Cursor's tool call is surfaced to the client as a standard
  `tool_calls` entry (`choices[0].message.tool_calls`), not executed by
  the plugin itself and not silently dropped.
- Sending the tool result back as a `tool` role message in the next
  request is accepted and the conversation continues coherently (Cursor
  produces a follow-up response that accounts for the tool result) - the
  translation layer itself is unit-tested
  (`TestBuildAgentRunRequest_ToolResultRoundTrip`, which asserts the
  actual result text decodes correctly at the right nested oneof depth,
  e.g. `ShellResult.Success.Stdout`), but this step verifies Cursor's
  real backend actually accepts and acts on the re-encoded tool result,
  which no mock can prove.
- Confirm the conversation genuinely uses prior turns as context (ask a
  follow-up question that only makes sense with earlier context) - this
  validates `rootPromptMessagesJson` is actually reaching Cursor's model
  prompt construction, not just being present in the request.
- If Cursor attempts to execute a tool directly through the exec-request
  channel (as opposed to surfacing it as a `tool_calls` entry), confirm
  the plugin declines it (per fact-r5-tool-roundtrip) without hanging or
  erroring the whole turn, and that the turn still completes with
  whatever content Cursor could produce despite the decline.

This validates fact-r5-tool-roundtrip end-to-end, beyond what the mocked
tests can prove on their own.

### 5. Force a refresh failure

Simulate a bad stored refresh token (e.g. temporarily corrupt/replace the
persisted `auths/<account-id>.json` refresh token with an invalid value,
or wait for natural expiry and revoke access from your Cursor account
settings if available) and trigger a refresh (either by waiting for the
host's normal refresh cycle or by using CLIProxyAPI's manual auth-refresh
management action, if available).

Confirm:

- The account surfaces as `degraded`/`needs_reauth` in the management
  UI/API, with a clear error message.
- The account is **not** silently dropped or left showing a misleadingly
  healthy status.
- A subsequent chat request against that account fails fast with a clear
  "account degraded" error rather than a confusing Cursor-side failure.

## Recording results

There is no automated capture for this checklist (by design — it
requires a real account and human judgment about response quality). Note
the CLIProxyAPI version, plugin version, and date tested when you
complete this checklist, and file any deviations as issues before
considering the corresponding Acceptance Criteria item satisfied.

## Real run log

### 2026-08-19 — cmd/e2eprobe against api2.cursor.sh

Ran via `cmd/e2eprobe` (see that command's doc comment): a standalone
CLI that drives this plugin's actual production `internal/auth`,
`internal/account`, `internal/executor`, and `internal/discovery` code
directly against Cursor's real backend, without a full CLIProxyAPI host
process — the same code paths the plugin uses when loaded by CLIProxyAPI,
just invoked from a smaller harness.

- **Login (Steps 1-2)**: `go run ./cmd/e2eprobe login` generated a real
  PKCE login URL, completed in a real browser against a real Cursor
  account, and `auth.Provider.PollLogin` correctly transitioned to
  `success` against the real `api2.cursor.sh/auth/poll` endpoint.
  Account id `cursor-a4b0dd41-34fa-4679-934b-a5f5d1be60d9`, token expiry
  correctly parsed from the real JWT.
- **Model discovery**: `go run ./cmd/e2eprobe models` returned the real,
  account-specific model catalog via `GetUsableModels` (Claude Sonnet
  4.5, GPT-5.4/5.1, Gemini 3.5 Flash, Kimi K3, GLM 5.2, and others) —
  confirming `internal/discovery.Discoverer` normalizes a real response,
  not a mocked one.
- **Single-turn chat (Step 3)**: `go run ./cmd/e2eprobe chat "What model
  are you, and what is 17 * 24? ..." gpt-5-mini` returned a real,
  correct answer ("I'm GPT-5 Mini and 17 * 24 = 408.") in 4.34s through
  `internal/executor.Executor.Execute`'s real bidirectional HTTP/2 Run
  exchange.
- **Multi-turn context (Step 3, `rootPromptMessagesJson` verification)**:
  a 3-message conversation (user states a number, assistant
  acknowledges, user asks for that number + 100 — all in one
  `Execute` call, via a new `conv` probe command) returned "Your
  favorite number plus 100 is 107." — correctly recalling the number
  from turn 1, which was never repeated in turn 3's text. This is
  concrete, real-backend evidence that `rootPromptMessagesJson`
  (`internal/executor/translate_request.go`) genuinely reaches Cursor's
  model prompt construction, not just that it is present on the wire
  (which the mocked `TestBuildAgentRunRequest_RootPromptMessagesJson`
  already proved at the encoding level).

Not yet run in this pass: a forced refresh failure (Step 5). That
remains an open follow-up verification item; this entry covers login,
discovery, and single-turn, multi-turn, and native-tool-triggering chat
translation against the real backend.

### 2026-08-19 (continued) — native-tool-triggering conversation

Ran via `go run ./cmd/e2eprobe tool [model-id]` (new probe command),
which sends a message that naturally prompts Cursor's model to request a
native tool (listing files) and decodes the resulting `tool_calls`
entries.

- **First run**: Cursor genuinely requested 4 native tool calls
  (3× `shell_tool_call`, 1× `glob_tool_call`) against the real backend.
  The plugin correctly declined execution per fact-r5-tool-roundtrip via
  the `ExecServerMessage`-decline path built during the boundary review,
  `finish_reason: "tool_calls"` was correctly set, and multi-tool-call
  batching into one array worked. Cursor's own model text adapted
  correctly to the decline ("I can't run shell commands from here...").
  **This run also surfaced a real defect**: Cursor's raw `call_id` for
  these tool calls contained an embedded newline plus a second
  concatenated internal id (e.g.
  `"call_xyz\nfc_0e03a4cd..."`), which the plugin was passing straight
  through into the chat-completions-facing `tool_calls[].id` field —
  not a clean OpenAI-compatible opaque token.
- **Fix**: `internal/executor/translate_response.go`'s `toChatToolCall`
  now always generates a fresh `call_<hex>` id, decoupled entirely from
  Cursor's raw `call_id` (verified safe: the tool-result round-trip in
  `translate_request.go` reconstructs the Cursor `ToolCall` from
  `Function.Name`/`Arguments` only, never from the client-facing `id`).
- **Re-run after the fix**: Cursor requested 2 native `shell_tool_call`s
  (tool invocation is model-decided, so the count varies per run); both
  surfaced ids were clean, opaque, whitespace-free tokens
  (`call_0e75067e50614632322416ffdaaab0c3`,
  `call_3d203bee81b32c50fba7c13e7c4ee04a`), correctly declined, with
  `finish_reason: "tool_calls"`.

This is a concrete example of the two-tier verification strategy finding
a real defect the mocked suite could not: the mocked
`TestResponseAccumulator_BatchesMultipleToolCallsIntoOneArray` and
`TestExecute_SingleToolCallRoundTrip` construct their own well-formed
`CallId` fixtures and never exercised what Cursor's real backend
actually sends.

### 2026-08-19 (continued) — custom client-declared tools (tools[] -> McpTools)

Ran via `go run ./cmd/e2eprobe mcptool [model-id]` (new probe command),
which declares a custom `get_weather` tool via an OpenAI-style `tools[]`
array on the request and asks Cursor's model to call it.

- Before this change, `chatCompletionsRequest` had no `tools` field at
  all - a client-supplied custom tool set was silently ignored and
  never reached Cursor's model.
- Implemented `buildMcpTools` (translates declared tools into Cursor's
  real `AgentRunRequest.McpTools`/`McpToolDefinition`, matching
  gajae-code's `input_schema`-as-serialized-`google.protobuf.Value`
  wire convention) plus an always-appended system instruction
  (`nativeToolsOnlyMcpSystemPrompt`) telling the model to only call
  declared tools, never Cursor's native ones - skipped if the client's
  own system message already contains the identical instruction.
- **Verified live, 2/2 runs**: Cursor's model correctly called the
  declared custom `get_weather` tool via the `mcp_tool_call` path (not
  any native tool like `shell_tool_call`) both times, proving
  `McpTools` and the system instruction both genuinely reach the real
  model, not just the wire encoding. The plugin correctly declined
  in-plugin execution per fact-r5-tool-roundtrip and Cursor's model
  handled the decline gracefully in both responses.

This closes the "custom tool sets work with this plugin" question:
before this change, no; after, yes, verified against the real backend.