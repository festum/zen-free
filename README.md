# zen-free

`zen-free` is a reverse proxy in front of the OpenCode gateway that speaks like
the OpenCode desktop client, which is what the free tier checks for. Any
OpenAI-compatible client can point at it to reach the `-free` catalog. The
upstream sees the desktop user agent, a minted `ses_`/`msg_` id pair, the
`x-opencode-*` headers and a block of four tools (`bash`, `glob`, `grep`, `read`);
the caller's `Authorization` header is forwarded unchanged and no key is stored.

## Config

- Listener `0.0.0.0:8801`, override with `ZEN_PORT`.
- `ZEN_UPSTREAM`: upstream base URL, defaults to `https://opencode.ai/inference`.
- `OPENCODE_GO_API_KEY`: when set, forwarded upstream as `Authorization: Bearer <key>`.
- `ZEN_RETRY_BUDGET`: retry-loop budget in seconds (default `300`); exhausted, the
  proxy answers `502 upstream_interrupted`.
- Module `github.com/festum/zen-free`, no third-party dependencies; build with
  `go build .` or the included Dockerfile.

## Free-tier gate

The upstream serves a free-tier request only when the body carries
`"stream": true` and a tool named exactly `read`; `stream: false` returns
`403 FreeTierError` whatever the headers say, and arbitrary tool definitions
(`bash`, `glob`, `grep` and any other name) do not satisfy the gate on their own.
This proxy therefore always forces `stream: true` upstream and injects the
`bash`/`glob`/`grep`/`read` quartet when the caller sends none, so a plain chat
call passes.

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

- `muse-spark-*` requests are converted to the Responses wire in flight.
- `POST /v1/responses` on any other model is converted to the chat wire and the
  reply converted back, retrying once on the native `/v1/responses` path when the
  upstream answers `400 ModelProtocolUnsupported`.
- `jev-*` needs the systemone wire and is rejected with `400`.
