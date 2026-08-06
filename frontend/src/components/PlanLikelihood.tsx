import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  api,
  ApiError,
  type AllocationPlanSummary,
  type LikelihoodResult,
  type PlanComparison,
  type PlanDrift,
  type PlanTracking,
  type RankedPlan,
  type SimulatedFigures,
} from '../lib/api'
import { formatMoney } from '../lib/money'
import { CHART, SINGLE_SERIES, STATUS } from './charts/tokens'

/**
 * The likelihood layer (doc 33): the distribution behind a plan, the guardrail's
 * pick, and plan-vs-actual tracking.
 *
 * FOUR RULES THE UI HAS TO KEEP, because the engines are careful about them and
 * a careless surface would undo the care:
 *
 *  1. A SUCCESS RATE IS NEVER A PROBABILITY. "Meets your target in 94% of 1,000
 *     simulated futures" is the claim. "94% chance" is a different and
 *     unsupported one, and it is the shorter phrasing — so it is the one that
 *     creeps in.
 *  2. THE TWO HEADLINE FIGURES ARE LABELLED DIFFERENTLY AND NEITHER IS "P50".
 *     One compounds at the assumed return; the other is the median of the
 *     simulation. They disagree, the gap is explained on the card, and two
 *     surfaces disagreeing without explanation is worse than neither existing.
 *  3. NO PICK IS RENDERED AS NO PICK. When every plan breaches the drawdown
 *     floor the view says so; it does not quietly promote the least-bad one.
 *  4. AN UNTRACKED BUCKET IS NOT A ZERO. "We cannot see what you paid in" and
 *     "you paid in nothing" are opposite findings.
 */

// --------------------------------------------------------------------------
// One plan's distribution
// --------------------------------------------------------------------------

export function PlanLikelihoodPanel({ plan }: { plan: AllocationPlanSummary }) {
  const [result, setResult] = useState<LikelihoodResult | null>(null)

  const run = useMutation({
    mutationFn: () => api.planLikelihood(plan.id),
    onSuccess: setResult,
  })

  return (
    <section className="glass p-6">
      <div className="flex flex-wrap items-baseline justify-between gap-3">
        <h3 className="text-lg font-medium">Will this work?</h3>
        {!result && (
          <button
            type="button"
            className="btn-primary"
            onClick={() => run.mutate()}
            disabled={run.isPending}
          >
            {run.isPending ? 'Simulating…' : 'Run the simulation'}
          </button>
        )}
      </div>
      <p className="mt-1 text-sm text-mist-500">
        The same plan, run over a thousand sampled futures around your own assumed return
        and volatility. Not a forecast, and not market history.
      </p>

      {run.isError && (
        <p className="mt-4 text-sm text-ember-400">
          {run.error instanceof ApiError ? run.error.message : 'Could not simulate this plan.'}
        </p>
      )}

      {result && <LikelihoodBody result={result} />}
    </section>
  )
}

function LikelihoodBody({ result }: { result: LikelihoodResult }) {
  if (!result.monte_carlo_enabled || !result.simulated) {
    return (
      <div className="mt-5">
        <Headline
          label="Projected at your assumed return"
          value={formatMoney(result.projected_at_assumed_return)}
        />
        <p className="mt-3 text-xs text-mist-500">{result.basis}</p>
      </div>
    )
  }

  const sim = result.simulated
  return (
    <div className="mt-5 space-y-5">
      {sim.success_rate !== null && (
        <SuccessRateHeadline rate={sim.success_rate} runs={result.runs} target={sim.target} />
      )}

      <FanChart sim={sim} horizonYears={result.horizon_years} />

      {/* THE TWO FIGURES, SIDE BY SIDE AND LABELLED DIFFERENTLY. */}
      <div className="grid gap-4 sm:grid-cols-2">
        <Headline
          label="Projected at your assumed return"
          value={formatMoney(result.projected_at_assumed_return)}
          note="compounding at the rate you set — one number"
        />
        <Headline
          label="Median simulated outcome"
          value={formatMoney(sim.p50)}
          note={`the middle of ${result.runs.toLocaleString()} sampled futures`}
        />
      </div>
      <p className="text-xs text-mist-500">{result.gap_note}</p>

      <div className="grid gap-4 sm:grid-cols-3">
        <Headline
          label="Worse case (P10)"
          value={formatMoney(sim.p10)}
          note="1 in 10 futures land below this"
        />
        <Headline
          label="Better case (P90)"
          value={formatMoney(sim.p90)}
          note="1 in 10 land above it"
        />
        <Headline
          label="Deepest dip you'd sit through"
          value={formatPct(sim.drawdown_p5)}
          note={`5th-percentile peak-to-trough across ${result.runs.toLocaleString()} futures`}
        />
      </div>

      <p className="text-xs text-mist-500">{result.basis}</p>
    </div>
  )
}

/**
 * The largest number on the card — and the one most likely to be misread, so
 * the phrasing is fixed. "Of N simulated futures" is part of the claim, not a
 * footnote: it is what makes the figure a statement about a model rather than
 * about the world.
 */
function SuccessRateHeadline({
  rate,
  runs,
  target,
}: {
  rate: string
  runs: number
  target?: string
}) {
  const pct = Math.round(Number(rate) * 100)
  const tone = pct >= 85 ? 'text-rune-300' : pct >= 60 ? 'text-mist-100' : 'text-ember-400'
  return (
    <div className="rounded-xl border border-white/10 bg-white/[0.03] p-5">
      <div className={`text-4xl font-medium ${tone}`}>{pct}%</div>
      <p className="mt-1 text-sm text-mist-300">
        of {runs.toLocaleString()} simulated futures {target ? 'reach' : 'meet'} your target
        {target ? ` of ${formatMoney(target)}` : ''}.
      </p>
      <p className="mt-1 text-xs text-mist-500">
        That is a count over modelled sequences — not a {pct}% chance of it happening.
      </p>
    </div>
  )
}

/**
 * The fan chart: the P10–P90 band with the median simulated outcome, and the
 * target line in ember.
 *
 * Deliberately a band and not a single line. A single projected curve is the
 * thing this whole doc exists to stop being the only picture on the page.
 */
function FanChart({ sim, horizonYears }: { sim: SimulatedFigures; horizonYears: number }) {
  const p10 = Number(sim.p10)
  const p50 = Number(sim.p50)
  const p90 = Number(sim.p90)
  const target = sim.target ? Number(sim.target) : null

  const top = Math.max(p90, target ?? 0) * 1.05
  if (!Number.isFinite(top) || top <= 0) return null

  const W = 620
  const H = 180
  const pad = { l: 8, r: 8, t: 12, b: 22 }
  const plotW = W - pad.l - pad.r
  const plotH = H - pad.t - pad.b

  const y = (v: number) => pad.t + plotH - (v / top) * plotH
  // The band widens from a point (today, one known balance) to the horizon
  // spread. A quadratic opening is the honest shape: uncertainty compounds.
  const curve = (end: number) => {
    const pts: string[] = []
    const steps = 24
    for (let i = 0; i <= steps; i++) {
      const t = i / steps
      const x = pad.l + t * plotW
      const v = p50 + (end - p50) * t * t
      pts.push(`${x.toFixed(1)},${y(v).toFixed(1)}`)
    }
    return pts
  }

  const upper = curve(p90)
  const lower = curve(p10)
  const band = `M ${upper.join(' L ')} L ${lower.slice().reverse().join(' L ')} Z`

  return (
    <figure>
      <svg
        viewBox={`0 0 ${W} ${H}`}
        className="w-full"
        role="img"
        aria-label={`Range of simulated outcomes over ${horizonYears} years: 10th percentile ${sim.p10}, median ${sim.p50}, 90th percentile ${sim.p90}`}
      >
        <path d={band} fill={SINGLE_SERIES} opacity={0.18} />
        <line
          x1={pad.l}
          x2={W - pad.r}
          y1={y(p50)}
          y2={y(p50)}
          stroke={SINGLE_SERIES}
          strokeWidth={2}
        />
        {target !== null && (
          <line
            x1={pad.l}
            x2={W - pad.r}
            y1={y(target)}
            y2={y(target)}
            stroke={STATUS.serious}
            strokeWidth={1.5}
            strokeDasharray="5 4"
          />
        )}
        <line
          x1={pad.l}
          x2={W - pad.r}
          y1={pad.t + plotH}
          y2={pad.t + plotH}
          stroke={CHART.axis}
        />
        <text x={pad.l} y={H - 6} fontSize={11} fill={CHART.textMuted}>
          today
        </text>
        <text x={W - pad.r} y={H - 6} fontSize={11} fill={CHART.textMuted} textAnchor="end">
          {horizonYears} years
        </text>
      </svg>
      <figcaption className="mt-1 text-xs text-mist-500">
        The shaded band is the 10th to 90th percentile of the simulated outcomes; the solid
        line is the median simulated outcome
        {target !== null && <>, and the dashed line is your target</>}. The band opens because
        uncertainty compounds — it is a range, not a prediction.
      </figcaption>
    </figure>
  )
}

// --------------------------------------------------------------------------
// Plan comparison — the guardrail
// --------------------------------------------------------------------------

export function PlanComparisonPanel({ plans }: { plans: AllocationPlanSummary[] }) {
  const [selected, setSelected] = useState<string[]>([])
  const [comparison, setComparison] = useState<PlanComparison | null>(null)

  const compare = useMutation({
    mutationFn: () => api.comparePlans(selected),
    onSuccess: setComparison,
  })

  const toggle = (id: string) =>
    setSelected((prev) => {
      setComparison(null)
      if (prev.includes(id)) return prev.filter((p) => p !== id)
      if (prev.length >= 4) return prev
      return [...prev, id]
    })

  if (plans.length < 2) return null

  return (
    <section className="glass p-6">
      <h3 className="text-lg font-medium">Compare plans</h3>
      <p className="mt-1 text-sm text-mist-500">
        Pick two to four saved plans. Every one is simulated at the same run count, and a
        documented rule names the top pick from the computed figures — not from an opinion.
      </p>

      <div className="mt-4 flex flex-wrap gap-2">
        {plans.map((p) => {
          const on = selected.includes(p.id)
          return (
            <button
              key={p.id}
              type="button"
              onClick={() => toggle(p.id)}
              aria-pressed={on}
              className={
                on
                  ? 'rounded-full bg-arcane-500/20 px-3 py-1 text-xs text-arcane-200 ring-1 ring-arcane-400/40'
                  : 'rounded-full px-3 py-1 text-xs text-mist-400 hover:bg-white/5'
              }
            >
              {p.name}
            </button>
          )
        })}
      </div>

      <div className="mt-4 flex flex-wrap items-center gap-3">
        <button
          type="button"
          className="btn-primary"
          disabled={selected.length < 2 || compare.isPending}
          onClick={() => compare.mutate()}
        >
          {compare.isPending ? 'Simulating…' : `Compare ${selected.length || ''}`}
        </button>
        {compare.isError && (
          <span className="text-sm text-ember-400">
            {compare.error instanceof ApiError ? compare.error.message : 'Could not compare.'}
          </span>
        )}
      </div>

      {comparison && <ComparisonResult comparison={comparison} />}
    </section>
  )
}

function ComparisonResult({ comparison }: { comparison: PlanComparison }) {
  const { ranking } = comparison
  const [showRule, setShowRule] = useState(false)

  return (
    <div className="mt-6 space-y-4">
      {/* NO PICK IS AN ANSWER. It is rendered as one. */}
      {ranking.top_pick === null ? (
        <div className="rounded-xl border border-ember-500/30 bg-ember-500/5 p-4">
          <div className="text-sm font-medium text-ember-300">No pick</div>
          <p className="mt-1 text-sm text-mist-300">{ranking.no_pick_reason}</p>
        </div>
      ) : (
        <div className="rounded-xl border border-rune-400/25 bg-rune-400/5 p-4">
          <div className="text-sm font-medium text-rune-200">The rule&rsquo;s pick</div>
          <p className="mt-1 text-sm text-mist-200">{ranking.explanation}</p>
        </div>
      )}

      {ranking.no_plan_meets_every_goal && ranking.top_pick !== null && (
        <p className="text-sm text-ember-400">
          No plan here meets every stated goal at the median simulated outcome.
        </p>
      )}
      {!ranking.floor_applied && (
        <p className="text-xs text-mist-500">
          You have no drawdown floor on file, so the risk filter was skipped entirely. Set one
          under Assumptions to have plans excluded for a dip you could not sit through.
        </p>
      )}

      <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
        {ranking.plans.map((p) => (
          <PlanCard key={p.plan_id} plan={p} runs={ranking.runs} floorPct={ranking.floor_pct} />
        ))}
      </div>

      <button
        type="button"
        className="text-xs text-arcane-300 hover:underline"
        onClick={() => setShowRule((v) => !v)}
      >
        {showRule ? 'Hide the rule' : 'Show the rule that produced this'}
      </button>
      {showRule && (
        <pre className="overflow-x-auto whitespace-pre-wrap rounded-xl border border-white/10 bg-white/[0.03] p-4 text-xs text-mist-400">
          {ranking.rule}
        </pre>
      )}
    </div>
  )
}

function PlanCard({
  plan,
  runs,
  floorPct,
}: {
  plan: RankedPlan
  runs: number
  floorPct?: string
}) {
  const border = plan.top_pick
    ? 'border-rune-400/40 bg-rune-400/5'
    : plan.excluded
      ? 'border-white/5 bg-white/[0.01] opacity-70'
      : 'border-white/10 bg-white/[0.03]'

  return (
    <div className={`rounded-xl border p-4 ${border}`}>
      <div className="flex items-baseline justify-between gap-2">
        <span className="text-mist-200">{plan.name}</span>
        {plan.top_pick && <span className="text-xs text-rune-300">top pick</span>}
        {plan.excluded && <span className="text-xs text-mist-500">{plan.excluded_by}</span>}
      </div>

      {plan.excluded ? (
        <p className="mt-2 text-xs text-ember-400">Dropped: {plan.reason}</p>
      ) : (
        <p className="mt-2 text-xs text-mist-500">
          {plan.meets_all_goals
            ? 'Meets every stated goal at the median outcome'
            : `Short on: ${plan.missed_goals.join(', ')}`}
        </p>
      )}

      <dl className="mt-3 space-y-1.5 text-xs">
        <Row
          label="Success rate"
          value={
            plan.success_rate === null
              ? 'no target set'
              : `${Math.round(Number(plan.success_rate) * 100)}% of ${runs.toLocaleString()}`
          }
        />
        <Row label="Median outcome" value={formatMoney(plan.p50)} />
        <Row label="Spread (σ)" value={formatMoney(plan.sigma)} />
        <Row
          label="P5 drawdown"
          value={formatPct(plan.drawdown_p5) + (floorPct ? ` of ${floorPct}% floor` : '')}
        />
      </dl>
    </div>
  )
}

// --------------------------------------------------------------------------
// Plan tracker
// --------------------------------------------------------------------------

export function PlanTrackerPanel({ planId }: { planId: string }) {
  const queryClient = useQueryClient()
  const tracking = useQuery<PlanTracking>({
    queryKey: ['plan-tracking', planId],
    queryFn: () => api.planTracking(planId),
  })

  const record = useMutation({
    mutationFn: () => api.recordPlanTracking(planId),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['plan-tracking', planId] }),
  })

  if (tracking.isPending) {
    return <div className="glass h-32 animate-pulse p-6" aria-hidden />
  }
  if (tracking.isError || !tracking.data) {
    return (
      <section className="glass p-6">
        <p className="text-sm text-ember-400">Could not load this plan&rsquo;s tracking.</p>
      </section>
    )
  }

  const drift = tracking.data.drift
  return (
    <section className="glass p-6">
      <div className="flex flex-wrap items-baseline justify-between gap-3">
        <h3 className="text-lg font-medium">Are you doing it?</h3>
        <button
          type="button"
          className="text-xs text-arcane-300 hover:underline"
          onClick={() => record.mutate()}
          disabled={record.isPending}
        >
          {record.isPending ? 'Saving…' : 'Pin today’s snapshot'}
        </button>
      </div>

      <p
        className={`mt-3 text-sm ${
          drift.months === 0 ? 'text-mist-400' : drift.on_track ? 'text-rune-300' : 'text-ember-400'
        }`}
      >
        {drift.summary}
      </p>

      {drift.months > 0 && <DriftSparkline drift={drift} />}

      {drift.buckets.length > 0 && (
        <ul className="mt-4 space-y-2 text-sm">
          {drift.buckets.map((b) => (
            <li
              key={b.account_id}
              className="rounded-xl border border-white/10 bg-white/[0.03] p-3"
            >
              <div className="flex flex-wrap items-baseline justify-between gap-2">
                <span className="text-mist-200">{b.name}</span>
                {b.tracked ? (
                  <span className={Number(b.drift) < 0 ? 'text-ember-400' : 'text-rune-300'}>
                    {Number(b.drift) < 0 ? '' : '+'}
                    {formatMoney(b.drift)}
                  </span>
                ) : (
                  <span className="text-xs text-mist-500">not tracked</span>
                )}
              </div>
              <p className="mt-1 text-xs text-mist-500">
                {b.tracked
                  ? `expected ${formatMoney(b.expected_to_date)}, actual ${formatMoney(b.actual_to_date)}`
                  : b.note}
              </p>
            </li>
          ))}
        </ul>
      )}

      <p className="mt-4 text-xs text-mist-500">{drift.basis}</p>
    </section>
  )
}

/**
 * The drift sparkline: expected against actual, as two bars rather than a line.
 *
 * Two bars because there are exactly two numbers and a two-point line would
 * imply a trend the data does not carry. The snapshot history exists for a real
 * line once there is more than one point in it.
 */
function DriftSparkline({ drift }: { drift: PlanDrift }) {
  const expected = Number(drift.expected_to_date)
  const actual = Number(drift.actual_to_date)
  const top = Math.max(expected, actual, 1)

  const bar = (value: number, color: string, label: string) => (
    <div className="flex items-center gap-3">
      <span className="w-20 shrink-0 text-xs text-mist-500">{label}</span>
      <div className="h-3 flex-1 overflow-hidden rounded-full bg-white/5">
        <div
          className="h-full rounded-full"
          style={{ width: `${Math.max(0, (value / top) * 100)}%`, background: color }}
        />
      </div>
      <span className="w-24 shrink-0 text-right text-xs text-mist-300">
        {formatMoney(String(value))}
      </span>
    </div>
  )

  return (
    <div className="mt-4 space-y-2">
      {bar(expected, CHART.axis, 'the plan')}
      {bar(actual, drift.on_track ? STATUS.good : STATUS.serious, 'actually')}
      {drift.untracked.length > 0 && (
        <p className="text-xs text-mist-500">
          {drift.untracked.join(', ')} {drift.untracked.length === 1 ? 'has' : 'have'} no
          contribution trail and {drift.untracked.length === 1 ? 'is' : 'are'} left out of both
          figures — that is unknown, not zero.
        </p>
      )}
    </div>
  )
}

// --------------------------------------------------------------------------
// Small helpers
// --------------------------------------------------------------------------

function Headline({ label, value, note }: { label: string; value: string; note?: string }) {
  return (
    <div className="rounded-xl border border-white/10 bg-white/[0.03] p-4">
      <div className="text-xs text-mist-500">{label}</div>
      <div className="mt-1 text-xl font-medium text-mist-100">{value}</div>
      {note && <div className="mt-1 text-xs text-mist-500">{note}</div>}
    </div>
  )
}

function Row({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-baseline justify-between gap-2">
      <dt className="text-mist-500">{label}</dt>
      <dd className="text-mist-200">{value}</dd>
    </div>
  )
}

/** A fraction rendered as a percent. "0.19" is a 19% drawdown. */
function formatPct(fraction: string): string {
  const n = Number(fraction)
  if (!Number.isFinite(n)) return '—'
  return `${(n * 100).toFixed(1)}%`
}
