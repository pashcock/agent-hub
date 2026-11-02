# Kimi Code integration

Native permission profile targets Kimi Code 2.3.2. The hub drives the native
`kimi acp` process; authentication and inference remain in Kimi Code.
No direct Moonshot API adapter or model-name allowlist is involved.

## Enable the provider

Add to `~/.config/agent-hub/config.toml`, then restart an idle hub:

```toml
[agents.kimi]
enabled = true
max_live_processes = 4
idle_kill_s = 900
# Optional; otherwise PATH, then ~/.kimi-code/bin/kimi:
# bin = "/absolute/path/to/kimi"
# Optional; otherwise KIMI_CODE_HOME, then ~/.kimi-code:
# source_home = "/absolute/path/to/.kimi-code"
```

`peer_models(agent="kimi")` queries a no-prompt native ACP session. IDs come
from its model picker; resolved IDs and supported efforts come from native
configuration, including model overrides. Catalog discovery does not establish
remaining quota. Explicit model and effort selections are confirmed through
`session/set_config_option`; a mismatch aborts setup before inference. Changing
model/effort through `peer_send` is unsupported; start another conversation.
An omitted model uses Kimi's configured default.

The installed account advertised these aliases during validation:

- `kimi-code/k3` (configured default)
- `kimi-code/k3-256k`
- `kimi-code/kimi-for-coding`
- `kimi-code/kimi-for-coding-highspeed`

Use the live catalog for future requests. Do not assume every model accepts the
same thinking levels. The adapter supports persistent turns, cold resume after
process/daemon restart, cancellation, process capacity and idle eviction.
Mid-turn steering is unsupported.

## Let standalone Kimi call the hub

Merge this entry into `~/.kimi-code/mcp.json`:

```json
{
  "mcpServers": {
    "agent-hub": {
      "url": "http://127.0.0.1:7777/mcp",
      "bearerTokenEnvVar": "AGENT_HUB_TOKEN"
    }
  }
}
```

The launching environment must contain `AGENT_HUB_TOKEN`; use the existing
session-level token setup in the main README. No token is stored in this JSON.
Create a new Kimi session, inspect `/mcp`, then call `peer_list` or `peer_models`.
Existing Kimi sessions do not gain newly configured servers automatically.
Follow [the orchestrator workflow](../agent-usage.md) for starts, continuation,
waiting, approvals and closing conversations. Native Kimi MCP permission prompts
still apply. No broad automatic-approval rule is installed.

## Isolation and approval boundary

See the [provider capability matrix](capability-matrix.md) for the cross-provider
comparison and why native Kimi Plan is not the hub read-only profile.

Standalone Kimi can orchestrate the hub. Hub-launched Kimi uses a private
runtime home, with account/model/service settings copied from the native profile
and OAuth credentials linked to the existing account directory.

User MCP definitions are copied from `source_home/mcp.json`, excluding agent-hub
names. Project `.kimi-code/local.toml`, `mcp.json` and plugins are accepted and
loaded by native Kimi. The child sets `KIMI_CODE_TRUST_WORKSPACE=true` for the
project selected by the caller, so native workspace trust does not silently hide
project MCPs or additional directories. Search, code-analysis and browser MCP tools are available
in both code and read-only modes. These are trusted project settings, not an
isolation boundary against hostile configuration. The generated tool/permission
profile excludes `mcp__agent-hub__*` and `mcp__agent_hub__*`, and the child environment
never inherits `AGENT_HUB_TOKEN`. Global hooks, plugins, remembered approvals and
extra workspaces are not copied from the standalone profile.

- Built-in `WebSearch`, `FetchURL` and `select_tools` remain available alongside
  configured MCP tools. Read-only profiles auto-allow MCP calls; local file and
  terminal restrictions do not constrain remote MCP side effects.
- `code`: native **Ask When Needed** (`yolo`), with
  `KIMI_CODE_DANGEROUS_COMMAND_GUARD=true`. Ordinary workspace writes and commands
  run without a duplicate client approval. Native risky-operation requests are
  forwarded through `session/request_permission` to the hub.
- ACP `auto` means **Never Ask** internally; despite its misleading ACP picker
  description it is not the mode used by delegated code sessions.
- The client retains approvals for writes outside the project, terminal working
  directories outside the project, privilege escalation and nonstandard terminal
  environment overrides. File paths are resolved before classification, and
  symlink targets are checked again before a pending write executes.
- `ask`, `plan`, `review`: the generated native allowlist exposes `Read`, `Glob`,
  `Grep`, web search/fetch, MCP discovery and MCP tools. ACP writes and terminal
  creation remain forbidden.
  Read-only applies to local execution, not to side effects in MCP services.
- This remains tool-level policy, not an OS sandbox. The adapter reports
  `sandboxed_read_only=false` and rejects explicit sandbox overrides. Shell
  commands run with the hub user's access; network is not restricted by the hub.
- `acceptForSession` grants one operation, not persistent permission.
  Terminal processes are tracked and killed on cancellation, completion, close
  or eviction. Background terminals and native subagents are not exposed by the
  generated tool profile. Native free-text question forms remain unsupported;
  text questions can be answered with `peer_send`.

ACP 2.1.1 emits context occupancy (`usage_update.used/size`), not reliable
per-turn input/output/cache/cost totals. These events are preserved raw.
Consumption remains unreported; occupancy is never presented as money or tokens
spent. Hub timing remains available.

## Verification

```sh
go test -race ./internal/kimi ./internal/config ./internal/doctor ./cmd/agent-hub
KIMI_HUB_INTEGRATION=1 go test -tags integration ./internal/kimi -run Real -v
```

Real tests consume the existing Kimi subscription. They use temporary profiles,
workspaces, SQLite databases and in-process MCP hubs. They do not restart the
installed daemon or call Claude. Coverage includes all four configured models,
read-only file preservation, routine workspace writes and commands, native dangerous-command approvals, project MCP loading,
conversation restoration, cancellation, and standalone Kimi starting/polling/
closing a real Kimi peer through authenticated MCP.

References: [ACP](https://moonshotai.github.io/kimi-code/en/reference/kimi-acp),
[MCP](https://moonshotai.github.io/kimi-code/en/customization/mcp.html),
[configuration](https://moonshotai.github.io/kimi-code/en/configuration/config-files.html).
