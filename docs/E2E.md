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

**Known remaining gap - client-facing streaming is buffered per turn,
not token-by-token:** the exchange **between this plugin and Cursor** is
the real, fully bidirectional multi-message protocol described above. The
exchange **between this plugin and the local chat-completions client**
now uses the correct OpenAI *streaming* wire format
(`object: "chat.completion.chunk"` with `choices[].delta`, plus a
terminal chunk carrying `finish_reason` and `usage`), so standard
streaming clients parse it normally. What remains buffered is the
*timing*, not the format: `Executor.ExecuteStream`
(`internal/executor/executor.go`) waits for the entire Cursor turn to
finish (or fail) before emitting its chunks, so a client sees one pause
followed by the whole answer rather than progressive tokens. This is a
real scope limitation, not a cosmetic detail -
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
  correctly-formatted SSE stream (`chat.completion.chunk` events with
  `delta`, then a terminal chunk with `finish_reason` and `usage`, then
  `[DONE]`), but arriving all at once after a pause rather than
  progressively. That timing behavior is the documented
  buffered-per-turn limitation above, not a bug.

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
- **CORRECTION (this entry was originally overstated).** The two runs
  recorded here were executed against a **stale `cursor.dylib` built
  before `buildMcpTools` existed**, so they did NOT prove the McpTools
  wiring at all: the `mcp_tool_call` observed was Cursor's own
  speculative behavior. Worse, surfacing a tool call as
  `function.name: "mcp_tool_call"` is useless to a client, which can only
  dispatch tools it actually declared. Genuine verification came later -
  see the "six client-compatibility bugs" entry below.

### 2026-08-19 (continued) — six client-compatibility bugs found by real client integration

Running a real OpenAI-compatible client (`gjc`) against a real
CLIProxyAPI host with this plugin loaded exposed six genuine plugin bugs
that neither the mocked suite nor hand-written `curl` requests caught.
Recorded here because each one is a lesson about what mocked tests and
simplified manual requests cannot prove.

1. **`execute_stream` `Err` serialization (critical, plugin-fusing).**
   `pluginapi.ExecutorStreamChunk.Err` is a bare `error` interface with no
   JSON tags, so `encoding/json` can never unmarshal into it. Emitting any
   `err` field made the host fail to decode the entire result, which made
   CLIProxyAPI **fuse** the plugin: every later request returned
   `503 auth_unavailable` and the plugin was eventually unloaded. Chunk
   errors are now surfaced as a terminal error payload chunk instead.
   *This also explains log lines previously mis-attributed to shutdown.*
2. **`content` array form rejected.** OpenAI allows
   `"content": [{"type":"text",...}]`, but `Content` was a plain `string`,
   so every request from a client using the content-parts form failed to
   decode. Both forms are accepted now.
3. **No `usage` block.** Clients reject responses with absent/all-zero
   usage as anomalous ("empty response with anomalously low token
   usage"). Usage is now always emitted, with completion tokens taken
   from Cursor's own `TokenDelta` stream.
4. **Client tool names discarded.** Cursor requests client-declared tools
   through `ExecServerMessage{McpArgs{Name,Args}}`; the
   `ToolCallCompleted` that follows a decline carries only the rejection.
   The name/args are now captured from the exec request and surfaced
   under the client's own declared name, making tool calls dispatchable.
5. **`McpArgs` values are serialized `google.protobuf.Value`**, not raw
   JSON. Treating them as text leaked wire bytes into client-facing JSON
   (`{"city":"\u001a\u0005Seoul"}`). Encoded/decoded correctly both
   directions; tool results now round-trip for client-declared tools
   instead of failing with "unknown tool call variant".
6. **Streaming used the non-streaming object shape.** Streaming clients
   parse `choices[].delta` with `object == "chat.completion.chunk"`, so
   sending a `chat.completion` object with `choices[].message` made them
   accumulate nothing and report an empty response.

**Verified live after the fixes:**

- A real `gjc` session returns a correct answer:
  `21 × 13 is 273.`
- SSE format confirmed: `chat.completion.chunk` events with `delta`, then
  a terminal chunk with `finish_reason` and `usage`.
- **Full tool round trip with no declining, under the client's own tool
  name:** client declares `get_weather` -> Cursor calls it as
  `get_weather` with clean `{"city":"Seoul"}` -> client executes -> result
  fed back as a `tool` message -> model answers *"The weather tool
  reported 21 degrees Celsius."*
- Plugin stays loaded and healthy across repeated streaming requests plus
  the model registrar (no fuse), confirming bug 1 is resolved under load.

This closes the "custom tool sets work with this plugin" question:
before, no; after these fixes, yes - verified against the real backend
through a real client, not a probe.