import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { api, type DigestEntry, type DigestPayload } from '../lib/api'
import { formatDate } from '../lib/money'
import { STATUS } from '../components/charts/tokens'
import { EmptyState } from '../components/EmptyState'
import { SkeletonRows } from '../components/Skeleton'

/**
 * The digest history: what happened with your money, period by period.
 *
 * Two rules govern everything on this page.
 *
 * 1. **Render the stored payload, never a live refetch.** A digest is a
 *    statement about a past period. If the figures here disagree with the
 *    Spending page today, that is correct: a transaction was recategorised
 *    since, and last week's digest must still say what it said when it was
 *    read. Nothing on this page calls a reporting endpoint.
 *
 * 2. **Never do arithmetic on a figure.** Every money value arrives as a
 *    finished display string, formatted server-side in decimal. This file
 *    prints them; it does not parse, sum or reformat them.
 */
export function Digest() {
  const qc = useQueryClient()
  const [limit, setLimit] = useState(12)
  const [selectedID, setSelectedID] = useState<string | null>(null)

  const list = useQuery({
    queryKey: ['digests', limit],
    queryFn: () => api.digests({ limit }),
  })

  const markRead = useMutation({
    mutationFn: api.markDigestRead,
    onSuccess: () => qc.invalidateQueries({ queryKey: ['digests'] }),
  })

  const entries = list.data?.entries ?? []
  // Default to the newest, and follow it as new digests arrive — but never
  // yank the selection away from an older entry the reader has opened.
  const selected =
    entries.find((e) => e.id === selectedID) ?? entries[0] ?? null

  // Reading one marks it read. Deliberately not tied to a button: the entry is
  // fully rendered on screen, so "have you seen this" is already answered.
  useEffect(() => {
    if (selected && !selected.read_at && !markRead.isPending) {
      markRead.mutate(selected.id)
    }
    // markRead is a stable mutation handle; re-running on its own state would
    // loop.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [selected?.id, selected?.read_at])

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-2xl font-semibold">Digest</h1>
        <p className="mt-1 text-mist-300">
          Your periodic recap, kept. Each one shows the figures as they stood
          when it was written — not as they look today.
        </p>
      </div>

      {list.isPending ? (
        <section className="glass p-6">
          <SkeletonRows count={5} />
        </section>
      ) : entries.length === 0 ? (
        <section className="glass p-6">
          <EmptyState
            title="No digests yet"
            action={
              <Link to="/settings?tab=digest" className="btn-ghost px-4 py-2 text-sm">
                Digest settings
              </Link>
            }
          >
            One is written for you on your cadence — weekly on a Monday, or on
            the 1st for a monthly cadence. You can also send one now from
            Settings to see what it looks like.
          </EmptyState>
        </section>
      ) : (
        <div className="grid gap-6 lg:grid-cols-[minmax(0,1fr)_18rem]">
          {selected && <DigestDetail entry={selected} />}

          <aside className="space-y-3">
            <h2 className="text-sm font-medium text-mist-300">Earlier</h2>
            <ul className="space-y-1">
              {entries.map((e) => (
                <li key={e.id}>
                  <button
                    onClick={() => setSelectedID(e.id)}
                    className={`flex w-full items-center gap-2 rounded-lg px-3 py-2 text-left text-sm transition ${
                      e.id === selected?.id
                        ? 'bg-white/10 text-mist-100'
                        : 'text-mist-300 hover:bg-white/5 hover:text-mist-100'
                    }`}
                  >
                    {/* Unread dot. Sized as a fixed slot either way so the
                        labels do not shift as entries are read. */}
                    <span
                      aria-hidden
                      className={`h-1.5 w-1.5 shrink-0 rounded-full ${
                        e.read_at ? 'bg-transparent' : 'bg-arcane-500'
                      }`}
                    />
                    <span className="min-w-0 flex-1 truncate">{e.label}</span>
                    <span className="shrink-0 text-xs text-mist-500">
                      {e.period_key}
                    </span>
                    {!e.read_at && <span className="sr-only">unread</span>}
                  </button>
                </li>
              ))}
            </ul>
            {list.data && list.data.total > entries.length && (
              <button
                className="btn-ghost w-full px-4 py-2 text-sm"
                onClick={() => setLimit((n) => n + 12)}
              >
                Load older
              </button>
            )}
          </aside>
        </div>
      )}
    </div>
  )
}

/** One digest, rendered entirely from its stored payload. */
function DigestDetail({ entry }: { entry: DigestEntry }) {
  const p = entry.payload

  return (
    <article className="space-y-6">
      <section className="glass p-6">
        <div className="flex flex-wrap items-baseline justify-between gap-2">
          <h2 className="text-xl font-semibold">{entry.label}</h2>
          <p className="text-sm text-mist-500">
            {formatDate(p.period_start)} – {formatDate(p.period_end)}
            {p.in_progress && ' · in progress'}
          </p>
        </div>

        {entry.narrative && (
          <div className="prose-invert mt-4 max-w-none space-y-3 text-mist-200">
            <ReactMarkdown remarkPlugins={[remarkGfm]}>
              {entry.narrative}
            </ReactMarkdown>
          </div>
        )}

        <dl className="mt-6 grid grid-cols-2 gap-4 sm:grid-cols-4">
          <Figure label="In" value={p.income} />
          <Figure label="Out" value={p.spending} />
          <Figure label="Left over" value={p.leftover} />
          <Figure label="Savings rate" value={p.savings_rate} />
        </dl>

        <p className="mt-4 text-xs text-mist-500">
          {p.transaction_count} transaction
          {p.transaction_count === 1 ? '' : 's'}
          {p.prior_spending && ` · ${p.prior_spending} spent the month before`}
          {p.gross_savings_rate &&
            ` · ${p.gross_savings_rate} of gross pay saved`}
        </p>
      </section>

      {p.net_worth && (
        <section className="glass p-6">
          <h3 className="text-lg font-medium">Net worth</h3>
          <p className="tabular mt-2 text-3xl font-semibold text-[#f2d492]">
            {p.net_worth.current}
          </p>
          <p className="mt-1 text-sm text-mist-400">
            {p.net_worth.direction && p.net_worth.change
              ? `${p.net_worth.direction === 'up' ? 'Up' : p.net_worth.direction === 'down' ? 'Down' : 'Flat at'} ${stripSign(p.net_worth.change)} over this period, from ${p.net_worth.start}`
              : `as of ${formatDate(p.net_worth.as_of)}`}
          </p>
        </section>
      )}

      {p.above_baseline.length > 0 && (
        <section className="glass p-6">
          <h3 className="text-lg font-medium">Running above usual</h3>
          <ul className="mt-4 space-y-3">
            {p.above_baseline.map((d) => (
              <li key={d.name} className="flex flex-wrap justify-between gap-2 text-sm">
                <span className="text-mist-200">{d.name}</span>
                <span className="tabular text-mist-400">
                  {d.this_month} · usually {d.typical} ·{' '}
                  <span style={{ color: STATUS.warning }}>{d.over} over</span>
                </span>
              </li>
            ))}
          </ul>
        </section>
      )}

      <SpendSection payload={p} />

      {p.budgets.length > 0 && (
        <section className="glass p-6">
          <h3 className="text-lg font-medium">Budgets</h3>
          <ul className="mt-4 space-y-4">
            {p.budgets.map((b) => (
              <li key={b.slug || b.name}>
                <div className="flex justify-between text-sm">
                  <span className="text-mist-200">{b.name}</span>
                  <span className="tabular text-mist-400">
                    {b.spent} of {b.available}
                  </span>
                </div>
                <div
                  className="mt-1.5 h-1.5 w-full overflow-hidden rounded-full bg-white/10"
                  role="img"
                  aria-label={`${b.percent_used}% used`}
                >
                  <div
                    className="h-full rounded-full"
                    style={{
                      // Capped at 100% so an overspent envelope stays inside
                      // its track; the colour is what says "over", not a bar
                      // spilling out of its container.
                      width: `${Math.min(b.percent_used, 100)}%`,
                      background: b.over ? STATUS.critical : STATUS.good,
                    }}
                  />
                </div>
                <p className="mt-1 text-xs text-mist-500">
                  {b.over
                    ? `${stripSign(b.remaining)} over`
                    : `${b.remaining} left`}
                </p>
              </li>
            ))}
          </ul>
        </section>
      )}

      {p.upcoming_bills.length > 0 && (
        <section className="glass p-6">
          <h3 className="text-lg font-medium">Coming up</h3>
          <ul className="mt-4 space-y-2">
            {p.upcoming_bills.map((b) => (
              <li
                key={`${b.label}-${b.due_date}`}
                className="flex justify-between text-sm"
              >
                <span className="text-mist-200">{b.label}</span>
                <span className="tabular text-mist-400">
                  {b.amount} · {formatDate(b.due_date)}
                </span>
              </li>
            ))}
          </ul>
        </section>
      )}

      {p.insights.length > 0 && (
        <section className="glass p-6">
          <h3 className="text-lg font-medium">Worth a look</h3>
          <ul className="mt-4 space-y-3">
            {p.insights.map((i) => (
              <li key={i.id}>
                <p className="text-sm font-medium text-mist-100">{i.title}</p>
                <p className="mt-0.5 text-sm text-mist-400">{i.body}</p>
              </li>
            ))}
          </ul>
          <Link
            to="/insights"
            className="mt-4 inline-block text-sm text-rune-300 hover:underline"
          >
            Open the feed →
          </Link>
        </section>
      )}
    </article>
  )
}

/** The two spending breakdowns, in one panel when either has anything to say. */
function SpendSection({ payload: p }: { payload: DigestPayload }) {
  if (p.top_categories.length === 0 && p.largest_transactions.length === 0) {
    return null
  }
  return (
    <section className="glass grid gap-8 p-6 sm:grid-cols-2">
      {p.top_categories.length > 0 && (
        <div>
          <h3 className="text-lg font-medium">Where it went</h3>
          <ul className="mt-4 space-y-2">
            {p.top_categories.map((c) => (
              <li key={c.name} className="flex justify-between text-sm">
                {/* Links through where the digest knows the slug. The figure
                    beside it is still the stored one, so a click is a move to
                    today's data, not a refresh of this line. */}
                {c.slug ? (
                  <Link
                    to={`/spending?category=${encodeURIComponent(c.slug)}`}
                    className="text-mist-200 hover:underline"
                  >
                    {c.name}
                  </Link>
                ) : (
                  <span className="text-mist-200">{c.name}</span>
                )}
                <span className="tabular text-mist-400">{c.total}</span>
              </li>
            ))}
          </ul>
        </div>
      )}

      {p.largest_transactions.length > 0 && (
        <div>
          <h3 className="text-lg font-medium">Biggest purchases</h3>
          <ul className="mt-4 space-y-2">
            {p.largest_transactions.map((t, i) => (
              <li
                key={`${t.merchant}-${t.date}-${i}`}
                className="flex justify-between gap-3 text-sm"
              >
                <span className="min-w-0 truncate text-mist-200">
                  {t.merchant}
                </span>
                <span className="tabular shrink-0 text-mist-400">
                  {t.amount} · {t.date}
                </span>
              </li>
            ))}
          </ul>
        </div>
      )}
    </section>
  )
}

function Figure({ label, value }: { label: string; value?: string }) {
  return (
    <div>
      <dt className="text-sm text-mist-300">{label}</dt>
      <dd className="tabular mt-1 text-xl font-semibold text-mist-100">
        {value || '—'}
      </dd>
    </div>
  )
}

/**
 * Drops a leading minus from an already-formatted amount, for the places where
 * the surrounding words carry the direction ("$40.00 over", "Down $120.00").
 * String surgery on a display value — no parsing, no arithmetic.
 */
function stripSign(amount: string): string {
  return amount.startsWith('-') ? amount.slice(1) : amount
}
