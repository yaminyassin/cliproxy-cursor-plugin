# cliproxy-cursor-plugin

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) plugin that adds **Cursor** as an
OAuth-authenticated provider: real PKCE browser login, dynamic model discovery, and
chat-completions ↔ Cursor Connect/gRPC protocol translation — including native tool call
surfacing and client-supplied custom tool (MCP) support.

Built as a single combined Go cgo plugin implementing CLIProxyAPI's C ABI v1 plugin system:
`auth_provider` + `executor` + `model_registrar` + `model_provider` capabilities in one binary.

The plugin is a **translator, not an agent**: it never executes Cursor's native tools itself.
Tool calls (native or custom) are always surfaced to your own downstream client as standard
OpenAI-style `tool_calls` entries for your client to execute and report back.

## What this gets you

Once installed, any OpenAI-compatible client pointed at your CLIProxyAPI instance can use
real Cursor models (Grok, GPT-5.x, Claude Sonnet, Gemini, Kimi, GLM, and whatever else your
Cursor plan exposes) through the standard `/v1/chat/completions` and `/v1/models` endpoints,
authenticated via your own Cursor account — no separate Cursor IDE session required.

## Prerequisites

- Go 1.26+ (to build the plugin)
- A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) installation (v7.2.134 or
  compatible) — either the released binary or built from source
- A Cursor account (any active subscription able to log in via browser)

## Installation

### 1. Build the plugin

```sh
git clone https://github.com/yaminyassin/cliproxy-cursor-plugin.git
cd cliproxy-cursor-plugin
make build
```

This produces a platform-appropriate shared library under `bin/`:
`cursor.dylib` (macOS), `cursor.so` (Linux), or `cursor.dll` (Windows).

### 2. Install it into CLIProxyAPI

CLIProxyAPI loads plugins from its configured `plugins.dir` (default `plugins`) by filename.
Copy the built binary there:

```sh
cp bin/cursor.dylib /path/to/your/cliproxyapi/plugins/   # adjust extension for your platform
```

### 3. Wire it into `config.yaml`

Merge this into your existing CLIProxyAPI `config.yaml` (see
[`config/plugins-config-example.yaml`](config/plugins-config-example.yaml) for the annotated
version):

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cursor:
      enabled: true
      priority: 1
      # Optional: override the compiled-in x-cursor-client-version header.
      # x_cursor_client_version: "cli-2026.02.13-41ac335"
```

### 4. Start CLIProxyAPI

```sh
./cliproxy-api -config config.yaml
```

You should see plugin-load log lines confirming the plugin registered:

```
pluginhost: plugin loaded plugin_id=cursor path=/path/to/plugins/cursor.dylib
pluginhost: plugin registered plugin_id=cursor plugin_name=cursor version=0.1.0 ...
```

## Usage

### Log in with your Cursor account

CLIProxyAPI's Management API exposes a login-URL route for every plugin-provided auth
provider, named `<provider-id>-auth-url`:

```sh
curl -H "Authorization: Bearer <your-management-secret-key>" \
  http://127.0.0.1:8317/v0/management/cursor-auth-url
```

This returns a real `cursor.com/loginDeepControl` URL and a `state` token:

```json
{"state":"...", "status":"ok", "url":"https://cursor.com/loginDeepControl?..."}
```

Open the URL in a browser and complete the login. Then poll for completion:

```sh
curl -H "Authorization: Bearer <your-management-secret-key>" \
  "http://127.0.0.1:8317/v0/management/get-auth-status?state=<state-from-above>"
```

Poll every few seconds until you get a terminal `success` or `error` status (host-conformant:
5-second interval, 15-minute ceiling). On success, CLIProxyAPI writes a credential file into
its configured `auth-dir` and the account becomes usable immediately.

### List available Cursor models

```sh
curl http://127.0.0.1:8317/v1/models -H "Authorization: Bearer <your-cliproxyapi-api-key>"
```

Models are discovered dynamically per-account via Cursor's real model catalog — you'll see
whatever your specific Cursor plan actually has access to.

### Query current Cursor usage through the management backend

Cursor credentials expose only their current access token through CPA's canonical
`metadata.access_token` field. This enables server-side `$TOKEN$` substitution without
returning the token or refresh material to the dashboard:

```sh
curl -H "Authorization: Bearer <your-management-secret-key>" \
  -H "Content-Type: application/json" \
  http://127.0.0.1:8317/v0/management/api-call \
  -d '{
    "auth_index": "<cursor-auth-index>",
    "method": "POST",
    "url": "https://api2.cursor.sh/aiserver.v1.DashboardService/GetCurrentPeriodUsage",
    "header": {
      "Authorization": "Bearer $TOKEN$",
      "Content-Type": "application/json",
      "Connect-Protocol-Version": "1",
      "x-cursor-client-type": "cli"
    },
    "data": "{}"
  }'
```

The response includes the current billing window and plan usage fields such as total,
included, remaining, and limit spend. Cursor's current dashboard maps
`planUsage.autoPercentUsed` to **Cursor Models** and `planUsage.apiPercentUsed` to
**Other Models**. This DashboardService RPC is used by Cursor's own clients but is not a
documented public API, so its path or response schema may change.

See the living [Cursor quota compatibility record](docs/cursor-quota-management-research.md)
for primary-source links, field mappings, security boundaries, and the revalidation runbook.

### Send a chat request

```sh
curl http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer <your-cliproxyapi-api-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "cursor-grok-4.6-high",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

### Custom tools (client-supplied function calling)

Pass a standard OpenAI-style `tools` array; the plugin translates it into Cursor's
`AgentRunRequest.McpTools` and instructs the model to only call your declared tools (never
Cursor's own native shell/file tools):

```sh
curl http://127.0.0.1:8317/v1/chat/completions \
  -H "Authorization: Bearer <your-cliproxyapi-api-key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "cursor-grok-4.6-high",
    "messages": [{"role": "user", "content": "What is the weather in Seoul?"}],
    "tools": [{
      "type": "function",
      "function": {
        "name": "get_weather",
        "description": "Gets the current weather for a named city",
        "parameters": {
          "type": "object",
          "properties": {"city": {"type": "string"}},
          "required": ["city"]
        }
      }
    }]
  }'
```

The model's tool call comes back as a standard `tool_calls` entry under **your own declared
tool name** (e.g. `function.name: "get_weather"`) with clean JSON arguments. Your client is
responsible for actually executing it and sending the result back as a
`{"role": "tool", "tool_call_id": "...", "content": "..."}` message in the next request,
exactly like OpenAI's own tool-calling contract. The plugin re-encodes that result into
Cursor's protocol so the model can use it on the next turn.

## What's implemented

- **auth_provider**: PKCE browser login (`auth.login.start`/`auth.login.poll`), token
  storage/refresh (singleflight-deduplicated, degrades the account on failure rather than
  silently dropping it).
- **executor**: real bidirectional HTTP/2 duplex exchange with Cursor's backend (content-
  addressed blob store, `KvServerMessage` and `ExecServerMessage` handshakes) — not a
  unary/simplified approximation. Conversation history, multi-turn context, and tool-call
  round-trips are all wired through Cursor's actual wire protocol.
- **model_registrar / model_provider**: dynamic per-account model discovery via Cursor's
  `GetUsableModels`.
- Custom client-supplied tools (`tools[]` → Cursor `McpTools`), with an automatic
  system-prompt instruction so the model prefers your declared tools over Cursor's native
  ones.

## Known limitations

- **Client-facing streaming is buffered per turn, not token-by-token.** The wire format is
  correct OpenAI streaming (`object: "chat.completion.chunk"` with `choices[].delta` plus a
  terminal `usage` chunk), so standard streaming clients parse it normally — but the full
  Cursor turn completes before the first chunk is emitted, so you get the whole answer at
  once rather than progressive tokens. See [`docs/E2E.md`](docs/E2E.md) for detail.
- The plugin never executes tools itself, by design — native Cursor tool-execution requests
  (`ExecServerMessage`) are always declined so your own client stays the sole executor.
- `x_cursor_client_version` reconfiguration via the Management API's `config_yaml` payload is
  wired and tested, but CLIProxyAPI's own lifecycle-payload shape can change between
  versions; re-verify after upgrading CLIProxyAPI.

## Development

```sh
make build      # build the plugin
make generate   # regenerate internal/cursorpb from the Cursor protobuf descriptor
make clean      # remove build artifacts
go test ./...   # run the full mocked unit test suite (no live Cursor account needed)
```

See [`internal/cursorpb/README.md`](internal/cursorpb/README.md) for how the Cursor protobuf
schema was extracted, and [`docs/E2E.md`](docs/E2E.md) for the full manual real-account
verification checklist (including a real end-to-end run log against a live Cursor account).

`cmd/e2eprobe` is a standalone CLI for driving this plugin's real auth/executor/discovery code
directly against Cursor's backend without a full CLIProxyAPI instance — useful for isolated
debugging:

```sh
go run ./cmd/e2eprobe login
go run ./cmd/e2eprobe chat "hello"
go run ./cmd/e2eprobe models
go run ./cmd/e2eprobe mcptool
```

## License

See [LICENSE](LICENSE) if present in this repository, or the repository's license terms.
