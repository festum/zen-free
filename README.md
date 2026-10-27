# zen-free

`zen-free` is a reverse proxy in front of the OpenCode gateway that speaks like
the OpenCode CLI client (v2.1.4), which is what the free tier checks for. Any
OpenAI-compatible client can point at it to reach the `-free` catalog. The
upstream sees the CLI user agent, a minted `ses_`/`msg_` id pair, the
`x-opencode-*` headers (including a process-stable project ID and session
affinity plus the `x-session-id` alias), `b3`/`traceparent` tracing headers with
a stable trace root per process and a fresh span per request, the agent system
preamble, and a tool list bridged from the captured 12-tool agent manifest: the
`edit`, `glob`, `grep`, `read`, and `write` manifest definitions plus the
captured `bash` definition. Caller-declared tool calls remain visible; calls to
proxy-appended tools are removed from responses. Chat Completions requests on
the inference path default `max_completion_tokens` to 32000 and request usage
with `stream_options.include_usage`.

## zen-free vs [bansos-router](https://github.com/ihsan-ramadhan/bansos-router)

- **Your model choice sticks.** zen-free runs exactly the model the request
  names and never substitutes another. bansos-router registers a smart default
  and, on `401`/`403`/`429`/`5xx`, auto-fails over to the closest equivalent
  model on a different upstream, retrying up to two extra candidates, so a
  pinned model can silently become a different model mid-session.
- **Sticky upstream session.** Every request carries a minted `ses_`/`msg_` id
  pair plus `x-session-affinity` and `x-session-id`, giving upstream session
  state something to pin to.
- **Bring your own key.** The caller's `Authorization` is forwarded untouched,
  and `OPENCODE_GO_API_KEY` is sent as `Bearer` when set, so your own credential
  runs alongside the keyless catalog and stretches how long you can keep using
  it. bansos-router is keyless by design and has no key path at all.
- **Much faster.** zen-free is one Go binary forwarding one request to one
  upstream with nothing in between; bansos-router is a Node daemon that
  health-checks a catalog, parks rate-limited models, and runs cross-provider
  failover before it answers.
- **One upstream, no daemon.** zen-free proxies OpenCode only (`/inference` and
  `/go`) and needs no CLI, dashboard, or background service. bansos-router
  multiplexes Zen, Kilo, LLM7, and OVH behind a daemon with a web UI, a relay
  manager, and per-IP rate limiting.

## Config

- Listener `0.0.0.0:8801`, override with `ZEN_PORT`.
- `ZEN_UPSTREAM`: upstream base URL, defaults to `https://opencode.ai/inference`.
- `ZEN_RELAY_URL`: optional full HTTPS relay endpoint URL; unset means requests connect directly to `ZEN_UPSTREAM`. Relay URLs may include a fixed path, but not userinfo, query, or fragment.
- Relay mode sends requests to the configured relay with `x-relay-target: https://opencode.ai` and the exact final upstream path and query in `x-relay-path`; caller-supplied relay headers are ignored. Relay mode uses one upstream attempt and does not retry or protocol-fallback, avoiding duplicate inference when delivery is uncertain.
- The relay can observe or log request bodies, authorization and fingerprint headers, and responses. Configure only a trusted HTTPS relay.
- `deploy-relay.mjs` publishes that relay to Vercel; see the section below.
- `OPENCODE_GO_API_KEY`: when set, forwarded upstream as `Authorization: Bearer <key>`.
- `ZEN_RETRY_BUDGET`: retry-loop budget in seconds (default `300`); exhausted, the
  proxy answers `502 upstream_interrupted`.
- `ZEN_DEBUG_DUMP`: any non-empty value enables temporary upstream 4xx diagnostics with structural request summaries and capture metadata, without raw bodies. Each handler captures at most 16 records, with a two-second error-body pre-read deadline and a 64 KiB error-body capture limit. Metadata may still contain sensitive identifiers; headers are never included.
- `ZEN_DEBUG_DUMP_RAW`: exactly `1` adds raw bodies only when `ZEN_DEBUG_DUMP` is enabled. Raw client and upstream request bodies are capped at 1 MiB each; raw error bodies are capped at 64 KiB. Raw bodies may contain sensitive user data. Enable diagnostics only for a short investigation and remove both variables afterward.
- Module `github.com/festum/zen-free`, no third-party dependencies; build with
  `go build .` or the included Dockerfile.

## Deploy the relay to Vercel

```bash
export VERCEL_API_KEY=<token>   # https://vercel.com/account/tokens
bun deploy-relay.mjs
```

Paste the printed `https://` URL into `ZEN_RELAY_URL` and recreate the service:

```bash
docker compose -f compose.yaml up -d --no-deps zen
```

The script deploys a Bun-runtime function (`bunVersion: "1.4.x"`), not an edge
function: the upstream gate fingerprints the caller's TLS stack and only Bun's
is accepted, so an edge deployment answers `429`. The handler honours
`x-relay-target`/`x-relay-path` and allowlists `https://opencode.ai`, so the
public URL cannot be used as an open proxy. Set `RELAY_NAME=<name>` to deploy a
second relay under a different project.

## Free-tier gate

The upstream serves a free-tier request only when the body carries
`"stream": true` and a tool named exactly `read`; `stream: false` returns
`403 FreeTierError` whatever the headers say, and arbitrary tool definitions
do not satisfy the gate on their own. This proxy forces `stream: true` upstream.
For upstream URLs under the `/inference` path family, it replaces caller
definitions named `bash`, `edit`, `glob`, `grep`, `read`, or `write` with the
bridged definitions (agent manifest schemas for `edit`, `glob`, `grep`, `read`,
and `write`; the captured `bash` schema), preserves arbitrary tools, and adds
missing gate-required `bash`, `glob`, `grep`, and `read` tools. Empty tool
lists retain the bridged six-tool injection behavior. Outside `/inference`,
tool handling retains its existing behavior. For Chat Completions, the proxy
defaults `tool_choice` to `"none"` only when the caller did not supply tools or
a choice; Responses requests leave the choice unpinned. Only calls to tools
appended by the proxy are removed from responses. Caller-declared calls remain
visible even when their schemas are normalized.

## Run

```bash
git clone https://github.com/festum/zen-free.git
cd zen-free && docker build -t zen-free .
docker run -d --name zen-free -p 127.0.0.1:8801:8801 \
  -e ZEN_UPSTREAM=https://opencode.ai/inference \
  --network <your-bridge-network> zen-free
```

Bifrost runs in Docker, so put `zen-free` on the same bridge network and reach it
as `http://zen-free:8801/v1`; from the host, `http://127.0.0.1:8801/v1`. Add it
like any other OpenAI-compatible provider:

```json
{"name": "zen-free", "base_url": "http://zen-free:8801/v1", "api_key": "your-opencode-api-key"}
```

`GET /v1/models` lists the free catalog without a key.

## OpenCode Go

The Go catalog is the same gateway with `/go` appended:
`https://opencode.ai/inference/go/v1`. It requires a credential, so run a second
instance of this proxy:

```yaml
services:
  oc-zen-go:
    build: .
    container_name: oc-zen-go
    restart: unless-stopped
    ports: ["127.0.0.1:8802:8801"]
    environment:
      - ZEN_UPSTREAM=https://opencode.ai/inference/go
      - OPENCODE_GO_API_KEY=${OPENCODE_GO_API_KEY}
    networks: [internal]
```

Reach it as `http://oc-zen-go:8801/v1`.

## Wires

- OpenAI Chat Completions and Responses, Anthropic Messages, and Gemini requests
  use the `/openai`, `/anthropic`, and `/google` inference families. Query strings
  are retained.
- `muse-spark-*` requests are converted to the Responses wire in flight.
- `POST /v1/responses` on any other model is converted to the chat wire and the
  reply converted back, retrying once on the native Responses route when the
  upstream answers `400 ModelProtocolUnsupported`.
- Chat-backed Responses preserve upstream reasoning as the first `reasoning` item
  with `content[].reasoning_text`, followed by the assistant message when present,
  then function calls. Streaming reasoning remains live; text and tool events wait
  for the upstream terminal condition so reasoning delta/done events finish before
  output text, including when reasoning arrives after content. Replay the complete
  `output` array in subsequent `input` history to restore assistant
  `reasoning_content`; summary-only history uses `summary[].summary_text` as a fallback.
- `jev-*` needs the systemone wire and is rejected with `400`.
