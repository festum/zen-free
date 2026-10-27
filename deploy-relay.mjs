#!/usr/bin/env bun
// Deploy the Bun-runtime relay to Vercel so zen-free can point ZEN_RELAY_URL
// at a remote, URL-swappable hop instead of the local bun-relay container.
//
// Why Bun and not the default edge runtime: the opencode free-tier gate
// fingerprints the TLS/HTTP stack of the caller and rejects everything except
// Bun (a local bun-relay answers HTTP 200 where the same request over Go or
// node is refused with 429 FreeUsageLimitError). An edge runtime presents the
// platform's own fingerprint, so it reproduces that refusal. Setting
// bunVersion makes Vercel run the function inside Bun, whose fetch makes the
// outbound TLS connection itself.
//
// The function speaks the same scheme zen-free sends in relay mode:
// x-relay-target + x-relay-path (relay.go requestURL returns the relay URL
// unchanged and hands the upstream path over separately). It also falls back
// to the request URL when those headers are absent, so the deployment can be
// probed directly. The target is allowlisted: the deployment is public, and
// without that check anyone could use it as an open proxy.
//
// Usage:  VERCEL_API_KEY=<token> bun deploy-relay.mjs
// Env:    RELAY_NAME  project/deployment name (default zen-free-bun-relay)

const API = "https://api.vercel.com";

const token = process.env.VERCEL_API_KEY;
if (!token) {
	console.error("VERCEL_API_KEY is not set");
	process.exit(1);
}
const name = process.env.RELAY_NAME || "zen-free-bun-relay";

// Deployed source. Kept free of backticks and ${} so it can live inside this
// template literal unchanged; string building uses concatenation.
const FUNCTION = `const ALLOWED_TARGETS = ["https://opencode.ai"];
const HOP_BY_HOP = new Set([
  "connection",
  "keep-alive",
  "proxy-authenticate",
  "proxy-authorization",
  "te",
  "trailers",
  "transfer-encoding",
  "upgrade",
]);

function forwardHeaders(headers, drop) {
  const out = new Headers();
  for (const [name, value] of headers) {
    const lower = name.toLowerCase();
    if (HOP_BY_HOP.has(lower) || drop.includes(lower)) continue;
    out.append(name, value);
  }
  return out;
}

Bun.serve({
  async fetch(req) {
    const url = new URL(req.url);
    const target = (req.headers.get("x-relay-target") || "https://opencode.ai").replace(/\\/+$/, "");
    if (!ALLOWED_TARGETS.includes(target)) {
      return new Response(JSON.stringify({ error: "Forbidden target" }), {
        status: 403,
        headers: { "content-type": "application/json" },
      });
    }
    const relayPath = req.headers.get("x-relay-path") || url.pathname + url.search;
    if (!relayPath.startsWith("/")) {
      return new Response(JSON.stringify({ error: "Bad path" }), {
        status: 400,
        headers: { "content-type": "application/json" },
      });
    }

    // accept-encoding: identity keeps Bun's fetch from handing back a body it
    // already decoded while content-encoding still claims otherwise. The
    // relay headers themselves are for this hop only and must not reach
    // opencode.
    const reqHeaders = forwardHeaders(req.headers, [
      "host",
      "accept-encoding",
      "content-length",
      "x-relay-target",
      "x-relay-path",
    ]);
    reqHeaders.set("accept-encoding", "identity");

    const method = req.method;
    const body = method === "GET" || method === "HEAD" ? undefined : await req.arrayBuffer();

    let upstream;
    try {
      upstream = await fetch(target + relayPath, {
        method,
        headers: reqHeaders,
        body,
        redirect: "manual",
      });
    } catch (err) {
      return new Response(JSON.stringify({ error: String(err) }), {
        status: 502,
        headers: { "content-type": "application/json" },
      });
    }

    const respHeaders = forwardHeaders(upstream.headers, ["content-encoding", "content-length"]);
    return new Response(upstream.body, {
      status: upstream.status,
      statusText: upstream.statusText,
      headers: respHeaders,
    });
  },
});
`;

const files = [
	{ file: "api/server.ts", data: FUNCTION },
	{ file: "package.json", data: JSON.stringify({ name, version: "1.0.0" }) },
	{
		file: "vercel.json",
		data: JSON.stringify({
			bunVersion: "1.4.x",
			rewrites: [{ source: "/(.*)", destination: "/api/server" }],
		}),
	},
];

const auth = {
	Authorization: "Bearer " + token,
	"Content-Type": "application/json",
};

console.log("creating deployment " + name + " (Bun runtime)…");
const dep = await fetch(API + "/v13/deployments", {
	method: "POST",
	headers: auth,
	body: JSON.stringify({
		name,
		files,
		projectSettings: { framework: null },
		target: "production",
	}),
});
if (!dep.ok) {
	const e = await dep.json().catch(() => ({}));
	throw new Error(
		(e.error && e.error.message) || "Vercel deploy failed (HTTP " + dep.status + ")",
	);
}
const depJson = await dep.json();
const depId = depJson.id || depJson.uid;
const projectId = depJson.projectId || name;

// Disable SSO protection so the relay is reachable without a Vercel login.
await fetch(API + "/v9/projects/" + projectId, {
	method: "PATCH",
	headers: auth,
	body: JSON.stringify({ ssoProtection: null }),
});

console.log("waiting for deployment to go live…");
const deadline = Date.now() + 120000;
while (Date.now() < deadline) {
	const s = await fetch(API + "/v13/deployments/" + depId, {
		headers: { Authorization: "Bearer " + token },
	});
	const j = await s.json();
	if (j.readyState === "READY") {
		const url = "https://" + j.url;
		console.log("relay deployed: " + url);
		// One probe: no relay headers means the fallback path is used, so this
		// exercises Bun's outbound fetch to opencode end to end.
		const probe = await fetch(url + "/", { redirect: "manual" }).catch((err) => ({
			status: "probe failed: " + String(err),
		}));
		console.log("probe GET / -> " + probe.status);
		process.exit(0);
	}
	if (j.readyState === "ERROR" || j.readyState === "CANCELED") {
		throw new Error("Deployment failed: " + j.readyState);
	}
	await new Promise((r) => setTimeout(r, 3000));
}
throw new Error("Deployment timed out (120s)");
