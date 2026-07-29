# Deployment

Everything in [Getting started](getting-started.md) runs against Plaid's
**Sandbox** — fake banks, fake money. This is how you get to real data on your
own server.

---

## Do you need Plaid "Production access"?

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
access to most OAuth institutions — the big banks that require a redirect login
— without the full Production registration.

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

The Trial plan runs against the **Production** environment — it is not a separate
one. So in `.env`:

```bash
PLAID_ENV=production
PLAID_CLIENT_ID=<your production client_id>
PLAID_SECRET=<your PRODUCTION secret, not the sandbox one>
```

Sandbox and Production have **different secrets** for the same `client_id`.
Using the sandbox secret against production fails with an auth error.

---

## Transaction history: get this right the first time

Plaid returns **90 days** of history by default, and its documentation is
explicit that once Transactions is added to an Item, **the requested history
window cannot be changed**. Ledgermancy requests the maximum (**730 days**) at
link time, so a freshly linked institution backfills up to two years.

Two consequences:

- **How much you actually get varies by institution.** Two years is the ceiling,
  not a promise — some banks only return 90 days or 12 months. Check the
  Accounts page after linking; it reports the span that landed.
- **An Item linked with the wrong setting is stuck.** If you ever see an
  institution capped at ~90 days, the only fix is **Unlink and relink** it.
  There is no server-side repair.

If an institution genuinely cannot provide a year, the
[CSV importer](features/transactions.md#importing-a-csv) can fill the gap.

---

## Deploying to your server

```bash
git clone <your repo> ledgermancy && cd ledgermancy
cp .env.example .env
```

Generate the two required secrets:

```bash
openssl rand -base64 32   # -> ENCRYPTION_KEY
openssl rand -base64 32   # -> SESSION_SECRET
```

!!! danger "Keep `ENCRYPTION_KEY` safe"
    It encrypts your Plaid access tokens at rest. Lose it and every institution
    must be relinked. Never rotate it casually.

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

The compose stack runs everything: Postgres, the API, the worker, and the
**frontend**. The frontend is an nginx container (`frontend/Dockerfile`) that
builds the SPA and reverse-proxies `/api`, `/webhooks`, and `/healthz` to the
API over the internal compose network. It handles the SPA deep-link fallback
itself, so `/net-worth` and friends return the app shell rather than a 404.

In production the API has **no host port** — the frontend is the only published
service. It binds the host port you set in `.env`:

```bash
FRONTEND_HOST_PORT=8081   # whatever port your TLS proxy will forward to
```

Put a TLS-terminating reverse proxy in front of that port. Because the frontend
already handles the `/api` + `/webhooks` split internally, the outer proxy only
has to terminate TLS and forward everything through. A minimal Caddyfile:

```caddy
ledgermancy.yourdomain.com {
    reverse_proxy 127.0.0.1:8081
}
```

!!! warning "TLS is not optional in production"
    Session cookies are marked `Secure` when `APP_ENV=production`, so the
    browser will refuse to send them over plain HTTP and nobody will be able to
    stay logged in.

---

## Client IP addresses and your outer proxy

The API rate-limits sign-ins per client address and records that address in the
security audit log, so it has to know who actually connected.

The production overlay sets `TRUST_PROXY_HEADERS=true` for you. That is safe
because the bundled nginx **overwrites** the address headers — it clears
`True-Client-IP` and replaces `X-Forwarded-For` rather than appending to it —
and because the overlay removes the API's host port entirely, so nothing can
reach the API without passing through nginx first.

This takes **two** settings, and getting only the first one right is the common
mistake:

1. Your outer TLS proxy must *send* `X-Forwarded-For` and `X-Forwarded-Proto`.
   Caddy and Traefik do this by default; nginx and HAProxy need it configured.
2. The bundled nginx must be told to *believe* it, by naming that proxy in
   `TRUSTED_PROXIES`.

```bash
# In .env — the address the outer proxy connects FROM, not the address it
# listens on. Comma- or space-separated; IPs and CIDR ranges both work.
TRUSTED_PROXIES=10.0.0.4
```

Skip step 2 and every request in the world resolves to your proxy's address.
That is not just cosmetic: the per-client rate limits all collapse into one
shared bucket, so a single bot probing `/api/auth/login` can exhaust the
sign-in limit for **everyone**, and every row in the audit log names the proxy
instead of a user. If you are not sure what address to use, start the stack and
look:

```bash
docker compose logs frontend | tail   # the address at the start of each line
```

Leave `TRUSTED_PROXIES` **empty** if nothing sits in front of the frontend
container. It is then the edge and already sees real client addresses; naming
something there would let that something forge them. Only connections coming
*from* a listed address get their `X-Forwarded-For` believed — anything reaching
the container directly is still pinned to its true address, so publishing the
port stays safe either way.

!!! danger "Never set `TRUST_PROXY_HEADERS=true` without a sanitising proxy"
    Any caller could then choose its own apparent IP, walk past every rate
    limit, and write whatever it liked into the audit log.

Also make sure `.env` is not world-readable — it holds your database password
and both encryption keys:

```bash
chmod 600 .env
```

See [Configuration](configuration.md) for every variable.

---

## Webhooks

`PLAID_WEBHOOK_URL` must be reachable from the public internet for Plaid to push
updates. Without it the app still works — the worker sweeps every hour — but new
transactions arrive up to an hour late instead of within seconds.

The endpoint takes no authentication by design (Plaid is not a browser). It is
safe because the payload is treated purely as a hint: the only thing it can
trigger is "re-sync this item", and the sync re-reads everything from Plaid
using our own stored access token. A forged webhook wastes a sync; it cannot
alter data.

---

## First run

1. Open your domain and register — **the first account creates the household**.
2. Settings → Household → invite your spouse; send them the one-time link.
3. Accounts → **Connect an account**, and link your real banks.
4. Watch the Accounts page: it shows backfill progress and the history span each
   institution returned.
5. Once the backfill finishes, Spending and Net worth populate automatically.

Registration is invite-only after the first account, so the app is not an open
signup form on the public internet. An invite is also bound to the address it
was issued for, so an intercepted link cannot be redeemed under a different one.

---

## Turn on two-factor authentication

Do this immediately after step 1. This account can read every balance and
transaction in the household, and a password is one phishing email away from
being someone else's.

1. **Settings → Security → Set up two-factor.** Your password is required again
   here — that is what stops someone with a stolen browser session attaching
   their own authenticator.
2. Scan the QR with any TOTP app (Google Authenticator, 1Password, Aegis,
   Bitwarden). If you cannot scan, the base32 key below it can be typed in.
3. Enter the 6-digit code to confirm. Enrolment is not complete until you do —
   an unconfirmed secret never gates a login, so a mis-scan cannot lock you out.
4. **Save the ten recovery codes.** They are shown exactly once; only hashes are
   stored, so nobody can recover them for you afterwards.

Enabling two-factor signs out every other device on the account.

### If you lose your phone

Sign in and enter one of your recovery codes instead of the 6-digit code. Each
works once. Generate a fresh set from the Security page afterwards.

**If you have lost the phone *and* the recovery codes**, there is no email
recovery — this app sends no mail at all. Clear the second factor directly:

```bash
docker compose exec postgres psql -U ledgermancy -d ledgermancy -c \
  "UPDATE users SET totp_enabled = false, totp_secret_encrypted = NULL, \
                    totp_confirmed_at = NULL, totp_last_step = NULL \
   WHERE email = 'you@example.com';"
```

That returns the account to password-only. Sign in and enrol again straight
away. Anyone who can run that command already has your whole database, so it
grants nothing they did not have — but it does mean shell access to the server
is equivalent to account access. Guard it accordingly.

See [Security](security.md) for the full model.

---

## What happens after 730 days?

**Nothing — your history keeps growing.** The 730-day limit is only about how
far back Plaid will reach *at the moment you link an institution*. Once
transactions land in Ledgermancy's Postgres they are yours, and every sync only
*adds* what is new.

So linking today gives you up to 2 years of backfill, and in five years you will
have roughly seven years of history: the original backfill plus everything
accumulated since. The same is true of net-worth snapshots, which Plaid never
had in the first place — that trend exists only because this app records it
daily.

Three things can still remove data, so they are worth knowing:

- **Unlinking an institution deletes its accounts and transactions** (a database
  cascade). Unlink only if you truly want the history gone.
- **Plaid can retract a transaction.** A sync applies `removed` events, which is
  correct — a reversed or duplicated charge should disappear — but it does mean
  the ledger is not strictly append-only.
- **Losing the database loses everything Plaid can no longer re-supply**:
  anything older than the link window, and the entire net-worth trend.

Which makes the next section the important one.

---

## One-off: normalising imported merchant keys

Skip this unless you imported transactions from CSV **before** the descriptor
normaliser was applied to that path.

Every transaction carries a `merchant_key`, the stable handle that merchant
grouping, the categorisation cache and recurring detection all key off. Plaid rows
always went through the normaliser that strips store numbers, processor prefixes
and order ids. CSV rows did not — they were only lower-cased — so one business
arrived fragmented across dozens of keys: a separate "merchant" for every
`AMAZON MKTPL*<order id>` and every `KWIK TRIP #<store>`.

The import path is fixed. To repair rows it already wrote:

```bash
# Read-only: prints what it would change and stops.
docker compose exec api normalise-merchant-keys

# Writes, inside one transaction.
docker compose exec api normalise-merchant-keys --apply
```

**Take a backup first.** The command rewrites keys that `merchant_category_map`,
`merchant_aliases` and `recurring_overrides` all point at, and collapses the
duplicates that result — keeping the highest-precedence category mapping (manual
over rule over model) and reporting any descriptors you had filed under two
different merchants.

It moves no money. Amounts, dates and category assignments on transactions are
untouched; only the merchant handle and the per-key tables change. Verify with the
totals on [Spending](features/spending.md) before and after — they must match.

---

## Backups

This becomes the only record of your net-worth history — Plaid keeps no balance
history, so a lost database cannot be reconstructed by re-syncing.

**Backups are on by default and you do not have to set them up.** Once a day the
worker dumps the database, archives the document vault, and writes a portable
JSON export. Once a week it restores the latest dump into a temporary database
and verifies it, including opening one document end to end — your live database
is never touched. Status is at **Settings → Continuity**.

Two things are still yours to do:

- **Keep more than one copy.** `BACKUP_DIR` defaults to a Docker volume on this
  machine; point it somewhere else, or set `BACKUP_MIRROR_DIR` to a second
  location, or both. The app cannot tell what hardware is under either path, so
  it will not claim your backups are or are not on this host — that part is
  yours to know.
- **Back up `.env` separately and securely.** It holds `ENCRYPTION_KEY`, which
  is in no backup this app takes, deliberately. Without it a restored database
  cannot decrypt its own Plaid tokens or open a single document.

See [Continuity & backups](features/continuity.md) for the format of the export
and how to read the panel, and
[DEPLOYING.md §7](https://github.com/madeofpendletonwool/ledgermancy/blob/main/DEPLOYING.md#7-continuity-backups-and-proving-they-work)
for the tested restore procedure.

---

## Network exposure

Postgres is an internal service. The api and worker always reach it over the
compose network at `postgres:5432`; no host port is involved in normal
operation.

| | Postgres | API |
| --- | --- | --- |
| Development (`docker compose up`) | `127.0.0.1:5433` — loopback only, for `psql` | `0.0.0.0:8080` |
| Production (with the prod overlay) | not published at all | `127.0.0.1:8080`, behind a reverse proxy |

```bash
# Production: database unreachable from the host entirely
docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d --build

# Get a psql shell without publishing a port
docker compose exec postgres psql -U ledgermancy -d ledgermancy
```

Two things here are easy to get wrong and are deliberate:

- The dev mapping is written `127.0.0.1:5433:5432`. Writing `5433:5432` binds
  `0.0.0.0` and exposes the database to the entire LAN.
- The prod overlay uses `ports: !override []`. Compose *merges* list fields
  across files, so a plain `ports: []` does nothing and the base binding
  survives.

Verify at any time with `docker compose ps` — the `Ports` column shows the real
bind address.

---

## Cost summary

| | Cost |
| --- | --- |
| Plaid Trial plan (≤10 Items) | **$0** |
| Postgres, API, worker | your server |
| AI features (optional) | pennies — deterministic rules handle most transactions, and LLM answers are cached per merchant |

**Sources**

- [How are Sandbox, Production, Trial plan, and Limited Production different?](https://support.plaid.com/hc/en-us/articles/16110110883479-How-are-Sandbox-Production-Trial-plan-and-Limited-Production-different)
- [Plaid pricing](https://plaid.com/pricing/)
