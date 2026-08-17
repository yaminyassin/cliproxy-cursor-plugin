# Real End-to-End Verification

The mocked unit test suite (`make build && go test ./...`) proves every
capability's logic against fake/mocked Cursor backends without a live
account. Per the ralplan-approved plan's two-tier verification strategy
(fact-r3-acceptance), the plugin is only considered fully done once it
has also been checked once against a **real** Cursor account through a
**real** CLIProxyAPI instance. This document is that manual checklist.

## Known v1 scope limitation: buffered, not incremental, streaming

`executor.execute_stream` is implemented on top of Cursor's generated
Connect-RPC `Run`/`RunSSE` client methods, both of which are **unary**
RPCs in the codegen this plugin uses (see `internal/cursorpb/README.md`
and the ADR in the approved plan) — Cursor's own low-level incremental
multiplexed event stream (the way gajae-code itself consumes it) is built
on hand-rolled raw HTTP/2 frame parsing outside this codegen path, which
is explicitly out of scope for v1 per the plan's ADR.

Concretely: **one `Run` call currently surfaces at most one
`InteractionUpdate` from Cursor's response into the chat-completions
reply.** For a Cursor turn that emits multiple discrete update events
(for example, a text delta followed by a separate tool-call-completed
event as distinct top-level `AgentServerMessage`s rather than one
combined message), only the first captured update is translated; later
updates in the same underlying Cursor response are not yet accumulated.
The per-turn tool-call *batching* behavior itself (multiple tool calls
arriving within a single `InteractionUpdate`) is fully implemented and
tested (`TestResponseAccumulator_BatchesMultipleToolCallsIntoOneArray`).
Step 4 below is the specific manual check for this boundary: if a real
Cursor tool-using turn spans multiple top-level server messages, confirm
whether this manifests as a truncated response in practice, and file a
follow-up story to accumulate across `Run`'s full response if so.

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
  produces a follow-up response that accounts for the tool result) — the
  translation layer itself is unit-tested
  (`TestBuildAgentRunRequest_ToolResultRoundTrip`), but this step
  verifies Cursor's real backend actually accepts and acts on the
  re-encoded tool result, which the mock cannot prove.
- Watch specifically for the buffered-streaming limitation documented
  above: if Cursor emits the tool call and a text response as separate
  top-level messages within one turn, confirm whether both arrive in the
  chat-completions response or only the first.

This validates fact-r5-tool-roundtrip end-to-end, beyond what the mocked
tests can prove on their own (they verify the translation logic in
isolation; this step verifies it against Cursor's real backend and a
real downstream client).

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
