import { useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import { useSearchParams } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { api } from '../lib/api'
import type { Category, LapsedMerchant, MerchantExplorerRow } from '../lib/api'
import { formatDate, formatMoney } from '../lib/money'
import { WINDOWS, matchedWindow, windowRange } from '../lib/period'
import { SINGLE_SERIES, STATUS } from './charts/tokens'
import { MerchantLink } from './MerchantLink'
import { CategoryLink } from './CategoryLink'
import { merchantDetailPath } from '../lib/merchants'
import { Link } from 'react-router-dom'

/**
 * Where the money goes — every merchant, searchable.
 *
 * This replaced a hard-coded top-ten list. The top ten is still what it opens on,
 * because "where does my money actually go?" is the question people arrive with,
 * but a household has hundreds of merchants and the previous version could not
 * reach any of them: below your tenth-biggest merchant there was simply no way in.
 *
 * Search, sort and paging are all local. One request carries the whole window, so
 * typing filters instantly with no debounce, no refetch and no loading flicker —
 * and the four insight panels below fall out of the same array rather than needing
 * four more endpoints. Only the window and the category filter go to the server,
 * because only they change which charges are in scope.
 *
 * No total is computed here. Every figure is a decimal string from SQL; this file
 * compares and formats them, and the only arithmetic is on percentages.
 */

const PAGE_SIZE = 25

/** How many merchants the concentration headline talks about. */
const CONCENTRATION_TOP_N = 10

type SortKey = 'total' | 'count' | 'average' | 'change' | 'recent'

const SORTS: { key: SortKey; label: string }[] = [
  { key: 'total', label: 'Biggest total' },
  { key: 'count', label: 'Most charges' },
  { key: 'average', label: 'Biggest average' },
  { key: 'change', label: 'Biggest increase' },
  { key: 'recent', label: 'Most recent' },
]

export function MerchantExplorer() {
  // The window, the category filter and the needle live in the URL: a filtered
  // view is then shareable and the back button retraces it, matching how the
  // merchant and transaction views already work.
  const [params, setParams] = useSearchParams()
  const months = Number(params.get('months')) || 12
  const categoryID = params.get('category') ?? ''
  const search = params.get('q') ?? ''
  const sort = (params.get('sort') as SortKey) || 'total'
  const [page, setPage] = useState(0)

  const range = useMemo(() => windowRange(months), [months])

  // Search is NOT in the query key — it is applied locally, so putting it there
  // would refetch the whole window on every keystroke to no purpose.
  const explorer = useQuery({
    queryKey: ['merchant-explorer', range.from, range.to, categoryID],
    queryFn: () =>
      api.merchantExplorer({
        ...range,
        ...(categoryID ? { category_id: categoryID } : {}),
      }),
  })

  // The recurring detector already knows which merchants are subscriptions, so the
  // badge costs nothing beyond a request the Spending page also makes.
  const recurring = useQuery({ queryKey: ['recurring'], queryFn: api.recurring })
  const recurringKeys = useMemo(
    () => new Set((recurring.data ?? []).map((r) => r.merchant_key)),
    [recurring.data],
  )

  const categories = useQuery({ queryKey: ['categories'], queryFn: api.categories })

  // Memoised so the empty fallback does not hand out a fresh array identity on
  // every render, which would defeat the filter/sort memos below.
  const rows = useMemo(() => explorer.data?.merchants ?? [], [explorer.data])

  const update = (next: Record<string, string>) => {
    const merged = new URLSearchParams(params)
    for (const [k, v] of Object.entries(next)) {
      if (v === '') merged.delete(k)
      else merged.set(k, v)
    }
    setParams(merged, { replace: true })
    setPage(0)
  }

  const matched = useMemo(() => filterRows(rows, search), [rows, search])
  const sorted = useMemo(() => sortRows(matched, sort), [matched, sort])
  const pageCount = Math.max(1, Math.ceil(sorted.length / PAGE_SIZE))
  const current = Math.min(page, pageCount - 1)
  const visible = sorted.slice(current * PAGE_SIZE, current * PAGE_SIZE + PAGE_SIZE)

  // The bar scale is the biggest value on this page, not in the whole list: a page
  // of small merchants deep in the list would otherwise render as invisible slivers
  // against a top-ten leader that is nowhere in sight.
  const max = Math.max(...visible.map((m) => Number(m.total)), 0)

  const windowTotal = Number(explorer.data?.window_total ?? 0)

  return (
    <section className="glass p-6">
      <div className="mb-5 flex flex-wrap items-start justify-between gap-4">
        <div className="min-w-0">
          <h2 className="text-lg font-medium">Where the money goes</h2>
          <Concentration
            rows={rows}
            windowTotal={windowTotal}
            isPending={explorer.isPending}
          />
        </div>
        <select
          className="field w-44"
          aria-label="Time window"
          value={matchedWindow(range.from, range.to)}
          onChange={(e) => update({ months: e.target.value })}
        >
          {WINDOWS.map((w) => (
            <option key={w.months} value={w.months}>
              {w.label}
            </option>
          ))}
        </select>
      </div>

      {rows.length > 0 && <ParetoStrip rows={rows} windowTotal={windowTotal} />}

      <div className="mb-4 flex flex-wrap items-center gap-2">
        <input
          className="field min-w-0 flex-1 sm:max-w-xs"
          type="search"
          placeholder="Search any merchant…"
          aria-label="Search merchants"
          value={search}
          onChange={(e) => update({ q: e.target.value })}
        />
        <select
          className="field w-auto"
          aria-label="Category"
          value={categoryID}
          onChange={(e) => update({ category: e.target.value })}
        >
          <option value="">All categories</option>
          {spendCategories(categories.data ?? []).map((c) => (
            <option key={c.id} value={c.id}>
              {c.name}
            </option>
          ))}
        </select>
        <select
          className="field w-auto"
          aria-label="Sort by"
          value={sort}
          onChange={(e) => update({ sort: e.target.value })}
        >
          {SORTS.map((s) => (
            <option key={s.key} value={s.key}>
              {s.label}
            </option>
          ))}
        </select>
      </div>

      {explorer.isPending ? (
        <p className="py-6 text-center text-sm text-mist-400">Loading…</p>
      ) : explorer.isError ? (
        <p className="py-6 text-center text-sm text-red-300">
          Couldn’t load your merchants.
        </p>
      ) : rows.length === 0 ? (
        <Empty>No spending in this window.</Empty>
      ) : sorted.length === 0 ? (
        <div className="py-6 text-center">
          <p className="text-sm text-mist-400">
            No merchant matches “{search}”.
          </p>
          <button
            className="btn-ghost mt-2 text-sm"
            onClick={() => update({ q: '' })}
          >
            Clear the search
          </button>
        </div>
      ) : (
        <>
          <div className="space-y-2.5">
            {visible.map((m) => (
              <MerchantRow
                key={m.merchant_key}
                merchant={m}
                max={max}
                range={range}
                isRecurring={recurringKeys.has(m.merchant_key)}
              />
            ))}
          </div>

          <Pager
            page={current}
            pageCount={pageCount}
            shown={visible.length}
            first={current * PAGE_SIZE + 1}
            total={sorted.length}
            onChange={setPage}
          />

          {explorer.data?.truncated && (
            <p className="mt-2 text-xs text-mist-500">
              Showing the {rows.length} biggest of {explorer.data.merchant_count}{' '}
              merchants. Search reaches only the ones listed here.
            </p>
          )}
        </>
      )}

      {!explorer.isPending && rows.length > 0 && (
        <div className="mt-6 grid gap-4 md:grid-cols-2">
          <NewMerchants rows={rows} range={range} />
          <GoneQuiet merchants={explorer.data?.lapsed ?? []} />
        </div>
      )}
    </section>
  )
}

/** Spending categories only — income and transfers are not merchant spend. */
function spendCategories(all: Category[]): Category[] {
  return all
    .filter((c) => !c.is_income && !c.is_transfer)
    .sort((a, b) => a.name.localeCompare(b.name))
}

/**
 * Local search, over the display name only.
 *
 * The server matched the needle against every raw descriptor as well, which is
 * what finds a merchant by the text the bank prints. That work cannot be repeated
 * here — the response carries no descriptors — so a needle the server has NOT seen
 * only narrows what it already returned. In practice the server sees the needle on
 * the next fetch of the window and the two agree; between them, filtering by name
 * is the honest subset rather than a wrong answer.
 */
function filterRows(rows: MerchantExplorerRow[], search: string): MerchantExplorerRow[] {
  const needle = search.trim().toLowerCase()
  if (needle === '') return rows
  return rows.filter(
    (m) =>
      m.merchant.toLowerCase().includes(needle) ||
      m.merchant_key.toLowerCase().includes(needle),
  )
}

function sortRows(rows: MerchantExplorerRow[], sort: SortKey): MerchantExplorerRow[] {
  const out = [...rows]
  switch (sort) {
    case 'count':
      return out.sort((a, b) => b.transaction_count - a.transaction_count)
    case 'average':
      return out.sort((a, b) => Number(b.average) - Number(a.average))
    case 'recent':
      return out.sort((a, b) => b.last_seen.localeCompare(a.last_seen))
    case 'change':
      // By absolute change, not percent. A $2 subscription that went to $6 is a
      // 200% rise and nobody cares; a $400 jump matters at any percentage.
      return out.sort((a, b) => delta(b) - delta(a))
    default:
      return out.sort((a, b) => Number(b.total) - Number(a.total))
  }
}

function delta(m: MerchantExplorerRow): number {
  return Number(m.total) - Number(m.prior_total)
}

function MerchantRow({
  merchant: m,
  max,
  range,
  isRecurring,
}: {
  merchant: MerchantExplorerRow
  max: number
  range: { from: string; to: string }
  isRecurring: boolean
}) {
  const pct = max > 0 ? (Number(m.total) / max) * 100 : 0

  return (
    <div className="grid grid-cols-[1fr_auto] items-center gap-x-3 gap-y-1 rounded-lg py-1.5 sm:grid-cols-[14rem_1fr_7rem_5rem]">
      <div className="flex min-w-0 items-center gap-2">
        <MerchantLink
          name={m.merchant}
          merchantKey={m.merchant_key}
          range={range}
          className="text-sm text-mist-100"
        />
        {isRecurring && (
          <span className="shrink-0 rounded-full border border-rune-400/30 bg-rune-400/10 px-1.5 py-0.5 text-[10px] text-rune-300">
            recurring
          </span>
        )}
      </div>

      {/* One measure across a categorical dimension is ONE series, so every bar
          carries the same colour — hue would encode nothing. */}
      <span className="order-last col-span-2 h-2.5 overflow-hidden rounded-full bg-white/5 sm:order-none sm:col-span-1">
        <span
          className="block h-full rounded-full"
          style={{ width: `${Math.max(pct, 1)}%`, backgroundColor: SINGLE_SERIES }}
        />
      </span>

      <div className="text-right">
        <span className="tabular block text-sm text-mist-200">
          {formatMoney(m.total)}
        </span>
        <span className="block text-[11px] text-mist-500">
          {m.transaction_count} charge{m.transaction_count === 1 ? '' : 's'}
        </span>
      </div>

      <div className="hidden text-right sm:block">
        <Change merchant={m} />
        {m.category_name && (
          <CategoryLink
            name={m.category_name}
            categoryID={m.category_id}
            range={range}
            className="max-w-full truncate text-[11px] text-mist-500"
          />
        )}
      </div>
    </div>
  )
}

/**
 * Change against the equivalent prior window.
 *
 * A merchant with no prior spend gets "new" rather than a percentage: a change
 * from zero has no ratio, and rendering ∞% (or 100%) would be inventing a figure.
 */
function Change({ merchant: m }: { merchant: MerchantExplorerRow }) {
  const prior = Number(m.prior_total)
  if (m.is_new) {
    return <span className="block text-xs text-rune-300">new</span>
  }
  if (prior <= 0) {
    return <span className="block text-xs text-mist-500">back</span>
  }
  const change = ((Number(m.total) - prior) / prior) * 100
  // Under a couple of percent is noise, not a trend.
  if (Math.abs(change) < 2) {
    return <span className="block text-xs text-mist-500">—</span>
  }
  const up = change > 0
  return (
    <span
      className="tabular block text-xs"
      style={{ color: up ? STATUS.serious : STATUS.good }}
      title={`was ${formatMoney(m.prior_total)} over the previous period`}
    >
      {up ? '▲' : '▼'} {Math.abs(Math.round(change))}%
    </span>
  )
}

/**
 * How concentrated the spending is — one sentence, because that is what the
 * question deserves. "Your top 10 merchants are 43% of everything you spent" says
 * more about a household's shape than any chart of the same numbers.
 */
function Concentration({
  rows,
  windowTotal,
  isPending,
}: {
  rows: MerchantExplorerRow[]
  windowTotal: number
  isPending: boolean
}) {
  if (isPending) {
    return <p className="mt-1 text-sm text-mist-300">Loading…</p>
  }
  if (rows.length === 0 || windowTotal <= 0) {
    return (
      <p className="mt-1 text-sm text-mist-300">
        Pick a merchant to see its history, how it’s filed, and every charge.
      </p>
    )
  }

  const top = rows.slice(0, CONCENTRATION_TOP_N)
  const topTotal = top.reduce((sum, m) => sum + Number(m.total), 0)
  const share = Math.round((topTotal / windowTotal) * 100)

  return (
    <p className="mt-1 text-sm text-mist-300">
      {rows.length <= CONCENTRATION_TOP_N ? (
        <>
          {rows.length} merchant{rows.length === 1 ? '' : 's'}, {formatMoney(String(windowTotal))} in
          all.
        </>
      ) : (
        <>
          Your top {top.length} merchants are <strong>{share}%</strong> of the{' '}
          {formatMoney(String(windowTotal))} you spent, across {rows.length} merchants
          in all.
        </>
      )}
    </p>
  )
}

/**
 * The concentration curve as one stacked bar: each of the top merchants as its
 * share of the window, then everything else.
 *
 * A single bar rather than a cumulative line because the question is "how much of
 * my spending is a handful of businesses?", and a stacked share answers that at a
 * glance where a Pareto curve asks to be read.
 */
function ParetoStrip({
  rows,
  windowTotal,
}: {
  rows: MerchantExplorerRow[]
  windowTotal: number
}) {
  if (windowTotal <= 0) return null
  const top = rows.slice(0, CONCENTRATION_TOP_N)
  const topTotal = top.reduce((sum, m) => sum + Number(m.total), 0)
  const rest = Math.max(0, windowTotal - topTotal)

  return (
    <div className="mb-5">
      <div className="flex h-3 gap-px overflow-hidden rounded-full bg-white/5">
        {top.map((m, i) => (
          <span
            key={m.merchant_key}
            className="h-full first:rounded-l-full"
            style={{
              width: `${(Number(m.total) / windowTotal) * 100}%`,
              // One series, stepped only in opacity so the segments read as
              // ranked slices of one measure rather than as different things.
              backgroundColor: SINGLE_SERIES,
              opacity: 1 - i * 0.07,
            }}
            title={`${m.merchant} · ${formatMoney(m.total)}`}
          />
        ))}
        {rest > 0 && (
          <span
            className="h-full rounded-r-full bg-white/10"
            style={{ width: `${(rest / windowTotal) * 100}%` }}
            title={`Everyone else · ${formatMoney(String(rest))}`}
          />
        )}
      </div>
      <p className="mt-1.5 text-[11px] text-mist-500">
        Each segment is one of your biggest merchants; the pale tail is everyone
        else.
      </p>
    </div>
  )
}

/**
 * Merchants charging this household for the first time.
 *
 * `is_new` is computed against all of history rather than against the previous
 * period, so a merchant you last used two years ago does not turn up here as a
 * discovery.
 */
function NewMerchants({
  rows,
  range,
}: {
  rows: MerchantExplorerRow[]
  range: { from: string; to: string }
}) {
  const fresh = rows
    .filter((m) => m.is_new)
    .sort((a, b) => Number(b.total) - Number(a.total))
    .slice(0, 5)

  return (
    <Panel title="New this period">
      {fresh.length === 0 ? (
        <p className="text-xs text-mist-500">
          No merchants you hadn’t used before.
        </p>
      ) : (
        <ul className="space-y-1.5">
          {fresh.map((m) => (
            <li key={m.merchant_key} className="flex items-baseline justify-between gap-3">
              <MerchantLink
                name={m.merchant}
                merchantKey={m.merchant_key}
                range={range}
                className="min-w-0 text-sm text-mist-200"
              />
              <span className="shrink-0 text-right">
                <span className="tabular block text-sm text-mist-200">
                  {formatMoney(m.total)}
                </span>
                <span className="block text-[11px] text-mist-500">
                  since {formatDate(m.first_seen)}
                </span>
              </span>
            </li>
          ))}
        </ul>
      )}
    </Panel>
  )
}

/**
 * Merchants that used to bill on a regular cadence and have stopped — the
 * forgotten-cancellation list, and the one panel here that can find money.
 *
 * These merchants are by definition absent from the window, so they come from the
 * server as their own list rather than from the rows above. It is the exact
 * complement of the recurring detector: same cadence rules, opposite activity test.
 */
function GoneQuiet({ merchants }: { merchants: LapsedMerchant[] }) {
  const shown = merchants.slice(0, 5)
  return (
    <Panel title="Gone quiet">
      {shown.length === 0 ? (
        <p className="text-xs text-mist-500">
          Nothing that used to bill regularly has stopped.
        </p>
      ) : (
        <>
          <ul className="space-y-1.5">
            {shown.map((m) => (
              <li
                key={m.merchant_key}
                className="flex items-baseline justify-between gap-3"
              >
                {/* No window on this link: these charges are outside the window
                    being explored, so carrying it through would open an empty
                    merchant page. */}
                <Link
                  to={merchantDetailPath(m.merchant_key)}
                  className="min-w-0 truncate text-sm text-mist-200 underline decoration-white/20 underline-offset-4 hover:decoration-white/60"
                >
                  {m.merchant}
                </Link>
                <span className="shrink-0 text-right">
                  <span className="tabular block text-sm text-mist-200">
                    ~{formatMoney(m.monthly_estimate)}/mo
                  </span>
                  <span className="block text-[11px] text-mist-500">
                    last {formatDate(m.last_seen)}
                  </span>
                </span>
              </li>
            ))}
          </ul>
          <p className="mt-2 text-[11px] text-mist-500">
            A subscription that stopped billing — cancelled, or a card that expired.
          </p>
        </>
      )}
    </Panel>
  )
}

function Panel({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div className="rounded-xl border border-white/10 p-4">
      <h3 className="mb-2 text-sm font-medium text-mist-100">{title}</h3>
      {children}
    </div>
  )
}

function Pager({
  page,
  pageCount,
  shown,
  first,
  total,
  onChange,
}: {
  page: number
  pageCount: number
  shown: number
  first: number
  total: number
  onChange: (page: number) => void
}) {
  if (total <= PAGE_SIZE) {
    return (
      <p className="mt-4 text-center text-xs text-mist-500">
        {total} merchant{total === 1 ? '' : 's'}
      </p>
    )
  }
  return (
    <div className="mt-4 flex items-center justify-center gap-4 text-xs text-mist-500">
      <button
        className="btn-ghost px-2 py-1 text-xs disabled:opacity-40"
        disabled={page === 0}
        onClick={() => onChange(page - 1)}
      >
        ‹ Prev
      </button>
      <span className="tabular">
        {first}–{first + shown - 1} of {total}
      </span>
      <button
        className="btn-ghost px-2 py-1 text-xs disabled:opacity-40"
        disabled={page >= pageCount - 1}
        onClick={() => onChange(page + 1)}
      >
        Next ›
      </button>
    </div>
  )
}

function Empty({ children }: { children: ReactNode }) {
  return <p className="py-6 text-center text-sm text-mist-400">{children}</p>
}
