/**
 * Typed client for the Ledgermancy API.
 *
 * Two things the backend requires and this module hides from callers:
 *
 *  1. Sessions live in an httpOnly cookie, so every request must be sent with
 *     `credentials: 'include'`. There is no token to store or attach.
 *  2. Unsafe methods must echo the CSRF cookie in an X-CSRF-Token header
 *     (double-submit). A brand-new client has no CSRF cookie, so it is
 *     bootstrapped from GET /api/auth/csrf on first use. The backend also
 *     rotates the token on login, so it is always read fresh from the cookie
 *     rather than cached in module state.
 *
 * Offline, a GET may be answered from the service worker's cache rather than
 * the server; every response is reported to `offline.ts` so the UI can say so.
 * Writes are refused outright rather than failing obscurely — see
 * `assertOnline`.
 */

import { noteResponseOrigin } from './offline'

export interface User {
  id: string
  household_id: string
  email: string
  display_name: string
  /**
   * owner | member | child.
   *
   * Drives which navigation renders. It is NOT the enforcement — every
   * restricted route is guarded server-side, because a client-side role check
   * is decoration that a devtools console removes in one line.
   */
  role: Role
  /** The household_people row this login belongs to. */
  person_id: string | null
}

export type Role = 'owner' | 'member' | 'child'

/** An owner or a full member: everyone who is not a reduced child login. */
export function isAdult(user: Pick<User, 'role'> | null | undefined): boolean {
  return user?.role === 'owner' || user?.role === 'member'
}

/**
 * The household owner. Gates the operator surface (Settings -> Continuity),
 * which describes the instance rather than the household.
 *
 * Like isAdult this only decides what renders. The /api/admin routes are
 * guarded by auth.RequireOwner server-side.
 */
export function isOwner(user: Pick<User, 'role'> | null | undefined): boolean {
  return user?.role === 'owner'
}

/**
 * Login stopped at the second factor. Deliberately carries no user detail —
 * nothing about the account is readable until both factors are satisfied.
 */
export interface MFARequired {
  mfa_required: true
}

export type LoginResult = User | MFARequired

/** Narrows a login result; `mfa_required` is only ever present on the challenge. */
export function isMFARequired(result: LoginResult): result is MFARequired {
  return 'mfa_required' in result
}

export interface MFAStatus {
  enabled: boolean
  confirmed_at: string | null
  recovery_codes_remaining: number
  /** A secret exists but was never confirmed, so setup can be resumed. */
  setup_pending: boolean
}

export interface MFASetup {
  /** Inline PNG data URI. Rendered server-side so no QR library ships here. */
  qr_png: string
  /** Base32, for typing in by hand when a camera is not an option. */
  secret: string
  account: string
}

export interface RecoveryCodes {
  /** Returned exactly once. Only hashes are stored, so these cannot be re-read. */
  recovery_codes: string[]
}

export interface ActiveSession {
  id: string
  user_agent: string | null
  client_ip: string | null
  last_used_at: string
  expires_at: string
  created_at: string
  /** The browser making this request. */
  is_current: boolean
}

export interface AuthEvent {
  event_type: string
  client_ip: string | null
  user_agent: string | null
  metadata: Record<string, unknown>
  created_at: string
}

export interface Household {
  id: string
  name: string
}

export interface Member {
  id: string
  email: string
  display_name: string
  role: Role
  created_at: string
}

/**
 * Someone the household's money can be ABOUT, whether or not they can sign in.
 *
 * Distinct from Member, which is a login. A six-year-old with a 529 is a
 * Person with no Member — that distinction is the whole point of the feature.
 */
export interface Person {
  id: string
  display_name: string
  /** YYYY-MM-DD, or null when not given. Never defaulted. */
  birthdate: string | null
  /** Derived server-side from the birthdate. Null when there is none. */
  age: number | null
  is_dependent: boolean
  user_id: string | null
  email: string | null
  role: Role | null
  has_login: boolean
  created_at: string
}

export interface PersonInput {
  display_name: string
  birthdate: string | null
  is_dependent: boolean
}

export interface Allowance {
  person_id: string
  amount: string | null
  cadence: 'weekly' | 'biweekly' | 'monthly' | null
  monthly_limit: string | null
  auto_post: boolean
  /** Derived by summing the ledger. Never stored. */
  balance: string
  spent_this_month: string
  /** Null when no limit is set — different from a remaining balance of zero. */
  limit_remaining: string | null
}

export type AllowanceKind = 'allowance' | 'chore' | 'gift' | 'spend' | 'correction'

export interface AllowanceEntry {
  id: string
  person_id: string
  kind: AllowanceKind
  /**
   * POSITIVE for money INTO the balance, NEGATIVE for money out — the opposite
   * of a transaction amount. This table is a balance, not a spend feed.
   */
  amount: string
  occurred_on: string
  note: string | null
  created_at: string
}

export interface AllowanceEntryInput {
  kind: AllowanceKind
  /** Always positive; the server derives the sign from the kind. */
  amount: string
  occurred_on?: string
  note?: string | null
}

export interface Invite {
  id: string
  email: string
  role: Role
  person_id: string | null
  person_name: string | null
  expires_at: string
  created_at: string
}

export interface CreateInviteInput {
  email: string
  role?: Role
  /** Attaches the new login to an existing person instead of making a new one. */
  person_id?: string | null
}

/** One person's share of a split transaction. */
export interface SplitShare {
  id: string
  person_id: string
  person_name: string
  amount: string
  settled_at: string | null
}

export interface TransactionSplits {
  transaction_id: string
  transaction_name: string
  transaction_amount: string
  date: string
  shares: SplitShare[]
}

export interface SplitTransaction {
  transaction_id: string
  name: string
  date: string
  amount: string
  payer_name: string | null
  split_count: number
  unsettled_count: number
  fully_settled: boolean
}

/** One direction of "who owes whom", already netted. */
export interface LedgerEntry {
  debtor_id: string
  debtor_name: string
  creditor_id: string
  creditor_name: string
  amount: string
}

export interface GoalContribution {
  id: string
  goal_id: string
  person_id: string
  person_name: string
  amount: string
  occurred_on: string
  note: string | null
  created_at: string
}

export interface ContributorTotal {
  person_id: string
  person_name: string
  total: string
  share_pct: string
}

export interface GoalContributions {
  goal_id: string
  /**
   * Sum of logged contributions. This is ATTRIBUTION, not the goal's progress:
   * an account-linked goal still derives progress from the account balance.
   * Render it as "funded by", never as the progress bar.
   */
  total: string
  contributors: ContributorTotal[]
  history: GoalContribution[]
}

/** An account held FOR the signed-in person. */
export interface MyAccount {
  id: string
  name: string
  institution_name: string | null
  type: string
  subtype: string | null
  tax_treatment: string | null
  balance: string | null
  /** 529, UTMA/UGMA, Coverdell, custodial Roth or Trump account. */
  is_custodial: boolean
}

export interface PersonNetWorth {
  person_id: string
  person_name: string
  is_dependent: boolean
  age: number | null
  account_total: string
  custodial_total: string
  manual_total: string
  total: string
}

/**
 * A BREAKDOWN of household assets by the person they are held for — never a
 * new total. `assigned + unassigned === total`, which is what the UI shows so
 * the two obviously reconcile.
 */
export interface NetWorthByPerson {
  people: PersonNetWorth[]
  assigned: string
  unassigned: string
  total: string
}

export interface CreatedInvite extends Invite {
  /** Returned exactly once, at creation. It cannot be retrieved later. */
  token: string
}

/** A linked institution. */
export interface PlaidItem {
  id: string
  institution_name: string
  /** active | login_required | revoked | error */
  status: string
  products: string[]
  is_shared: boolean
  /** False when a household member linked it, not you. */
  is_own: boolean
  backfill_complete: boolean
  last_synced_at: string | null
  error_code: string | null
  earliest_transaction: string | null
  latest_transaction: string | null
  /** Days of history the institution actually returned, null if none yet. */
  history_days: number | null
}

export interface SyncResult {
  item_id: string
  pages: number
  added: number
  modified: number
  removed: number
  accounts: number
  earliest_transaction: string | null
  latest_transaction: string | null
}

export interface Account {
  id: string
  /** The linked institution this belongs to. Group by this, never by
   *  `institution_name` — two household members can link the same bank. */
  item_id: string
  name: string
  mask: string | null
  /** depository | credit | loan | investment | other */
  type: string
  subtype: string | null
  institution_name: string | null
  /** Decimal serialised as a string — never parse into a JS number for maths. */
  current_balance: string | null
  available_balance: string | null
  currency: string
  is_own: boolean
}

export interface Transaction {
  id: string
  date: string
  name: string
  merchant_name: string | null
  /** Normalized key the app caches categories by; present even when
   * merchant_name is null, empty when there was too little signal to key on. */
  merchant_key: string | null
  /** Positive = money out, negative = money in (Plaid's convention). */
  amount: string
  currency: string
  pending: boolean
  account_id: string
  account_name: string
  institution_name: string | null
  plaid_category_primary: string | null
  plaid_category_detailed: string | null
  category_id: string | null
  notes: string | null
  /** 'plaid' | 'csv' | 'manual'. Only 'manual' rows can be edited or deleted. */
  source: string
  /**
   * A hand-entered row that a later Plaid charge now appears to match (same
   * account, same amount, within four days) — likely the issuer finally
   * delivering the charge the user reconciled by hand.
   */
  possible_duplicate: boolean
}

/**
 * Body for creating or editing a manual transaction. Amount is a decimal string
 * already signed by the caller (positive = money out, negative = a refund), so
 * it never passes through a JS float.
 */
export interface ManualTransactionInput {
  account_id: string
  date: string
  amount: string
  name: string
  merchant_name?: string | null
  category_id?: string | null
  notes?: string | null
}

export interface ImportResult {
  imported: number
  skipped_duplicates: number
  skipped_invalid: number
  uncategorized: number
}

export interface TransactionQuery {
  from?: string
  to?: string
  limit?: number
  offset?: number
  /**
   * Restrict to these accounts. Serialized comma-joined (an empty array drops
   * out entirely), which the API reads as "all visible accounts".
   */
  accounts?: string[]
  /** Restrict to one category. Empty/omitted means all categories. */
  category_id?: string
  /** Only rows still needing a category (null or the fallback bucket). */
  uncategorised?: boolean
}

export interface Category {
  id: string
  name: string
  slug: string
  color: string | null
  is_income: boolean
  is_transfer: boolean
  is_fixed: boolean
  is_system: boolean
}

/**
 * Body for creating/editing a custom category. `is_transfer` marks money moving
 * between your own accounts (a card payment, a transfer to savings) — excluded
 * from spending entirely; `is_income` marks money coming in. At most one is
 * true; the server treats a transfer/income category as never "fixed".
 */
export interface CategoryWrite {
  name: string
  color: string | null
  is_fixed: boolean
  is_income: boolean
  is_transfer: boolean
}

/** All money fields are decimal strings. Never sum them in JavaScript. */
export interface Summary {
  from: string
  to: string
  income: string
  spending: string
  fixed_spending: string
  discretionary_spending: string
  leftover: string
  /** 0–1, or null when the period had no income (the ratio is meaningless). */
  savings_rate: string | null
  transaction_count: number
}

export interface CategorySpend {
  category_id: string
  name: string
  slug: string
  color: string | null
  is_fixed: boolean
  total: string
  transaction_count: number
}

export interface TrendPoint {
  /** "YYYY-MM" */
  month: string
  income: string
  spending: string
  leftover: string
}

export interface CategoryAverage extends CategorySpend {
  monthly_average: string
}

/** One calendar day's spend. `day` is "YYYY-MM-DD". */
export interface DaySpend {
  day: string
  spending: string
}

export interface MerchantSpend {
  merchant: string
  /**
   * The resolved merchant key — an entity id for a grouped merchant, the raw
   * descriptor otherwise. This is what addresses the merchant detail view, so it
   * is what makes a top-merchants row a link.
   */
  merchant_key: string
  total: string
  transaction_count: number
}

export interface MerchantMonthPoint {
  /** "YYYY-MM-DD", first of the month. */
  month: string
  total: string
  transaction_count: number
}

export interface MerchantDetailTransaction {
  id: string
  date: string
  amount: string
  /** The raw text the bank printed for this charge. */
  descriptor: string
  raw_merchant_key: string | null
  account_name: string
  category_name: string | null
  category_id: string | null
}

/**
 * One merchant over one period: the numbers, the shape over time, how it is
 * filed, and the charges behind all three.
 *
 * Works for merchants that were never grouped as well as merged ones, because it
 * is addressed by resolved key rather than by entity id — and most of a
 * household's spending sits at merchants nobody has grouped.
 */
export interface MerchantDetail {
  merchant_key: string
  merchant: string
  /** True when this is a merged merchant rather than a lone descriptor. */
  is_grouped: boolean
  /** The raw descriptors resolving here; one when ungrouped. */
  descriptors: string[]
  from: string
  to: string
  total: string
  transaction_count: number
  average: string
  largest: string
  first_seen: string | null
  last_seen: string | null
  monthly: MerchantMonthPoint[]
  /** Shaped as CategorySpend so CategoryBars consumes it directly. */
  categories: CategorySpend[]
  transactions: MerchantDetailTransaction[]
}

export interface BudgetProgress {
  budget_id: string
  category_id: string
  name: string
  slug: string
  color: string | null
  budgeted: string
  /** weekly | monthly | yearly. */
  period: string
  /** Inclusive window the spend is measured over (YYYY-MM-DD). */
  period_start: string
  period_end: string
  /** Whether unspent amount rolls into next month (envelope budgeting; monthly only). */
  rollover: boolean
  /** Balance carried in from prior months; negative if the envelope was overspent. */
  carryover: string
  /** This month's amount plus carryover — the true ceiling this month. */
  available: string
  spent: string
  remaining: string
}

/**
 * "Safe to spend" and its parts. All amounts are decimal strings. safe_to_spend
 * = expected_income − fixed_costs − budgeted_discretionary − goal_contributions.
 */
export interface SafeToSpend {
  expected_income: string
  fixed_costs: string
  budgeted_discretionary: string
  goal_contributions: string
  safe_to_spend: string
  /** Months the income average is based on; low values mean a thin history. */
  income_months: number

  /**
   * The bill-aware view, added ALONGSIDE safe_to_spend — the original field
   * keeps its meaning. `fixed_costs` is a trailing six-month average of what was
   * actually paid; these fields replace that per category with the bills the
   * calendar knows are still to come, so no bill is subtracted twice.
   */
  upcoming_obligations: string
  fixed_costs_after_bills: string
  safe_to_spend_after_bills: string
  /**
   * How many fixed categories have a known obligation behind them. 0 means the
   * after-bills figure carries no new information and should not be shown as if
   * it did.
   */
  obligation_coverage: number
}

/**
 * A recurring obligation: a bill the app knows is coming. `next_due` and
 * `monthly_estimate` are DERIVED server-side, never stored — a stored next-due
 * date is a cache that goes stale the moment the calendar rolls over.
 *
 * `source` is 'detected' for rows promoted from the recurring-charge detector
 * and 'manual' for ones a member entered. Manual entry is the only way an annual
 * premium or anything paid by cheque can ever be known.
 */
export interface Obligation {
  id: string
  label: string
  amount: string
  category_id: string | null
  account_id: string | null
  interval_count: number
  interval_unit: ObligationUnit
  /** Human phrasing of the cadence, e.g. "every 2 weeks". Decided server-side. */
  cadence: string
  anchor_date: string
  end_date: string | null
  next_due: string | null
  days_until_due: number | null
  monthly_estimate: string
  source: 'manual' | 'detected'
  merchant_key: string | null
  is_shared: boolean
  is_active: boolean
  /** True once a human has edited it; re-detection then leaves it alone. */
  user_edited: boolean
  is_personal: boolean
}

export type ObligationUnit = 'day' | 'week' | 'month' | 'year'

/** Fields to create or update an obligation. Amounts and dates are strings. */
export interface ObligationInput {
  label: string
  amount: string
  category_id?: string | null
  account_id?: string | null
  interval_count: number
  interval_unit: ObligationUnit
  anchor_date: string
  end_date?: string
  personal?: boolean
  is_active?: boolean
}

/** One obligation on one date. Several rows can share obligation_id. */
export interface UpcomingObligation {
  obligation_id: string
  label: string
  amount: string
  category_id: string | null
  account_id: string | null
  cadence: string
  source: 'manual' | 'detected'
  due_date: string
  days_until_due: number
}

export interface UpcomingObligations {
  from: string
  to: string
  total: string
  items: UpcomingObligation[]
}

export interface ProjectionPoint {
  date: string
  balance: string
  /** What falls due on this day — zero on most of them. */
  due: string
}

export interface AccountProjection {
  /** Null on the combined series, which is every cash account together. */
  account_id: string | null
  name: string
  mask: string | null
  institution_name: string | null
  current_balance: string
  lowest_balance: string
  lowest_date: string
  goes_negative: boolean
  points: ProjectionPoint[]
}

/**
 * Balances carried forward through KNOWN obligations only. It is not a
 * prediction of discretionary spending and must not be presented as one.
 * `unassigned_total` is the money the per-account lines cannot show because no
 * account was named on those bills.
 */
export interface BalanceProjection {
  from: string
  to: string
  combined: AccountProjection
  accounts: AccountProjection[]
  unassigned_total: string
  total_due: string
}

export interface PeriodQuery {
  from?: string
  to?: string
}

/**
 * One proposed budget from POST /api/budgets/suggest. `computed_average` is the
 * exact SQL figure (never the model's); `suggested_amount` is a round target at
 * or above it. All money fields are decimal strings — never summed here.
 */
export interface BudgetProposal {
  category_id: string
  category_name: string
  slug: string
  is_fixed: boolean
  computed_average: string
  suggested_amount: string
  rationale: string
  already_budgeted: boolean
  current_budget?: string
}

export interface BudgetSuggestions {
  period_months: number
  /** True when an AI tailored the targets/rationale; false for rule-based rounding. */
  ai_tailored: boolean
  proposals: BudgetProposal[]
}


export interface NetWorthBreakdown {
  cash: string
  investments: string
  other_assets: string
  manual_assets: string
  credit_debt: string
  loan_debt: string
  manual_debt: string
}

export interface NetWorth {
  assets_total: string
  liabilities_total: string
  net_worth: string
  breakdown: NetWorthBreakdown
  as_of: string
}

export interface NetWorthPoint {
  as_of: string
  assets_total: string
  liabilities_total: string
  net_worth: string
}

export interface Holding {
  id: string
  security_name: string | null
  ticker: string | null
  security_type: string | null
  quantity: string
  cost_basis: string | null
  value: string | null
  gain: string | null
  account_name: string
  institution_name: string | null
  is_cash_equivalent: boolean
}

export interface Liability {
  id: string
  /** The account this debt belongs to — the key to filter accounts by when a
   *  picker must only offer debts (a payoff goal's linked account). */
  account_id: string
  kind: string
  account_name: string
  mask: string | null
  institution_name: string | null
  apr: string | null
  balance: string | null
  minimum_payment: string | null
  next_payment_due_date: string | null
  is_overdue: boolean | null
}

export interface ManualAsset {
  id: string
  name: string
  kind: string
  value: string
  is_liability: boolean
  as_of: string
  notes: string | null
}

// --- Investments ----------------------------------------------------------

/** The nine tax treatments the backend's CHECK constraint allows. */
export type TaxTreatment =
  | 'taxable'
  | 'trad_401k'
  | 'roth_401k'
  | 'trad_ira'
  | 'roth_ira'
  // Custodial treatments. Money held for a dependent: excluded from the
  // household's retirement nest egg, because it is not the household's to
  // retire on. A UTMA is irrevocably the child's property.
  | 'utma_ugma'
  | 'coverdell'
  | 'custodial_roth'
  | 'trump'
  | '529'
  | 'hsa'
  | 'trust'
  | 'other'

export interface InvestmentAccount {
  id: string
  name: string
  mask: string | null
  subtype: string | null
  institution_name: string | null
  balance: string | null
  currency: string
  /** Null until the user confirms one. Never inferred on their behalf. */
  tax_treatment: TaxTreatment | null
  /** The person this account is held FOR (a 529's beneficiary, a UTMA's minor). */
  beneficiary_person_id?: string | null
  /**
   * Inferred from the Plaid subtype for display beside the picker. Empty when
   * the subtype cannot distinguish — a 401(k) is reported identically whether
   * it is traditional or Roth, and guessing changes every retirement number.
   */
  suggested_tax_treatment: TaxTreatment | ''
  is_managed: boolean | null
}

export interface InvestmentOverview {
  total_value: string
  cost_basis: string
  /** Null when no holding reports a cost basis. */
  unrealised_gain: string | null
  unrealised_gain_pct: string | null
  /** Market value the gain figures actually cover, and what they exclude. */
  basis_coverage_value: string
  basis_excluded_holdings: number
  accounts: InvestmentAccount[]
  untagged_accounts: number
  /** Days of recorded value history. Zero means performance has nothing yet. */
  history_days: number
}

export type InvestmentPeriod = 'ytd' | '1y' | '3y' | '5y' | 'inception'

export interface InvestmentPerformance {
  period: InvestmentPeriod
  /** False when history is too thin for any figure; `caveat` says why. */
  computable: boolean
  caveat: string
  start: string
  end: string
  days: number
  start_value: string
  end_value: string
  net_flows: string
  gain: string
  /** Fractions, not percentages: "0.0734" is 7.34%. */
  twr: string | null
  annualised: string | null
  /** Null when the IRR solve found no root — a legitimate answer, not an error. */
  mwr: string | null
  /** Non-empty exactly when `mwr` is null. */
  mwr_note: string
}

export interface SeriesPoint {
  date: string
  value: string
}

export interface BenchmarkSeries {
  label: string
  points: SeriesPoint[]
}

export interface BenchmarkComparison {
  period: InvestmentPeriod
  /** False when the operator has not opted into outbound price fetching. */
  enabled: boolean
  series: BenchmarkSeries[]
  basis: string
}

export interface AllocationSlice {
  label: string
  value: string
  /** 0–100. */
  percent: string
}

export interface InvestmentAllocation {
  by_asset_class: AllocationSlice[]
  by_tax_treatment: AllocationSlice[]
  note: string
}

export interface DetailedHolding {
  id: string
  account_id: string
  account_name: string
  institution_name: string | null
  security_name: string | null
  ticker: string | null
  security_type: string | null
  quantity: string
  cost_basis: string | null
  value: string | null
  last_price: string | null
  last_price_as_of: string | null
  gain: string | null
  gain_pct: string | null
  is_cash_equivalent: boolean
  tax_treatment: TaxTreatment | null
}

export interface FeeDrag {
  annual_cost: string
  covered_value: string
  uncovered_value: string
  covered_holdings: number
  excluded_holdings: number
  /** Coverage disclosure. Always shown — a partial fee total is misinformation. */
  note: string
}

export interface DividendMonth {
  month: string
  total: string
}

export interface DividendIncome {
  months: DividendMonth[]
  total: string
  basis: string
}

export interface ProjectionPoint {
  month: string
  net_worth: string
  assets: string
  liabilities: string
  contributed: string
  growth: string
}

export interface Projection {
  assumptions: {
    monthly_surplus: string
    annual_return_rate: string
    annual_debt_paydown: string
    months: number
  }
  points: ProjectionPoint[]
  /** Always true. These are illustrations, not forecasts. */
  estimate: boolean
  basis: string
  /**
   * An AI-written phrasing of the same milestones, present only when AI is
   * enabled and the call succeeded. The numbers and the caveat render without it.
   */
  narrative?: string | null
}

export interface ProjectionQuery {
  months?: number
  monthly_surplus?: string
  annual_return_rate?: string
  annual_debt_paydown?: string
}

// --- Retirement -----------------------------------------------------------
//
// A separate surface from the Net Worth projection above, not a replacement.
// That one compounds a single pooled balance and has no withdrawal phase; this
// one is account-aware and answers "when can I stop, and does it last".
//
// Every rate here is a fraction ("0.05" is 5%) and REAL — already net of
// inflation — so every dollar figure is in today's dollars.

export interface RetirementAssumptions {
  real_return_rate: string
  inflation_rate: string
  withdrawal_rate: string
  /** Null means "not decided", which is a real answer and not a zero. */
  target_retirement_age: number | null
  current_age: number | null
  annual_ss_income: string | null
  ss_start_age: number | null
  /**
   * What the user set. When null the projection uses `defaulted_spending` —
   * the household's own trailing-twelve-month spend — and says so.
   */
  target_annual_spending: string | null
  defaulted_spending: string
  spending_is_defaulted: boolean
  basis: string
}

export interface AccountPoint {
  balance: string
  /** The employee's own money. The employer's match is kept separate. */
  contributed: string
  employer: string
  growth: string
}

export interface RetirementPoint {
  month: string
  age: number
  /** The nest egg: every projected account except education accounts. */
  retirement: string
  /** 529 balances, tracked apart and never counted as retirement money. */
  education: string
  contributed: string
  growth: string
  employer_contributed: string
  /** Withdrawal rate on the nest egg, plus Social Security once it starts. */
  supported_spending: string
  by_account: Record<string, AccountPoint>
}

/** A contribution the projection had to reduce to stay inside an IRS limit. */
export interface CapNote {
  group: string
  planned_annual: string
  allowed_annual: string
}

export interface RetirementProjection {
  points: RetirementPoint[]
  limits_year: number
  /** False when that year is missing: contributions were NOT capped. */
  limits_configured: boolean
  cap_notes: CapNote[]
  nest_egg_at_target: string | null
  supported_at_target: string | null
  /** Null when FI is not reached inside the horizon — an answer, not a gap. */
  fi_age: number | null
  fi_month: string | null
  already_fi: boolean
  /** Accounts with no confirmed tax treatment, excluded and named. */
  excluded_accounts: string[]
  excluded_value: string
}

export interface SavingsRateSolve {
  reachable: boolean
  required_monthly: string | null
  /** Null when income is unknown — a rate with no denominator is not a rate. */
  required_rate: string | null
  note: string
}

export interface MonteCarloResult {
  runs: number
  survived: number
  years: number
  /** A fraction over the MODELLED SEQUENCES, never a real-world probability. */
  survival_rate: string
  median_ending_balance: string
  /** Reported so a result can be reproduced exactly. */
  seed: number
  /** Rendered verbatim. Never show the survival rate without it. */
  basis: string
}

export interface RetirementResponse {
  /** Travels with every projection so a curve is never rendered inputless. */
  assumptions: RetirementAssumptions
  projection: RetirementProjection
  required_savings?: SavingsRateSolve
  monte_carlo?: MonteCarloResult
  monte_carlo_enabled: boolean
  estimate: boolean
  basis: string
  /** What this model deliberately does not do. Always shown. */
  omissions: string[]
}

export interface ContributionAccount {
  id: string
  name: string
  mask: string | null
  subtype: string | null
  institution_name: string | null
  balance: string | null
  tax_treatment: TaxTreatment | null
  monthly_contribution: string
  employer_match_pct: string | null
  annual_salary: string | null
  employer_match_limit: string | null
  beneficiary_current_age: number | null
  beneficiary_target_age: number | null
  /** The IRS cap this kind is subject to; null when none applies or the year is unconfigured. */
  annual_limit: string | null
  /** True when that cap covers more than this account (all 401ks share one). */
  limit_shared: boolean
  planned_annual: string
}

export interface ContributionsResponse {
  accounts: ContributionAccount[]
  limits_year: number
  limits_configured: boolean
  limits_note: string
  untagged_accounts: number
}

export interface AssumptionsInput {
  real_return_rate: string
  inflation_rate: string
  withdrawal_rate: string
  target_retirement_age: number | null
  current_age: number | null
  annual_ss_income: string | null
  ss_start_age: number | null
  target_annual_spending: string | null
}

export interface ContributionInput {
  monthly_contribution: string
  employer_match_pct: string | null
  annual_salary: string | null
  employer_match_limit: string | null
  beneficiary_current_age: number | null
  beneficiary_target_age: number | null
}

export type AlertType =
  | 'big_spend'
  | 'budget_threshold'
  | 'unusual_merchant'
  | 'low_leftover'
  | 'predicted_low_balance'

/** A configured alert rule. config is the type-specific threshold object. */
export interface Alert {
  id: string
  type: AlertType
  config: Record<string, string | number>
  /** Whether the rule fires at all (and shows in the in-app feed). */
  enabled: boolean
  /** Whether a fired event is also pushed to members' notification channels. */
  push: boolean
}

/**
 * A raised alert. payload is a flat map of display strings the backend already
 * formatted (money as fixed-2 decimal strings — never summed here).
 */
export interface AlertEvent {
  id: string
  alert_type: AlertType
  payload: Record<string, string>
  triggered_at: string
  read: boolean
}

/** The two kinds of goal: put money aside, or clear a debt. */
export type GoalKind = 'savings' | 'debt_payoff'

/**
 * The amortization detail behind a debt_payoff goal. Every figure is computed
 * server-side in exact decimal — interest is never approximated in the browser.
 */
export interface GoalPayoff {
  /** False when there's no schedule to show; `reason` says why. */
  available: boolean
  reason: string
  /** What's owed now — the account's current balance. */
  balance: string
  apr: string
  monthly_payment: string
  monthly_interest: string
  /**
   * The headline case: the payment is at or below the interest, so the balance
   * never falls. `months`, `total_interest` and `payoff_date` are then empty —
   * there is no schedule.
   */
  never_pays_off: boolean
  months: number
  total_interest: string
  payoff_date: string | null
  /** Smallest payment that clears the balance by the target date. */
  required_monthly: string
  target_reachable: boolean
}

/**
 * A goal plus its DERIVED standing. `current_amount`, `required_monthly` and the
 * on-track/shortfall figures are computed server-side (never stored, so they
 * can't drift). All money fields are decimal strings — never summed here.
 *
 * The standing fields mean the same thing for both kinds but are computed
 * differently: savings goals accumulate toward the target, payoff goals amortize
 * a debt down to nothing, so a payoff goal's `required_monthly` accounts for
 * interest and is larger than remaining ÷ months.
 *
 * For a payoff goal `target_amount` is THE BALANCE TO ELIMINATE, captured from
 * the account when the goal was created — not zero. `current_amount` is
 * therefore the debt retired so far, and the progress bar reads the same way for
 * both kinds.
 */
export interface Goal {
  id: string
  scope: 'household' | 'user' | 'person'
  /** Set iff scope === 'person': the household member the goal belongs to. */
  person_id: string | null
  kind: GoalKind
  name: string
  target_amount: string
  target_date: string | null
  account_id: string | null
  category_id: string | null
  current_amount: string
  required_monthly: string
  shortfall: string
  months_left: number
  on_track: boolean
  /** True when the goal has no target date, so there's nothing to be "behind" on. */
  open_ended: boolean
  achieved: boolean
  created_at: string
  /** Present only on a debt_payoff goal. */
  payoff?: GoalPayoff
}

/** Fields to create or update a goal. Amounts/dates are strings, never floats. */
export interface GoalInput {
  name: string
  /** Omit only on a debt_payoff *create*, where the server captures the linked
   *  account's current balance as the amount to eliminate. Every other call —
   *  savings creates and all updates — must supply it. */
  target_amount?: string
  target_date?: string
  /** Defaults to 'savings'. A 'debt_payoff' goal must set `account_id`. */
  kind?: GoalKind
  scope?: 'household' | 'user' | 'person'
  /** Required when scope is 'person'. */
  person_id?: string | null
  account_id?: string | null
  category_id?: string | null
}

/** A parsed goal proposal from POST /api/goals/parse (never auto-saved). */
export interface GoalProposal {
  name: string
  /** Empty on a debt_payoff proposal — the balance comes from the account. */
  target_amount: string
  target_date: string | null
  kind: GoalKind
  /** Set on a debt_payoff proposal: the debt the sentence named, resolved
   *  server-side to a real account. */
  account_id: string | null
  account_name: string
}

/** A parsed alert proposal from POST /api/alerts/parse (never auto-saved). */
export interface ParsedAlert {
  type: AlertType
  config: Record<string, string | number>
}

/** A parsed budget proposal: the category is already resolved to a real id/slug. */
export interface ParsedBudget {
  category_id: string
  category_slug: string
  category_name: string
  amount: string
}

/**
 * The result of parsing a natural-language rule request. `kind` narrows which of
 * `alert`/`budget` is present. `summary` describes exactly what the engine will
 * enforce (not the user's phrasing); `caveats` flag any lost detail. An
 * `unsupported` result carries only a `reason` and cannot be saved.
 */
export interface ParseRuleResult {
  kind: 'alert' | 'budget' | 'unsupported'
  alert?: ParsedAlert
  budget?: ParsedBudget
  summary?: string
  caveats?: string[]
  reason?: string
}

/** A detected recurring charge (subscription/bill). Amounts are decimal strings. */
export interface RecurringMerchant {
  /** Stable key the detector groups by; what a "not recurring" override acts on. */
  merchant_key: string
  merchant: string
  occurrences: number
  average_amount: string
  avg_gap_days: string
  /** weekly | every 2 weeks | monthly */
  cadence: string
  /** Charge normalised to a per-month figure, computed server-side. */
  monthly_estimate: string
  last_seen: string
}

/**
 * One raw bank descriptor mapped to a canonical merchant.
 *
 * The evidence fields are not decoration: nobody can judge "are these the same
 * business?" from two strings, but the spend, the count and the date range make
 * it obvious most of the time.
 */
export interface MerchantAlias {
  merchant_key: string
  /** The most recent raw descriptor, before normalisation stripped it down. */
  sample_name: string
  /** manual | fuzzy | llm | suggested. 'suggested' rows affect no report. */
  source: string
  /** The suggester's own 0–1 rating; null for a merge made by hand. */
  confidence: number | null
  transaction_count: number
  total_amount: string
  first_seen: string | null
  last_seen: string | null
}

/** A canonical merchant: the descriptors merged into it, plus pending proposals. */
export interface Merchant {
  id: string
  canonical_name: string
  default_category_id: string | null
  /** Confirmed descriptors. Only these count toward the totals below. */
  members: MerchantAlias[]
  /** Proposed descriptors awaiting review. Inert until confirmed. */
  suggested: MerchantAlias[]
  transaction_count: number
  total_amount: string
}

/** A raw descriptor in the household, with its current mapping. */
export interface MerchantKeyStat {
  merchant_key: string
  sample_name: string
  transaction_count: number
  total_amount: string
  first_seen: string
  last_seen: string
  entity_id: string | null
  alias_source: string
}

/** What a merge did to the descriptors' cached categories. */
export interface MergeResult {
  entity_id: string
  /** Two descriptors carried different MANUAL categories; nothing was changed. */
  category_conflict: boolean
  category_applied: string | null
  /** The surviving category was a manual choice, and the merge kept it. */
  category_from_manual: boolean
}

/** A merchant the household has marked "not recurring". */
export interface SuppressedRecurringMerchant {
  merchant_key: string
  merchant: string
  suppressed_at: string
}

/** The AI monthly recap. summary is null when none has been generated yet. */
export interface MonthlySummary {
  month: string
  label: string
  summary: string | null
  model?: string
  generated_at?: string
}

/** Optional-feature flags so the UI hides AI surfaces when no key is set. */
export interface Capabilities {
  ai_enabled: boolean
  /** Whether an ntfy server is configured, so Settings can gate push controls. */
  notify_enabled: boolean
}

/**
 * One proactive-feed insight. `data` is the deterministic facts the narrative
 * was built from — money as decimal strings, never summed here. Higher
 * `priority` sorts first. `read_at`/`dismissed_at` are null until acted on.
 */
export interface Insight {
  id: string
  kind: string
  priority: number
  title: string
  body: string
  data: Record<string, string | number>
  period: string | null
  created_at: string
  read_at: string | null
  dismissed_at: string | null
}

/**
 * The caller's resolved preferences: user-scoped values (with reserved-key
 * defaults filled in by the server) and household-scoped values. Values are
 * whatever JSON was stored — a string, boolean, or array depending on the key.
 */
export interface Preferences {
  user: Record<string, unknown>
  household: Record<string, unknown>
}

/** One preference to upsert. The owning ID is taken from the session, never here. */
export interface PreferenceWrite {
  scope: 'user' | 'household'
  key: string
  value: unknown
}

// --- Continuity ------------------------------------------------------------

/**
 * The kinds of thing the backup subsystem does, in the order the panel lists
 * them. `restore_test` is first deliberately: it is the one that says whether
 * any of the others are worth anything.
 */
export type ContinuityKind =
  | 'restore_test'
  | 'db_dump'
  | 'documents_archive'
  | 'export'
  | 'mirror_push'
  | 'key_ack'

/**
 * `off` means deliberately not configured; `never` means configured and has
 * never produced a result. They look similar and mean opposite things, so the
 * panel styles them differently.
 */
export type ContinuityHealth = 'good' | 'warn' | 'bad' | 'off' | 'never'

export interface ContinuityRun {
  kind: ContinuityKind
  health: ContinuityHealth
  status?: 'success' | 'failure'
  at?: string
  age?: string
  /** A sentence naming the consequence, written server-side. Rendered verbatim. */
  headline: string
  detail?: string
  size_bytes?: number
  artifact_path?: string
}

export interface Continuity {
  enabled: boolean
  settings: {
    dir: string
    mirror_dir: string
    interval: string
    restore_test_interval: string
    include_documents: boolean
    keep_daily: number
    keep_weekly: number
    keep_monthly: number
  }
  runs: ContinuityRun[]
  coverage: {
    in_export: number
    dump_only: number
    derived: number
    ephemeral: number
    blob_stores: { name: string; why: string }[]
  }
}

// --- Document vault --------------------------------------------------------

export const DOCUMENT_TYPES = [
  'receipt',
  'tax',
  'warranty',
  'insurance',
  'contract',
  'statement',
  'other',
] as const

export type DocumentType = (typeof DOCUMENT_TYPES)[number]

/** The ledger records a document can be attached to. */
export type DocumentTargetKind =
  | 'transaction'
  | 'manual_asset'
  | 'account'
  | 'goal'

export interface DocumentTarget {
  kind: DocumentTargetKind
  id: string
}

export interface DocumentLink {
  id: string
  document_id: string
  target_kind: DocumentTargetKind
  target_id: string
  label: string
  /** Present for transaction links. */
  date: string | null
  amount: string | null
}

export interface VaultDocument {
  id: string
  title: string
  doc_type: DocumentType
  filename: string
  mime_type: string
  size_bytes: number
  /** False when another household member uploaded it. */
  is_shared: boolean
  uploaded_by: string | null
  is_own: boolean
  content_hash: string

  document_date: string | null
  expires_at: string | null
  /** Advisory keep-until, computed from the type. Nothing is ever auto-deleted. */
  retain_until: string | null
  notes: string | null

  /**
   * Non-empty when the bytes may be rendered inline, e.g. "image/png". Only
   * ever pass this to `api.documentPreviewURL` — never `mime_type`, which is
   * whatever the uploader claimed.
   */
  preview_type: string

  link_count: number
  links: DocumentLink[]

  /** Cached OCR result, or null if this receipt has never been read. */
  extraction: StoredExtraction | null

  created_at: string
  updated_at: string
}

export interface DocumentFilters {
  doc_type?: DocumentType | ''
  search?: string
  from?: string
  to?: string
  expiring_before?: string
  /** Omit for everything; true = attached to something, false = standalone. */
  linked?: boolean
}

export interface DocumentStorage {
  bytes_used: number
  /** 0 means unlimited. */
  quota_bytes: number
  max_file_bytes: number
  document_count: number
  /** Where ciphertext lands, e.g. "local:/var/lib/ledgermancy/documents". */
  backend: string
  ocr_enabled: boolean
}

export interface DocumentMetadata {
  title: string
  doc_type: DocumentType
  is_shared: boolean
  document_date: string | null
  expires_at: string | null
  notes: string | null
}

export interface DocumentUpload extends DocumentMetadata {
  file: File
  link?: DocumentTarget
}

/** A transaction a receipt's amount and date could belong to. */
export interface ReceiptMatch {
  transaction_id: string
  /** When the charge posted. */
  date: string
  /**
   * When the card was actually swiped, where the institution reports it. This
   * is what the date printed on a receipt corresponds to, and it explains a
   * match whose posted date looks days off.
   */
  authorized_date: string | null
  amount: string
  label: string
}

/**
 * What OCR last read off a receipt, cached on the document.
 *
 * Its existence is why re-opening a receipt is free: the fields are already
 * here, so nothing is sent to the AI provider a second time. Null when the
 * receipt has never been read.
 */
export interface StoredExtraction {
  extracted_at: string
  merchant: string
  /** Empty when the model declined to guess rather than misread it. */
  total: string
  date: string
  confidence: number
  notes: string
}

/**
 * Fields read off a receipt image. Every one is a *suggestion* awaiting
 * confirmation — nothing has been written, and a blank field means the model
 * declined to guess rather than that the receipt was empty.
 */
export interface ReceiptExtraction {
  merchant: string
  total: string
  date: string
  currency: string
  confidence: number
  notes: string
  matches: ReceiptMatch[]
}

/** One turn in a chatbot conversation. */
export interface ChatTurn {
  role: 'user' | 'assistant'
  content: string
}

/** An API error carrying the HTTP status, so callers can branch on 401 etc. */
export class ApiError extends Error {
  // Declared and assigned explicitly rather than as a constructor parameter
  // property, which `erasableSyntaxOnly` disallows.
  status: number

  constructor(status: number, message: string) {
    super(message)
    this.name = 'ApiError'
    this.status = status
  }
}

const CSRF_COOKIE = 'ledgermancy_csrf'

function readCookie(name: string): string | null {
  const match = document.cookie.match(
    new RegExp(`(?:^|; )${name.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}=([^;]*)`),
  )
  return match ? decodeURIComponent(match[1]) : null
}

/**
 * Returns the current CSRF token, asking the server for one if this client
 * does not have a cookie yet.
 */
async function ensureCsrfToken(): Promise<string> {
  const existing = readCookie(CSRF_COOKIE)
  if (existing) return existing

  const res = await fetch('/api/auth/csrf', { credentials: 'include' })
  if (!res.ok) throw new ApiError(res.status, 'could not obtain a CSRF token')

  const body: { csrf_token: string } = await res.json()
  return body.csrf_token
}

const UNSAFE = new Set(['POST', 'PUT', 'PATCH', 'DELETE'])

/**
 * Refuses a write the browser knows it cannot deliver.
 *
 * The UI disables the obvious write controls when offline, but "obvious" is
 * doing a lot of work in that sentence — there are dozens of them and more
 * arrive with every feature. This is the backstop that turns the ones nobody
 * remembered into a sentence the user can act on, instead of a bare "Failed to
 * fetch" from somewhere in TanStack Query.
 *
 * Nothing is queued for replay. Replaying a recategorisation against data that
 * moved while the phone was in a tunnel is a correctness problem, not a
 * plumbing one, and a half-built version of it is worse than none.
 */
function assertOnline(): void {
  if (typeof navigator !== 'undefined' && navigator.onLine === false) {
    throw new ApiError(
      503,
      "You're offline, so this change can't be saved. It has not been queued — try again once you reconnect.",
    )
  }
}

async function request<T>(
  method: string,
  path: string,
  body?: unknown,
): Promise<T> {
  const headers: Record<string, string> = {}
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  if (UNSAFE.has(method)) {
    assertOnline()
    headers['X-CSRF-Token'] = await ensureCsrfToken()
  }

  const res = await fetch(path, {
    method,
    headers,
    credentials: 'include',
    body: body === undefined ? undefined : JSON.stringify(body),
  })

  // Tells the offline banner whether this came off the network or off disk.
  noteResponseOrigin(res)

  if (res.status === 204) return undefined as T

  // Errors always arrive as {"error": "..."}, but a proxy or crash could still
  // produce non-JSON, so fall back to the status text rather than throwing a
  // parse error that hides the real failure.
  if (!res.ok) {
    let message = res.statusText
    try {
      const parsed = await res.json()
      if (parsed?.error) message = parsed.error
    } catch {
      /* keep statusText */
    }
    throw new ApiError(res.status, message)
  }

  return (await res.json()) as T
}

export const api = {
  register: (input: {
    email: string
    password: string
    display_name: string
    household_name?: string
    invite_token?: string
  }) => request<User>('POST', '/api/auth/register', input),

  login: (input: { email: string; password: string }) =>
    request<LoginResult>('POST', '/api/auth/login', input),

  logout: () => request<void>('POST', '/api/auth/logout'),

  me: () => request<User>('GET', '/api/auth/me'),

  // --- Security ------------------------------------------------------------
  // The second step of a login. It needs no token from us: the challenge rides
  // in an httpOnly cookie the browser sends automatically, so a script on this
  // page cannot read or forward a half-completed sign-in.
  verifyMFA: (input: { code?: string; recovery_code?: string }) =>
    request<LoginResult>('POST', '/api/auth/mfa/verify', input),

  mfaStatus: () => request<MFAStatus>('GET', '/api/auth/mfa'),

  // The password is required again on every one of these. Holding a session is
  // not authority to change the factors that guard the account.
  mfaSetup: (password: string) =>
    request<MFASetup>('POST', '/api/auth/mfa/setup', { password }),

  mfaActivate: (code: string) =>
    request<RecoveryCodes>('POST', '/api/auth/mfa/activate', { code }),

  mfaDisable: (password: string, code: string) =>
    request<void>('POST', '/api/auth/mfa/disable', { password, code }),

  regenerateRecoveryCodes: (password: string) =>
    request<RecoveryCodes>('POST', '/api/auth/mfa/recovery-codes', { password }),

  changePassword: (input: {
    current_password: string
    new_password: string
    code?: string
  }) => request<void>('POST', '/api/auth/password', input),

  sessions: () => request<ActiveSession[]>('GET', '/api/auth/sessions'),

  revokeSession: (id: string) =>
    request<void>('DELETE', `/api/auth/sessions/${id}`),

  revokeOtherSessions: () =>
    request<void>('POST', '/api/auth/sessions/revoke-others'),

  authEvents: () => request<AuthEvent[]>('GET', '/api/auth/events'),

  household: () => request<Household>('GET', '/api/household/'),

  members: () => request<Member[]>('GET', '/api/household/members'),

  setMemberRole: (userId: string, role: Role) =>
    request<Member>('PUT', `/api/household/members/${userId}/role`, { role }),

  invites: () => request<Invite[]>('GET', '/api/household/invites'),

  createInvite: (input: CreateInviteInput) =>
    request<CreatedInvite>('POST', '/api/household/invites', input),

  deleteInvite: (id: string) =>
    request<void>('DELETE', `/api/household/invites/${id}`),

  // --- People ------------------------------------------------------------
  people: () => request<Person[]>('GET', '/api/household/people'),

  createPerson: (input: PersonInput) =>
    request<Person>('POST', '/api/household/people', input),

  updatePerson: (id: string, input: PersonInput) =>
    request<Person>('PUT', `/api/household/people/${id}`, input),

  deletePerson: (id: string) =>
    request<void>('DELETE', `/api/household/people/${id}`),

  // --- Allowance ---------------------------------------------------------
  allowance: (personId: string) =>
    request<Allowance>('GET', `/api/household/people/${personId}/allowance`),

  saveAllowance: (
    personId: string,
    input: {
      amount: string | null
      cadence: string | null
      monthly_limit: string | null
      auto_post: boolean
    },
  ) => request<Allowance>('PUT', `/api/household/people/${personId}/allowance`, input),

  allowanceEntries: (personId: string) =>
    request<AllowanceEntry[]>(
      'GET',
      `/api/household/people/${personId}/allowance/entries`,
    ),

  addAllowanceEntry: (personId: string, input: AllowanceEntryInput) =>
    request<AllowanceEntry>(
      'POST',
      `/api/household/people/${personId}/allowance/entries`,
      input,
    ),

  deleteAllowanceEntry: (entryId: string) =>
    request<void>('DELETE', `/api/household/allowance/entries/${entryId}`),

  // --- The signed-in person's own surface --------------------------------
  // Everything under /me is scoped to the caller and is the only data a child
  // login can reach.
  myPerson: () => request<Person>('GET', '/api/me/person'),

  updateMyPerson: (input: PersonInput) =>
    request<Person>('PUT', '/api/me/person', input),

  myAllowance: () => request<Allowance>('GET', '/api/me/allowance'),

  myAllowanceEntries: () =>
    request<AllowanceEntry[]>('GET', '/api/me/allowance/entries'),

  /** A child records their own spending. Credits are a parent's action. */
  recordMySpend: (amount: string, note?: string) =>
    request<AllowanceEntry>('POST', '/api/me/allowance/entries', {
      kind: 'spend',
      amount,
      note: note ?? null,
    }),

  myAccounts: () => request<MyAccount[]>('GET', '/api/me/accounts'),

  myGoals: () => request<Goal[]>('GET', '/api/me/goals'),

  // --- Bill split --------------------------------------------------------
  splitTransactions: () => request<SplitTransaction[]>('GET', '/api/splits/'),

  householdLedger: () => request<LedgerEntry[]>('GET', '/api/splits/ledger'),

  transactionSplits: (transactionId: string) =>
    request<TransactionSplits>('GET', `/api/splits/transactions/${transactionId}`),

  /**
   * Replaces the whole split set. Send `equal` to divide evenly (the server
   * resolves the remainder), or `shares` with exact amounts. The server refuses
   * the write unless the shares sum to the transaction exactly.
   */
  setTransactionSplits: (
    transactionId: string,
    input:
      | { equal: string[] }
      | { shares: { person_id: string; amount: string }[] },
  ) => request<TransactionSplits>('PUT', `/api/splits/transactions/${transactionId}`, input),

  clearTransactionSplits: (transactionId: string) =>
    request<void>('DELETE', `/api/splits/transactions/${transactionId}`),

  settleSplit: (splitId: string) =>
    request<void>('POST', `/api/splits/${splitId}/settle`),

  unsettleSplit: (splitId: string) =>
    request<void>('DELETE', `/api/splits/${splitId}/settle`),

  // --- Goal contributions ------------------------------------------------
  goalContributions: (goalId: string) =>
    request<GoalContributions>('GET', `/api/goals/${goalId}/contributions`),

  addGoalContribution: (
    goalId: string,
    input: { person_id?: string; amount: string; occurred_on?: string; note?: string },
  ) => request<GoalContribution>('POST', `/api/goals/${goalId}/contributions`, input),

  deleteGoalContribution: (contributionId: string) =>
    request<void>('DELETE', `/api/goals/contributions/${contributionId}`),

  // --- Whose money is it -------------------------------------------------
  /** Tags an account with the person it is held for. Null clears it. */
  setAccountBeneficiary: (accountId: string, personId: string | null) =>
    request<{ id: string; name: string; beneficiary_person_id: string | null }>(
      'PATCH',
      `/api/investments/accounts/${accountId}/beneficiary`,
      { person_id: personId },
    ),

  netWorthByPerson: () => request<NetWorthByPerson>('GET', '/api/networth/by-person'),

  // --- Plaid -------------------------------------------------------------
  /**
   * Mints a Link token. Pass an item id to open Link in *update mode*, which
   * re-authenticates that institution in place and keeps its history, rather
   * than linking a new one.
   */
  createLinkToken: (itemId?: string) =>
    request<{ link_token: string }>(
      'POST',
      '/api/plaid/link-token',
      itemId ? { item_id: itemId } : undefined,
    ),

  exchangePublicToken: (publicToken: string) =>
    request<PlaidItem>('POST', '/api/plaid/exchange', {
      public_token: publicToken,
    }),

  items: () => request<PlaidItem[]>('GET', '/api/plaid/items'),

  syncItem: (id: string) =>
    request<SyncResult>('POST', `/api/plaid/items/${id}/sync`),

  /** Clears an item's error state after Link update mode succeeds. */
  itemReconnected: (id: string) =>
    request<PlaidItem>('POST', `/api/plaid/items/${id}/reconnected`),

  setItemSharing: (id: string, isShared: boolean) =>
    request<PlaidItem>('PATCH', `/api/plaid/items/${id}/sharing`, {
      is_shared: isShared,
    }),

  deleteItem: (id: string) => request<void>('DELETE', `/api/plaid/items/${id}`),

  // --- Ledger ------------------------------------------------------------
  accounts: () => request<Account[]>('GET', '/api/accounts'),

  transactions: (params: TransactionQuery = {}) =>
    request<Transaction[]>('GET', withQuery('/api/transactions', params)),

  recategorise: (
    transactionID: string,
    categoryID: string,
    applyToMerchant: boolean,
  ) =>
    request<{ id: string; category_id: string; category_source: string }>(
      'PATCH',
      `/api/transactions/${transactionID}/category`,
      { category_id: categoryID, apply_to_merchant: applyToMerchant },
    ),

  createTransaction: (input: ManualTransactionInput) =>
    request<{ id: string; source: string }>('POST', '/api/transactions', input),

  // Imports pre-mapped CSV rows into one account. The caller has already turned
  // each row into a signed amount (positive = spending, negative = money in),
  // so the server never sees the source bank's column layout.
  importTransactions: (input: {
    account_id: string
    rows: { date: string; amount: string; description: string }[]
  }) => request<ImportResult>('POST', '/api/transactions/import', input),

  updateTransaction: (id: string, input: ManualTransactionInput) =>
    request<{ id: string; source: string }>('PUT', `/api/transactions/${id}`, input),

  deleteTransaction: (id: string) =>
    request<void>('DELETE', `/api/transactions/${id}`),

  categories: () => request<Category[]>('GET', '/api/categories'),

  createCategory: (input: CategoryWrite) =>
    request<Category>('POST', '/api/categories', input),

  updateCategory: (id: string, input: CategoryWrite) =>
    request<Category>('PUT', `/api/categories/${id}`, input),

  deleteCategory: (id: string) =>
    request<void>('DELETE', `/api/categories/${id}`),

  // --- Merchants ----------------------------------------------------------
  // Canonical merchants and the review queue for proposed merges. Everything
  // the suggestion job writes is inert until confirmed here, so nothing on this
  // page can change a number elsewhere without an explicit action.
  merchantGroups: () => request<Merchant[]>('GET', '/api/merchants'),

  /** Every raw descriptor, for the manual merge picker. */
  merchantKeys: () => request<MerchantKeyStat[]>('GET', '/api/merchants/keys'),

  /**
   * Merge descriptors into one merchant. Pass entityId to confirm a suggestion
   * or extend an existing merchant; omit it to create a new one.
   *
   * reject_keys confirms PART of a proposal: the listed descriptors are recorded
   * as a different business in the same request, so the next suggestion pass
   * proposes them separately instead of re-proposing the grouping just declined.
   */
  mergeMerchants: (input: {
    merchant_keys: string[]
    entity_id?: string
    canonical_name?: string
    reject_keys?: string[]
  }) => request<MergeResult>('POST', '/api/merchants/merge', input),

  /** Dismiss proposed descriptors and remember the refusal. */
  rejectMerchantSuggestion: (merchantId: string, merchantKeys: string[]) =>
    request<void>('POST', `/api/merchants/${merchantId}/reject`, {
      merchant_keys: merchantKeys,
    }),

  /** Detach a descriptor from its merchant, undoing an over-eager merge. */
  splitMerchant: (merchantKey: string) =>
    request<void>('POST', '/api/merchants/split', { merchant_key: merchantKey }),

  renameMerchant: (merchantId: string, canonicalName: string) =>
    request<Merchant>('PATCH', `/api/merchants/${merchantId}`, {
      canonical_name: canonicalName,
    }),

  /** Run a suggestion pass now instead of waiting for the daily sweep. */
  scanMerchants: () => request<void>('POST', '/api/merchants/scan'),

  /**
   * One merchant's detail for a period, addressed by RESOLVED key — an entity id
   * for a grouped merchant, the raw descriptor otherwise. The key travels as a
   * query parameter because a descriptor can contain a slash.
   */
  merchantDetail: (key: string, params: PeriodQuery = {}) =>
    request<MerchantDetail>(
      'GET',
      withQuery('/api/merchants/detail', { ...params, key }),
    ),

  // --- Reports ------------------------------------------------------------
  summary: (params: PeriodQuery = {}) =>
    request<Summary>('GET', withQuery('/api/reports/summary', params)),

  byCategory: (params: PeriodQuery = {}) =>
    request<CategorySpend[]>('GET', withQuery('/api/reports/by-category', params)),

  byDay: (params: PeriodQuery = {}) =>
    request<DaySpend[]>('GET', withQuery('/api/reports/by-day', params)),

  merchants: (params: PeriodQuery & { limit?: number } = {}) =>
    request<MerchantSpend[]>('GET', withQuery('/api/reports/merchants', params)),

  trend: (params: PeriodQuery = {}) =>
    request<TrendPoint[]>('GET', withQuery('/api/reports/trend', params)),

  averages: (params: PeriodQuery = {}) =>
    request<CategoryAverage[]>('GET', withQuery('/api/reports/averages', params)),

  // --- Budgets ------------------------------------------------------------
  budgets: (params: PeriodQuery = {}) =>
    request<BudgetProgress[]>('GET', withQuery('/api/budgets', params)),

  setBudget: (
    categoryID: string,
    amount: string,
    period: 'weekly' | 'monthly' | 'yearly' = 'monthly',
    rollover = false,
  ) =>
    request<{ id: string }>('POST', '/api/budgets', {
      category_id: categoryID,
      amount,
      period,
      rollover,
    }),

  deleteBudget: (id: string) => request<void>('DELETE', `/api/budgets/${id}`),

  // How much is left to spend freely this month after fixed costs, discretionary
  // budgets, and goal contributions — computed server-side from typical income.
  safeToSpend: () =>
    request<SafeToSpend>('GET', '/api/budgets/safe-to-spend'),

  // Proposes a round budget target per spending category, anchored on each
  // category's true average. Works with or without AI (rule-based rounding when
  // off); ai_tailored says which. Approval is a loop of setBudget, unchanged.
  suggestBudgets: () =>
    request<BudgetSuggestions>('POST', '/api/budgets/suggest'),

  // --- Bill calendar -------------------------------------------------------
  // Obligations are the stored rules; /upcoming expands them into dated
  // occurrences and /projection carries balances forward through those. Both
  // derived views come from one server-side expansion, so they cannot disagree
  // about when a bill is due.
  obligations: () => request<Obligation[]>('GET', '/api/obligations'),

  createObligation: (input: ObligationInput) =>
    request<Obligation>('POST', '/api/obligations', input),

  updateObligation: (id: string, input: ObligationInput) =>
    request<Obligation>('PUT', `/api/obligations/${id}`, input),

  // Deletes a manual obligation; deactivates a detected one, which would
  // otherwise be recreated by the next detection pass.
  deleteObligation: (id: string) =>
    request<void>('DELETE', `/api/obligations/${id}`),

  upcomingObligations: (days = 30) =>
    request<UpcomingObligations>('GET', withQuery('/api/obligations/upcoming', { days })),

  obligationProjection: (days = 30) =>
    request<BalanceProjection>('GET', withQuery('/api/obligations/projection', { days })),

  // --- Goals --------------------------------------------------------------
  goals: () => request<Goal[]>('GET', '/api/goals'),

  createGoal: (input: GoalInput) => request<Goal>('POST', '/api/goals', input),

  updateGoal: (id: string, input: GoalInput) =>
    request<Goal>('PUT', `/api/goals/${id}`, input),

  archiveGoal: (id: string) => request<void>('DELETE', `/api/goals/${id}`),

  // Parses a natural-language goal into a confirmable proposal. Never writes —
  // confirmation calls createGoal. 503 when AI is off, 422 on an unreadable parse.
  parseGoal: (text: string) =>
    request<GoalProposal>('POST', '/api/goals/parse', { text }),

  // --- Net worth ----------------------------------------------------------
  netWorth: () => request<NetWorth>('GET', '/api/networth'),

  netWorthHistory: (params: PeriodQuery = {}) =>
    request<NetWorthPoint[]>('GET', withQuery('/api/networth/history', params)),

  snapshotNetWorth: () => request<NetWorth>('POST', '/api/networth/snapshot'),

  projection: (params: ProjectionQuery = {}) =>
    request<Projection>('GET', withQuery('/api/networth/projection', params)),

  holdings: () => request<Holding[]>('GET', '/api/holdings'),

  liabilities: () => request<Liability[]>('GET', '/api/liabilities'),

  manualAssets: () => request<ManualAsset[]>('GET', '/api/manual-assets'),

  createManualAsset: (input: {
    name: string
    kind: string
    value: string
    is_liability: boolean
  }) => request<ManualAsset>('POST', '/api/manual-assets', input),

  deleteManualAsset: (id: string) =>
    request<void>('DELETE', `/api/manual-assets/${id}`),

  // --- Investments --------------------------------------------------------
  investments: () => request<InvestmentOverview>('GET', '/api/investments/'),

  investmentPerformance: (period: InvestmentPeriod) =>
    request<InvestmentPerformance>(
      'GET',
      withQuery('/api/investments/performance', { period }),
    ),

  investmentBenchmarks: (period: InvestmentPeriod) =>
    request<BenchmarkComparison>(
      'GET',
      withQuery('/api/investments/benchmarks', { period }),
    ),

  investmentAllocation: () =>
    request<InvestmentAllocation>('GET', '/api/investments/allocation'),

  investmentHoldings: () =>
    request<DetailedHolding[]>('GET', '/api/investments/holdings'),

  investmentFees: () => request<FeeDrag>('GET', '/api/investments/fees'),

  investmentDividends: () =>
    request<DividendIncome>('GET', '/api/investments/dividends'),

  // Confirms a classification. Passing null clears it back to untagged, which
  // is a legitimate action — "I do not know" beats a wrong tag.
  setAccountTaxTreatment: (
    accountID: string,
    input: { tax_treatment: TaxTreatment | null; is_managed: boolean | null },
  ) =>
    request<{
      id: string
      name: string
      tax_treatment: TaxTreatment | null
      is_managed: boolean | null
    }>('PATCH', `/api/investments/accounts/${accountID}/tax-treatment`, input),

  // --- Retirement ---------------------------------------------------------
  retirementAssumptions: () =>
    request<RetirementAssumptions>('GET', '/api/projections/assumptions'),

  // Every field is sent on every save: the form is the whole state, so clearing
  // an age genuinely clears it rather than leaving a stale value behind.
  saveRetirementAssumptions: (input: AssumptionsInput) =>
    request<RetirementAssumptions>('PUT', '/api/projections/assumptions', input),

  retirementContributions: () =>
    request<ContributionsResponse>('GET', '/api/projections/contributions'),

  // Returns the refreshed list, so headroom updates without a second round trip.
  saveContribution: (accountID: string, input: ContributionInput) =>
    request<ContributionsResponse>(
      'PUT',
      `/api/projections/contributions/${accountID}`,
      input,
    ),

  // Clears a plan rather than storing zeroes, so "no plan set" and "planning to
  // contribute nothing" stay distinguishable.
  deleteContribution: (accountID: string) =>
    request<void>('DELETE', `/api/projections/contributions/${accountID}`),

  retirementProjection: (params: { months?: number; volatility?: string } = {}) =>
    request<RetirementResponse>(
      'GET',
      withQuery('/api/projections/retirement', params),
    ),

  // --- Alerts -------------------------------------------------------------
  alerts: () => request<Alert[]>('GET', '/api/alerts/'),

  createAlert: (
    type: AlertType,
    config: Record<string, string | number>,
    enabled: boolean,
    push: boolean,
  ) => request<Alert>('POST', '/api/alerts/', { type, config, enabled, push }),

  // The backend keeps an existing alert's type; only config, enabled and push
  // change.
  updateAlert: (
    id: string,
    config: Record<string, string | number>,
    enabled: boolean,
    push: boolean,
  ) => request<Alert>('PUT', `/api/alerts/${id}`, { config, enabled, push }),

  deleteAlert: (id: string) => request<void>('DELETE', `/api/alerts/${id}`),

  // Parses a natural-language sentence into a confirmable alert/budget proposal.
  // Never writes — confirmation calls createAlert/updateAlert/setBudget. Returns
  // kind 'unsupported' (not an error) when the request can't be enforced.
  parseAlert: (text: string) =>
    request<ParseRuleResult>('POST', '/api/alerts/parse', { text }),

  alertEvents: () => request<AlertEvent[]>('GET', '/api/alerts/events'),

  unreadAlertCount: () =>
    request<{ count: number }>('GET', '/api/alerts/events/unread-count'),

  markAlertRead: (id: string) =>
    request<void>('POST', `/api/alerts/events/${id}/read`),

  markAllAlertsRead: () => request<void>('POST', '/api/alerts/events/read-all'),

  // --- Preferences --------------------------------------------------------
  preferences: () => request<Preferences>('GET', '/api/preferences'),

  setPreferences: (items: PreferenceWrite[]) =>
    request<void>('PUT', '/api/preferences', { items }),

  // Sends one throwaway push to the caller's saved channel, synchronously, so
  // the UI can report the real outcome. Errors (unconfigured, bad topic,
  // unreachable server) come back as a thrown request error.
  testNotification: () =>
    request<{ status: string }>('POST', '/api/notifications/test'),

  // Queues a one-off digest for the caller now, bypassing cadence/dedupe. Async
  // — resolves once queued; the push itself arrives shortly after.
  sendDigestNow: () => request<{ status: string }>('POST', '/api/digest/test'),

  // --- Continuity (owner-only operator surface) ---------------------------
  continuity: () => request<Continuity>('GET', '/api/admin/continuity'),

  // Records that the operator has stored ENCRYPTION_KEY somewhere safe. The
  // app cannot verify this; asking is the point.
  acknowledgeKeyBackup: () =>
    request<void>('POST', '/api/admin/continuity/key-ack'),

  // Queues a backup cycle or a restore test now. Resolves once queued.
  runContinuityJob: (kind: 'backup' | 'restore_test') =>
    request<{ status: string }>('POST', '/api/admin/continuity/run', { kind }),

  // --- Insights -----------------------------------------------------------
  capabilities: () => request<Capabilities>('GET', '/api/capabilities'),

  // The proactive feed. state 'all' includes dismissed insights; the default
  // 'unread' hides them.
  insights: (params: { state?: 'unread' | 'all' } = {}) =>
    request<Insight[]>('GET', withQuery('/api/insights/', params)),

  markInsightRead: (id: string) =>
    request<void>('POST', `/api/insights/${id}/read`),

  dismissInsight: (id: string) =>
    request<void>('POST', `/api/insights/${id}/dismiss`),

  recurring: () =>
    request<RecurringMerchant[]>('GET', '/api/reports/recurring'),

  /** Mark a merchant "not recurring" so it drops out of the detector everywhere. */
  suppressRecurring: (merchantKey: string, merchant: string) =>
    request<void>('POST', '/api/reports/recurring/suppress', {
      merchant_key: merchantKey,
      merchant,
    }),

  /** Restore a previously-suppressed merchant to the detector. */
  unsuppressRecurring: (merchantKey: string) =>
    request<void>(
      'DELETE',
      withQuery('/api/reports/recurring/suppress', { merchant_key: merchantKey }),
    ),

  /** The household's suppressed merchants, for the restore list. */
  suppressedRecurring: () =>
    request<SuppressedRecurringMerchant[]>(
      'GET',
      '/api/reports/recurring/suppressed',
    ),

  monthlySummary: (month: string) =>
    request<MonthlySummary>(
      'GET',
      withQuery('/api/reports/monthly-summary', { month }),
    ),

  generateMonthlySummary: (month: string) =>
    request<MonthlySummary>(
      'POST',
      withQuery('/api/reports/monthly-summary', { month }),
    ),

  // --- Document vault ------------------------------------------------------
  documents: (filters: DocumentFilters = {}) =>
    request<VaultDocument[]>('GET', withQuery('/api/documents/', filters)),

  document: (id: string) => request<VaultDocument>('GET', `/api/documents/${id}`),

  /** Storage used against the quota, plus the limits the UI states up front. */
  documentStorage: () =>
    request<DocumentStorage>('GET', '/api/documents/storage'),

  /** The documents attached to one ledger record — what a paperclip expands into. */
  attachedDocuments: (target: DocumentTarget) =>
    request<VaultDocument[]>(
      'GET',
      withQuery('/api/documents/attached', targetQuery(target)),
    ),

  /**
   * Attachment counts for a page of transactions, keyed by transaction id.
   * One request per page rather than one per row.
   */
  documentCounts: (transactionIds: string[]) =>
    request<Record<string, number>>(
      'GET',
      `/api/documents/counts?${transactionIds
        .map((id) => `transaction_id=${encodeURIComponent(id)}`)
        .join('&')}`,
    ),

  uploadDocument: (input: DocumentUpload) => uploadDocument(input),

  updateDocument: (id: string, input: DocumentMetadata) =>
    request<VaultDocument>('PUT', `/api/documents/${id}`, input),

  deleteDocument: (id: string) =>
    request<void>('DELETE', `/api/documents/${id}`),

  linkDocument: (id: string, target: DocumentTarget) =>
    request<DocumentLink>('POST', `/api/documents/${id}/links`, {
      target_kind: target.kind,
      target_id: target.id,
    }),

  unlinkDocument: (documentId: string, linkId: string) =>
    request<void>('DELETE', `/api/documents/${documentId}/links/${linkId}`),

  /**
   * Reads the fields off a receipt image. Returns *suggestions only* — nothing
   * is written until the user confirms them, by design.
   *
   * This is the only call that sends the image to the AI provider, so it is
   * never made automatically. The reading it produces is cached on the
   * document, and `documentMatches` re-checks against it for free — call this
   * once per receipt and re-read only when the user explicitly asks.
   */
  extractReceipt: (id: string) =>
    request<ReceiptExtraction>('POST', `/api/documents/${id}/extract`),

  /**
   * Re-runs the transaction match against a receipt's already-read fields.
   *
   * Costs nothing: no decryption, no upload, no model call. This is what finds
   * the charge for a receipt that was scanned before it posted.
   */
  documentMatches: (id: string) =>
    request<ReceiptMatch[]>('GET', `/api/documents/${id}/matches`),

  /**
   * Fetches a document's bytes as an object URL for inline preview.
   *
   * The blob's type is forced to the caller-supplied `previewType`, which the
   * server derived from a short allowlist (images and PDF). That matters:
   * blob: URLs inherit this origin, so a document that got to choose its own
   * type could render as HTML here. Callers must pass a non-empty
   * `preview_type` from the document, never the raw `mime_type`.
   */
  documentPreviewURL: (id: string, previewType: string) =>
    documentObjectURL(id, previewType),

  // The chat endpoint streams its answer as Server-Sent Events: one
  // {"delta":"…"} frame per chunk, a terminal {"done":true}, or {"error":"…"}.
  // onDelta is called as text arrives so the UI can render it live.
  chat: (messages: ChatTurn[], onDelta: (text: string) => void) =>
    streamChat(messages, onDelta),
}

/**
 * Uploads a document as multipart/form-data.
 *
 * It bypasses `request` because the body is a FormData, not JSON: setting a
 * Content-Type by hand would omit the multipart boundary the browser
 * generates. The CSRF header is still required, so it is attached explicitly.
 */
async function uploadDocument(input: DocumentUpload): Promise<VaultDocument> {
  const form = new FormData()
  form.set('file', input.file)
  form.set('title', input.title)
  form.set('doc_type', input.doc_type)
  form.set('is_shared', String(input.is_shared))
  if (input.document_date) form.set('document_date', input.document_date)
  if (input.expires_at) form.set('expires_at', input.expires_at)
  if (input.notes) form.set('notes', input.notes)
  if (input.link) {
    form.set('link_kind', input.link.kind)
    form.set('link_id', input.link.id)
  }

  assertOnline()

  const res = await fetch('/api/documents/', {
    method: 'POST',
    headers: { 'X-CSRF-Token': await ensureCsrfToken() },
    credentials: 'include',
    body: form,
  })

  if (!res.ok) {
    let message = res.statusText
    try {
      const parsed = await res.json()
      if (parsed?.error) message = parsed.error
    } catch {
      /* keep statusText */
    }
    throw new ApiError(res.status, message)
  }
  return (await res.json()) as VaultDocument
}

/** See api.documentPreviewURL for why the type is forced rather than inherited. */
async function documentObjectURL(
  id: string,
  previewType: string,
): Promise<string> {
  const res = await fetch(`/api/documents/${id}/download`, {
    credentials: 'include',
  })
  if (!res.ok) throw new ApiError(res.status, 'could not load the document')

  const bytes = await res.arrayBuffer()
  return URL.createObjectURL(new Blob([bytes], { type: previewType }))
}

function targetQuery(target: DocumentTarget): Record<string, string> {
  return { [`${target.kind}_id`]: target.id }
}

// streamChat POSTs the transcript and reads the SSE body, invoking onDelta for
// each token. It resolves when the stream reports done and rejects on an error
// frame or a transport failure, so callers can await completion.
async function streamChat(
  messages: ChatTurn[],
  onDelta: (text: string) => void,
): Promise<void> {
  assertOnline()

  const res = await fetch('/api/chat', {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      'X-CSRF-Token': await ensureCsrfToken(),
      Accept: 'text/event-stream',
    },
    credentials: 'include',
    body: JSON.stringify({ messages }),
  })

  if (!res.ok || !res.body) {
    let message = res.statusText
    try {
      const parsed = await res.json()
      if (parsed?.error) message = parsed.error
    } catch {
      /* keep statusText */
    }
    throw new ApiError(res.status, message)
  }

  const reader = res.body.getReader()
  const decoder = new TextDecoder()
  let buffer = ''

  // SSE frames are separated by a blank line; each frame's payload is the
  // concatenation of its `data:` lines. We only ever emit single-line frames,
  // but parse defensively.
  const handleFrame = (frame: string) => {
    const data = frame
      .split('\n')
      .filter((l) => l.startsWith('data:'))
      .map((l) => l.slice(5).trim())
      .join('')
    if (!data) return
    const evt = JSON.parse(data) as {
      delta?: string
      done?: boolean
      error?: string
    }
    if (evt.error) throw new ApiError(500, evt.error)
    if (evt.delta) onDelta(evt.delta)
  }

  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    buffer += decoder.decode(value, { stream: true })
    let sep: number
    while ((sep = buffer.indexOf('\n\n')) !== -1) {
      const frame = buffer.slice(0, sep)
      buffer = buffer.slice(sep + 2)
      handleFrame(frame)
    }
  }
  if (buffer.trim()) handleFrame(buffer)
}

// Generic rather than Record<string, unknown>: an interface without an index
// signature is not assignable to Record, so PeriodQuery would be rejected.
function withQuery<T extends object>(path: string, params: T): string {
  const search = new URLSearchParams()
  for (const [key, value] of Object.entries(params)) {
    if (value !== undefined && value !== null && value !== '') {
      search.set(key, String(value))
    }
  }
  const qs = search.toString()
  return qs ? `${path}?${qs}` : path
}
