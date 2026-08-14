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

/** A personal API token as its owner sees it in the revoke list. */
export interface ApiToken {
  id: string
  name: string
  /** `read` is always present; `write` is what makes it read-write. */
  scopes: string[]
  /** Null until something authenticates with it. */
  last_used_at: string | null
  /** Null means it never expires, which is the normal case. */
  expires_at: string | null
  created_at: string
}

/**
 * The one response that carries the plaintext token.
 *
 * The server stores only an HMAC, so this value exists exactly once, in this
 * response. It is held in component state to be shown and copied, and is never
 * written to storage — there would be nowhere to retrieve it from anyway.
 */
export interface CreatedApiToken extends ApiToken {
  token: string
}

export interface AuthEvent {
  event_type: string
  client_ip: string | null
  user_agent: string | null
  metadata: Record<string, unknown>
  created_at: string
}

/** An outgoing webhook subscription. The signing secret is never in here. */
export interface Webhook {
  id: string
  /**
   * The member who created it. Load-bearing rather than decorative: alert
   * deliveries are filtered by what this user can see, so a webhook created by
   * one partner never carries the other's private-account alerts.
   */
  user_id: string
  name: string
  url: string
  /** A paused subscription keeps its delivery history and stops receiving. */
  active: boolean
  triggers: string[]
  created_at: string
  updated_at: string
}

/**
 * The one response that carries the signing secret.
 *
 * Returned by a create or a rotate and never again — the server stores it
 * sealed and only the delivery worker ever opens it. Held in component state
 * to be shown and copied, exactly like {@link CreatedApiToken}.
 */
export interface CreatedWebhook extends Webhook {
  secret: string
}

/** One event owed to one subscriber. */
export interface WebhookMessage {
  id: string
  webhook_id: string
  trigger: string
  /** The exact body that was (or will be) sent and signed. */
  payload: unknown
  /** pending until delivered; sent on a 2xx; failed once the retries run out. */
  status: 'pending' | 'sent' | 'failed'
  attempts: number
  delivered_at: string | null
  last_error: string | null
  created_at: string
}

/** One HTTP request and whatever came back — or why nothing did. */
export interface WebhookAttempt {
  id: string
  attempt: number
  request_headers: Record<string, string>
  request_body: string
  /** Null when the request never completed; `error` says why. */
  response_status: number | null
  response_headers: Record<string, string[]> | null
  response_body: string | null
  error: string | null
  duration_ms: number
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
   *  `institution_name` — two household members can link the same bank.
   *
   *  Null for a manual account, which belongs to no institution. Those group
   *  together under their own heading rather than under an invented key. */
  item_id: string | null
  name: string
  mask: string | null
  /** depository | credit | loan | investment | brokerage | other */
  type: string
  subtype: string | null
  institution_name: string | null
  /** Decimal serialised as a string — never parse into a JS number for maths. */
  current_balance: string | null
  available_balance: string | null
  currency: string
  is_own: boolean
  /** Which affordances the row offers. A Plaid account syncs and reconnects; a
   *  manual one is edited and has its balance set by hand. */
  source: 'plaid' | 'manual'
  tax_treatment: string | null
  is_shared: boolean
  /**
   * User-entered deposit yield as a PERCENT string ("4.50"), on a depository
   * account. Null means nobody has entered one — UNKNOWN, never zero. The
   * cash-drag detector stays silent on a null rather than reporting a
   * high-yield savings account as the household's worst drag.
   */
  deposit_apy: string | null
}

/** Fields the manual-account editor sends. Balance is a string like every other
 *  money value — a JS number would be a float. */
export interface ManualAccountInput {
  name: string
  type: string
  subtype?: string | null
  mask?: string | null
  currency?: string
  tax_treatment?: string | null
  is_shared?: boolean
  /** Opening balance, on create only. Later changes go through
   *  setManualAccountBalance, which records why the balance moved. */
  balance?: string | null
}

/** One dated balance for a manual account. `scheduled` rows were written by the
 *  auto-posting worker, not by hand. */
export interface AccountBalanceEntry {
  as_of: string
  balance: string
  reason: 'manual' | 'scheduled' | 'holding_revalue' | 'fee' | 'dividend' | 'snapshot'
  note: string | null
}

export interface SetBalanceInput {
  balance: string
  as_of?: string
  reason?: 'manual' | 'holding_revalue' | 'fee' | 'dividend'
  note?: string | null
}

export interface Security {
  id: string
  ticker: string | null
  name: string | null
  type: string | null
  close_price: string | null
  close_price_as_of: string | null
  currency: string
  source: 'plaid' | 'manual'
}

export interface SecurityInput {
  ticker: string
  name?: string | null
  type?: string | null
  cusip?: string | null
  isin?: string | null
  close_price?: string | null
  close_price_as_of?: string | null
  currency?: string
  is_cash_equivalent?: boolean
}

export interface HoldingInput {
  security_id: string
  /** Fractional shares are normal in a retirement plan, so this is a decimal
   *  string, not an integer. */
  quantity: string
  cost_basis?: string | null
  institution_price?: string | null
  as_of?: string
}

/** One recorded investment transaction on a manual account.
 *
 *  SIGN: `amount` is NEGATIVE for money moving INTO the portfolio (a
 *  contribution) and positive for money leaving it. This matches Plaid and is
 *  what the return calculations expect; getting it backwards inverts every
 *  performance figure rather than producing an error. */
export interface InvestmentTransaction {
  id: string
  date: string
  source: 'plaid' | 'manual' | 'scheduled'
  type: string
  subtype: string | null
  amount: string
  quantity: string | null
  price: string | null
  fees: string | null
  name: string | null
  ticker: string | null
  security_name: string | null
}

export interface InvestmentTransactionInput {
  account_id: string
  security_id?: string | null
  type: string
  subtype?: string | null
  amount: string
  quantity?: string | null
  price?: string | null
  fees?: string | null
  date: string
  name?: string | null
}

export interface Transaction {
  id: string
  date: string
  name: string
  merchant_name: string | null
  /**
   * The name to DISPLAY for this row: the canonical merchant name when the
   * descriptor has been grouped or renamed, otherwise the raw merchant_name/name
   * the bank sent. This is what the row and recent activity render — prefer it
   * over `merchant_name ?? name`, which shows stale bank text after a rename.
   */
  merchant: string
  /** Normalized key the app caches categories by; present even when
   * merchant_name is null, empty when there was too little signal to key on. */
  merchant_key: string | null
  /**
   * The key that addresses the MERCHANT rather than the descriptor: an entity id
   * for a merchant the household has grouped, the raw key otherwise. Always use
   * this one to link to the merchant detail view — `merchant_key` above would
   * strand every fragment of a grouped merchant but one. Empty when there was too
   * little signal to key on, and the name then renders as plain text.
   */
  merchant_key_resolved: string
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
  /** Only 'manual' rows can be edited or deleted. 'scheduled' rows were posted
   *  by the auto-posting worker from an obligation — editing one here would be
   *  undone the next time it posted, so the obligation is what you change. */
  source: 'plaid' | 'csv' | 'manual' | 'scheduled'
  /**
   * A hand-entered row that a later Plaid charge now appears to match (same
   * account, same amount, within four days) — likely the issuer finally
   * delivering the charge the user reconciled by hand.
   */
  possible_duplicate: boolean
  /**
   * Hidden from every report — a duplicate, or money that is not really the
   * household's. Rows with this set are only listed when the query asks for
   * them (`include_excluded`), which is the only way to clear the flag again.
   */
  excluded_from_reports: boolean
  /**
   * Real, but not repeating: a loan payoff, a tax bill, a car purchase. It still
   * counts in the month it fell — the Spending page shows it — but it is kept
   * out of the trailing averages that predict future months, so one unusual
   * month cannot rewrite the household's idea of its fixed costs.
   */
  is_one_time: boolean
  /**
   * The free-form labels on this row, orthogonal to `category_id` above: a
   * transaction has exactly one category and any number of tags. Always an
   * array — empty when nothing is labelled, never null.
   */
  tags: TransactionTag[]
}

/**
 * A tag as a ledger row renders it: id to act on, name to show. Deliberately
 * not the full {@link Tag} — a row has no use for the tag's all-time total, and
 * computing fifty of those per page would be a report query per chip.
 */
export interface TransactionTag {
  id: string
  name: string
}

/**
 * A relationship two transactions can stand in — "refunds", "relates to",
 * "paid for", or one the household invented.
 *
 * A link type is DIRECTED, and the two phrasings are the whole reason it is an
 * object rather than a string: `outward` is how the source end reads, `inward`
 * is how the target end reads, and one stored link serves both. A symmetric
 * relationship simply has the same string twice.
 */
export interface LinkType {
  id: string
  slug: string
  name: string
  /** How the SOURCE end reads: "refunds". */
  outward: string
  /** How the TARGET end reads: "is refunded by". */
  inward: string
  /**
   * Whether a link of this type is one the "Net linked refunds" view acts on.
   * Read-only and true only for the built-in Refund type — a household-defined
   * relationship never moves a reported figure. Sending it is rejected.
   */
  nets_spend: boolean
  /** One of the three shipped types, which cannot be renamed or deleted. */
  is_system: boolean
  created_at: string
}

/** Body for creating or editing one of the household's own link types. */
export interface LinkTypeInput {
  name: string
  outward: string
  inward: string
}

/**
 * The far end of a link, as a panel renders it. A trimmed ledger row — enough to
 * recognise the charge and open it, deliberately not the full {@link
 * Transaction}.
 */
export interface LinkedTransaction {
  id: string
  date: string
  name: string
  merchant: string
  amount: string
  currency: string
  category_id: string | null
  account_name: string
}

/**
 * One link, already oriented FROM the transaction it was read through.
 *
 * The same stored row yields a different `direction` and a different `relation`
 * depending on which end asked, which is why neither is derived here: the server
 * picks the verb, and the two readings of one edge cannot drift.
 */
export interface TransactionLink {
  id: string
  link_type_id: string
  link_type_slug: string
  link_type_name: string
  /** "outward" when the transaction you read through is the link's source. */
  direction: 'outward' | 'inward'
  /** The verb as this end reads it — already the outward or inward phrase. */
  relation: string
  /** True when this link is one the netting view acts on. */
  nets_spend: boolean
  transaction: LinkedTransaction
  created_at: string
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
  /**
   * Restrict to one merchant, by resolved key — so a grouped merchant returns
   * every descriptor's charges. This is what "Open in Transactions" on a merchant
   * page passes.
   */
  merchant?: string
  /**
   * A composable search query. Bare words are free text over the merchant name,
   * raw name and descriptor key — which is all `q` used to be, so existing links
   * keep working. `key:value` terms narrow further (`over:10`, `since:-30d`,
   * `has_no_category`), a leading `-` negates one, and everything is ANDed. See
   * the backend's `internal/search` for the vocabulary, or api.searchOperators().
   *
   * A query that names its own dates owns the date window: `from`/`to` are
   * ignored for it, so `since:2019-01-01` is not silently clipped to the page's
   * rolling year.
   */
  q?: string
  /** Only rows still needing a category (null or the fallback bucket). */
  uncategorised?: boolean
  /**
   * Also list rows flagged excluded_from_reports, which are hidden by default.
   * The ledger is the only place the flag can be cleared, so without this there
   * is no way back from excluding a row.
   */
  include_excluded?: boolean
  /**
   * Restrict to rows carrying ANY of these tags. Serialized comma-joined like
   * `accounts`. OR rather than AND: picking two widens the view to "the trip or
   * the remodel", which is the question a household actually asks.
   */
  tags?: string[]
  /** Only rows nobody has labelled yet — the tag counterpart of
   *  `uncategorised`, and how a backlog of untagged charges gets drained. */
  untagged?: boolean
}

/**
 * One operator the `q` grammar accepts. The list comes from the parser via
 * api.searchOperators() rather than being written out here, so the search bar
 * can never suggest something the server would treat as free text.
 */
export interface SearchOperator {
  /** The operator as typed, without the trailing colon. */
  name: string
  /** False for flags (`has_no_category`), which are written bare. */
  takes_value: boolean
  help: string
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
  /**
   * 0–1, or null when the period had no income (the ratio is meaningless).
   *
   * Still a real ratio when `in_progress` is true — but a ratio of a whole
   * month's spending SO FAR to however much of the month's income has landed,
   * which is not a savings rate. Do not render it as one; render the in-progress
   * state instead (MAD-110).
   */
  savings_rate: string | null
  transaction_count: number
  /**
   * True when this period is a single calendar month that has not ended, so
   * every figure above is month-to-date. False for a rolling window that merely
   * runs up to today — the trailing-twelve report's one partial month is diluted
   * across eleven whole ones and needs no caveat.
   */
  in_progress: boolean
  /** "YYYY-MM-DD" the figures run through. Present only when `in_progress`. */
  as_of?: string
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

/**
 * One income source for the cash-flow Sankey. Same shape as CategorySpend minus
 * `is_fixed`, because an income category is never fixed (the API forces
 * is_fixed = false for income). The rows sum to `CashFlow.income_total` to the
 * cent — there is no "uncategorised income" the way there can be uncategorised
 * spending, because a row only counts as income when its category is_income.
 */
export interface CashFlowSource {
  category_id: string
  name: string
  slug: string
  color: string | null
  total: string
  transaction_count: number
}

/**
 * The cash-flow Sankey payload (item #13, MAD-33): income sources on the left,
 * spending categories on the right, and the leftover flowing to savings.
 *
 * The three totals are the SAME figures `/api/reports/summary` returns for the
 * period — the Spending page's headline tiles — so the Sankey's bands reconcile
 * with that page to the dollar. The spending category flows plus
 * `uncategorized_spending` sum to `spending_total`, and the income source flows
 * sum to `income_total`. All money is computed server-side in NUMERIC; the
 * client only sizes display geometry from these strings.
 */
export interface CashFlow {
  from: string
  to: string
  income_total: string
  spending_total: string
  /** income_total − spending_total. Negative in a deficit period. */
  leftover: string
  /**
   * Spending whose category_id was null — present in `spending_total` but absent
   * from `spending_categories` (which comes from an INNER join on categories).
   * Usually zero; carried only so the flows always sum to `spending_total`.
   */
  uncategorized_spending: string
  income_sources: CashFlowSource[]
  /** Same rows `/api/reports/by-category` returns for the period. */
  spending_categories: CategorySpend[]
}

export interface TrendPoint {
  /** "YYYY-MM" */
  month: string
  income: string
  spending: string
  leftover: string
  /**
   * Spending decomposed into its fixed and discretionary buckets — same split
   * the period summary reports, per month. The two sum to `spending` to the
   * cent (computed server-side in exact decimal), which is what makes the
   * fixed-vs-discretionary stacked bar a decomposition of the headline rather
   * than a second story about the same money. Drives item #9 (MAD-34).
   */
  fixed_spending: string
  discretionary_spending: string
  /**
   * The same three headline figures in base-period dollars, present only when
   * the request asked for `real` AND this month has a published CPI index.
   *
   * `undefined` means the month CANNOT be deflated — it predates the series, or
   * it is one BLS never published. It never means "unchanged". Render a gap and
   * say so; never fall back to the nominal figure under a real label.
   */
  real_income?: string
  real_spending?: string
  real_leftover?: string
  /**
   * True on the one point the clock is currently inside. Its figures are real
   * but month-to-date: the spending is everything spent so far, the income is
   * however many paychecks have landed so far.
   *
   * The point is NOT dropped server-side, because the money did move — a chart
   * that plots amounts should keep it and mark it. A chart that plots a RATIO of
   * income to spending must not plot it at all: a whole month of spending over a
   * fraction of a month's income produces an arbitrarily large negative number,
   * and an axis fitted to it flattens every real month beside it (MAD-110).
   */
  in_progress: boolean
  /** "YYYY-MM-DD" this point runs through. Present only when `in_progress`. */
  as_of?: string
}

/**
 * The envelope `/api/reports/trend` returns. Carries the monthly `points`
 * alongside the request that produced them.
 *
 * `exclude_one_time` is the echo of the "Hide one-time charges" toggle: true
 * means `is_one_time` rows were dropped from every point, false means the
 * default real-period series. It exists so the three trend-fed charts on
 * Spending can never render filtered figures under an unfiltered label — the
 * react-query key already prevents a stale cache, and this makes the payload
 * self-describing too. Read it before labelling the chart, not just before
 * drawing it.
 */
export interface TrendResponse {
  points: TrendPoint[]
  exclude_one_time: boolean
  /**
   * True when every spending figure in the series has had its linked refunds
   * subtracted from the charge they refund, in the charge's own month. Income is
   * unaffected either way. Echoed back for the same reason `exclude_one_time`
   * is: a chart must never render netted figures under an un-netted label.
   */
  net_refunds: boolean
}

export interface CategoryAverage extends CategorySpend {
  monthly_average: string
}

/** One calendar day's spend. `day` is "YYYY-MM-DD". */
export interface DaySpend {
  day: string
  spending: string
}

/**
 * The category × month spending matrix. Returned by the heatmap endpoint so two
 * charts can render from one round trip: the intensity heatmap (item #8) and
 * the category-mix small multiples (item #12).
 *
 * `months` is the full axis across the range; `categories[i].cells` carries
 * only the months that had spend, so a missing key reads as zero (the same
 * contract GetMerchantMonthlySpend uses).
 */
export interface SpendingHeatmap {
  from: string
  to: string
  /** "YYYY-MM" strings, ascending, across [from, to]. */
  months: string[]
  /** Sorted by whole-range `total` descending. */
  categories: SpendingHeatmapCategory[]
  /**
   * The one entry in `months` that has not finished yet, or absent when the
   * window ends in the past. Its cells are part-month figures, so they are not
   * comparable with the columns beside them and must stay out of the colour
   * ramp's ceiling (MAD-110).
   */
  in_progress_month?: string
  /** "YYYY-MM-DD" the in-progress column runs through. */
  as_of?: string
  /**
   * Echo of the "Hide one-time charges" toggle. True means `is_one_time` rows
   * were dropped from every cell — the matrix is the trailing year without the
   * charges the reader flagged as not repeating, so a cell no longer reconciles
   * to the cent with the same month's real-period figures. Read it before
   * labelling the heatmap; the react-query key already prevents a stale cache,
   * and this makes the payload agree with its own label.
   */
  exclude_one_time?: boolean
}

export interface SpendingHeatmapCategory {
  category_id: string
  name: string
  slug: string
  color: string | null
  is_fixed: boolean
  /** Whole-range spend as a decimal string; the ranking key. */
  total: string
  /** "YYYY-MM" → spend that month, decimal string. Only non-zero months. */
  cells: Record<string, string>
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

/** One row of the merchant explorer. All money fields are decimal strings. */
export interface MerchantExplorerRow {
  merchant: string
  /** The resolved key — what addresses the merchant detail view. */
  merchant_key: string
  total: string
  transaction_count: number
  average: string
  first_seen: string
  last_seen: string
  /**
   * Spend here over the equivalent preceding window, "0" when there was none.
   * Never render a percentage change against zero — `is_new` covers that case.
   */
  prior_total: string
  /** No charge here before the window at all, not merely none last period. */
  is_new: boolean
  /** The category most of this merchant's spend lands in; null when uncategorised. */
  category_id: string | null
  category_name: string | null
  category_color: string | null
}

/** A merchant that used to charge on a regular cadence and has stopped. */
export interface LapsedMerchant {
  merchant: string
  merchant_key: string
  typical_amount: string
  monthly_estimate: string
  cadence: string
  last_seen: string
  days_quiet: number
}

/**
 * Every merchant in a window, plus the gone-quiet list.
 *
 * Deliberately unpaginated: a household has hundreds of merchants, so the page
 * fetches the window once and searches, sorts and pages it locally. `truncated`
 * says whether the server's row cap bit, which the UI must surface rather than
 * present a partial list as the whole picture.
 */
export interface MerchantExplorer {
  from: string
  to: string
  prior_from: string
  prior_to: string
  /** Everything spent in the window — the concentration denominator. Unaffected
   * by the search needle, so the headline stays true while the user types. */
  window_total: string
  merchant_count: number
  truncated: boolean
  merchants: MerchantExplorerRow[]
  lapsed: LapsedMerchant[]
}

/** One merchant's slice of a category. Shaped like MerchantSpend on purpose. */
export interface CategoryMerchant {
  merchant: string
  merchant_key: string
  total: string
  transaction_count: number
}

export interface CategoryDetailTransaction {
  id: string
  date: string
  amount: string
  /** The raw text the bank printed for this charge. */
  descriptor: string
  /** The canonical merchant name, and the key that opens its detail page. */
  merchant: string
  merchant_key: string
  account_name: string
}

/**
 * One category over one period: the numbers, the shape over time, who the money
 * went to, and the charges behind all three. The counterpart of MerchantDetail.
 */
export interface CategoryDetail {
  category_id: string
  name: string
  slug: string
  color: string | null
  is_fixed: boolean
  /** A built-in category, which cannot be renamed or deleted. */
  is_system: boolean
  from: string
  to: string
  total: string
  transaction_count: number
  average: string
  largest: string
  first_seen: string | null
  last_seen: string | null
  /** Shaped as MerchantMonthPoint so MonthlyBars consumes it directly. */
  monthly: MerchantMonthPoint[]
  merchants: CategoryMerchant[]
  transactions: CategoryDetailTransaction[]
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
  /** The MEDIAN month's income over the trailing six, not the mean. */
  expected_income: string
  /**
   * What fixed categories cost in a MEDIAN month of the trailing six, summed
   * per category. Deliberately not a mean: one loan payoff inside the window
   * would otherwise raise this for six months running.
   */
  fixed_costs: string
  budgeted_discretionary: string
  goal_contributions: string
  safe_to_spend: string
  /** Months the income figure is based on; low values mean a thin history. */
  income_months: number

  /**
   * The bill-aware view, added ALONGSIDE safe_to_spend — the original field
   * keeps its meaning. `fixed_costs` estimates a typical month from history;
   * these fields replace that, per category, with this month's actual schedule,
   * so no bill is subtracted twice.
   */
  fixed_costs_after_bills: string
  /**
   * What is still to fall due between today and month end. REPORTED, NOT
   * SUBTRACTED — and NOT the amount that went into fixed_costs_after_bills,
   * which uses the whole month and only fixed categories. Presenting this as
   * the driver of the after-bills figure makes it look like safe-to-spend grows
   * as the month wears on.
   */
  upcoming_obligations: string
  safe_to_spend_after_bills: string
  /**
   * How many fixed categories have a known obligation behind them. 0 means the
   * after-bills figure carries no new information and should not be shown as if
   * it did.
   */
  obligation_coverage: number
}

/**
 * One row of an option's "how this was calculated" panel: an input's label and
 * the finished figure that went into the ranking.
 */
export interface AdviceBasis {
  label: string
  value: string
}

/**
 * One computed use of one marginal dollar.
 *
 * Every figure is finished server-side in exact decimal. NOTHING HERE IS
 * RECOMPUTED IN JS — not the value, not the order. `tier` is the waterfall step
 * that placed it and the list arrives already sorted; re-sorting it in the
 * client would silently replace a published rule with a different one.
 */
export interface AdviceOption {
  /** Stable (kind, subject) identity — what a household suppresses. */
  key: string
  kind: string
  subject_id?: string
  /** 1–7; 0 with `unranked` true means it could not be placed. */
  tier: number
  label: string
  detail: string
  amount: string
  /**
   * What `amount` buys. `value_kind` says in what unit — for
   * 'months_earlier' this is a COUNT OF MONTHS, not money, and rendering it
   * with a currency symbol is the mistake that makes the panel untrustworthy.
   */
  value: string
  value_kind:
    | 'interest_avoided'
    | 'match_captured'
    | 'months_earlier'
    | 'gap_closed'
    | 'headroom'
    | 'projected_growth'
  /**
   * An input the option needed is missing — in practice a debt with no APR
   * anywhere. Listed and labelled rather than dropped or defaulted to zero: a
   * debt with an unknown rate is the one most likely to be expensive.
   */
  unranked: boolean
  /**
   * Tier 7. These are shown SIDE BY SIDE rather than ranked against each other
   * — the app has no opinion on a guaranteed 3.5% versus an assumed 7%.
   */
  tradeoff: boolean
  /** A debt that never clears at its current payment; `value` is then zero. */
  unbounded: boolean
  note?: string
  basis: AdviceBasis[]
}

/**
 * One advisor run.
 *
 * `slack` is the SAME figure the Budgets page prints as safe-to-spend, and the
 * copy around it must stay CONDITIONAL — "if you don't spend this" — never "you
 * have this available". Two surfaces giving opposite instructions about one
 * number is how a household stops trusting both.
 */
export interface Advice {
  slack: string
  /** 'after_bills' when known bills were used; 'typical_month' otherwise. */
  slack_basis: 'after_bills' | 'typical_month'
  obligation_coverage: number
  income_months: number
  threshold: string
  /** False when there is nothing worth saying; `options` is then empty. */
  significant: boolean
  /** The APR, as a percentage, above which guaranteed beats assumed. */
  hurdle: string
  hurdle_basis: string
  slack_parts: {
    expected_income: string
    fixed_costs: string
    fixed_costs_after_bills: string
    budgeted_discretionary: string
    goal_contributions: string
    upcoming_obligations: string
  }
  options: AdviceOption[]
  suppressed: string[]
  /**
   * The model's phrasing of the list. EMPTY IS NORMAL — with no API key the
   * options render as a plain list and nothing is missing but the prose.
   */
  narrative: string
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
  /** The stated expected range (MAD-120), both null when none was stated.
   *  `amount` stays the expected figure the projection uses either way — the
   *  range is a tolerance around it, and a charge outside it raises the
   *  bill_out_of_range insight instead of quietly marking the bill paid. */
  amount_min: string | null
  amount_max: string | null
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
  /** When on, the worker materialises due occurrences as real transactions.
   *  Off by default: an obligation is a forecast until someone says otherwise. */
  auto_post: boolean
  /** The account a posting CREDITS, as distinct from `account_id`, which is the
   *  one the bill is paid FROM. Null means "same as account_id". */
  posting_account_id: string | null
  /** Occurrences on or before this date have been posted. */
  last_posted_date: string | null
  /** Per-item reminders opt-out (MAD-85). On by default; off silences the
   *  overdue-bill coaching for this one obligation. */
  remind: boolean
}

export interface AutoPostInput {
  auto_post: boolean
  posting_account_id?: string | null
}

export type ObligationUnit = 'day' | 'week' | 'month' | 'year'

/** Fields to create or update an obligation. Amounts and dates are strings. */
export interface ObligationInput {
  label: string
  amount: string
  /** Optional expected range. Send both or neither; empty strings clear a range
   *  that was previously set. */
  amount_min?: string
  amount_max?: string
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
  /** The stated expected range, both null when none. The list `total` still sums
   *  `amount`: a range is a tolerance, not a second figure to add up. */
  amount_min: string | null
  amount_max: string | null
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
  /**
   * `balance` plus a trailing-median estimate of income and typical spending.
   * A GUESS, not a known figure — absent when the household has no income
   * history to estimate from (see `BalanceProjection.estimate.has_income_history`).
   */
  estimated_balance?: string
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

/** The trailing-median estimate layered on top of the known-obligations line. */
export interface ProjectionEstimate {
  expected_monthly_income: string
  extra_monthly_spend: string
  /** How many of the trailing months had any income — caveat a thin history. */
  income_months: number
  /** False when there's no income history at all; don't draw the estimate line. */
  has_income_history: boolean
}

/**
 * Balances carried forward through KNOWN obligations only. It is not a
 * prediction of discretionary spending and must not be presented as one.
 * `unassigned_total` is the money the per-account lines cannot show because no
 * account was named on those bills. `estimate` (and each point's
 * `estimated_balance`) is a separate, clearly-labeled guess layered on top —
 * never blend it into the known figures above it.
 */
export interface BalanceProjection {
  from: string
  to: string
  combined: AccountProjection
  accounts: AccountProjection[]
  unassigned_total: string
  total_due: string
  estimate: ProjectionEstimate
}

export interface PeriodQuery {
  from?: string
  to?: string
}

/**
 * Opt into inflation-adjusted figures on an endpoint that offers them.
 *
 * Omitting it, or sending `real: false` (which reaches the server as the string
 * "false" and is read as nominal), gets the response the endpoint has always
 * returned. Nominal is the default everywhere and stays that way: silently
 * changing the meaning of a figure would break every comparison a user carries
 * in their head.
 */
export interface RealQuery {
  real?: boolean
}

/**
 * Opt into dropping `is_one_time` rows from a trailing-baseline view.
 *
 * The mirror of `RealQuery` for the "Hide one-time charges" toggle. Omitting
 * it, or sending `exclude_one_time: false`, returns the same real-period
 * figures the endpoint has always returned — the money did leave, and the
 * default view keeps it. The server reads "1" or "true" as on and anything
 * else (including the bare zero value) as off, matching how `real` is parsed.
 *
 * One toggle sends this flag to both `/trend` and `/heatmap`; the response
 * echoes it back so a chart can never render filtered figures under an
 * unfiltered label.
 */
export interface OneTimeQuery {
  exclude_one_time?: boolean
}

/**
 * Opt into netting linked refunds against the charges they refund.
 *
 * The third member of the `RealQuery`/`OneTimeQuery` family and the same
 * contract: omitting it, or sending `net_refunds: false`, returns the figures
 * the endpoint has always returned. Off is the default because "what left the
 * account in each month" is what every figure in this app has meant, and a link
 * is a statement a user made rather than a correction to the bank's record.
 *
 * When on, a refund's amount comes off the ORIGINAL CHARGE, in the charge's own
 * month and category — never as a negative in the refund's month. Only links
 * whose type nets (today, only the built-in Refund) do anything; income is
 * untouched. See the backend's `queries/reports.sql` for the full rules.
 */
export interface NetRefundsQuery {
  net_refunds?: boolean
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
  /**
   * The composition recorded the day this snapshot was taken. Present on every
   * snapshot the app has ever written (the column is NOT NULL DEFAULT '{}'),
   * so the stacked-area composition chart (item #11, MAD-34) can plot assets vs
   * liabilities across the whole trend. Omitted only when a stored row would
   * not decode — a backstop, not a live case.
   */
  breakdown?: NetWorthBreakdown
  /**
   * The same three figures in base-period dollars, present only when the
   * request asked for `real` and this snapshot's month has a published CPI
   * index. Absent means the point cannot be deflated — never that deflating it
   * would have changed nothing. See `Inflation` for the base period every one
   * of these is denominated in.
   */
  real_assets_total?: string
  real_liabilities_total?: string
  real_net_worth?: string
}

/**
 * The CPI-U deflator behind every real ("inflation-adjusted") figure, from
 * GET /api/inflation.
 *
 * Read this before rendering a real figure anywhere. `base_label` is not
 * decoration — a real number without the month it is denominated in is not a
 * number anybody can use, so the label ships beside the value, not in a
 * footnote.
 */
export interface Inflation {
  /** False when the series is empty. Hide the real toggle entirely. */
  available: boolean
  series: string
  source_url: string
  /** "2026-06" and "June 2026". Real figures are in THESE dollars. */
  base_period?: string
  base_label?: string
  /** The span that can be deflated at all, as "YYYY-MM". */
  earliest?: string
  latest?: string
  /** True once the series is more than two months behind; one is normal. */
  stale: boolean
  stale_note?: string
  /**
   * Months inside the covered span with no published index, as "YYYY-MM".
   * Permanent holes, not a sync failure — BLS never collected them.
   */
  gaps: string[]
  gap_note?: string
  /** Inflation from last December to the base period, as a decimal fraction. */
  ytd_rate?: string
  ytd_from?: string
  ytd_label?: string
  context?: InflationContext
  /**
   * The shortest window on which a real view is worth offering. Below this,
   * hide the toggle: deflating one month by one month's price change is noise
   * dressed as precision.
   */
  min_span_months: number
  basis: string
}

/** The household's own year set against the price level. Every field optional. */
export interface InflationContext {
  net_worth_change?: string
  net_worth_real_change?: string
  net_worth_from?: string
  net_worth_to?: string
  net_worth_start_value?: string
  net_worth_end_value?: string
  income_change?: string
  income_note?: string
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

/**
 * One debt the household carries, with whatever terms are known for it.
 *
 * This list is every credit and loan ACCOUNT, not only the ones Plaid served
 * loan terms for. Plaid supports its Liabilities product at a minority of
 * institutions, and keying this on it meant a household with a mortgage and two
 * credit cards saw an empty debt table under a non-zero debt total, and an empty
 * payoff-goal picker. A debt with no terms is still a debt; it just has nulls.
 */
export interface Liability {
  /** The ACCOUNT id — a debt is an account, and most have no separate
   *  institution-reported terms row behind them at all. */
  id: string
  /** The account this debt belongs to; identical to `id`, kept because callers
   *  filtering an account list read better against this name. */
  account_id: string
  kind: string
  /** The raw account type, 'credit' or 'loan'. `kind` is the display label
   *  (Plaid's subtype); this is what decides whether the rate is an APR or a
   *  note rate — see isAmortizingDebt(). */
  account_type: string
  account_name: string
  mask: string | null
  institution_name: string | null
  apr: string | null
  /** What is owed now, matching the Accounts page and the Net Worth
   *  liabilities tile — not the last statement balance. */
  balance: string | null
  minimum_payment: string | null
  /** Where each figure came from. '' means nobody knows it: render the "add it"
   *  affordance rather than a confident zero. 'manual' means the household
   *  typed it, and it will survive every future sync. */
  apr_source: '' | 'manual' | 'plaid'
  payment_source: '' | 'manual' | 'plaid'
  /** The household's own scheduled bill where one is set, otherwise whatever
   *  the institution reported. Derived from the recurrence on every read, so it
   *  is never a stale cached date. */
  next_payment_due_date: string | null
  /** The recurrence behind that date; null when no bill is scheduled. */
  schedule: PaymentSchedule | null
  /** Only ever populated by Plaid — there is no manual entry for this. */
  is_overdue: boolean | null
}

/**
 * A debt payment's recurrence, in the app's only cadence vocabulary: an anchor
 * date plus "every N days/weeks/months/years". It is a `recurring_obligations`
 * row, so a scheduled debt payment is a bill like any other and appears on the
 * schedule and in cash-flow forecasting.
 *
 * End-of-month is an anchor on a 31st: the expansion clamps 01-31 to 02-28 and
 * returns to the 31st in March rather than drifting off it.
 */
export interface PaymentSchedule {
  obligation_id: string
  anchor_date: string
  interval_count: number
  interval_unit: 'day' | 'week' | 'month' | 'year'
  /** How the rest of the UI words this cadence — "monthly", "every 2 weeks". */
  label: string
}

/** The cadences the terms form offers. Anything the obligations model accepts is
 *  valid; these are the ones worth a menu entry for a debt payment. */
export const PAYMENT_CADENCES: {
  label: string
  interval_count: number
  interval_unit: PaymentSchedule['interval_unit']
}[] = [
  { label: 'Weekly', interval_count: 1, interval_unit: 'week' },
  { label: 'Every 2 weeks', interval_count: 2, interval_unit: 'week' },
  { label: 'Monthly', interval_count: 1, interval_unit: 'month' },
  { label: 'Quarterly', interval_count: 3, interval_unit: 'month' },
  { label: 'Yearly', interval_count: 1, interval_unit: 'year' },
]

export interface ManualAsset {
  id: string
  name: string
  kind: string
  value: string
  is_liability: boolean
  as_of: string
  notes: string | null

  /**
   * Equity, present only when a loan is linked. These are DISPLAY figures —
   * net worth already counts the asset as an asset and the loan as a
   * liability, so equity must never be added to a total.
   */
  loan_account_id?: string
  loan_name?: string
  loan_balance?: string
  equity?: string
  paid_fraction?: string
  underwater?: boolean

  /** The recorded value is old enough to have drifted. Bonds are never stale. */
  stale: boolean
  bond_series?: string
}

/** Class-specific asset metadata. Every field is optional by design. */
export interface AssetDetail {
  address: string | null
  beds: string | null
  baths: string | null
  sqft: number | null
  lot_sqft: number | null

  year: number | null
  make: string | null
  model: string | null
  trim: string | null
  mileage: number | null
  annual_mileage: number | null

  bond_series: string | null
  issue_date: string | null
  purchase_price: string | null
  face_value: string | null
  coupon_rate: string | null
  maturity_date: string | null
  tax_exempt: boolean | null

  condition: string | null
}

export interface AssetValuation {
  value: string
  as_of: string
  /** 'manual' the user typed it, 'estimated' the app computed it, 'api' external. */
  source: string
  note: string | null
}

/**
 * A proposed revaluation. It is a PROPOSAL: rendering it must never be mistaken
 * for the app having changed anything, which is why `estimate` rides along in
 * the payload rather than being assumed by the caller.
 */
export interface AssetSuggestion {
  ok: boolean
  reason?: string
  value?: string
  change?: string
  current: string
  basis?: string
  estimate: boolean
}

/** One published rate period that went into a bond valuation. */
export interface AppliedBondRate {
  period_start: string
  announced: string
  fixed_rate: string
  inflation_rate: string | null
  composite_rate: string
  months: number
}

export interface BondValue {
  ok: boolean
  reason?: string
  /** What the bond could be turned into today — the figure that enters net worth. */
  redemption_value: string
  /** What it has earned. Higher than redemption inside the first five years. */
  accrued_value: string
  penalty_applied: boolean
  doubling_applied: boolean
  matured: boolean
  as_of: string
  valued_through: string
  final_maturity?: string
  basis?: string
  months_to_doubling?: number
  rates: AppliedBondRate[]
}

export interface SavingsBondRate {
  series: string
  period_start: string
  fixed_rate: string
  inflation_rate: string | null
  source_url: string
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
  /** Whether positions and transactions can be entered by hand here. Only
   *  manual accounts can — a Plaid account's are the institution's to report,
   *  and a hand-entered row would be overwritten by the next sync. */
  source: 'plaid' | 'manual'
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
  /**
   * The inflation-adjusted view, present only when the request asked for `real`
   * and both ends of the measured span have a published CPI index.
   *
   * RETURNS only — there is deliberately no real `start_value` or `net_flows`.
   * Deflating a period's cash flows correctly needs each one converted on its
   * own date, and converting them from the span's endpoints would be a
   * precise-looking guess. `note` says exactly this; render it.
   */
  real?: RealPerformance
}

export interface RealPerformance {
  /** Price-level change across the same span, as a fraction. */
  inflation: string
  /**
   * That change compounded to an annual rate; null for spans under a year.
   * This — not `inflation` — is what an already-annualised return (`mwr`) was
   * deflated by.
   */
  annual_inflation: string | null
  twr: string | null
  annualised: string | null
  mwr: string | null
  note: string
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
  /**
   * What CPI-U actually did over the trailing decade, annualised, beside the
   * rate the household assumed.
   *
   * Shown, NEVER applied: `inflation_rate` above is still the only inflation
   * input the projection uses. This exists so choosing that rate is an informed
   * act rather than accepting a 3% default nobody has checked.
   */
  measured_inflation?: string
  measured_inflation_years?: number
  measured_inflation_note?: string
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
  /** This account's own assumed real annual return (fraction, 0.06 = 6%). null = use the household rate. */
  assumed_real_return: string | null
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
  assumed_real_return: string | null
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
/**
 * 'college' joins the two original kinds in doc 32. Its target_amount is ONE
 * YEAR'S cost in today's dollars, not the multi-year total — the engine inflates
 * each year separately and draws them down, which is what makes "funded through
 * sophomore year, short in junior year" computable.
 */
export type GoalKind = 'savings' | 'debt_payoff' | 'college'

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
  /** A percentage, already fixed to the right number of places for the account
   *  type — three on a loan, whose note rates are quoted in eighths, two on a
   *  card. Render it as given rather than reformatting. */
  apr: string
  /** 'credit' or 'loan', so the rate can be named correctly. See
   *  isAmortizingDebt(). */
  account_type: string
  monthly_payment: string
  monthly_interest: string
  /**
   * Where the rate and payment came from. '' means nobody knows the figure, and
   * `apr`/`monthly_payment` are then "0.00" as an arithmetic placeholder, NOT as
   * a reading — render the difference. A schedule with `apr_source: ''` is
   * computed interest-free, so its payoff date is the earliest possible one
   * rather than the real one.
   */
  apr_source: '' | 'manual' | 'plaid'
  payment_source: '' | 'manual' | 'plaid'
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
  /**
   * The per-month balance + interest series behind `months` and
   * `total_interest`, present only when the debt amortizes. Drives the
   * amortization curve (item #10, MAD-34): a declining balance line to zero
   * with the interest portion shaded. Omitted when there is no schedule —
   * never_pays_off, achieved, or no terms — so the chart renders its empty
   * state rather than a curve that contradicts the headline.
   */
  schedule?: PayoffSchedulePoint[]
}

/** One month of a debt-payoff amortization schedule. Decimal strings — never
 *  touched as floats in the browser. */
export interface PayoffSchedulePoint {
  /** 1-based payment number; 1 is the first payment after now. */
  month: number
  /** Interest charged this month. Summed across the schedule it equals
   *  `total_interest`. */
  interest: string
  /** Balance still owed after this month's payment. The final point is "0.00";
   *  the series never goes negative. */
  balance: string
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
  /** Years of study a college goal funds. Defaults to 4; ignored on other kinds. */
  college_years: number
  /** Present only on a college goal: the sentence that stops target_amount
   *  being read as the whole cost of a degree. */
  college_basis?: string
  /** Per-item reminders opt-out (MAD-85). On by default; off silences the
   *  payoff-progress coaching for this one goal. */
  remind: boolean
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
  /** Years of study, for a 'college' goal. Defaults to 4 on create; omitting it
   *  on an UPDATE leaves the stored value alone rather than resetting it. */
  college_years?: number
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

/**
 * One row of an object's change history (the "History" panel). old_value and
 * new_value are the raw JSONB the server stored: a string for most fields, null
 * when a field was set on create or cleared. `field === 'created'` marks the
 * object's first appearance — render it as "created by X" rather than a diff.
 */
export type ObjectChangeKind = 'transaction' | 'budget' | 'goal'

export interface ObjectChange {
  field: string
  old_value: unknown
  new_value: unknown
  actor_user_id: string | null
  actor_display_name: string | null
  created_at: string
}

/**
 * A tag: a free-form label that cuts ACROSS categories.
 *
 * A category answers "what kind of spending is this?" and a transaction has
 * exactly one. A tag answers "what was this FOR?" and a transaction can carry
 * several — "Summer Vacation" spans Food, Travel and Lodging. Tags are
 * household-scoped, but the counts and totals below are computed for the
 * CALLING member: a tag on a charge from another member's private account is
 * not counted here, exactly as that charge is not listed in the ledger.
 *
 * Both figures are server-computed decimal strings. They deliberately answer
 * different questions — see the field comments.
 */
export interface Tag {
  id: string
  name: string
  description: string | null
  /** The envelope's target ("this trip is meant to cost 3,000"), or null when
   *  none is set. Null and "0.00" are different facts: one renders no progress
   *  bar, the other renders a full one. */
  expected_amount: string | null
  /** How many visible transactions carry the tag — all of them, including
   *  income and pending rows. A fact about the LABEL. */
  transaction_count: number
  /** What the tag has COST, all-time, under the same money rules as every other
   *  spending figure in the app. This is what reads against expected_amount. */
  total: string
  created_at: string
}

/** Fields to create or update a tag. Amounts are strings, never floats; an
 *  empty `expected_amount` clears the target. */
export interface TagInput {
  name: string
  description?: string | null
  expected_amount?: string | null
}

/** One row of a tag's transaction list. A trimmed ledger row: enough to
 *  recognise the charge, not the full {@link Transaction} shape. */
export interface TagTransaction {
  id: string
  date: string
  name: string
  merchant: string
  amount: string
  currency: string
  category_id: string | null
  account_name: string
}

/** The tag detail payload: header figures and the charges behind them, read
 *  under the same visibility scope in the same request. */
export interface TagDetail {
  tag: Tag
  transactions: TagTransaction[]
}

/**
 * One tag's spend in a reporting window — the by-tag breakdown, beside
 * by-category and by-merchant.
 *
 * The three panels obey the same money rules, so a figure here means what a
 * figure there means. They will NOT sum to the same period total, and that is
 * correct: a transaction can carry several tags (so it appears under each) or
 * none (so it appears under none). A tag breakdown is a set of overlapping
 * answers, not a partition of the month.
 */
export interface TagSpend {
  tag_id: string
  name: string
  expected_amount: string | null
  total: string
  transaction_count: number
}

/**
 * A rule's condition kind. The server holds the authoritative list and validates
 * every write against it.
 *
 * Note that `description_*` reads the raw text on the charge, NOT the merchant.
 * `merchant_is` is a separate condition, because Plaid's two differ constantly
 * ("SQ *BLUE BOTTLE 0421" vs "Blue Bottle Coffee") and a user who wants one is
 * rarely served by the other.
 */
export type RuleTriggerType =
  | 'description_contains'
  | 'description_starts'
  | 'description_ends'
  | 'description_is'
  | 'amount_more'
  | 'amount_less'
  | 'amount_exactly'
  | 'merchant_is'
  | 'category_is'
  | 'has_no_category'
  | 'account_is'
  | 'has_attachments'

/** A rule's action kind. */
export type RuleActionType = 'set_category' | 'add_tag' | 'set_notes' | 'append_notes'

/** One condition. Every condition on a rule must hold — the join is AND. */
export interface RuleTrigger {
  id: string
  type: RuleTriggerType
  /**
   * The operand as stored: a text fragment, a decimal STRING, or a UUID. Never a
   * number — an amount that round-tripped through a JSON float would be a
   * different rule than the one the user wrote.
   */
  value: string
  /** NOT. Flips this one condition. */
  invert: boolean
}

/** One thing the rule does. Actions run in the order they appear. */
export interface RuleAction {
  id: string
  type: RuleActionType
  value: string
  /**
   * When this action is REFUSED (the category was set by hand, the thing it
   * names is gone), abandon the rest of this rule for that transaction. An
   * action that was already satisfied is a success and stops nothing.
   */
  stop_on_fail: boolean
}

/**
 * A user-editable IF-THEN rule over transactions.
 *
 * Household-scoped like a tag or a category. Which TRANSACTIONS it reaches is
 * still per-member: a rule you run never touches, or even counts, a charge on
 * another member's private account.
 */
export interface Rule {
  id: string
  name: string
  description: string | null
  /** An inactive rule is kept, not deleted — switching automation off to see
   *  whether it caused something is how you debug it. */
  active: boolean
  /** Higher runs first, and the order is load-bearing: a later rule sees what an
   *  earlier one did, so "set category to Coffee" can be observed by a rule
   *  triggering on "category is Coffee". */
  priority: number
  triggers: RuleTrigger[]
  actions: RuleAction[]
  created_at: string
}

/** Fields to create or update a rule. An update REPLACES the condition and
 *  action lists rather than merging them. */
export interface RuleInput {
  name: string
  description?: string | null
  active?: boolean
  priority?: number
  triggers: { type: RuleTriggerType; value: string; invert: boolean }[]
  actions: { type: RuleActionType; value: string; stop_on_fail: boolean }[]
}

/**
 * What one action did, or would do.
 *
 * `unchanged` is a SUCCESS — the tag was already there, the note already said
 * that — and it is what makes running a rule twice a no-op. `refused` is the
 * failure `stop_on_fail` reacts to.
 */
export type RuleOutcome = 'applied' | 'unchanged' | 'refused' | 'skipped'

export interface RuleChange {
  action: RuleActionType
  value: string
  outcome: RuleOutcome
  /** Why, for anything other than "applied". An outcome with no explanation is
   *  what makes people distrust automation. */
  reason: string
}

/** One transaction a dry run fired on, trimmed to what identifies the charge. */
export interface RuleTestMatch {
  transaction_id: string
  date: string
  name: string
  merchant: string
  amount: string
  currency: string
  account_name: string
  changes: RuleChange[]
}

/**
 * The dry run. The counts describe every transaction you can see; the list is
 * capped at a screenful. A truncated list beside an exact count is honest — a
 * truncated count would understate what you are about to do.
 */
export interface RuleTestResult {
  scanned: number
  matched: number
  would_change: number
  truncated: boolean
  matches: RuleTestMatch[]
}

/**
 * What running a rule over existing transactions did. `matched` staying high
 * while `changed` falls to zero on a second run is idempotence working, not the
 * rule breaking.
 */
export interface RuleRunResult {
  scanned: number
  matched: number
  changed: number
}

/**
 * A piggy bank: a lightweight savings envelope ("Car Repair Fund") sitting on
 * an asset account. `current_amount` is DERIVED server-side from the
 * append-only event log (deposits − withdrawals, summed in SQL) — never stored,
 * so it can't drift. Money fields are decimal strings, never summed here.
 *
 * A piggy bank is an ANNOTATION on part of the account's balance, not a
 * separate balance: the money is already in the account, so a deposit/withdraw
 * here only earmarks a slice of it. Net worth never moves when a piggy bank
 * does — say so on the page so users don't double-count.
 */
export interface PiggyBank {
  id: string
  account_id: string
  name: string
  /** Nullable: an open-ended jar with no finish line. */
  target_amount: string | null
  current_amount: string
  created_at: string
}

/** Fields to create or update a piggy bank. account_id is fixed at create. */
export interface PiggyBankInput {
  name: string
  /** Required on create; ignored on update (the account can't be moved). */
  account_id?: string
  /** Omit for an open-ended jar. */
  target_amount?: string
}

/** One row of the append-only deposit/withdraw event log. */
export interface PiggyBankEvent {
  id: string
  type: 'deposit' | 'withdraw'
  amount: string
  /** Optional link to a real transaction this event corresponds to. */
  transaction_id: string | null
  created_at: string
}

/**
 * The funding account's unassigned balance: its current balance minus the sum
 * of every piggy bank's derived balance drawing from it. Negative means the
 * household has earmarked more than the account holds — the over-allocation
 * case the UI must surface as a loud warning, never a silently clipped number.
 */
export interface AccountAllocation {
  account_id: string
  account_balance: string
  assigned: string
  available: string
  over_allocated: boolean
}

/** The result of a deposit or withdraw: the event, the piggy bank's new
 *  standing, and the funding account's allocation afterward. */
export interface PiggyBankMovement {
  event: PiggyBankEvent
  piggy_bank: PiggyBank
  allocation: AccountAllocation
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
  /**
   * The MEDIAN charge, not the mean. A merchant's one anomalous charge — a loan
   * payoff billing under the same descriptor as its monthly payment, an annual
   * true-up — must not move what the household reads as "what this bill costs".
   */
  typical_amount: string
  avg_gap_days: string
  /** weekly | every 2 weeks | monthly | every 2 months | quarterly | every 6 months | yearly */
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
  /** The key the suppression is recorded under — what unsuppress takes. */
  merchant_key: string
  /**
   * The same key canonicalised, which is what addresses the merchant detail view.
   * A suppression recorded against a raw descriptor before that descriptor was
   * merged would otherwise link nowhere.
   */
  merchant_key_resolved: string
  merchant: string
  suppressed_at: string
}

/**
 * Which anomaly detector a suppression silences. "this merchant charges odd
 * amounts" and "this merchant bills me twice" are different claims, so
 * silencing one does not silence the other; 'all' covers both.
 */
export type AnomalyScope = 'all' | 'outlier' | 'duplicate'

export interface SuppressedAnomalyMerchant {
  /** The key the suppression is recorded under — what unsuppress takes. */
  merchant_key: string
  /** The same key canonicalised, which is what addresses the merchant detail view. */
  merchant_key_resolved: string
  merchant: string
  scope: AnomalyScope
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
  /**
   * Whether a mail server is configured. Gates the emailed-digest toggle, so
   * Settings never offers a switch that could not deliver anything.
   */
  smtp_enabled: boolean
  /**
   * Whether the OPERATOR opted into the merchant logo fetcher
   * (`MERCHANT_LOGOS_ENABLED`, plus a Logo.dev token and an AI key). Settings
   * uses this to explain why a household switch would be doing nothing.
   */
  merchant_logos_available: boolean
  /**
   * Whether an avatar should try to load a logo at all: the operator switch AND
   * the household's `merchant.logos` preference, resolved server-side so a
   * component rendered fifty times a page asks one question rather than two.
   */
  merchant_logos_enabled: boolean
}

/**
 * A stored digest: what one period's recap SAID, frozen when it was generated.
 *
 * `payload` is rendered as given and is never recomputed. That is the point of
 * the feature — recategorise a transaction inside the period and last week's
 * digest still reads exactly as it did when you read it. Nothing here should
 * re-derive a figure from it: every money value is already a finished display
 * string built server-side, in decimal, by the same formatter the recap uses.
 */
export interface DigestEntry {
  id: string
  cadence: string
  period_key: string
  period_start: string
  period_end: string
  label: string
  payload: DigestPayload
  /** The AI narrative, or null on an install running without an AI key. */
  narrative: string | null
  read_at: string | null
  created_at: string
}

export interface DigestPayload {
  version: number
  cadence: string
  label: string
  period_start: string
  period_end: string
  in_progress: boolean

  income: string
  spending: string
  leftover: string
  prior_spending?: string
  savings_rate?: string
  gross_pay?: string
  gross_savings_rate?: string
  recurring_total?: string
  transaction_count: number

  top_categories: { name: string; slug: string; total: string }[]
  above_baseline: {
    name: string
    this_month: string
    typical: string
    over: string
  }[]
  budgets: {
    name: string
    slug: string
    available: string
    spent: string
    remaining: string
    percent_used: number
    over: boolean
  }[]
  largest_transactions: {
    merchant: string
    amount: string
    date: string
    category: string
  }[]
  net_worth?: {
    current: string
    as_of: string
    start?: string
    change?: string
    direction?: 'up' | 'down' | 'flat'
  }
  insights: {
    id: string
    kind: string
    title: string
    body: string
    priority: number
  }[]
  upcoming_bills: { label: string; amount: string; due_date: string }[]
}

/** One page of digest history, with the counts needed to page and to badge. */
export interface DigestList {
  entries: DigestEntry[]
  total: number
  unread: number
  limit: number
  offset: number
}

/**
 * One proactive-feed insight. `data` is the deterministic facts the narrative
 * was built from — money as decimal strings, never summed here. Higher
 * `priority` sorts first. `read_at`/`dismissed_at` are null until acted on.
 *
 * `retracted_at` is set when the app withdrew the insight because its fact
 * stopped being true — the overdue bill got paid, the surprise charge was
 * brought inside a widened range. Both it and `dismissed_at` keep a row out of
 * the default feed and only appear under `state=all`, but they are not
 * interchangeable: dismissed is the member's decision, retracted is ours.
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
  retracted_at: string | null
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

// --- Operational status ----------------------------------------------------

/**
 * The same five-value health vocabulary the continuity panel uses, reused
 * rather than redefined so the two operator pages colour a state identically.
 */
export type StatusHealth = ContinuityHealth

/** One job being worked right now. */
export interface RunningJob {
  kind: string
  attempt: number
  max_attempts: number
  started_at?: string
  age?: string
}

/**
 * One kind of job that is failing. `retryable` means the system is still
 * trying; `discarded` means it has given up and is waiting on a human.
 */
export interface JobFailure {
  kind: string
  state: 'retryable' | 'discarded'
  count: number
  last_error?: string
  last_at?: string
  age?: string
}

export interface SystemStatus {
  jobs: {
    health: StatusHealth
    headline: string
    /** False means no worker process is alive and nothing is being worked. */
    worker_alive: boolean
    counts: Record<string, number>
    waiting_since?: string
    waiting_age?: string
    running: RunningJob[]
    failures: JobFailure[]
  }
  sync: {
    health: StatusHealth
    headline: string
    configured: boolean
    items: {
      id: string
      institution: string
      health: StatusHealth
      status: string
      error_code?: string
      last_synced_at?: string
      age?: string
      backfill_complete: boolean
    }[]
  }
  backup: {
    health: StatusHealth
    headline: string
    at?: string
    age?: string
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

// --- Payroll ---------------------------------------------------------------
//
// The pre-tax side of the ledger. Two things are worth stating once, because
// getting either wrong would quietly put a wrong number on screen:
//
//   * Every money field is a decimal STRING computed server-side, like the rest
//     of this file. Never sum one in JavaScript.
//   * `confirmed` is not cosmetic. An unconfirmed paystub contributes to no
//     reported figure anywhere — the filter lives in SQL — so the review queue
//     is the only place an unconfirmed stub's numbers mean anything at all.

/** How often an employer pays. Drives the "N pay periods left" figure. */
export type PayFrequency = 'weekly' | 'biweekly' | 'semimonthly' | 'monthly'

export interface Employer {
  id: string
  name: string
  address: string | null
  pay_frequency: PayFrequency
  /** Scoped to what the caller can see, never another member's private stubs. */
  paystub_count: number
  has_ein: boolean
  /** "**-***6789". The full EIN is only ever returned by the tax summary. */
  ein_masked: string | null
}

export interface EmployerInput {
  name: string
  /**
   * Three-valued, and the API depends on it: omit to leave the stored EIN
   * alone, send "" to remove it, send digits to replace it. A sealed column
   * cannot be read back and compared, so there is no fourth option.
   */
  ein?: string
  address?: string
  pay_frequency: PayFrequency
}

/** A deduction category, as the server's taxonomy defines it. */
export interface PayrollCategory {
  category: string
  label: string
  /** tax | retirement | health | insurance | other */
  group: string
  is_tax: boolean
  pre_tax_by_default: boolean
  employer_only: boolean
  /**
   * True when pre-tax status is not the user's to choose — a tax is not a
   * pre-tax deduction of itself, and a Roth deferral is post-tax by definition.
   * The form omits the toggle rather than offering a choice the server rejects.
   */
  pre_tax_locked: boolean
  /** "401k" | "ira" | "hsa" | "" — the shared IRS cap this counts against. */
  limit_group: string
}

export interface PayrollTaxonomy {
  categories: PayrollCategory[]
  pay_frequencies: {
    value: PayFrequency
    label: string
    periods_per_year: number
  }[]
}

export interface PaystubLine {
  id: string
  category: string
  /** The employer's own wording. */
  label: string
  /** The taxonomy's name for the category. */
  category_label: string
  group: string
  amount: string
  ytd_amount: string | null
  pre_tax: boolean
  is_employer: boolean
  is_tax: boolean
}

export interface PaystubDeposit {
  transaction_id: string
  date: string
  /** Positive: money in. Plaid's sign convention is handled server-side. */
  amount: string
}

export interface PaystubBand {
  group: string
  label: string
  amount: string
}

export interface Paystub {
  id: string
  employer_id: string
  employer_name: string
  pay_frequency: PayFrequency
  period_start: string
  period_end: string
  pay_date: string
  gross: string
  net: string
  ytd_gross: string | null
  ytd_net: string | null
  /** manual | pdf | plaid */
  source: string
  confirmed: boolean
  confirmed_at: string | null
  is_shared: boolean
  /** False for a household member's shared stub: visible, not editable. */
  is_own: boolean
  document_id: string | null
  deposit: PaystubDeposit | null
  lines: PaystubLine[]
  tax_total: string
  /** A fraction, 0–1. Null when gross was zero. */
  effective_tax_rate: string | null
  employer_total: string
  total_compensation: string
  /** gross − deductions − net, within a cent. Confirmation requires it. */
  balances: boolean
  residual: string
  breakdown: PaystubBand[]
}

export interface PaystubLineInput {
  category: string
  label: string
  amount: string
  ytd_amount?: string
  pre_tax: boolean
  is_employer: boolean
}

export interface PaystubInput {
  employer_id: string
  period_start: string
  period_end: string
  pay_date: string
  gross: string
  net: string
  ytd_gross?: string
  ytd_net?: string
  source?: 'manual' | 'pdf'
  /** Honoured only when the stub balances; the server refuses otherwise. */
  confirm: boolean
  is_shared: boolean
  document_id?: string
  lines: PaystubLineInput[]
}

/**
 * A paystub read off a PDF's text layer. Every field is a SUGGESTION.
 *
 * Nothing was sent anywhere to produce this: the extraction is a local parse of
 * the text a payroll provider already put in the file. A scanned stub has no
 * text layer and comes back as a 422 telling the user to type it in.
 */
export interface PaystubProposal {
  employer_name_hint: string
  pay_date: string | null
  period_start: string | null
  period_end: string | null
  gross: string | null
  net: string | null
  ytd_gross: string | null
  ytd_net: string | null
  lines: {
    category: string
    category_label: string
    group: string
    label: string
    amount: string
    ytd_amount: string | null
    pre_tax: boolean
    is_employer: boolean
  }[]
  /** Money-bearing lines the parser could not classify, for the user to assign. */
  unmatched: string[]
  warnings: string[]
  balances: boolean
  residual: string
  source: 'pdf'
  document_id: string | null
}

export interface DepositMatch {
  transaction_id: string
  date: string
  amount: string
  label: string
  account_name: string
  /** How far this deposit is from the stub's net pay. Zero is exact. */
  delta: string
  exact: boolean
}

export interface ContributionHeadroom {
  /** The SHARED limit group — a traditional and a Roth 401(k) are one cap. */
  group: string
  label: string
  contributed: string
  limit: string
  remaining: string
  /** Non-zero means an excess deferral, which has to be withdrawn. */
  over_by: string
  periods_left: number | null
  per_period: string | null
}

export interface PayrollSummary {
  tax_year: number
  has_data: boolean
  paystub_count: number
  unconfirmed_count: number
  gross: string
  net: string
  tax_total: string
  effective_tax_rate: string | null
  employer_total: string
  total_compensation: string
  employers: {
    employer_id: string
    name: string
    pay_frequency: PayFrequency
    paystub_count: number
    gross: string
    net: string
    last_pay_date: string | null
  }[]
  categories: {
    category: string
    label: string
    group: string
    amount: string
    is_tax: boolean
    is_employer: boolean
  }[]
  headroom: ContributionHeadroom[]
  /** False when the app has no IRS limits for this year. Say so; never guess. */
  limits_configured: boolean
  latest_limit_year: number
  /** False when no birthdate is known, so no catch-up was applied. */
  age_known: boolean
}

/**
 * Both savings rates, side by side.
 *
 * The net one is the app's existing figure and is unchanged. The gross one is
 * the honest one and is normally a good deal lower, because 30–45% of gross
 * never reaches an account at all.
 */
export interface GrossSavingsRate {
  from: string
  to: string
  net_income: string
  spending: string
  leftover: string
  savings_rate_net: string | null
  gross_pay: string | null
  savings_rate_gross: string | null
  paystub_count: number
  /** Non-empty when the paystubs on file only partly cover the window. */
  coverage: string
}

export interface TaxSummary {
  tax_year: number
  employers: {
    employer_id: string
    employer_name: string
    address: string | null
    ein: string | null
    boxes: { box: string; code: string; label: string; amount: string }[]
  }[]
  /** Rendered wherever this is shown or exported. It is not a W-2. */
  disclaimer: string
}

/**
 * One tool call's result, exactly as the server computed it and exactly as the
 * model received it.
 *
 * This is what an inline chart is drawn from. The chart component is chosen by
 * a DETERMINISTIC map from `tool` to component — the model never picks a chart
 * type, never shapes the data, and never labels an axis. A wrong tool pick
 * renders the wrong chart, which is the same visible, debuggable failure as
 * today's wrong-tool-pick rendering wrong prose.
 */
export interface ChatToolResult {
  tool: string
  result: unknown
}

/** One turn in a chatbot conversation. */
export interface ChatTurn {
  role: 'user' | 'assistant'
  content: string
  /**
   * Tool results behind an assistant turn, in the order they landed. Present on
   * a live turn as its frames arrive, and on a reloaded one from the persisted
   * tool_trace — which is what lets a saved conversation re-render its charts.
   */
  tools?: ChatToolResult[]
  /**
   * True for a turn read back out of a saved thread. FIGURES IN HISTORY ARE
   * CONTEXT, NEVER CURRENT: a six-week-old "safe to spend" is not this month's,
   * and the UI says so rather than reprinting it as though it were.
   */
  stale?: boolean
}

/**
 * The advisor briefing: the household's opening position, composed by
 * deterministic code. No AI anywhere in this payload — it renders identically
 * with no API key configured.
 */
export interface Briefing {
  as_of: string
  net_worth: string
  assets: string
  debts: string
  monthly_slack: string
  slack_basis: 'after_bills' | 'typical_month'
  income_months: number
  /** Null when financial independence is not reached inside the horizon — an
   *  answer, not a gap. */
  fi_age: number | null
  already_fi: boolean
  /** False when there is nothing to project, so the tile is omitted rather than
   *  printing a confident "never". */
  retirement_projected: boolean
  debt_free: {
    /** THE DATE THE LAST DEBT CLEARS, not the first. Null when never, or when
     *  there is nothing to pay off. */
    date: string | null
    never: boolean
    never_account?: string
    projected: number
    excluded: number
    excluded_names: string[]
    total_balance: string
  }
  runway: {
    liquid: string
    monthly_fixed: string
    /** Null when there are no fixed costs to divide by. */
    months: string | null
    target_months: number
  }
  attention: {
    id: string
    kind: string
    priority: number
    title: string
    body: string
  }[]
}

/** A saved advisor conversation. */
export interface AdvisorThread {
  id: string
  user_id: string | null
  title: string
  /** False makes the thread invisible to the rest of the household. */
  is_shared: boolean
  message_count: number
  created_at: string
  updated_at: string
}

export interface AdvisorThreadDetail extends AdvisorThread {
  messages: {
    id: string
    role: 'user' | 'assistant'
    content: string
    created_at: string
    tool_trace?: ChatToolResult[]
    stale: boolean
  }[]
}

export type ActionItemStatus = 'open' | 'done' | 'dismissed'
export type ActionItemSource = 'option' | 'allocation' | 'thread' | 'manual'

/**
 * Something the household decided to do. TRACKED, NEVER EXECUTED — the advisor
 * moves no money, and nothing here is a step towards it.
 */
export interface ActionItem {
  id: string
  title: string
  detail: string | null
  source: ActionItemSource
  status: ActionItemStatus
  due_date: string | null
  created_at: string
  completed_at: string | null
}

export type FilingStatus = 'single' | 'married_joint' | 'married_separate' | 'hoh'

/**
 * The household profile fields doc 31 added. Both nullable, and null is a real
 * answer: "I have not told you my filing status" is not "single".
 */
export interface HouseholdProfile {
  filing_status: FilingStatus | null
  /** A PERCENT, e.g. "20.00" for a 20% drawdown floor. */
  risk_drawdown_floor: string | null
  /**
   * Modified AGI for magi_tax_year, doc 32's Roth-eligibility input. Null is a
   * real answer and the important one: without it the eligibility check reports
   * `unknown`, never `eligible`. Ledgermancy cannot compute a MAGI — it is not
   * an AGI and it is not gross income — so it is typed in or it is absent.
   */
  magi: string | null
  /**
   * The tax year the MAGI is for. A figure from a different year is treated as
   * absent rather than silently reused.
   */
  magi_tax_year: number | null
}

// --- Allocation planner (doc 32) -------------------------------------------

/**
 * Which arithmetic a bucket needs. Only an investment bucket compounds; a debt
 * amortizes and cash accrues, and the UI renders a different formula for each.
 * Showing a credit card a compound-growth formula would be worse than showing
 * nothing.
 */
export type BucketKind = 'investment' | 'debt' | 'cash'

/** One account money can be allocated to. */
export interface AllocationBucket {
  account_id: string
  name: string
  institution?: string
  kind: BucketKind
  treatment?: string
  subtype?: string
  balance: string
  /** What is already being paid in, before this plan adds anything. */
  existing_monthly: string
  /** A PERCENT. Absent means nobody has entered one — unknown, never zero. */
  deposit_apy?: string
  /** Null where unknown. A genuine 0% card and an unknown rate are different. */
  apr?: string
  minimum_payment?: string
}

export interface AllocationBuckets {
  buckets: AllocationBucket[]
  real_return_rate: string
  inflation_rate: string
  excluded_accounts: string[]
  filing_status?: string
  magi_known: boolean
  age_known: boolean
}

/** One line of a proposed split. Percentages 0–100, as decimal strings. */
export interface AllocationSplitInput {
  account_id: string
  lump_pct: string
  monthly_pct: string
  /** A FRACTION ("0.06"); omit to use the household's assumed rate. */
  real_return_rate?: string | null
}

export interface AllocationRunInput {
  lump: string
  monthly: string
  horizon_years: number
  target_nest_egg?: string | null
  family_hsa?: boolean
  splits: AllocationSplitInput[]
}

export interface PayoffSummary {
  monthly_payment: string
  never_pays_off: boolean
  months: number
  total_interest: string
  payoff_date?: string
  monthly_interest: string
}

export interface AllocationBucketResult {
  account_id: string
  name: string
  institution?: string
  kind: BucketKind
  treatment?: string

  /** What the split asked for vs what actually went in after eligibility. */
  allocated_lump: string
  allocated_monthly: string
  applied_lump: string
  applied_monthly: string

  engine: 'compound' | 'amortization' | 'accrual'
  /** The arithmetic spelled out with this bucket's own numbers. */
  formula: string

  start_balance: string
  /**
   * The value at the horizon. NOT a P50 — this is the projected value at the
   * assumed return, and compounding at the mean is a different statistic from
   * the median of a simulation. Doc 33's fan chart renders beside it.
   */
  projected_value: string
  contributed: string
  growth: string
  return_rate: string
  rate_is_household: boolean
  deposit_apy?: string

  cap_group?: string
  annual_limit?: string
  eligibility?: string
  eligibility_note?: string
  /** Money the eligibility check refused, as distinct from money the cap did. */
  eligibility_spill: string

  payoff_base?: PayoffSummary
  payoff_plan?: PayoffSummary
  interest_avoided: string
  months_saved: number

  notes: string[]
}

export interface AllocationCapNote {
  group: string
  planned_annual: string
  allowed_annual: string
  spill_annual: string
}

export interface AllocationGoalMapping {
  goal_id: string
  name: string
  kind: string
  plan_monthly: string
  linked: boolean
  target: string
  current: string
  remaining: string
  required_monthly: string
  shortfall: string
  on_track: boolean
  open_ended: boolean
  achieved: boolean
  months_left: number
}

export interface CollegeYearDetail {
  year: number
  cost: string
  covered: string
  shortfall: string
  balance_after: string
}

export interface CollegeResult {
  goal_id: string
  name: string
  projectable: boolean
  note?: string
  account_id?: string
  account_name?: string
  years_to_enrollment: number
  years: number
  annual_cost_today: string
  inflation_rate: string
  real_excess_rate: string
  balance_at_enrollment: string
  total_cost: string
  total_covered: string
  total_shortfall: string
  funded_pct: string
  first_shortfall_year: number
  monthly_needed?: string
  years_detail: CollegeYearDetail[]
  basis: string
}

export interface AllocationHorizonFlag {
  goal_id: string
  goal_name: string
  bucket_name: string
  months_left: number
  message: string
}

export interface AllocationResult {
  horizon_years: number
  as_of: string
  horizon_date: string
  lump: string
  monthly: string
  unallocated_lump: string
  unallocated_monthly: string
  buckets: AllocationBucketResult[]
  /** Investment + cash. Debt is NOT added in — a retired balance is not a portfolio. */
  projected_assets: string
  baseline_assets: string
  delta: string
  total_interest_avoided: string
  target_nest_egg?: string
  target_met?: boolean
  cap_notes: AllocationCapNote[]
  limits_year: number
  limits_configured: boolean
  cap_basis: string
  goals: AllocationGoalMapping[]
  college: CollegeResult[]
  horizon_flags: AllocationHorizonFlag[]
  excluded_accounts: string[]
  notes: string[]
  estimate: boolean
  basis: string
}

export interface AllocationPlanSummary {
  id: string
  name: string
  input_version: number
  inputs: {
    lump: string
    monthly: string
    horizon_years: number
    target?: string
    family_hsa: boolean
    buckets: {
      account_id: string
      lump_pct: string
      monthly_pct: string
      real_return_rate?: string
    }[]
  }
  assumptions: {
    real_return_rate: string
    inflation_rate: string
    withdrawal_rate: string
    college_inflation_rate: string
    current_age: number
    age_known: boolean
    filing_status?: string
    magi_known: boolean
    tax_year: number
  }
  created_at: string
  updated_at: string
  /** Present on a single-plan read: the plan RECOMPUTED against live data. */
  result?: AllocationResult
  result_error?: string
}

// --- Likelihood layer (doc 33) ---------------------------------------------
//
// TWO FIGURES, TWO DIFFERENT STATISTICS, AND THEY DO NOT MATCH.
// `projected_at_assumed_return` compounds at the assumed return;
// `simulated.p50` is the median of the simulation and is normally LOWER,
// because volatility drags compounding. Never label either of them "P50", and
// always render `gap_note` where both appear.

export interface AccountOutcome {
  account_id: string
  name: string
  p10: string
  p50: string
  p90: string
}

export interface SimulatedFigures {
  p10: string
  /** The MEDIAN SIMULATED OUTCOME. Never rendered as "P50". */
  p50: string
  p90: string
  /**
   * A fraction over the MODELLED SEQUENCES. Render as "meets your target in
   * 94% of 1,000 simulated futures" — never as a chance or a probability.
   */
  success_rate: string | null
  target?: string
  sigma: string
  /** FIFTH-PERCENTILE peak-to-trough drawdown, as a fraction. Not a maximum. */
  drawdown_p5: string
  by_account: AccountOutcome[]
  seed: number
}

export interface LikelihoodResult {
  plan_id?: string
  name?: string
  horizon_years: number
  runs: number
  volatility: string
  projected_at_assumed_return: string
  /** Absent when the simulation gate is off; the deterministic figure is then the whole answer. */
  simulated?: SimulatedFigures
  monte_carlo_enabled: boolean
  gap_note: string
  basis: string
  estimate: boolean
}

export interface RankedPlan {
  plan_id: string
  name: string
  success_rate: string | null
  p50: string
  sigma: string
  drawdown_p5: string
  meets_all_goals: boolean
  missed_goals: string[]
  /** A filter dropped this plan; excluded_by names the clause, reason says it in words. */
  excluded: boolean
  excluded_by?: string
  reason?: string
  top_pick: boolean
}

export interface PlanRanking {
  runs: number
  plans: RankedPlan[]
  /** Null means NO PICK, which is a real answer — never promote the least-bad plan. */
  top_pick: string | null
  no_pick_reason?: string
  floor_applied: boolean
  floor_pct?: string
  no_plan_meets_every_goal: boolean
  rule: string
  explanation: string
}

export interface PlanComparison {
  ranking: PlanRanking
  plans: LikelihoodResult[]
  monte_carlo_enabled: boolean
  basis: string
  estimate: boolean
}

export interface BucketDrift {
  account_id: string
  name: string
  expected_to_date: string
  actual_to_date: string
  /** NEGATIVE means behind. Signed, so "ahead" is expressible. */
  drift: string
  monthly_drift: string
  /** False means no contribution trail — UNTRACKED, never zero. */
  tracked: boolean
  note?: string
}

export interface PlanDrift {
  as_of: string
  since: string
  months: number
  expected_lump: string
  expected_to_date: string
  actual_to_date: string
  drift: string
  monthly_drift: string
  on_track: boolean
  projected_shortfall: string
  remaining_months: number
  buckets: BucketDrift[]
  untracked: string[]
  summary: string
  basis: string
  estimate: boolean
}

export interface PlanTrackingSnapshot {
  as_of: string
  expected_lump: string
  expected_total: string
}

export interface PlanTracking {
  plan_id: string
  name: string
  drift: PlanDrift
  history: PlanTrackingSnapshot[]
}

export interface IdleCashItem {
  account_id: string
  name: string
  institution?: string
  subtype?: string
  balance: string
  apy: string
  operating_float: string
  idle_balance: string
  annual_drag: string
  detail: string
}

export interface IdleCashReport {
  /** False when no deposit yield is on file anywhere: the detector stays silent. */
  has_benchmark: boolean
  benchmark: string
  benchmark_account?: string
  items: IdleCashItem[]
  total_annual_drag: string
  total_idle: string
  unknown_yield_accounts: string[]
  note: string
  basis: string
}

export interface AssetLocationRule {
  asset_class: string
  preferred_account: string
  reason: string
  assumption: string
}

export interface AssetLocationDisclosure {
  rules: AssetLocationRule[]
  /** Always false. In the payload so this cannot be rendered as advice by accident. */
  is_recommendation: boolean
  bracket_known: boolean
  note: string
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

  // A SUCCESSFUL response with no body is not an error.
  //
  // This used to check only for 204, which made every body-less success on any
  // other status look like a failure: res.json() on an empty body throws, the
  // mutation rejects, and the UI reports that the request did not take. The
  // "Scan for groupings" button sat behind exactly that — the endpoint answers
  // 202 Accepted with no body because the work is queued, so the scan ran every
  // time and the page always said it could not start.
  //
  // Testing the body rather than enumerating statuses means the next endpoint
  // that returns 201 or 202 with nothing in it cannot reintroduce this.
  const text = await res.text()
  if (text === '') return undefined as T

  return JSON.parse(text) as T
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

  // Personal API tokens. These routes need the session cookie: a token cannot
  // manage tokens, so a leaked one cannot mint replacements for itself or
  // revoke the ones you would use to lock it out.
  apiTokens: () => request<ApiToken[]>('GET', '/api/auth/tokens'),

  createApiToken: (input: { name: string; scopes: string[]; expires_at?: string }) =>
    request<CreatedApiToken>('POST', '/api/auth/tokens', input),

  revokeApiToken: (id: string) => request<void>('DELETE', `/api/auth/tokens/${id}`),

  // Outgoing webhooks. Every one of these answers 503 when the instance has not
  // set WEBHOOKS_ENABLED, which is what the settings section keys off — the
  // feature is not merely hidden, it is genuinely absent.
  webhooks: () => request<Webhook[]>('GET', '/api/webhooks/'),

  /** The trigger vocabulary, read from the server so the UI cannot offer one it does not have. */
  webhookTriggers: () => request<string[]>('GET', '/api/webhooks/triggers'),

  createWebhook: (input: { name: string; url: string; triggers: string[]; active?: boolean }) =>
    request<CreatedWebhook>('POST', '/api/webhooks/', input),

  updateWebhook: (
    id: string,
    input: { name: string; url: string; triggers: string[]; active: boolean },
  ) => request<Webhook>('PUT', `/api/webhooks/${id}`, input),

  deleteWebhook: (id: string) => request<void>('DELETE', `/api/webhooks/${id}`),

  /** Mints a new signing secret and returns it once. Every receiver holding the old one breaks. */
  rotateWebhookSecret: (id: string) =>
    request<{ secret: string }>('POST', `/api/webhooks/${id}/secret`),

  /** Queues a test delivery. It does not wait for the receiver — poll the messages list. */
  testWebhook: (id: string) =>
    request<{ message_id: string }>('POST', `/api/webhooks/${id}/test`),

  webhookMessages: (id: string) =>
    request<WebhookMessage[]>('GET', `/api/webhooks/${id}/messages`),

  webhookAttempts: (webhookId: string, messageId: string) =>
    request<WebhookAttempt[]>(
      'GET',
      `/api/webhooks/${webhookId}/messages/${messageId}/attempts`,
    ),

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

  /**
   * The operator vocabulary the `q` grammar accepts, for the search bar's
   * autocomplete. Generated from the parser, so the suggestions cannot drift
   * from what the server will actually accept.
   */
  searchOperators: () =>
    request<SearchOperator[]>('GET', '/api/transactions/search-operators'),


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

  /**
   * Sets how a transaction COUNTS, as distinct from what it says. Omitted fields
   * are left alone, so either flag can be toggled without sending the other.
   * Accepts Plaid-synced rows — a synced loan payoff is the case it exists for.
   */
  setTransactionFlags: (
    transactionID: string,
    flags: { is_one_time?: boolean; excluded_from_reports?: boolean },
  ) =>
    request<{
      id: string
      excluded_from_reports: boolean
      is_one_time: boolean
    }>('PATCH', `/api/transactions/${transactionID}/flags`, flags),

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

  // --- Tags ---------------------------------------------------------------
  // The second axis over a transaction. Nothing here touches a row's category:
  // tagging a charge never changes what kind of spending it is.
  tags: () => request<Tag[]>('GET', '/api/tags'),

  createTag: (input: TagInput) => request<Tag>('POST', '/api/tags', input),

  updateTag: (id: string, input: TagInput) =>
    request<Tag>('PUT', `/api/tags/${id}`, input),

  /** Deleting a tag unlabels its transactions; the transactions themselves are
   *  untouched. */
  deleteTag: (id: string) => request<void>('DELETE', `/api/tags/${id}`),

  tagDetail: (id: string) =>
    request<TagDetail>('GET', `/api/tags/${id}/transactions`),

  /**
   * Replaces a transaction's whole tag set — the editor is a list of checkboxes
   * the user confirms, not a stream of deltas.
   *
   * `applyToMerchant` additionally ADDS these tags to every visible charge from
   * the same merchant. It never removes, which is where this differs from
   * "apply category to all from this merchant": a category is single-valued so
   * applying one necessarily overwrites, while replacing a tag set across a
   * merchant would silently strip labels somebody put there for unrelated
   * reasons.
   */
  setTransactionTags: (
    transactionID: string,
    tagIDs: string[],
    applyToMerchant = false,
  ) =>
    request<{ tags: TransactionTag[] }>(
      'PUT',
      `/api/transactions/${transactionID}/tags`,
      { tag_ids: tagIDs, apply_to_merchant: applyToMerchant },
    ),

  // --- Transaction links --------------------------------------------------
  // How one transaction relates to another. Links are annotations: nothing here
  // changes either transaction's amount, date or category. The only figure a
  // link can move is one a reader has explicitly asked to net — see
  // NetRefundsQuery.
  linkTypes: () => request<LinkType[]>('GET', '/api/link-types'),

  createLinkType: (input: LinkTypeInput) =>
    request<LinkType>('POST', '/api/link-types', input),

  updateLinkType: (id: string, input: LinkTypeInput) =>
    request<LinkType>('PUT', `/api/link-types/${id}`, input),

  /** Deleting a type removes its links; both transactions are left untouched.
   *  The three built-in types cannot be deleted (404). */
  deleteLinkType: (id: string) =>
    request<void>('DELETE', `/api/link-types/${id}`),

  /** Every link on this transaction, from BOTH ends, phrased from its point of
   *  view. A link whose far end you cannot see is absent rather than redacted. */
  transactionLinks: (transactionID: string) =>
    request<TransactionLink[]>('GET', `/api/transactions/${transactionID}/links`),

  /**
   * Connect this transaction to another, and get the resulting link list back.
   *
   * `direction` is stated, not inferred: "outward" (the default) means the
   * transaction in the path is the SOURCE — "this refunds that". Getting it
   * backwards would make the netting view subtract the charge from the credit,
   * so the picker should say which way round it is reading.
   */
  linkTransaction: (
    transactionID: string,
    input: {
      transaction_id: string
      link_type_id: string
      direction?: 'outward' | 'inward'
    },
  ) =>
    request<TransactionLink[]>(
      'POST',
      `/api/transactions/${transactionID}/links`,
      input,
    ),

  /** Removes one link from either of its ends. Both transactions survive. */
  unlinkTransaction: (transactionID: string, linkID: string) =>
    request<void>('DELETE', `/api/transactions/${transactionID}/links/${linkID}`),

  // --- Rules --------------------------------------------------------------
  // User-editable IF-THEN over transactions. Rules fire when a transaction
  // arrives and can be re-run over history. They run AFTER the app's own
  // category resolution, and never overwrite a category you set by hand.
  rules: () => request<Rule[]>('GET', '/api/rules'),

  createRule: (input: RuleInput) => request<Rule>('POST', '/api/rules', input),

  /** Replaces the whole rule, conditions and actions included — the editor is a
   *  set of rows you confirm, not a stream of deltas. */
  updateRule: (id: string, input: RuleInput) =>
    request<Rule>('PUT', `/api/rules/${id}`, input),

  /** Deleting a rule does NOT undo what it already did: the categories it set
   *  and the tags it added are your data now, exactly as if you had set them by
   *  hand. */
  deleteRule: (id: string) => request<void>('DELETE', `/api/rules/${id}`),

  /** Dry run: what this rule would do to what is already stored. Writes
   *  nothing, and shares its planner with `runRule`, so it cannot promise
   *  something the run would not do. */
  testRule: (id: string) => request<RuleTestResult>('POST', `/api/rules/${id}/test`),

  /** The same walk, applied. Idempotent: running it a second time changes
   *  nothing. */
  runRule: (id: string) => request<RuleRunResult>('POST', `/api/rules/${id}/trigger`),

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

  /**
   * One category's detail for a period. Addressed by id as a path segment, which
   * a category can afford where a merchant cannot — a category id is a UUID,
   * whereas a raw merchant descriptor routinely contains a slash.
   */
  categoryDetail: (categoryID: string, params: PeriodQuery = {}) =>
    request<CategoryDetail>(
      'GET',
      withQuery(`/api/categories/${categoryID}/detail`, params),
    ),

  // --- Reports ------------------------------------------------------------
  summary: (params: PeriodQuery = {}) =>
    request<Summary>('GET', withQuery('/api/reports/summary', params)),

  byCategory: (params: PeriodQuery = {}) =>
    request<CategorySpend[]>('GET', withQuery('/api/reports/by-category', params)),

  byTag: (params: PeriodQuery = {}) =>
    request<TagSpend[]>('GET', withQuery('/api/reports/by-tag', params)),

  byDay: (params: PeriodQuery = {}) =>
    request<DaySpend[]>('GET', withQuery('/api/reports/by-day', params)),

  merchants: (params: PeriodQuery & { limit?: number } = {}) =>
    request<MerchantSpend[]>('GET', withQuery('/api/reports/merchants', params)),

  /**
   * Every merchant in a window, for the explorer. One request covers search,
   * sorting, paging and every insight card, all of which happen client-side —
   * so only the window and the category filter belong in the query key.
   */
  merchantExplorer: (
    params: PeriodQuery & { category_id?: string; search?: string } = {},
  ) =>
    request<MerchantExplorer>(
      'GET',
      withQuery('/api/reports/merchant-explorer', params),
    ),

  trend: (params: PeriodQuery & RealQuery & OneTimeQuery & NetRefundsQuery = {}) =>
    request<TrendResponse>('GET', withQuery('/api/reports/trend', params)),

  /**
   * The category × month spending matrix behind the heatmap (item #8) and the
   * category-mix small multiples (item #12). One endpoint, two renderings: the
   * heatmap folds to "Other" past its row cap, the small multiples cap
   * themselves at eight. Every money field is a decimal string — the only
   * arithmetic in the client is display-side intensity scaling.
   */
  spendingHeatmap: (params: PeriodQuery & OneTimeQuery = {}) =>
    request<SpendingHeatmap>('GET', withQuery('/api/reports/heatmap', params)),

  /**
   * The cash-flow Sankey payload (item #13, MAD-33): income sources, spending
   * categories and the period totals in one round trip, all from the same
   * queries every other report uses. The chart's bands reconcile with the
   * Spending page tiles to the cent, and honour the same money rules
   * (transfers and credit-card payments excluded, one-time charges included).
   */
  cashFlow: (params: PeriodQuery = {}) =>
    request<CashFlow>('GET', withQuery('/api/reports/cash-flow', params)),

  /**
   * Per-category monthly average and annual total — the planning figures.
   *
   * The response stays a bare array when `net_refunds` is on, unlike /trend
   * which echoes the flag back. Keep the flag in the react-query key: that is
   * what stops a netted average rendering under an un-netted label here.
   */
  averages: (params: PeriodQuery & NetRefundsQuery = {}) =>
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

  // The proactive advisor: the same slack figure safeToSpend returns, plus a
  // ranked list of what it would do if it were not spent. Read-only — the
  // advisor executes nothing.
  advisor: () => request<Advice>('GET', '/api/advisor'),

  // --- Advisor surface -----------------------------------------------------
  // The briefing, saved conversations, and the action items a household
  // accepted out of them. Still read-heavy and still executes nothing: an
  // action item is a note about a decision, not an instruction to move money.
  //
  // The briefing is deterministic end to end and renders with no AI key — the
  // page's headline figures never depend on a model being reachable.
  advisorBriefing: () => request<Briefing>('GET', '/api/advisor/briefing'),

  advisorThreads: () => request<AdvisorThread[]>('GET', '/api/advisor/threads'),

  createAdvisorThread: (title: string, isShared = true) =>
    request<AdvisorThread>('POST', '/api/advisor/threads', {
      title,
      is_shared: isShared,
    }),

  advisorThread: (id: string) =>
    request<AdvisorThreadDetail>('GET', `/api/advisor/threads/${id}`),

  renameAdvisorThread: (id: string, title: string) =>
    request<AdvisorThread>('PATCH', `/api/advisor/threads/${id}`, { title }),

  deleteAdvisorThread: (id: string) =>
    request<void>('DELETE', `/api/advisor/threads/${id}`),

  actionItems: (status?: ActionItemStatus) =>
    request<ActionItem[]>(
      'GET',
      withQuery('/api/advisor/action-items', { status: status ?? '' }),
    ),

  createActionItem: (input: {
    title: string
    detail?: string | null
    source?: ActionItemSource
    due_date?: string | null
  }) => request<ActionItem>('POST', '/api/advisor/action-items', input),

  // Status only. The title was computed by whatever proposed the item; a tray
  // toggle must not become an edit surface for it.
  updateActionItem: (id: string, status: ActionItemStatus) =>
    request<ActionItem>('PATCH', `/api/advisor/action-items/${id}`, { status }),

  householdProfile: () =>
    request<HouseholdProfile>('GET', '/api/household/profile/'),

  updateHouseholdProfile: (input: {
    filing_status: FilingStatus | null
    risk_drawdown_floor: string | null
    magi?: string | null
    magi_tax_year?: number | null
  }) => request<HouseholdProfile>('PUT', '/api/household/profile/', input),

  // --- Allocation planner (doc 32) -----------------------------------------
  // Splitting a lump and a monthly surplus across buckets, with caps,
  // eligibility, per-bucket projections and goal mapping.
  //
  // runAllocation is a POST that WRITES NOTHING. It is a POST because the
  // request carries a body the engine needs, not because it mutates anything —
  // the plan is computed and returned, and the household's accounts, goals and
  // contributions come out of it byte-identical.
  allocationBuckets: () =>
    request<AllocationBuckets>('GET', '/api/allocation/buckets'),

  runAllocation: (input: AllocationRunInput) =>
    request<AllocationResult>('POST', '/api/allocation/plan', input),

  allocationPlans: () =>
    request<AllocationPlanSummary[]>('GET', '/api/allocation/plans'),

  // Opening a saved plan RECOMPUTES it against today's baseline. Results are
  // deliberately never stored: a saved projection is a figure that quietly
  // stops being true and keeps being displayed.
  allocationPlan: (id: string) =>
    request<AllocationPlanSummary>('GET', `/api/allocation/plans/${id}`),

  saveAllocationPlan: (name: string, input: AllocationRunInput) =>
    request<AllocationPlanSummary>('POST', '/api/allocation/plans', {
      name,
      ...input,
    }),

  deleteAllocationPlan: (id: string) =>
    request<void>('DELETE', `/api/allocation/plans/${id}`),

  // --- Likelihood layer (doc 33) -------------------------------------------
  // The distribution behind a plan, the guardrail's pick, and plan tracking.
  //
  // Like runAllocation these POSTs WRITE NOTHING — recordPlanTracking is the
  // one exception, and what it writes is the EXPECTED side of a snapshot.
  // Actuals are read live every time drift is computed, so correcting an old
  // contribution corrects the history rather than leaving a wrong figure frozen.
  planLikelihood: (planId: string, volatility?: string) =>
    request<LikelihoodResult>(
      'POST',
      `/api/likelihood/plan/${planId}` +
        (volatility ? `?volatility=${encodeURIComponent(volatility)}` : ''),
    ),

  // Every plan is simulated at ONE pinned run count, decided server-side. Both
  // figures the rule sorts on move with the run count, so a comparison
  // assembled from differing counts is refused rather than rendered.
  comparePlans: (planIds: string[], volatility?: string) =>
    request<PlanComparison>('POST', '/api/likelihood/compare', {
      plan_ids: planIds,
      volatility: volatility ?? '',
    }),

  planTracking: (planId: string) =>
    request<PlanTracking>('GET', `/api/likelihood/plans/${planId}/track`),

  recordPlanTracking: (planId: string) =>
    request<PlanTrackingSnapshot>('POST', `/api/likelihood/plans/${planId}/track`),

  idleCash: () => request<IdleCashReport>('GET', '/api/accounts/idle-cash'),

  assetLocation: () =>
    request<AssetLocationDisclosure>('GET', '/api/allocation/asset-location'),

  // A PERCENT ("4.50" = 4.5%). Null CLEARS it, which is a real operation: an
  // empty field means "nobody has said", and the cash-drag detector stays
  // silent on it rather than reading it as zero.
  setDepositApy: (accountId: string, depositApy: string | null) =>
    request<{ id: string; name: string; deposit_apy: string | null }>(
      'PUT',
      `/api/accounts/${accountId}/deposit-apy`,
      { deposit_apy: depositApy },
    ),

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

  // Auto-posting is its own endpoint rather than a field on updateObligation
  // because it is a different kind of decision: editing an obligation changes a
  // forecast, enabling this starts writing transactions.
  setObligationAutoPost: (id: string, input: AutoPostInput) =>
    request<Obligation>('PUT', `/api/obligations/${id}/auto-post`, input),

  // --- Reminders (MAD-85) ------------------------------------------------
  // Mark one occurrence paid (the matcher could not find a payment, but the
  // member confirms it went through); clear that mark to re-arm the reminder;
  // toggle the per-item reminders opt-out. The Reminders view itself is a
  // filtered read of /api/insights, so it has no list method here.
  satisfyObligation: (id: string, dueDate: string) =>
    request<{ obligation_id: string; due_date: string; source: string; satisfied_at: string }>(
      'POST',
      `/api/obligations/${id}/satisfy`,
      { due_date: dueDate },
    ),

  clearObligationSatisfied: (id: string, dueDate: string) =>
    request<void>('DELETE', `/api/obligations/${id}/satisfy`, { due_date: dueDate }),

  setObligationRemind: (id: string, remind: boolean) =>
    request<Obligation>('PUT', `/api/obligations/${id}/remind`, { remind }),

  // --- Manual accounts (doc 30) -------------------------------------------
  // Every one of these refuses a Plaid-linked account id. A linked account's
  // name and balance belong to the institution, and an edit here would last
  // only until the next sync silently reverted it.

  createManualAccount: (input: ManualAccountInput) =>
    request<Account>('POST', '/api/accounts', input),

  updateManualAccount: (id: string, input: ManualAccountInput) =>
    request<Account>('PUT', `/api/accounts/${id}`, input),

  deleteManualAccount: (id: string) =>
    request<void>('DELETE', `/api/accounts/${id}`),

  setManualAccountBalance: (id: string, input: SetBalanceInput) =>
    request<Account>('PUT', `/api/accounts/${id}/balance`, input),

  // from / to bound the window (YYYY-MM-DD). Absent returns the whole trail —
  // the manual balance editor's call — and a chart passes a year so a linked
  // account's daily snapshots are not pulled in full every render.
  accountBalanceHistory: (id: string, params: { from?: string; to?: string } = {}) =>
    request<AccountBalanceEntry[]>(
      'GET',
      withQuery(`/api/accounts/${id}/balance-history`, params),
    ),

  listSecurities: (search?: string) =>
    request<Security[]>('GET', withQuery('/api/securities', { q: search })),

  createManualSecurity: (input: SecurityInput) =>
    request<Security>('POST', '/api/securities', input),

  upsertManualHolding: (accountId: string, input: HoldingInput) =>
    request<{ id: string }>('POST', `/api/accounts/${accountId}/holdings`, input),

  deleteManualHolding: (id: string) =>
    request<void>('DELETE', `/api/holdings/${id}`),

  accountInvestmentTransactions: (accountId: string) =>
    request<InvestmentTransaction[]>(
      'GET', `/api/accounts/${accountId}/investment-transactions`),

  createManualInvestmentTransaction: (input: InvestmentTransactionInput) =>
    request<{ id: string }>('POST', '/api/investment-transactions', input),

  deleteManualInvestmentTransaction: (id: string) =>
    request<void>('DELETE', `/api/investment-transactions/${id}`),

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

  // Per-item reminders opt-out (MAD-85). Off silences payoff-progress coaching.
  setGoalRemind: (id: string, remind: boolean) =>
    request<Goal>('PUT', `/api/goals/${id}/remind`, { remind }),

  // Parses a natural-language goal into a confirmable proposal. Never writes —
  // confirmation calls createGoal. 503 when AI is off, 422 on an unreadable parse.
  parseGoal: (text: string) =>
    request<GoalProposal>('POST', '/api/goals/parse', { text }),

  // --- Change history -----------------------------------------------------
  // The field-level "who changed what, when" log for any audited object. One
  // read endpoint serves every kind; visibility is re-resolved server-side per
  // kind, so the caller only names the object it is already viewing.
  objectHistory: (kind: ObjectChangeKind, objectId: string) =>
    request<ObjectChange[]>(
      'GET',
      `/api/audit?object_kind=${kind}&object_id=${objectId}`,
    ),

  // --- Piggy banks --------------------------------------------------------
  // Lightweight savings jars on an asset account. A deposit/withdraw only
  // annotates part of the balance — it never moves real money — so the whole
  // group is read-mostly and every total stays server-computed.
  piggyBanks: () => request<PiggyBank[]>('GET', '/api/piggy-banks'),

  createPiggyBank: (input: PiggyBankInput) =>
    request<PiggyBank>('POST', '/api/piggy-banks', input),

  updatePiggyBank: (id: string, input: PiggyBankInput) =>
    request<PiggyBank>('PUT', `/api/piggy-banks/${id}`, input),

  deletePiggyBank: (id: string) =>
    request<void>('DELETE', `/api/piggy-banks/${id}`),

  depositPiggyBank: (id: string, amount: string) =>
    request<PiggyBankMovement>('POST', `/api/piggy-banks/${id}/deposit`, { amount }),

  withdrawPiggyBank: (id: string, amount: string) =>
    request<PiggyBankMovement>('POST', `/api/piggy-banks/${id}/withdraw`, { amount }),

  piggyBankEvents: (id: string) =>
    request<PiggyBankEvent[]>('GET', `/api/piggy-banks/${id}/events`),

  accountPiggyBanks: (accountId: string) =>
    request<PiggyBank[]>('GET', `/api/accounts/${accountId}/piggy-banks`),

  accountAvailableForPiggy: (accountId: string) =>
    request<AccountAllocation>('GET', `/api/accounts/${accountId}/available-for-piggy`),

  // --- Net worth ----------------------------------------------------------
  netWorth: () => request<NetWorth>('GET', '/api/networth'),

  netWorthHistory: (params: PeriodQuery & RealQuery = {}) =>
    request<NetWorthPoint[]>('GET', withQuery('/api/networth/history', params)),

  /**
   * The CPI-U deflator: coverage, freshness, and the household's own year set
   * against the price level. Read before rendering any real figure — it is
   * where the base-period label comes from.
   */
  inflation: () => request<Inflation>('GET', '/api/inflation'),

  snapshotNetWorth: () => request<NetWorth>('POST', '/api/networth/snapshot'),

  projection: (params: ProjectionQuery = {}) =>
    request<Projection>('GET', withQuery('/api/networth/projection', params)),

  holdings: () => request<Holding[]>('GET', '/api/holdings'),

  liabilities: () => request<Liability[]>('GET', '/api/liabilities'),

  /**
   * Records the rate and monthly payment for a debt whose bank reports neither
   * — which is most of them, since Plaid serves loan terms at a minority of
   * institutions. Typed values always beat synced ones and survive every sync.
   *
   * Every field is sent on every save: the form is the whole state, so null on
   * either one genuinely clears it and hands that figure back to whatever the
   * institution reports. That is the only way back, precisely because manual
   * wins otherwise.
   */
  setAccountTerms: (
    accountID: string,
    input: {
      apr: string | null
      minimum_payment: string | null
      /** The recurring bill that pays this debt. Null clears it, which
       *  deactivates the obligation rather than deleting it — a schedule
       *  someone turned off is not the same as one that never existed. */
      schedule: {
        anchor_date: string
        interval_count: number
        interval_unit: PaymentSchedule['interval_unit']
      } | null
    },
  ) => request<Liability>('PUT', `/api/accounts/${accountID}/terms`, input),

  manualAssets: () => request<ManualAsset[]>('GET', '/api/manual-assets'),

  createManualAsset: (input: {
    name: string
    kind: string
    value: string
    is_liability: boolean
  }) => request<ManualAsset>('POST', '/api/manual-assets', input),

  deleteManualAsset: (id: string) =>
    request<void>('DELETE', `/api/manual-assets/${id}`),

  assetDetail: (id: string) =>
    request<AssetDetail>('GET', `/api/manual-assets/${id}/detail`),

  saveAssetDetail: (id: string, input: Partial<AssetDetail>) =>
    request<AssetDetail>('PUT', `/api/manual-assets/${id}/detail`, input),

  assetValuations: (id: string) =>
    request<AssetValuation[]>('GET', `/api/manual-assets/${id}/valuations`),

  /** The ONLY call that changes an asset's value. Estimates never write. */
  recordValuation: (
    id: string,
    input: { value: string; as_of?: string; source?: string; note?: string },
  ) => request<ManualAsset>('POST', `/api/manual-assets/${id}/valuations`, input),

  assetSuggestion: (id: string) =>
    request<AssetSuggestion>('GET', `/api/manual-assets/${id}/suggestion`),

  bondValue: (id: string) =>
    request<BondValue>('GET', `/api/manual-assets/${id}/bond`),

  linkAssetLoan: (id: string, loanAccountID: string | null) =>
    request<ManualAsset>('PUT', `/api/manual-assets/${id}/loan`, {
      loan_account_id: loanAccountID,
    }),

  savingsBondRates: () =>
    request<SavingsBondRate[]>('GET', '/api/savings-bond-rates'),

  // --- Investments --------------------------------------------------------
  investments: () => request<InvestmentOverview>('GET', '/api/investments/'),

  investmentPerformance: (period: InvestmentPeriod, real = false) =>
    request<InvestmentPerformance>(
      'GET',
      withQuery('/api/investments/performance', { period, real }),
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
  // — resolves once queued; the entry (and any push) appears shortly after.
  // Works with no notification channel configured: the in-app entry is always
  // somewhere for it to go.
  sendDigestNow: () => request<{ status: string }>('POST', '/api/digest/test'),

  // --- Digest history -----------------------------------------------------
  // Scoped to the signed-in user server-side, not to the household: two members
  // legitimately see different figures for the same period.
  digests: (params: { limit?: number; offset?: number } = {}) =>
    request<DigestList>('GET', withQuery('/api/digests/', params)),

  digest: (id: string) => request<DigestEntry>('GET', `/api/digests/${id}`),

  markDigestRead: (id: string) =>
    request<void>('POST', `/api/digests/${id}/read`),

  // --- Continuity (owner-only operator surface) ---------------------------
  continuity: () => request<Continuity>('GET', '/api/admin/continuity'),

  // Records that the operator has stored ENCRYPTION_KEY somewhere safe. The
  // app cannot verify this; asking is the point.
  acknowledgeKeyBackup: () =>
    request<void>('POST', '/api/admin/continuity/key-ack'),

  // Queues a backup cycle or a restore test now. Resolves once queued.
  runContinuityJob: (kind: 'backup' | 'restore_test') =>
    request<{ status: string }>('POST', '/api/admin/continuity/run', { kind }),

  // What the instance is doing right now. Polled while the System tab is open,
  // so it is a single cheap read rather than anything that writes.
  systemStatus: () => request<SystemStatus>('GET', '/api/admin/status'),

  // --- Insights -----------------------------------------------------------
  capabilities: () => request<Capabilities>('GET', '/api/capabilities'),

  /**
   * The `<img src>` for a merchant's cached logo (MAD-38).
   *
   * A URL rather than a fetch: this is the one endpoint the browser is meant to
   * load as an image, and it is same-origin, so the session cookie rides along
   * without any of the plumbing `request` exists for. 404 is the normal answer
   * for a merchant with no logo — MerchantAvatar's `onError` handles it.
   *
   * It never points at a logo host. The bytes were fetched server-side; that is
   * the whole privacy argument for the feature.
   */
  merchantLogoUrl: (merchantKey: string) =>
    withQuery('/api/merchants/logo', { key: merchantKey }),

  // The proactive feed. state 'all' includes dismissed insights; the default
  // 'unread' hides them.
  insights: (params: { state?: 'unread' | 'all' } = {}) =>
    request<Insight[]>('GET', withQuery('/api/insights/', params)),

  markInsightRead: (id: string) =>
    request<void>('POST', `/api/insights/${id}/read`),

  dismissInsight: (id: string) =>
    request<void>('POST', `/api/insights/${id}/dismiss`),

  /**
   * "This is normal": suppress the merchant for whichever detector raised this
   * insight, and dismiss it, in one request. The merchant key is read off the
   * stored insight server-side rather than sent from here.
   */
  markInsightNormal: (id: string) =>
    request<void>('POST', `/api/insights/${id}/normal`),

  /** Stop the anomaly detectors flagging a merchant. */
  suppressAnomaly: (merchantKey: string, merchant: string, scope: AnomalyScope = 'all') =>
    request<void>('POST', '/api/insights/anomaly/suppress', {
      merchant_key: merchantKey,
      merchant,
      scope,
    }),

  /** Restore a previously-suppressed merchant to the anomaly detectors. */
  unsuppressAnomaly: (merchantKey: string, scope: AnomalyScope = 'all') =>
    request<void>(
      'DELETE',
      withQuery('/api/insights/anomaly/suppress', { merchant_key: merchantKey, scope }),
    ),

  /** The household's anomaly suppressions, for the restore list. */
  suppressedAnomalies: () =>
    request<SuppressedAnomalyMerchant[]>('GET', '/api/insights/anomaly/suppressed'),

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

  // --- Payroll --------------------------------------------------------------

  payrollTaxonomy: () =>
    request<PayrollTaxonomy>('GET', '/api/payroll/taxonomy'),

  paystubYears: () => request<number[]>('GET', '/api/payroll/years'),

  payrollSummary: (year?: number, familyHSA?: boolean) =>
    request<PayrollSummary>(
      'GET',
      withQuery('/api/payroll/summary', {
        year,
        family_hsa: familyHSA ? 'true' : undefined,
      }),
    ),

  payrollSavingsRate: (params: PeriodQuery) =>
    request<GrossSavingsRate>(
      'GET',
      withQuery('/api/payroll/savings-rate', params),
    ),

  payrollTaxSummary: (year: number) =>
    request<TaxSummary>('GET', withQuery('/api/payroll/tax-summary', { year })),

  employers: () => request<Employer[]>('GET', '/api/payroll/employers'),

  createEmployer: (input: EmployerInput) =>
    request<Employer>('POST', '/api/payroll/employers', input),

  updateEmployer: (id: string, input: EmployerInput) =>
    request<Employer>('PUT', `/api/payroll/employers/${id}`, input),

  deleteEmployer: (id: string) =>
    request<void>('DELETE', `/api/payroll/employers/${id}`),

  paystubs: (year?: number) =>
    request<Paystub[]>('GET', withQuery('/api/payroll/paystubs', { year })),

  paystub: (id: string) =>
    request<Paystub>('GET', `/api/payroll/paystubs/${id}`),

  createPaystub: (input: PaystubInput) =>
    request<Paystub>('POST', '/api/payroll/paystubs', input),

  updatePaystub: (id: string, input: PaystubInput) =>
    request<Paystub>('PUT', `/api/payroll/paystubs/${id}`, input),

  deletePaystub: (id: string) =>
    request<void>('DELETE', `/api/payroll/paystubs/${id}`),

  // Refused with a 422 naming the gap when the stub does not balance. That is
  // the point of the endpoint, not an edge case to swallow.
  confirmPaystub: (id: string, confirmed: boolean) =>
    request<Paystub>('POST', `/api/payroll/paystubs/${id}/confirm`, {
      confirmed,
    }),

  setPaystubSharing: (id: string, isShared: boolean) =>
    request<Paystub>('PATCH', `/api/payroll/paystubs/${id}/sharing`, {
      is_shared: isShared,
    }),

  paystubDepositMatches: (id: string) =>
    request<DepositMatch[]>(
      'GET',
      `/api/payroll/paystubs/${id}/deposit-matches`,
    ),

  // Null unlinks. Nothing else writes this field — the matcher only proposes.
  linkPaystubDeposit: (id: string, transactionID: string | null) =>
    request<Paystub>('PUT', `/api/payroll/paystubs/${id}/deposit`, {
      transaction_id: transactionID,
    }),

  parsePaystubPDF: (file: File) => parsePaystubUpload(file),

  parsePaystubDocument: (documentID: string) =>
    request<PaystubProposal>('POST', '/api/payroll/parse-document', {
      document_id: documentID,
    }),

  // The chat endpoint streams its answer as Server-Sent Events: a
  // {"tool_set":"…"} frame naming the deterministically-chosen tool set, an
  // optional {"tools_added":[…]} frame when the assistant loads a tool that set
  // did not carry, one {"delta":"…"} frame per chunk of prose, a
  // {"tool":…,"result":…} frame per tool result, then a terminal {"done":true}
  // or {"error":"…"}.
  //
  // The tool frames arrive BEFORE the final prose, because tool calls complete
  // earlier in the server's loop — so a chart mounts while the answer composes,
  // which reads as native rather than as an afterthought.
  chat: (
    messages: ChatTurn[],
    onDelta: (text: string) => void,
    opts?: ChatStreamOptions,
  ) => streamChat(messages, onDelta, opts),
}

/** Callbacks and options for one streamed chat turn. */
export interface ChatStreamOptions {
  /** Persists the turn to a saved conversation when set. */
  threadID?: string
  /** Fires per tool result, so a chart can be drawn from the turn's own data. */
  onTool?: (result: ChatToolResult) => void
  /** Fires once with the tool set the server chose, so a wrong pick is visible. */
  onToolSet?: (set: string) => void
  /**
   * Fires when the assistant pulls in a tool the chosen set did not carry.
   *
   * The set is picked from the question's wording and can pick wrong; the
   * assistant recovers by loading what it needs mid-turn. Surfacing that is what
   * makes a bad pick MEASURABLE — a set escalated out of on most turns is a
   * membership bug, where the only previous symptom was an answer that quietly
   * declined to compute something.
   */
  onToolsAdded?: (names: string[]) => void
}

/**
 * Posts a paystub PDF for local extraction.
 *
 * Bypasses `request` for the same reason uploadDocument does: the body is a
 * FormData and setting a Content-Type by hand would omit the multipart
 * boundary. Nothing is stored — the response is a proposal and the bytes are
 * dropped.
 */
async function parsePaystubUpload(file: File): Promise<PaystubProposal> {
  const form = new FormData()
  form.set('file', file)

  assertOnline()

  const res = await fetch('/api/payroll/parse', {
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
  return (await res.json()) as PaystubProposal
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
  opts?: ChatStreamOptions,
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
    // Only role and content travel: the server holds the transcript's tool
    // results itself, and echoing them back would be both large and a second
    // source of truth for figures the server already has.
    body: JSON.stringify({
      messages: messages.map((m) => ({ role: m.role, content: m.content })),
      thread_id: opts?.threadID,
    }),
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
      tool?: string
      result?: unknown
      tool_set?: string
      tools_added?: string[]
    }
    if (evt.error) throw new ApiError(500, evt.error)
    if (evt.delta) onDelta(evt.delta)
    if (evt.tool_set) opts?.onToolSet?.(evt.tool_set)
    if (evt.tools_added?.length) opts?.onToolsAdded?.(evt.tools_added)
    if (evt.tool) opts?.onTool?.({ tool: evt.tool, result: evt.result })
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
