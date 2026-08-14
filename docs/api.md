# API reference

The backend is a Go HTTP server (chi router). Every state-changing request from a
browser needs the **CSRF token** echoed in an `X-CSRF-Token` header.

## Two ways to authenticate

| | Session cookie | Personal API token |
| --- | --- | --- |
| Used by | the web app | third-party clients, scripts, agents |
| Sent as | `ledgermancy_session` cookie | `Authorization: Bearer lgm_…` |
| CSRF header | required | **not** required |
| Expires | 30 days, or 7 days idle | never, unless you set an expiry |
| Revoked from | Settings → Security → Signed-in devices | Settings → Security → API tokens |

Both resolve to the **same identity**, so a token sees exactly what its owner
sees. Every visibility rule is unchanged by which credential arrived.

A request carrying an `Authorization` header is resolved *only* as a token and
never falls back to the cookie — that is what makes the CSRF exemption safe.

### Personal API tokens

Create one under **Settings → Security → API tokens**. The value is shown once;
only an HMAC of it is stored, so it cannot be recovered afterwards.

```bash
curl -H "Authorization: Bearer lgm_xxxxxxxx" \
     https://ledgermancy.example.com/api/accounts/
```

Two scopes:

- **`read`** — every token has it. Safe methods only.
- **`write`** — additionally permits `POST`/`PUT`/`PATCH`/`DELETE`. Without it a
  state-changing request is refused with **403** before it reaches a handler.

Tokens cannot manage credentials: `/api/auth/tokens`, `/api/auth/password`,
`/api/auth/mfa/*` and session revocation all require the session cookie, so a
leaked token cannot mint replacements for itself or lock you out of your own
account. Rate limits apply exactly as they do to a browser.

## Authentication

| Method | Path | Auth | Notes |
| ------ | ---- | ---- | ----- |
| GET | `/healthz` | – | Process + database health |
| GET | `/api/auth/csrf` | – | **Call first.** Issues the CSRF cookie/token |
| POST | `/api/auth/register` | – | First user creates the household; the rest need an invite |
| POST | `/api/auth/login` | – | Rotates the CSRF token on success |
| POST | `/api/auth/logout` | – | Deletes the session server-side |
| GET | `/api/auth/me` | ✓ | Current user |
| POST | `/api/auth/mfa/verify` | – | Second login step; consumes the challenge cookie |
| GET | `/api/auth/mfa` | ✓ | Two-factor status and recovery codes left |
| POST | `/api/auth/mfa/setup` | ✓ | Password required. Returns QR + base32 secret |
| POST | `/api/auth/mfa/activate` | ✓ | Confirms a code; returns recovery codes **once** |
| POST | `/api/auth/mfa/disable` | ✓ | Requires password **and** a current code |
| POST | `/api/auth/mfa/recovery-codes` | ✓ | Regenerates the set, invalidating the old one |
| POST | `/api/auth/password` | ✓ | Change password; signs out every other device |
| GET | `/api/auth/sessions` | ✓ | Active sessions with device and address |
| DELETE | `/api/auth/sessions/{id}` | ✓ | Revoke one device |
| POST | `/api/auth/sessions/revoke-others` | ✓ | Sign out everywhere but here |
| GET | `/api/auth/events` | ✓ | Last 50 security events on the account |
| GET | `/api/auth/tokens` | cookie | Personal API tokens, with scopes and last-used |
| POST | `/api/auth/tokens` | cookie | `{name, scopes, expires_at?}`. Returns the token **once** |
| DELETE | `/api/auth/tokens/{id}` | cookie | Revoke; effective on the next request |

"cookie" in the Auth column means the session cookie specifically — an API token
is refused on those routes. Everything else marked ✓ accepts either.

## Household

| Method | Path | Auth | Notes |
| ------ | ---- | ---- | ----- |
| GET | `/api/household/` | ✓ | Current household |
| GET | `/api/household/members` | ✓ | Household members |
| POST | `/api/household/invites` | ✓ | Returns the invite token **once** |
| GET | `/api/household/invites` | ✓ | Pending invites |
| DELETE | `/api/household/invites/{id}` | ✓ | Revoke an invite |

## Plaid

| Method | Path | Auth | Notes |
| ------ | ---- | ---- | ----- |
| POST | `/api/plaid/link-token` | ✓ | Token for opening Plaid Link |
| POST | `/api/plaid/exchange` | ✓ | Completes linking; starts the backfill |
| GET | `/api/plaid/items` | ✓ | Linked institutions and their sync state |
| POST | `/api/plaid/items/{id}/sync` | ✓ | Refresh now (routine syncs run in the worker) |
| PATCH | `/api/plaid/items/{id}/sharing` | ✓ | Share an institution with the household |
| DELETE | `/api/plaid/items/{id}` | ✓ | Unlink (cascades to accounts + transactions) |

## Data

| Method | Path | Auth | Notes |
| ------ | ---- | ---- | ----- |
| GET | `/api/accounts` | ✓ | Visible accounts with balances |
| GET | `/api/transactions` | ✓ | `from`, `to`, `limit`, `offset`; defaults to a rolling year. `q` is a [composable search query](features/transactions.md#search) |
| GET | `/api/transactions/search-operators` | ✓ | The operator vocabulary `q` accepts, for autocomplete |
| GET | `/api/export/transactions.csv` | ✓ | Financial Summary transactions export |
| GET | `/api/export/categories.csv` | ✓ | Category summary export |
| GET | `/api/export/net-worth.csv` | ✓ | Net-worth history export |
| GET | `/api/export/holdings.csv` | ✓ | Investment holdings export |

Plus reporting endpoints consumed by the frontend (summary, by-category,
by-day, trend, averages, merchants, recurring, net-worth + history + projection,
holdings, liabilities, budgets, goals, insights, alerts, advisor chat, and
preferences/capabilities).

### Inflation-adjusted figures

| Method | Path | Auth | Notes |
| ------ | ---- | ---- | ----- |
| GET | `/api/inflation` | ✓ | The CPI-U deflator: `base_period`/`base_label` (real figures are in THOSE dollars), the covered span, `stale`, `gaps`, YTD inflation, and the household's own year set against it. `available: false` when the series is empty |

Three endpoints take **`real=1`**: `/api/networth/history`,
`/api/reports/trend` and `/api/investments/performance`.

The parameter **adds fields and changes nothing else**. Without it each returns
exactly what it always has, byte for byte — every added field is omitted rather
than nulled.

An added field is **absent when the figure cannot be deflated**: its month has
no published index, either because it predates the series or because BLS never
published it (October 2025). Absent never means "same as nominal" — render a gap
and say why. The base month and the gap list both come from `/api/inflation`.

`/api/investments/performance?real=1` deflates **returns only** — `real.twr`,
`real.annualised`, `real.mwr`. The dollar figures beside them stay nominal,
because deflating a period's cash flows correctly needs each one converted on its
own date. `real.mwr` is deflated by `real.annual_inflation` rather than
`real.inflation`, since MWR arrives already annualised.

`/api/projections/assumptions` gained `measured_inflation` — what CPI-U actually
did over the trailing decade, compounded. It is informational: the projection
still uses the household's own `inflation_rate`.

### Merchant logos

| Method | Path | Auth | Notes |
| ------ | ---- | ---- | ----- |
| GET | `/api/merchants/logo` | ✓ | `key` = the **resolved** merchant key. Returns the cached image bytes, with the content type **sniffed from the bytes** rather than read from the stored column, plus `nosniff` and `Cache-Control: private`. Adult-only |

`404` is the ordinary answer, and the frontend is built around it: the operator
never enabled the feature, the household opted out in **Settings → Appearance**,
the merchant never resolved to a domain, or the domain had no logo — all four
return the same response and the avatar falls back to its monogram. Nothing here
reveals which.

This endpoint is **not a proxy**. It never contacts Logo.dev: a logo is fetched
by the worker, once per merchant, and cached, so a page render never depends on
a third party being reachable. See
[Security](security.md#merchant-logos-add-a-host-but-never-to-the-browser).

### Digests

The stored digest history. Unlike every other read on this page these are scoped
to the requesting **user**, not the household: a digest entry records one
member's view of the money, computed under their own visibility, so another
member reading it would be handed figures from institutions deliberately not
shared with them.

`payload` is returned as the exact JSON that was frozen when the digest was
generated. It is never recomputed, so a digest that disagrees with today's
reporting endpoints is correct rather than stale — see
[Digest](features/digest.md).

| Method | Path | Auth | Notes |
| ------ | ---- | ---- | ----- |
| GET | `/api/digests/` | ✓ | `limit` (≤50, default 12), `offset`. Returns the page plus `total` and `unread` |
| GET | `/api/digests/{id}` | ✓ | One entry. 404 for an id belonging to anyone else |
| POST | `/api/digests/{id}/read` | ✓ | Stamps `read_at`, once. 204 either way |
| POST | `/api/digest/test` | ✓ | Queues a digest for the current period now, ignoring cadence and dedupe. Needs no notification channel |

### Investments

| Method | Path | Auth | Notes |
| ------ | ---- | ---- | ----- |
| GET | `/api/investments/` | ✓ | Accounts, total value, unrealised gain with its coverage, days of recorded history |
| GET | `/api/investments/performance` | ✓ | `period` = `ytd`\|`1y`\|`3y`\|`5y`\|`inception`. Returns are **fractions** (`0.0734` = 7.34%); `computable: false` with a `caveat` when history is too thin, and a null `mwr` carries an `mwr_note` |
| GET | `/api/investments/benchmarks` | ✓ | Growth rebased to 100, deposits removed. `enabled: false` when benchmark fetching is off |
| GET | `/api/investments/allocation` | ✓ | By asset class and by tax treatment; unclassified value is its own slice, never redistributed |
| GET | `/api/investments/holdings` | ✓ | Per-position detail with gain in dollars and percent |
| GET | `/api/investments/fees` | ✓ | Fund expense drag, always with its coverage disclosed |
| GET | `/api/investments/dividends` | ✓ | Dividends by month, from investment transactions |
| PATCH | `/api/investments/accounts/{id}/tax-treatment` | ✓ | Confirms a classification. `null` clears it back to untagged |

### Manual accounts

Accounts Plaid cannot link (TreasuryDirect, a Voya plan, a private holding) are
first-class accounts created here, with full Investments-page parity. The
organising rule: **a manual endpoint never touches a Plaid row** — every mutation
resolves through a `source='manual'` query, so a linked account's id gets a 404
rather than an edit the next sync would silently revert. Manual balances are the
only user-owned balance-write path; every change is paired with an
`account_balance_history` row in the same transaction. See
[Accounts → Accounts without Plaid](features/accounts.md#accounts-without-plaid).

| Method | Path | Auth | Notes |
| ------ | ---- | ---- | ----- |
| POST | `/api/accounts` | ✓ | Create a manual account. `type` ∈ depository\|investment\|brokerage\|credit\|loan\|other; balance is a decimal string |
| PUT | `/api/accounts/{id}` | ✓ | Manual only — a `source='plaid'` id 404s |
| DELETE | `/api/accounts/{id}` | ✓ | Manual only; cascades its transactions |
| PUT | `/api/accounts/{id}/balance` | ✓ | `{as_of, balance, reason, note}`. Re-running the same `as_of` updates in place |
| GET | `/api/accounts/{id}/balance-history` | ✓ | The append-only balance trail |
| POST | `/api/accounts/{id}/holdings` | ✓ | Upsert a manual holding (keys on account + security) |
| GET | `/api/accounts/{id}/investment-transactions` | ✓ | The account's manual investment transactions |
| GET | `/api/securities` | ✓ | Reference data — a security states what a ticker is, not who holds it. For the picker |
| POST | `/api/securities` | ✓ | Create a manual security keyed on lowercased `ticker_key` |
| POST | `/api/investment-transactions` | ✓ | `type`/`subtype` from the same vocabulary Plaid uses; a contribution is an external flow that moves TWR/MWR |
| DELETE | `/api/investment-transactions/{id}` | ✓ | Manual only |
| DELETE | `/api/holdings/{id}` | ✓ | Manual positions only; a Plaid holding deleted here would reappear on the next sync |

The existing `GET /api/accounts` (above) and the `/api/investments/*` reads need
no change — they call the same queries, which now return manual rows too.

### Manual assets, valuations & bonds

The revaluation surface for [manual assets](features/net-worth.md#manual-assets).
The defining rule: **only `POST /valuations` writes a value**. The suggestion
endpoint computes a proposal and returns it — net worth never moves on an
estimate the user has not accepted. Bonds are the exception, because a savings
bond's value is arithmetic over published rates rather than a judgement.

| Method | Path | Auth | Notes |
| ------ | ---- | ---- | ----- |
| GET | `/api/manual-assets/{id}/detail` | ✓ | Class-specific metadata (real estate / vehicle / bond fields) |
| PUT | `/api/manual-assets/{id}/detail` | ✓ | Upsert the detail row |
| GET | `/api/manual-assets/{id}/valuations` | ✓ | The append-only value history |
| POST | `/api/manual-assets/{id}/valuations` | ✓ | The one write that moves a value, paired with the authoritative current column atomically |
| GET | `/api/manual-assets/{id}/suggestion` | ✓ | A depreciation proposal — never a write. `ok=false` with a `reason` when the curve has nothing to say |
| GET | `/api/manual-assets/{id}/bond` | ✓ | A savings bond's redemption value, computed against `savings_bond_rates` |
| PUT | `/api/manual-assets/{id}/loan` | ✓ | Link an asset to a loan account for equity, without double-counting |
| GET | `/api/savings-bond-rates` | ✓ | The published rate table, seeded. Each row names its source |
| PUT | `/api/savings-bond-rates` | ✓ | Correct a row against treasurydirect.gov — a bundled table is only defensible if it is checkable |

### Documents

The encrypted vault. Every route is scoped to the caller's household **and**
user — including the download, so a document id alone is never sufficient to
fetch a blob. Misses return **404 rather than 403**, so a response cannot be
used to probe whether an id exists in another household. All routes report
`503` when no document storage is configured.

| Method | Path | Auth | Notes |
| ------ | ---- | ---- | ----- |
| GET | `/api/documents/` | ✓ | Filters: `doc_type`, `search`, `from`, `to`, `expiring_before`, `linked` (tri-state — omit for all) |
| POST | `/api/documents/` | ✓ | `multipart/form-data`: `file`, plus `title`, `doc_type`, `document_date`, `expires_at`, `notes`, `is_shared`, and optional `link_kind`/`link_id`. `413` over the per-file cap or the household quota |
| GET | `/api/documents/storage` | ✓ | Bytes used, quota, per-file cap, backend, whether OCR is available |
| GET | `/api/documents/attached` | ✓ | Exactly one of `transaction_id`, `manual_asset_id`, `account_id`, `goal_id` |
| GET | `/api/documents/counts` | ✓ | Repeated `transaction_id` params → `{id: count}`, for paperclip badges |
| GET | `/api/documents/{id}` | ✓ | Metadata plus links |
| PUT | `/api/documents/{id}` | ✓ | Metadata only. `retain_until` is recomputed from the type, never accepted from a client |
| DELETE | `/api/documents/{id}` | ✓ | Removes the row, then the blob |
| GET | `/api/documents/{id}/download` | ✓ | Decrypted bytes. Content type is **sniffed** against a small allowlist, never echoed from the upload; always `Content-Disposition: attachment` + `nosniff`. `422` when decryption or the integrity check fails, `410` when the blob is missing from storage |
| POST | `/api/documents/{id}/links` | ✓ | `target_kind` ∈ `transaction`\|`manual_asset`\|`account`\|`goal`. A target outside the household is refused without saying why |
| DELETE | `/api/documents/{id}/links/{linkId}` | ✓ | Detach |
| POST | `/api/documents/{id}/extract` | ✓ | Receipt OCR. **Suggestions only** — returns fields plus candidate transactions and writes no ledger data. `403` unless `DOCUMENTS_OCR_ENABLED`, **and `403` for any `doc_type` other than `receipt`**, checked before the file is decrypted. `415` for a non-image. The reading is cached on the document, so call this once per receipt |
| GET | `/api/documents/{id}/matches` | ✓ | Re-runs the transaction match against the cached reading. **No decryption, no upload, no model call** — this is what finds the charge for a receipt scanned before it posted. Empty list when the receipt has not been read |

### Payroll

The pre-tax side of the ledger. Adult-only like every other financial surface,
but the group guard is **not** the whole access story here: a paystub is private
to the person whose pay it is, and the `(owner OR shared)` predicate is enforced
per row in SQL. Reads use it; every mutation uses a stricter owner-only lookup,
because seeing a shared stub is not permission to change it. Misses are **404,
never 403** — the distinction would confirm that another member has a stub.

Two invariants every consumer inherits:

- **Unconfirmed paystubs are inert.** They appear in the listing — that is the
  review queue — and contribute nothing to `/summary`, `/savings-rate` or
  `/tax-summary`. The filter lives in SQL, once.
- **Confirmation requires the stub to balance**, `gross − Σ(deductions) = net`
  within a cent. `422` otherwise, with the gap named in the message.

| Method | Path | Auth | Notes |
| ------ | ---- | ---- | ----- |
| GET | `/api/payroll/taxonomy` | ✓ | Line categories and pay frequencies. Served rather than duplicated client-side — the wage-base rules are a server fact |
| GET | `/api/payroll/years` | ✓ | Tax years with confirmed stubs, newest first |
| GET | `/api/payroll/summary` | ✓ | `year`, `family_hsa`. Year roll-up: gross, net, effective tax rate, total comp, per-category totals, contribution headroom. `limits_configured: false` when the app has no IRS limits for that year — it never substitutes an adjacent one |
| GET | `/api/payroll/savings-rate` | ✓ | `from`, `to`. Both rates side by side: the existing net-based figure **unchanged**, the gross-based one beside it, plus a coverage warning when the stubs on file only partly cover the window |
| GET | `/api/payroll/tax-summary` | ✓ | `year`. Confirmed stubs mapped onto W-2 boxes, per employer. The only endpoint that returns a full EIN. Carries its own disclaimer **in the payload** — it is not a W-2 and will be printed away from the page that framed it |
| POST | `/api/payroll/parse` | ✓ | `multipart/form-data`: `file`. Local PDF text-layer extraction — **no network call, no AI provider, nothing stored**. Returns a proposal and writes nothing. `422` for a scan with no text layer, `415` for a non-PDF |
| POST | `/api/payroll/parse-document` | ✓ | The same local parse over a vault document's decrypted bytes. Deliberately **not** gated on `DOCUMENTS_OCR_ENABLED`: that switch governs what may be uploaded to a third party, and nothing here is |
| GET | `/api/payroll/employers` | ✓ | Paystub counts are visibility-scoped; the EIN is masked |
| POST | `/api/payroll/employers` | ✓ | `409` on a duplicate name — two rows for one employer would break the shared-limit pooling |
| PUT | `/api/payroll/employers/{id}` | ✓ | `ein` is three-valued: omit to keep, `""` to clear, digits to replace |
| DELETE | `/api/payroll/employers/{id}` | ✓ | `409` while paystubs exist. The FK cascades, which is exactly why |
| GET | `/api/payroll/paystubs` | ✓ | `year`. Includes unconfirmed stubs — the review queue |
| POST | `/api/payroll/paystubs` | ✓ | `confirm: true` requires a balancing stub (`422`). `409` on a duplicate employer + pay date |
| GET | `/api/payroll/paystubs/{id}` | ✓ | Lines plus every derived figure |
| PUT | `/api/payroll/paystubs/{id}` | ✓ | Lines are replaced wholesale; an edit re-opens the review |
| DELETE | `/api/payroll/paystubs/{id}` | ✓ | Owner only |
| POST | `/api/payroll/paystubs/{id}/confirm` | ✓ | `{confirmed}`. `422` naming the gap when the stub does not reconcile |
| PATCH | `/api/payroll/paystubs/{id}/sharing` | ✓ | Private by default — the one place this app inverts its sharing default |
| GET | `/api/payroll/paystubs/{id}/deposit-matches` | ✓ | Candidate deposits ranked by distance from net. A **proposal**; a deposit already claimed by another stub is not offered at all |
| PUT | `/api/payroll/paystubs/{id}/deposit` | ✓ | `{transaction_id}`, null to unlink. The only thing that ever writes the link |

## CSRF

Every state-changing request needs the CSRF token echoed in an `X-CSRF-Token`
header. The token is issued by `GET /api/auth/csrf` (which also sets the cookie)
and rotated on a successful login.

## Webhooks

| Method | Path | Auth | Notes |
| ------ | ---- | ---- | ----- |
| POST | `/webhooks/plaid` | – | Plaid push notifications |

The webhook is deliberately **outside** authentication and CSRF — Plaid is not a
browser and carries no session. That is safe because the payload is treated
purely as a hint: the only action it can trigger is "re-sync this item", and the
sync re-reads everything from Plaid using our own stored access token. **A forged
webhook can cause a wasted sync, never a data change.** See
[Deployment → Webhooks](deployment.md#webhooks).

## Visibility scoping

Every authenticated query — including [Advisor](features/advisor.md) tools —
inherits the same shape: `WHERE u.household_id = $1 AND (i.user_id = $2 OR
i.is_shared)`. Your own items ∪ household items where shared. A query that
forgot this would leak a spouse's private account, so the scoping is enforced in
the data layer, not left to each caller.
