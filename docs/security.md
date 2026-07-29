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

## Registration

**Invite-only after the first account.** The first account creates the
household; everyone after joins via a one-time invite that is bound to the
address it was issued for (so an intercepted link can't be redeemed under a
different email). This keeps the app a private household ledger rather than an
open sign-up form on the public internet. See [Households](features/households.md).

## Operational hygiene

- `.env` is gitignored. **Do not commit real Plaid credentials or secrets.**
  Make it non-world-readable (`chmod 600 .env`) — it holds the database password
  and both encryption keys.
- The app **sends no email** and phones home to nothing but Plaid and
  (optionally) your AI provider. One opt-in exception: setting
  `BENCHMARK_PRICES_ENABLED=true` lets a daily job fetch end-of-day index closes
  from Stooq for the Investments benchmark chart. It is off by default, sends
  only a ticker symbol, and carries no account data.
- **Back up the database** — it's the only record of net-worth history. See
  [Deployment](deployment.md#back-up-the-database).
