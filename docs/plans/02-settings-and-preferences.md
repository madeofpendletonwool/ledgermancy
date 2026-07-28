# 02 — Settings page & preferences store

## Context

Today the only account-management surface is a single **Security** page
(`frontend/src/routes/Security.tsx`, ~640 lines: `TwoFactorSection`,
`PasswordSection`, `SessionsSection`, `ActivitySection`), reached at `/security`
(`App.tsx` line ~55, nav in `AppLayout.tsx` line 19). There is **no
preferences/settings storage anywhere** — no table, no API, no client.

Notifications (`03`), the insight feed (`04`), and the scheduled digest (`10`)
all need per-user knobs: which channel to push to, which insight kinds to push,
whether the digest is on and how often. This doc builds that home. It:

1. Reframes Security as **Settings** — Security becomes one tab/section within a
   `/settings` page, with **all existing security functionality preserved**.
2. Adds the **preferences store** (this doc *owns* the preferences contract in
   `00-shared-contracts.md`).
3. Ships the Settings UI shells for the reserved preference keys. The keys are
   *stored and editable* here; the actual notification/digest wiring that reads
   them lives in `03` and `10`.

## AI vs deterministic split

No AI. Pure CRUD over a key/value store plus a settings UI.

## Prerequisites

None hard. Lands early in Wave 0. `03`, `04`, and `10` depend on this.

## Data model

`preferences` table — **new migration** (claim the next free number; at time of
writing that is `00009_preferences.sql`; goose-annotated `-- +goose Up` /
`-- +goose Down` like `00008_monthly_summaries.sql`). Shape per the shared
contract:

```sql
CREATE TABLE preferences (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scope        TEXT NOT NULL CHECK (scope IN ('user', 'household')),
    user_id      UUID REFERENCES users (id)      ON DELETE CASCADE, -- iff scope='user'
    household_id UUID REFERENCES households (id) ON DELETE CASCADE, -- iff scope='household'
    key          TEXT NOT NULL,
    value        JSONB NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (scope, user_id, household_id, key)
);
```

A JSONB key/value store, extensible without a migration per setting — the same
deliberate choice as `alerts.config`.

### Reserved keys (initial)
- `notify.channel` — `"none" | "ntfy"` (per user)
- `notify.ntfy_topic` — string (per user; their private ntfy topic)
- `notify.push_kinds` — array of insight/alert kinds to push (per user)
- `digest.enabled` — bool (per user)
- `digest.cadence` — `"weekly" | "monthly"` (per user)

The UI in this doc writes all five; `03`/`10` are the readers.

## Backend

**New queries** in a new `backend/internal/db/queries/preferences.sql`
(regenerate sqlc afterward — `go run github.com/sqlc-dev/sqlc/cmd/sqlc@latest
generate` from `backend/`):

- `UpsertPreference` — `INSERT … ON CONFLICT (scope, user_id, household_id, key)
  DO UPDATE SET value = EXCLUDED.value, updated_at = now()`. Note NULLs are not
  equal in a unique index, so scope resolution must set exactly one of
  `user_id`/`household_id` and pass the other explicitly.
- `ListUserPreferences` — user-scoped rows for one `user_id`.
- `ListHouseholdPreferences` — household-scoped rows for one `household_id`.
- (For `03`/`10`) `GetUserPreference` by `(user_id, key)` — a single-row lookup
  the notifier will use per user.

**New handlers** in a new `backend/internal/api/preferences_handlers.go`,
following the shape of `category_handlers.go` (identity from
`auth.MustFromContext`, `decodeJSON`, `writeJSON`, `s.internalError`):

- `GET /api/preferences` → resolved bundle for the caller: their user prefs
  merged with their household prefs. Return a flat `{key: value}` map (or
  `{user:{…}, household:{…}}` — pick one and keep the client in step). Include
  the defaults for reserved keys the user has never set, so the UI always has
  something to render.
- `PUT /api/preferences` → upsert one or many. Body a list of
  `{scope, key, value}`. **Household-scoped writes gated to household members**
  (the caller's `household_id` from identity — a user may only write their own
  household). User-scoped writes use the caller's `user_id`; a caller can never
  write another user's prefs.

**Routes** — add a `preferences` group in `server.go` alongside the others
(e.g. after the `/household` block, ~line 192), both behind
`authMW.Authenticate`:
```
r.Route("/preferences", func(r chi.Router) {
    r.Use(authMW.Authenticate)
    r.Get("/", s.handleGetPreferences)
    r.Put("/", s.handleUpsertPreferences)
})
```

**Client** — add to `frontend/src/lib/api.ts`: a `Preferences` type, plus
`api.preferences()` → GET and `api.setPreferences(items)` → PUT. Mirror the
existing method style (e.g. the alerts/household methods).

## Frontend

**Rename Security → Settings** without losing anything:

1. Create `frontend/src/routes/Settings.tsx`. Give it a tabbed or sectioned
   layout. Move the existing `Security.tsx` sections in **unchanged** —
   simplest path: keep `Security.tsx` and its four exported-internal sections,
   and render `<Security />` (or its sections) as the **Security** tab of
   Settings. Do **not** rewrite `TwoFactorSection`/`PasswordSection`/
   `SessionsSection`/`ActivitySection`; they work and are security-sensitive.
2. Add a **Notifications** tab and a **Digest** tab (or one Preferences tab)
   that read `api.preferences()` and write via `api.setPreferences()`:
   - Notifications: channel select (`none`/`ntfy`), ntfy topic text input,
     push-kinds multiselect (checkboxes over known insight/alert kinds). These
     are **shells** — they persist the preference; nothing sends yet until `03`.
   - Digest: enabled toggle + cadence select (`weekly`/`monthly`). Shell until
     `10`.
   Use the existing form/`Section` styling from `Security.tsx` and the `glass`
   card conventions.

**Router** (`frontend/src/App.tsx`): add `<Route path="/settings"
element={<Settings />} />`. Keep `/security` working — either leave it, or
redirect `/security` → `/settings` (a `<Navigate>`), your call; preserving the
old path avoids breaking bookmarks.

**Nav** (`frontend/src/components/AppLayout.tsx`): change the `NAV` entry at
line 19 from `{ to: '/security', label: 'Security', … }` to
`{ to: '/settings', label: 'Settings', … }`.

**Capability gating note:** the notifications/digest tabs are only *useful* once
`03`/`10` exist, but the store and UI are harmless without them, so no gate is
required here. If you want to hide the push-channel controls until ntfy is
configured, follow the exact pattern in `AppLayout.tsx`'s `useNavItems` (lines
24–35): fetch `api.capabilities()` and branch on a flag. `03` extends the
`Capabilities` payload with a `notify_enabled` flag — you can consume it once it
exists, but do not block this doc on it.

## AI notes

None.

## Verification

- New throwaway PG per README, run migrations, confirm the `preferences` table
  and its unique constraint exist.
- `PUT /api/preferences` with a `notify.channel` value, then `GET
  /api/preferences` returns it; confirm the row in psql
  (`SELECT scope, key, value FROM preferences;`).
- Confirm a user-scoped write lands with `user_id` set and `household_id` NULL,
  and a household-scoped write the reverse; confirm a non-member cannot write
  another household's prefs (gate returns 403/404).
- Every existing Security function still works from the Settings page: MFA
  enrol/disable, password change, session revoke, activity list.
- `go build/vet/test ./...` in `backend/`; `tsc`/`build`/lint in `frontend/`.

## Out of scope

- Sending anything — that is `03` (ntfy) and `10` (digest).
- The `insights` feed and its kinds enumeration — `04` (the push-kinds UI can
  hardcode the known kinds list for now).
- Household-wide notification policy beyond the reserved keys.
