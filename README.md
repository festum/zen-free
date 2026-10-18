# zen-free

`zen-free` is a reverse proxy in front of the OpenCode gateway that speaks like
the OpenCode desktop client, which is what the free tier checks for. Any
OpenAI-compatible client can point at it to reach the `-free` catalog. The
upstream sees the desktop user agent, a minted `ses_`/`msg_` id pair, the
`x-opencode-*` headers (including a process-stable project ID and session affinity),
and the six captured OpenCode `bash`, `edit`, `glob`, `grep`, `read`, and `write`
tools when the caller supplies no tools. Caller tool lists are preserved unchanged.

## Config

- Listener `0.0.0.0:8801`, override with `ZEN_PORT`.
- `ZEN_UPSTREAM`: upstream base URL, defaults to `https://opencode.ai/inference`.
- `ZEN_RELAY_URL`: optional full HTTPS relay endpoint URL; unset means requests connect directly to `ZEN_UPSTREAM`. Relay URLs may include a fixed path, but not userinfo, query, or fragment.
- Relay mode sends requests to the configured relay with `x-relay-target: https://opencode.ai` and the exact final upstream path and query in `x-relay-path`; caller-supplied relay headers are ignored. Relay mode uses one upstream attempt and does not retry or protocol-fallback, avoiding duplicate inference when delivery is uncertain.
- The relay can observe or log request bodies, authorization and fingerprint headers, and responses. Configure only a trusted HTTPS relay.
- `OPENCODE_GO_API_KEY`: when set, forwarded upstream as `Authorization: Bearer <key>`.
- `ZEN_RETRY_BUDGET`: retry-loop budget in seconds (default `300`); exhausted, the
  proxy answers `502 upstream_interrupted`.
- Module `github.com/festum/zen-free`, no third-party dependencies; build with
  `go build .` or the included Dockerfile.

## Free-tier gate

The upstream serves a free-tier request only when the body carries
`"stream": true` and a tool named exactly `read`; `stream: false` returns
`403 FreeTierError` whatever the headers say, and arbitrary tool definitions
do not satisfy the gate on their own. This proxy forces `stream: true` upstream
and injects the captured OpenCode `bash`, `edit`, `glob`, `grep`, `read`, and
`write` tools only when the caller provides no tools. For Chat Completions, the proxy
defaults `tool_choice` to `"none"` only when the caller did not supply a choice;
Responses requests leave the choice unpinned. Injected calls are removed from responses, while calls to
caller-supplied tools remain available.

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
- `jev-*` needs the systemone wire and is rejected with `400`.
