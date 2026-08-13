# Webhooks

Send this household's events to something else. Ledgermancy POSTs a small,
signed JSON body to a URL you choose, retries when it fails, and records every
attempt so you can see what happened.

This is the escape hatch for every integration the app does not ship. Home
Assistant, a Discord relay, an n8n or Node-RED flow, a script that appends large
charges to a spreadsheet — none of those need to be built into the app if the app
can just tell them what happened.

!!! warning "Off by default"
    Webhooks only exist when the operator sets `WEBHOOKS_ENABLED=true`. With it
    unset, **Settings → Webhooks** says so and every `/api/webhooks` route
    answers `503`.

    Ledgermancy's standing claim is that it contacts nothing but Plaid and your
    AI provider unless you switch something on. Every other optional switch adds
    one host *we* chose; this one lets you choose the host, and what it receives
    is your household's own financial events. That is exactly the kind of thing
    that should be a decision rather than a default.

## Setting one up

**Settings → Webhooks → Add a webhook.** You give it:

- **A name** — what it is for. This is the only handle you will have on it later.
- **A URL** — where deliveries go. `http://` to a private address on your own
  network is fine and is what most of these are for.
- **The events** you want.

You are then shown a **signing secret, once**. Copy it into your receiver. Only a
sealed copy is kept, so it cannot be shown again — if you lose it, rotate it and
update the receiver.

Any adult member of the household can create one. It is not owner-only: adults
can already read every figure a webhook could carry.

## The events

| Trigger | Fires when |
| --- | --- |
| `insight.created` | The [insight](insights.md) engine raises a **new** insight. Not on a refresh — an insight that is still true is re-checked hourly, and forwarding that would announce the same finding all day. |
| `alert.fired` | An [alert](alerts.md) rule raises an event. |
| `goal.contribution.recorded` | A contribution is logged against a [goal](goals.md). |

A rule's **push** switch is about your phone, not about webhooks: an automation
subscribed to `alert.fired` gets every alert, including the ones you chose not to
be buzzed for.

### Private accounts stay private

A webhook belongs to the member who created it, and **alert deliveries are
filtered by what that member can see**. An alert raised by a charge on a private
account reaches only a webhook its owner created — never one their partner set
up. Aggregate alerts (budget thresholds, low leftover) carry no transaction and
are household-visible, so they reach every subscriber, exactly as they do in the
app.

## The payload

```json
{
  "trigger": "alert.fired",
  "event_id": "2b9a…",
  "household_id": "7d41…",
  "occurred_at": "2026-02-14T09:31:22Z",
  "data": {
    "alert_event_id": "2b9a…",
    "alert_type": "big_spend",
    "fields": { "merchant": "Blue Bottle", "amount": "412.55", "date": "2026-02-14" }
  }
}
```

Everything above `data` is identical for every event, so one generic handler can
switch on `trigger` and read `data`.

!!! important "Money is always a string"
    Every amount is a fixed-2 decimal **string**, never a JSON number. Parse it
    with a decimal type. This is the same rule the whole app follows: `412.55`
    read as a float is no longer `412.55`.

`event_id` identifies the **thing that happened** (the insight, the alert event).
The delivery itself is identified by the `X-Ledgermancy-Delivery` header, which
is stable across retries — that is the value to dedupe on.

### Headers

| Header | Meaning |
| --- | --- |
| `X-Ledgermancy-Delivery` | Unique per (event, webhook), stable across retries. |
| `X-Ledgermancy-Trigger` | Same value as the body's `trigger`, for routing without parsing. |
| `X-Ledgermancy-Attempt` | 1-based. Above 1 means "you may have seen this already". |
| `X-Ledgermancy-Signature` | See below. |

## Verifying the signature

```
X-Ledgermancy-Signature: t=1800000000,v1=6f2c…
```

- `t` is the unix second the delivery was signed.
- `v1` is `HMAC-SHA256` over the bytes **`<t>.<body>`**, keyed by the webhook's
  secret string, hex-encoded.

The key is the **whole secret string**, `whsec_` prefix included, as UTF-8 bytes.
There is nothing to strip or base64-decode first.

Verify against the **raw request body**, before any JSON parsing — re-serialising
JSON changes bytes and will not match.

```python
import hmac, hashlib, time

def verify(secret: str, header: str, body: bytes, tolerance: int = 300) -> bool:
    parts = dict(p.split("=", 1) for p in header.split(","))
    ts, sig = parts["t"], parts["v1"]
    if abs(time.time() - int(ts)) > tolerance:
        return False  # too old to accept — this is what the timestamp is for
    expected = hmac.new(secret.encode(), f"{ts}.".encode() + body, hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, sig)
```

The timestamp is inside the MAC on purpose. A signature over the body alone is
replayable forever: anyone who captures one delivery — trivially, if your
receiver is plain HTTP on a LAN — can resend it verbatim next month and it will
still verify. Checking `t` against a window is what bounds that, and only your
receiver can decide the window.

!!! note "Signed is not encrypted"
    Over `http://`, the body is readable by anything on that network. The
    signature proves a delivery came from your Ledgermancy and was not altered;
    it does not hide it. On a home LAN that is usually the right trade. Over the
    public internet, use `https://`.

## Retries, and what happens when your receiver is down

A delivery succeeds on any `2xx`. Anything else — a `500`, a `404`, a refused
connection, a timeout, or a redirect — is a failure and is retried:

**after 1 minute, then 4, then 16, then an hour** — five attempts in all, spread
over about eighty minutes.

Redirects are **not followed**. A `3xx` is a receiver asking us to send your
signed financial events somewhere else, so it is recorded as a failed attempt for
you to look at rather than obeyed.

After the last attempt the message is **dead-lettered**: marked failed, with the
reason and every attempt kept. It is not retried again, and it is not deleted.

The event is **never lost to a failed delivery**. Every message is written to the
database *before* the first request is made, so a receiver that is down, a worker
that restarts mid-request, or a container that dies costs a retry and nothing
more.

**Pausing** a webhook (rather than deleting it) is the right move while you fix a
receiver: deliveries stop, the history stays, and messages queue up rather than
failing. Re-enable it and the backlog drains.

## The delivery inspector

**Deliveries** on any webhook shows every message: its status, how many attempts
it took, and the exact body that was sent. Open one and you get each HTTP request
with the response status, headers and body — or the reason nothing answered.

This is the answer to "why didn't my webhook fire?", and it is deliberately a
page in the app rather than a line in a server log you might not have access to.

**Send test** queues a test delivery — a real one, through the real worker, with
a real signature — and it shows up in this list like anything else. Its trigger
is `webhook.test`, which nothing can subscribe to, so testing never fans out to
your other webhooks.

Delivery history is kept for **30 days**.

## Rotating the secret

**Rotate secret** mints a new one and shows it once. Every receiver holding the
old secret starts failing the moment it lands, including messages already queued
— which is correct if you are rotating because the old one leaked. Update your
receiver promptly.

## API

Everything the settings page does is available directly. See the
[API reference](../api.md#outgoing-webhooks).
