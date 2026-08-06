import type { CategorySpend, ChatToolResult, DaySpend, TrendPoint } from '../lib/api'
import { CategoryBars } from './charts/CategoryBars'
import { DayBars } from './charts/DayBars'
import { TrendChart } from './charts/TrendChart'

/**
 * A chart rendered inline in an assistant turn, drawn from the tool result the
 * turn already computed.
 *
 * THE MODEL NEVER PICKS A CHART. The mapping below is a deterministic map from
 * TOOL NAME to component, hardcoded here — the model does not choose a chart
 * type, does not shape the data, and does not label an axis. A wrong tool pick
 * renders the wrong chart, which is the same visible, debuggable failure mode as
 * a wrong tool pick rendering wrong prose today.
 *
 * The consequence worth stating: the trend chart in the chat, the trend chart on
 * the Spending page, and the model's stated figures all derive from the same
 * server-side engine, so they cannot drift. That is the property most "AI chart"
 * products lose — the generated chart quietly disagrees with the dashboard — and
 * the architecture here forbids it rather than testing for it.
 *
 * An unmapped tool renders NOTHING and the prose still stands. Silence is the
 * correct answer for a tool whose result has no honest chart: half-fitting a
 * result into a component that expects different data is how a chart starts
 * lying.
 */
export function ChartAttachment({ frame }: { frame: ChatToolResult }) {
  const chart = renderChart(frame)
  if (!chart) return null

  return (
    <figure className="mt-3 rounded-xl border border-white/10 bg-white/[0.03] p-3">
      {chart}
      <figcaption className="mt-2 text-[11px] text-mist-500">
        Drawn from the same figures quoted above.
      </figcaption>
    </figure>
  )
}

function renderChart(frame: ChatToolResult) {
  switch (frame.tool) {
    case 'monthly_trend': {
      const r = frame.result as
        | { months?: unknown; avg_leftover?: string }
        | undefined
      const months = asArray(r?.months)
      if (months.length === 0) return null
      // The average is the SERVER's figure, passed straight through. Deriving
      // it here from the series would be a client-computed money figure, which
      // is the one thing this whole architecture is arranged to prevent.
      return (
        <TrendChart
          data={months as unknown as TrendPoint[]}
          avgLeftover={r?.avg_leftover}
        />
      )
    }

    case 'spend_by_category':
    case 'category_averages':
    case 'budget_status': {
      const rows = asArray(frame.result)
      const bars = rows.flatMap((row) => toCategorySpend(frame.tool, row))
      if (bars.length === 0) return null
      // No onSelect: these rows carry no category id (the tool answers in
      // names, which is what the model needs), so a click has nowhere to go.
      return <CategoryBars data={bars} />
    }

    case 'spending_by_day': {
      const rows = asArray(frame.result)
      const days = rows.flatMap((row) => toDaySpend(row))
      if (days.length === 0) return null
      const [year, month] = days[0].day.split('-').map(Number)
      if (!year || !month) return null
      // No reference line: the tool answers for one month and does not carry
      // the prior month's daily mean. Zero is DayBars' documented "no reference
      // line" input, not a claim that last month averaged nothing.
      return <DayBars year={year} month={month} days={days} lastMonthAvgDaily={0} />
    }

    // Deliberately unmapped for now, each for a reason rather than an omission:
    //
    //   top_merchants     — MerchantPareto needs a prior-period figure and a
    //                       window total the tool does not return. Mapping it
    //                       would mean inventing both.
    //   net_worth         — NetWorthComposition plots a SERIES; the tool returns
    //                       one snapshot. A one-point composition chart is a
    //                       table with extra steps.
    //
    // Both become mappable when a tool returns the shape honestly. Until then
    // the prose answers the question and no chart claims to.
    default:
      return null
  }
}

/** Narrows an unknown tool result to an array of records, or nothing. */
function asArray(value: unknown): Record<string, unknown>[] {
  if (!Array.isArray(value)) return []
  return value.filter(
    (v): v is Record<string, unknown> => typeof v === 'object' && v !== null,
  )
}

/**
 * Adapts one tool row to the shape CategoryBars reads.
 *
 * The three tools name their measure differently — spend_by_category returns
 * `spent`, category_averages `monthly_average`, budget_status `spent` against a
 * budget — so the field is picked per tool rather than by probing, which would
 * silently chart the wrong column the day a tool grows a second money field.
 *
 * `slug` is derived from the name only to give CategoryBars a stable React key
 * and its "Other" fold something to compare against. It is not an identifier and
 * is never sent anywhere.
 */
function toCategorySpend(tool: string, row: Record<string, unknown>): CategorySpend[] {
  const name = typeof row.category === 'string' ? row.category : ''
  if (!name) return []

  const field = tool === 'category_averages' ? 'monthly_average' : 'spent'
  const total = row[field]
  if (typeof total !== 'string') return []

  return [
    {
      category_id: '',
      name,
      slug: name.toLowerCase().replace(/[^a-z0-9]+/g, '-'),
      color: null,
      is_fixed: false,
      total,
      transaction_count: typeof row.count === 'number' ? row.count : 0,
    },
  ]
}

function toDaySpend(row: Record<string, unknown>): DaySpend[] {
  if (typeof row.day !== 'string' || typeof row.spending !== 'string') return []
  return [{ day: row.day, spending: row.spending }]
}
