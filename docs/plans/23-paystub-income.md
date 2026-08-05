# 23 — Pre-tax income & deduction tracking (paystub importer)

*(TODO.md "Next major initiatives" #8. The largest doc in the backlog.)*

**Shipped.** Migration `00048_paystubs.sql` — **not** the `00037` this doc names
below, which was void; the table in [README.md](README.md) is authoritative.
Backend in `internal/payroll/`, HTTP under `/api/payroll`, frontend at
`/paystubs`.

Nine things anyone touching this should know. Six are places this doc was wrong
against the code, and one is a hazard it could not have known about.

**`00048` lands in a gap, and doc 30 has to move.** `00047_manual_accounts.sql`
is reserved but unwritten. Goose runs strict-ordering, so once `00048` has been
applied to an instance a later `00047` is refused outright. Doc 30 must ship
before this reaches a live database, or renumber above it. There is no way to
fix this from inside this doc's code; the reservation table is the only
coordination point.

**The schema as printed below cannot balance a normal paystub.** This doc's own
verification rule is `gross − Σ(deduction lines) = net`, and `paystub_lines` as
specified has no way to distinguish a deduction from an employer contribution —
so the `401k_employer_match` category it also specifies gets summed as a
deduction and every stub carrying a match reads as short by exactly the match.
That is the most common paystub in America. `paystub_lines.is_employer` is the
fix: employer lines are excluded from the balance equation and are the entire
basis of the total-compensation figure. `TestBalanceExcludesEmployerLines` pins
it.

**`employers.pay_frequency` is stored, not inferred.** "You're $15,500 from
maxing your 401k, 6 pay periods left" divides by a cadence, and inferring one
from the gaps between stubs fails hardest in the case where the question is most
urgent — a new job, with one stub on file and no history to infer from. The
remaining-periods count is then derived from the CALENDAR rather than from
"periods per year minus stubs entered", because a household that starts using
this in June and enters three stubs has not been paid three times this year.

**"Scope reads to the owning user by default" needed a column, and the doc did
not give it one.** `paystubs.is_shared` defaults to **FALSE** — the one place
this app inverts its sharing default, since `plaid_items.is_shared` and
`documents.is_shared` both default TRUE. Read visibility and write permission
are separate: a shared stub is visible to the household and editable only by its
owner, resolved through `GetOwnedPaystub`, and a stub belonging to somebody else
404s rather than 403s. `employers.ein` is stored as `ein_encrypted BYTEA` under
the same key as Plaid tokens, returned in full by the tax-summary endpoint alone
and masked everywhere else.

**No AI path was built, and `ocrEligibleTypes` was not touched.** The doc's own
"Extraction without an upload" section is right and route 1 turned out to be
enough: `internal/payroll/pdftext.go` reads the text layer that ADP/Gusto/
Paychex/UKG already put in the PDF — stdlib only (`compress/zlib`), no new
dependency, no network, no model. `paystubs.source` is `'pdf'` rather than the
doc's `'ocr'` for that reason. A stub with no text layer is reported as such and
typed in; a font with a custom encoding is detected and refused rather than
guessed at, because a gross salary derived from mis-decoded glyphs is worse than
"we could not read it". The direct-upload endpoint **stores nothing** — the
bytes are parsed in memory and dropped — and a PDF already in the vault can be
parsed locally over its decrypted bytes, which involves no third party and so is
not gated on `DOCUMENTS_OCR_ENABLED`.

**Pre-tax is not one thing, and the W-2 mapping is wrong if you treat it as
one.** A 401(k) deferral comes out before federal income tax and NOT before
FICA; a Section 125 cafeteria item comes out before both. That is why box 1 and
boxes 3/5 differ on every real W-2 of anybody contributing to a 401(k), and a
summary that got it wrong would disagree with the form it exists to help check —
in a way that looks like the employer made the mistake. `payroll.WageBasis`
carries the distinction per category; the stored `pre_tax` flag decides whether
a line reduces anything at all, the category decides what it reduces.

**Deposit reconciliation ranks rather than matches.** An exact-amount matcher
finds nothing for a household whose direct deposit is split between checking and
savings, which is extremely ordinary. Candidates come back ordered by distance
from net with the gap shown, and a deposit already claimed by another stub is
not offered at all — so the doc's two-identical-deposits case cannot even be
mis-offered, let alone auto-matched. Nothing writes the link except a request in
which a human named the transaction.

**Year-to-date totals take the LARGER of the printed YTD and the sum of the
periods on file.** Each is wrong in a different direction — the sum is short for
a partial history, the printed figure is short after a payroll-provider change —
and understating a contribution total is the expensive direction, because it
reports headroom that is not there. The 401(k) elective limit is pooled across
employers, so the doc's mid-year-job-change case works;
`TestMidYearEmployerChangePoolsOneLimit` is the guard.

**The gross-based savings rate was added alongside the net one, never replacing
it**, exactly as the doc asks: `MonthlySummaryInput.GrossSavingsRate` is a
distinct field with its own prompt line, and `/api/payroll/savings-rate` returns
both figures side by side with a warning when the stubs on file only partly
cover the window.

**Not built:** Plaid Payroll Income (needs the Income product enabled on the
item, and there is nothing to test against), the local-OCR sidecar for genuinely
scanned stubs, and any AI extraction path. Manual entry and PDF text extraction
cover the population between them; the sidecar is the next thing to add if
scanned stubs turn out to be common.

## Context

The app only ever sees money that has already survived the gross-to-net
transformation. Every transaction Plaid syncs is post-tax, post-deduction, so the
app is blind to the single largest claim on most users' income: withholding,
retirement contributions, insurance premiums, HSA/FSA, garnishments. For a
typical W-2 earner **30–45% of gross income is invisible.**

That undercuts the core promise directly. The spending side tracks every post-tax
dollar competently; the income side tracks only the residual that lands in
checking. Concretely, it means:

- "Savings rate" is measured against net, not gross — a materially different and
  flattering number.
- Doc 15's retirement projection cannot see 401k contributions or an employer
  match, which are usually the largest wealth-building flows in the household.
- The app cannot answer "what's my effective tax rate" or "am I on track to max
  my 401k", both of which it otherwise has every input for.

Closing it also makes the app a genuine tax-filing **companion** — not a filer.
With paystub lines plus the document vault, the app holds nearly everything that
goes on a 1040. The goal is "the data-gathering is already done", not "we file
for you."

## AI vs deterministic split

**Deterministic:** every total, rate, limit check, and reconciliation. Effective
tax rate is arithmetic over stored lines.

**AI:** OCR extraction from a paystub PDF, and **only as a proposal**. The parsed
paystub is always queued for user review before it is written — never
auto-applied. Same discipline as `categorize/llm.go` and doc 17. A model
misreading a YTD figure that then flows into a tax summary is exactly the failure
this rule exists to prevent.

**But read the next section before assuming AI is how the extraction happens at
all.** This doc was written before doc 18 shipped, and 18's OCR gate deliberately
refuses everything except receipts. Sending a paystub to a third party is a
materially different act from sending a grocery receipt, and "add `paystub` to
the allowlist" is the wrong instinct.

### Extraction without an upload — prefer this

A paystub and a W-2 are **fixed-layout documents**, and that changes the problem
entirely. Commercial tax software has peeled the fields off a W-2 for years
without a vision model, because a W-2 has numbered boxes in known positions and
a payroll provider's stub template does not change between pay periods. That is
a parsing problem, not a perception one.

Three routes, in order of preference:

1. **Text-layer extraction.** A paystub PDF from ADP, Gusto, Paychex or UKG is
   generated, not scanned — the text is *already in the file* as text. Pulling
   it out is a local PDF text extraction, no model and no network. Combined
   with per-provider label patterns ("Gross Pay", "Federal Income Tax",
   "401(k)"), this handles the majority of real paystubs on the host.
2. **Local OCR** for genuinely scanned stubs — Tesseract in a sidecar, opt-in.
   Adds an image to the deploy, but nothing leaves the machine.
3. **The AI provider**, last, and only with per-document consent at the point of
   upload rather than a deployment-wide switch. If this path is built, it needs
   its own config flag; it must **not** inherit `DOCUMENTS_OCR_ENABLED`, which
   an operator turned on to read receipts.

Route 1 is also simply more accurate: transcribing a known field from a text
layer cannot misread a digit, whereas a vision model can, and the whole reason
this doc insists on a review queue is that a misread YTD figure is expensive.

Redaction is worth considering regardless — a paystub's SSN is never needed by
any figure this doc computes, so stripping it before storage (not merely before
extraction) would lower the sensitivity ceiling of the entire database.

## Prerequisites

- **[18-document-vault.md](18-document-vault.md)** — hard dependency for path 2
  (PDF storage + OCR). Paths 1 and 3 (Plaid Payroll, manual entry) do not need
  it, so this doc can start before 18 lands if manual entry is built first.
- **[14-investments-page.md](14-investments-page.md)** — soft. `tax_treatment`
  tags let contributions route to the right account.

**Doc 15 improves substantially once this lands** (real savings rate, real
contribution tracking) but does not block on it.

## Data model

**Reserved migration: `00037_paystubs.sql`.**

```sql
CREATE TABLE employers (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id UUID NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    -- An EIN is tax-identifying data. See the security note below.
    ein          TEXT,
    address      TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE paystubs (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id       UUID NOT NULL REFERENCES users (id)     ON DELETE CASCADE,
    employer_id   UUID NOT NULL REFERENCES employers (id) ON DELETE CASCADE,
    period_start  DATE NOT NULL,
    period_end    DATE NOT NULL,
    pay_date      DATE NOT NULL,
    gross         NUMERIC(20,4) NOT NULL,
    net           NUMERIC(20,4) NOT NULL,
    ytd_gross     NUMERIC(20,4),
    ytd_net       NUMERIC(20,4),
    source        TEXT NOT NULL CHECK (source IN ('plaid','ocr','manual')),
    -- Set once the user has reviewed an OCR/Plaid import. Unconfirmed paystubs
    -- must not feed ANY reported figure.
    confirmed_at  TIMESTAMPTZ,
    -- The bank deposit this paystub's net pay corresponds to, once matched.
    transaction_id UUID REFERENCES transactions (id) ON DELETE SET NULL,
    document_id   UUID,  -- FK added conditionally once doc 18 has landed
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, employer_id, pay_date)
);
CREATE INDEX paystubs_user_paydate_idx ON paystubs (user_id, pay_date DESC);

CREATE TABLE paystub_lines (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    paystub_id  UUID NOT NULL REFERENCES paystubs (id) ON DELETE CASCADE,
    category    TEXT NOT NULL CHECK (category IN (
        'federal_income_tax','state_income_tax','local_income_tax',
        'fica_social_security','fica_medicare','medicare_surcharge',
        '401k_pre_tax','401k_roth','401k_employer_match',
        'ira_pre_tax','ira_roth','hsa','fsa',
        'health_premium','dental','vision','life_insurance',
        'disability','garnishment','commuter','dependent_care',
        'tuition_assistance','other')),
    label       TEXT NOT NULL,          -- the employer's own wording
    amount      NUMERIC(20,4) NOT NULL,
    ytd_amount  NUMERIC(20,4),
    pre_tax     BOOLEAN NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX paystub_lines_paystub_idx ON paystub_lines (paystub_id);
```

**Paystubs are per-user, not per-household.** A household member's salary is
among the most sensitive data the app will hold. Scope reads to the owning user
by default and require an explicit opt-in to share with the household — follow
the `plaid_items.is_shared` precedent rather than defaulting open.

**EIN and gross salary raise the sensitivity ceiling of the whole database.**
Note it in `docs/security.md`. If an AI extraction path is built at all, the
paystub goes to the configured provider — that must be stated plainly in the UI
at the point of upload, not buried, and consent belongs on the individual
document rather than on a deployment-wide flag. Doc 18 established the pattern
of gating on `doc_type` before decrypting; whatever this doc does must be at
least as strict, because a paystub is more sensitive than the tax documents 18
already refuses to send.

**`confirmed_at` gates everything.** An unconfirmed paystub is inert: excluded
from savings rate, tax summary, contribution tracking, and doc 15's projections.
This is the same inert-suggestion rule as doc 17's `suggested` aliases.

## Backend

New package `backend/internal/payroll/`.

### Ingestion, three paths

1. **Plaid Payroll Income** — cleanest where supported (ADP, Gusto, Paychex,
   UKG). Extends `plaid/client.go`; the Income product needs separate Plaid
   enablement, so gate it on the configured products the way
   `HasProduct(c.Products(), ProductTransactions)` already gates transactions.
2. **PDF extraction**, storing the PDF through doc 18's `documents.Vault`.
   Structured output, then the review queue. Prefer local text-layer extraction
   over an AI upload — see "Extraction without an upload" above, which is the
   part of this doc most worth re-reading before writing any code. Doc 18's
   `ocrEligibleTypes` allowlist refuses everything but receipts today, and
   widening it is a decision to make deliberately, not a line to change in
   passing.
3. **Manual entry** — the universal fallback and the one that must work first.
   The line taxonomy above is the form schema, so a paper stub or an unsupported
   employer is fully capturable.

### Reconciliation against the deposit

Match a paystub's `net` to a bank deposit within a few days of `pay_date`.
Propose, let the user confirm, then store `transaction_id`. This ties the pre-tax
record to the post-tax transaction so the two views cannot drift.

Do not auto-match ambiguous cases: two identical net amounts in a two-earner
household are common and a wrong match corrupts both records.

### Derived figures

- **Effective tax rate** = total tax lines / gross, per period and YTD.
- **Real savings rate** against gross, alongside the existing net-based figure in
  `reporting/summary.go`. **Do not silently redefine the existing savings rate** —
  it is consumed by the recap, insights, and chat. Add the gross-based figure as
  a distinct, labelled number.
- **Contribution-limit headroom.** IRS limits live in the versioned Go map doc 15
  establishes (`backend/internal/networth/`) — **share it, do not duplicate it.**
  If 15 has not landed, create it there anyway so 15 adopts it.
- **Total compensation** = gross + employer match + employer-paid benefits.

### Tax-prep summary

An annual report mapping stored lines to W-2 boxes: wages (box 1), federal
withheld (2), SS wages/tax (3/4), Medicare wages/tax (5/6), box 12 codes for
401k/Roth, HSA. Combined with vault documents, this is the packet handed to an
accountant.

**Label it unambiguously as a data summary, not a tax document, in the export
itself** — not just in the UI. It will be printed and emailed out of context.

## Frontend

- **Paystub list** per employer, with the review queue prominent.
- **"Where your paycheck went"** — a Sankey or stacked bar, gross → taxes →
  retirement → insurance → net. The single most clarifying chart most people have
  never seen, and the reason to build this feature.
- **Contribution progress** — "you're $15,500 from maxing your 401k, 6 pay
  periods left", with the limit year visible.
- **Effective tax rate** over time.
- **Total comp** vs. net pay.
- **Manual entry form** built from the line taxonomy, with add-a-line for
  anything unusual.

## Verification

- `go test -p 1 ./...`.
- **`gross − Σ(deduction lines) = net`** must hold for every confirmed paystub.
  Reject or flag one that does not balance — a paystub that does not reconcile is
  a mis-entry, and silently storing it poisons every downstream figure.
- Unconfirmed paystubs are inert: assert savings rate, tax summary, and
  contribution tracking are byte-identical with and without one present.
- Effective tax rate and YTD roll-ups against hand-computed fixtures.
- Contribution headroom with a mid-year employer change (two employers, combined
  401k deferral against one shared limit — a real and commonly-mishandled case).
- Reconciliation: two identical net deposits produce a *proposal*, not an
  auto-match.
- Access control: another household member cannot read an unshared paystub.
- Frontend `tsc -b`, `vite build`, `oxlint`.

## Out of scope

- **Filing anything.** No e-file, no form generation, no CPA advice. The output
  is a report over tracked data.
- State-specific tax logic beyond recording what the stub says.
- Equity comp modelling (RSU/option vesting) — that belongs with doc 28's job-offer
  scenario.
- Predicting future withholding.
