# Configuration

All configuration is via environment variables, set in `.env` (copied from
`.env.example`). The compose stack reads that file automatically.

```bash
cp .env.example .env
```

---

## Database

| Variable | Default | Notes |
| --- | --- | --- |
| `POSTGRES_USER` | `ledgermancy` | Database role |
| `POSTGRES_PASSWORD` | — | **Set to something long and random in production** |
| `POSTGRES_DB` | `ledgermancy` | Database name |
| `POSTGRES_HOST_PORT` | `5433` | Loopback-only host port for local `psql`. Not used by the api/worker. The prod overlay removes it. |
| `DATABASE_URL` | — | Used by the api/worker to reach `postgres:5432` over the compose network. `sslmode=disable` is fine while Postgres is a container on the same host. |

!!! warning "If Postgres ever moves off-host"
    Switch `DATABASE_URL` to `sslmode=verify-full`. While it's a container on
    the local bridge network, traffic never leaves the host so `disable` is
    fine.

## Server

| Variable | Default | Notes |
| --- | --- | --- |
| `APP_ENV` | `development` | `production` marks session cookies `Secure` (TLS required) |
| `HTTP_ADDR` | `:8080` | API listen address |
| `FRONTEND_ORIGIN` | `http://localhost:5173` | Origin the browser loads the frontend from (CORS + cookie settings) |
| `FRONTEND_HOST_PORT` | `8081` | Host port the frontend nginx container publishes |
| `API_HOST_PORT` | `8080` | Loopback host port for local dev only; prod overlay removes it |
| `TRUST_PROXY_HEADERS` | `false` | Believe `X-Forwarded-For`/`X-Forwarded-Proto`. See [Deployment](deployment.md#client-ip-addresses-and-your-outer-proxy). |
| `TRUSTED_PROXIES` | _(empty)_ | IPs/CIDRs whose `X-Forwarded-For` is believed. Leave empty if nothing is in front of the frontend container. |

!!! danger "Proxy headers"
    Never set `TRUST_PROXY_HEADERS=true` where the API is reachable directly.
    Any caller could then choose its own apparent IP, walk past every rate
    limit, and poison the audit log. The prod overlay sets it because the
    bundled nginx strips client-supplied headers and is the only route in.

## Secrets

| Variable | Notes |
| --- | --- |
| `ENCRYPTION_KEY` | 32-byte base64 key. Encrypts Plaid access tokens at rest (AES-GCM). **Never rotate casually** — losing it means relinking every institution. Generate: `openssl rand -base64 32` |
| `SESSION_SECRET` | Signs/derives session cookie values. Generate: `openssl rand -base64 32` |

## Plaid

| Variable | Default | Notes |
| --- | --- | --- |
| `PLAID_ENV` | `sandbox` | `sandbox` \| `development` \| `production` |
| `PLAID_CLIENT_ID` | — | From Dashboard → Developers → Keys |
| `PLAID_SECRET` | — | **Different per environment** for the same `client_id` |
| `PLAID_PRODUCTS` | `transactions` | Comma-separated: `transactions`, `investments`, `liabilities`. Start with `transactions` only. |
| `PLAID_WEBHOOK_URL` | _(empty)_ | Public URL Plaid posts to. Blank disables webhooks locally (worker still sweeps hourly). |

See [Deployment](deployment.md) for the Trial plan, history window, and the
730-day one-way door.

## AI provider

Any Anthropic Messages API-compatible endpoint works (GLM, Claude, …) via a
configurable base URL.

| Variable | Default | Notes |
| --- | --- | --- |
| `AI_BASE_URL` | `https://api.z.ai/api/anthropic` | Anthropic Messages API base |
| `AI_API_KEY` | _(empty)_ | Leave blank to run **fully functional with AI disabled** — categorisation falls back to Plaid's categories |
| `AI_MODEL` | `glm-4.6` | Model identifier |

## Push notifications (ntfy)

The **server** is set here, once, for the whole deployment; each user only picks
their private topic in the UI (**Settings → Notifications**).

| Variable | Default | Notes |
| --- | --- | --- |
| `NTFY_BASE_URL` | `https://ntfy.sh` | Self-host ntfy → point at your instance. Otherwise defaults to the public ntfy.sh. |
| `NTFY_TOKEN` | _(empty)_ | Optional Bearer token for a protected / self-hosted ntfy server |
