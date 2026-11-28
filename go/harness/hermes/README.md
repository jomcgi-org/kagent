# Hermes Harness

The Hermes Harness compiles an `api.kagent.dev/v1alpha3` `AgentTemplate` into a
compiler-owned Hermes configuration and runs one Hermes Agent `0.23.4`
`hermes gateway` process with only its API server enabled, on loopback. Each
public A2A Task is one non-streaming `POST /v1/responses` call. The Hermes
session store stays in the Actor's `DurableDir`.

Select it with `spec.hermes: {}` on a `Harness`. Kagent publishes no Hermes
image, so the Harness must set `spec.workload.image` to an image that holds
Hermes Agent and this adapter. The `hermes-runner` image holds only the adapter
binary at `/usr/local/bin/kagent-hermes`, as a layer to add to a Hermes Agent
image; make that binary the entrypoint.

## Code structure

| Path | Look here for |
| --- | --- |
| [`../../core/internal/translator/hermes`](../../core/internal/translator/hermes) | Translating `Harness`, `AgentTemplate`, model, and Secret inputs into a runtime revision |
| [`config/config.go`](config/config.go) | The versioned JSON contract shared by the compiler and runtime |
| [`cmd/main.go`](cmd/main.go) | Actor startup, bootstrap bundle, and private A2A startup |
| [`executor`](executor) | The shared A2A executor and continuation store around the driver |
| [`internal/adapter`](internal/adapter) | Building the driver from the compiled configuration |
| [`internal/driver`](internal/driver) | `HERMES_HOME`, gateway lifecycle, environment allowlist, and the `/v1/responses` client |

## Request flow

```text
private A2A (gRPC) -> runtime/a2a executor -> driver.Run
  -> POST 127.0.0.1:<port>/v1/responses   (Bearer <random key>,
       X-Hermes-Session-Key = Actor session key,
       previous_response_id = chain head, or conversation = session key,
       Idempotency-Key = kagent-task-<task ID>, store = true)
  <- completed | incomplete  -> one text artifact, task completed,
                                chain head = this response
  <- failed | cancelled | any other status -> task failed (no polling),
                                chain head unchanged
```

A failed task's message is fixed text. Only an allowlisted Hermes reason
code joins it, never the provider's text:
`Hermes turn failed: content_policy_blocked` for a turn that the provider's
safety filter refused.

## Implemented support

- One OpenAI-compatible model, written as the named `providers.kagent` entry
  (`base_url`, the placeholder `api_key`, `api_mode: chat_completions`) with
  `model.provider: kagent`. Hermes takes a named entry's key before any
  environment key. A bare `provider: custom` resolves a remote host such as
  openrouter.ai without a key and fails. `openAI.baseUrl` names the endpoint, such as
  OpenRouter; without it, the OpenAI API.
- The model key comes from a Secret through
  [Substrate gateway injection](../../../docs/architecture/credential-injection.md),
  or from `actorJwt`. The Actor and `config.yaml` hold only the placeholder.
  A ModelConfig with neither sends the placeholder.
- The compiled system prompt goes in each call's `instructions`.
- Token usage per turn. Hermes reports one total for a turn that can span
  several model requests, so the adapter counts tokens and opens no model span.

- RemoteMCPServer tools over Streamable HTTP. Each binding must list its
  tools; the adapter renders them as `tools.include` and names only the bound
  servers in `platform_toolsets.api_server`, so no built-in toolset loads.
  Sampling, elicitation, and the resource and prompt utility tools are off,
  and Tool Search is off, so each MCP tool is in the request itself. A Secret
  header or `actorJwt` holds the credential placeholder, and the egress
  gateway injects the value. The compiler and the runner reject an MCP
  server named like a Hermes toolset or selector, such as `memory` or
  `onboarding`, or starting with `mcp-`, Hermes's toolset name for a server.
- Anthropic server tools from `spec.hermes.serverTools` (`webSearch`,
  `webFetch` with `maxUses`, and `codeExecution`). They need a ModelConfig
  with the Anthropic provider. Hermes 0.23.4 builds the Anthropic `tools`
  list from function tools only, so the adapter writes the plugin
  `kagent-server-tools` into `$HERMES_HOME/plugins` and enables only it. Its
  `llm_request` middleware adds the tool entries to each turn request.
  Hermes keeps the answer text and drops the server tool blocks when it
  replays history. That edit can invalidate a later thinking block, so the
  plugin also sets `thinking.block_binding.prefix_mismatch_behavior:
  drop_block` (beta `thinking-binding-controls-2026-08-01`) on adaptive
  thinking, and adds adaptive thinking only on models that always think.
  The web tools are the basic, direct-call versions (`web_search_20250305`,
  `web_fetch_20250910`). The plugin sets `disable_parallel_tool_use`, so a
  model step makes at most one tool call, and that setting does not support
  the 20260209 versions, which call through code execution.
- Hermes's built-in memory, `MEMORY.md` and `USER.md`, from
  `spec.hermes.memory.remoteMCPServer`: a RemoteMCPServer bound to the
  agent whose `memory_read {file}` returns `{file, content, version}` and
  whose `memory_write {file, content, base_version}` replaces a file only
  from its current version, else returns `{conflict: true, content,
  version}`. The server must be stateless and answer `application/json`.
  The binding must not list either tool, so the model gets only Hermes's
  `memory` tool. The adapter calls them with the binding's URL and headers,
  so the egress gateway injects the binding's credential. See "Memory sync".
- MCP entries carry the RemoteMCPServer default 30 s `timeout` and
  `strict_redirect_headers: true`, which keeps headers on the configured
  origin and turns off the SSE fallback. A URL, tool name or header value
  with `${` is rejected: Hermes expands it.

Server tool limits, not handled yet:

- A `pause_turn` stop (Anthropic's server loop reached its limit) maps to
  `stop`, so the turn ends with the text so far. `maxUses` keeps the loop
  short.
- Hermes drops server tool blocks when it replays history. A server call
  still pending next to an MCP call would then never finish, so the plugin
  allows one tool call per model step. Completed server blocks are dropped
  too, which only removes them from history.
- Each Messages request gets a new code execution container; files do not
  carry over between model steps.

The compiler rejects sub-agent tools, skills, plugins, Shared agents,
structured output, `defaultHeaders`, TLS settings, `tokenPassthrough`,
`requireApproval` (Hermes denies every approval), and OpenAI options other
than `baseUrl` and `apiFormat: chatCompletions`.

## Isolation

- `config.yaml` turns off every built-in toolset, memory, and lazy installs, and denies
  every approval (`unattended_mode: deny`), so a turn never waits on a person.
  With `spec.hermes.memory`, the `memory` toolset is the one built-in toolset
  on, and `nudge_interval: 0` turns off Hermes's background memory review,
  which would save memories without the turn's model call deciding to.
  The adapter writes it before every gateway start; the compiled configuration
  owns it.
- Before every gateway start the adapter removes `$HERMES_HOME/.env`, which
  Hermes loads over the process environment.
- The gateway inherits only an allowlist of the Actor environment (locale,
  CA bundles, the Hermes image's own settings, and `OTEL_*`). A messaging
  token, the model placeholder, or `HERMES_YOLO_MODE` never reaches it.
- The API server listens on `127.0.0.1` behind a random 256-bit key.

## Session key

The shared executor binds one native conversation to each Actor; the A2A
`contextId` names only controller history. On its first turn the adapter
creates `kagent-<uuid>`, binds it through `runtime.SessionStarted` before it
calls Hermes, and sends it as `X-Hermes-Session-Key` on every later turn. The
continuation store keeps it in `DurableDir`, so it survives Actor replacement
and snapshot restore. The header alone scopes the session; Hermes 0.23.4
replays a transcript only through `previous_response_id` or `conversation`.

Hermes's API server stores every turn, a refused one too, and moves the
`conversation` name to it. A provider filter judges the whole transcript, so
a refused message in it refuses every later turn. (Hermes's chat gateway
removes a refused turn; its API server does not.) So the driver keeps a chain
head: the last answered response of each session, in
`HERMES_HOME/kagent-chain/`. A turn chains to the head with
`previous_response_id`, and only an answered turn moves the head. A refused
turn (`content_policy_blocked`) is also deleted from Hermes's response store,
which drops a `conversation` name that points at it.

- A session with no head (its first turn, or a session from an older
  adapter) continues by `conversation` name.
- A retry of the task that moved the head sends that turn's request again,
  under the same idempotency key, so Hermes replays the first answer.
- If the response store no longer holds the head, the turn continues by
  `conversation` name under the idempotency key `kagent-task-<task
  ID>-conversation`.
- A turn that the driver did not see answered (a timeout) is not in the
  chain, even if Hermes later finishes it.

## Gateway lifecycle

The first turn starts the gateway, never actor start-up: a golden snapshot
restore gives `/data` new backing files, which would break the SQLite files a
running gateway holds open. Hermes' `/health` does not read that state, so
before each turn the adapter also checks that `config.yaml` is still the file
it wrote at start. If the gateway exited, `HERMES_HOME` was replaced, or
`/health` fails, the adapter stops it, rewrites `HERMES_HOME`, and starts a
new one. A turn that hits the client timeout also stops the gateway, so the
next turn gets a new one.

## Memory sync

```text
before each turn, for MEMORY.md and USER.md:
  local file unchanged since the last sync, or missing -> memory_read -> local file
  local file changed (a failed write-back)              -> write back, as below
turn: Hermes reads memories/ into its prompt; its memory tool edits them
after each turn, for a changed file:
  memory_write(content, base_version) -> ok: record the new version
  conflict -> apply this session's entry adds and removes (against the
              last synced copy) to the newer copy, retry, at most 3 times
```

- Entries are split on Hermes's `\n§\n` delimiter. A replaced entry is a
  removal and an addition, inserted after the nearest earlier entry the newer
  copy still holds. A merged file over Hermes's limit (2200 characters for
  `MEMORY.md`, 1375 for `USER.md`) keeps the newer copy's entries, applies
  only this session's removals, and logs.
- Hermes reads the files into the prompt when a session starts. A
  continuing session reuses its stored system prompt for the prefix cache
  (`agent/conversation_loop.py` `_restore_or_build_system_prompt`), so
  another session's change reaches its prompt only when Hermes rebuilds the
  prompt, such as at compression. Its memory tool still edits the newest
  local copy, and the write-back merges.
- A missing local file loads from the server: Hermes never deletes one, so a
  write-back must not empty the server's copy.
- A failed call logs and never fails the turn: the local copy stays, and the
  next turn writes it back. `$HERMES_HOME/kagent-memory-sync.json` records the
  last synced copy and version, so a new process on the same volume still
  merges. A missing file starts empty.
- The adapter takes Hermes's lock (`<file>.lock`) while it reads or replaces
  a file.

## Limits

- Cancellation closes the HTTP request. Hermes can finish the turn on its side.
- The first turn of each Actor waits for the gateway to start.
