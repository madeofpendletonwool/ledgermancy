# Getting started

This walks you through running Ledgermancy locally against Plaid's **sandbox**
(fake banks, fake money) so you can try every feature without connecting a real
account. When you're ready for real data, follow the [Deployment](deployment.md)
guide.

## Prerequisites

- **Docker** and Docker Compose — the whole app runs in containers, frontend
  included
- A Plaid account — the free **sandbox** is enough to try the app. Sign up at
  [dashboard.plaid.com](https://dashboard.plaid.com/) and grab your sandbox
  keys from *Developers → Keys*.

> Node.js is **not** required to run Ledgermancy. The frontend builds inside its
> own container and is served by nginx. You only need Node if you want
> hot-reloading while editing frontend code — see
> [Development](development.md#working-environment).

## 1. Configure the environment

```bash
git clone https://github.com/madeofpendletonwool/ledgermancy.git
cd ledgermancy
cp .env.example .env
```

Generate the two required secrets and paste them into `.env`:

```bash
openssl rand -base64 32   # -> ENCRYPTION_KEY
openssl rand -base64 32   # -> SESSION_SECRET
```

Fill in your Plaid sandbox keys:

```bash
PLAID_ENV=sandbox
PLAID_CLIENT_ID=<your sandbox client_id>
PLAID_SECRET=<your SANDBOX secret>
PLAID_PRODUCTS=transactions
```

You can leave everything else at its default for a local run. The full set of
options is documented in [Configuration](configuration.md).

!!! note "Leave the AI key blank to start"
    Every feature except the [Assistant](features/assistant.md), the monthly
    recap, and natural-language parsing works with `AI_API_KEY` empty.
    Categorisation falls back to Plaid's own categories. Add a provider later
    when you want the AI layer.

## 2. Start the stack

```bash
docker compose up --build
```

This brings up the whole app — backend **and** frontend — in four services:

| Service    | What it does                                              |
| ---------- | --------------------------------------------------------- |
| `postgres` | PostgreSQL 17, on loopback `127.0.0.1:5433` for `psql`    |
| `api`      | The Go HTTP server, on `api:8080` (loopback `127.0.0.1:8080`) |
| `worker`   | Background jobs: Plaid sync, alerts, net-worth snapshots  |
| `frontend` | nginx serving the built SPA on `${FRONTEND_HOST_PORT:-8081}`, reverse-proxying `/api`, `/webhooks`, `/healthz` to the api |

Migrations run automatically on API startup. Check health through the frontend
(the published edge):

```bash
curl http://localhost:8081/healthz
# {"status":"ok","db":true}
```

## 3. Open the app

Open **http://localhost:8081**. The frontend nginx serves the SPA and proxies
`/api` to the api over the compose network, so the browser only ever sees one
origin — the session cookie stays same-origin and CORS is not involved.

## 4. Create your household

The **first** account you register becomes the household. Everyone after that
joins by invitation from **Settings → Household**.

Registration is invite-only after the first account on purpose: this is a
private household ledger, so an open sign-up form would only be an attack
surface. See [Households](features/households.md) for inviting people and
sharing institutions.

!!! tip "Turn on two-factor next"
    Once you're signed in, go to **Settings → Security → Set up two-factor**
    and enrol a TOTP authenticator. Save the recovery codes; they're shown only
    once. See [Security](security.md).

## 5. Connect your first account

1. Open **Accounts → Connect an account**.
2. Plaid Link opens. In the **sandbox**, pick any institution and log in with
   the test credentials:

   ```
   username: user_good
   password: pass_good
   ```

3. Ledgermancy backfills transaction history (sandbox institutions generate
   ~90 days) and starts the recurring sync loop. The Accounts page shows
   backfill progress and the history span each institution returned.

Once the backfill finishes, **Spending**, **Net worth**, and the **Dashboard**
populate automatically. Explore from there — the rest of this site walks through
each page.

## Sandbox gotchas

- **Short history.** Plaid's sandbox institutions only generate about 90 days
  of transactions, so a sandbox link will never show a full year. That's a
  fixture limit, not a backfill limit — against a real institution the sync
  loop pulls until `has_more` is false (up to ~24 months).
- **Balances move on their own.** Sandbox balances fluctuate on Plaid's
  schedule, which makes net-worth snapshots a bit noisy. Normal.
- **No Investments/Liabilities by default.** `PLAID_PRODUCTS=transactions` is
  the right starting point. Adding `investments` or `liabilities` narrows the
  institution list to banks supporting *every* requested product. See
  [Accounts & Plaid](features/accounts.md#per-institution-products).

## Where to go next

- Take the [feature tour](features/index.md).
- Learn [why the numbers are trustworthy](concepts.md).
- [Deploy](deployment.md) for real when you're ready.
