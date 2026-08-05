-- +goose Up

-- Pre-tax income and deduction tracking. See docs/plans/23-paystub-income.md.
--
-- NOTE ON THE NUMBER, because doc 23's own text is wrong about it. The doc
-- names `00037_paystubs.sql`; that reservation is void and was issued below
-- `00043`, and goose runs in strict-ordering mode, so applying a `00037` to an
-- instance already at `00046` refuses to start outright. The reservation table
-- in docs/plans/README.md is authoritative and now allocates `00048` to this
-- doc — which is what this file takes. That is exactly the case the README
-- warns to check there rather than in the plan doc.
--
-- ORDERING HAZARD, and it is worth stating where somebody will read it.
-- `00047_manual_accounts.sql` (doc 30) is reserved but has not been written.
-- Once THIS migration has been applied to an instance, `00046` is no longer the
-- high-water mark and a later `00047` will be refused. Doc 30 must either land
-- before this reaches a live database or renumber above it. Nothing here can
-- prevent that; the reservation table is the only coordination point.
--
-- Everything the app has seen until now is post-tax: Plaid reports the deposit
-- that lands in checking, which is what is left after withholding, retirement
-- deferrals and premiums have already been taken. For a typical W-2 earner that
-- is 30-45% of gross income the app has never been able to see. These three
-- tables are the record of the other side.
--
-- Three rules run through the whole schema and every consumer of it:
--
--   1. A paystub is PER USER, not per household, and is private by default.
--      Salary is the most sensitive figure the app will ever hold, and the
--      difference between two members' pay is the kind of thing that must not
--      leak because a listing query forgot a predicate. See is_shared below.
--   2. `confirmed_at` gates everything. An unconfirmed paystub is inert: it
--      feeds no savings rate, no tax summary, no contribution total. Same
--      inert-suggestion rule as merchant alias suggestions (00023) and the
--      cached receipt extractions (00033).
--   3. Money is NUMERIC. Every figure derived from these rows is computed in
--      SQL or in shopspring/decimal, never in a float and never in JavaScript.

-- --------------------------------------------------------------------------
-- 1. Employers
-- --------------------------------------------------------------------------

-- Household-scoped rather than user-scoped, deliberately: two members can work
-- for the same employer, and a mid-year job change has to be able to reuse the
-- previous employer row when somebody goes back. Nothing about an employer row
-- is itself sensitive except the EIN, which is handled below.
CREATE TABLE employers (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id UUID NOT NULL REFERENCES households (id) ON DELETE CASCADE,
    name         TEXT NOT NULL,

    -- An EIN is tax-identifying data, so it is sealed with the same AES-GCM key
    -- that protects Plaid access tokens and vault documents rather than sitting
    -- in plaintext in every pg_dump. It is stored at all only because the
    -- annual summary handed to an accountant needs it; nothing the app computes
    -- reads it.
    --
    -- The consequence is the same one DEPLOYING.md already states for the
    -- vault: losing ENCRYPTION_KEY loses this column. That is the right trade
    -- for a field a user can retype off a W-2 in ten seconds.
    ein_encrypted BYTEA,

    address      TEXT,

    -- How often this employer pays. Stored rather than inferred from the gaps
    -- between stubs because the contribution-headroom figure ("$15,500 to go,
    -- 6 pay periods left") divides by it, and a household with two stubs on
    -- file would otherwise get a cadence guessed from a single gap.
    --
    -- Inferring it would also be wrong in the one case that matters most: a new
    -- job, where there is no history to infer from and the headroom question is
    -- the most urgent.
    pay_frequency TEXT NOT NULL DEFAULT 'biweekly',

    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT employers_pay_frequency_check
        CHECK (pay_frequency IN ('weekly', 'biweekly', 'semimonthly', 'monthly')),
    -- One row per employer name per household. Without it, "Acme Corp" typed
    -- twice produces two employers and the shared 401(k) limit is applied to
    -- each of them separately — which is exactly the mid-year-job-change bug
    -- the plan's verification section calls out.
    UNIQUE (household_id, name)
);

CREATE INDEX employers_household_idx ON employers (household_id);

-- --------------------------------------------------------------------------
-- 2. Paystubs
-- --------------------------------------------------------------------------

CREATE TABLE paystubs (
    id           UUID NOT NULL PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID NOT NULL REFERENCES users (id)     ON DELETE CASCADE,
    employer_id  UUID NOT NULL REFERENCES employers (id) ON DELETE CASCADE,

    period_start DATE NOT NULL,
    period_end   DATE NOT NULL,
    pay_date     DATE NOT NULL,

    gross        NUMERIC(20, 4) NOT NULL,
    net          NUMERIC(20, 4) NOT NULL,
    ytd_gross    NUMERIC(20, 4),
    ytd_net      NUMERIC(20, 4),

    -- 'pdf' rather than the plan's 'ocr': nothing here goes to an AI provider.
    -- The importer reads the text layer that ADP/Gusto/Paychex/UKG already put
    -- in the file, on this machine, with no network call — see
    -- internal/payroll/pdftext.go for why that is both safer and more accurate
    -- than a vision model on a fixed-layout document.
    --
    -- 'plaid' is reserved for the Payroll Income product and is accepted by the
    -- constraint so adding that path later is not a migration.
    source       TEXT NOT NULL CHECK (source IN ('plaid', 'pdf', 'manual')),

    -- Set once a human has reviewed an imported stub. NULL means the row exists
    -- but counts for nothing, anywhere. A manually typed stub is confirmed on
    -- creation — the user typed it, so there is nothing left to review.
    confirmed_at TIMESTAMPTZ,

    -- Private by default, and this is the one place the app INVERTS its usual
    -- sharing default. plaid_items.is_shared and documents.is_shared both
    -- default TRUE because a household's accounts and paperwork are normally
    -- joint. A salary is not: in a two-earner household the other member
    -- learning what you make must be a decision somebody made, not the
    -- consequence of a column default.
    is_shared    BOOLEAN NOT NULL DEFAULT FALSE,

    -- The bank deposit this stub's net pay corresponds to, once a human has
    -- confirmed the match. Never written by the matcher itself: two identical
    -- net deposits in a two-earner household are common, and a wrong link
    -- corrupts both the pre-tax and the post-tax record at once.
    transaction_id UUID REFERENCES transactions (id) ON DELETE SET NULL,

    -- The vault copy of the stub, when there is one. A real foreign key, not
    -- the plan's conditional one: 00024 has landed, so there is no reason to
    -- leave this dangling. ON DELETE SET NULL because deleting the PDF is not
    -- deleting the figures read off it.
    document_id  UUID REFERENCES documents (id) ON DELETE SET NULL,

    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT paystubs_period_order_check CHECK (period_end >= period_start),
    CONSTRAINT paystubs_gross_nonneg_check CHECK (gross >= 0),
    -- Net may legitimately be zero (a stub entirely consumed by deductions, or
    -- a correction run) but never negative: an employer does not take money out
    -- of a bank account through payroll.
    CONSTRAINT paystubs_net_nonneg_check CHECK (net >= 0),

    -- One stub per person per employer per pay date. A duplicate import is the
    -- expected mistake here — the same PDF dropped in twice — and it would
    -- silently double every YTD figure derived from period amounts.
    UNIQUE (user_id, employer_id, pay_date)
);

CREATE INDEX paystubs_user_paydate_idx  ON paystubs (user_id, pay_date DESC);
CREATE INDEX paystubs_employer_idx      ON paystubs (employer_id);
CREATE INDEX paystubs_transaction_idx   ON paystubs (transaction_id)
    WHERE transaction_id IS NOT NULL;

-- --------------------------------------------------------------------------
-- 3. Paystub lines
-- --------------------------------------------------------------------------

CREATE TABLE paystub_lines (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    paystub_id UUID NOT NULL REFERENCES paystubs (id) ON DELETE CASCADE,

    category   TEXT NOT NULL CHECK (category IN (
        'federal_income_tax', 'state_income_tax', 'local_income_tax',
        'fica_social_security', 'fica_medicare', 'medicare_surcharge',
        '401k_pre_tax', '401k_roth', '401k_employer_match',
        'ira_pre_tax', 'ira_roth', 'hsa', 'fsa',
        'health_premium', 'dental', 'vision', 'life_insurance',
        'disability', 'garnishment', 'commuter', 'dependent_care',
        'tuition_assistance', 'other')),

    -- The employer's own wording, kept verbatim so a stub can be reconciled
    -- against the paper it came from. Untrusted text: rendered as text, never
    -- as markup, and redacted of anything that looks like an SSN before it is
    -- ever stored (see payroll.RedactIdentifiers).
    label      TEXT NOT NULL,

    -- Always POSITIVE — the magnitude of the line, with its direction carried
    -- by the category and by is_employer. Signing deductions negative would
    -- make the balance check below read as an addition and invite exactly the
    -- sign error it exists to catch.
    amount     NUMERIC(20, 4) NOT NULL CHECK (amount >= 0),
    ytd_amount NUMERIC(20, 4),

    -- Whether this deduction came out before tax was computed. Stored per line
    -- rather than derived from the category because the same benefit is
    -- pre-tax under a Section 125 plan and post-tax without one, and only the
    -- stub knows which. The category decides WHICH taxable wage base a pre-tax
    -- line reduces (federal only, or federal and FICA); this flag decides
    -- whether it reduces anything at all. See internal/payroll/taxonomy.go.
    pre_tax    BOOLEAN NOT NULL,

    -- TRUE for money the employer paid on top of gross rather than took out of
    -- it: the 401(k) match, employer-paid premiums, tuition assistance.
    --
    -- This column is not in the plan's schema and the feature does not work
    -- without it. The plan's own verification rule is
    -- `gross - SUM(deduction lines) = net`, and a match included in that sum
    -- makes every stub with an employer match fail to balance. Employer lines
    -- are excluded from the balance equation and are the whole basis of the
    -- total-compensation figure.
    is_employer BOOLEAN NOT NULL DEFAULT FALSE,

    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- A match is by definition employer money. Enforced here rather than left
    -- to the API so a future importer cannot file one as an employee deduction
    -- and quietly break every stub's balance.
    CONSTRAINT paystub_lines_match_is_employer_check
        CHECK (category <> '401k_employer_match' OR is_employer)
);

CREATE INDEX paystub_lines_paystub_idx ON paystub_lines (paystub_id);
-- Contribution headroom sums one category across a tax year, so it reads by
-- category far more often than it reads a whole stub.
CREATE INDEX paystub_lines_category_idx ON paystub_lines (category);

-- +goose Down
DROP TABLE IF EXISTS paystub_lines;
DROP TABLE IF EXISTS paystubs;
DROP TABLE IF EXISTS employers;
