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
  /** Positive = money out, negative = money in (Plaid's convention). */
  amount: string
  currency: string
  pending: boolean
  account_name: string
  institution_name: string | null
  plaid_category_primary: string | null
  plaid_category_detailed: string | null
}

export interface TransactionQuery {
  from?: string
  to?: string
  limit?: number
  offset?: number
  /** Restrict to one account. Empty/omitted means all visible accounts. */
  account_id?: string
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
  enabled: boolean
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

/** A detected recurring charge (subscription/bill). Amounts are decimal strings. */
export interface RecurringMerchant {
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
    request<User>('POST', '/api/auth/login', input),

  logout: () => request<void>('POST', '/api/auth/logout'),

  me: () => request<User>('GET', '/api/auth/me'),

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

  categories: () => request<Category[]>('GET', '/api/categories'),

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
  ) => request<Alert>('POST', '/api/alerts/', { type, config, enabled }),

  // The backend keeps an existing alert's type; only config and enabled change.
  updateAlert: (
    id: string,
    config: Record<string, string | number>,
    enabled: boolean,
  ) => request<Alert>('PUT', `/api/alerts/${id}`, { config, enabled }),

  deleteAlert: (id: string) => request<void>('DELETE', `/api/alerts/${id}`),

  alertEvents: () => request<AlertEvent[]>('GET', '/api/alerts/events'),

  unreadAlertCount: () =>
    request<{ count: number }>('GET', '/api/alerts/events/unread-count'),

  markAlertRead: (id: string) =>
    request<void>('POST', `/api/alerts/events/${id}/read`),

  markAllAlertsRead: () => request<void>('POST', '/api/alerts/events/read-all'),

  // --- Insights -----------------------------------------------------------
  capabilities: () => request<Capabilities>('GET', '/api/capabilities'),

  recurring: () =>
    request<RecurringMerchant[]>('GET', '/api/reports/recurring'),

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
