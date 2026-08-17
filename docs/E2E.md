# Real End-to-End Verification

The mocked unit test suite (`make build && go test ./...`) proves every
capability's logic against fake/mocked Cursor backends without a live
account. Per the ralplan-approved plan's two-tier verification strategy
(fact-r3-acceptance), the plugin is only considered fully done once it
has also been checked once against a **real** Cursor account through a
**real** CLIProxyAPI instance. This document is that manual checklist.

## Protocol implementation note (post-boundary-review rework)

An initial implementation of the executor used connect-go's generated
**unary** `Run` client. A terminal review verified against
`../gajae-code/packages/ai/src/providers/cursor.ts` that Cursor's real
`Run` exchange is **not** a simple request/response call: it is a
bidirectional HTTP/2 stream where the server can send multiple top-level
messages and can request the client to resolve content-addressed blob
IDs mid-exchange (the `KvServerMessage`/`KvClientMessage`
`getBlobArgs`/`setBlobArgs` handshake) before continuing. The unary-only
implementation silently dropped conversation history, mis-encoded tool
results, and only ever consumed the first server message.

The executor (`internal/executor/stream.go`) now performs the real
exchange: a genuine HTTP/2 duplex request, Cursor's own Connect streaming
frame format, a content-addressed blob store
(`internal/executor/blobstore.go`), and the `rootPromptMessagesJson`
field (which is what Cursor's server actually reads to build the model
prompt - `turns[]` is a UI-history view only). This is covered by tests
using a real HTTP/2 test server (`h2c`), not a substituted unary mock -
see `internal/executor/executor_test.go` and `stream_test.go`.

**Known remaining gap:** `x_cursor_client_version` is declared as a
plugin `ConfigField` (so the management UI can display/edit it), but
`plugin.reconfigure` does not yet parse and apply a live override to the
running client - the CLIProxyAPI SDK version this plugin builds against
does not expose a per-plugin config payload in its public lifecycle
request shape. The compiled-in default is kept in sync with gajae-code's
`CURSOR_CLIENT_VERSION` at the time of writing
(`internal/executor/client.go`), and should be re-verified/updated
periodically, since Cursor gates backend features on this header.

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
- If the exchange required a Cursor-initiated blob request (this can
  happen depending on conversation length/complexity), confirm the
  response still comes back successfully rather than hanging or erroring
  — this validates the real KV handshake against Cursor's actual
  backend, not just the local `h2c` test server.

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
