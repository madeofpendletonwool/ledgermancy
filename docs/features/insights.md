# Insights

![Insights](../screenshots/insights.png)
<em>Things worth a look — spending changes, budget pace, recurring charges.</em>

Insights is the proactive feed: things the app has noticed about your money that
are worth a glance. It is **not AI-gated** — the feed is deterministic and exists
with or without an AI key.

The same feed also appears at the top of the [Dashboard](dashboard.md) (a few
cards) and rides along in the scheduled [digest](#digests) when you have one
configured.

## What kinds of insights exist

- **Spending spikes** — a category notably higher than its recent typical.
- **Budget pace** — how you're tracking against your [budgets](budgets.md), plus a
  budget-vs-actual trend.
- **New recurring charges** — a merchant that just started appearing on a
  cadence.
- **Subscriptions & price creep** — a subscription whose charge has risen.
- **Upcoming bill** — a known [obligation](schedule.md) about to fall due. This
  is the one insight that can prevent an overdraft rather than explain it
  afterwards; a large one is high enough priority to push.
- **Forecasts** — projected month-end cash position.
- **Large transaction** — a single charge that stands out for its context, judged
  against the household-wide typical. For any merchant with five or more prior
  charges it **yields to the per-merchant outlier below** — one behaviour, not
  two — so a large charge at a known merchant raises one insight, not a pair.
- **Merchant outlier** — a charge that is statistically implausible for *that
  merchant*: Netflix normally $15.99, this one $900. Judged against the
  merchant's own baseline (median and 95th percentile, computed leave-one-out
  on demand), and only after five prior charges and a minimum dollar floor — a
  $4 coffee that is 3× a $1.30 baseline is statistically odd and practically
  noise. The comparison is shown inline: this charge, the typical charge, and
  the sample size.
- **Possible duplicate** — the same merchant, same amount, within a day. The
  classic double-charge, and the most common real billing error. A habitual-repeat
  check silences merchants that legitimately double up at a fixed price (transit
  fares, the daily coffee), and a charge paired with its reversal is dropped by
  sign.
- **Income change** — your income shifting noticeably.
- **Savings milestones** — savings-rate thresholds crossed.
- **Goal progress** — a nudge where a [goal](goals.md) needs attention.
- **Document expiry** — a [warranty or policy](documents.md) about to run out.
- **Receipt match** — a scanned receipt whose charge has now posted
  ([Documents](documents.md)).
- **Alert explanations** *(AI)* — a plain-English note on why an
  [alert](alerts.md) fired.

Period-scoped insights **auto-expire** once their month passes, so the feed
stays about the present rather than accumulating history.

## Anomaly detection

The merchant-outlier and duplicate-charge producers are the fraud-and-billing-mistake
pair, and they are deterministic like everything else in this feed — a model is
never asked to decide what is anomalous.

A couple of behaviours worth knowing:

- **Sensitivity** is a household setting (conservative / balanced / sensitive)
  in **Settings**. The floor is fixed across all three so that tightening it can
  never open a gap where neither the per-merchant outlier nor the household-wide
  large-transaction producer fires.
- **"This is normal"** on an outlier card writes a per-merchant suppression, so
  a merchant you have decided is fine does not re-fire on the next pass.
- **Dismissal survives regeneration.** Both anomaly kinds key on a stable
  transaction id rather than the merchant, so merging that merchant later does
  not silently resurrect an insight you dismissed.

## Using the page

- Toggle **Show dismissed** to include insights you've dismissed.
- Dismiss an insight from its card to clear it from the unread feed.

## Push and digests

Insights surface in-app and in the digest, and the high-priority ones **push to
your notification channel in real time** the moment they are detected — the same
dispatch path an [alert](alerts.md) uses. Not every insight does: only a
newly-created one above a priority threshold pushes (a large upcoming bill should
reach your phone; a small budget-pace nudge should not). Lower-priority insights
are pull + digest.

### Digests

A digest is a periodic recap — the period's figures, your narrative and the top
insights. It is kept in the app on the [Digest](digest.md) page, and can
additionally be pushed to your notification channel or emailed to you.

It needs no configuration to exist: the in-app digest is on by default and does
not require a notification channel. See [Digest](digest.md).
