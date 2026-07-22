# Deploying Ledgermancy and connecting real accounts

Everything so far has run against Plaid's **Sandbox** — fake banks, fake money.
This is how you get to real data on your own server.

---

## 1. Do you need Plaid "Production access"?

**Almost certainly not.** As of **15 April 2026** Plaid offers a free **Trial
plan** for US/Canada developers, and it covers this app completely.

| | Sandbox | **Trial plan** | Production (full) |
| --- | --- | --- | --- |
| Cost | Free | **Free** | Paid |
| Data | Mock | **Real** | Real |
| Item limit | Unlimited | **10 Production Items** | Unlimited |
| Approval | None | **Auto-approved for most developers** | Required, a few business days |

An **Item** is one login at one institution — not one account. Linking a bank
that holds a checking, savings, and credit card is **one** Item. For two people,
ten Items is a lot of institutions.

The Trial plan includes all eight core products, which is everything Ledgermancy
uses and more: Auth, **Transactions** (+ Refresh), Balance, Identity, Assets,
**Liabilities**, **Investments** (+ Refresh), and Statements. It also grants
access to most OAuth institutions — the big banks that require a redirect login —
without the full Production registration.

Apply for full Production only if you exceed 10 Items or need a product outside
that bundle.

> **Note:** *Limited Production*, the old free tier, closed to new US/CA signups
> on 15 April 2026. If you read an older guide mentioning it, or mentioning a
> separate "Development" environment, that guidance is out of date.

### Getting Trial access

1. Create a Plaid Dashboard account and verify your email.
2. Complete the **Trial plan application form** in the Dashboard.
3. Once approved, copy your **production** `client_id` and secret from
   Dashboard → Developers → Keys.

The Trial plan runs against the **Production** environment — it is not a
separate one. So in `.env`:

```bash
PLAID_ENV=production
PLAID_CLIENT_ID=<your production client_id>
PLAID_SECRET=<your PRODUCTION secret, not the sandbox one>
```

Sandbox and Production have **different secrets** for the same `client_id`.
Using the sandbox secret against production fails with an auth error.

---

## 2. Transaction history: get this right the first time

Plaid returns **90 days** of history by default, and its documentation is
explicit that once Transactions is added to an Item, **the requested history
window cannot be changed**. Ledgermancy requests the maximum (**730 days**) at
link time, so a freshly linked institution backfills up to two years.

Two consequences:

- **How much you actually get varies by institution.** Two years is the ceiling,
  not a promise — some banks only return 90 days or 12 months. Check the Accounts
  page after linking; it reports the span that landed.
- **An Item linked with the wrong setting is stuck.** If you ever see an
  institution capped at ~90 days, the only fix is **Unlink and relink** it. There
  is no server-side repair.

If an institution genuinely cannot provide a year, CSV import can fill the gap.

---

## 3. Deploying to your server

```bash
git clone <your repo> ledgermancy && cd ledgermancy
cp .env.example .env
```

Generate the two required secrets:

```bash
openssl rand -base64 32   # -> ENCRYPTION_KEY
openssl rand -base64 32   # -> SESSION_SECRET
```

**Keep `ENCRYPTION_KEY` safe and never rotate it casually.** It encrypts your
Plaid access tokens at rest; lose it and every institution must be relinked.

Fill in the rest of `.env`:

```bash
APP_ENV=production
POSTGRES_PASSWORD=<something long and random>
DATABASE_URL=postgres://ledgermancy:<that password>@postgres:5432/ledgermancy?sslmode=disable
FRONTEND_ORIGIN=https://ledgermancy.yourdomain.com

PLAID_ENV=production
PLAID_CLIENT_ID=...
PLAID_SECRET=...
PLAID_PRODUCTS=transactions
PLAID_WEBHOOK_URL=https://ledgermancy.yourdomain.com/webhooks/plaid
```

Then bring it up with the production overlay, which publishes **no** database
port and binds the API to loopback:

```bash
docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d --build
```

Migrations run automatically on API startup.

### Serving the frontend

The compose stack runs the API and worker. The frontend is a static build:

```bash
cd frontend && npm ci && npm run build   # outputs to frontend/dist
```

Serve `frontend/dist` from a reverse proxy that also terminates TLS and forwards
`/api` and `/webhooks` to the API on `127.0.0.1:8080`. A minimal Caddyfile:

```
ledgermancy.yourdomain.com {
    handle /api/* {
        reverse_proxy 127.0.0.1:8080
    }
    handle /webhooks/* {
        reverse_proxy 127.0.0.1:8080
    }
    handle {
        root * /srv/ledgermancy/dist
        try_files {path} /index.html
        file_server
    }
}
```

`try_files {path} /index.html` is required — the app uses client-side routing, so
a deep link like `/net-worth` must return the app shell rather than a 404.

**TLS is not optional in production.** Session cookies are marked `Secure` when
`APP_ENV=production`, so the browser will refuse to send them over plain HTTP and
nobody will be able to stay logged in.

---

## 4. Webhooks

`PLAID_WEBHOOK_URL` must be reachable from the public internet for Plaid to push
updates. Without it the app still works — the worker sweeps every hour — but new
transactions arrive up to an hour late instead of within seconds.

The endpoint takes no authentication by design (Plaid is not a browser). It is
safe because the payload is treated purely as a hint: the only thing it can
trigger is "re-sync this item", and the sync re-reads everything from Plaid using
our own stored access token. A forged webhook wastes a sync; it cannot alter data.

---

## 5. First run

1. Open your domain and register — **the first account creates the household**.
2. Household → invite your spouse; send them the one-time link.
3. Accounts → **Connect an account**, and link your real banks.
4. Watch the Accounts page: it shows backfill progress and the history span each
   institution returned.
5. Once the backfill finishes, Spending and Net worth populate automatically.

Registration is invite-only after the first account, so the app is not an open
signup form on the public internet.

---

## 6. What happens after 730 days?

**Nothing — your history keeps growing.** The 730-day limit is only about how far
back Plaid will reach *at the moment you link an institution*. Once transactions
land in Ledgermancy's Postgres they are yours, and every sync only *adds* what is
new.

So linking today gives you up to 2 years of backfill, and in five years you will
have roughly seven years of history: the original backfill plus everything
accumulated since. The same is true of net-worth snapshots, which Plaid never had
in the first place — that trend exists only because this app records it daily.

Three things can still remove data, so they are worth knowing:

- **Unlinking an institution deletes its accounts and transactions** (a database
  cascade). Unlink only if you truly want the history gone.
- **Plaid can retract a transaction.** A sync applies `removed` events, which is
  correct — a reversed or duplicated charge should disappear — but it does mean the
  ledger is not strictly append-only.
- **Losing the database loses everything Plaid can no longer re-supply**: anything
  older than the link window, and the entire net-worth trend.

Which makes the next section the important one.

## 7. Back up the database

This becomes the only record of your net-worth history — Plaid keeps no balance
history, so a lost database cannot be reconstructed by re-syncing.

```bash
docker compose exec -T postgres pg_dump -U ledgermancy ledgermancy \
  | gzip > ledgermancy-$(date +%F).sql.gz
```

Back up `.env` too, separately and securely: without `ENCRYPTION_KEY` a database
restore cannot decrypt its own Plaid tokens.

---

## Cost summary

| | Cost |
| --- | --- |
| Plaid Trial plan (≤10 Items) | **$0** |
| Postgres, API, worker | your server |
| AI features (phase 6, optional) | pennies — deterministic rules handle most transactions, and LLM answers are cached per merchant |

**Sources**
- [How are Sandbox, Production, Trial plan, and Limited Production different?](https://support.plaid.com/hc/en-us/articles/16110110883479-How-are-Sandbox-Production-Trial-plan-and-Limited-Production-different)
- [Plaid pricing](https://plaid.com/pricing/)
