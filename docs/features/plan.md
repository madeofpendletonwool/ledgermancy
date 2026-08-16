# The plan

The Plan page is where your household keeps what it **means**, in writing —
every other page shows where you *are*, computed. Three kinds of content, one
page, no math.

## Sections

A fixed outline rather than a blank wiki:

- **Strategy & priorities** — what you are doing and why, in your words. This
  is the section the [Advisor](advisor.md) reads before it answers anything.
- **Income & employment** — the context the bank feed cannot carry: side
  income, expected changes, whose job is stable.
- **Estate & insurance** — wills, policies, beneficiaries. Keep the paperwork
  itself in the [document vault](documents.md); keep the decisions here.
- **Per-person notes** — one card per person on the household, for the 529
  reasoning, custodial situations, and allowance thinking that belongs to a
  person rather than to the household strategy.
- **Notes** — everything else.

Sections hold prose, not figures. Anything the app can compute — an emergency
fund target, a MAGI, a payoff date — already has a home that stays current, and
a number restated in the plan is stale the day after the next sync. Link to the
live thing; don't copy it.

## Decisions

The decisions log is append-only on purpose. A confirmed decision is never
edited and never deleted — when you change your mind, you add a new decision
that names the old one, and the old one stays visible, struck through, under
its replacement. In two years, "why did we do that" is answered by the log
itself.

Each decision has a date you set, not the date you typed it in — a plan is
often written down after the fact, and backdating honestly is supported.

## The advisor uses it — both directions

- **It reads the plan.** The advisor briefing carries a digest of your
  strategy, per-person notes, and active decisions, so a question like "why is
  our emergency fund only three months" gets answered with your stated
  reasoning rather than a lecture about the six-month rule.
- **It proposes, you confirm.** Any advisor answer has a **Save to plan**
  button. It lands as a *proposed* decision — editable, discardable, and not
  part of the log until you confirm it on the Plan page. The chat never
  writes to your plan unattended.

## Keeping it current

A plan nobody re-reads is worse than no plan: it still says what you used to
mean. Two halves keep it honest:

- **Mark reviewed** stamps the whole plan as re-read and still current.
- If the newest of your review stamp, section edits, and confirmed decisions
  goes quiet for about six months, a **plan stale** reminder lands in the
  [Reminders](index.md) feed — and leaves the moment you stamp a review.

## Privacy

Plan text is the most sensitive writing in the app — intentions, names,
timelines. Bodies are encrypted at rest with the same key that seals advisor
transcripts and vault documents, are withheld from the portable JSON export,
and the page is not available offline: the plan is readable while it is on
your screen, not from a browser cache.
