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
- **Large transaction** — a single charge that stands out for its context.
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
