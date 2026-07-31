# Security

## Deployment model

flaresolverr-go drives a real browser to arbitrary URLs on behalf of whoever can reach its API.
It is designed to run on a **trusted network** — a homelab, a container network, or localhost.

`/settings` and `/api/settings` have **no authentication**. This is a deliberate design decision,
not an oversight. Do not expose the service directly to the internet.

Recommended:

- bind to `127.0.0.1` or an internal interface (`host` in `init.yaml`, or `HOST`)
- restrict access with a firewall, a reverse-proxy ACL, or a VPN
- if a reverse proxy is used, put authentication in front of `/settings` and `/api/settings`

## What is enforced in-process

- `POST /api/settings` requires `Content-Type: application/json` and rejects cross-site
  `Origin` / `Sec-Fetch-Site`. Without this, a POST with `Content-Type: text/plain` is a CORS
  *simple request* — no preflight — so any page an operator visits could rewrite `browser_path`
  or `driver_path` and get an executable of its choosing spawned, persisted to `init.yaml`.
- `chrome_for_testing_url` must be `https`. The driver fetched from it is `chmod 0755`'d and
  executed.
- Downloaded driver archives are size-capped.
- `/v1` accepts only `http` and `https` URLs, so `file://` and `chrome://` cannot be used to read
  local files or internal pages through the browser.
- Request bodies are size-limited and the HTTP server has read/idle timeouts.

## Known limitations

- `GET /api/settings` returns the configured proxy password in cleartext to anyone who can reach
  the endpoint.
- There is no cap on the number of sessions or on `maxTimeout`. A caller that can reach `/v1` can
  exhaust host memory by creating browsers. Rate-limit in front of the service if the caller is not
  fully trusted.
- The `geckodriver` backend cannot pass proxy credentials (a W3C protocol limitation); it logs a
  warning and contacts the proxy unauthenticated.

## Reporting a vulnerability

Open an issue at https://github.com/trinity-aml/flaresolverr-go/issues. If the report involves a
vulnerability that is not already covered by the deployment model above, please mark it clearly and
avoid posting a working exploit in the initial report.
