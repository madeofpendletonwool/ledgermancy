# 20 — PWA install + offline read

*(TODO.md "Next major initiatives" #11.)*

## Context

Self-hosters check their finances on their phones constantly. The SPA works in a
mobile browser — `9d9c307 Mobile friendly` did that work — but it is not
installable and does not degrade at all offline: a dropped connection produces
failed fetches and empty cards rather than the numbers the user saw ten minutes
ago.

An installable, offline-readable app closes most of the distance to a native
mobile competitor without writing a native app. It is also the cheapest item in
the backlog: **no backend changes at all** for the offline-read MVP.

## AI vs deterministic split

No AI. Pure frontend.

## Prerequisites

None, and it touches nothing any other doc touches — `vite.config.ts`,
`index.html`, `public/`, and a small amount of `App.tsx`. It is the safest doc to
run in parallel with anything.

## Data model

None. No migration, no API change.

## Frontend

### Manifest

`frontend/public/manifest.webmanifest`, referenced from `index.html`:

- `name` / `short_name`, `display: "standalone"`, `start_url: "/"`,
  `scope: "/"`.
- `theme_color` and `background_color` from `BRAND.md` — do not invent colours;
  the palette is documented and the logo already lives in `images/`.
- Icons at 192×192, 512×512, and a 512×512 `maskable` variant. The maskable one
  matters: without it Android crops the logo badly and the install looks broken.
- Apple touch icon in `index.html` — iOS ignores the manifest for this.

### Service worker

Use `vite-plugin-pwa` rather than hand-rolling registration and precache
manifests; it integrates with the existing Vite build and generates the shell
precache list automatically.

Caching strategy, and the distinction is load-bearing:

- **App shell** (JS, CSS, fonts, icons) — precache, cache-first. Versioned by the
  build hash so a deploy invalidates cleanly.
- **API GET responses** — network-first with a cache fallback, and only for
  read-only report endpoints (dashboard, spending, net worth, transactions list,
  categories). Never cache-first: stale financial figures presented as current
  are worse than an error.
- **Never cache** auth endpoints, session state, or anything under a mutation
  path.

### Offline UX

- A persistent, unmissable banner when serving cached data: **"Offline — showing
  data from 14:32."** Not a subtle icon. A user must never mistake a cached
  balance for a live one; that is the whole risk this feature introduces.
- Read-only mode offline: disable mutation controls (recategorise, budget edit,
  CSV import, sync) rather than letting them fail obscurely.
- Routes with no cached data show an honest empty state, not a spinner forever.

### Write queueing (stretch)

The TODO lists queue-and-replay for writes. **Treat it as explicitly optional and
land the read-only MVP first.** Replaying a queued recategorisation against data
that changed while offline is a genuine correctness problem, and a half-built
version of it is worse than not having it. If it is attempted: queue only
idempotent mutations, stamp each with the client time, and show the queue to the
user rather than replaying silently.

### Install promotion

Capture `beforeinstallprompt`, show a dismissible "Install app" affordance, and
respect a dismissal persistently. iOS Safari does not fire the event — detect
standalone mode and show brief Add-to-Home-Screen instructions instead, or show
nothing. Do not nag.

### Session interaction

Sessions are server-side. An installed PWA left open for days will hit an expired
session, and the failure must be a clean redirect to login, not a wall of failed
requests behind a cached shell. Test this path explicitly — it is the most likely
real-world bug in the whole doc.

## Verification

- `npm run build` produces a manifest, a service worker, and a precache list.
- Lighthouse PWA audit passes installability.
- Install on Android Chrome and iOS Safari; confirm the icon is not cropped and
  the app opens standalone.
- **Offline test:** load the dashboard, go offline via devtools, reload — cached
  figures render, the offline banner is visible and states the timestamp, and
  mutation controls are disabled.
- Deploy a new build while a client has the old SW; confirm it updates rather
  than serving a stale shell indefinitely.
- Expired session while installed and offline-then-online → clean login redirect.
- `tsc -b` and `oxlint` clean.

## Out of scope

- Push notifications through the browser. ntfy is the notification path
  (`backend/internal/notify/`) and adding a second one is a different project.
- A native app.
- Offline write replay, unless the MVP lands first and it is taken up
  deliberately.
- Background sync.
