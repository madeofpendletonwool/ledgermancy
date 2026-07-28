# API reference

The backend is a Go HTTP server (chi router). Every state-changing request needs
the **CSRF token** echoed in an `X-CSRF-Token` header.

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
| GET | `/api/transactions` | ✓ | `from`, `to`, `limit`, `offset`; defaults to a rolling year |
| GET | `/api/export/transactions.csv` | ✓ | Financial Summary transactions export |
| GET | `/api/export/categories.csv` | ✓ | Category summary export |
| GET | `/api/export/net-worth.csv` | ✓ | Net-worth history export |
| GET | `/api/export/holdings.csv` | ✓ | Investment holdings export |

Plus reporting endpoints consumed by the frontend (summary, by-category,
by-day, trend, averages, merchants, recurring, net-worth + history + projection,
holdings, liabilities, budgets, goals, insights, alerts, assistant chat, and
preferences/capabilities).

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

Every authenticated query — including [Assistant](features/assistant.md) tools —
inherits the same shape: `WHERE u.household_id = $1 AND (i.user_id = $2 OR
i.is_shared)`. Your own items ∪ household items where shared. A query that
forgot this would leak a spouse's private account, so the scoping is enforced in
the data layer, not left to each caller.
