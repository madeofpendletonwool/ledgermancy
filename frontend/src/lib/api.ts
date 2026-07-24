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
 */

export interface User {
  id: string
  household_id: string
  email: string
  display_name: string
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
  created_at: string
}

export interface Invite {
  id: string
  email: string
  expires_at: string
  created_at: string
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
  total: string
  transaction_count: number
}

export interface BudgetProgress {
  budget_id: string
  category_id: string
  name: string
  slug: string
  color: string | null
  budgeted: string
  spent: string
  remaining: string
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

export type AlertType =
  | 'big_spend'
  | 'budget_threshold'
  | 'unusual_merchant'
  | 'low_leftover'

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

/**
 * A savings goal plus its DERIVED standing. `current_amount`, `required_monthly`
 * and the on-track/shortfall figures are computed server-side (never stored, so
 * they can't drift). All money fields are decimal strings — never summed here.
 */
export interface Goal {
  id: string
  scope: 'household' | 'user'
  kind: string
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
}

/** Fields to create or update a goal. Amounts/dates are strings, never floats. */
export interface GoalInput {
  name: string
  target_amount: string
  target_date?: string
  scope?: 'household' | 'user'
  account_id?: string | null
  category_id?: string | null
}

/** A parsed goal proposal from POST /api/goals/parse (never auto-saved). */
export interface GoalProposal {
  name: string
  target_amount: string
  target_date: string | null
  kind: string
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

async function request<T>(
  method: string,
  path: string,
  body?: unknown,
): Promise<T> {
  const headers: Record<string, string> = {}
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  if (UNSAFE.has(method)) headers['X-CSRF-Token'] = await ensureCsrfToken()

  const res = await fetch(path, {
    method,
    headers,
    credentials: 'include',
    body: body === undefined ? undefined : JSON.stringify(body),
  })

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

  invites: () => request<Invite[]>('GET', '/api/household/invites'),

  createInvite: (email: string) =>
    request<CreatedInvite>('POST', '/api/household/invites', { email }),

  deleteInvite: (id: string) =>
    request<void>('DELETE', `/api/household/invites/${id}`),

  // --- Plaid -------------------------------------------------------------
  createLinkToken: () =>
    request<{ link_token: string }>('POST', '/api/plaid/link-token'),

  exchangePublicToken: (publicToken: string) =>
    request<PlaidItem>('POST', '/api/plaid/exchange', {
      public_token: publicToken,
    }),

  items: () => request<PlaidItem[]>('GET', '/api/plaid/items'),

  syncItem: (id: string) =>
    request<SyncResult>('POST', `/api/plaid/items/${id}/sync`),

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

  setBudget: (categoryID: string, amount: string) =>
    request<{ id: string }>('POST', '/api/budgets', {
      category_id: categoryID,
      amount,
    }),

  deleteBudget: (id: string) => request<void>('DELETE', `/api/budgets/${id}`),

  // Proposes a round budget target per spending category, anchored on each
  // category's true average. Works with or without AI (rule-based rounding when
  // off); ai_tailored says which. Approval is a loop of setBudget, unchanged.
  suggestBudgets: () =>
    request<BudgetSuggestions>('POST', '/api/budgets/suggest'),

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

  // The chat endpoint streams its answer as Server-Sent Events: one
  // {"delta":"…"} frame per chunk, a terminal {"done":true}, or {"error":"…"}.
  // onDelta is called as text arrives so the UI can render it live.
  chat: (messages: ChatTurn[], onDelta: (text: string) => void) =>
    streamChat(messages, onDelta),
}

// streamChat POSTs the transcript and reads the SSE body, invoking onDelta for
// each token. It resolves when the stream reports done and rejects on an error
// frame or a transport failure, so callers can await completion.
async function streamChat(
  messages: ChatTurn[],
  onDelta: (text: string) => void,
): Promise<void> {
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
