# Security

Ledgermancy holds your full financial life, so the security model is deliberate
and layered. This page is the full picture; the quick version is also in the
[Deployment](deployment.md) guide.

---

## Two-factor authentication (TOTP)

Optional but strongly recommended. Standard authenticator apps (Google
Authenticator, 1Password, Aegis, Bitwarden) — scan a QR, enter 6 digits.

- The half-authenticated state between the password and the code lives in its
  own `mfa_challenges` table rather than as a flag on `sessions`. A row in
  `sessions` continues to mean exactly one thing — *fully authenticated* — so a
  pending challenge cannot satisfy the auth middleware however it changes.
- **TOTP secrets are encrypted at rest** with the same AES-GCM key as Plaid
  tokens.
- Each accepted code's **time-step is recorded**, so a code cannot be replayed
  inside the 90-second window it stays valid for.
- Enabling two-factor, changing a password, or disabling two-factor all
  **require the password again** — holding a session is not authority to change
  the factors guarding the account. Disabling additionally requires a current
  code.
- **Recovery codes** are HMAC-hashed like session tokens (they are high-entropy
  randoms, so argon2 would buy nothing and cost ten 64 MiB verifications per
  attempt). They're shown exactly once.

### Recovery

- Lose your phone? Sign in with a recovery code; each works once. Regenerate
  the set from the Security page.
- Lost phone **and** recovery codes? There is no email recovery — the app sends
  no mail. Clear the second factor at the database; see
  [Deployment](deployment.md#if-you-lose-your-phone). Shell access to the server
  is equivalent to account access, so guard it.

## Sessions

- **Server-side**, in `httpOnly` + `SameSite=Strict` cookies — not localStorage.
- Expire after **30 days**, or **7 days idle**.
- Enabling 2FA signs out every other device; **changing a password** signs out
  every other device by design.
- The **Security** page lists every active session with its browser, address,
  and last-used time, with per-device revoke and a "sign out everywhere" action.
- **Recent activity**: the last 50 sign-ins, failures, and security changes.

## Personal API tokens

A user can mint a bearer token from **Settings → Security** so a third-party
client reaches the API without a session cookie. See
[API → Two ways to authenticate](api.md#two-ways-to-authenticate) for how to use
one; what makes it safe:

- **Shown once, hashed at rest.** Stored as HMAC-SHA256 keyed with
  `SESSION_SECRET`, exactly like a session token, so a leaked database alone
  cannot be used to forge or recognise a live token. It is never logged.
- **Same identity, same visibility.** A token resolves to precisely what its
  owner sees — no more, and not a different scoping path that could drift.
- **Read-only by default.** `write` has to be asked for, and its absence is
  enforced in the authentication middleware rather than per handler, so a route
  written later inherits the refusal.
- **CSRF-exempt, safely.** A bearer request is not browser-initiated, so it is
  not held to the double-submit check. The exemption cannot be turned into a
  bypass because a request carrying an `Authorization` header is *never*
  resolved from the session cookie — the worst a forged cross-site request with
  a junk bearer header achieves is a 401.
- **Not a credential factory.** Token creation and revocation, password changes,
  MFA enrolment and session revocation all require the session cookie. A leaked
  token cannot mint replacements for itself or lock the owner out.
- **Revocation is immediate** (the row is deleted; authentication reads the table
  on every request), and creation/revocation are recorded in **Recent activity**.
- **Rate limits are unchanged** — a token-authenticated request is subject to the
  same per-address and per-user budgets as a browser's.

## Credentials

- **Plaid access tokens are encrypted at rest** (AES-GCM) and **never returned
  to the browser**.
- **Passwords are argon2id.** Login failures are indistinguishable between an
  unknown address and a wrong password, in both message and timing. Hashes made
  under weaker parameters are transparently upgraded on next sign-in.

## Rate limiting & lockout

- **Rate limiting** on sign-in, registration, and account changes, keyed on the
  real client address.
- **Durable per-account exponential backoff** that survives a restart.
- A locked account still returns the generic error, so lockout is not an oracle
  for which addresses exist.

## Proxy headers & client addresses

The API only believes `X-Forwarded-For`/`X-Forwarded-Proto` when
`TRUST_PROXY_HEADERS` is set. The production overlay sets it, because there the
bundled nginx strips client-supplied address headers and is the only route in.
See [Deployment → Client IP addresses](deployment.md#client-ip-addresses-and-your-outer-proxy).

## Security headers

Every response carries:

- **CSP** tuned for Plaid Link.
- **HSTS** behind TLS.
- `X-Content-Type-Options: nosniff`.
- `X-Frame-Options: DENY`.
- `Referrer-Policy: no-referrer`.
- `Cache-Control: no-store` so financial JSON and CSV exports never land in a
  cache.

## Document vault

Documents are sealed with AES-256-GCM under `ENCRYPTION_KEY` — the same key
that protects Plaid access tokens — before anything is written. The storage
backend, whether a mounted volume or an S3 bucket, never sees plaintext.

Four properties are worth stating explicitly, because each closes a specific
failure:

- **The path a document is stored at is a generated UUID, never its filename.**
  This removes path traversal as a class rather than filtering for it. A file
  called `../../etc/passwd` is stored at a UUID and downloads as `passwd`.
- **A document id is not an authorisation.** Every route including the download
  resolves the row scoped to the caller's household *and* user id. A document
  belonging to another household — or a private one belonging to another
  household member — returns **404**, not 403, so the response cannot be used to
  confirm that an id exists somewhere.
- **Downloads never echo the uploader's content type.** The type is sniffed
  from the decrypted bytes against a short allowlist (PNG, JPEG, GIF, WebP,
  PDF); everything else is served as `application/octet-stream`. Every response
  carries `Content-Disposition: attachment` with a sanitised filename and
  `X-Content-Type-Options: nosniff`. An HTML file filed as a "receipt"
  downloads; it does not execute on this origin. SVG is deliberately absent
  from the allowlist — it is a scriptable document format, not an image.
- **Reads verify a SHA-256 of the plaintext.** GCM already detects tampering;
  the hash catches the different failure of a storage-layer mixup serving the
  wrong intact blob, which under encryption would otherwise be undetectable.
  A mismatch fails closed with no partial output.

**Private documents** (`is_shared = false`) are invisible to the rest of the
household in every listing, count and download — the same model as per-
institution sharing.

**Nothing is ever auto-deleted.** Each document gets an advisory "keep until"
date from its type; it is surfaced in the UI and acted on by a person. A
finance app that silently discards a tax return has failed at its one job.

### Receipt OCR sends images off the host

`DOCUMENTS_OCR_ENABLED` is off by default and is a **separate switch from
`AI_API_KEY`**, because it is the only feature that uploads an image of your
paperwork to a third party.

**Only documents typed `receipt` are eligible.** Tax documents, insurance
policies, contracts, statements, warranties and anything filed as `other` are
refused with a 403 — server-side, before the file is even decrypted, not merely
by hiding the button. A W-2 scanned to a PNG is exactly as ineligible as a PDF
of one. That distinction is the point: a receipt is a merchant, a total and a
date that already exist in the transaction it belongs to, whereas a tax return
is a name, an address, an SSN and a complete financial picture. They are not the
same exposure and the app does not treat them as one because both happen to be
images.

The allowlist has exactly one entry and is checked with an allowlist rather than
a blocklist deliberately: a doc type added by some future migration is
ineligible by default rather than sendable until somebody remembers it. Refiling
a document as a receipt is the deliberate act that opts it in.

Beyond that, extraction is **only ever user-initiated** on one named document —
there is no background job, so nothing is sent because a sweep ran. PDFs are
refused, the type is decided by sniffing the bytes rather than trusting the
uploader's claim, and images above 5 MB are rejected.

The results are suggestions. The only action offered is attaching the document
to a transaction *you* pick from the candidates. No transaction, category or
amount is ever written from a model's reading of a receipt.

**One upload per receipt, ever.** What the model read is cached on the document,
so re-opening a receipt, and every later attempt to match it against a charge,
runs as local SQL. The image is only re-sent if you explicitly press "Read
again". This is a privacy property as much as a performance one: without the
cache, matching a receipt to a charge that posted three days later would mean
uploading it a second time.

## Merchant logos add a host, but never to the browser

`MERCHANT_LOGOS_ENABLED` is off by default and is a **separate switch from
`AI_API_KEY`**, for the same reason receipt OCR is: configuring a model to sort
your spending is not the same as agreeing to tell a logo company which shops you
use.

**Nothing changes in the browser's request list.** The obvious way to build this
feature is an `<img src="https://img.logo.dev/...">`, and that is precisely what
is not done here — it would put a third party in the page, hand it your IP and
`Referer` on every render, and let it count your visits. Instead the worker
fetches each logo once, stores the bytes in the database, and the api serves
them from this origin. The api is **not a proxy**: if the worker has not cached
a logo, the answer is a 404 and the app's own monogram is drawn. No page render
ever depends on a third party being reachable.

**What is sent, and to whom.** Your AI provider is asked which website a
merchant is, by name — a name it already sees during categorisation, so this
step adds no destination that categorisation had not already added. Logo.dev is
sent a bare domain. Neither is sent an amount, a balance, an account, a date or
a transaction. What Logo.dev can learn is the set of businesses this deployment
has merchants for, one domain at a time, and that is the honest cost of the
feature.

**Each merchant costs one request, ever.** A resolved logo is cached; a merchant
with no logo is cached as having none and is never asked about again. There is
no re-fetch schedule and no refresh — so switching the feature on produces a
burst of requests once and then effectively nothing.

**Households can refuse it.** Even with the operator switch on, a household can
turn the imagery off in **Settings → Appearance**. That stops the lookups on
their behalf and **deletes the logos already cached for them**: the cache is
derived data about where they shop, and keeping it past a "no" would be keeping
the part they objected to.

Bytes coming back are treated as untrusted input, because they end up rendered
on this app's origin. The response header is ignored and the content type is
sniffed from the bytes, SVG is refused outright (it is a script-bearing document
format wearing an image's clothes — the same call the document vault makes), and
anything over `MERCHANT_LOGOS_MAX_BYTES` is discarded as "no logo".

The domain the model returns is validated before it is used: it must be a bare
lowercase hostname, so a path, a port, credentials, an IP address or a scheme is
refused before any request is made. The model's input ultimately arrives from a
bank feed, and this is the boundary where that stops mattering.

## Paystubs raise the sensitivity ceiling of the database

Paystub tracking stores gross salary, an employer, and — optionally — an EIN.
That is a step up in sensitivity from anything else the app holds, and the
schema is built around three consequences.

**Paystubs are private by default, and this is the one place the app inverts its
sharing default.** Linked institutions (`plaid_items.is_shared`) and vault
documents (`documents.is_shared`) both default to *shared*, because a
household's accounts and paperwork are normally joint. A salary is not. In a
two-earner household, the other member learning what you make has to be a
decision somebody made, never the consequence of a column default — so
`paystubs.is_shared` defaults to **false** and every read is scoped to
`(owner OR shared)`. The adult-only route guard does nothing about this; it is
enforced per row, in SQL.

Seeing a shared stub is also not permission to change it. Confirming, editing,
deleting and linking a deposit all resolve the row through an owner-scoped
query, and a stub belonging to another member returns **404** rather than 403 —
the distinction would confirm that it exists.

**An EIN is sealed with `ENCRYPTION_KEY`,** the same key that protects Plaid
tokens and vault documents, so it is not readable from a database dump alone and
is excluded from the portable JSON export by type. It is returned in full by
exactly one endpoint — the annual tax summary, the only place it is needed —
and every other response shows `**-***6789`. The same consequence applies as for
the vault: losing the key loses this column, which is the right trade for a
field you can retype off a W-2 in ten seconds.

**Personal identifiers are stripped before storage, not before display.** Any
text taken off a stub — a line label, an unclassified row — is redacted of
anything matching an SSN or a masked account number on the way *in*. A database
that has never contained an SSN is a materially different thing to back up, to
export, and to lose.

### Paystub PDFs never leave the host

There is no AI path for paystubs, deliberately.

A PDF stub from a payroll provider is a **generated** document: the text is
already in the file, in a fixed layout, so reading it is a parsing problem
rather than a perception one. The importer pulls that text layer out locally
(`internal/payroll/pdftext.go`) with no network call and no model. A scanned or
photographed stub has no text layer, and the app says so and asks you to type it
in — which is the fallback that has to work anyway.

That is a stronger position than the receipt OCR above rather than a weaker one,
and it is not an accident of scope. A paystub is *more* sensitive than the tax
documents the OCR allowlist already refuses to send, so widening
`ocrEligibleTypes` to include one would have been a single line and exactly the
wrong line. Local extraction is also simply more accurate: transcribing a known
field out of a text layer cannot misread a digit, and a misread year-to-date
figure flowing into a tax summary is the expensive failure here.

Reading a stub already in the vault is the same local parse over decrypted
bytes, so it involves no third party either, and is not gated on
`DOCUMENTS_OCR_ENABLED` — that switch decides what may be *uploaded* somewhere,
which is a different question.

### Unconfirmed paystubs are inert

A paystub that has not been reviewed contributes to **no reported figure**: not
the savings rate, not the effective tax rate, not contribution totals, not the
tax summary. The filter lives in SQL, once, so no consumer can forget it.

Confirmation additionally requires the stub to reconcile — gross minus the
deductions must equal net, within a cent. A stub that does not balance can be
saved as a draft but never confirmed, because silently storing a mis-entry would
put its gap into every figure derived from it.

## Registration

**Invite-only after the first account.** The first account creates the
household; everyone after joins via a one-time invite that is bound to the
address it was issued for (so an intercepted link can't be redeemed under a
different email). This keeps the app a private household ledger rather than an
open sign-up form on the public internet. See [Households](features/households.md).

## Dependency scanning

Both dependency trees are scanned on every push and pull request, and again
every Monday — an advisory is published against code that did not change, so a
push-only trigger would never see the one that matters. The workflow is
[`.github/workflows/security.yml`](https://github.com/madeofpendletonwool/ledgermancy/blob/main/.github/workflows/security.yml),
kept separate from CI so that "the supply chain moved" is a distinguishable
signal from "the build broke".

**The Go side is gated with no exceptions.** `govulncheck ./...` fails the
build, and needs no allowlist because it is *reachability*-based: it reports a
vulnerability only when the compiled code actually calls into the affected
function. Vulnerabilities that merely exist somewhere in `go.sum` stay silent,
so a finding here means the binary is genuinely exposed and there is nothing to
argue about. Two such unreachable advisories are present today and correctly
produce nothing.

**The npm side is gated against a written allowlist**, which is
`frontend/audit-allowlist.json`, enforced by `npm run audit`. High and critical
advisories fail the build unless the file carries an entry naming the advisory,
explaining why it does not apply here, and giving a date by which the
explanation must be re-checked. Moderate and below are printed, not enforced.

The allowlist exists because the two obvious designs both fail. A bare
`npm audit --audit-level=high` is red on every run for an advisory somebody has
assessed, and a check that is always red is a check people learn to click past —
the next advisory arrives into a build that was already failing. Report-only is
worse: it is read by nobody. So the gate is loud about anything nobody has
assessed and silent about everything somebody has, which is the only combination
where a red build still carries information.

The file is kept from rotting into a list of forgotten excuses from both ends:
an entry past its review date fails, and so does an entry npm no longer reports.
The upgrade that finally retires an advisory is the moment to delete the line,
and CI insists on it rather than leaving a permanent exemption behind. The
allowlist is currently empty: every high and critical advisory `npm audit`
previously reported has either been fixed or stopped being reported, and nothing
is being excused.

## Operational hygiene

- `.env` is gitignored. **Do not commit real Plaid credentials or secrets.**
  Make it non-world-readable (`chmod 600 .env`) — it holds the database password
  and both encryption keys.
- The app **sends no email unless you configure SMTP**, and phones home to
  nothing but Plaid and (optionally) your AI provider. Five opt-in exceptions:
    - Setting `SMTP_HOST` enables the emailed
      [digest](features/digest.md). Off by default; the digest is the only thing
      the app ever mails, and only to members who tick the box themselves in
      **Settings → Digest**. SMTP configured with nobody opted in sends nothing.
      Both encrypted transports verify the server's certificate, and there is no
      bypass setting.
    - Setting `BENCHMARK_PRICES_ENABLED=true` lets a daily job fetch end-of-day
      index closes from Stooq for the Investments benchmark chart. It is off by
      default, sends only a ticker symbol, and carries no account data.
    - Setting `MERCHANT_LOGOS_ENABLED=true` lets a daily job fetch merchant
      logos from Logo.dev. Off by default; see below.
    - Setting `CPI_FETCH_ENABLED=true` lets a daily job pull the newest month of
      the CPI-U series from the BLS public API. Off by default, and the least
      consequential of the five: the series ships bundled from January 2010, so
      inflation-adjusted views work fully without it. The request names one
      public series and a year range — identical for every install on earth,
      with nothing about your household in it.
    - Setting `WEBHOOKS_ENABLED=true` lets members configure
      [outgoing webhooks](features/webhooks.md). Off by default, and the most
      consequential of the five: it is the only one where **you** choose the
      destination, and what it carries is your household's own events — an alert
      with an amount and a merchant, a newly-raised insight, a goal
      contribution. Each webhook is signed with its own secret, minted by the
      app and stored sealed with `ENCRYPTION_KEY`. Redirects are never followed,
      so a receiver cannot forward a signed payload onward. With the switch off
      no delivery worker exists at all, so an outbound webhook request cannot
      happen by accident.
- **Back up the database** — it's the only record of net-worth history. See
  [Deployment](deployment.md#back-up-the-database).
