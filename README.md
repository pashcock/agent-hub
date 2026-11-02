# agent-hub

A single local daemon that lets native coding agents hold stateful,
asynchronous conversations with each other. Each agent keeps its own
harness, authentication, model, configuration and context management.

This public source snapshot omits private development history, session links,
and machine-specific operational notes. Provider authentication and runtime
state are kept outside the repository.

Kimi Code supports both directions: [native ACP provider and standalone MCP client](docs/providers/kimi-code.md).

[Provider capability matrix](docs/providers/capability-matrix.md) documents mode mappings,
permissions, sandbox boundaries, MCP access, and remaining differences.

## Install

```bash
CGO_ENABLED=0 go install ./cmd/agent-hub
mkdir -p ~/.config/systemd/user
cp deploy/agent-hub.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now agent-hub
```

## Check the environment

```bash
agent-hub doctor
```

The last line reports whether an agent started in *this* environment could
actually reach the hub. That check exists because the failure is silent: an
agent without `AGENT_HUB_TOKEN` does not error — its MCP entry reports
`failed` and the peer tools are simply absent, which looks the same as never
having configured the hub.

```
hub      degraded                127.0.0.1:7777
         AGENT_HUB_TOKEN is not set in this environment, so agents started
         here cannot reach the hub and will fail silently.
```

## Set the token once

Both agents read the token at run time, which is what makes the second
isolation layer work. Set it for the whole user session rather than in a shell
profile: a Claude or Codex started from a desktop launcher or an IDE never
reads `~/.bashrc` and would fail to reach the hub without any error.

```bash
mkdir -p ~/.config/environment.d
printf 'AGENT_HUB_TOKEN=%s\n' "$(cat ~/.local/share/agent-hub/token)" \
  > ~/.config/environment.d/agent-hub.conf
chmod 600 ~/.config/environment.d/agent-hub.conf

# For the session that is already running:
systemctl --user set-environment AGENT_HUB_TOKEN="$(cat ~/.local/share/agent-hub/token)"
```

`environment.d` needs systemd 233 or newer. It applies on next login to login
shells, GUI-launched applications, IDE-spawned processes and user services
alike. Set it once; there is nothing to repeat. If the token is ever
regenerated, update that one file.

## Register with an orchestrator

```bash
# Note the single quotes: the header must keep the ${...} reference, not
# the token itself. A token baked into the config cannot be taken away
# from a peer, and the second isolation layer stops working.
claude mcp add --transport http agent-hub http://127.0.0.1:7777/mcp -s user \
  --header 'Authorization: Bearer ${AGENT_HUB_TOKEN}'

codex mcp add agent-hub --url http://127.0.0.1:7777/mcp \
  --bearer-token-env-var AGENT_HUB_TOKEN
```

Peers the hub launches cannot use this server. Each is masked in its own
way, and in both cases the hub also removes `AGENT_HUB_TOKEN` from the
child's environment:

| | mask | second layer |
| --- | --- | --- |
| Codex | `-c mcp_servers.agent-hub.enabled=false` on the shared app-server | bearer token read from the environment at run time |
| Claude | `--disallowedTools mcp__agent-hub` on each peer process | the same, through `${AGENT_HUB_TOKEN}` in the header |

A peer therefore cannot call the hub's tools, and could not authenticate
even if it could see them. No recursion is possible.

Both halves are proved separately against the real binaries by

```bash
go test -tags integration ./internal/codex/  -run CannotSee
go test -tags integration ./internal/claude/ -run CannotUseTheHub
```

Each compares three processes: a reference that can reach the hub and sees its
twelve tools, one with the token removed, and one with both defences. The
reference is what stops the test passing vacuously.

## Tools

Every tool returns immediately. Nothing blocks while a peer thinks.

| tool | purpose |
| --- | --- |
| `peer_list` | available peers |
| `peer_capabilities` | models, MCP servers, steering support |
| `peer_models` | native model catalog, resolved aliases and reasoning efforts |
| `peer_start` | open a conversation, send the first turn |
| `peer_send` | continue the same native session |
| `peer_poll` | check status, collect events, read the result |
| `peer_ask` | one question, short bounded wait |
| `peer_steer` | add context to a running turn |
| `peer_cancel` | stop a running turn |
| `peer_approve` | answer a peer's permission request |
| `peer_transcript` | observable dialogue |
| `peer_close` | end a conversation |

A conversation takes one turn at a time. Poll until the current turn is
terminal, or cancel it, before sending the next message.

## Web view

The daemon serves a small view of itself on the same loopback listener:

```
http://127.0.0.1:7777/
```

Agents with their capacity, running and finished conversations, the queue,
token and cost totals, and per-turn breakdowns. Buttons answer a parked
permission request — including the human-gated classes `peer_approve`
refuses — and cancel a running turn.

It carries no token. The listener is loopback-only and `hub.db` is readable
by this user anyway, so a token would guard nothing while adding a step to
every page load. The token exists to keep peers off `/mcp`.

## Spend

Providers report turn consumption; Claude and Grok can also report money.
A cost of `—` means "not reported", never "free".

| | Codex | Claude | Grok |
| --- | --- | --- | --- |
| source | `thread/tokenUsage/updated` (`last` is the turn, `total` the thread) | the `result` line | ACP prompt `_meta.usage` |
| tokens | yes | yes | yes |
| cost | no | `total_cost_usd` | complete `costUsdTicks` reports |

`peer_poll` returns `usage` **only once a turn is terminal** — a running total
is noise in a conversation between agents, and it is not final until the turn
ends. The full breakdown lives in the web view.

## Capacity

Two separate limits, often confused:

| | what it bounds | scope |
| --- | --- | --- |
| `limits.max_parallel_peer_turns` | turns running at once | per calling session |
| `agents.claude.max_live_processes` | Claude processes alive at once | the whole hub |

Codex reports no process limit: every thread lives in the one app-server, so
only the per-caller turn limit applies to it.

When an agent is at capacity, `peer_start` **fails** rather than silently
queueing, so a caller is never left believing work started when it has not:

```
codex is at capacity (2 of 2 live). Retry later, or pass queue=true to
have the hub start it when a slot frees
```

Passing `queue=true` accepts it instead: you get a `conversation_id` with
`status: "queued"` and no turn yet. `peer_poll` reports `queued` until a slot
frees, then the hub starts it with the task it was queued with.
`peer_cancel` takes it back out of the queue. A queued start survives a hub
restart; the whole request is persisted, not just the text.

## Seeing what a peer is doing

`peer_poll` reports `activity` while a turn runs: the command Codex is running,
the tool Claude just called, the file being edited. It is the newest label, not
a log — "what is it doing now", which is what makes `silent_for_s` judgeable.

```json
{"turn_status":"running","activity":"Bash: go test ./...","silent_for_s":12}
```

Codex labels come from `item/started` across the nineteen thread-item kinds;
Claude's come from the `tool_use` blocks of an assistant message. An item that
says nothing useful produces no label rather than noise.

## Waiting inline

`peer_send` takes `wait_s`. With it the hub waits for the answer and returns it
inline, which saves a poll loop on a short exchange. The wait is bounded: a turn
that outlives it hands back a handle, so a long peer never holds the caller's
own turn open. `peer_ask` is the same thing for a new conversation.

## Waiting without holding the conversation open

`wait_s` costs the caller its own turn: while the tool call is out, the
orchestrator cannot answer the human. For a peer that will take twenty minutes
that is the wrong trade, and the workaround an agent reaches for — a monitor
running `sleep 900` — reports a two-minute answer thirteen minutes late, and a
two-minute failure just as late.

The hub therefore publishes the same state as JSON on the loopback listener,
with no token, so the waiting can happen outside the agent's turn:

```
GET /api/c/<id>            one snapshot
GET /api/c/<id>/wait       blocks until the turn is over (timeout_s, max 900)
```

```json
{"conversation_id":"example-conversation","agent":"codex","liveness":"live",
 "turn_status":"running","done":false,"activity":"running go test ./...",
 "silent_for_s":12,"stalled":false,"cursor":1841}
```

`done` is the field to test. It covers every reason a peer has stopped working
— completed, failed, cancelled, or waiting on a permission somebody has to
answer — so a loop never hangs on a turn that will never finish on its own.

Use background/yielded `curl` on `/wait`, then collect the same command's
result. Do not create a monitor agent or repeatedly wake a model to check status.
Background completion notification depends on the harness; HTTP alone does not
guarantee that an idle session wakes up.

Read the canonical [agent usage guide](docs/agent-usage.md) before delegating.
It covers curl, status handling, questions, approvals, and cleanup. A completed
turn can contain a question: answer with `peer_send` in the same conversation.

`/wait` holds a connection for at most 900 seconds and then returns the current
state with `"timed_out": true` rather than an error, so a longer wait is a
reconnect — which also surfaces a hub that restarted underneath the caller.

## Answering a question

`peer_approve` takes `answer`. Codex has request kinds that ask a question
rather than for permission — `item/tool/requestUserInput` and an MCP
elicitation — and a decision verb alone cannot answer "which environment?".
Without an answer those are replied to with an empty selection, which unblocks
the turn but tells the peer nothing.

## Stalls

`peer_poll` reports `silent_for_s`, and sets `stalled` once a running turn has
produced nothing for `limits.stall_warn_s` (600 by default):

```json
{"turn_status": "running", "silent_for_s": 812, "stalled": true,
 "note": "this peer has produced nothing for a while. It may be working, or
          it may have hung. Decide whether to keep waiting or call peer_cancel."}
```

A stall is reported, never enforced. A peer that thinks for an hour is
legitimate; one that has hung is not, and only the caller knows which it asked
for. This replaces pairing every delegated run with a separate liveness
monitor, which is what a silent hang used to require.

## Conversations survive restarts

Conversation history lives in the provider, not in the hub. When the daemon
restarts, every conversation becomes dormant and the next `peer_send` reloads
the native session from disk with its context intact. Only the turn that was
running at that moment is lost, and it is reported as failed rather than left
hanging:

```json
{"turn_status":"failed",
 "error":"the hub restarted while this turn was running, so it was aborted
          part-way. Any changes it had already written are still on disk, so
          check the working tree before assuming nothing happened. The
          conversation itself survived: peer_send continues the same native
          session with its context intact."}
```

That last sentence matters for `mode: "code"`: `failed` means the turn did not
complete, not that nothing happened.

## Approvals

The orchestrator model answers ordinary permission requests itself with
`peer_approve`, so routine work is never interrupted. Three classes are
refused there and need you:

- privilege escalation (`sudo`, `su`, `pkexec`, `doas`)
- writes outside the conversation's project directory
- persistent execpolicy or network policy amendments

```bash
agent-hub approvals
agent-hub approve <request_id> --conversation <id> --accept
```

An unanswered request is decided by `permissions.on_timeout` after
`permissions.timeout_s`, so it can never hold a turn forever.

## Configuration

`~/.config/agent-hub/config.toml`, state in `~/.local/share/agent-hub/`.

```toml
[server]
addr = "127.0.0.1:7777"

[limits]
max_parallel_peer_turns = 3

[permissions]
forward_to_caller = true
timeout_s         = 600
on_timeout        = "deny"
human_gated       = ["privilege_escalation", "write_outside_cwd", "policy_amendment"]

[agents.codex]
enabled = true
idle_kill_s = 900
```

Codex conversations share one app-server, but each loaded thread can own MCP
processes. Closing a conversation now sends `thread/unsubscribe`; failures are
returned instead of reporting the conversation closed. Idle threads are also
unsubscribed after `agents.codex.idle_kill_s` (900 seconds by default), excluding
running turns and pending approvals. The next send resumes the same thread and
history. App-server applies its own unload grace period after unsubscribe, so
child processes need not exit immediately.

## Provider version drift

The Codex app-server protocol is experimental and does change. Nothing notices
when it does: a renamed field decodes as a zero value and the adapter simply
stops seeing something, with no error anywhere. `agent-hub doctor` therefore
records the version each adapter was last verified against and says so when the
installed one moves past it. It is silent while the two agree.

Made-up version below, to show the shape of the warning:

```
codex    0.155.1    app-server   /home/example/.local/bin/codex
         codex 0.155.1 is newer than the verified 0.154.0. The app-server
         protocol is experimental; re-check it with `codex app-server
         generate-json-schema --out <dir>` and compare against
         internal/codex/protocol.go
```

Verified as of 2026-09-10: codex 0.154.0, claude 2.1.267. The check compares
every method name, every parameter field the adapter sends, and every response
shape it builds; update `verifiedAgainst` in `internal/doctor/drift.go` once a
new version passes.

## Known constraint: Codex read-only sandbox

`codex exec --sandbox read-only` disables **every** MCP server for that run,
this one included, and reports nothing: the tools are simply absent while
`codex mcp get agent-hub` still says `enabled: true`. Use
`--sandbox workspace-write` when a headless Codex run needs the hub.

This does not affect a Codex *peer*. Peers run inside the hub's own
`app-server`, where the sandbox is a per-thread setting and MCP servers stay
loaded, so `mode="review"` really is read-only and really does keep its tools.

## Providers


Enable the providers you intend to use in your own configuration.
Check `peer_list` for the current runtime list. Qwen Code is planned, not enabled.
For Grok, use `peer_start(agent="grok", model="grok-4.7", ...)`, then HTTP `/wait`
and `peer_send` on the same conversation. `peer_models(agent="grok")` returns
current model IDs. Include `Do NOT use the superpowers plugin or any of its skills.`
in delegated tasks. Hub-launched peers cannot call the hub recursively;
native Grok subagents remain available. Claude calls are currently paused by
the user because of token limits, despite its adapter remaining enabled.

Model catalogs come from the native clients, not a hard-coded hub list.
`peer_models` queries them on demand; it does not upgrade provider binaries.
Codex follows `model/list` pagination. Grok uses `_x.ai/models/list`. Claude
uses SDK control requests without a user turn and reports native aliases plus
`resolved_model` when available. See [model discovery](docs/agent-usage.md#discover-models)
for freshness, project settings, and verification limits.


| | Codex | Claude Code |
| --- | --- | --- |
| backend | one `codex app-server`, a thread per conversation | one `claude -p` process per conversation |
| session id | assigned by the provider | chosen by the hub with `--session-id` |
| `peer_steer` | absorbed mid-turn, returns `steered` | `unsupported`: poll, then `peer_send` |
| `peer_approve` | answers a parked request | `unsupported`: Claude raises none in `-p` mode |
| read-only | provider sandbox | `--permission-mode plan` plus a tool denylist |
| write mode | `sandbox=workspace-write` | `--permission-mode acceptEdits` |
| idle cost | ~8.6 MB per thread | ~283 MB per process, so idle ones are released |
| process limit | none; one process holds every thread | `max_live_processes`, enforced at `peer_start` |

A Claude conversation whose process was released reports `liveness: "cold"` and
is restored by the next `peer_send`. `peer_poll` never restores one; it answers
from SQLite.

`[agents.claude]` takes `max_live_processes` and `idle_kill_s`. Two live
processes is the default: enough for one reviewer plus one spare.

## Scope

Kimi and Grok are available as opt-in native ACP adapters. See the
[capability matrix](docs/providers/capability-matrix.md) for current behavior;
the original design document describes an earlier scope.

## Grok Build

Grok is opt-in. After reviewing the change, enable it in the hub configuration
and restart the hub when no peer work is running:

```toml
[agents.grok]
enabled = true
max_live_processes = 4
idle_kill_s = 900
```

The adapter uses `grok agent --no-leader stdio`. Each conversation retains one
process across turns. Native session IDs and the Grok transcript survive a
process or hub restart; the next `peer_send` loads that session. `peer_poll`
does not launch a process. Idle processes are released after `idle_kill_s` (900 seconds by default),
retaining the native session and event stream. Running turns and permission
requests are never evicted; the process limit defaults to four.

Authentication uses the existing grok.com login (`grok login`). The adapter
selects `cached_token` and removes API-key overrides from the child environment;
it does not silently fall back to paid API credentials. `GROK_AUTH_PATH`, or
`$GROK_HOME/auth.json` when set on the hub, selects the existing auth file.
Otherwise the path is `~/.grok/auth.json`.

Auth refresh and model discovery use one serialized, short-lived service probe
in a fresh private directory, outside the model sandbox. It sends only ACP
initialize/authenticate/model-list requests: no session, prompt, tools, or
permission approvals. Model sessions keep their `workspace`/`read-only` sandbox.
The probe runs before session attachment and turns (reusing an auth check for
up to one minute), and once per minute while turns are active so native Grok
can renew the shared credentials for long-running work. A failed refresh is
reported; model turns are never automatically replayed. There can be one extra
service process in addition to the configured conversation process limit.

Grok can return a built-in model list even when authentication or the catalog
fetch failed. Discovery therefore requires a freshly written subscription
catalog in the probe's empty home and matching ACP model IDs. Missing, stale,
or mismatched evidence returns an error instead of claiming a model is absent.
This check depends on the native `models_cache.json` format verified with Grok
1.0.41; an incompatible format fails explicitly. No model IDs are hard-coded.
Discovery reports the account catalog; caller project settings are applied
when the sandboxed model session starts, not in the unsandboxed service probe.

The hub owns a separate Grok runtime home at
`~/.local/share/agent-hub/grok`. Its generated profile disables legacy Claude
settings import and the `codex` and `superpowers` plugins. Foreign MCP, hook,
agent, skill and session discovery is disabled in the child environment, and
`AGENT_HUB_TOKEN` is removed. Every session creation/resume verifies the live
plugin and MCP inventories before sending a prompt. Discovery-only output from
`grok inspect` is not used as proof that a plugin is active.

- `code`: native `workspace` sandbox, `autoMode=true`, `yoloMode=false`.
- `ask`, `plan`, `review`: native `read-only` sandbox, Auto disabled; requests
  for additional permissions are rejected. A native turn cancelled by that
  denial is reported as failed with the reason, rather than a silent interrupt.
  The adapter conservatively reports
  `sandboxed_read_only=false` until broader sandbox conformance is established.
- Model and reasoning effort are pinned at conversation creation. The adapter
  verifies the model/effort echoed by Grok; changing either requires a new
  conversation. `peer_models` reports the native catalog.
- `peer_cancel` sends ACP `session/cancel`; unsupported client requests fail
  explicitly rather than hanging. Mid-turn `peer_steer` is unsupported.
- Auto handles ordinary safe operations. Remaining permission requests go
  through the hub. Unknown or unstructured requests are human-gated as policy
  requests. `acceptForSession` grants only one invocation because ACP's
  `allow_always` may persist beyond this conversation. Free-text questions are
  not implemented by this adapter.
- Whole-prompt token usage comes from `_meta.usage`, including cache and
  reasoning fields. Reported cost uses Grok's USD ticks; incomplete/partial
  costs are not presented as exact. This is provider-reported usage, not a
  measurement of the remaining subscription allowance.

Validated against Grok Build 1.0.25. Run deterministic tests with:

```bash
go test -race ./internal/grok
```

Real integration tests are opt-in and use an **isolated in-process MCP hub**,
a temporary SQLite database and temporary workspaces. They never restart the
installed system service. They require the existing grok.com login and an
environment where Grok's native sandbox can run; a nested restricted sandbox
may reject `bwrap` namespace creation. These tests consume subscription usage:

```bash
GROK_HUB_INTEGRATION=1 go test -tags integration ./internal/grok -run Real -v
```

Native Grok subagents remain available to delegated Grok sessions. Grok Build
1.0.25 exposes `spawn_subagent` and `get_command_or_subagent_output`; the live
test verifies a child ID and the returned child result. These children belong
to Grok's harness, not to separate hub conversations. The restriction on a
peer calling the hub does not disable native subagents.

A standalone Grok orchestrator can also call this hub over authenticated MCP
HTTP. `TestRealGrokStandaloneCallsCodex` supplies an isolated target hub to a
native ACP session, then verifies that Grok starts and polls a real Codex
conversation. The target hub contains no Claude adapter. This test uses a
test-only orchestrator driver; the production Grok peer adapter still rejects
the same MCP connection as recursive. It does not configure the user's
interactive Grok profile or test the TUI frontend.

For a standalone profile, Grok supports this entry in `~/.grok/config.toml`
(the token must be present in the launching environment):

```toml
[mcp_servers.agent-hub]
url = "http://127.0.0.1:7777/mcp"
bearer_token_env_var = "AGENT_HUB_TOKEN"
```

## Operator controls and performance

Providers can be paused without restarting the daemon. A pause is persisted
in SQLite, blocks new starts/sends and capability probes, and holds queued
starts. Existing turns, approvals and cancellation continue normally. The
`peer_list` result includes `availability.paused`, its reason and optional
expiry. Expired pauses stop blocking automatically. It is operator state,
not a claim about the provider's remaining subscription quota.

```bash
agent-hub pause claude --reason "subscription quota" --for 5h
agent-hub resume claude
```

Omit `--for` for an indefinite pause. Web controls provide the same actions;
`POST /operator/provider` requires the hub bearer token. There is no MCP tool
that lets a delegated task resume an operator pause.

The web overview searches message text, IDs and paths, with exact workspace,
agent, model, latest state and conversation creation-period filters. Workspace
means the recorded cwd; separate Git worktrees are not silently merged.
Filters live in the URL. Results paginate at 50 conversations. Live refresh is
opt-in, every five seconds, and stops while editing or selecting text.

Read-only local monitoring also has:

- `GET /api/conversations?q=...&cwd=...&agent=...&model=...&status=...&period=7d&page=1`
- `GET /api/agents`
- Existing `GET /api/c/<id>` and `/api/c/<id>/wait`.

Conversation-list JSON includes the filtered total, offset and limit. These
reads do not invoke a model. They use the existing loopback-only read API.

New turns record queue delay, session setup/resume, prompt submission,
first observed activity/text, time parked for approval and end-to-end time.
`peer_poll` and the status JSON return `timings` in milliseconds; turn details
show the breakdown. Historical turns remain unmeasured, never zero-filled.
The overview compares mean times and sample counts by agent/model for completed
turns in matching conversations; fast failures do not lower those means.
Approval means include only turns that waited for approval, with their own
sample count. These are harness/task times, not generation
tokens per second; completed turns are not verified-quality scores. Test
success, rework and benchmark task labels still need explicit evidence.

HTTP snapshots report the latest event cursor without replaying old events;
MCP polling retains its incremental cursor contract. Dialogue and turn history
are paginated at 100 messages and 50 turns. Latest events are fetched from the
actual tail, with an older-events cursor.
No history is deleted by this change. Native subagent usage remains in raw
provider events and is not added twice to parent totals.

Explicit sandbox overrides are persisted for newly created conversations and
restored with their model/effort. Existing databases receive an additive
`sandbox` column; old overrides absent from stored history cannot be inferred.

### Local semantic history search and UI preview

Optional offline hybrid search reuses an installed model2vec encoder (including
Semble's cached `potion-code-16M-v2`). It combines literal and meaning matches,
scopes retrieval by workspace, and exposes snippets through the web view and
`/api/conversations?search=hybrid&q=...`. No model download is automatic.

`cmd/agent-hub-preview` serves a read-only snapshot on a separate loopback port
without starting agents or touching the running daemon. Run
`go run ./cmd/agent-hub-preview -h` for its available options.

### Conversation accounting for callers

HTTP status/wait and MCP poll/ask/waited-send return `conversation.usage`
(all reported hub turns), cache reads/writes and hit percentage, plus
`conversation.execution_ms` (sum of measured finished-turn elapsed times).
Coverage counts distinguish missing historical measurements from zero.
Latest-turn `usage` and `timings` remain separate. Native child usage is not
independently collected; responses mark inclusion as unverified. See the
[accounting contract and provider boundaries](docs/agent-usage.md#read-consumption-before-planning-follow-ups).
