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

The compiler rejects tools, skills, plugins, Shared agents, structured output,
`defaultHeaders`, TLS settings, `tokenPassthrough`, and OpenAI options other
than `baseUrl` and `apiFormat: chatCompletions`.

## Isolation

- `config.yaml` turns off every toolset, memory, and lazy installs, and denies
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
