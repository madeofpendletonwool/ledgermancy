---
tags:
  - operations
---

# Continuity

**Settings → Continuity** (owner only)

Whether this deployment could actually be restored — and, unusually for a backup
feature, some evidence rather than an assumption.

Nobody else has a copy of your ledger. There is no support line and no account
recovery. That is the arrangement self-hosting makes, and this page exists so it
stays a good one.

## What runs

Backups are **on by default**. Every other optional subsystem in Ledgermancy
defaults off; this one does not, because opting in to backups is something
people intend to do and don't.

Once a day the worker takes three artefacts:

| Artefact | What it is | Why both exist |
| --- | --- | --- |
| **Database dump** | `pg_dump`, custom format | Restores *this app*, exactly |
| **Document archive** | `tar.gz` of the vault | `pg_dump` holds every document's title and none of its bytes |
| **Portable export** | plain JSON, documented below | Outlives this app entirely |

Once a week it does the part almost nobody does by hand: **restores the latest
dump into a temporary database and checks it**. Row counts are compared table by
table against the live database, the schema version is verified, and one document
is pulled out of the archive, decrypted, and checked against its recorded hash.
Then the temporary database is dropped.

An untested backup is not a backup, it is a belief about a backup. This is the
line on the panel worth looking at.

!!! note "Your live database is never touched"
    The check builds a throwaway copy beside your real database and reads both.
    You are not expected to restore your production instance on a schedule, and
    you should not — this exists precisely so that you do not have to find out
    whether a backup works by using it.

## Reading the panel

Each row is graded and says what would happen if that failure were tested today,
rather than reporting a status word:

> Your last verified restore was 40 days ago. If the database failed today, you
> would be restoring from a backup nobody has tested.

Grey means switched off deliberately. Red means either a failure or "configured
and has never worked" — which looks like a fresh install and is not.

The **second backup location** row is grey until you set `BACKUP_MIRROR_DIR`. Note
what it does and does not claim: the app cannot see what hardware is underneath
`BACKUP_DIR`, so it will never tell you your backups are or are not on this
machine. If you have already pointed `BACKUP_DIR` at a NAS mount, they are
off-host and this row being grey is not a criticism. What the row reports is
simply that one location is configured rather than two — and a second
independent copy is still worth having, because losing a location to a dead
disk or a mistaken `rm` is a different failure from losing the machine.

The **encryption key** row is red until you confirm you have stored
`ENCRYPTION_KEY` somewhere safe. The app cannot verify this and does not pretend
to — asking is the point. Without that key a restored database will not decrypt
its own Plaid tokens or open a single document, and nobody can recover it for
you.

## What is covered

Every table in the schema is classified into one of four categories, and **the
build fails if one is not**. A feature cannot ship with data nobody thought to
back up, because the test that enumerates the schema will not let it.

| Category | Meaning |
| --- | --- |
| **Your data** | Things you created, decided, or accumulated. In the dump *and* the portable export. |
| **App internals** | Credentials and bookkeeping. In the dump only — an access token means nothing outside this app, and does not belong in a plain JSON file. |
| **Recomputed** | Rebuilt by a job: the insight feed, alert events, monthly recaps. |
| **Discarded** | Sessions and queue state. Restoring these would be wrong, not merely unnecessary. |

Durable state that lives outside Postgres — today, the document vault — is
registered separately, so it appears on the panel and in the backup whether or
not anyone remembered to add a line to the UI.

Classifying a table as *your data* is all it takes to enrol it in the dump, the
export, and the restore test's row-count check. Nothing else needs editing.

## The portable export

The dump restores this app. The export is what makes "you own your data" true
rather than rhetorical: plain JSON, documented here, readable by anything, with
no dependency on Ledgermancy or on Postgres ever running again.

Download the current one from the panel, or find the scheduled ones in
`BACKUP_DIR/export/`.

```json
{
  "meta": {
    "schema_version": 1,
    "generated_at": "2026-07-29T16:54:23Z",
    "migration_version": 35,
    "application": "ledgermancy",
    "row_counts": { "transactions": 108, "accounts": 24 },
    "note": "…"
  },
  "tables": {
    "transactions": [
      {
        "id": "e845535a-9b7f-4528-a127-2d81d09dd96a",
        "account_id": "8e8c399b-46a5-4f4b-a0d9-f7b6d9096e3c",
        "amount": "-500.0000",
        "currency": "USD",
        "date": "2026-07-11",
        "name": "United Airlines"
      }
    ]
  }
}
```

Rules the format guarantees:

- **Money is always a decimal string, never a JSON number.** JSON has one
  numeric type and it is a double. Values are cast to text in SQL, before any
  driver sees them, so nothing downstream is in a position to turn `1234.56`
  into `1234.5599999999999`. Trailing zeros reflect the column's declared scale.
- **Tables are keyed by their database name, rows by their column name.** Not a
  curated shape — a mechanical one, so completeness can be checked rather than
  hoped for.
- **Dates are `YYYY-MM-DD`.** Timestamps are RFC 3339. IDs are readable UUID
  strings.
- **Stored JSON is passed through byte for byte**, never parsed and re-encoded.
- **No credentials.** Password hashes, encrypted Plaid tokens, TOTP secrets and
  recovery codes are excluded — by column type, not by a list somebody has to
  remember to update. This file is your data, not your logins.
- `schema_version` changes only when the envelope changes. A table gaining a
  column is not a breaking change; readers are expected to tolerate it.

## Retention

7 daily, 4 weekly, 6 monthly by default.

Each of the three kinds of file gets its own count — so you keep 7 daily
database dumps *and* 7 daily document archives *and* 7 daily exports, not 7
files in total. If you have configured a second location, it is pruned on the
same schedule independently, so a slow or briefly unreachable NAS never drags
the primary's retention with it.

Retention is relative to the backups that exist, not to the clock. A home server
that was powered off for a month comes back, keeps what it has, and starts a new
daily series — rather than concluding that everything it owns has expired.

Files it did not write are never touched, so your own
`ledgermancy-2024-manual.dump` can sit in the directory safely.

## Configuration

Set in `.env`; these describe the host, so changing them is a deploy.

| Variable | Default | |
| --- | --- | --- |
| `BACKUP_ENABLED` | `true` | |
| `BACKUP_DIR` | `/var/lib/ledgermancy/backups` | A volume in the Compose deploy |
| `BACKUP_MIRROR_DIR` | *(empty)* | Second destination — set this |
| `BACKUP_INTERVAL` | `24h` | |
| `BACKUP_RESTORE_TEST_INTERVAL` | `168h` | |
| `BACKUP_KEEP_DAILY` / `_WEEKLY` / `_MONTHLY` | `7` / `4` / `6` | |
| `BACKUP_INCLUDE_DOCUMENTS` | `true` | Ignored when the vault is off |

The worker image pins `postgresql17-client` to match the `postgres:17` server.
A cross-major `pg_dump` can produce an archive that will not restore, so the app
checks the two versions at boot and before every dump, and **writes no file at
all** on a mismatch — a stale backup is a problem you can see, a fresh one that
does not restore is not.

## Restoring

The step-by-step procedure is in
[DEPLOYING.md §7](https://github.com/madeofpendletonwool/ledgermancy/blob/main/DEPLOYING.md#7-continuity-backups-and-proving-they-work).
It was run start to finish before it was written down.

Four things are needed and missing any one of them fails the restore: the code,
your `.env` (for `ENCRYPTION_KEY`), the database dump, and the document archive.
Verify by **opening a document**, not just by logging in — logging in proves the
database restored; opening a document proves the database, the archive and the
key all agree.
