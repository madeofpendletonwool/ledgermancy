# Install & offline

Ledgermancy installs to a phone's home screen and opens without a network.
There is no app store, no native build, and nothing to configure — a
self-hosted deployment is installable the moment it is served over HTTPS.

## Installing

**Android / Chrome / Edge.** The app offers an **Install** bar the first time
you use it. Dismiss it and it never comes back; you can still install from the
browser's own menu whenever you like.

**iOS / Safari.** Apple provides no programmatic install, so the app shows the
manual route instead: **Share → Add to Home Screen**. Same result.

**Desktop.** Chromium browsers show an install icon in the address bar.

Installed, it runs standalone — no browser chrome, its own icon, its own entry
in the app switcher.

!!! note "HTTPS is required"

    Browsers only register a service worker on a secure origin, so an
    installable Ledgermancy needs TLS. `localhost` is exempt, which is why it
    works in development. See [Deployment](../deployment.md) for the reverse
    proxy setup.

## What works offline

Offline mode is **read-only, and deliberately so.**

When the network is gone, the app opens and re-renders the last data it
successfully fetched for the pages you had visited: dashboard, spending, net
worth, investments, budgets, goals, transactions, categories, alerts and
insights. A page you have never opened on that device has nothing saved and
will say so rather than spinning forever.

Whenever you are looking at saved figures, a banner sits under the header and
states the time they were fetched:

> **Offline.** These figures were saved at 14:32 and are not live. Changes
> cannot be saved until you reconnect.

That banner is the point of the whole feature. A balance from this morning
looks exactly like a balance from this second, and the app never lets you
mistake one for the other.

## What does not work offline

Everything that writes. Recategorising, editing a budget, adding a transaction,
importing a CSV, syncing an institution, uploading a document, asking the
assistant — all of it is switched off while you are offline, with a tooltip
saying why.

**Nothing is queued for later.** This is a considered decision, not a missing
feature. Replaying a recategorisation against data that moved while your phone
was in a tunnel is a correctness problem, and a half-built version of it — one
that silently applies a stale edit to a changed ledger — is worse than not
having it at all. If you try to change something offline, you are told
immediately and clearly that it was not saved.

Two things are also never available offline no matter what:

- **Documents.** The vault decrypts files on read. Its entire premise is that
  your tax returns are encrypted at rest, and writing the decrypted bytes into
  a browser cache would quietly undo that.
- **Signing in.** Sessions are validated by the server. If you are signed out,
  you stay signed out until you can reach it.

## Sessions and stale installs

An installed app left closed for a week comes back to an expired session. The
app handles that specific case deliberately:

- Offline, it remembers *who* was signed in — only enough to draw the screen —
  so the app opens to your data instead of bouncing to a login form you cannot
  submit.
- The instant connectivity returns it re-asks the server. If the session really
  did expire, you land on the login screen cleanly rather than watching every
  request fail behind a cached shell.

Signing out clears both the remembered identity **and** every saved figure on
disk. On a shared device, the next person cannot pull your balances up by
turning off the network.

## Updates

A deploy does not disturb an open app. When a new build is detected you get a
bar offering **Reload**, and the update applies when you accept — never in the
middle of a budget edit.

If you are running a reverse proxy of your own in front of Ledgermancy, do not
add caching for `/sw.js` or `/manifest.webmanifest`. The container already
serves both with `Cache-Control: no-cache`, and a proxy that caches them will
pin installed clients to an old build indefinitely — the one genuinely
confusing failure mode this feature has.

## Push notifications

Not through the browser. Ledgermancy delivers push via
[ntfy](../configuration.md#push-notifications-ntfy), which works the same on
desktop and mobile, installed or not, and does not require the app to be open.
Adding a second notification path was out of scope.
