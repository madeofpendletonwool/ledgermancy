# Digest

A periodic recap of what happened with your money — the period's figures, an AI
narrative when you have a provider configured, and the [insights](insights.md)
worth a look. It's the Sunday-morning check-in.

The digest lives **in the app**, on its own page, and is kept. It can also be
pushed to your notification channel or emailed to you, but neither is required
and neither is the point: a push you dismiss is gone, and the recap it carried
used to be gone with it.

## What's in one

- **In, out, left over** for the period, with the savings rate — and, when you
  keep [paystubs](paystubs.md), the honest gross-based rate alongside it.
- **The narrative**, when an AI provider is configured. Written from the figures
  below, never instead of them.
- **Running above usual** — the categories most above their own trailing
  average. Normally the most useful part.
- **Where it went** and the **biggest purchases**.
- **Budgets**, worst-off first.
- **Net worth** and how it moved over the period.
- **Coming up** — [bills](schedule.md) due in the next fortnight, when you keep
  a bill calendar.
- **Worth a look** — the top unread insights at the time it was written.

Sections whose feature you don't use simply don't appear.

## The figures do not change

!!! important "A digest is a statement about a past period"
    Every figure is **snapshotted** when the digest is written and rendered
    exactly as generated, forever. Recategorise a transaction inside that period
    and the digest still says what it said when you read it.

    So a digest disagreeing with today's [Spending](spending.md) page is
    **correct**, not stale. The alternative — recomputing on read — would mean a
    digest that quietly rewrote itself, and "$340 on groceries last week" would
    become a number you could never check.

Digests are also **write-once**: nothing rewrites a stored one, including a
second sweep, a retry, or **Send one now**.

## Cadence

Set it under **Settings → Digest**.

- **Weekly** — written on a Monday, covering the month so far.
- **Monthly** — written on the 1st or 2nd, covering the month just finished. (Day
  2 gives the previous month's last day time to settle.)

**Send one now** writes a digest for the current period immediately, ignoring
the schedule. It doesn't consume the scheduled digest's slot, and — since the
digest always has the in-app page to land on — it doesn't need a notification
channel.

## Where it goes

Three independent switches, one per surface. Turning one off never silences the
others.

| Surface | Default | Needs |
| --- | --- | --- |
| **Keep it in the app** | **On** | Nothing |
| **Push it to me** | Off | A notification channel (**Settings → Notifications**) |
| **Email it to me** | Off | The operator to have configured SMTP |

A push that fails no longer loses the digest: the entry is written **before**
delivery is attempted, so the content survives an unreachable ntfy server or a
mail server that is down.

### Email

Off by default, and inert unless the operator set `SMTP_HOST` — see
[Configuration](../configuration.md#email-smtp). The digest is the only thing
Ledgermancy ever emails, it goes to your account address, and it is plain text.

## Per-person, by design

Two members of one household legitimately see **different figures** for the same
period: [visibility scoping](../concepts.md) means a private institution counts
towards its owner's digest and nobody else's. So each member gets their own
entry, and one member cannot read another's — enforced in SQL on every read, not
just by hiding a link.

## Privacy

The digest adds no new outbound anything. The figures are computed locally in
SQL and decimal; the narrative reuses the same monthly recap your
[Financial Summary](report.md) shows, so it makes no extra model call on the
common path. With no AI key the digest is unchanged apart from the missing
paragraph.
