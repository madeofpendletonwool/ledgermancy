# Feature plan docs

Individual, execution-ready plans. Each doc is scoped so a single agent (or
person) can pick it up cold and implement it.

Waves 0–2 (docs 00–12) expanded Ledgermancy's AI beyond chat and are **shipped**
— they remain here as the reference for how the insight engine, preferences, and
notification contracts work.

Waves 3–7 (docs 13–33) cover **everything remaining in
[TODO.md](https://github.com/madeofpendletonwool/ledgermancy/blob/main/TODO.md)**: all sixteen "Next major initiatives" plus the two
small known gaps, the advisor initiative (wave 6), and the capstone scenario
engine. **Waves 3 and 4 are complete; wave 5 is the current cycle.** Wave 6
(the Advisor) begins after wave 5's honesty foundations land — it consumes
their inputs.

Within waves 0–2, implement in order; later docs depend on earlier ones. Waves
3+ are mostly parallel — most docs have no prerequisites at all. Read the
**Prerequisites** section of any doc before starting it, and check the migration
reservation table below before writing a migration.

### Outdated claims in TODO.md, corrected here

TODO.md's initiative descriptions were written before some of the work landed.
Where a doc's Context section contradicts TODO.md, the doc is right — it was
checked against the code:

- **Real-time insight push** is wired (`dispatchInsightPushes`, `jobs/jobs.go`),
  not the open seam TODO #1's neighbours imply.
- **Price creep** is shipped (`insights/subscription.go`), despite TODO #2
  listing it as new work. Doc 22 scopes around it.
- **Plaid reconnect / Link update mode** shipped mid-planning
  (`plaid.CreateUpdateLinkToken`); it never needed a doc.
- **There is no `dividends` category**, despite doc 14 and TODO #4 both saying
  one exists. Dividends are credited inside the brokerage account and never
  reach the bank feed, so 14 sources them from `investment_transactions` by
  subtype instead. Anything else planning to read a dividend category should do
  the same.

## The one rule every doc honors

**AI never computes; it interprets.** Detection, aggregation, and arithmetic
stay in SQL / `shopspring/decimal`. AI is used only for language, messy
classification, explaining *why* a number matters, and turning natural language
into structured intent. The model is always handed *finished* figures
(`StringFixed(2)` strings) — never asked to add, average, or divide. This mirrors
the existing `buildSummaryInput` pattern (`backend/internal/api/summary_handlers.go`)
and the chat tool layer (`backend/internal/api/chat_handlers.go`).

## Read first

- **[00-shared-contracts.md](00-shared-contracts.md)** — the three new contracts
  every feature builds on: the insight engine (`insights` table + producer
  interface), the preferences store, and the `Notifier` (ntfy) abstraction. Read
  this before any feature doc.

## Waves

### Wave 0 — Foundations (mostly non-AI; unblock everything)
- **[01-budgets-ui.md](01-budgets-ui.md)** — frontend budget management over the
  existing budget API. *No AI.*
- **[12-manual-transactions.md](12-manual-transactions.md)** — enter/edit/delete
  transactions by hand to reconcile aggregator gaps, with a duplicate guard.
  *No AI.* (Wave 0 ledger-management work; pairs with 01.)
- **[02-settings-and-preferences.md](02-settings-and-preferences.md)** — rename
  Security→Settings, add the preferences store. *No AI.*
- **[03-notifications-ntfy-delivery.md](03-notifications-ntfy-delivery.md)** —
  ntfy delivery client + wiring. *No AI.*
- **[04-insight-engine-and-feed.md](04-insight-engine-and-feed.md)** — THE SPINE:
  insights table, producer interface, generation job, feed UI. The "proactive
  insight feed."

### Wave 1 — AI producers on the spine
- **[05-subscription-intelligence.md](05-subscription-intelligence.md)**
- **[06-forecast-narration.md](06-forecast-narration.md)**
- **[07-alert-explanation.md](07-alert-explanation.md)**

### Wave 2 — depend on budgets / delivery / greenfield schema
- **[08-budget-suggestions.md](08-budget-suggestions.md)** — needs 01
- **[09-nl-alerts-and-budgets.md](09-nl-alerts-and-budgets.md)** — needs 01
- **[10-scheduled-digest.md](10-scheduled-digest.md)** — needs 02, 03, 04
- **[11-goal-coaching.md](11-goal-coaching.md)** — needs 04, 06

### Wave 3 — next major initiatives

Docs 00–12 are shipped, and so are **13, 14, 15 and 16**. Drawn from the "Next
major initiatives" section of [TODO.md](https://github.com/madeofpendletonwool/ledgermancy/blob/main/TODO.md). Unlike waves 0–2 these are
large and mostly independent. **This wave is complete.**

- **[13-bill-calendar.md](13-bill-calendar.md)** — recurring obligations
  (detected *and* manually entered), a Schedule page, day-by-day projected
  balances, safe-to-spend integration, a predicted-low-balance alert.
  *TODO #1.* No prerequisites.
- **[14-investments-page.md](14-investments-page.md)** — **shipped.** A
  dedicated Investments surface: TWR/IRR, benchmarks, allocation, fee drag, and
  the **account tax-treatment tagging 15 depends on** — `accounts.tax_treatment`
  is live (migration `00020`) and `reporting.SuggestTaxTreatment` returns a
  suggestion for confirmation, never a stored value. *TODO #4.*

  Two things 15's implementer should know. First, the doc assumed
  `investment_transactions` was populated; it was not — the table existed but
  nothing wrote to it, so `plaid.GetInvestmentTransactions` was added and
  `SyncInvestments` now stores them. Second, target allocation + drift and the
  rebalance helper were **deferred**: both need a stored per-household target
  that nothing else in the backlog wants yet, and the allocation view is honest
  and useful without them. Fee drag ships as structure plus disclosure only —
  Plaid supplies no expense ratio, so the endpoint reports full exclusion rather
  than a number computed over part of a portfolio.
- **[15-fire-projections.md](15-fire-projections.md)** — **shipped.**
  Account-aware retirement projection and a withdrawal-rate lens at
  `/retirement`, beside (not replacing) the linear model in
  `networth/project.go`. *TODO #5.* Migration `00021` is taken.

  Four things 24's and 28's implementers should know, since both build on this.
  First, the engine is `networth.ProjectRetirement` and takes `now` as a
  parameter — call it that way rather than reaching for the clock, or the tests
  become calendar-dependent. Second, IRS limits live in a versioned Go map
  (`networth/limits.go`) keyed by tax year, and an unconfigured year returns
  `ok=false` on purpose: surface it, never substitute an adjacent year. Third,
  the migration gained three columns doc 15's sketch did not have —
  `account_contributions.annual_salary` (an employer match is a percentage *of
  salary*, so a match with no salary behind it is not an amount and is refused
  at the API), and `beneficiary_current_age` / `beneficiary_target_age` for the
  529 horizon.

  Fourth and most load-bearing: **Monte Carlo did not ship as a historical
  backtest**, and nothing downstream should assume one exists. No return series
  is bundled — shipping one would mean either an outbound fetch the README
  promises against or a transcribed table of numbers nobody can verify. What
  ships instead draws sequences around the user's *own* stated real return and a
  volatility they set, seeds deterministically from the inputs, names that basis
  in every response, and is gated behind `RETIREMENT_MONTE_CARLO_ENABLED`
  (default off). Tax drag on withdrawals and RMDs are likewise **not modelled**;
  they are listed as omissions in the UI and want doc 23's income data.
- **[16-continuity-and-backups.md](16-continuity-and-backups.md)** — **shipped.**
  Automated `pg_dump`, document-vault archive, portable JSON export, and a
  **verified restore test**; a Continuity panel at Settings → Continuity
  (owner-only); a tested restore runbook in `DEPLOYING.md` §7. *TODO #15;
  resolves the open HIGH audit finding.* Migration `00035` is taken.

  Four things later waves need to know, because three of them change how you
  write a doc:

  **The backup runs in the worker, not a sidecar.** The doc specified a
  `pg_dump` sidecar in `docker-compose.prod.yml`; it is River periodic jobs in
  the existing worker instead. The deciding reason was not tidiness — the
  restore test compares row counts against the live database, reads the coverage
  registry to know which tables to compare, and writes `backup_runs` — so a
  shell sidecar would have called back into the app anyway while adding a
  container to keep running and patched. The worker image pins
  `postgresql17-client`; **upgrading Postgres now means changing two lines**,
  the compose image and that package.

  **Off-host push is a directory, not S3.** `BACKUP_MIRROR_DIR` is copied to
  after every artefact. No object store and, deliberately, **no second
  encryption key**: the dump's Plaid tokens and every document byte are already
  sealed with `ENCRYPTION_KEY`, and a `BACKUP_ENCRYPTION_KEY` would add a second
  thing to lose to the one doc whose whole thesis is reducing loss surfaces. If
  a *third-party* destination is ever added, client-side encryption becomes
  mandatory and that trade has to be revisited.

  **Every table you add must be classified, and the build enforces it.** See the
  Continuity rule below — this is the part that affects every wave-5 doc.

  **An instance hosts exactly one household**, by construction: the first user
  bootstraps it and every registration after is invite-only into it. The export
  relies on this rather than carrying a per-table household filter, and asserts
  it at the download endpoint rather than assuming it. A doc that makes an
  instance multi-household owes that endpoint a scoping story.

### Wave 4 — foundations (no prerequisites, all parallel-safe)

**This wave is complete.** 16 was deliberately held until 18 shipped so the
document vault could be part of the backup rather than a later amendment — it
is, and the restore test opens a document end to end to prove the dump, the
archive and `ENCRYPTION_KEY` all agree.

- **[17-merchant-canonicalization.md](17-merchant-canonicalization.md)** —
  **shipped.** Fragmented merchant strings map to canonical entities at
  `/merchants`, suggestion-then-confirm. *TODO #6.* Migration `00023` is taken.

  Four things doc 22's implementer should know, since it builds on this. First,
  **the resolution rule lives in SQL and only in SQL** — it is documented in full
  at the top of `db/queries/merchants.sql` and every consumer inlines the same
  two LEFT JOINs. A Go-side `Resolver` was written and then deleted: nothing
  needed it, and a second implementation of the rule is a thing to keep in step
  for no benefit. Copy the SQL block; do not reintroduce a Go resolver unless a
  caller genuinely cannot do the join.

  Second, **the resolved key is an entity UUID rendered as text**, and raw keys
  pass through unchanged. Resolution is idempotent (no alias is ever keyed by a
  UUID), which is what lets `recurring_overrides` and
  `recurring_obligations.merchant_key` hold either form and still match —
  including rows written before a merge. `GetRecurringMerchants` returns the
  resolved key, so anything storing that key gets an identifier that survives
  further descriptors joining the merchant. Baselines built per entity should key
  the same way.

  Third, the doc's schema shipped as written, plus a `key_a < key_b` check
  constraint on `merchant_merge_rejections` so a pair has one representation, and
  a partial index on active aliases. The one behaviour the doc did not specify:
  **a component containing any rejected pair is dropped whole**, not split around
  the refusal. Transitivity would otherwise re-form a rejected merge through a
  third descriptor.

  Fourth, one consumer outside the doc's list needed fixing and got it:
  `UnusualMerchantCandidates` (`alerts.sql`) judged "first ever seen" per raw
  descriptor, so a merged merchant's second descriptor fired a false new-merchant
  alert. It now resolves. Anything else asking "have we seen this merchant
  before?" must do the same.

  Entity logos are out of scope and stayed out — an outbound dependency the app
  otherwise does not have.
- **[18-document-vault.md](18-document-vault.md)** — **shipped.** Encrypted
  document storage over the existing AES-GCM cipher, linked to
  transactions/assets/goals, with expiry nudges. *TODO #7.* Migration `00024` is
  taken. **Doc 23's storage dependency is satisfied** — the storage layer,
  encryption and upload/download endpoints are live, so 23 can build its
  paystub-specific schema on top of `documents` today.

  Five things doc 23's implementer (and anyone touching the vault) should know.

  First, **the storage layer is a `Storage` interface with two implementations**
  (`documents/storage.go`, `documents/s3.go`) and a `Vault` on top that owns the
  cipher. Nothing outside the package knows documents are encrypted: callers
  hand `Vault.Store` plaintext and get a storage key back. A paystub importer
  should reuse `Vault` rather than sealing anything itself.

  Second, **S3 shipped without an SDK.** SigV4 is signed by hand in `s3.go`,
  because the vault needs three verbs against one bucket and aws-sdk-go-v2 is a
  large tree for that. It is tested for encoding rules, header set, determinism
  and coverage, not against a live endpoint — if you extend it beyond
  PUT/GET/DELETE, extend those tests first.

  Third, **the `mime_type` column is display metadata and nothing else.** The
  download path sniffs the decrypted bytes against a five-entry allowlist and
  falls back to `application/octet-stream`; the stored claim never reaches a
  response header. Anything that adds a serving path must do the same, or the
  HTML-as-receipt hole reopens.

  Fourth, **`document_links` has four typed nullable target columns with a
  one-of CHECK**, not a `(kind, id)` pair, so the foreign keys are real. Adding
  a fifth target kind means a migration *and* a `Target*InHousehold` ownership
  query — the insert cannot verify a target and deliberately does not try.

  Fifth, **OCR shipped as suggestion-only and gated on `doc_type`, and doc 23
  must not casually widen either.** There is no code path from `ExtractReceipt`
  to a written row; the endpoint returns fields plus candidate transactions for
  a person to act on. Eligibility is `documents.OCREligible`, an allowlist
  containing `receipt` and nothing else, checked *before* the bytes are
  decrypted — so a tax document cannot be sent whatever file format it is in.

  This is the one thing doc 23 has to think hardest about, because a paystub is
  more sensitive than a receipt and more sensitive than most tax documents: it
  carries an employer, a full name, frequently a partial SSN, and exact gross
  and net pay. Adding `paystub` to `ocrEligibleTypes` and moving on would be the
  wrong instinct. The options worth weighing are a per-document opt-in rather
  than a type-wide one, redaction before upload, or a local OCR path with no
  outbound call at all. Whichever it is, it deserves its own switch rather than
  inheriting `DOCUMENTS_OCR_ENABLED`, which a user turned on to read grocery
  receipts. `ai.Block` gained image support (`ImageBlock`) and is available —
  under that rule.
- **[19-debt-payoff-goals.md](19-debt-payoff-goals.md)** — the `debt_payoff`
  goal kind the schema has always allowed, plus thousands separators in
  `money()`. *TODO known gaps.* **Shipped.** No migration, as advertised.
  `goals.ComputePayoff` sits beside `Compute` and `money()` now delegates to the
  shared `internal/moneyfmt`. Three notes for anyone touching it. The doc's
  iteration-cap warning needed extending: a payment one cent above the interest
  *does* amortize, over millions of months, so anything past a 100-year horizon
  is reported as "never" too. Two callers were quietly applying savings maths to
  every goal and had to be taught to skip payoff ones — `insights/goal.go` and
  `reporting/safetospend.go`, the latter because a debt's payment is already a
  bill in the cashflow it reserves against. And the feature was dead on arrival
  until a *separate* bug was fixed: the Plaid sync gated Liabilities on
  `plaid_items.products`, a link-time snapshot, so the `liabilities` table was
  empty for every existing item. See `internal/plaid/modules.go`.
- **[20-pwa-offline.md](20-pwa-offline.md)** — installable PWA with read-only
  offline. *TODO #11.* **Shipped.** Frontend only, as advertised. The MVP
  landed; write queueing did not, and was closed rather than deferred — writes
  are refused with an explanation instead. Two notes for anyone touching it:
  the cacheable API set in `frontend/src/sw.ts` is an allowlist, so a new
  read-only endpoint needs adding there before it works offline, and sign-out
  must keep clearing the worker's caches (`clearApiCache`) or a shared device
  leaks the previous user's figures.
- **[21-household-sharing.md](21-household-sharing.md)** — **shipped.** The
  organizing idea is that a **person** and a **login** are different things:
  `household_people` carries a name, a birthdate and an optional `user_id`, so a
  six-year-old with a 529 exists without credentials. Accounts and manual assets
  gain the person they are *for*; custodial treatments (`utma_ugma`,
  `coverdell`, `custodial_roth`, `trump`) join `529` in being segregated from the
  retirement nest egg. Kid logins are opt-in on top of that (`users.role`), with
  an allowance ledger and person-scoped goals as the teaching surface. Shared-goal
  contributions and the bill-split ledger are unchanged from the first draft.
  *TODO #9.* Doc 16's admin check should consume this role.

  **The birthdate is load-bearing beyond kids.** The app currently stores ages —
  `projection_assumptions.current_age` and
  `account_contributions.beneficiary_current_age` — which are correct once and
  wrong every year after, and `networth/limits.go` already gates catch-up
  contributions on an age it has no reliable source for. Neither column is
  dropped; both become the fallback behind a derived age. Anything needing an age
  after this doc lands resolves birthdate → stored integer → `ok=false`, and
  derives against `ProjectRetirement`'s `now` parameter rather than the clock.

  Six things anyone touching this should know.

  First, **migration numbering broke and was fixed here.** `db.Migrate` runs
  goose in strict-ordering mode, so a `00025` could never apply to an instance
  already at `00033` — it fails at startup with `found N missing migrations`.
  This doc took `00034` and every remaining reservation was reissued above
  `00033`. `goose.WithAllowMissing()` would also have silenced it and was
  deliberately not used; see the note on `db.Migrate`.

  Second, **the role guard is mounted on route groups, never per handler**
  (`auth.RequireAdult` / `RequireOwner`). `Routes()` was split into
  `routesWithAuth` so `role_enforcement_test.go` can mount a stub identity and
  assert **every** route individually — 169 subtests, and it fails loudly if a
  new top-level group forgets the guard. Adding a route means deciding which
  group it belongs in; there is no third option. The one legitimate per-handler
  check is `handleUpsertPreferences`, because that resource is split by scope
  rather than by URL.

  Third, **the sign convention on `allowance_entries` is inverted** relative to
  `transactions.amount`: positive means money INTO the child's balance. It is a
  balance, not a spend feed. Nothing joins the two tables and nothing should —
  `TestAllowanceEntriesDoNotChangeHouseholdSpend` is the guard.

  Fourth, **three double-counting invariants are asserted with exact decimals**
  (`split_invariants_test.go`): splitting a transaction, adding allowance
  entries, and tagging a beneficiary each leave the corresponding household
  total byte-identical. These are the tests that make the rest trustworthy.

  Fifth, **age resolution is birthdate → stored integer → `ok=false`**
  (`networth.ResolveAge`). The backfill leaves every existing person's birthdate
  NULL precisely so an upgraded instance produces identical projections until
  somebody enters one — `TestResolveAgeIsUpgradeSafe` asserts that directly.
  Self-service editing lives at `PUT /api/me/person`, surfaced as a Profile tab
  in Settings, because `UpdateUserProfile` existed in `users.sql` with no handler
  and there was no profile-editing endpoint in the app at all.

  Sixth, **custodial money is excluded from the retirement nest egg.**
  `networth.IsCustodial` covers `529`, `utma_ugma`, `coverdell`,
  `custodial_roth` and `trump`. A UTMA is irrevocably the child's property, so
  counting it as household retirement savings overstates the position by the
  whole balance — in the flattering direction, which is why nobody catches it.

### Wave 5 — foundations & honesty (no advisor yet)

Every downstream projection must be trustworthy before it advises on it. This
wave is the honesty/income/manual-accounts layer the advisor (wave 6) builds
on. **Doc 24 (the proactive advisor) is held for wave 6** by user direction —
"wave 5 done before the proactive advisor is touched" — so the ranker and the
allocation planner both land on real contribution headroom (23), non-drifting
asset values (26), and honest long-horizon dollars (27).

- **[22-anomaly-detection.md](22-anomaly-detection.md)** — **shipped.**
  Per-merchant outliers and duplicate charges, on the existing insight spine.
  Migration `00046_anomaly_overrides.sql` is taken. *TODO #2.*

  Read that doc's shipped notes before touching this area — five of its own
  claims were wrong against the code and are corrected there. The two that reach
  outside the doc: **there is no `merchant_baselines` table** (a stored median or
  p95 cannot be made leave-one-out, so the baseline is computed on demand, and
  nothing downstream should expect a cached one), and **`large_transaction` now
  yields to `merchant_outlier`** on any merchant with 5+ prior charges, so the
  two are halves of one behaviour rather than independent producers.
- **[23-paystub-income.md](23-paystub-income.md)** — the biggest hole in the
  data model: 30–45% of gross income is invisible today. Paystub schema, three
  ingest paths, paycheck breakdown, contribution limits, tax-prep summary.
  *TODO #8.* **18 has shipped**, so its dependency is met — read 18's notes
  above before choosing where paystub files live.
- **[25-in-app-digest.md](25-in-app-digest.md)** — **shipped.** The digest is
  persisted to `digest_entries` and surfaced at `/digest`; push and SMTP are now
  two optional surfaces beside it rather than the only ones. Migration
  `00049_digest_entries.sql` is taken. *TODO #10.*

  Read that doc's shipped notes before touching this area. The one that reaches
  outside the doc: **reserve-ahead migration numbers no longer work.** These docs
  have shipped out of order, the schema is already past most of the numbers they
  reserve, and goose refuses to apply a migration below the current version — so
  a doc's reserved number would fail every existing deployment at boot. Every
  unshipped doc here should take the next free number, not the one it names.
- **[26-real-asset-revaluation.md](26-real-asset-revaluation.md)** — asset
  classes, depreciation curves, value history, asset↔loan equity, **and
  directly-held bonds**. *TODO #14.* Brokerage-held bonds already work through
  Plaid holdings; what has no home is TreasuryDirect — Series I and EE savings
  bonds, which sit in `manual_assets` as a frozen number while their real value
  accrues monthly against published rates. They are the one asset class here
  whose correct value is arithmetic rather than an estimate, which is why they
  are the single exception to this doc's "an estimate is a proposal, never a
  write" rule. Soft tie to 21: savings bonds for a child attach through
  `manual_assets.person_id`.
- **[27-inflation-adjusted-views.md](27-inflation-adjusted-views.md)** — a CPI
  series and a real/nominal toggle. *TODO #12.* Small and self-contained; good
  candidate to bundle with 14 or 15.
- **[30-manual-accounts.md](30-manual-accounts.md)** — accounts without Plaid,
  per-holding manual investment tracking with full Investments-page parity
  (TWR/MWR, allocation, dividends, snapshots), and auto-posting scheduled
  transactions that also adjust the manual balance. Closes the gap docs 12, 13,
  and 26 deliberately left open: a manual Voya retirement account can replace a
  broken Plaid link end-to-end. Migration `00047_manual_accounts.sql` is taken.
  No prerequisites beyond the shipped docs 12, 13, 14.

### Wave 6 — the Advisor

The advisor initiative: turn the `Assistant` route into an `Advisor` route,
expose the existing deterministic engines to the chat, ship the proactive
ranker, and build the multi-bucket allocation planner with a Monte Carlo
likelihood layer. Built on wave 5's honest inputs. The whole initiative has a
visual companion — **[advisor-overview.html](advisor-overview.html)** — with
live demos of the options ranker and the bucket allocator.

The four docs sequence W1→W4 within the wave: surface first (cheap, biggest
coverage gain), then the ranker, then the allocator, then the likelihood layer.

**All four docs were reviewed against the code before wave 5 started, and all
four were corrected.** The review found two classes of problem worth naming
here, because they generalize:

**Named APIs that did not exist.** Doc 32 twice instructed the implementer to
delegate to `ProjectByAccount`, which is not in the tree — the real engine is
`networth.ProjectRetirement`, whose `RetirementPoint.ByAccount` map supplies
the per-account series. It also called `limitGroup` from a new package, but
that function is unexported. A doc whose load-bearing instruction is "call this
exact function, do not fork it" is only as good as the name, and neither had
been checked.

**Assumptions that flatter.** Three of them, all in doc 33, all moving the
headline number in the direction that makes a plan look good: independent
per-bucket return draws (which under-counts correlated equity risk and inflates
every success rate), an arithmetic-vs-geometric mean gap that would have put
doc 32's and doc 33's "P50" ~15% apart on the same card, and a worst-case
drawdown defined as a maximum over runs — an unstable statistic that would let
the run count change which plan the guardrail picks. Each is now fixed in the
doc with a test that fails if the flattering version is implemented.

The largest *capability* gap the review found was not a bug in any doc but an
absence across all four: **unclaimed employer match**, the highest guaranteed
return available to most households and computable from data the app already
holds (`annualMatch`, `retirement.go:394`). It is now tier 2 of doc 24's
waterfall. The second was **contribution eligibility** — `AnnualLimitFor`
returns a cap, and a cap is not permission; a household over the Roth MAGI
phase-out was going to be shown a $7,500/yr plan it is not allowed to execute.
Doc 32 now owns an eligibility table beside `limits.go`, and doc 31's
`households.filing_status` exists to key it.

- **[24-proactive-advisor.md](24-proactive-advisor.md)** — ranked, deterministic
  options for surplus cash; the model only narrates. *TODO #3.* **Needs 13 and
  15** (both shipped) and the wave-5 honesty docs for its allocation-flavoured
  options. The single-pick ranker ("you have $X — here are the options") and
  the multi-debt avalanche/snowball strategy ship here. Its ranking rule is now
  an explicit **waterfall** (starter EF → employer match → debt above a hurdle
  derived from the household's own assumed return → full EF → expiring
  tax-advantaged headroom → goals → everything below the hurdle as a stated
  tradeoff). The first draft's "guaranteed return first" would have paid down a
  3.5% mortgage ahead of a Roth and drained an emergency fund into a card.
- **[31-advisor-surface.md](31-advisor-surface.md)** — rename Assistant →
  Advisor, expose ~12 existing engines as chat tools (no new math), build the
  Briefing/Horizon/Assumptions/Threads shell around the chat, add household
  profile fields and conversation/action-item persistence. The cheapest,
  highest-leverage doc in the wave.
- **[32-allocation-planner.md](32-allocation-planner.md)** — the multi-bucket
  allocator: split a lump and/or monthly surplus across Roth/529/brokerage/
  debt/EF with per-bucket projection, contribution-cap enforcement
  (`limits.go`), goal-mapping, cash-drag detection, asset-location, and
  college-cost projection. The thing a real advisor does that the app can't.
- **[33-likelihood-layer.md](33-likelihood-layer.md)** — Monte Carlo over
  return distributions (sharing doc 15's seeding/RNG machinery, though the
  accumulation loop is genuinely new rather than a generalization),
  P10/P50/P90 + success rates, a documented guardrail rule that lets the AI
  name a top pick from computed likelihoods, and plan-vs-actual tracking that
  closes the loop. **Also hard-depends on doc 30** — `Reconcile` reads
  `account_balance_history` — which the first draft did not list.

### Wave 7 — capstone / far future

- **[28-decision-modeling.md](28-decision-modeling.md)** — the what-if scenario
  engine: rent-vs-buy with opportunity cost, retirement stress tests,
  solve-for-X, job-offer comparison. *TODO #16.* **Hard dependency on 15**;
  strongly wants 13, 23, 26, 27. Ship the engine plus one or two families first.
- **[29-multi-currency.md](29-multi-currency.md)** — currency on transactions,
  FX rates, conversion at aggregation time. *TODO #13.* Highest blast radius in
  the backlog and worth nothing to a US-only user — read its "should you be
  doing this" section first.

## Standard template

Every feature doc has these sections: **Context** (problem + why) · **AI vs
deterministic split** (explicit) · **Prerequisites** (which docs) · **Data model**
(tables/columns) · **Continuity** (see below) · **Backend** (queries, jobs, API —
naming concrete reuse paths) · **Frontend** (routes/components, capabilities
gating) · **AI notes** (prompt/tooling, where applicable) · **Verification**
(seed sandbox data, drive endpoint, cross-check psql, `go build/vet/test`,
frontend `tsc/build/lint`) · **Out of scope**.

### The Continuity rule

**Every new table must be classified, and every new volume must be accounted
for.** A doc that adds either owes one line saying which.

This is not a convention you can forget: `backend/internal/continuity/coverage.go`
holds the registry and `coverage_test.go` fails `go test ./...` — with no
database needed, so it fires on a laptop — the moment a migration creates a table
nobody has classified, or `docker-compose.yml` declares a volume nobody has
accounted for. The failure message names the file to edit and the four
categories:

| Category | Use when |
|---|---|
| `InExport` | User data — created, decided, or accumulated, and not re-derivable. Goes in the dump **and** the portable export, and is automatically enrolled in the restore test's row-count check. |
| `DumpOnly` | Credentials, or bookkeeping meaningless outside this app. In the dump, deliberately absent from the portable export. |
| `Derived` | A job already rebuilds it. Name the job in the comment. |
| `Ephemeral` | Restoring it would be *wrong*: sessions, challenges, queue rows. |

Classifying a table `InExport` is all it takes — the export walks the registry,
so a wave-5 table is dumped, exported and restore-verified without editing
anything else. If it is `InExport` it also needs to survive the export's
invariants: money is cast to text in SQL (never a JSON number), and binary
columns are withheld by type, so an encrypted secret should be `BYTEA` and will
then be excluded automatically.

Durable state that is **not** in Postgres — a new blob store, a cache with real
content, an upload staging area — goes in the same file's `blobStores` registry
*and* needs a capture step in `archive.go`; a separate test fails when only one
of the two exists. The document vault is the precedent: it was nearly shipped
without a backup story because the backup story predated it.

The reason this is mechanical rather than advisory: a feature shipping with data
nobody backs up is invisible until a restore, at which point it is permanent.
Making it a build failure moves the discovery to the pull request.

## Environment notes for implementers

- Local dev stack is `docker compose up -d --build`; Plaid is in **sandbox** and
  AI is enabled (GLM-4.6 via z.ai, Anthropic-compatible). The stack already has
  ~102 sandbox transactions for testing.
- Throwaway Postgres for tests:
  `docker run -d --name lmtest-pg -e POSTGRES_PASSWORD=test -e POSTGRES_DB=lmtest -p 55432:5432 postgres:17-alpine`
  then `TEST_DATABASE_URL='postgres://postgres:test@localhost:55432/lmtest?sslmode=disable' go test -p 1 ./...`
- Migrations are numbered, and **`db.Migrate` runs goose in strict-ordering
  mode**. That is the constraint everything below follows from: a new migration
  must be numbered **above every version already applied**, or an instance that
  has run the higher one refuses to start with
  `found N missing migrations before current version`.

  **`00046_anomaly_overrides.sql` (doc 22) is the latest.** Applied before it:
  `00001`–`00021`, `00023`, `00024`, `00033`–`00035`, and `00043`–`00045`
  (`00043_account_terms`, `00044_loan_account_outflow`,
  `00045_one_time_transactions` — the last two are out-of-wave bugfixes that
  landed after this table was last written).

  **The old `00022`–`00032` reservations are void and have been reissued
  above `00033`.** They were allocated below `00033` before doc 18's follow-up
  took that number, which left every one of them unusable — applying a `00025`
  to an instance already at `00033` fails outright. Doc 21 hit this and
  renumbered; the rest are renumbered here so nobody hits it again.
  `goose.WithAllowMissing()` would also have silenced it and was deliberately
  **not** used: it trades away "the schema is a function of the version number"
  for a problem that renumbering solves outright.

  **The `00036`–`00042` reservations (docs 22, 23, 25–29) were void for the same
  reason, and have now all been reissued** in the table below. Doc 22 went first
  and took `00046`. `00043_account_terms.sql` is an out-of-wave bugfix — the
  Goals payoff picker listed no debts for a household that had three, because it
  gated on Plaid having served loan terms rather than on the account being a
  debt — and it had to take a number above everything applied.

  **Leaving them un-numbered turned out to be the more expensive choice, and it
  nearly collided.** Wave 6 (docs 31–33) reserved `00048`–`00050` while wave 5's
  reissues were still unassigned — and wave 5 ships first, so the first wave-5
  implementer to need a migration would have reasonably taken `00048` and landed
  on doc 31. Every remaining doc now has a concrete number, allocated in ship
  order: wave 5 takes `00047`–`00051`, wave 6 takes `00052`–`00054`, wave 7 takes
  `00055`–`00056`. **Docs 23 and 25–29 still carry their old numbers inline in
  their own text; this table is authoritative — check here before writing a
  migration, not there.**

  To avoid the collision class that already bit this repo once (two `00007`s),
  each doc that needs a migration has a **reserved number**. A reservation is a
  claim on a name, not a guarantee: before writing one, check it is both still
  free **and still above the highest applied version**, and renumber (+ update
  this table) if either has stopped being true.

  | Migration | Doc | Adds |
  |---|---|---|
  | ~~`00019_recurring_obligations.sql`~~ | 13 | `recurring_obligations` table — **taken** |
  | ~~`00020_investment_analysis.sql`~~ | 14 | `accounts.tax_treatment`, `investment_snapshots`, `asset_prices` — **taken** |
  | ~~`00021_projection_assumptions.sql`~~ | 15 | `projection_assumptions`, `account_contributions` — **taken** |
  | ~~`00023_merchant_entities.sql`~~ | 17 | `merchant_entities`, `merchant_aliases`, `merchant_merge_rejections` — **taken** |
  | ~~`00024_documents.sql`~~ | 18 | `documents`, `document_links` — **taken** |
  | ~~`00033_document_extractions.sql`~~ | 18 | `documents.extracted_*` — **taken** (follow-up to 18) |
  | ~~`00034_household_people_and_splits.sql`~~ | 21 | `household_people`, `users.role`, `accounts.beneficiary_person_id`, `manual_assets.person_id`, `allowances`, `allowance_entries`, `goal_contributions`, `transaction_splits`, `goals.person_id` — **taken** |
  | ~~`00035_backup_status.sql`~~ | 16 | `backup_runs` — **taken** |
  | ~~`00043_account_terms.sql`~~ | (bugfix) | `account_terms` — **taken** |
  | ~~`00046_anomaly_overrides.sql`~~ | 22 | `anomaly_overrides` table — **taken**. Note the scope changed: no `merchant_baselines` table ships, because a stored median/p95 cannot be made leave-one-out. See doc 22's shipped notes. |
  | ~~`00047_manual_accounts.sql`~~ | 30 | `accounts.source`/`user_id`/`is_shared`/`household_id`, `account_balance_history`, `securities.source`/`ticker_key`, `investment_transactions.source`, `recurring_obligations.auto_post`/`last_posted_date`/`posting_account_id` — **taken** |
  | ~~`00048_paystubs.sql`~~ | 23 | `employers`, `paystubs`, `paystub_lines` — **taken**. Two additions to the schema doc 23 prints, both load-bearing: `paystub_lines.is_employer` (without it a 401(k) match is summed as a deduction and every stub carrying one fails the doc's *own* `gross − Σdeductions = net` rule) and `employers.pay_frequency` (the "N pay periods left" figure divides by it, and inferring a cadence from stub gaps fails hardest on a new job — which is when the question matters most). `employers.ein` is stored sealed, as `ein_encrypted BYTEA`. **This landing before `00047` means doc 30 must ship first or renumber above it** — see doc 23's shipped notes. |
  | `00049_digest_entries.sql` | 25 | `digest_entries` table |
  | `00050_asset_revaluation.sql` | 26 | `asset_details` (incl. bond columns), `asset_valuations`, `savings_bond_rates`, `manual_assets.loan_account_id` |
  | `00051_cpi_series.sql` | 27 | `cpi_series` table |
  | `00052_advisor_surface.sql` | 31 | `households.filing_status`/`risk_drawdown_floor`, `advisor_threads`, `advisor_messages`, `advisor_action_items`. (`households.state` was dropped from this doc — no wave-6 engine consumed it; see 31.) |
  | `00053_allocation_planner.sql` | 32 | `accounts.deposit_apy`, `projection_assumptions.college_inflation_rate`, `goals.kind='college'`, `allocation_plans` |
  | `00054_likelihood_layer.sql` | 33 | `plan_trackings` |
  | `00055_scenarios.sql` | 28 | `scenarios` table |
  | `00056_multi_currency.sql` | 29 | `*.currency` columns, `households.base_currency`, `fx_rates` |

  Docs **19, 20, and 24 need no migration.** Wave 3+ docs run in parallel, so
  **these reservations are load-bearing** — take only your own number, and only
  after confirming it still sits above everything applied.

  One pair can still genuinely collide and needs coordination rather than just
  distinct numbers:

  - **21 → 16.** 16 needs an owner/admin check and observes that no role column
    exists; 21 adds `users.role`. Whichever lands second adopts the other's
    mechanism instead of inventing a parallel one.
  - **21 → 15 (shipped).** 21 changes where an age comes from. It drops neither
    `projection_assumptions.current_age` nor
    `account_contributions.beneficiary_current_age` — both become the fallback
    behind a birthdate — but `ProjectRetirement` and `AnnualLimitFor` change
    which source they prefer. Read 21's "Ages come from birthdates" before
    touching either.
  - **21 ↔ 26.** Both widen `manual_assets`, in different directions and without
    overlap: 21 adds `person_id`, 26 adds `loan_account_id` plus the bond side
    table. Distinct migration numbers are sufficient; no coordination needed
    beyond not assuming the other has landed.

  (The 14 → 15 pair is resolved: 15 shipped reading `accounts.tax_treatment`
  from 14's `00020` and added no copy of it.)

  Docs 01, 05–09, and 12 needed **no** migration (UI, new queries, or reuse
  only). All migrations goose-annotated (`-- +goose Up` / `-- +goose Down`).
- Regenerate DB code after query/schema changes:
  `go run github.com/sqlc-dev/sqlc/cmd/sqlc@latest generate` (from `backend/`).
