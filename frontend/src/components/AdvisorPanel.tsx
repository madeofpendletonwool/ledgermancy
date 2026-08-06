import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api, type AdviceOption } from '../lib/api'
import { formatMoney } from '../lib/money'

/**
 * The Dashboard advisor panel: what the household's slack would DO if it were
 * not spent, as a ranked list of computed options.
 *
 * Three rules this component exists to hold, all of them about honesty rather
 * than layout.
 *
 * FIRST, THE COPY IS CONDITIONAL. `slack` is the identical figure the Budgets
 * page prints as "safe to spend" — money the household is explicitly told it may
 * spend freely. This panel must never say "you have $400 available"; it says "if
 * you don't spend this, here is what it would do". Same number, and the two
 * pages no longer give opposite instructions about it.
 *
 * SECOND, THE ORDER IS THE SERVER'S. The list arrives ranked by a published
 * waterfall the app is accountable for. Nothing here sorts, filters or promotes
 * — a client-side re-sort would replace that rule with an invisible one.
 *
 * THIRD, IT APPEARS ONLY WHEN THERE IS SOMETHING TO SAY. No empty state, no
 * "nothing to advise" box. A household with no debts, no goals and no retirement
 * accounts degrades to silence.
 */
export function AdvisorPanel({ onAccept }: { onAccept?: (option: AdviceOption) => void } = {}) {
  const advice = useQuery({ queryKey: ['advisor'], queryFn: api.advisor })

  // Presence-gated, like the insight card. Pending renders nothing rather than a
  // skeleton: this panel is not guaranteed to appear at all, and reserving space
  // for something that may never arrive makes the page jump.
  if (advice.isPending || advice.isError) return null
  const data = advice.data
  if (!data || !data.significant || data.options.length === 0) return null

  const tradeoffs = data.options.filter((o) => o.tradeoff)
  const ranked = data.options.filter((o) => !o.tradeoff && !o.unranked)
  const unranked = data.options.filter((o) => o.unranked)

  return (
    <section className="glass p-6">
      <div className="mb-1 flex items-baseline justify-between gap-3">
        <h2 className="text-lg font-medium">
          If you don&rsquo;t spend {formatMoney(data.slack)} this month
        </h2>
      </div>
      <p className="mb-5 text-sm text-mist-500">
        {data.slack_basis === 'after_bills'
          ? 'Based on a typical month, after the bills already scheduled.'
          : 'Based on a typical month — the median of your trailing history, not this month so far.'}
        {data.income_months > 0 && data.income_months < 3
          ? ` Only ${data.income_months} month${data.income_months === 1 ? '' : 's'} of income history so far, so treat this as provisional.`
          : ''}
      </p>

      {data.narrative && (
        <p className="mb-5 whitespace-pre-line text-sm leading-relaxed text-mist-300">
          {data.narrative}
        </p>
      )}

      <ol className="space-y-3">
        {ranked.map((o, i) => (
          <OptionRow key={o.key} option={o} rank={i + 1} hurdle={data.hurdle} onAccept={onAccept} />
        ))}
      </ol>

      {tradeoffs.length > 0 && (
        <div className="mt-5">
          {/*
            Tier 7 is rendered as a COMPARISON, never as more ranked rows. The
            app has computed both sides and declines to order them, because one
            return is guaranteed and the other is assumed. Presenting them as
            positions 5 and 6 of a list would be exactly the recommendation the
            feature says it does not make.
          */}
          <h3 className="mb-1 text-sm font-medium">Or, a genuine toss-up</h3>
          <p className="mb-3 text-xs text-mist-500">
            These are shown side by side on purpose. A guaranteed return below{' '}
            {data.hurdle}% and an assumed one above it are not comparable, and the
            app does not have an opinion on which is better for you.
          </p>
          <ol className="space-y-3">
            {tradeoffs.map((o) => (
              <OptionRow key={o.key} option={o} hurdle={data.hurdle} onAccept={onAccept} />
            ))}
          </ol>
        </div>
      )}

      {unranked.length > 0 && (
        <div className="mt-5">
          {/*
            Listed, never dropped and never defaulted to a 0% rate. A debt sorted
            to the bottom because nobody knows its rate is exactly the debt most
            likely to be the expensive one.
          */}
          <h3 className="mb-3 text-sm font-medium">Can&rsquo;t be compared yet</h3>
          <ol className="space-y-3">
            {unranked.map((o) => (
              <OptionRow key={o.key} option={o} hurdle={data.hurdle} onAccept={onAccept} />
            ))}
          </ol>
        </div>
      )}

      <p className="mt-5 text-xs text-mist-500">
        Ranked by a published rule, not a judgement: emergency fund first, then
        unclaimed employer match, then debt above {data.hurdle}% (
        {data.hurdle_basis}), then the rest. These are computed tradeoffs, not
        advice, and nothing here moves any money.
      </p>
    </section>
  )
}

/**
 * One option, with its arithmetic one click away.
 *
 * The "how this was calculated" disclosure is not decoration — it is what
 * separates this from a horoscope, and it is the same stance the projection
 * assumptions take on the Retirement page.
 */
function OptionRow({
  option,
  rank,
  hurdle,
  onAccept,
}: {
  option: AdviceOption
  rank?: number
  hurdle: string
  /**
   * Records the option as an action item. Present only on the Advisor page —
   * the Dashboard panel is a glance, and a tracked decision belongs where the
   * tray that holds it lives.
   *
   * "Accept" TRACKS, it does not execute. Nothing here moves money, and the
   * copy on the button has to keep saying so.
   */
  onAccept?: (option: AdviceOption) => void
}) {
  const [open, setOpen] = useState(false)
  return (
    <li className="rounded-xl border border-white/10 bg-white/[0.03] p-4">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0 flex-1">
          <div className="flex items-baseline gap-2">
            {rank != null && (
              <span className="text-xs font-semibold text-mist-500">{rank}.</span>
            )}
            <span className="font-medium">{option.label}</span>
          </div>
          <p className="mt-1 text-sm leading-relaxed text-mist-400">{option.detail}</p>
          {option.note && (
            <p className="mt-1 text-xs text-amber-300/80">{option.note}</p>
          )}
        </div>
        <div className="shrink-0 text-right">
          <div className="text-sm font-semibold">{formatMoney(option.amount)}/mo</div>
          <div className="text-xs text-mist-500">{valueLabel(option)}</div>
        </div>
      </div>

      <div className="mt-3 flex items-center gap-4">
        <button
          type="button"
          onClick={() => setOpen((v) => !v)}
          className="text-xs text-rune-300 hover:underline"
          aria-expanded={open}
        >
          {open ? 'Hide the arithmetic' : 'How this was calculated'}
        </button>
        {onAccept && (
          <button
            type="button"
            onClick={() => onAccept(option)}
            className="text-xs text-mist-400 hover:text-mist-200 hover:underline"
          >
            Track this
          </button>
        )}
      </div>
      {open && (
        <dl className="mt-2 space-y-1 border-t border-white/5 pt-2">
          {option.basis.map((b) => (
            <div key={b.label} className="flex justify-between gap-4 text-xs">
              <dt className="text-mist-500">{b.label}</dt>
              <dd className="text-mist-300">{b.value}</dd>
            </div>
          ))}
          <div className="flex justify-between gap-4 text-xs">
            <dt className="text-mist-500">Hurdle rate</dt>
            <dd className="text-mist-300">{hurdle}%</dd>
          </div>
        </dl>
      )}
    </li>
  )
}

/**
 * What the option's `value` MEANS, in words.
 *
 * `months_earlier` is a COUNT, not money — running it through formatMoney would
 * print "$3.00" for "three months sooner", which is the kind of small lie that
 * makes a reader stop believing the rest of the panel.
 */
function valueLabel(o: AdviceOption): string {
  if (o.unbounded) return 'never clears at today’s payment'
  switch (o.value_kind) {
    case 'interest_avoided':
      return `${formatMoney(o.value)} interest avoided`
    case 'match_captured':
      return `${formatMoney(o.value)} match captured`
    case 'months_earlier': {
      const n = Number(o.value)
      if (!Number.isFinite(n) || n <= 0) return 'brings the date forward'
      return `${n} month${n === 1 ? '' : 's'} earlier`
    }
    case 'gap_closed':
      return `${formatMoney(o.value)} to go`
    case 'headroom':
      return `${formatMoney(o.value)} of room`
    case 'projected_growth':
      return `${formatMoney(o.value)} projected growth`
    default:
      return ''
  }
}
