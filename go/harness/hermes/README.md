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
       X-Hermes-Session-Key = conversation = Actor session key,
       Idempotency-Key = kagent-task-<task ID>, store = true)
  <- completed | incomplete  -> one text artifact, task completed
  <- failed | cancelled | any other status -> task failed (no polling)
```

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
calls Hermes, and sends it as both `X-Hermes-Session-Key` and `conversation` on
every later turn. The continuation store keeps it in `DurableDir`, so it
survives Actor replacement and snapshot restore. The header alone scopes the
session; `conversation` makes Hermes 0.23.4 chain the transcript.

## Gateway lifecycle

The first turn starts the gateway, never actor start-up: a golden snapshot
restore gives `/data` new backing files, which would break the SQLite files a
running gateway holds open. Hermes' `/health` does not read that state, so
before each turn the adapter also checks that `config.yaml` is still the file
it wrote at start. If the gateway exited, `HERMES_HOME` was replaced, or
`/health` fails, the adapter stops it, rewrites `HERMES_HOME`, and starts a
new one. A turn that hits the client timeout also stops the gateway, so the
next turn gets a new one.

## Limits

- Cancellation closes the HTTP request. Hermes can finish the turn on its side.
- The first turn of each Actor waits for the gateway to start.
