# 23 — Pre-tax income & deduction tracking (paystub importer)

*(TODO.md "Next major initiatives" #8. The largest doc in the backlog.)*

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

## Prerequisites

- **[18-document-vault.md](18-document-vault.md)** — hard dependency for path 2
  (PDF storage + OCR). Paths 1 and 3 (Plaid Payroll, manual entry) do not need
  it, so this doc can start before 18 lands if manual entry is built first.
- **[14-investments-page.md](14-investments-page.md)** — soft. `tax_treatment`
  tags let contributions route to the right account.

**Doc 15 improves substantially once this lands** (real savings rate, real
contribution tracking) but does not block on it.

## Data model

**Reserved migration: `00027_paystubs.sql`.**

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
Note it in `docs/security.md`. If OCR is used, the paystub image goes to the
configured AI provider — that must be stated plainly in the UI at the point of
upload, not buried.

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
2. **PDF OCR** via the configured AI provider, storing the PDF through doc 18.
   Structured output, then the review queue.
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
