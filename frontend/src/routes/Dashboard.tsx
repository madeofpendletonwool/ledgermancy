import type { ReactNode } from 'react'
import { Link } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { api, type DaySpend, type MerchantSpend, type Transaction } from '../lib/api'
import { useSession } from '../lib/session'
import {
  formatMoney,
  formatDate,
  formatTransactionAmount,
  isLiability,
} from '../lib/money'
import { CategoryBars } from '../components/charts/CategoryBars'
import { DayBars } from '../components/charts/DayBars'
import { InsightFeed } from '../components/InsightFeed'

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

  // Previous calendar month, for the pace reference.
  const lm = new Date(year, now.getMonth() - 1, 1)
  const lastFrom = `${lm.getFullYear()}-${String(lm.getMonth() + 1).padStart(2, '0')}-01`
  const lastTo = new Date(lm.getFullYear(), lm.getMonth() + 1, 0).toISOString().slice(0, 10)
  const daysInLastMonth = new Date(lm.getFullYear(), lm.getMonth() + 1, 0).getDate()

  const summary = useQuery({ queryKey: ['summary', 'current'], queryFn: () => api.summary() })
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
          {household.data?.name ?? 'Loading household…'}
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

      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <StatTile
          label="Accounts linked"
          value={hasData ? String(rows.length) : '0'}
          hint={
            items.data?.length
              ? `${items.data.length} institution${items.data.length === 1 ? '' : 's'}`
              : 'Connect Plaid to begin'
          }
        />
        <StatTile
          label="Cash & investments"
          value={hasData ? formatMoney(cash) : '—'}
          hint="Across depository and investment accounts"
        />
        <StatTile
          label="Debt"
          value={hasData ? formatMoney(debt) : '—'}
          hint="Credit cards and loans"
          tone={hasData && Number(debt) > 0 ? 'warn' : 'default'}
        />
        <StatTile
          label={`${monthName} spend`}
          value={s ? formatMoney(s.spending) : '—'}
          hint={
            s && Number(s.income) > 0
              ? `${formatMoney(s.income)} in this month`
              : 'Money out this month'
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
                </div>
              )}
            </div>

            <div className="mt-5">
              {byDayThis.isPending ? (
                <Loading />
              ) : (
                <DayBars
                  year={year}
                  month={month}
                  days={byDayThis.data ?? []}
                  lastMonthAvgDaily={lastMonthAvgDaily}
                />
              )}
            </div>

            {/* Income / left / savings rate for the month. */}
            <div className="mt-6 grid gap-4 border-t border-white/5 pt-6 sm:grid-cols-3">
              <MiniStat label="Income" value={s ? formatMoney(s.income) : '—'} />
              <MiniStat
                label="Left to invest"
                value={s ? formatMoney(s.leftover) : '—'}
                tone={s && Number(s.leftover) < 0 ? 'bad' : 'good'}
              />
              <MiniStat
                label="Savings rate"
                value={
                  s?.savings_rate != null
                    ? `${(Number(s.savings_rate) * 100).toFixed(1)}%`
                    : '—'
                }
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
              <Loading />
            ) : (byCategory.data?.length ?? 0) === 0 ? (
              <Empty>No categorised spending yet this month.</Empty>
            ) : (
              <CategoryBars data={byCategory.data ?? []} />
            )}
          </section>

          <div className="grid gap-8 lg:grid-cols-2">
            {/* Top merchants this month. */}
            <section className="glass p-6">
              <h2 className="mb-5 text-lg font-medium">Top merchants · {monthName}</h2>
              {merchants.isPending ? (
                <Loading />
              ) : (merchants.data?.length ?? 0) === 0 ? (
                <Empty>No spending recorded yet this month.</Empty>
              ) : (
                <ul className="space-y-3">
                  {(merchants.data ?? []).map((m) => (
                    <MerchantRow key={m.merchant} merchant={m} />
                  ))}
                </ul>
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
                <Loading />
              ) : (recent.data?.length ?? 0) === 0 ? (
                <Empty>Nothing has come in yet.</Empty>
              ) : (
                <ul className="divide-y divide-white/5">
                  {(recent.data ?? []).map((t) => (
                    <RecentRow key={t.id} transaction={t} />
                  ))}
                </ul>
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

function StatTile({
  label,
  value,
  hint,
  tone = 'default',
}: {
  label: string
  value: string
  hint: string
  tone?: 'default' | 'warn'
}) {
  return (
    <div className="glass p-5">
      <p className="text-sm text-mist-300">{label}</p>
      <p
        className={`tabular mt-2 text-3xl font-semibold ${
          tone === 'warn' ? 'text-ember-400' : 'text-rune-300'
        }`}
      >
        {value}
      </p>
      <p className="mt-1 text-xs text-mist-500">{hint}</p>
    </div>
  )
}

function MiniStat({
  label,
  value,
  tone = 'default',
}: {
  label: string
  value: string
  tone?: 'default' | 'good' | 'bad'
}) {
  const color =
    tone === 'bad' ? 'text-ember-400' : tone === 'good' ? 'text-verdant-400' : 'text-mist-100'
  return (
    <div>
      <p className="text-xs text-mist-500">{label}</p>
      <p className={`tabular mt-1 text-xl font-semibold ${color}`}>{value}</p>
    </div>
  )
}

function MerchantRow({ merchant: m }: { merchant: MerchantSpend }) {
  return (
    <li className="flex items-center gap-4">
      <div className="min-w-0 flex-1">
        <p className="truncate font-medium">{m.merchant}</p>
        <p className="text-xs text-mist-500">
          {m.transaction_count} transaction{m.transaction_count === 1 ? '' : 's'}
        </p>
      </div>
      <span className="tabular shrink-0 font-medium text-mist-100">
        {formatMoney(m.total)}
      </span>
    </li>
  )
}

function RecentRow({ transaction: t }: { transaction: Transaction }) {
  const amount = formatTransactionAmount(t.amount, t.currency)
  return (
    <li className="flex items-center gap-4 py-3">
      <div className="w-16 shrink-0 text-xs text-mist-500">{formatDate(t.date)}</div>
      <div className="min-w-0 flex-1">
        <p className="truncate font-medium">{t.merchant_name ?? t.name}</p>
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

function Loading() {
  return <p className="py-8 text-center text-sm text-mist-500">Loading…</p>
}

function Empty({ children }: { children: ReactNode }) {
  return <p className="py-8 text-center text-sm text-mist-500">{children}</p>
}
