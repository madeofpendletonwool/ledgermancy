# 16 — Self-hosted continuity, backups, and key management

*(TODO.md "Next major initiatives" #15.)*

## Context

This is the gap unique to self-hosting, and it is the only item in the backlog
where the failure mode is **permanent, total data loss**.

`ENCRYPTION_KEY` is a single point of catastrophic failure. It encrypts every
Plaid access token via the AES-GCM cipher in `backend/internal/crypto/crypto.go`.
Lose it and every institution link is unrecoverable — and there is no company to
call, which is the whole point of self-hosting and also the whole problem.

Worse, **nothing in the repo automates a backup.** `docker-compose.prod.yml`
hardens the network surface (Postgres and the api publish no host ports) and says
nothing about durability. `DEPLOYING.md` documents `pg_dump` as a manual step. A
prior production-readiness audit flagged "no automated database backup" as HIGH,
and that finding is still open.

The result: the median self-hosted install of this app is one disk failure away
from losing a year of financial history, and its operator does not know that.

Everything else in the backlog makes the app better. This makes it *survivable*.

## AI vs deterministic split

No AI. Backups are not a place for a model.

## Prerequisites

None. Fully parallel with 13, 14, and 15 — it touches ops surfaces
(`docker-compose.prod.yml`, `DEPLOYING.md`, a new sidecar) and a Settings panel,
none of which those docs go near.

**The document vault (doc 18) has since shipped, so this is no longer a "will
need updating" — it is scope.** Documents are a second thing that must be backed
up, and they are *not* in `pg_dump`: the database holds every title, type and
expiry and none of the contents. Concretely, this doc now owes:

- The `documents-data` volume (compose) or the configured S3 bucket in the
  backup sidecar, not just the dump.
- A restore test that verifies a **document downloads**, not only that the
  database restores. All three of dump, volume and `ENCRYPTION_KEY` have to
  agree, and a two-of-three restore is exactly the failure that looks fine
  until someone opens a tax return.
- A line in the continuity panel for document storage — size, backend, and when
  it was last captured.

`DEPLOYING.md` already carries the manual version of this ("The document volume
is not in the dump"); the sidecar should automate what is written there rather
than inventing a second procedure.

## Data model

**Reserved migration: `00022_backup_status.sql`.**

```sql
-- One row per backup or restore-test attempt. This table exists so the app can
-- tell the operator the truth about their recovery posture instead of assuming
-- it. A backup nobody has verified is a guess.
CREATE TABLE backup_runs (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    kind         TEXT NOT NULL CHECK (kind IN ('db_dump','restore_test','export','offhost_push')),
    status       TEXT NOT NULL CHECK (status IN ('success','failure')),
    started_at   TIMESTAMPTZ NOT NULL,
    finished_at  TIMESTAMPTZ NOT NULL,
    size_bytes   BIGINT,
    detail       TEXT,          -- error message, or row counts for a restore test
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX backup_runs_kind_started_idx ON backup_runs (kind, started_at DESC);
```

This table is **not household-scoped** — it is operator-level infrastructure
state, not user data. Guard the endpoint that reads it accordingly (see below).

## Backend / ops

### 1. Backup sidecar

A service in `docker-compose.prod.yml` running scheduled `pg_dump` against the
compose-network Postgres, writing to a mounted volume with a retention policy
(e.g. 7 daily, 4 weekly, 6 monthly). Match the file's existing house style: it
is heavily commented and explains *why* each choice is made, including the
`!override` note on list merging. Keep that standard.

Use the same Postgres major version as the server for `pg_dump` — a version
mismatch produces dumps that will not restore, which is the worst possible
failure mode here since it looks like success.

### 2. Restore test — the part that makes it real

A scheduled job that restores the latest dump into a throwaway database and
verifies it. Verification must be substantive, not "the command exited 0":

- Restore into a scratch database on the same instance.
- Compare row counts for the load-bearing tables (`transactions`, `accounts`,
  `plaid_items`, `categories`, `goals`, `budgets`, `insights`) against the live
  database, allowing for drift since the dump was taken.
- Confirm `goose` migration version matches.
- Drop the scratch database.
- Record the outcome and the counts in `backup_runs.detail`.

An untested backup is the single most common self-hosted disaster. This job is
the highest-value thing in the doc.

### 3. Portable full-data export

A scheduled export to versioned, documented, plain-decodable JSON: transactions,
accounts, net-worth history, manual assets, goals, budgets, categories,
recurring obligations. Include a `schema_version` field and document the format
in `docs/`.

This is a different artefact from the `pg_dump` and both are needed. The dump
restores *this app*; the export outlives it. Self-hosters chose self-hosting to
own their data — this is what makes that true rather than rhetorical.

Reuse the Report page's CSV export conventions where the shapes overlap.

### 4. Optional off-host push

Push the dump + export to an S3-compatible bucket, **encrypted client-side** with
a key the storage provider does not have. Off by default; configured through the
existing pattern in `backend/internal/config/config.go`.

Two things to get right:

- This is an outbound data path carrying the entire financial database. It must
  be off unless explicitly configured, and `DEPLOYING.md` must be unambiguous
  about what leaves the host.
- Do not reuse `ENCRYPTION_KEY` for the backup encryption. A separate key means a
  compromised backup key does not also decrypt every Plaid token in the live
  database.

### 5. Status endpoint

`GET /api/admin/continuity` returning the latest run per `kind`, with
success/failure and age. **This is operator-level data and must not be readable
by every household member** — gate it on whatever role the first/owner account
has, or on an explicit setting. If no such role exists yet, add the narrowest
possible check rather than exposing it broadly; note the limitation in the doc
rather than quietly shipping it open.

## Frontend

A **Continuity** panel in `frontend/src/routes/Settings.tsx`, matching the
existing section conventions there.

Red / yellow / green for each of:

- Last successful DB backup (green < 48h, yellow < 7d, red beyond).
- Last successful **restore test** — the one that actually matters.
- Last successful export.
- Last off-host push, when configured.
- **Key backup confirmed** — a manual "I have stored `ENCRYPTION_KEY` somewhere
  safe" acknowledgement with a date. The app cannot verify this; asking the
  question is the point, and an un-acknowledged key should show red.

Write the copy so the failure mode is legible. Not "backup status: stale" but
"your last verified restore was 40 days ago — if the database failed today you
would be restoring from an untested backup." Operators under-invest here because
nothing ever tells them the stakes plainly.

## Documentation

`DEPLOYING.md` gets a **Continuity** section — this is half the deliverable, not
an afterthought:

- Where `ENCRYPTION_KEY` must live (password manager + offline copy), and
  explicitly what is lost without it.
- The four things needed to rebuild: code (git), env (password manager),
  database (backup), key. Anything missing and the restore fails.
- What to back up, how often, and how to verify.
- A **tested, step-by-step restore procedure**, written as commands, that has
  actually been run start to finish before it is committed. An untested runbook
  is worse than none — it produces false confidence at the worst moment.

## Verification

- Run the sidecar against the local stack; confirm a dump appears and retention
  prunes correctly (fake timestamps rather than waiting weeks).
- **Restore-test the restore test:** corrupt a dump deliberately and assert the
  job records `failure` with a useful `detail`, rather than passing.
- Assert a `pg_dump` major-version mismatch is detected and reported, not
  silently accepted.
- Export: round-trip a JSON export and confirm decimal strings survive exactly —
  no float coercion anywhere in the path. This is the app's core invariant and an
  export is an easy place to break it.
- Endpoint returns 403/404 for a non-owner household member.
- Follow the written restore runbook on a clean machine, end to end, and fix
  whatever it gets wrong. Then commit it.

## Out of scope

- The legacy/inheritance access mechanism from TODO #15 (inactivity-triggered
  household-member recovery, split-key arrangements). It is a real need and a
  genuinely hard design — security-sensitive, easy to get subtly wrong, and
  deserving of its own doc. Backups and the runbook are the urgent part; ship
  those first.
- Document-vault backup (TODO #7) — nothing to back up until it exists.
- Postgres streaming replication / PITR. Out of scope for a Compose deployment.
