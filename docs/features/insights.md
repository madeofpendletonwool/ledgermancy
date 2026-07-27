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
- **Budget pace** — how you're tracking against your [budgets](budgets.md).
- **New recurring charges** — a merchant that just started appearing on a
  cadence.
- **Subscriptions & price creep** — a subscription whose charge has risen.
- **Forecasts** — projected month-end position.
- **Alert explanations** *(AI)* — a plain-English note on why an
  [alert](alerts.md) fired.

Period-scoped insights **auto-expire** once their month passes, so the feed
stays about the present rather than accumulating history.

## Using the page

- Toggle **Show dismissed** to include insights you've dismissed.
- Dismiss an insight from its card to clear it from the unread feed.

## Push and digests

Insights surface in-app and in the digest, but they do **not** push in real time
the way an [alert](alerts.md) does. That seam is a known gap — see
[Roadmap](../roadmap.md).

### Digests

A digest is a periodic recap — your monthly narrative plus the top insights —
pushed to your notification channel on a schedule. Configure it under
**Settings → Digest**:

1. Set up a notification channel first (**Settings → Notifications** — ntfy).
2. Toggle **Send me a digest** and pick weekly or monthly.
3. Use **Send one now** to preview a digest immediately, ignoring the schedule.
