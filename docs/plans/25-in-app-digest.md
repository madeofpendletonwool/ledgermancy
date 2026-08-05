# 25 — In-app weekly digest (+ optional SMTP)

*(TODO.md "Next major initiatives" #10.)*

**Shipped.** Migration `00049_digest_entries.sql` (**not** the `00038` reserved
below — see the first note). A `Digest` route backed by `GET /api/digests`,
`internal/mailer` for the optional SMTP, and `reporting.BuildDigestPayload` for
the figures. Doc 24 had not shipped, so the advisor section is absent as the doc
allows; doc 13 had, so upcoming bills are in.

Four things anyone touching this should know — three of them places the plan
below was wrong against the code.

**The reserved migration number could not be used.** This doc reserved `00038`,
but the plan docs have shipped out of order and the schema is already at
`00048`. `goose.Up` refuses to apply a migration below the current version
("found N missing migrations"), so a `00038` file would have failed **every
existing deployment at boot**, not merely looked untidy. It is `00049`.
Reserve-ahead numbering only holds while the docs land in order; when they do
not, take the next free number. This applies to every unshipped doc in this
directory.

**`UNIQUE (user_id, cadence, period_start)` would have capped a weekly household
at one digest a month.** `digestWindow` reports the **month-to-date** for a
weekly cadence, so all four of July's weekly digests share `period_start =
2026-07-01`. The uniqueness key is `(user_id, period_key)` instead — the same
`'2026-W29'` / `'2026-06'` vocabulary `digest_deliveries` already uses, and the
same vocabulary `digestDue` already returns. `period_start`/`period_end` are
still stored, for display.

**The sweep had to stop gating on the push opt-in, and that opened a hole worth
knowing about.** `ListDigestEnabledUsers` is replaced by
`ListDigestCandidateUsers`, which enumerates everyone and returns all three
switches. It also filters `role <> 'child'`: a child login can write its own
user-scoped preferences, so an unfiltered sweep would let a child turn a
household spending recap on for themselves, past every adult-only route guard.
The old query's `digest.enabled = true` join hid that; nothing else did.

**Entries are write-once, including on a forced send.** `InsertDigestEntry` is
`ON CONFLICT DO NOTHING`. A "send one now" for a period that is already stored
pushes again but does not rewrite the stored copy, so the immutability invariant
is unconditional rather than "unless somebody pressed the button". They are also
classified `InExport` for continuity, **not** `Derived` — the tempting wrong
answer. A digest looks like job output, but the job cannot reproduce it: the
transactions behind it have since moved, so re-running the sweep writes a
*different* digest. A lost entry is lost history.

## Context

The digest machinery already exists and is well-built:
`backend/internal/jobs/digest.go` has `DigestSweepWorker` (finds due households),
`DigestWorker` (builds and sends), `digestDue`/`digestWindow` for cadence, and
`buildDigestNotification` (line 295) to render for ntfy. `digest_deliveries`
(migration `00013`) records what went out.

The gap is the **surface**. Every one of those paths terminates in a push
notification, so the digest only exists if the user has configured ntfy. There is
no in-app "what happened with my money this week", which is the natural
Sunday-morning check-in and should not require a notification channel to see.

Worse, a push is ephemeral — dismiss it and the content is gone. The generated
narrative is thrown away after delivery.

**The work here is the surface and the persistence, not the generation.** Reuse
`DigestWorker`; do not build a parallel path.

## AI vs deterministic split

Unchanged from the existing digest. Every figure is computed in SQL/decimal and
handed to the model as finished strings; the model writes the narrative. This doc
adds no new AI.

## Prerequisites

- Existing digest (doc 10) — shipped.
- **[13-bill-calendar.md](13-bill-calendar.md)** — soft, for the upcoming-bills
  section. Build the section conditionally so this doc is not blocked.
- **[24-proactive-advisor.md](24-proactive-advisor.md)** — soft, for advisor
  entries. Same treatment.

Both degrade cleanly: without them, the digest is spending, transactions,
net worth, and insights, which is already worth shipping.

## Data model

**Reserved migration: `00038_digest_entries.sql`.**

```sql
-- A rendered digest, kept so it can be read in-app and browsed historically.
-- digest_deliveries (00013) records that something was SENT; this records what
-- it SAID. Keep both: one is a delivery log, one is content.
CREATE TABLE digest_entries (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id UUID NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    -- Which member's view this is: visibility scoping means two members of the
    -- same household can legitimately see different figures.
    user_id      UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    cadence      TEXT NOT NULL,            -- matches digestDue's vocabulary
    period_start DATE NOT NULL,
    period_end   DATE NOT NULL,
    label        TEXT NOT NULL,            -- the existing digestWindow label
    -- The computed figures, so a historical digest renders exactly as generated
    -- rather than being recomputed against changed data.
    payload      JSONB NOT NULL,
    -- The AI narrative, when one was generated. NULL when AI is disabled.
    narrative    TEXT,
    read_at      TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, cadence, period_start)
);
CREATE INDEX digest_entries_user_created_idx ON digest_entries (user_id, created_at DESC);
```

**Snapshot the figures into `payload`; do not recompute on read.** A digest is a
statement about a past week. Recomputing it later against recategorised
transactions or a corrected import would silently change history, and a user who
read "$340 on groceries" last Sunday must still see that. This mirrors the
existing summary-caching rationale in `jobs/summary.go`.

**Per-user, not per-household.** Visibility scoping (`user_id = $2 OR is_shared`)
means two members legitimately see different numbers. One shared row would leak
private institutions.

## Backend

### Persist what is already built

In `DigestWorker.Work` (`digest.go:172`), write a `digest_entries` row alongside
the existing notification send. Two rules:

- **A push failure must not lose the entry.** Write the entry first, then send.
  Today a delivery failure loses the content entirely.
- **Generate the entry even when no notification channel is configured.** This is
  the central change: `DigestSweepWorker` should enqueue for every household on
  cadence, not only those with a channel. Check `digest.go`'s existing gating and
  `boolPref` (line 325) — the in-app digest should have its own preference,
  defaulting on, separate from the push preference.

### Content

Reuse what `buildDigestNotification` assembles, extended for a richer surface
than a push body allows:

- Spending vs. budget for the period.
- Largest transactions.
- Net-worth change.
- The period's insights.
- Upcoming bills (doc 13, conditional).
- Advisor suggestions (doc 24, conditional).

### Optional SMTP

Off by default, configured through `config/config.go`. Sends the same content.

**The README says the app "sends no email and phones home to nothing but Plaid
and (optionally) your AI provider."** Update that line to "sends no email unless
you configure SMTP." Do not leave a claim in the README that the code has made
false — same rule as doc 14's benchmark fetch.

Use `net/smtp` or a small library; support STARTTLS and implicit TLS. Treat send
failures as non-fatal to the job, matching how insight-push failures are handled
(`jobs.go:657-660`).

### API

`GET /api/digests` (paginated), `GET /api/digests/{id}`, and a mark-read. Scope
to the requesting user — a digest entry is per-user and another member must not
read it.

## Frontend

A **Digest** route, or a Dashboard panel that links to a history — either works;
the history is the requirement.

- "This week" prominently, with previous entries paginated behind it.
- Unread indicator, using `read_at`.
- Render the stored `payload`, not a live refetch. If the two differ that is
  correct and expected.
- Digest cadence and in-app/push/email toggles in Settings, alongside the
  existing notification preferences.

## Verification

- `go test -p 1 ./...`.
- **The immutability assertion:** generate a digest, recategorise a transaction
  in that period, re-read the digest — the figures are unchanged. This is the
  core invariant.
- A household with no notification channel still gets an in-app entry. Assert
  directly; it is the main behavioural change.
- A push send failure still leaves the entry persisted.
- The unique constraint prevents a double entry when the sweep runs twice in a
  period.
- Two members of one household with different institution sharing get different,
  correctly-scoped entries; neither can read the other's.
- SMTP off by default; with it configured, a send failure does not fail the job.
- Frontend `tsc -b`, `vite build`, `oxlint`.

## Out of scope

- Rebuilding digest generation. It exists and works.
- Rich HTML email templating — plain text or minimal HTML.
- Per-section subscription preferences. One digest, one toggle set.
- Replacing ntfy. This is an additional surface, not a migration.
