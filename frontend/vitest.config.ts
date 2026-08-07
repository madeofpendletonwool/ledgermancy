import { defineConfig } from 'vitest/config'

// Deliberately NOT an extension of vite.config.ts.
//
// That file is the *build*: the PWA plugin, Tailwind, the dev proxy. None of it
// is reachable from a unit test, and loading it would mean every `npm test`
// generates a service-worker manifest to run an assertion about date parsing.
// Vitest still transforms TypeScript through the same esbuild pipeline either
// way, so the only thing skipping the plugins costs is the plugins.
export default defineConfig({
  test: {
    // `node`, not `jsdom`: everything covered here is library code. api.ts is
    // the one module that touches the DOM at all, and it touches exactly
    // `document.cookie` — a stub in the test is more honest than a whole DOM
    // implementation as a dependency. Component tests would need jsdom and
    // @testing-library/react; that is a separate decision, not a default.
    environment: 'node',

    // A default timezone west of UTC — the condition under which a calendar
    // date serialised as midnight UTC renders as the previous day.
    //
    // This is a backstop, not the mechanism. Every date test in money.ts and
    // period.ts stubs TZ itself, so those catch the midnight-UTC trap on any
    // runner including a UTC one (verified: reverting formatDate to
    // `new Date(iso)` fails the same 6 tests either way). What this buys is the
    // NEXT date test — the one written without thinking about zones. Under a
    // UTC default that test would pass while the code was wrong, and nobody
    // would find out until a user west of Greenwich did.
    env: { TZ: 'America/Los_Angeles' },

    include: ['src/**/*.test.ts', 'src/**/*.test.tsx'],
  },
})
