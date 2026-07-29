# Merchants

<em>Your bank writes one business several ways. Grouping them lets the app see one
merchant instead of three.</em>

A single business arrives as a handful of different descriptors — `AMAZON.COM`,
`AMZN.COM/BILL`, `AMZ*ORDER 1234`. To the app each fragment is a separate
merchant, and that quietly degrades almost everything: a subscription billing
under two descriptors never reaches the recurring threshold and is never
detected, the second descriptor of an old merchant fires a false *new merchant*
alert, and every report double-counts a business it should be grouping.

Merchants is where you collapse those fragments into one canonical business — and
where the app proposes merges for you to confirm.

## A suggestion never moves a number

This is the rule the whole page is built on. Both the deterministic pass (token
overlap, punctuation-insensitive equality, prefix containment, vowel-dropped
abbreviation — pure string work) and the AI pass that sees only the residue write
aliases marked `source='suggested'`, and **every reporting query excludes those.**
A pass therefore cannot change a total; it can only fill a review queue. That is
what makes it safe to run unattended, and it is asserted directly by a test.

Each pass **replaces** the queue rather than adding to it, so a proposal the
current rules would no longer make does not outlive the rules that produced it.

Nothing changes anywhere until you **Confirm** a grouping here.

## Suggested groupings

The review queue shows each proposal with the evidence behind it — each
descriptor's sample name, total spend, charge count, and date range — because
nobody can judge *same business?* from two strings alone, but the spend and the
date spans usually settle it.

- **Group these as** — set the canonical name for the merged merchant.
- **Tick the descriptors that belong.** A proposal is a guess about a *set*, and a
  guess about a set can be half right — the engine will offer you `HOME DEPOT`
  and `HOMEGOODS` together sooner or later. Untick the ones that don't belong and
  the button becomes **Group N of M**.
- **Confirm** groups what you ticked. Whatever you unticked is recorded in the
  same action as a *different* business, which is what makes it come back as its
  own proposal next pass instead of the same wrong grouping arriving forever.
- **Not the same** dismisses the whole proposal.
- Rejections are **remembered** pairwise. Because transitivity could otherwise
  re-form a merge through a third descriptor, matches are applied
  strongest-evidence-first and a match is skipped when it would put a rejected
  pair in one group. A refusal therefore **cuts** the descriptor graph where you
  objected rather than retiring the whole family — so having said `HOMEGOODS` is
  not `THE HOME DEPOT`, next month's `THE HOME DEPOT #1234` is still proposed.

Suggestions refresh on a daily schedule; **Scan for groupings** runs a pass
immediately.

### When a category would be clobbered

If the descriptors being merged were filed under two *different* categories you
set by hand, **nothing is changed** — the *manual is sticky* rule outranks any
merge, and the page tells you to pick one on the [Transactions](transactions.md)
page first. A merge where the fragments agree simply reconciles to the shared
category.

## Group merchants yourself

Pick two or more descriptors the app hasn't grouped, name the group, and merge.
Only **ungrouped** descriptors are offered — to move one that is already in a
merchant, separate it first, which keeps *where did this go?* answerable.

## Grouped merchants

These read as one merchant everywhere in the app. From a grouped merchant you
can:

- **Rename** the canonical name.
- **Separate** any descriptor that doesn't belong — split is a first-class action
  on purpose. An over-eager merge *will* happen eventually, and one nobody can
  undo is worse than one that never happened.
- **Open the merchant** by name, for its spending history (see below).

A merchant is retired only once it has **no** descriptors left. A merchant holding
one descriptor is a name you chose, which is worth keeping: normalising descriptors
at import means a business whose every form collapses to a single key legitimately
has one, and discarding it would put the raw bank text back in your reports.

## Per-merchant detail

Any merchant name in the app — here, on the [Dashboard](dashboard.md)'s top
merchants, in the recurring table on [Spending](spending.md) — opens that
merchant's own page: total, charge count, per-charge and per-month averages,
largest charge, spend per month as a chart, how the spending is filed by category,
and every charge behind those numbers. On a grouped merchant the charge list shows
which descriptor each charge arrived under.

This works for merchants you have **never grouped**, not just merged ones, because
the page is addressed by the *resolved* merchant key — an entity for a grouped
merchant, the raw descriptor otherwise. Most of a household's spending sits at
merchants nobody has grouped, and they need a history too.

## What it improves as a side effect

Because the resolved merchant becomes the unit of analysis everywhere, one merge
fixes things across the app at once:

- **[Recurring detection](spending.md)** — a subscription split across two
  descriptors is finally detected as recurring; a subscription's price creep is
  measured against the whole.
- **[Top merchants](spending.md)** and the monthly recap stop double-counting a
  single business. Top merchants groups by resolved key rather than by display
  name, so a bank that varies its own text — `THE HOME DEPOT #4905` one month and
  `THE HOME DEPOT 4905` the next — still reads as one row.
- **[Alerts](alerts.md)** — a merged merchant's second descriptor no longer fires
  a false *new merchant* alert; *new merchant* resolves against the entity.
- **[Categorisation](../concepts.md#categorisation-order)** — the same business is
  categorised once rather than re-sent to the model per fragment.

An unmapped merchant behaves exactly as it did before this page existed. The
feature is additive: a household that never opens it sees no change at all.

## Not included

- **Merchant logos.** A nice touch, but an outbound dependency the app otherwise
  does not have.
- **Cross-household shared merchant data.** Everything here is household-scoped,
  matching every other surface.
