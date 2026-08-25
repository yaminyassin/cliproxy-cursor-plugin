# Cursor quota integration compatibility record

> Living document. Update this file whenever Cursor changes its dashboard,
> private usage RPC, authentication flow, response fields, pool names, or
> pricing model. Never add access tokens, management keys, cookies, or account
> identifiers.

| Record | Value |
| --- | --- |
| First researched | 2026-08-25 |
| Last end-to-end validation | 2026-08-25 |
| Last public-link audit | 2026-08-25 |
| Cursor Agent build inspected | `2026.08.11-e8db854` |
| Cursor dashboard bundle inspected | `3iat84zr3gtbx.js` |
| Cursor plan validated | Individual Ultra |
| Maintained plugin repository | [yaminyassin/cliproxy-cursor-plugin](https://github.com/yaminyassin/cliproxy-cursor-plugin) |
| Durable CPA maintenance contract | [cpa-durable-maintenance.md](cpa-durable-maintenance.md) |
| Maintained Management Center repository | [yaminyassin/Cli-Proxy-API-Management-Center](https://github.com/yaminyassin/Cli-Proxy-API-Management-Center) |
| CPA SDK/API version pinned by this plugin | `v7.2.134` |
| Live CPA build validated | `v7.2.140` via Homebrew |
| Cursor plugin version validated | `0.2.0` |
| Status | Working, but dependent on a private Cursor contract |

## Contract snapshot

This is the shortest current description of the integration. The evidence and
limitations behind it are recorded in the sections below.

| Concern | Current contract |
| --- | --- |
| Upstream method | `POST https://api2.cursor.sh/aiserver.v1.DashboardService/GetCurrentPeriodUsage` |
| Authentication | Current Cursor OAuth access token in `Authorization: Bearer <token>` |
| CPA selection | Stable credential `auth_index` |
| CPA substitution | `Authorization: Bearer $TOKEN$` resolved from `metadata.access_token` |
| Dashboard transport | Connect JSON with body `{}` |
| Headers in validated request | `Content-Type: application/json`, `Connect-Protocol-Version: 1`, `x-cursor-client-type: cli` |
| Billing timestamps | Unix milliseconds, currently encoded as decimal strings |
| Money | Integer cents |
| Cursor Models | `planUsage.autoPercentUsed` |
| Other Models | `planUsage.apiPercentUsed` |
| Missing `planUsage` | Unavailable/unsupported, never 0% used |
| Browser dashboard route | `POST https://cursor.com/api/dashboard/get-current-period-usage`, WorkOS browser session only |

The signed-in reference page is
[Cursor Spending: Included in Ultra](https://cursor.com/dashboard/spending#included-in-ultra).

## Verdict

Cursor does expose current plan and on-demand usage to its own CLI, including a
remaining amount and billing-cycle end, through an account-scoped private RPC.
It does **not** document that RPC as a public API. Cursor's supported SDK and
Cloud Agents usage APIs are agent/run-scoped billing records, not an account's
remaining subscription allowance.

CLIProxyAPI (CPA) has now been live-validated calling the private RPC with a
stable `auth_index` and server-side `$TOKEN$` replacement from
`metadata.access_token`. That keeps the token out of the dashboard's response
in the normal flow, but the generic CPA endpoint is not a least-privilege proxy:
it accepts any absolute destination and arbitrary headers. Use this only as an
experimental, fail-soft integration with a fixed destination. A provider-scoped,
allowlisted adapter is the safer production shape.

## Evidence matrix

| Surface | What it returns | Authentication | Individual remaining allowance? | Status and CPA fit |
| --- | --- | --- | --- | --- |
| Cursor Spending dashboard/editor | Real-time usage for both current pools, remaining allowance, on-demand charges, and reset date | Signed-in Cursor account | Yes | Supported UI, but Cursor documents no programmatic individual-usage endpoint. See [Usage and limits](https://cursor.com/help/models-and-usage/usage-limits) and [Models & Pricing](https://cursor.com/docs/models-and-pricing#usage-pools). |
| Python SDK `agent.get_usage()` / SDK Bridge `GetUsage` | Billed token usage and cost for one agent, with run/turn breakdown | User or service-account API key; Team Admin API keys are unsupported | No | `charged_cents == 0` is ambiguous among plan-included, BYOK, and credit-grant usage. It has no plan limit, remaining amount, or reset date. See [Python SDK authentication and billing](https://cursor.com/docs/sdk/python#authentication), [`agent.get_usage()`](https://cursor.com/docs/sdk/python#agentget_usage), and the first-party bridge [`GetUsage` schema](https://github.com/cursor/sdk-bridge/blob/260a73d33f906abe9f4adfde486bbdeb133344b7/proto/sdk/v1/sdk_agent_service.proto#L54-L56). |
| Cloud Agents REST `GET /v1/agents/{id}/usage` | Token totals and per-run token usage for one cloud agent | Generated user or service-account API key via Basic or Bearer auth | No | Supported public beta API, but it cannot power a plan-quota card. See [Cloud Agents API authentication and Get Agent Usage](https://cursor.com/docs/cloud-agent/api/endpoints#get-agent-usage). |
| Team Admin API `POST /teams/spend` | Current-cycle per-member on-demand spend, overall spend, and configured/effective spend limits | Team Admin API key via Basic auth | No, not the two individual plan pools | Useful for a separate team-admin view. It requires a different credential class from the plugin's user access token. See [Admin API](https://cursor.com/docs/account/teams/admin-api#get-spending-data). |
| Organization API `POST /organizations/pooled-usage` | Organization pool limit, used, remaining, contract window, and per-team breakdown | Enterprise Organization API key with `usage:*`, via Basic auth | No, organization pool only | Supported and quota-shaped, but not an individual subscription endpoint. See [Get Pooled Usage](https://cursor.com/docs/account/organizations/organization-admin-api#get-pooled-usage). |
| Cursor CLI private `DashboardService/GetCurrentPeriodUsage` | Billing window, plan spend/limit/remaining and on-demand/spend-limit fields | Current Cursor CLI access token via Bearer auth | Yes for plans returning `planUsage` | Cursor documents the `/usage` experience in its [July 13, 2026 CLI release](https://cursor.com/docs/cli/changelog#july-13-2026-release). Direct Connect JSON and CPA-mediated calls returned HTTP 200, but the underlying RPC remains private and undocumented. |

The public SDK's account-level namespace currently lists only identity, models,
and repositories, not plan usage. The public bridge schema also exposes
`USAGE_LIMIT_EXCEEDED` and optional rate-limit `limit`, `remaining`, and `reset`
only as error metadata; that is reactive failure information, not a proactive
subscription quota query. See [the Cursor namespace](https://cursor.com/docs/sdk/python#the-cursor-namespace)
and the first-party [`SdkErrorDetails` schema](https://github.com/cursor/sdk-bridge/blob/260a73d33f906abe9f4adfde486bbdeb133344b7/proto/sdk/v1/sdk_errors.proto#L14-L65).

There is a smaller public-docs inconsistency: the pinned bridge proto says
`GetUsage` is cloud-only, while the current Python SDK page says local agents
return a per-turn breakdown. Both descriptions remain agent-scoped and neither
exposes subscription remaining allowance.

## Current first-party CLI evidence

Read-only inspection used the installed official Cursor Agent build
`2026.08.11-e8db854`. The inspected files were under
`~/.local/share/cursor-agent/versions/2026.08.11-e8db854/`.

- `6260.index.js` contains `src/usage/usage-command.ts`, which registers `/usage`
  as **Show plan and on-demand usage**. `src/usage/usage-data.ts` calls
  `getCurrentPeriodUsage(GetCurrentPeriodUsageRequest({}))`, `getHardLimit`, and
  `getPlanInfo` in parallel.
- The generated first-party descriptor in `index.js` identifies
  `aiserver.v1.DashboardService` and unary method `GetCurrentPeriodUsage`. A
  Connect client therefore targets:

  ```text
  POST https://api2.cursor.sh/aiserver.v1.DashboardService/GetCurrentPeriodUsage
  ```

- Its empty request has optional `team_id` and `include_pooled_usage` fields.
  The response includes `billing_cycle_start`, `billing_cycle_end`, `plan_usage`,
  and `spend_limit_usage`.
- `plan_usage` contains `total_spend`, `included_spend`, `bonus_spend`,
  `remaining`, `limit`, optional Auto/API spend and limits, and percent-used
  fields. `spend_limit_usage` contains pooled, individual, and overall limit,
  used, and remaining fields. The CLI divides these monetary integers by 100,
  so it treats them as cents.
- The CLI treats missing `planUsage` as a real plan variant, not as zero usage.
  For an Enterprise account it falls back to `GetMe`,
  `GetMonthlyBillingCycle`, and `GetAggregatedUsageEvents`; otherwise it reports
  that details are unavailable for that plan.
- Cursor's current spending-dashboard bundle supplies `planUsage.autoPercentUsed`
  to the row titled `Cursor Models` and `planUsage.apiPercentUsed` to the row
  titled `Other Models`. This validates those two display mappings for the
  inspected dashboard build. The private field names still have no published
  compatibility guarantee. The exact first-party bundle was
  [`3iat84zr3gtbx.js`](https://cursor.com/_next/static/immutable/chunks/3iat84zr3gtbx.js),
  2,274,613 bytes with SHA-256
  `7874dd80666ea736686532e83789e08204004b33cbf429b953299eaa48dd6c4d`.
  It contains `/api/dashboard/get-current-period-usage`, maps the two RPC fields
  to `autoPercentage` and `apiPercentage`, defines the two visible titles, and
  renders each title with its corresponding percentage.

The same first-party bundle implements the PKCE browser flow used by this
plugin: `loginDeepControl`, `/auth/poll`, and a returned `accessToken`. Its
dashboard transport obtains that current access token and sets
`Authorization: Bearer <accessToken>`. When the CLI starts from a user API key,
it first exchanges the key at `/auth/exchange_user_api_key` for an access/refresh
token pair. This validates the plugin's short-lived Cursor access token as the
right credential class for the private RPC; a raw Team Admin API key is not.

The exact first-party wire observed is Connect unary over HTTP/1.1 with binary
protobuf by default:

```text
Authorization: Bearer <current Cursor access token>
Content-Type: application/proto
Connect-Protocol-Version: 1
x-cursor-client-type: cli
body: empty serialized GetCurrentPeriodUsageRequest
```

The client also adds `x-cursor-client-version`, `x-ghost-mode`, and
`x-request-id`; whether the server requires them for this RPC is unknown.

2026-08-25 bundle evidence fingerprints follow. Append a new dated block when
Cursor changes; do not replace old fingerprints.

```text
6aceb24b7c7ecddb1993946ebb18a7dd4d025842e6efda955eb0c13255b1e5f0  index.js
285e3f24126b457872064e9661d76ab0e35a0059256ffa4ab44507821efe334e  6260.index.js
7874dd80666ea736686532e83789e08204004b33cbf429b953299eaa48dd6c4d  3iat84zr3gtbx.js (2,274,613 bytes)
```

Cursor now documents the current user-facing behavior in its
[July 13, 2026 CLI release](https://cursor.com/docs/cli/changelog#july-13-2026-release):
`/usage` displays included-usage meters with Auto/API breakdowns, on-demand
spend and limit, plan name, reset date, and Enterprise pooled charts. The
separate [CLI slash-command reference](https://cursor.com/docs/cli/reference/slash-commands)
still does not list `/usage`. A January 2026
[first-party CLI changelog](https://cursor.com/changelog/cli-jan-16-2026)
records the older streaks-and-stats form, which shows that the command's meaning
has already changed. Cursor documents the feature, but not its RPC path, schema,
headers, JSON compatibility, or plan coverage. Those private details still have
no published compatibility guarantee.

## Live validation

The complete JSON path was subsequently validated without recording any token
or secret value in this report:

1. A direct Connect JSON `POST` to
   `DashboardService/GetCurrentPeriodUsage`, using an existing Cursor access
   token, returned HTTP 200.
2. The plugin metadata bridge was built and installed, then a fresh Cursor OAuth
   login was completed through CPA. CPA assigned the credential a stable
   `auth_index`.
3. `POST /v0/management/api-call` selected that `auth_index`, substituted its
   `metadata.access_token` into `Authorization: Bearer $TOKEN$`, and reported an
   upstream Cursor HTTP 200.
4. The returned JSON included `billingCycleStart`, `billingCycleEnd`, and
   `planUsage` with `totalSpend`, `includedSpend`, `remaining`, `limit`, and
   percent-used fields. One redacted safe sample was:

   ```json
   {
     "planUsage": {
       "totalSpend": 4180,
       "includedSpend": 4180,
       "remaining": 35820,
       "limit": 40000
     }
   }
   ```

These amounts are cents, so that sample reconciles as 4,180 used plus 35,820
remaining against a 40,000 limit. This proves Connect JSON acceptance and the
end-to-end CPA metadata/token-substitution path for the tested account and
current Cursor/CPA builds. It does not turn the private method into a supported
or stable public contract. This validation used an individual Ultra plan;
response shape and fallback behavior remain unproven for other plans.

The signed-in web dashboard was also inspected at
`/dashboard/spending#included-in-ultra`. Its private
`POST /api/dashboard/get-current-period-usage` route is protected by the WorkOS
browser session: presenting the CPA Cursor OAuth bearer directly redirected to
WorkOS login. The page's compiled client maps the direct RPC fields as follows:

| Dashboard row | Private RPC field |
| --- | --- |
| Cursor Models | `planUsage.autoPercentUsed` |
| Other Models | `planUsage.apiPercentUsed` |

CPA therefore calls the underlying authenticated DashboardService RPC rather
than attempting to replay browser cookies or scrape rendered page text.

2026-08-25 deployed-state fingerprints:

```text
Plugin repository base HEAD:        5096f34c1fd2f3f36347638894e85df73fbfb192
Management Center base HEAD:        6586f88858ca27e840bd8db2630dccd371a1cd4a
Installed cursor.dylib SHA-256:      b115692045d488d07a492e9bf2e6fad019012d62ce2037d360a5ac7a9964d7ca
Served management.html SHA-256:      94831fed40ae9d6f46e78089f22fea143176c965cd287ff7127af9562a9741cb
```

Both repositories contained uncommitted implementation changes, so the base
HEAD values are reference points, not complete source identifiers. The deployed
artifact hashes are the authoritative fingerprints for the validated binaries.
Append future dated blocks instead of overwriting this one.

## CPA proxy fit and security boundary

This checkout pins CPA `v7.2.134`. Its
[`POST /v0/management/api-call` implementation](https://github.com/router-for-me/CLIProxyAPI/blob/v7.2.134/internal/api/handlers/management/api_tools.go#L29-L214):

1. selects an auth record by `auth_index`;
2. replaces `$TOKEN$` in request headers, preferring
   `metadata.access_token`;
3. sends the caller-supplied method, absolute URL, headers, and string body; and
4. returns the upstream status, headers, and body as JSON.

As inspected, the Cursor provider now mirrors only its current access token to
`metadata.access_token` on parse, login, and refresh. Refresh material remains
in plugin-owned `StorageJSON`. This matches the neighboring Copilot plugin's
quota-dashboard pattern and ensures scheduled refreshes update the token used by
CPA. CPA does not perform a Cursor-specific refresh inside `/api-call`, so host
refresh scheduling still matters.

The important limitations are:

- **Destination is not constrained.** CPA validates only that the URL has a
  scheme and host. A caller holding the management key can ask it to substitute
  a selected credential into a request to another host. Management API access
  must therefore be treated as credential-level privilege. A dashboard must
  hard-code the exact HTTPS host/path and never accept URL or header overrides
  from UI input; a host-side allowlist would be stronger.
- **Cursor's first-party CLI uses binary, while the dashboard path uses Connect
  JSON.** Binary protobuf is not a safe dashboard payload through CPA's generic
  JSON envelope. The alternative request using `Content-Type: application/json`,
  `Connect-Protocol-Version: 1`, and `{}` returned HTTP 200 both directly and
  through CPA, with a lower-camel JSON response. This is proven for the tested
  account/current builds but remains outside Cursor's public compatibility
  contract.
- **The contract is private.** Endpoint, protobuf fields, required headers, and
  plan-specific behavior can change independently of the public SDK.
- **Plan absence is not zero.** `planUsage == undefined`, auth failures, schema
  drift, or unsupported plans must render an unavailable state, not 0% used.

## Current implementation and recommendation

The installed dashboard adapter pins the exact `api2.cursor.sh` method and
headers, accepts no destination or header input from the UI, and treats a
missing or unrecognized `planUsage` object as unavailable. It normalizes the
billing-cycle end, aggregate included used/limit/remaining amounts, and the
dashboard's `Cursor Models` and `Other Models` percentages.

There is one known display-parity gap. The inspected Cursor dashboard applies
`value > 0 && value < 1 ? 1 : Math.min(value, 100)` before rounding a usage
percentage. The CPA adapter currently rounds directly. The validated live
values still render identically, but a raw `0.4%` would display as `1%` in
Cursor and `0%` in CPA, while Cursor caps values above `100%`. Treat exact
display parity for those edge cases as open implementation work, not a proven
property of the current adapter.

For a production integration, prefer a provider-scoped quota operation that
fixes and allowlists the destination, sends the typed protobuf request, decodes
the protobuf response server-side, caps response size, and returns normalized
JSON. Keep Team/Organization Admin API support as a separate explicit credential
feature; never place those higher-privilege keys in a user's OAuth metadata.

## Implementation map

### Cursor plugin repository

- [`internal/auth/storage.go`](../internal/auth/storage.go) builds the minimal
  host metadata containing only the provider type and current access token.
- [`internal/auth/provider.go`](../internal/auth/provider.go) refreshes that
  metadata on parse, login, and token refresh.
- [`internal/dispatch/dispatch.go`](../internal/dispatch/dispatch.go) publishes
  plugin version `0.2.0` and the official Cursor icon URL in management data.
- [`README.md`](../README.md) contains the manual CPA request example.

### CPA Management Center companion checkout

The local integration is in
`/Users/yaminyassin/work/Cli-Proxy-API-Management-Center`. Its relevant paths
are:

- `src/features/quota/providers/cursor/data.ts`: fixed upstream request,
  defensive response parser, pool mapping, and unavailable-state behavior.
- `src/features/quota/providers/cursor/CursorQuotaBody.tsx`: quota card display.
- `src/features/quota/quotaTimelineModel.ts`: billing-cycle timeline mapping.
- `src/assets/icons/cursor.svg`: local Cursor mark for auth and quota cards.
- `src/utils/quota/constants.ts`: endpoint and request headers.
- `tests/cursorQuota.test.ts`: request, schema, rendering, timeline, and
  classification contract tests.

Maintained forks:

- [Cursor plugin](https://github.com/yaminyassin/cliproxy-cursor-plugin)
- [CLI Proxy API Management Center](https://github.com/yaminyassin/Cli-Proxy-API-Management-Center)

Upstream projects retained as local `upstream` remotes:

- [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)
- [CLI Proxy API Management Center](https://github.com/router-for-me/Cli-Proxy-API-Management-Center)
- [Original Cursor plugin](https://github.com/Yeachan-Heo/cliproxy-cursor-plugin)

Plugin registration metadata and the README now advertise the maintained
`yaminyassin` fork. The Go module path remains the upstream
`github.com/router-for-me/cliproxy-cursor-plugin` identifier for source
compatibility; changing the module path is a separate migration.

## Primary-source URL index

### Cursor account usage and pricing

- [Spending dashboard: Included in Ultra](https://cursor.com/dashboard/spending#included-in-ultra)
- [Fingerprinted spending-dashboard bundle](https://cursor.com/_next/static/immutable/chunks/3iat84zr3gtbx.js)
- [Usage and limits](https://cursor.com/help/models-and-usage/usage-limits)
- [Models and Pricing: usage pools](https://cursor.com/docs/models-and-pricing#usage-pools)
- [Official light favicon used by plugin metadata](https://cursor.com/marketing-static/favicon-light.svg)
- [Official dark favicon](https://cursor.com/marketing-static/favicon.svg)

### Cursor supported programmatic usage surfaces

- [Python SDK authentication and billing](https://cursor.com/docs/sdk/python#authentication)
- [Python SDK `agent.get_usage()`](https://cursor.com/docs/sdk/python#agentget_usage)
- [Python SDK Cursor namespace](https://cursor.com/docs/sdk/python#the-cursor-namespace)
- [Cloud Agents API: Get Agent Usage](https://cursor.com/docs/cloud-agent/api/endpoints#get-agent-usage)
- [Team Admin API: Get Spending Data](https://cursor.com/docs/account/teams/admin-api#get-spending-data)
- [Organization Admin API: Get Pooled Usage](https://cursor.com/docs/account/organizations/organization-admin-api#get-pooled-usage)

### Cursor first-party CLI and schemas

- [CLI installation and updates](https://cursor.com/docs/cli/installation)
- [CLI authentication](https://cursor.com/docs/cli/reference/authentication)
- [CLI changelog, including the July 13, 2026 `/usage` release](https://cursor.com/docs/cli/changelog#july-13-2026-release)
- [CLI slash commands](https://cursor.com/docs/cli/reference/slash-commands)
- [CLI January 16, 2026 changelog](https://cursor.com/changelog/cli-jan-16-2026)
- [SDK Bridge `GetUsage` schema, pinned revision](https://github.com/cursor/sdk-bridge/blob/260a73d33f906abe9f4adfde486bbdeb133344b7/proto/sdk/v1/sdk_agent_service.proto#L54-L56)
- [SDK Bridge error and rate-limit schema, pinned revision](https://github.com/cursor/sdk-bridge/blob/260a73d33f906abe9f4adfde486bbdeb133344b7/proto/sdk/v1/sdk_errors.proto#L14-L65)

### Observed private Cursor routes

These are clickable so they remain easy to compare, but they are undocumented
API routes, not supported browser pages. Do not expect an anonymous `GET` to
succeed.

- [Cursor Agent DashboardService usage RPC](https://api2.cursor.sh/aiserver.v1.DashboardService/GetCurrentPeriodUsage)
- [Cursor website current-period usage route](https://cursor.com/api/dashboard/get-current-period-usage)

### Operational status

- [Cursor status summary API](https://status.cursor.com/api/v2/summary.json)

### CPA contract

- [CPA v7.2.134 management `api-call` implementation](https://github.com/router-for-me/CLIProxyAPI/blob/v7.2.134/internal/api/handlers/management/api_tools.go#L29-L214)

The private RPC and browser route are included in the contract snapshot, but
they are not documentation links and have no published compatibility promise.

## Public-link audit

An anonymous HTTP link check on 2026-08-25 returned HTTP 200 for every linked
Cursor documentation page, pinned SDK Bridge source, CPA source, icon asset,
and reachable project repository except these intentional or tracked cases:

- The signed-in spending dashboard returned HTTP 403 without browser-session
  cookies. Validate it in an authenticated browser; do not weaken or work around
  its WorkOS session boundary.
- The two observed private API routes returned HTTP 405 to anonymous
  browser-style `GET` requests. Validate them only with the documented
  method/credential boundary in this record, and never place credentials in a
  URL.

Repeat the anonymous check when updating this record, then manually open the
spending dashboard in a signed-in browser.

## Compatibility watch list

When Cursor changes, compare each item below before changing the adapter:

1. **Service and method:** confirm `aiserver.v1.DashboardService` still exposes
   `GetCurrentPeriodUsage` and whether its request remains empty for individual
   usage.
2. **Transport:** confirm Connect JSON is still accepted. The CLI may continue
   to use binary protobuf even if JSON stops working.
3. **Authentication:** confirm a refreshed Cursor OAuth access token still works
   directly. Do not substitute browser cookies or Team Admin keys.
4. **Headers:** test whether `Connect-Protocol-Version` and
   `x-cursor-client-type` remain sufficient, and record any newly required
   client-version headers.
5. **Response casing:** watch for changes between lower-camel Connect JSON and
   protobuf snake-case names.
6. **Units:** verify billing timestamps are still milliseconds and monetary
   values are still cents.
7. **Plan shape:** verify whether `planUsage` exists for each plan. Missing data
   must remain unavailable rather than zero.
8. **Pool mapping:** inspect the signed-in spending dashboard bundle and verify
   that `autoPercentUsed` still labels Cursor Models and `apiPercentUsed` still
   labels Other Models.
9. **New fields:** capture bonus, pooled, on-demand, and spend-limit changes
   before deciding whether they belong in the UI.
10. **Brand asset:** verify the icon URL still resolves and update the local SVG
    only when Cursor changes its mark.

## Revalidation runbook

### 1. Record the versions under test

```sh
agent --version
brew list --versions cliproxyapi
git -C /path/to/cliproxy-cursor-plugin rev-parse HEAD
git -C /path/to/Cli-Proxy-API-Management-Center rev-parse HEAD
shasum -a 256 /path/to/installed/cursor.dylib
curl -fsS http://127.0.0.1:8317/management.html | shasum -a 256
```

The local installation inspected for this record exposes the same CLI as
`cursor-agent`, so use `cursor-agent --version` if `agent` is not on `PATH`.
Cursor documents `agent update` as the manual update command and also enables
automatic updates by default. Record the version before inspecting or updating
it, then record the Cursor Agent bundle location and SHA-256 hashes of the
inspected files. Never copy the access token into this document.

### 2. Inspect first-party behavior

1. Open the signed-in [spending dashboard](https://cursor.com/dashboard/spending#included-in-ultra).
2. Use `agent status` only to confirm the intended account is authenticated;
   never copy its account details into this record.
3. Run `/usage` interactively and record only the plan shape, pool labels,
   amounts, and reset date needed for comparison.
4. Record the visible dashboard pool labels, percentages, included amount,
   on-demand amount, and reset date.
5. Inspect the current Cursor Agent descriptor/bundle for
   `DashboardService`, `GetCurrentPeriodUsage`, request fields, response fields,
   and headers.
6. Inspect the dashboard bundle for the field-to-label mapping. Do not infer a
   mapping only from similar names.

### 3. Probe through CPA

Use a stable Cursor `auth_index`; do not use a filename or plugin ID.

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

Save only a redacted structural sample. Safe fields include HTTP status,
billing-cycle timestamps, plan amount fields, and percentages. Remove account,
team, credential, token, and management-key values.

Expected invariants for the current adapter:

| JSON field | Expected form |
| --- | --- |
| `billingCycleStart`, `billingCycleEnd` | Positive Unix-millisecond number or decimal string |
| `planUsage` | Object; absence means unavailable |
| `totalSpend`, `includedSpend`, `remaining`, `limit` | Non-negative integer cents |
| `autoPercentUsed`, `apiPercentUsed`, `totalPercentUsed` | Non-negative number |

### 4. Validate the management UI

- Cursor appears as a quota-capable provider in Auth Files and Quota Management.
- The quota request succeeds without a token reaching browser-visible state.
- Cursor Models and Other Models match the signed-in Cursor dashboard after
  applying Cursor's minimum-1%, maximum-100%, then-round display rule.
- Included usage, remaining percentages, and billing reset are coherent.
- The timeline renders translated pool names, not i18n keys.
- The Plugins page reports the expected plugin version and official icon URL.
- An absent or malformed `planUsage` response displays unavailable, not 0%.

### 5. Run the contract checks

Plugin repository:

```sh
go test ./...
go vet ./...
git diff --check
```

Management Center repository:

```sh
bun test tests/cursorQuota.test.ts
bun run type-check
bun run lint
bun run build
git diff --check
```

### 6. Update this record

Update the metadata table, evidence, contract snapshot, hashes, open gates, and
change log. Preserve earlier dated fingerprints and validation results instead
of replacing them. If the plugin capability or wire contract changes, bump the
plugin version and update its registration test.

## Failure triage

| Symptom | Check before changing code |
| --- | --- |
| HTTP 401 or 403 | Token freshness, credential class, and newly required headers |
| HTTP 404 or Connect unimplemented | Service or method may have moved |
| HTTP 415 or protocol error | Connect JSON support or content type may have changed |
| HTTP 200 without `planUsage` | Treat as a supported plan variant or unavailable data, never zero usage |
| HTTP 200 with no recognized fields | Treat as schema drift and fail unavailable |
| HTTP 5xx | Check the [Cursor status summary](https://status.cursor.com/api/v2/summary.json) before changing the adapter |
| Cursor dashboard works but CPA fails | Compare authentication, headers, transport, and token refresh |
| Both work but values differ | Compare field mapping, units, display clamp/rounding, and cache delay |

## Change log

### 2026-08-25

- Inspected Cursor Agent `2026.08.11-e8db854` and captured bundle hashes.
- Identified and live-validated the private DashboardService RPC with direct
  Connect JSON and through CPA `api-call`.
- Proved the Cursor Models and Other Models mappings from the signed-in Cursor
  spending dashboard bundle and recorded its exact URL, byte size, and SHA-256
  fingerprint for later drift checks.
- Proved the website `/api/dashboard/get-current-period-usage` route requires a
  WorkOS browser session and does not accept the CPA OAuth bearer by itself.
- Implemented the CPA metadata bridge, Cursor quota adapter, card, timeline,
  translations, contract tests, and Cursor icon metadata.
- Installed plugin `0.2.0` and the custom Management Center locally. Live UI
  validation showed `$48.73 / $400.00` included usage, rounded `1%` Cursor
  Models usage, rounded `4%` Other Models usage, and the expected billing reset.
  These values are a point-in-time sample, not test fixtures or plan promises.
- Validated the installed plugin and dashboard against Homebrew CPA `v7.2.140`;
  the plugin source still pins CPA SDK/API `v7.2.134`.
- Added Cursor's current CLI changelog, installation/update, and authentication
  documentation to the compatibility record. The July 13, 2026 release is the
  first-party confirmation of the current `/usage` display behavior; the RPC
  contract remains private.
- Audited all public links anonymously and recorded the expected authenticated
  dashboard and private-route responses.
- Created maintained GitHub forks under `yaminyassin`, retained the original
  projects as `upstream` remotes, and updated the plugin registration and README
  to the maintained Cursor plugin repository.

## Open validation gates

- Can the currently optional client headers be reduced beyond the validated
  `x-cursor-client-type: cli` request without losing compatibility?
- What is returned for Pro, Pro Plus, Ultra, Start, Team, and Enterprise accounts,
  especially under the current two-pool pricing model?
- Does a newly refreshed token immediately work through CPA after the host
  persists updated metadata?
- How should the adapter detect private schema changes without rendering stale
  quota data?
- Should the CPA adapter mirror Cursor's exact percentage display clamp, or show
  more precise raw values instead? Its current direct rounding differs below 1%
  and above 100%.

Live validation used an existing token for the direct request and a fresh
CPA-owned OAuth login for the end-to-end request. No token, management key, or
credential identifier is included in this report.
