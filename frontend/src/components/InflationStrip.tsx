import { Link } from 'react-router-dom'
import { formatRate, useInflation } from '../lib/inflation'

/**
 * The Dashboard's inflation line: what prices did this year, set against what
 * the household's own money did (doc 27).
 *
 * The comparison is the entire point. "Inflation is 3.1%" is trivia nobody
 * acts on; "prices are up 3.1% and your net worth is up 2.4%, so you are
 * slightly behind" is a fact about this household. The strip renders the
 * headline alone when there is no comparison to draw, and nothing at all when
 * there is no series — never a placeholder.
 */
export function InflationStrip() {
  const inflation = useInflation()
  const data = inflation.data

  if (!data?.available || !data.ytd_rate || !data.base_label) return null

  const ctx = data.context
  const nominal = ctx?.net_worth_change
  const real = ctx?.net_worth_real_change
  const income = ctx?.income_change

  return (
    <div className="rounded-xl border border-white/10 bg-white/5 px-4 py-3 text-sm">
      <p className="text-mist-100">
        Prices are up{' '}
        <span className="tabular font-medium">{formatRate(data.ytd_rate)}</span>{' '}
        {data.ytd_label ?? 'this year'}.
        {real !== undefined && nominal !== undefined && (
          <>
            {' '}
            Your net worth is up{' '}
            <span className="tabular font-medium">{formatRate(nominal)}</span> —{' '}
            <span className="tabular font-medium">{formatRate(real)}</span> once
            that is taken out.
          </>
        )}
        {real === undefined && nominal !== undefined && (
          <>
            {' '}
            Your net worth is up{' '}
            <span className="tabular font-medium">{formatRate(nominal)}</span> in
            nominal terms.
          </>
        )}
      </p>

      {income !== undefined && (
        <p className="mt-1 text-mist-300">
          Income so far this year is{' '}
          <span className="tabular">{formatRate(income)}</span> against the same
          span last year.
        </p>
      )}

      <p className="mt-1.5 text-xs text-mist-500">
        {data.series}. Real figures across the app are {`in ${data.base_label}`}{' '}
        dollars —{' '}
        <Link to="/net-worth" className="underline underline-offset-2">
          see the net-worth trend in them
        </Link>
        .{data.stale && data.stale_note ? ` ${data.stale_note}` : ''}
      </p>
    </div>
  )
}
