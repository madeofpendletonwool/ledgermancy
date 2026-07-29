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
overlap, prefix containment, vowel-dropped abbreviation — pure string work) and
the AI pass that sees only the residue write aliases marked `source='suggested'`,
and **every reporting query excludes those.** A pass therefore cannot change a
total; it can only fill a review queue. That is what makes it safe to run
unattended, and it is asserted directly by a test.

Nothing changes anywhere until you **Confirm** a grouping here.

## Suggested groupings

The review queue shows each proposal with the evidence behind it — each
descriptor's sample name, total spend, charge count, and date range — because
nobody can judge *same business?* from two strings alone, but the spend and the
date spans usually settle it.

- **Group these as** — set the canonical name for the merged merchant.
- **Confirm** to make the suggestion count, or **Not the same** to reject it.
- Rejections are **remembered**. A pair you reject is never proposed again, and
  because transitivity could otherwise re-form the merge through a third
  descriptor, **a component containing any rejected pair is dropped whole** rather
  than split around the refusal.

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

## What it improves as a side effect

Because the resolved merchant becomes the unit of analysis everywhere, one merge
fixes things across the app at once:

- **[Recurring detection](spending.md)** — a subscription split across two
  descriptors is finally detected as recurring; a subscription's price creep is
  measured against the whole.
- **[Top merchants](spending.md)** and the monthly recap stop double-counting a
  single business.
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
