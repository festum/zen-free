# zen-free

`zen-free` is a reverse proxy in front of OpenCode Zen that speaks like the OpenCode
desktop client, which is what the free tier checks for. Any OpenAI-compatible client
can point at it: the upstream sees the desktop user agent, freshly minted `ses_` and
`msg_` ids, `x-opencode-*` headers, and a block of four decoy tools (`bash`, `glob`,
`grep`, `read`). The `Authorization` header of the caller is forwarded unchanged and
no key is stored anywhere.

The fingerprint work follows [stgmt](https://gist.github.com/stgmt/00e19fddf0ef1374515a58eee8884a14)'s
gist, which first showed a `200 OK` against the real endpoint. This proxy implements
that header set (`opencode/<version>` user agent, `ses_`/`msg_` ids, `x-opencode-*`
headers, four-tool block) and the tool padding the gist left to the caller.

The listener binds `0.0.0.0:8801` and the port is overridable with `ZEN_PORT`. The
upstream base URL comes from `ZEN_UPSTREAM` and defaults to
`https://opencode.ai/zen`. `ZEN_RETRY_BUDGET` sets the whole retry loop budget in
seconds (default `120`): the upstream gets that long to answer before the proxy
returns `502` with `upstream_interrupted`. The response header timeout follows the
budget, so a value above `90` also raises it. Invalid or missing values fall back
to the default. The module is `github.com/festum/zen-free`, with no
third-party dependencies. Build it with `go build .` or the included Dockerfile.

## Usage

### Docker

```bash
git clone https://github.com/festum/zen-free.git
cd zen-free
docker build -t zen-free .
docker run -d --name zen-free \
  -p 127.0.0.1:8801:8801 \
  -e ZEN_UPSTREAM=https://opencode.ai/zen \
  --network <your-bridge-network> \
  zen-free
```

Bifrost runs in Docker, so put `zen-free` on the same bridge network and reach it
as `http://zen-free:8801/v1`. From the host the port is published on loopback only.

The image build reads the newest opencode release tag and bakes it into the
`opencode/x.y.z` fingerprint, so the version in the user agent no longer sits in the
source. Use `--build-arg OPENCODE_VERSION=1.18.35` to bake a specific version.

### compose.yaml

```yaml
services:
  zen-free:
    build: .
    container_name: zen-free
    restart: unless-stopped
    ports:
      - 127.0.0.1:8801:8801
    environment:
      - ZEN_UPSTREAM=https://opencode.ai/zen
    networks:
      - internal
```

### Consumer side (Bifrost or any OpenAI-compatible client)

Add it the way you add any other OpenAI-compatible provider:

```json
{
  "name": "zen-free",
  "base_url": "http://zen-free:8801/v1",
  "api_key": "your-opencode-api-key"
}
```

`api_key` takes your own OpenCode API key. From the host, use
`http://127.0.0.1:8801/v1` instead. Streaming must be on for chat models, as the
Note below explains. `GET /v1/models` lists the free catalog without a key; after
that, call `chat/completions` as usual. `muse-spark-*` requests are converted to the
Responses wire in flight.

### OpenCode Go

OpenCode Go is the same gateway with the endpoint extended by `/go`
(`https://opencode.ai/zen/go/v1`) and one `OPENCODE_API_KEY` covers both catalogs.
Run a second instance of this proxy against that path:

```yaml
services:
  oc-zen-go:
    build: .
    container_name: oc-zen-go
    restart: unless-stopped
    ports:
      - 127.0.0.1:8802:8801
    environment:
      - ZEN_UPSTREAM=https://opencode.ai/zen/go
    networks:
      - internal
```

Reach it as `http://oc-zen-go:8801/v1` and take the catalog from
`GET /v1/models`. Every upstream request carries a minted `x-opencode-session`
header, so the proxy also covers the header requirement behind
[maximhq/bifrost#6815](https://github.com/maximhq/bifrost/issues/6815). The Go
path itself is not verified yet.

## Note

- The free tier needs `"stream": true` in the request body. A false or missing
  `stream` field returns `403 FreeTierError` whatever the headers say, while `true`
  returns 200 with any `Accept` header.
- The key travels to the upstream untouched: your own OpenCode API key, or `public`
  on the free lane. An unknown key returns `401 AuthError: Invalid API key`.
