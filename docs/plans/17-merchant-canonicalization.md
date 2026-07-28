# 17 — Smart merchant canonicalization

*(TODO.md "Next major initiatives" #6.)*

## Context

`plaid/merchant.go`'s `MerchantKey` already does real work: it strips processor
prefixes, store numbers, and a `CITY ST` tail so
`SQ *BLUE BOTTLE #4412 OAKLAND CA` and `SQ *BLUE BOTTLE #0087 BERKELEY CA`
collapse to one key. Its own comment is precise about the goal — "stability, not
a pretty name."

That is the right primitive and the wrong unit of analysis. It is a **pure string
transform with no memory**, so it cannot know that:

- `AMAZON.COM`, `AMZN.COM/BILL`, `AMZ*ORDER 1234`, and `AMAZON.COM*ABCD1` are one
  merchant. Different prefixes and suffixes survive normalization as different
  keys.
- `SAN FRANCISCO CA` leaves a stray `san` on the key — the file documents this and
  calls it harmless, which it is for *caching*, but not for *reporting*.
- `SQ *` and `TST*` fronts for the same local restaurant are the same business.

Every fragment is a separate merchant to the app. That silently degrades:

- `merchant_category_map` (`00001_core_schema.sql:227`) — the same business gets
  categorised repeatedly, and each fragment costs an LLM call the cache was
  built to avoid.
- `GetRecurringMerchants` — a subscription billing under two descriptors never
  reaches the `n >= 3` threshold and is never detected.
- Top-merchants, the recap's "biggest merchants", budgets, and the chat tool.
- Doc 22 (anomaly detection), which needs a per-merchant baseline and cannot
  build one over fragments.

Fixing this once improves every one of those as a side effect. TODO.md names it
one of the three highest-leverage starting points for exactly that reason.

## AI vs deterministic split

**Deterministic first.** Normalization and fuzzy candidate generation are pure
string work with no model involved.

**AI for the long tail only, and only as a suggestion.** The model proposes that
two keys are the same business; it never merges. This mirrors
`categorize/llm.go` exactly, where model output populates a cache the user can
override and `source` records where the answer came from. An automatic merge is
unrecoverable-looking to a user who cannot see why two merchants vanished into
one — so don't.

## Prerequisites

None. Fully parallel with everything in waves 3–4.

**Sequencing note:** doc 22 is materially better after this one, and doc 13's
obligation promotion is more accurate with it. If 22 or 13 are also in flight,
land this first where you can.

## Data model

**Reserved migration: `00023_merchant_entities.sql`.**

```sql
-- A canonical business. Many raw merchant_keys map to one entity.
CREATE TABLE merchant_entities (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id        UUID NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    canonical_name      TEXT NOT NULL,          -- "Amazon"
    default_category_id UUID REFERENCES categories (id) ON DELETE SET NULL,
    color               TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (household_id, canonical_name)
);

-- The many-to-one mapping. A merchant_key belongs to at most one entity.
CREATE TABLE merchant_aliases (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id UUID NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    entity_id    UUID NOT NULL REFERENCES merchant_entities (id) ON DELETE CASCADE,
    merchant_key TEXT NOT NULL,
    -- How this alias got here. 'suggested' rows are NOT yet active: they are
    -- pending user review and must not affect any report.
    source       TEXT NOT NULL CHECK (source IN ('manual','fuzzy','llm','suggested')),
    confidence   NUMERIC(5,4),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (household_id, merchant_key)
);

CREATE INDEX merchant_aliases_entity_idx ON merchant_aliases (entity_id);

-- Rejected suggestions must be remembered, or the suggester proposes the same
-- merge every pass and the review queue never empties.
CREATE TABLE merchant_merge_rejections (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id UUID NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    key_a        TEXT NOT NULL,
    key_b        TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (household_id, key_a, key_b)
);
```

**Do not denormalise an `entity_id` onto `transactions`.** It would need
backfilling on every merge and would drift. Join through `merchant_aliases` on
the existing `transactions.merchant_key` index. The unique constraint on
`(household_id, merchant_key)` is what keeps a key from belonging to two
entities.

**`suggested` rows are inert.** Every reporting query must treat an alias as
active only when `source <> 'suggested'`. Get this wrong and unreviewed model
guesses silently reshape the user's spending history — the exact failure this
design exists to prevent.

## Backend

### Resolution helper

One place that resolves `merchant_key → (entity_id, display_name)`, falling back
to the raw key when no active alias exists. Every consumer goes through it. An
unmapped merchant must behave exactly as it does today — this feature is additive
and a household that never opens the merge UI should see no change at all.

### Suggestion engine

A job in `backend/internal/jobs/`, producing `source='suggested'` alias rows:

1. **Deterministic pass.** Over the household's distinct `merchant_key` values:
   normalized token overlap, common-prefix length, and edit distance on the
   longest shared token. Propose only above a conservative threshold. This
   catches `amazon`/`amazon com`/`amz order` without a model.
2. **LLM pass** for the residue, via the existing `ai.Client`. Batch the
   remaining keys, ask which name the same business, and require structured
   output — the `parse.go` schema style. Cache by key pair so a pair is never
   sent twice, the same discipline `categorize/llm.go` applies.
3. Skip any pair present in `merchant_merge_rejections`.

Both passes are suggestion-only. Neither writes an active alias.

### Rewrite the consumers

Route these through the resolver:

- `GetRecurringMerchants` (`reports.sql:~200`) — group by resolved entity. This
  is the highest-value change: subscriptions split across descriptors start being
  detected. Honour `recurring_overrides` at the *entity* level once merged.
- Top merchants, the recap's merchant list, the chat tool's merchant queries.
- `merchant_category_map` — on merge, reconcile. If the fragments disagree on
  category, **prefer the `manual` one and never silently overwrite it**; the
  README's "a manual category is sticky" rule outranks any merge.

## Frontend

A **Merchants** section — either a new route or a tab in `Categories.tsx`,
whichever fits the nav better.

- **Review queue** for suggested merges: both raw strings, transaction counts,
  total spend, date ranges, and Confirm / Reject. Show the evidence; a user
  cannot judge a merge from two strings alone.
- **Manual merge and split.** Split matters — an over-eager merge must be
  reversible, and it will happen.
- **Rename** an entity's canonical name.
- Surface merged names wherever merchants render today.

## Verification

- `go test -p 1 ./...`.
- `MerchantKey` behaviour is unchanged — its existing tests in
  `plaid/merchant_test.go` must still pass untouched.
- **The inert-suggestion assertion is the important one:** create a `suggested`
  alias and assert every report is byte-identical to before it existed.
- Fuzzy pass on a fixture of real-world descriptor noise: assert the Amazon
  family merges and that two genuinely different merchants sharing a leading
  token (`SQ *BLUE BOTTLE` vs `SQ *BLUE RIDGE`) do **not**.
- Rejected pairs are never re-proposed.
- Recurring detection: a subscription split across two descriptors is undetected
  before the merge and detected after. That is this doc's headline result — test
  it directly.
- Merge reconciliation never overwrites a `manual` category.
- Frontend `tsc -b`, `vite build`, `oxlint`.

## Out of scope

- Merchant logos. A nice touch, an outbound dependency, and not worth it here.
- Cross-household shared merchant data. Everything stays household-scoped.
- Changing `MerchantKey` itself. It works; this layers on top.
