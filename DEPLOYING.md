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
Plaid access tokens at rest, and — since the document vault shipped — every
document you file as well. Losing it costs you two very different things: the
bank connections, which you can relink, and the vault, which you cannot. A
tax return whose key is gone is gone. Back the key up somewhere other than the
server it runs on — [section 7](#7-continuity-backups-and-proving-they-work)
covers where, and why no backup this app takes can help you here.

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

```
ledgermancy.yourdomain.com {
    reverse_proxy 127.0.0.1:8081
}
```

**TLS is not optional in production.** Session cookies are marked `Secure` when
`APP_ENV=production`, so the browser will refuse to send them over plain HTTP and
nobody will be able to stay logged in.

### Client IP addresses and your outer proxy

The API rate-limits sign-ins per client address and records that address in the
security audit log, so it has to know who actually connected.

The production overlay sets `TRUST_PROXY_HEADERS=true` for you. That is safe
because the bundled nginx **overwrites** the address headers — it clears
`True-Client-IP` and replaces `X-Forwarded-For` rather than appending to it — and
because the overlay removes the API's host port entirely, so nothing can reach
the API without passing through nginx first.

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
shared bucket, so a single bot probing `/api/auth/login` can exhaust the sign-in
limit for **everyone**, and every row in the audit log names the proxy instead
of a user. If you are not sure what address to use, start the stack and look:

```bash
docker compose logs frontend | tail   # the address at the start of each line
```

Leave `TRUSTED_PROXIES` **empty** if nothing sits in front of the frontend
container. It is then the edge and already sees real client addresses; naming
something there would let that something forge them. Only connections coming
*from* a listed address get their `X-Forwarded-For` believed — anything reaching
the container directly is still pinned to its true address, so publishing the
port stays safe either way.

Do **not** set `TRUST_PROXY_HEADERS=true` on a deployment where the API is
reachable directly. Any caller could then choose its own apparent IP, walk past
every rate limit, and write whatever it liked into the audit log.

Also make sure `.env` is not world-readable — it holds your database password
and both encryption keys:

```bash
chmod 600 .env
```

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
signup form on the public internet. An invite is also bound to the address it
was issued for, so an intercepted link cannot be redeemed under a different one.

---

## 5a. Turn on two-factor authentication

Do this immediately after step 1. This account can read every balance and
transaction in the household, and a password is one phishing email away from
being someone else's.

1. **Security → Set up two-factor.** Your password is required again here — that
   is what stops someone with a stolen browser session attaching their own
   authenticator.
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

### Other things the Security page does

- **Signed-in devices** — every active session with its browser, address and
  last-used time. Revoke any you do not recognise, or sign out everywhere.
- **Recent activity** — the last 50 sign-ins, failures, and security changes.
  Worth a glance now and then; a failed sign-in you did not make is worth
  acting on.
- **Change password** — signs out every other device, by design.

Sessions expire after 30 days, or after 7 days of not being used.

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

## 7. Continuity: backups, and proving they work

This is the only section here whose failure mode is permanent. Everything else
can be fixed by trying again.

The database becomes the only record of your net-worth history — Plaid keeps no
balance history, so a lost database cannot be reconstructed by re-syncing at any
price — and the document vault is the only copy of whatever you uploaded to it.

**Backups are on by default.** You do not have to configure anything for the
rest of this section to already be happening. Every other optional feature in
this app defaults off; this one defaults on, because an operator who has to opt
in to backups is an operator who finds out they never did on the day the disk
fails.

### What runs by itself

Once a day, the worker:

1. **Dumps the database** with `pg_dump` in custom format.
2. **Archives the document vault** to a `tar.gz`. The bytes are already
   encrypted, so the archive is no more sensitive than the volume — and just as
   useless without your key.
3. **Writes a portable export**: plain, documented JSON that needs no Postgres
   and no Ledgermancy to read. See [the continuity
   docs](https://madeofpendletonwool.github.io/ledgermancy/features/continuity/).

Once a week it does the thing almost nobody does by hand:

4. **Restores the latest dump into a scratch database and checks it.** Row
   counts are compared table by table against the live database, the schema
   version is checked, and one document is pulled out of the archive, decrypted
   with your key, and verified against its recorded hash. Then the scratch
   database is dropped.

That last step is the point of the whole feature. An untested backup is not a
backup, it is a belief about a backup.

Old backups are pruned on a 7 daily / 4 weekly / 6 monthly schedule, applied to
what exists rather than to the calendar — a server that was switched off for a
month comes back and keeps what it has.

Status for all of it is in the app, under **Settings → Continuity** (owner
only). Check it once. If it is green, you are in better shape than most
self-hosted deployments; if it is red, it says exactly why.

### Get the backups off this host

By default the backups sit on a Docker volume on the same machine as the
database they protect. If that machine is lost, stolen, or its disk dies, they
are lost with it — which is most of the reason people lose data.

Mount a second location into the worker and point `BACKUP_MIRROR_DIR` at it. A
NAS share, an external disk, a synced folder — anything the container can write
to:

```yaml
# docker-compose.yml, under the worker service
    volumes:
      - backup-data:/var/lib/ledgermancy/backups
      - documents-data:/var/lib/ledgermancy/documents:ro
      - /mnt/nas/ledgermancy:/mnt/backup-mirror   # <- add this
```

```bash
# .env
BACKUP_MIRROR_DIR=/mnt/backup-mirror
```

Every artefact is copied there as it is written, with its own retention.

**Treat that directory as being exactly as sensitive as the database, because it
contains it.** The dump holds your entire financial history in restorable form.
Plaid tokens and document contents inside it stay encrypted under
`ENCRYPTION_KEY`, but everything else — every transaction, balance, and merchant
— is in the clear. Do not point this at anything you would not point the
database itself at.

### The four things a restore needs

Miss any one and the restore fails:

| | Where it lives | What happens without it |
| --- | --- | --- |
| **Code** | git, or the published image | Nothing to run |
| **`.env`** | your password manager | No `ENCRYPTION_KEY` — see below |
| **Database dump** | `backup-data`, and your mirror | No ledger, no history |
| **Document archive** | same | Every document listed, none openable |

`.env` is the one people forget, because it is not in the repository and not in
any backup this app takes — deliberately, since a key stored beside the data it
protects is not protecting it.

### `ENCRYPTION_KEY` — read this twice

`ENCRYPTION_KEY` encrypts every Plaid access token and every document in the
vault. Nothing in this system can recover it, and neither can anyone else:

- **Without it, a perfect database restore is unusable.** The rows are all
  there. The Plaid tokens will not decrypt, so every institution needs
  relinking. No document in the vault will open, ever.
- **It is not in the dump, not in the archive, and not on the mirror.**
- **There is no reset.** There is no company to call. That is the arrangement
  you chose when you self-hosted, and it is a good one right up until this
  moment.

Put it in a password manager **and** keep one copy somewhere that does not
depend on this machine or that password manager being reachable — a printed
sheet in a drawer is not a joke, it is a valid second copy.

The Continuity panel asks you to confirm you have done this, and shows red until
you do. It cannot check, and does not pretend to. Asking is the point.

### Restoring, step by step

This procedure was run start to finish before it was written down. It assumes
you have the four things above and a machine with Docker.

```bash
# 0. Get the code and your .env back in place.
git clone https://github.com/madeofpendletonwool/ledgermancy.git
cd ledgermancy
# ...restore .env from your password manager. Check ENCRYPTION_KEY is present.

# 1. Bring up ONLY the database. Not the whole stack: the api runs migrations
#    on boot, and you want the dump's schema, not a freshly migrated one.
docker compose up -d postgres

# 2. Restore the dump into the empty database.
docker compose cp ledgermancy-db_dump-<stamp>.dump postgres:/tmp/db.dump
docker compose exec -T postgres pg_restore \
  --username=ledgermancy --dbname=ledgermancy \
  --no-owner --no-privileges --exit-on-error \
  /tmp/db.dump

# 3. Check it landed before going further.
docker compose exec -T postgres psql -U ledgermancy -d ledgermancy \
  -c "SELECT count(*) FROM transactions;" \
  -c "SELECT max(version_id) FROM goose_db_version;"

# 4. Restore the document vault. The archive's paths are exactly where the
#    restored database expects to find each file, so it extracts straight in.
docker compose up -d api
docker compose cp ledgermancy-documents_archive-<stamp>.tar.gz api:/tmp/docs.tar.gz
docker compose exec -T api tar xzf /tmp/docs.tar.gz -C /var/lib/ledgermancy/documents
docker compose exec -T api rm /tmp/docs.tar.gz

# 5. Start everything.
docker compose up -d
```

Then **verify by opening a document in the UI**, not just by logging in. Logging
in proves the database restored. Opening a document proves the database, the
archive, and your key all agree — and a restore where two of those three work
looks completely fine until the day you need the third.

If step 2 fails with `input file does not appear to be a valid archive`, the
dump is truncated or corrupt; use an older one. This is precisely what the
weekly restore test exists to tell you *before* you are standing here.

Note the `--exit-on-error` in step 2. Without it `pg_restore` restores what it
can and exits successfully, which would give you a partial database that reports
success — the worst available outcome.

### Doing it by hand

The automated backup is the same operation, so a manual one is only useful for
an ad-hoc copy before something risky:

```bash
docker compose exec -T postgres pg_dump -U ledgermancy -Fc ledgermancy \
  > ledgermancy-$(date +%F).dump

docker run --rm \
  -v ledgermancy_documents-data:/data:ro \
  -v "$PWD":/backup alpine \
  tar czf /backup/ledgermancy-documents-$(date +%F).tar.gz -C /data .
```

Use `-Fc` (custom format) to match what the restore procedure above expects.

If you use the S3 document backend, the bucket is your document storage; make
sure its lifecycle rules do not expire objects the database still references.
The scheduled archive still runs and still pulls every blob, so you get a second
copy either way.

### If you turn backups off

`BACKUP_ENABLED=false` is a supported choice — you may already back up the whole
host at the VM or ZFS layer, which is a perfectly good answer. If you do, make
sure whatever you use captures the Postgres volume in a consistent state, and
test restoring from it. And keep the `ENCRYPTION_KEY` advice above regardless;
it applies to every backup strategy, including yours.

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
