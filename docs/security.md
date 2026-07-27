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
  (optionally) your AI provider.
- **Back up the database** — it's the only record of net-worth history. See
  [Deployment](deployment.md#back-up-the-database).
