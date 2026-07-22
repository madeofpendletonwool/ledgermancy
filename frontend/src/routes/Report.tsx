import { useQuery } from '@tanstack/react-query'
import { api } from '../lib/api'
import { formatMoney } from '../lib/money'
import { useSession } from '../lib/session'

/** Trailing twelve months — the standard window for reviewing a year. */
function trailingYear() {
  const to = new Date()
  const from = new Date(to.getFullYear() - 1, to.getMonth(), to.getDate())
  const iso = (d: Date) => d.toISOString().slice(0, 10)
  return { from: iso(from), to: iso(to) }
}

/**
 * The financial summary report.
 *
 * Built as a print-styled page rather than a server-generated PDF: the browser
 * produces a better document than a PDF library would, with no headless-Chrome
 * dependency in the container, and what is on screen is exactly what prints.
 * "Save as PDF" in the print dialog is the export.
 */
export function Report() {
  const range = trailingYear()
  const { data: user } = useSession()

  const household = useQuery({ queryKey: ['household'], queryFn: api.household })
  const summary = useQuery({
    queryKey: ['report-summary', range.from, range.to],
    queryFn: () => api.summary(range),
  })
  const averages = useQuery({
    queryKey: ['report-averages', range.from, range.to],
    queryFn: () => api.averages(range),
  })
  const trend = useQuery({ queryKey: ['report-trend'], queryFn: () => api.trend() })
  const netWorth = useQuery({ queryKey: ['networth'], queryFn: api.netWorth })
  const liabilities = useQuery({ queryKey: ['liabilities'], queryFn: api.liabilities })
  const projection = useQuery({
    queryKey: ['report-projection'],
    queryFn: () => api.projection({ months: 120 }),
  })

  const s = summary.data
  const nw = netWorth.data
  const generated = new Date().toLocaleDateString('en-US', {
    year: 'numeric', month: 'long', day: 'numeric',
  })

  const fixedShare =
    s && Number(s.spending) > 0
      ? Math.round((Number(s.fixed_spending) / Number(s.spending)) * 100)
      : 0

  return (
    <div className="report mx-auto max-w-4xl">
      {/* Screen-only toolbar. Hidden when printing. */}
      <div className="no-print mb-8 flex flex-wrap items-center gap-3 rounded-2xl border border-white/10 bg-ink-850/60 p-4">
        <div className="mr-auto">
          <p className="font-medium">Financial summary</p>
          <p className="text-sm text-mist-300">
            Trailing 12 months. Print or save as PDF to share or file.
          </p>
        </div>
        <button className="btn-primary" onClick={() => window.print()}>
          Print / Save as PDF
        </button>
        <a className="btn-ghost" href="/api/export/transactions.csv">Transactions CSV</a>
        <a className="btn-ghost" href="/api/export/categories.csv">Categories CSV</a>
        <a className="btn-ghost" href="/api/export/net-worth.csv">Net worth CSV</a>
      </div>

      <article className="report-sheet space-y-8">
        <header className="border-b border-white/10 pb-5">
          <h1 className="text-3xl font-semibold">Financial Summary</h1>
          <p className="mt-1 text-mist-300">
            {household.data?.name ?? ''} · {range.from} to {range.to}
          </p>
          <p className="mt-0.5 text-xs text-mist-500">
            Prepared {generated} for {user?.display_name}
          </p>
        </header>

        <section>
          <h2 className="report-h2">Position today</h2>
          <div className="grid grid-cols-3 gap-4">
            <Figure label="Assets" value={nw ? formatMoney(nw.assets_total) : '—'} />
            <Figure label="Liabilities" value={nw ? formatMoney(nw.liabilities_total) : '—'} />
            <Figure label="Net worth" value={nw ? formatMoney(nw.net_worth) : '—'} strong />
          </div>
          {nw && (
            <table className="report-table mt-4">
              <tbody>
                <tr><td>Cash & deposits</td><td className="num">{formatMoney(nw.breakdown.cash)}</td></tr>
                <tr><td>Investments</td><td className="num">{formatMoney(nw.breakdown.investments)}</td></tr>
                <tr><td>Manual assets</td><td className="num">{formatMoney(nw.breakdown.manual_assets)}</td></tr>
                <tr><td>Credit card debt</td><td className="num">{formatMoney(nw.breakdown.credit_debt)}</td></tr>
                <tr><td>Loans</td><td className="num">{formatMoney(nw.breakdown.loan_debt)}</td></tr>
              </tbody>
            </table>
          )}
        </section>

        <section>
          <h2 className="report-h2">Cash flow, trailing 12 months</h2>
          <div className="grid grid-cols-4 gap-4">
            <Figure label="Income" value={s ? formatMoney(s.income) : '—'} />
            <Figure label="Spending" value={s ? formatMoney(s.spending) : '—'} />
            <Figure label="Left to invest" value={s ? formatMoney(s.leftover) : '—'} strong />
            <Figure
              label="Savings rate"
              value={s?.savings_rate != null ? `${(Number(s.savings_rate) * 100).toFixed(1)}%` : '—'}
            />
          </div>
          {s && Number(s.spending) > 0 && (
            <p className="mt-3 text-sm text-mist-300">
              Of {formatMoney(s.spending)} spent, {formatMoney(s.fixed_spending)} ({fixedShare}%)
              was fixed commitments and {formatMoney(s.discretionary_spending)} ({100 - fixedShare}%)
              discretionary.
            </p>
          )}
        </section>

        <section className="break-inside-avoid">
          <h2 className="report-h2">Spending by category</h2>
          <table className="report-table">
            <thead>
              <tr>
                <th>Category</th><th>Type</th>
                <th className="num">Avg / month</th><th className="num">Total / year</th>
              </tr>
            </thead>
            <tbody>
              {(averages.data ?? []).map((c) => (
                <tr key={c.category_id}>
                  <td>{c.name}</td>
                  <td className="text-mist-500">{c.is_fixed ? 'Fixed' : 'Discretionary'}</td>
                  <td className="num">{formatMoney(c.monthly_average)}</td>
                  <td className="num">{formatMoney(c.total)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </section>

        <section className="break-inside-avoid">
          <h2 className="report-h2">Month by month</h2>
          <table className="report-table">
            <thead>
              <tr>
                <th>Month</th><th className="num">Income</th>
                <th className="num">Spending</th><th className="num">Leftover</th>
              </tr>
            </thead>
            <tbody>
              {(trend.data ?? []).map((m) => (
                <tr key={m.month}>
                  <td>{m.month}</td>
                  <td className="num">{formatMoney(m.income)}</td>
                  <td className="num">{formatMoney(m.spending)}</td>
                  <td className="num">{formatMoney(m.leftover)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </section>

        {(liabilities.data?.length ?? 0) > 0 && (
          <section className="break-inside-avoid">
            <h2 className="report-h2">Debt</h2>
            <table className="report-table">
              <thead>
                <tr>
                  <th>Account</th><th>Type</th>
                  <th className="num">Rate</th><th className="num">Balance</th>
                </tr>
              </thead>
              <tbody>
                {liabilities.data!.map((l) => (
                  <tr key={l.id}>
                    <td>{l.account_name}</td>
                    <td className="text-mist-500">{l.kind}</td>
                    <td className="num">{l.apr ? `${Number(l.apr).toFixed(2)}%` : '—'}</td>
                    <td className="num">{formatMoney(l.balance)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </section>
        )}

        {projection.data && projection.data.points.length > 0 && (
          <section className="break-inside-avoid">
            <h2 className="report-h2">Outlook</h2>
            {/* The caveat travels with the numbers, on the page, not just in
                the UI around it. */}
            <p className="mb-3 text-sm text-mist-300">
              {projection.data.basis} Assumes{' '}
              {formatMoney(projection.data.assumptions.monthly_surplus)}/month saved and a{' '}
              {(Number(projection.data.assumptions.annual_return_rate) * 100).toFixed(1)}%
              annual return on invested assets.
            </p>
            <table className="report-table">
              <thead>
                <tr><th>Horizon</th><th className="num">Projected net worth</th></tr>
              </thead>
              <tbody>
                {[11, 59, 119].map((i) => {
                  const p = projection.data!.points[i]
                  if (!p) return null
                  const years = Math.round((i + 1) / 12)
                  return (
                    <tr key={p.month}>
                      <td>{years} year{years === 1 ? '' : 's'} ({p.month})</td>
                      <td className="num">{formatMoney(p.net_worth)}</td>
                    </tr>
                  )
                })}
              </tbody>
            </table>
          </section>
        )}

        <footer className="border-t border-white/10 pt-4 text-xs text-mist-500">
          Generated by Ledgermancy on {generated}. Figures are drawn from linked
          account data; projections are illustrations, not forecasts. Transfers
          between own accounts — including credit-card payments — are excluded
          from both income and spending.
        </footer>
      </article>
    </div>
  )
}

function Figure({ label, value, strong }: { label: string; value: string; strong?: boolean }) {
  return (
    <div className="report-figure">
      <p className="text-xs text-mist-500">{label}</p>
      <p className={`tabular mt-1 font-semibold ${strong ? 'text-2xl' : 'text-xl'}`}>{value}</p>
    </div>
  )
}
