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
| `ENCRYPTION_KEY` | 32-byte base64 key. Encrypts Plaid access tokens **and every document in the vault** at rest (AES-GCM). **Never rotate casually** — losing it means relinking every institution, and losing every document permanently. Generate: `openssl rand -base64 32` |
| `SESSION_SECRET` | Signs/derives session cookie values. Generate: `openssl rand -base64 32` |

## Plaid

| Variable | Default | Notes |
| --- | --- | --- |
| `PLAID_ENV` | `sandbox` | `sandbox` \| `development` \| `production` |
| `PLAID_CLIENT_ID` | — | From Dashboard → Developers → Keys |
| `PLAID_SECRET` | — | **Different per environment** for the same `client_id` |
| `PLAID_PRODUCTS` | `transactions` | Products an institution **must** support to be offered in Link. Every entry here *shrinks* the institution list, so leave it alone. |
| `PLAID_OPTIONAL_PRODUCTS` | `investments,liabilities` | Pulled where the institution and accounts support them, ignored where not — never a filter on which banks a user can pick. Also the switch for syncing them on **already-linked** accounts (no relink). Set empty to opt out. |
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
| `AI_ADDITIONAL_MODELS` | _(empty)_ | Comma-separated model ids selectable **in the Advisor chat only**. With one or more set, the chat window grows a model picker (the primary stays the default); every other AI feature keeps using `AI_MODEL` regardless. |

## Push notifications (ntfy)

The **server** is set here, once, for the whole deployment; each user only picks
their private topic in the UI (**Settings → Notifications**).

| Variable | Default | Notes |
| --- | --- | --- |
| `NTFY_BASE_URL` | `https://ntfy.sh` | Self-host ntfy → point at your instance. Otherwise defaults to the public ntfy.sh. |
| `NTFY_TOKEN` | _(empty)_ | Optional Bearer token for a protected / self-hosted ntfy server |

## Email (SMTP)

Off by default, and this switch carries more weight than the others on this page:
**until you set `SMTP_HOST`, Ledgermancy sends no email at all.** Configuring a
mail server is how you withdraw that guarantee for your own deployment.

The only thing ever mailed is the [digest](features/digest.md), and only to
members who tick **Email it to me** in **Settings → Digest**. An operator with
SMTP configured and nobody opted in still sends nothing.

Setting `SMTP_HOST` without `SMTP_FROM` **fails at startup**, rather than
discovering at 6am on a Monday that every message was rejected.

| Variable | Default | Notes |
| --- | --- | --- |
| `SMTP_HOST` | _(empty)_ | Mail server hostname. Empty = no email, ever. |
| `SMTP_PORT` | `587` | `587` for STARTTLS, `465` for implicit TLS, `25` for a local relay |
| `SMTP_FROM` | _(empty)_ | Envelope sender and `From:` header. **Required** when `SMTP_HOST` is set. |
| `SMTP_USERNAME` | _(empty)_ | Leave blank for a relay that needs no authentication |
| `SMTP_PASSWORD` | _(empty)_ | |
| `SMTP_SECURITY` | `starttls` | `starttls`, `tls` (implicit, usually port 465), or `none` |

!!! note "There is no certificate-verification bypass"
    Both encrypted modes verify the server's certificate against `SMTP_HOST`. A
    switch that turns an encrypted channel into an unauthenticated one is a
    footgun; an operator with no usable certificate can say so honestly with
    `SMTP_SECURITY=none` instead. Note that Go refuses to send a password over an
    unencrypted connection to anything but localhost — that is correct, and is
    not worked around.

## Benchmark prices (Investments page)

Off by default. This is one of only two outbound requests Ledgermancy makes to a
host that is neither Plaid nor your AI provider — the other is
[merchant logos](#merchant-logos) — so it is opt-in rather than something you
have to notice and disable. Enabling it lets a daily job fetch end-of-day
index closes from [Stooq](https://stooq.com) so the Investments page can plot
your portfolio's growth against the market. Only a ticker symbol is sent — no
account, balance or transaction data leaves the host.

With it off, the Investments page works exactly as before; the benchmark chart
simply shows your own line with nothing to compare it against, and says so.

| Variable | Default | Notes |
| --- | --- | --- |
| `BENCHMARK_PRICES_ENABLED` | `false` | Set `true` to allow the daily price fetch |
| `BENCHMARK_TICKERS` | `SPY,VTI,BND,QQQ` | US symbols, comma-separated. Queried at Stooq as e.g. `spy.us` |

A failed fetch degrades to a missing series: the job logs a warning, stores what
it did get, and never fails or retry-storms over a chart decoration.

## CPI refresh (inflation-adjusted views)

**Off by default**, and the least consequential of the outbound switches,
because the feature it serves works fully without it.

The CPI-U series ships **seeded** — January 2010 onward, committed in migration
`00052_cpi_series.sql`. Every real (inflation-adjusted) figure in the app is
computed from that bundled series. An install with no route to the internet gets
honest deflation from real published numbers; it simply stops gaining new months,
and the UI says so rather than quietly showing older dollars as though they were
today's.

Turning this on adds one request a day to `api.bls.gov` for the tail of the
series. No key and no account: the v1 endpoint serves a small number of series
unregistered, which is all one series over two years needs. The request names a
public series ID and a year range and is identical for every install; nothing
about your household is in it.

| Variable | Default | Notes |
| --- | --- | --- |
| `CPI_FETCH_ENABLED` | `false` | Set `true` to allow the daily CPI-U refresh |

The job **upserts** on period rather than inserting, because BLS revises. It
also skips months published as `-`: those were never collected — October 2025 is
the live example, lost to that year's lapse in appropriations — and a figure
dated in such a month is reported as undeflatable rather than being deflated
against an interpolated guess.

## Outgoing webhooks

**Off by default**, and this is the switch on this page that deserves the most
thought.

Every other outbound switch adds **one** host that *we* chose — Stooq, BLS,
Logo.dev — receiving a ticker or a merchant name. This one lets members of the
household choose the host, and what it receives is their own financial events: an
alert with an amount and a merchant, a newly-raised insight, a goal contribution.
That is a capability rather than a data source, so it is a decision rather than a
default.

With it off there is **no delivery worker registered**, every `/api/webhooks`
route answers `503`, and no producer writes a webhook row. An instance that never
sets this cannot make an outbound webhook request even by accident, and carries
no webhook rows to appear in a backup.

| Variable | Default | Notes |
| --- | --- | --- |
| `WEBHOOKS_ENABLED` | `false` | Set `true` to let households configure outgoing webhooks |

There is no credential of ours here. Each subscription mints its **own** signing
secret, shown to its creator once and stored sealed with `ENCRYPTION_KEY` — so a
leaked database does not yield a key anybody can forge deliveries with.

`http://` destinations are accepted alongside `https://`, deliberately: the
flagship receiver for a self-hosted finance app is a Home Assistant or a script
on the same LAN. The signature makes an unencrypted delivery **tamper-evident,
not private**; over the public internet, use TLS. Redirects are never followed,
so a receiver cannot forward a signed payload somewhere else.

See [Webhooks](features/webhooks.md) for the payload, the signature recipe and
the retry policy.

## Merchant logos

**Off by default.** This is the second of the two switches that add a host which
is neither Plaid nor your AI provider, and unlike the benchmark fetch above it
sends a merchant *name* rather than a ticker.

Every merchant already has imagery without this: a coloured tile carrying its
first letter, generated locally from the name, identical on every reload. Turning
this on replaces that tile with the company's real logo where one can be found,
and leaves the tile everywhere else.

**How it works.** Once a day the worker takes the merchants it has not seen
before and asks your AI provider which website each one is — "Blue Bottle
Coffee" → `bluebottlecoffee.com`. It then asks [Logo.dev](https://logo.dev) for
that domain's logo, stores the image in the database, and serves it from this
app. Both steps happen **once per merchant, ever**: a merchant with a logo is
never re-fetched, and a merchant without one is recorded as having none and
never asked about again.

**What leaves the host.** To your AI provider, the merchant's name — which it
already sees during categorisation, so this step adds no new destination. To
Logo.dev, a domain, one request per merchant. No amount, balance, account, date
or transaction reaches either one.

**Your browser never contacts Logo.dev.** Every image is fetched server-side and
served from this origin, so a page still loads nothing from a third party. The
api never proxies either: if the worker has not cached a logo, the answer is a
404 and the monogram is used.

| Variable | Default | Notes |
| --- | --- | --- |
| `MERCHANT_LOGOS_ENABLED` | `false` | Set `true` to allow the daily resolve-and-fetch pass |
| `LOGO_DEV_TOKEN` | — | Required when enabled. The **publishable** key (`pk_…`), not the secret one — see below. Free tier: 500,000 requests/month, no card. Sign up at [logo.dev](https://logo.dev) |
| `MERCHANT_LOGOS_SIZE` | `128` | Requested square size in pixels. Logo.dev's ceiling is 800 |
| `MERCHANT_LOGOS_MAX_BYTES` | `128KB` | Per-logo storage cap. A larger response is treated as "no logo" |

Enabling this **also needs `AI_API_KEY`**: Logo.dev is keyed by domain only, and
the AI provider is what turns a merchant name into one. The app refuses to start
if the switch is on without a key or a token, rather than presenting a feature
that quietly does nothing.

Each household can switch the imagery off for itself in **Settings →
Appearance**. Doing so stops the lookups for that household and deletes the
logos already cached for it — the cache is derived data about where they shop,
and keeping it past a "no" would be keeping the part they objected to.

### Which Logo.dev key, and why only one

Logo.dev issues two keys and only the **publishable** one (`pk_…`) belongs in
`LOGO_DEV_TOKEN`. There is nowhere in this app to put the secret key.

That is a consequence of using exactly one Logo.dev surface. The CDN image
endpoint, `img.logo.dev/{domain}`, authenticates with `?token=pk_…` and is free
at 500,000 requests a month. The **Brand API** (`api.logo.dev/brand/{domain}`),
which is what the secret key is for, returns brand profiles — colours, socials,
descriptions — at 5 credits per call, roughly 100 calls a month on the free
plan. Nothing here wants a brand profile, and name→domain resolution goes
through your AI provider precisely so that no part of this feature is metered.
The result is that turning merchant logos on cannot generate a bill.

Note that Logo.dev's publishable key is designed to sit in a public image URL.
Here it never does — the browser never talks to Logo.dev at all — so it stays
server-side as a matter of architecture rather than of secrecy.

### Attribution

Logo.dev requires a visible credit ("Logos provided by Logo.dev") only for
**commercial** use of the free tier; a personal, self-hosted instance owned by an
individual and earning nothing does not need one. Nothing in this app renders an
attribution link, and a login-gated self-hosted app has no public page to put one
on in the first place.

If you deploy Ledgermancy commercially and enable this on the free tier, that
obligation is yours rather than the project's: add the credit somewhere public,
or move to a paid plan, which removes the requirement. Check Logo.dev's
[attribution terms](https://www.logo.dev/docs/platform/attribution) rather than
relying on this paragraph.

## Document vault

Receipts, tax returns, warranties, policies and contracts, encrypted with
`ENCRYPTION_KEY` and stored next to the transactions they belong to. **On by
default** — unlike the two gated features below it makes no outbound request
and needs nothing but a writable directory, which the compose file provides.

| Variable | Default | Notes |
| --- | --- | --- |
| `DOCUMENTS_ENABLED` | `true` | Set `false` to remove the vault entirely. Its endpoints then report 503 and the rest of the app is unaffected |
| `DOCUMENTS_BACKEND` | `local` | `local` (a mounted volume) or `s3` |
| `DOCUMENTS_LOCAL_ROOT` | `/var/lib/ledgermancy/documents` | Where ciphertext is written, for the local backend |
| `DOCUMENTS_MAX_FILE_BYTES` | `25MB` | Per-file cap. Plain bytes or a `KB`/`MB`/`GB` suffix (binary multiples) |
| `DOCUMENTS_QUOTA_BYTES` | `2GB` | Per-household total. `0` for no limit |
| `DOCUMENTS_OCR_ENABLED` | `false` | Allow receipt field extraction via your AI provider |

If the storage backend cannot be opened at startup — an unwritable directory,
a bucket that refuses the credentials — the api logs an error and runs with the
vault disabled rather than refusing to boot. The ledger is not held hostage to
a misconfigured volume.

### Two limits that are not tuning knobs

`DOCUMENTS_MAX_FILE_BYTES` exists because encryption here is whole-buffer: a
file becomes its own size in memory for the duration of a request, twice over
while plaintext and ciphertext coexist. The cap is what turns "too big" into a
clean 413 instead of an out-of-memory crash under a few concurrent uploads.
Values above 100 MB are **rejected at startup** for that reason; a genuine need
for larger files wants streaming encryption, which is a redesign rather than a
bigger number.

`DOCUMENTS_QUOTA_BYTES` is counted across the whole household including private
uploads, because the thing being rationed is your disk, not anyone's allowance.

If you raise the file cap, **also raise `client_max_body_size` in
`frontend/nginx.conf`** — it defaults to 30m, deliberately a little above the
25 MiB file cap so multipart framing has room. Leave it behind and uploads fail
at the proxy with an HTML error the UI cannot explain, instead of the api's own
413 that names the limit.

### The volume is not in `pg_dump`

Document *metadata* is in the database; the bytes are not. A backup that only
dumps Postgres restores every title, type and expiry date and no contents.

The scheduled backup handles this — it archives the vault alongside each dump,
and the weekly restore test opens a document end to end to prove the dump, the
archive and `ENCRYPTION_KEY` all agree. See
[Continuity & backups](features/continuity.md). Keep `ENCRYPTION_KEY` somewhere
other than the server: a restore needs all three, and that one is in no backup
the app takes.

### S3-compatible storage

Only read when `DOCUMENTS_BACKEND=s3`. Works with AWS S3, MinIO, Garage and R2;
requests are signed with SigV4 directly rather than through an SDK.

| Variable | Default | Notes |
| --- | --- | --- |
| `DOCUMENTS_S3_ENDPOINT` | — | e.g. `https://s3.us-east-1.amazonaws.com`, or your MinIO host |
| `DOCUMENTS_S3_REGION` | `us-east-1` | Part of the signature; must match the endpoint |
| `DOCUMENTS_S3_BUCKET` | — | Required |
| `DOCUMENTS_S3_ACCESS_KEY` | — | Required |
| `DOCUMENTS_S3_SECRET_KEY` | — | Required |
| `DOCUMENTS_S3_PREFIX` | — | Optional key prefix, so one bucket can hold more than this app |
| `DOCUMENTS_S3_PATH_STYLE` | `true` | `{endpoint}/{bucket}/{key}` addressing. Correct for every self-hosted S3-compatible server |

A half-configured S3 backend **fails startup validation** rather than accepting
an upload it cannot store. Documents are encrypted before they are uploaded, so
the bucket never holds readable content — but it does hold your data, which
makes this an outbound destination you are choosing deliberately.

## Receipt OCR

Off by default, and gated separately from `AI_API_KEY` on purpose. This is the
only place in the app that would send an *image of your paperwork* to a third
party, and having configured a model for transaction categorisation is not the
same as agreeing to that.

With it on, an image document **typed `receipt`** gains an "Extract fields"
action that asks your provider to read the merchant, total and date off it.

**Nothing else is eligible.** Tax documents, insurance policies, contracts,
statements, warranties and anything filed as `other` are refused with a 403 in
the API — before the file is decrypted, not just by hiding the button. A W-2
scanned to a PNG cannot be sent. That is an allowlist of one entry rather than a
blocklist of the obvious offenders, so a document type added later is
ineligible by default instead of sendable until someone notices.

What comes back is a **suggestion**: the fields are displayed, along with the
existing transactions whose amount and date could match, and the one action
offered is attaching the document to whichever of those you pick. Nothing is
written from a model's reading of a receipt — not a transaction, not a category,
not an amount. A misread total should cost you one correction, not one wrong row
in your ledger.

Extraction is only ever triggered by you, on one named document. There is no
background job, so nothing is sent because a sweep ran.

| Variable | Default | Notes |
| --- | --- | --- |
| `DOCUMENTS_OCR_ENABLED` | `false` | Set `true` to offer extraction. Also needs `AI_API_KEY` |

## Retirement sequence-of-returns test

Off by default, and for a different reason than the benchmark fetch above: this
one makes **no outbound request at all**. The reason it is gated is that a
"success rate" is the most quotable number a retirement tool can print, and
Ledgermancy bundles no market history to compute one from.

What it actually does, if you turn it on, is draw return sequences around *your
own* stated real return and a volatility you set, then report how many of them
survived the withdrawal phase. That is a genuinely useful thing to look at — it
is what exposes sequence-of-returns risk, the reason two portfolios with the
same average return can end very differently — but it is **not** a historical
backtest, and the page says so in the panel rather than in a footnote. Seeds are
derived from the inputs, so the same scenario always produces the same number;
a figure that moves when you reload it is not one to plan around.

| Variable | Default | Notes |
| --- | --- | --- |
| `RETIREMENT_MONTE_CARLO_ENABLED` | `false` | Set `true` to show the survival-rate panel on **Retirement** |

Everything else on the Retirement page — the projection, FI age, supported
spending, contribution headroom, the required-savings-rate solve — works with
this off. It adds a panel; it is not load-bearing.

## Backups and continuity

**On by default** — the only optional subsystem here that is. Everything else on
this page defaults off because the safe answer is "don't"; here the safe answer
is "do". An operator who has to opt in to backups is an operator who finds out
they never did on the day the disk fails.

Once a day the worker dumps the database, archives the document vault, and
writes a portable JSON export. Once a week it restores the latest dump into a
temporary database and verifies it — row counts table by table, schema version,
and one document opened end to end — then drops it. Your live database is never
touched, and you are never expected to restore your production instance on a
schedule. Status is at **Settings → Continuity** (owner only). See
[Continuity & backups](features/continuity.md).

| Variable | Default | Notes |
| --- | --- | --- |
| `BACKUP_ENABLED` | `true` | Turning this off is a deliberate choice to have no automated recovery path |
| `BACKUP_DIR` | `/var/lib/ledgermancy/backups` | The `backup-data` volume in the Compose deploy |
| `BACKUP_MIRROR_DIR` | — | A second destination. **Set this** |
| `BACKUP_INTERVAL` | `24h` | Dump, archive and export |
| `BACKUP_RESTORE_TEST_INTERVAL` | `168h` | The restore test; much less frequent because it rebuilds the whole database |
| `BACKUP_KEEP_DAILY` | `7` | |
| `BACKUP_KEEP_WEEKLY` | `4` | |
| `BACKUP_KEEP_MONTHLY` | `6` | |
| `BACKUP_INCLUDE_DOCUMENTS` | `true` | Ignored when `DOCUMENTS_ENABLED=false` |

`BACKUP_MIRROR_DIR` is the one worth acting on. By default backups sit on a
volume on the same machine as the database they protect, so a dead disk takes
both. Bind mount a NAS share or external disk into the worker (there is a
commented example in `docker-compose.yml`) and point this at it.

**Treat that directory as being exactly as sensitive as the database, because it
contains it.** Plaid tokens and document contents inside the dump stay encrypted
under `ENCRYPTION_KEY`, but every transaction, balance and merchant is in the
clear. There is deliberately no separate backup-encryption key: it would be a
second thing to lose, in the one subsystem whose entire purpose is reducing the
number of things whose loss is unrecoverable.

Invalid settings fail at startup rather than degrading — a backup subsystem that
logs a warning and reports green is worse than one that is plainly off.
