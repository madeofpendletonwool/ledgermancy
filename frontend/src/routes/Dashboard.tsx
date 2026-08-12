import type { ReactNode } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import {
  api,
  type DaySpend,
  type MerchantSpend,
  type Transaction,
  type UpcomingObligation,
} from '../lib/api'
import { useSession } from '../lib/session'
import {
  formatMoney,
  formatDate,
  formatTransactionAmount,
  isLiability,
} from '../lib/money'
import { merchantDetailPath } from '../lib/merchants'
import { categoryDetailPath } from '../lib/categories'
import { CategoryBars } from '../components/charts/CategoryBars'
import { DayBars } from '../components/charts/DayBars'
import { SpendSparkline } from '../components/charts/SpendSparkline'
import { InflationStrip } from '../components/InflationStrip'
import { InsightFeed } from '../components/InsightFeed'
import { AdvisorPanel } from '../components/AdvisorPanel'
import { MerchantLink } from '../components/MerchantLink'
import { MerchantAvatar } from '../components/MerchantAvatar'
import { AnimatedNumber } from '../components/motion'
import { SkeletonChart, SkeletonRows, Reveal } from '../components/Skeleton'
import { EmptyState } from '../components/EmptyState'
import { enterProps } from '../lib/motion'
import { motion } from 'motion/react'

const pad2 = (n: number) => String(n).padStart(2, '0')

/**
 * The dashboard is the at-a-glance view: this month's spend and pace, where the
 * money is going, and the latest activity. It links into /spending for the full
 * breakdown rather than repeating it. Every headline figure comes from the
 * server already computed in exact decimal; the only JS arithmetic here sums a
 * handful of already-exact daily values for a secondary pace hint.
 */
export function Dashboard() {
  const { data: user } = useSession()
  const household = useQuery({ queryKey: ['household'], queryFn: api.household })
  const accounts = useQuery({ queryKey: ['accounts'], queryFn: api.accounts })
  const items = useQuery({ queryKey: ['items'], queryFn: api.items })

  const now = new Date()
  const year = now.getFullYear()
  const month = now.getMonth() + 1 // 1-based
  const monthName = now.toLocaleDateString('en-US', { month: 'long' })
  const navigate = useNavigate()

  // Deep-links into the Transactions page, which reads these same params.
  const openDay = (dom: number) => {
    const date = `${year}-${pad2(month)}-${pad2(dom)}`
    navigate(`/transactions?from=${date}&to=${date}`)
  }
  // Into the category's own breakdown. A filtered transaction list was the only
  // destination a category click ever had, and it cannot answer the questions the
  // click is asking — how much, how often, trending which way, and to whom.
  const openCategory = (categoryID: string) =>
    navigate(categoryDetailPath(categoryID))

  // Previous calendar month, for the pace reference.
  const lm = new Date(year, now.getMonth() - 1, 1)
  const lastFrom = `${lm.getFullYear()}-${String(lm.getMonth() + 1).padStart(2, '0')}-01`
  const lastTo = new Date(lm.getFullYear(), lm.getMonth() + 1, 0).toISOString().slice(0, 10)
  const daysInLastMonth = new Date(lm.getFullYear(), lm.getMonth() + 1, 0).getDate()

  const summary = useQuery({ queryKey: ['summary', 'current'], queryFn: () => api.summary() })
  const trend = useQuery({ queryKey: ['trend'], queryFn: () => api.trend() })
  const byDayThis = useQuery({ queryKey: ['by-day', 'current'], queryFn: () => api.byDay() })
  const byDayLast = useQuery({
    queryKey: ['by-day', lastFrom, lastTo],
    queryFn: () => api.byDay({ from: lastFrom, to: lastTo }),
  })
  const byCategory = useQuery({
    queryKey: ['by-category', 'current'],
    queryFn: () => api.byCategory(),
  })
  const merchants = useQuery({
    queryKey: ['merchants', 'current'],
    queryFn: () => api.merchants({ limit: 5 }),
  })
  const recent = useQuery({
    queryKey: ['recent-transactions'],
    queryFn: () => api.transactions({ limit: 5 }),
  })

  // Known bills, on two horizons: the next week for the strip near the top, and
  // the rest of this month so the pace verdict is not blind to a mortgage that
  // has not landed yet. Both totals are summed server-side.
  const daysLeftInMonth = new Date(year, month, 0).getDate() - now.getDate()
  const billsThisWeek = useQuery({
    queryKey: ['obligations-upcoming', 7],
    queryFn: () => api.upcomingObligations(7),
  })
  const billsThisMonth = useQuery({
    queryKey: ['obligations-upcoming', daysLeftInMonth],
    queryFn: () => api.upcomingObligations(daysLeftInMonth),
    enabled: daysLeftInMonth > 0,
  })
  const stillDue = Number(billsThisMonth.data?.total ?? 0)

  const rows = accounts.data ?? []
  const hasData = rows.length > 0

  // Cash and debt are summed here only to display them. Every figure that feeds
  // real analysis — monthly spend, savings rate, net worth — is computed
  // server-side in exact decimal, never in JavaScript.
  const cash = sumBalances(rows.filter((a) => !isLiability(a.type)))
  const debt = sumBalances(rows.filter((a) => isLiability(a.type)))

  const importing = items.data?.some((i) => !i.backfill_complete)
  const needsAttention = items.data?.filter((i) => i.status !== 'active') ?? []

  const s = summary.data
  const lastDaily = byDayLast.data ?? []
  const todayDom = now.getDate()
  // Secondary hint only: sum a handful of server-exact daily totals.
  const lastSameDay = sumDaily(lastDaily, todayDom)
  const lastMonthAvgDaily = daysInLastMonth ? sumDaily(lastDaily) / daysInLastMonth : 0
  const thisMTD = Number(s?.spending ?? 0)
  const paceDiff = thisMTD - lastSameDay

  return (
    <div className="space-y-8">
      <div>
        <h1 className="text-2xl font-semibold">
          Good to see you, {user?.display_name}
        </h1>
        <p className="mt-1 text-mist-300">
          {household.data?.name ?? '\u00A0'}
        </p>
      </div>

      {needsAttention.map((item) => (
        <div
          key={item.id}
          className="rounded-xl border border-ember-400/30 bg-ember-400/10 px-4 py-3 text-sm text-ember-400"
        >
          {item.institution_name} needs to be reconnected before it can sync
          again. <Link to="/accounts" className="underline">Go to accounts</Link>
        </div>
      ))}

      {importing && (
        <div className="rounded-xl border border-rune-400/30 bg-rune-400/10 px-4 py-3 text-sm text-rune-300">
          Importing your transaction history — this can take a minute.
        </div>
      )}

      {/* Proactive feed — the app noticing things. Renders nothing when there
          is nothing to flag, so it never leaves an empty box at the top. */}
      <InsightFeed variant="card" limit={3} />

      {/* The proactive advisor — what this month's slack would DO if it were not
          spent. Sits below the feed because the feed reports things that have
          already happened and this proposes something that has not. Renders
          nothing when the slack is not worth a suggestion. */}
      <AdvisorPanel />

      <BillsDueStrip bills={billsThisWeek.data?.items ?? []} total={billsThisWeek.data?.total} />

      {/* What prices did this year, set against what this household's money
          did. Renders nothing when there is no series or no comparison. */}
      <InflationStrip />

      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <StatTile
          label="Accounts linked"
          value={hasData ? rows.length : null}
          format={(n) => String(Math.round(n))}
          fallback="0"
          hint={
            items.data?.length
              ? `${items.data.length} institution${items.data.length === 1 ? '' : 's'}`
              : 'Connect Plaid to begin'
          }
        />
        <StatTile
          label="Cash & investments"
          value={hasData ? cash : null}
          hint="Across depository and investment accounts"
        />
        <StatTile
          label="Debt"
          value={hasData ? debt : null}
          hint="Credit cards and loans"
          tone={hasData && Number(debt) > 0 ? 'warn' : 'default'}
        />
        <StatTile
          label={`${monthName} spend`}
          value={s ? s.spending : null}
          hint={
            s && Number(s.income) > 0
              ? `${formatMoney(s.income)} in this month`
              : 'Money out this month'
          }
          footer={
            trend.data && trend.data.points.length >= 2 ? (
              <SpendSparkline data={trend.data.points} />
            ) : null
          }
        />
      </div>

      {!hasData ? (
        <section className="glass p-6">
          <h2 className="text-lg font-medium">Get started</h2>
          <p className="mt-2 max-w-2xl text-sm text-mist-300">
            Connect your first account to pull in balances and as much
            transaction history as your institution provides. Your spending,
            categories, and net worth populate automatically from there.
          </p>
          <Link to="/accounts" className="btn-primary mt-5 inline-flex">
            Connect an account
          </Link>
        </section>
      ) : (
        <>
          {/* This month: spend by day, with the pace verdict in the header. */}
          <section className="glass p-6">
            <div className="flex flex-wrap items-baseline justify-between gap-2">
              <div>
                <h2 className="text-lg font-medium">This month · spend by day</h2>
                <p className="mt-1 text-sm text-mist-300">{monthName}</p>
              </div>
              {s && (
                <div className="text-right">
                  <p className="tabular text-xl font-semibold text-rune-300">
                    {formatMoney(s.spending)}
                  </p>
                  <p className="text-xs text-mist-500">
                    {lastSameDay > 0 ? (
                      <>
                        {formatMoney(String(Math.abs(paceDiff)))}{' '}
                        {paceDiff > 0 ? 'more than' : 'less than'} last month by
                        day {todayDom}
                        <span className="text-mist-600">
                          {' '}
                          ({formatMoney(String(lastSameDay))})
                        </span>
                      </>
                    ) : (
                      'month to date'
                    )}
                  </p>
                  {/* Spend-to-date is only half the month's story while a
                      mortgage is still to clear. */}
                  {stillDue > 0 && (
                    <p className="mt-0.5 text-xs text-mist-400">
                      plus {formatMoney(billsThisMonth.data?.total)} of known
                      bills still to come
                    </p>
                  )}
                </div>
              )}
            </div>

            <div className="mt-5">
              {byDayThis.isPending ? (
                <SkeletonChart />
              ) : (
                <Reveal>
                  <DayBars
                    year={year}
                    month={month}
                    days={byDayThis.data ?? []}
                    lastMonthAvgDaily={lastMonthAvgDaily}
                    onSelect={openDay}
                  />
                </Reveal>
              )}
            </div>

            {/* Income / left / savings rate for the month. */}
            <div className="mt-6 grid gap-4 border-t border-white/5 pt-6 sm:grid-cols-3">
              <MiniStat label="Income" value={s ? s.income : null} />
              <MiniStat
                label="Left to invest"
                value={s ? s.leftover : null}
                tone={s && Number(s.leftover) < 0 ? 'bad' : 'good'}
              />
              <MiniStat
                label="Savings rate"
                value={s?.savings_rate ?? null}
                format={(n) => `${(n * 100).toFixed(1)}%`}
              />
            </div>
          </section>

          {/* Where it went — a teaser into the full breakdown. */}
          <section className="glass p-6">
            <div className="mb-5 flex items-baseline justify-between">
              <h2 className="text-lg font-medium">Top categories · {monthName}</h2>
              <Link to="/spending" className="text-sm text-rune-300 hover:underline">
                See full breakdown →
              </Link>
            </div>
            {byCategory.isPending ? (
              <SkeletonRows count={4} />
            ) : (byCategory.data?.length ?? 0) === 0 ? (
              <EmptyState title="No categorised spending yet this month.">
                Charges show up here once they land and are filed under a
                category.
              </EmptyState>
            ) : (
              <Reveal>
                <CategoryBars data={byCategory.data ?? []} onSelect={openCategory} />
              </Reveal>
            )}
          </section>

          <div className="grid gap-8 lg:grid-cols-2">
            {/* Top merchants this month. */}
            <section className="glass p-6">
              <h2 className="mb-5 text-lg font-medium">Top merchants · {monthName}</h2>
              {merchants.isPending ? (
                <SkeletonRows count={4} />
              ) : (merchants.data?.length ?? 0) === 0 ? (
                <EmptyState title="No spending recorded yet this month.">
                  Once a charge clears it lands here.
                </EmptyState>
              ) : (
                <Reveal>
                  <ul className="space-y-3">
                    {(merchants.data ?? []).map((m, i) => (
                      <MerchantRow key={m.merchant} merchant={m} index={i} />
                    ))}
                  </ul>
                </Reveal>
              )}
            </section>

            {/* Latest activity. */}
            <section className="glass p-6">
              <div className="mb-5 flex items-baseline justify-between">
                <h2 className="text-lg font-medium">Recent transactions</h2>
                <Link to="/transactions" className="text-sm text-rune-300 hover:underline">
                  View all →
                </Link>
              </div>
              {recent.isPending ? (
                <SkeletonRows count={4} />
              ) : (recent.data?.length ?? 0) === 0 ? (
                <EmptyState title="Nothing has come in yet.">
                  New transactions appear here as they sync.
                </EmptyState>
              ) : (
                <Reveal>
                  <ul className="divide-y divide-white/5">
                    {(recent.data ?? []).map((t) => (
                      <RecentRow key={t.id} transaction={t} />
                    ))}
                  </ul>
                </Reveal>
              )}
            </section>
          </div>
        </>
      )}
    </div>
  )
}

/** Sums decimal strings for display only. See the note in the component. */
function sumBalances(accounts: { current_balance: string | null }[]): string {
  return accounts
    .reduce((total, a) => total + Number(a.current_balance ?? 0), 0)
    .toFixed(2)
}

/**
 * Sums daily spend, optionally only through a given day-of-month. Operates on
 * values the server already summed exactly per day; used for a secondary pace
 * hint, never for a headline figure.
 */
function sumDaily(days: DaySpend[], throughDom?: number): number {
  return days.reduce((total, d) => {
    const dom = Number(d.day.slice(8, 10))
    if (throughDom !== undefined && dom > throughDom) return total
    return total + Number(d.spending)
  }, 0)
}

/**
 * Bills falling due in the next week.
 *
 * Renders nothing when there are none, so it never leaves an empty box on a
 * dashboard that is otherwise about what already happened. A surprise autopay is
 * the most common way a household ends up overdrawn, and this is the cheapest
 * possible place to prevent it.
 */
function BillsDueStrip({
  bills,
  total,
}: {
  bills: UpcomingObligation[]
  total: string | undefined
}) {
  if (bills.length === 0) return null

  return (
    <section className="glass p-5">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <h2 className="text-base font-medium">Due this week</h2>
        <div className="flex items-baseline gap-3">
          <span className="tabular text-lg font-semibold">{formatMoney(total)}</span>
          <Link to="/schedule" className="text-sm text-rune-300 hover:underline">
            See the schedule →
          </Link>
        </div>
      </div>
      <ul className="mt-3 flex flex-wrap gap-2">
        {bills.slice(0, 6).map((b) => (
          <li
            key={`${b.obligation_id}-${b.due_date}`}
            className="rounded-lg border border-white/10 bg-white/5 px-3 py-1.5 text-sm"
          >
            <span className="text-mist-200">{b.label}</span>
            <span className="tabular ml-2 text-mist-100">{formatMoney(b.amount)}</span>
            <span className="ml-2 text-xs text-mist-500">
              {b.days_until_due === 0
                ? 'today'
                : b.days_until_due === 1
                  ? 'tomorrow'
                  : formatDate(b.due_date)}
            </span>
          </li>
        ))}
        {bills.length > 6 && (
          <li className="px-2 py-1.5 text-sm text-mist-500">
            +{bills.length - 6} more
          </li>
        )}
      </ul>
    </section>
  )
}

function StatTile({
  label,
  value,
  hint,
  tone = 'default',
  footer,
  format,
  fallback = '—',
}: {
  label: string
  value: string | number | null | undefined
  hint: string
  tone?: 'default' | 'warn'
  footer?: ReactNode
  format?: (n: number) => string
  fallback?: string
}) {
  return (
    <div className="glass p-5">
      <p className="text-sm text-mist-300">{label}</p>
      <p
        className={`tabular mt-2 text-3xl font-semibold ${
          tone === 'warn' ? 'text-ember-400' : 'text-rune-300'
        }`}
      >
        <AnimatedNumber value={value} format={format} fallback={fallback} />
      </p>
      <p className="mt-1 text-xs text-mist-500">{hint}</p>
      {footer}
    </div>
  )
}

function MiniStat({
  label,
  value,
  tone = 'default',
  format,
  fallback = '—',
}: {
  label: string
  value: string | number | null | undefined
  tone?: 'default' | 'good' | 'bad'
  format?: (n: number) => string
  fallback?: string
}) {
  const color =
    tone === 'bad' ? 'text-ember-400' : tone === 'good' ? 'text-verdant-400' : 'text-mist-100'
  return (
    <div>
      <p className="text-xs text-mist-500">{label}</p>
      <p className={`tabular mt-1 text-xl font-semibold ${color}`}>
        <AnimatedNumber value={value} format={format} fallback={fallback} />
      </p>
    </div>
  )
}

function MerchantRow({ merchant: m, index }: { merchant: MerchantSpend; index: number }) {
  // A merchant with no resolved key cannot be addressed, so it stays plain text
  // rather than becoming a link that goes nowhere.
  const body = (
    <>
      <MerchantAvatar name={m.merchant} merchantKey={m.merchant_key} />
      <div className="min-w-0 flex-1">
        <p className="truncate font-medium">{m.merchant}</p>
        <p className="text-xs text-mist-500">
          {m.transaction_count} transaction{m.transaction_count === 1 ? '' : 's'}
        </p>
      </div>
      <span className="tabular shrink-0 font-medium text-mist-100">
        {formatMoney(m.total)}
      </span>
    </>
  )

  return (
    <motion.li {...enterProps(index)}>
      {m.merchant_key ? (
        <Link
          className="-mx-2 flex items-center gap-4 rounded-lg px-2 py-1 hover:bg-white/5"
          to={merchantDetailPath(m.merchant_key)}
        >
          {body}
        </Link>
      ) : (
        <div className="flex items-center gap-4">{body}</div>
      )}
    </motion.li>
  )
}

function RecentRow({ transaction: t }: { transaction: Transaction }) {
  const amount = formatTransactionAmount(t.amount, t.currency)
  return (
    <li className="flex items-center gap-4 py-3">
      <div className="w-16 shrink-0 text-xs text-mist-500">{formatDate(t.date)}</div>
      <MerchantAvatar
        name={t.merchant}
        merchantKey={t.merchant_key_resolved}
        size="sm"
      />
      <div className="min-w-0 flex-1">
        <p className="truncate font-medium">
          <MerchantLink
            name={t.merchant}
            merchantKey={t.merchant_key_resolved}
          />
        </p>
        <p className="truncate text-xs text-mist-500">{t.account_name}</p>
      </div>
      <span
        className={`tabular shrink-0 font-medium ${
          amount.isIncome ? 'text-verdant-400' : 'text-mist-100'
        }`}
      >
        {amount.text}
      </span>
    </li>
  )
}
