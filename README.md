# OpenAI API Server via Codex

[![CI](https://github.com/hotchpotch/openai-api-server-via-codex/actions/workflows/ci.yml/badge.svg)](https://github.com/hotchpotch/openai-api-server-via-codex/actions/workflows/ci.yml)
[![PyPI](https://img.shields.io/pypi/v/openai-api-server-via-codex.svg)](https://pypi.org/project/openai-api-server-via-codex/)
[![GitHub Release](https://img.shields.io/github/v/release/hotchpotch/openai-api-server-via-codex?include_prereleases)](https://github.com/hotchpotch/openai-api-server-via-codex/releases)
[![License](https://img.shields.io/github/license/hotchpotch/openai-api-server-via-codex)](LICENSE)

💰 **Get an OpenAI-compatible API from your ChatGPT subscription.**

Your ChatGPT subscription includes Codex, and the backend behind Codex works
much like the OpenAI API. This unofficial server exposes it as an
OpenAI-compatible API, so OpenAI SDKs and existing tools can use your ChatGPT
subscription.

Within your Codex usage limits, you don't need a Platform API key and there are
no per-token charges.

<img width="70%" src="https://raw.githubusercontent.com/hotchpotch/openai-api-server-via-codex/main/docs/assets/quick-start.png" alt="Start the Go server with uvx, then call the OpenAI-compatible Responses API" />

## Quick start

Sign in once if `~/.codex/auth.json` does not exist:

```console
$ codex login
```

Start the server in one terminal. The recommended installation requires only
[`uv`](https://docs.astral.sh/uv/):

```console
$ uvx openai-api-server-via-codex
```

Prefer another installation method? Every option runs the same Go server:

- **[Standalone binary](#install-standalone-binary):** download the archive for Linux, macOS, or Windows
  (x86_64 or ARM64) from the [latest GitHub Release](https://github.com/hotchpotch/openai-api-server-via-codex/releases/latest).
- **[Docker](#install-docker):** pull the public image with
  `docker pull ghcr.io/hotchpotch/openai-api-server-via-codex:latest`.
- **[Go](#install-go):** build and install directly from the public module with
  `go install github.com/hotchpotch/openai-api-server-via-codex/cmd/openai-api-server-via-codex@latest`.
- **[Source checkout](docs/build-from-source.md#build-for-the-current-machine):** clone the repository and build it locally.

See [Installation options](#installation-options) for complete commands,
checksums, platform details, and Docker volume configuration.

Point an existing OpenAI client at it from another terminal:

```console
$ export OPENAI_BASE_URL=http://127.0.0.1:18080/v1
$ # This server requires no key by default, but the OpenAI SDK requires a value.
$ export OPENAI_API_KEY=dummy-not-a-real-openai-api-key
```

> [!NOTE]
> `dummy-not-a-real-openai-api-key` is deliberately **not a real OpenAI API
> key**. With the default server settings, any non-empty dummy value works: it
> only satisfies the OpenAI SDK's client-side validation and is not checked by
> this server. Incoming requests require a real local key only if you start the
> server with `--api-key`. Codex authentication always uses
> `~/.codex/auth.json`.

OpenAI SDK code needs no server-specific changes. It only has to use a model
available through your Codex account:

```python
from openai import OpenAI

client = OpenAI()
response = client.responses.create(
    model="gpt-6-luna",
    input="Hello",
    reasoning={"effort": "low"},
)
print(response.output_text)
```

Save it as `example.py` and run it without permanently installing the SDK:

```console
$ uv run --with openai python example.py
```

<details>
<summary><strong>PowerShell environment variables</strong></summary>

```powershell
$env:OPENAI_BASE_URL = "http://127.0.0.1:18080/v1"
$env:OPENAI_API_KEY = "dummy-not-a-real-openai-api-key"
uv run --with openai python example.py
```

</details>

## Why use it?

This server is useful when you want to:

- run scripts and local tools on your ChatGPT subscription instead of paying
  Platform API per-token charges, within your Codex usage limits;
- reuse code written for `openai-python`, LangChain, LiteLLM, or another client
  that supports a custom base URL; or
- give tools that cannot call the Codex backend directly one consistent local
  API.

| Capability | What you get |
| --- | --- |
| Subscription-backed access | Use the Codex allowance included in your ChatGPT plan without separate Platform API token charges |
| OpenAI compatibility | Responses, Chat Completions, streaming, tools, structured output, images, and audio transcription |
| Small Go runtime | A single executable with low memory use and fast startup |
| Portable distribution | Wheels and archives for Linux, macOS, and Windows on x86_64 and ARM64 |
| Local-first defaults | Loopback binding, auth preflight, redacted logs, and optional incoming API-key protection |
| Multiple installation paths | `uvx`, standalone archives, Docker/GHCR, `go install`, or a local source build |

> [!TIP]
> **Why Go?** In the recorded Linux proxy benchmark, idle memory fell from about
> 59 MiB to 8.3 MiB (an **86% reduction**) and peak memory fell from 261–304 MiB
> to 18–20 MiB (a **93–94% reduction**). Startup fell from roughly 330 ms to
> 10 ms. See the [benchmark and methodology](docs/performance.md); real model
> latency is dominated by the upstream Codex service.

> [!NOTE]
> Your ChatGPT plan limits still apply. Usage beyond the included Codex
> allowance may require additional ChatGPT credits, which can cost extra. See
> the [official Codex pricing](https://learn.chatgpt.com/docs/pricing).

> [!WARNING]
> This is an unofficial compatibility server, not the OpenAI Platform API. It
> uses the Codex HTTP backend associated with your ChatGPT login, which may
> change without notice. It does not raise or bypass plan limits. Do not expose
> the server publicly, resell access, or use an account you are not authorized
> to use.

## Installation options

| Method | Requirement | Best for |
| --- | --- | --- |
| [`uvx openai-api-server-via-codex`](#install-uvx) | `uv` and a Codex login | Most users |
| [GitHub Release archive](#install-standalone-binary) | A Codex login | A standalone executable without Python or `uv` |
| [GHCR image](#install-docker) | Docker and a Codex login | Containers and reproducible deployment |
| [`go install ...@latest`](#install-go) | Go 1.27.3+ and a Codex login | Go users |
| [Build from a checkout](docs/build-from-source.md#build-for-the-current-machine) | Go 1.27.3+ and a Codex login | Development and customization |

<a id="install-uvx"></a>
<details>
<summary><strong>Install with uvx or uv tool</strong></summary>

Published wheels contain the Go executable for:

- Linux x86_64 and ARM64
- macOS Intel and Apple silicon
- Windows x86_64 and ARM64

Run without a permanent installation:

```console
$ uvx openai-api-server-via-codex
```

Or install it on your user tool path:

```console
$ uv tool install openai-api-server-via-codex
$ openai-api-server-via-codex --version
```

There is no Python server fallback and no generic source distribution. Build
the Go executable directly on an unsupported platform.

</details>

<a id="install-standalone-binary"></a>
<details>
<summary><strong>Download a standalone release archive</strong></summary>

Each GitHub Release includes versioned archives for Linux, macOS, and Windows
on x86_64 and ARM64, together with `checksums.txt`. Unix archives use `.tar.gz`;
Windows archives use `.zip`.

For example, on an Apple silicon Mac:

```console
$ curl -LO https://github.com/hotchpotch/openai-api-server-via-codex/releases/download/v0.3.0/openai-api-server-via-codex_0.3.0_darwin_arm64.tar.gz
$ curl -LO https://github.com/hotchpotch/openai-api-server-via-codex/releases/download/v0.3.0/checksums.txt
$ grep 'darwin_arm64.tar.gz' checksums.txt | shasum -a 256 --check
$ tar -xzf openai-api-server-via-codex_0.3.0_darwin_arm64.tar.gz
$ ./openai-api-server-via-codex --version
```

The stable archive URLs and SHA-256 checksums are suitable for Homebrew
Formulae. Installing a historical version through Homebrew requires a tap to
retain a versioned Formula such as `openai-api-server-via-codex@0.3.0`.

</details>

<a id="install-docker"></a>
<details>
<summary><strong>Run the published Docker image or build it locally</strong></summary>

Stable Linux x86_64 and ARM64 images are published at
`ghcr.io/hotchpotch/openai-api-server-via-codex`. `latest` tracks the newest
stable release. Exact tags such as `v0.3.0` provide reproducible deployments;
prereleases publish only their exact version tag.

The final Alpine image contains the Go server and CA certificates, but no
Python runtime or Go toolchain. On amd64, local measurements show about 7 MB of
compressed registry layers, a 21 MB local image, and roughly 4–7 MiB of idle
memory. Exact values depend on the release, architecture, and container runtime.

```console
$ docker pull ghcr.io/hotchpotch/openai-api-server-via-codex:latest
$ docker run --rm -p 127.0.0.1:18080:18080 \
    -v ~/.codex:/home/app/.codex \
    ghcr.io/hotchpotch/openai-api-server-via-codex:latest
```

The image does not require a registry login. The auth directory is mounted
read-write so refreshed tokens can be saved.

Or build from this checkout:

```console
$ docker compose run --rm --service-ports codex-login  # only if auth.json is missing
$ docker compose up --build -d
$ curl http://127.0.0.1:18080/healthz
```

The container runs as a non-root user. See the
[Docker guide](docs/docker.md) for Windows, permissions, Compose, login, and
troubleshooting.

</details>

<a id="install-go"></a>
<details>
<summary><strong>Install with Go or build from a checkout</strong></summary>

Go can download and install the command from its public module path:

```console
$ go install github.com/hotchpotch/openai-api-server-via-codex/cmd/openai-api-server-via-codex@latest
$ "$(go env GOPATH)/bin/openai-api-server-via-codex" --version
```

The executable is installed under `GOBIN`, or under `$(go env GOPATH)/bin`
when `GOBIN` is unset.

To build from a checkout:

```console
$ go build -trimpath -o ./bin/openai-api-server-via-codex ./cmd/openai-api-server-via-codex
$ ./bin/openai-api-server-via-codex --version
$ ./bin/openai-api-server-via-codex serve
```

This path does not require Python or `uv`. See
[Building the Go binary from source](docs/build-from-source.md) for version
stamping, installation, Windows commands, static builds, and cross-compilation.

</details>

## Authentication and security

There are two separate authentication layers:

| Layer | Default | Purpose |
| --- | --- | --- |
| Server → Codex | Required | Uses the ChatGPT login in `~/.codex/auth.json` |
| Client → local server | Disabled | Optionally protects incoming `/v1/...` requests with `--api-key` |

### Codex authentication

`serve` and `start` validate the Codex auth file before binding the HTTP port.
Missing, invalid, expired, or unrefreshable credentials fail startup with a
redacted error, a stable reason code, and a suggested action.

The one exception is when an API key is configured, which enables the
[operator console](#operator-console). Failing startup there would make the
console unreachable in exactly the case it exists for — no `auth.json` yet and
no shell to run `codex login` — so the server logs
`codex.auth.preflight_deferred` and starts anyway. `/v1/...` still rejects every
request until the login is established.

The server notices external changes to `auth.json` without a restart. If Codex
returns `401 Unauthorized` before a streaming response begins, the server
clears its credential cache, reloads the file, and retries once. A second `401`
is returned without another retry.

Select another auth file when needed:

```console
$ openai-api-server-via-codex --auth-json /path/to/auth.json
$ OPENAI_VIA_CODEX_AUTH_JSON=/path/to/auth.json openai-api-server-via-codex
```

### Multiple accounts

The proxy can hold several ChatGPT logins and spread requests across them.
Clients see nothing of this: same base URL, same key, same responses.

Accounts live next to the configured `auth.json`, so a single-account setup
needs no change:

| Path | Holds |
| --- | --- |
| `<auth dir>/auth.json` | account `default` — the original login |
| `<auth dir>/accounts/<id>/auth.json` | one directory per added account |
| `<auth dir>/accounts.json` | the accounts an operator disabled |

Add, disable, enable and remove accounts from the [operator console](#operator-console).
Dropping a login into `accounts/<id>/auth.json` by hand also works; it is
picked up within ten seconds.

Routing, per request:

- The ready account with the fewest requests in flight wins. Ties go to the
  account with the lower reported usage (the `x-codex-*-used-percent` headers
  Codex returns), then to the one idle longest. A WebSocket counts as load on
  its account for as long as it stays open.
- A conversation stays on the account that produced its encrypted reasoning,
  because only that account can decrypt it. If that account is unavailable,
  the replayed reasoning is dropped and the request runs elsewhere. A request
  with the same `prompt_cache_key` prefers the same account while it is not
  noticeably busier, which keeps prompt caching effective.
- An account that answers `429` cools down until the reset time Codex reports
  (or for a short default), and one that still answers `401` after a credential
  reload cools down for ten minutes. In both cases the request moves to the next
  account before any byte reaches the client. When every account is cooling, the
  one that recovers first is still tried, so a single-account proxy behaves as
  it always did.
- A disabled account receives no new requests; requests already running on it
  finish. Disabling persists across restarts; a cooldown does not.

Each login must exist in exactly one place. Refresh tokens rotate when used, so
two processes refreshing copies of one login eventually invalidate both. The
console refuses to add a login that is already in the pool.

### Protecting the local API

Configure an incoming API key when other machines or untrusted processes can
reach the server:

```console
$ openai-api-server-via-codex \
    --host 0.0.0.0 \
    --api-key local-secret
```

Clients must then send `Authorization: Bearer local-secret`. `/healthz` remains
unauthenticated.

> [!CAUTION]
> Do not bind to `0.0.0.0` without an incoming API key unless the surrounding
> network provides equivalent access control. The default `127.0.0.1` binding
> is the safest choice for local use.

Incoming API keys, cookies, and `Authorization` headers are never forwarded to
Codex. Normal logs include authentication failure reason codes and upstream
`401` retry decisions, but never raw credentials, tokens, or upstream response
bodies. `--verbose` adds deeper redacted diagnostics.

## Operator console

A small web console at `/ui` for signing the server in to ChatGPT accounts and
managing the [account pool](#multiple-accounts) without shell access — the case that matters when it runs as an unattended
container on a shared host.

It offers two paths:

- **Device code** — starts `codex login --device-auth`, shows the one-time code
  and the verification link, and waits while you approve it on any device with a
  browser. Needs the Codex CLI in the image (the `runtime-ui` target below).
- **Paste an `auth.json`** — the fallback when device-code auth is disabled for
  a workspace. Run `codex login` elsewhere and paste the result. The content is
  validated before it replaces anything, so a bad paste cannot destroy a working
  login.

Both paths add a new account by default; "Sign in again" on an account, or
choosing it under "Save as", replaces that account's login instead.

For every account it shows whether the credentials actually work (not merely
that a file exists), the email and plan, requests in flight, the usage windows
Codex reports, and any cooldown with its reason. Each account can be disabled,
enabled or removed there.

### Access control

The console is gated by `OPENAI_VIA_CODEX_API_KEY`, exchanged for an HttpOnly,
`SameSite=Strict` session cookie — a browser cannot attach a bearer header to a
navigation. **When no API key is set the console refuses to serve** rather than
defaulting open, because signing in through it hands over the ChatGPT account.

Treat the console as privileged: reach it over a private interface or a tunnel,
never the public internet.

### Running it

```console
$ export OPENAI_VIA_CODEX_API_KEY=$(openssl rand -hex 32)
$ docker compose -f docker-compose.console.yml up -d --build
```

Then open `http://<host>:18080/ui` and unlock with that key.

The `runtime-ui` image is Debian-based rather than Alpine because the Codex CLI
ships a glibc-linked binary. The default `runtime` image is unchanged: still
Alpine, still without the CLI, so `docker build` with no `--target` produces the
same lean server image as before.

## Troubleshooting

| Symptom or reason code | What to do |
| --- | --- |
| `auth_file_not_found` | Run `codex login`, or pass the correct path with `--auth-json` |
| `invalid_auth_json` or `unsupported_auth_mode` | Run `codex login` again and confirm the login uses ChatGPT mode |
| `expired_without_refresh_token` | Run `codex login` again to create refreshable credentials |
| `token_refresh_failed` | Check network access and retry `codex login` if the failure persists |
| `auth_file_write_failed` in Docker | Mount `/home/app/.codex` read-write and check host UID/GID permissions |
| Repeated upstream `401` | Check the auth reason logs, refresh the Codex login, and avoid multiple servers sharing rotating credentials |
| Address already in use | Stop the existing server or choose another port with `--port` |
| An official OpenAI SDK requires `OPENAI_API_KEY` | Set the placeholder shown in [Quick start](#quick-start) |

> [!TIP]
> Start with `--verbose` when diagnosing configuration, request routing, or
> Codex stream behavior. Sensitive token-like values remain redacted.

For container-specific problems, see the [Docker guide](docs/docker.md).

## Usage examples

<details>
<summary><strong>Chat Completions</strong></summary>

```python
from openai import OpenAI

client = OpenAI()
chat = client.chat.completions.create(
    model="gpt-6-luna",
    messages=[{"role": "user", "content": "Hello"}],
    reasoning_effort="low",
)
print(chat.choices[0].message.content)
```

</details>

<details>
<summary><strong>Streaming Responses and Chat Completions</strong></summary>

Responses:

```python
stream = client.responses.create(
    model="gpt-6-luna",
    input="Stream a short reply.",
    stream=True,
    reasoning={"effort": "low"},
)

for event in stream:
    if event.type == "response.output_text.delta":
        print(event.delta, end="")
```

Chat Completions:

```python
stream = client.chat.completions.create(
    model="gpt-6-luna",
    messages=[{"role": "user", "content": "Stream a short reply."}],
    stream=True,
    reasoning_effort="low",
)

for chunk in stream:
    if chunk.choices and chunk.choices[0].delta.content:
        print(chunk.choices[0].delta.content, end="")
```

</details>

<details>
<summary><strong>Image input, generation, and editing</strong></summary>

Image input:

```python
response = client.responses.create(
    model="gpt-6-luna",
    input=[
        {
            "role": "user",
            "content": [
                {"type": "input_text", "text": "Describe this image."},
                {
                    "type": "input_image",
                    "image_url": "data:image/png;base64,...",
                },
            ],
        }
    ],
)
```

Image generation:

```python
import base64

image = client.images.generate(
    model="gpt-image-2",
    prompt="A cozy pixel art bowl of ramen, no text.",
    size="1024x1024",
    quality="medium",
    output_format="png",
)

with open("ramen.png", "wb") as file:
    file.write(base64.b64decode(image.data[0].b64_json))
```

Image editing:

```python
edited = client.images.edit(
    model="gpt-image-2",
    image=open("ramen.png", "rb"),
    prompt="Make the bowl blue and keep everything else unchanged.",
    quality="medium",
    output_format="png",
)

with open("ramen-blue.png", "wb") as file:
    file.write(base64.b64decode(edited.data[0].b64_json))
```

Pass a list to `image=` to supply several reference images, and `mask=` to edit
only the masked region. Uploads are inlined as data URLs, so `file_id`
references are not supported.

Streamed generation and editing, with intermediate previews:

```python
stream = client.images.generate(
    model="gpt-image-2",
    prompt="A cozy pixel art bowl of ramen, no text.",
    stream=True,
    partial_images=2,
)

for event in stream:
    if event.type == "image_generation.partial_image":
        print("preview", event.partial_image_index)
    elif event.type == "image_generation.completed":
        with open("ramen.png", "wb") as file:
            file.write(base64.b64decode(event.b64_json))
```

Edits emit `image_edit.partial_image` and `image_edit.completed` instead.
`partial_images` accepts `0`-`3` and is only a hint: observed live runs have
returned both fewer and more previews than requested, so treat the count as
unpredictable and rely on the completed event, which is the only guaranteed one.
Streaming requires `n=1`.

Images are always returned as `data[].b64_json`; `response_format="url"` is not
implemented, because Codex returns image bytes rather than a hosted URL. Codex
also decides the final pixel dimensions, so a `size` request is a hint rather
than a guarantee.

</details>

<details>
<summary><strong>Function tool calling</strong></summary>

```python
response = client.responses.create(
    model="gpt-6-luna",
    input="What is the weather in Tokyo?",
    tools=[
        {
            "type": "function",
            "name": "get_weather",
            "description": "Get the weather for a city.",
            "parameters": {
                "type": "object",
                "properties": {"city": {"type": "string"}},
                "required": ["city"],
                "additionalProperties": False,
            },
            "strict": True,
        }
    ],
)
```

</details>

<details>
<summary><strong>Drawing like the Codex CLI: the <code>/imagegen/v1</code> routes</strong></summary>

`/v1` draws with the hosted `image_generation` tool inside `/codex/responses`.
The Codex CLI (0.166 and later) draws differently: its `image_gen` tool calls
Codex's standalone Images endpoint, `/codex/images/generations` and
`/codex/images/edits`. The same server serves that path under a second base URL:

```python
client = OpenAI(base_url="http://127.0.0.1:8080/imagegen/v1", api_key="...")
```

- `POST /imagegen/v1/images/generations` and `/images/edits` send the request
  straight to the Images endpoint with the CLI's body (`model="gpt-image-2"`,
  `quality` and `size` `"auto"` unless given, `background` opaque or
  transparent). A masked edit keeps the hosted tool: the Images endpoint takes no
  mask. `stream=true` sends one `*.completed` event, without partial images.
- `POST /imagegen/v1/responses` with an `image_generation` tool runs the CLI's
  loop. The model is offered an `imagegen` function instead; when it calls it,
  the server draws on the Images endpoint, shows the client an
  `image_generation_call` item, returns the picture to the model as the call's
  output, and lets the model finish. The model chooses up to five recent
  conversation images to edit (`num_last_images_to_include`). `max_tool_calls`
  bounds the drawings; without it a response draws at most eight.
- `output_format` must be `png` (or omitted): the CLI's request has no format
  field, so `jpeg` and `webp` are refused with a 400 rather than silently drawn
  as PNG. Use `/v1` for them. A drawing round ends early, without another model
  turn, when the model also calls one of the caller's own tools.
- The response reports the `quality` and `size` the endpoint actually used; it
  has been seen to ignore requested values. The server logs each draw as
  `codex.imagegen.done ... quality=... size=...`.
- Every other `/imagegen/v1` route behaves exactly like its `/v1` twin, so a
  client switches by changing only its base URL. The Responses WebSocket stays on
  the hosted tool.

</details>

## API compatibility

<details>
<summary><strong>Supported behavior</strong></summary>

- sync and async `openai-python` clients
- non-streaming and streaming Responses and Chat Completions
- Responses WebSocket relay with upstream conversation state, async tools, and
  mid-turn steering ([usage and Codex limitations](docs/websocket-mode.md))
- `previous_response_id` backed by bounded local context
- stored Chat list/retrieve/update/delete/messages APIs
- Responses retrieve streaming, delete, cancel, and input-token count
- function/tool calling and streamed tool arguments
- JSON mode and structured outputs
- URL and data-URL image input
- hosted image generation and editing translated through a Codex Responses tool
- streamed image previews via `partial_images`
- audio transcription forwarded to Codex
- optional incoming API-key authentication
- bounded request concurrency and in-memory stores

At the Codex HTTP boundary, requests are normalized to `stream=true`, `store=false`,
low text verbosity by default, Codex-compatible tool defaults, and encrypted
reasoning content. Public storage compatibility is implemented in the server's
bounded in-memory stores.

Upstream failures stay failures. Responses keeps them as they are: streaming
forwards `error` and `response.failed`, and non-streaming returns the response
with `status: "failed"` and its `error`. Chat Completions and image generation
cannot represent a failed result, so they return an error instead. A failure
before any output is a real HTTP error, streaming included; a streamed chat that
fails after output has started ends with an OpenAI `error` chunk and no
`finish_reason`. Codex `server_is_overloaded` maps to `503`, rate and usage
limits to `429`, invalid-request and policy codes to `400`, and anything else to
`502`. The upstream `code`, `type`, `param` and `Retry-After` are kept, so
`openai-python` raises its typed errors instead of returning an empty answer.
Transient failures are retried before any output reaches the client
(`server.max_retries`).

Model listing is best-effort. A model can sometimes accept direct requests even
when it is absent from the upstream catalog returned by `/v1/models`.

</details>

<details>
<summary><strong>Implemented endpoints</strong></summary>

| Method | Path |
| --- | --- |
| `GET` | `/healthz` |
| `GET` | `/v1/models` |
| `GET` (WebSocket upgrade) | `/v1/responses` |
| `POST` | `/v1/responses` |
| `GET` | `/v1/responses/{response_id}` |
| `DELETE` | `/v1/responses/{response_id}` |
| `POST` | `/v1/responses/{response_id}/cancel` |
| `POST` | `/v1/responses/input_tokens` |
| `POST` | `/v1/audio/transcriptions` |
| `POST` | `/v1/images/generations` |
| `POST` | `/v1/images/edits` |
| `POST` | `/v1/chat/completions` |
| `GET` | `/v1/chat/completions` |
| `GET` | `/v1/chat/completions/{completion_id}` |
| `POST` | `/v1/chat/completions/{completion_id}` |
| `DELETE` | `/v1/chat/completions/{completion_id}` |
| `GET` | `/v1/chat/completions/{completion_id}/messages` |

WebSocket responses stay on the upstream connection and are not registered with
the local HTTP retrieve/cancel helpers.

Unknown `/v1/...` requests use a best-effort fallback proxy. The server
forwards the method, path, query, safe OpenAI-style headers, and body with its
own Codex credentials. Upstream support determines whether an unknown endpoint
returns `2xx`, `400`, `403`, or `404`.

</details>

<details>
<summary><strong>Not supported</strong></summary>

- Embeddings have no implemented endpoint. Requests reach only the fallback
  proxy, so do not rely on them.
- Image generation does not provide URL results, image editing, or streamed
  partial images.
- Background mode (`background=true`) is rejected, and Codex rejects named
  WebSocket streams (`stream_id`). Neither is emulated; see the
  [WebSocket guide](docs/websocket-mode.md).

</details>

## Operations and configuration

<details>
<summary><strong>Configuration file and important settings</strong></summary>

Generate a configuration file:

```console
$ openai-api-server-via-codex config-generate
$ openai-api-server-via-codex config-generate --stdout
```

The default path is
`$XDG_CONFIG_HOME/openai-api-server-via-codex/config.toml`, falling back to
`~/.config/openai-api-server-via-codex/config.toml`.

Settings resolve in this order:

```text
CLI flag -> environment variable -> config file -> default
```

Example configuration using `gpt-6-luna`:

```toml
[server]
host = "127.0.0.1"
port = 18080
default_model = "gpt-6-luna"
timeout = 300.0
verbose = false
max_stored_items = 1000
max_concurrent_requests = 10
max_retries = 2
# api_key = "change-me"

[codex]
auth_json = "~/.codex/auth.json"
backend_base_url = "https://chatgpt.com/backend-api/codex"
client_version = "1.0.0"
originator = "codex_exec"
user_agent = "codex_exec/0.146.0 (Mac OS 15.6.1; arm64) iTerm.app (codex_exec; 0.146.0)"

[compat]
drop_params = ["fast_mode"]

[daemon]
state_dir = "~/.config/openai-api-server-via-codex/run"
stop_timeout = 10.0
```

| Setting | Default | Purpose |
| --- | --- | --- |
| `server.host` | `127.0.0.1` | HTTP bind address |
| `server.port` | `18080` | HTTP port |
| `server.default_model` | `gpt-5.6-luna` | Model used when a request omits one |
| `server.api_key` | unset | Protect incoming `/v1/...` requests |
| `server.max_stored_items` | `1000` | Bound local stores; `0` disables storage |
| `server.max_concurrent_requests` | `10` | Bound HTTP requests/streams and open WebSocket connections; full capacity returns 503 with `Retry-After`; `0` disables the cap |
| `server.max_retries` | `2` | Retries for transient Codex failures before any output reaches the client; `0` disables |
| `server.timeout` | `300.0` | Codex backend timeout in seconds; responses apply it between stream events |
| `server.verbose` | `false` | Enable redacted application diagnostics |
| `codex.auth_json` | `~/.codex/auth.json` | Codex OAuth file |
| `codex.originator` | `codex_exec` | `originator` header sent upstream |
| `codex.user_agent` | Codex CLI string | `User-Agent` sent upstream; empty falls back to `openai-api-server-via-codex/<version>` |
| `compat.drop_params` | `["fast_mode"]` | Top-level request fields removed before forwarding |

**On `originator` / `user_agent`:** the Codex backend does not treat every
caller alike — requests identifying as the official `codex_exec` CLI are
accepted where a third-party originator is throttled or refused. These default
to the CLI's own values for that reason, and are configurable so the choice
stays visible. `service_tier` is additionally forwarded as an `X-Service-Tier`
header, because passing it in the request body alone had no effect.

`fast_mode` is dropped by default: it measurably hurt latency against Codex.

Examples:

```console
$ openai-api-server-via-codex --port 19090 --verbose
$ OPENAI_VIA_CODEX_MAX_CONCURRENT_REQUESTS=20 openai-api-server-via-codex
$ openai-api-server-via-codex --config ./config.toml
```

Use `drop_params` only for parameters known to be rejected by Codex:

```toml
[compat]
drop_params = ["temperature", "top_p"]
```

`prompt_cache_breakpoint` on `input` content parts is always removed because Codex rejects it; `prompt_cache_key` is still forwarded.

</details>

<details>
<summary><strong>Background daemon commands</strong></summary>

The Go executable implements foreground and daemon lifecycle commands:

```console
$ openai-api-server-via-codex start
$ openai-api-server-via-codex status
$ openai-api-server-via-codex stop
```

PID and log files default to:

```text
~/.config/openai-api-server-via-codex/run/
```

On Linux and macOS, `stop` drains in-flight HTTP requests up to
`--stop-timeout`. Windows terminates the daemon process tree on a best-effort
basis, so an active stream may be interrupted.

Docker should run `serve` in the foreground and let the container runtime manage
restarts; do not use `start` inside a container.

</details>

<details>
<summary><strong>Logging behavior</strong></summary>

Normal operation logs one completion line per API request with its method,
redacted path, status, response size, and duration. Routine `/healthz` probes
stay quiet.

Verbose logs additionally include request starts and redacted query strings,
resolved settings, endpoint summaries, and Codex stream/auth activity. Raw
credentials and token-like values are redacted.

</details>

## Development

<details>
<summary><strong>Build, test, and release documentation</strong></summary>

The HTTP server, backend integration, auth, configuration, stores, daemon, and
redaction logic are implemented in Go under `cmd/` and `internal/`. Python is
used only for the `uvx` launcher, the official `openai-python` consumer
contract, and release tooling.

Requirements:

- Go 1.27.3 or newer
- Python 3.10 or newer
- `uv`

Run the complete deterministic validation suite:

```console
$ uv run tox
```

Focused Go validation:

```console
$ go test ./internal/app
$ go test ./test/e2e -v
$ go test ./...
$ go vet ./...
$ go test -race ./...
```

OpenAI SDK consumer compatibility:

```console
$ uv run python -m pytest tests/test_openai_client_contract.py -q
```

Real Codex tests are opt-in because they use the current login, network, model
allowance, and image quota:

```console
$ RUN_CODEX_LIVE_TESTS=1 uv run python -m pytest tests/test_live_astra_websocket.py -q -s
$ RUN_CODEX_LIVE_TESTS=1 go test ./test/live -v -count=1 -timeout=20m
$ RUN_CODEX_LIVE_TESTS=1 uv run python -m pytest tests/test_live_integration.py -q -s
$ RUN_CODEX_LIVE_TESTS=1 uv run python -m pytest tests/test_live_codex_http_compatibility.py -q -s
```

Releases are prepared on a `release/vX.Y.Z` branch and reviewed through a PR.
Merging the PR does not publish artifacts; pushing the annotated `vX.Y.Z` tag
on the merged commit starts the release workflow.

Further documentation:

- [Building the Go binary from source](docs/build-from-source.md)
- [Go runtime test policy](docs/go-migration.md)
- [Historical Go/Python performance comparison](docs/performance.md)
- [Release process](docs/release.md)

</details>

## Disclaimer

Use this project at your own risk. It is not the official OpenAI Platform API
and is not endorsed or supported by OpenAI. It forwards requests to the Codex
HTTP backend used by the Codex CLI and ChatGPT subscription flow instead of
`api.openai.com`; that backend may change without notice.

Use the server only with accounts and subscriptions you are authorized to use.
Do not evade limits, share account access, resell access, or expose the service
to untrusted networks without authentication. Follow OpenAI's
[Terms of Use](https://openai.com/policies/terms-of-use/) and
[Usage Policies](https://openai.com/policies/usage-policies/).

## License

Apache License 2.0. See [LICENSE](LICENSE).

## Acknowledgements

- Simon Willison's article,
  [A pelican for GPT-5.5 via the semi-official Codex backdoor API](https://simonwillison.net/2026/Apr/23/gpt-5-5/),
  and the implementation described there were the key references for this
  project. Without that article, this approach likely would not have been
  implemented here. Thank you to Simon for documenting the route clearly.
- [OpenClaw](https://github.com/openclaw/openclaw) was a useful reference for
  understanding Codex backend integration patterns.
- [Pi Monorepo](https://github.com/badlogic/pi-mono) was a useful reference for
  Codex backend API behavior and compatibility details.

## Author

- Yuichi Tateno ([@hotchpotch](https://github.com/hotchpotch))

<img height="160" src="https://storage.googleapis.com/secons-site-images/other/blog_images/secon_icon_nendo.webp" alt="Yuichi Tateno" />
