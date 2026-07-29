import { useEffect, useState, type FormEvent } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  api,
  type AssumptionsInput,
  type ContributionAccount,
  type RetirementAssumptions,
  type RetirementPoint,
  type RetirementResponse,
} from '../lib/api'
import { formatMoney } from '../lib/money'
import { CHART, SERIES, STATUS } from '../components/charts/tokens'

/**
 * Retirement projections.
 *
 * The rule this page is built around: **the assumptions are the honest part**,
 * so they are always visible and always editable — never behind a disclosure
 * triangle. A retirement number without the inputs that produced it is worse
 * than no number at all, because somebody will plan around it.
 *
 * Everything is in today's dollars: the return rate is real (already net of
 * inflation), which is why nothing here is discounted afterwards.
 */

const TREATMENT_LABELS: Record<string, string> = {
  taxable: 'Taxable brokerage',
  trad_401k: 'Traditional 401(k)',
  roth_401k: 'Roth 401(k)',
  trad_ira: 'Traditional IRA',
  roth_ira: 'Roth IRA',
  '529': '529 college',
  hsa: 'HSA',
  trust: 'Trust',
  other: 'Other',
}

/** Names the group an IRS limit is shared across, for the headroom copy. */
const LIMIT_GROUPS: Record<string, string> = {
  trad_401k: 'all your 401(k)s',
  roth_401k: 'all your 401(k)s',
  trad_ira: 'your IRAs',
  roth_ira: 'your IRAs',
  hsa: 'your HSA',
}

/** Renders a fraction from the API ("0.05") as a percentage ("5%"). */
function percent(value: string | null | undefined, digits = 1): string {
  if (value === null || value === undefined || value === '') return '—'
  const n = Number(value)
  if (!Number.isFinite(n)) return '—'
  return `${(n * 100).toFixed(digits)}%`
}

/** "2034-06" -> "June 2034". */
function monthLabel(month: string): string {
  const [year, m] = month.split('-').map(Number)
  return new Date(year, m - 1, 1).toLocaleDateString('en-US', {
    month: 'long',
    year: 'numeric',
  })
}

export function Retirement() {
  const projection = useQuery({
    queryKey: ['retirement'],
    queryFn: () => api.retirementProjection(),
  })
  const contributions = useQuery({
    queryKey: ['retirement', 'contributions'],
    queryFn: api.retirementContributions,
  })

  const data = projection.data

  return (
    <div className="space-y-8">
      <div>
        <h1 className="text-2xl font-semibold">Retirement</h1>
        <p className="mt-1 text-mist-300">
          When the money is enough, and whether it lasts. Every account is projected
          on its own terms — a 401(k), a Roth and a 529 do not behave alike.
        </p>
      </div>

      {data && <AssumptionsPanel assumptions={data.assumptions} />}

      {data && <Headline data={data} />}

      {data && data.projection.points.length > 0 && (
        <section className="glass p-6">
          <div className="mb-1 flex flex-wrap items-baseline justify-between gap-2">
            <h2 className="text-lg font-medium">Projected balance</h2>
            <p className="text-xs text-mist-500">{data.assumptions.basis}</p>
          </div>
          <p className="mb-5 text-sm text-mist-300">
            Split by where the money comes from, because a thirty-year curve at{' '}
            {percent(data.assumptions.real_return_rate)} is mostly assumption by the end.
          </p>
          <ProjectionChart
            points={data.projection.points}
            fiMonth={data.projection.fi_month}
          />
        </section>
      )}

      {data && <Gaps data={data} />}

      {data?.required_savings && (
        <SavingsRate solve={data.required_savings} assumptions={data.assumptions} />
      )}

      {data?.monte_carlo && <MonteCarlo result={data.monte_carlo} />}

      {contributions.data && (
        <Contributions
          accounts={contributions.data.accounts}
          limitsNote={contributions.data.limits_note}
          limitsConfigured={contributions.data.limits_configured}
        />
      )}

      {data && <Omissions items={data.omissions} basis={data.basis} />}

      {projection.isError && (
        <p role="alert" className="rounded-xl border border-ember-400/30 bg-ember-400/10 px-4 py-2.5 text-sm text-ember-400">
          {projection.error.message}
        </p>
      )}
    </div>
  )
}

// --------------------------------------------------------------------------
// Assumptions
// --------------------------------------------------------------------------

/**
 * The inputs, always on screen. Percentages are typed as percentages (5, not
 * 0.05) because that is how people say them; the ×100 happens exactly once, at
 * this boundary.
 */
function AssumptionsPanel({ assumptions }: { assumptions: RetirementAssumptions }) {
  const qc = useQueryClient()
  const [form, setForm] = useState(() => toForm(assumptions))

  // Re-seed when the server's copy changes (another tab, a fresh load).
  useEffect(() => setForm(toForm(assumptions)), [assumptions])

  const save = useMutation({
    mutationFn: (input: AssumptionsInput) => api.saveRetirementAssumptions(input),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['retirement'] }),
  })

  function onSubmit(e: FormEvent) {
    e.preventDefault()
    save.mutate(fromForm(form))
  }

  const set = (key: keyof typeof form) => (value: string) =>
    setForm((f) => ({ ...f, [key]: value }))

  return (
    <section className="glass p-6">
      <h2 className="text-lg font-medium">Your assumptions</h2>
      <p className="mt-1 mb-5 text-sm text-mist-300">
        Every number below this panel is produced from these. They stay on screen
        because a projection you cannot audit is not one you should plan around.
      </p>

      <form onSubmit={onSubmit} className="space-y-5">
        <div className="grid gap-4 sm:grid-cols-3">
          <Field
            id="real-return"
            label="Real return"
            suffix="%"
            hint="After inflation. 5% real is roughly 8% nominal."
            value={form.realReturn}
            onChange={set('realReturn')}
          />
          <Field
            id="inflation"
            label="Inflation"
            suffix="%"
            hint="Used to label the basis, not to gross figures up."
            value={form.inflation}
            onChange={set('inflation')}
          />
          <Field
            id="withdrawal"
            label="Withdrawal rate"
            suffix="%"
            hint="The convention is 4%. It is a convention, not a law."
            value={form.withdrawal}
            onChange={set('withdrawal')}
          />
        </div>

        <div className="grid gap-4 sm:grid-cols-3">
          <Field
            id="current-age"
            label="Your age today"
            value={form.currentAge}
            onChange={set('currentAge')}
            placeholder="—"
          />
          <Field
            id="target-age"
            label="Target retirement age"
            hint="Leave blank if undecided."
            value={form.targetAge}
            onChange={set('targetAge')}
            placeholder="—"
          />
          <Field
            id="target-spending"
            label="Annual spending to support"
            prefix="$"
            hint={
              assumptions.spending_is_defaulted
                ? `Blank uses your own trailing year: ${formatMoney(assumptions.defaulted_spending)}.`
                : 'Blank falls back to your trailing-year spend.'
            }
            value={form.targetSpending}
            onChange={set('targetSpending')}
            placeholder={Number(assumptions.defaulted_spending).toFixed(0)}
          />
        </div>

        <div className="grid gap-4 sm:grid-cols-3">
          <Field
            id="ss-income"
            label="Social Security / pension"
            prefix="$"
            hint="Per year, in today's dollars."
            value={form.ssIncome}
            onChange={set('ssIncome')}
            placeholder="—"
          />
          <Field
            id="ss-age"
            label="Starting at age"
            hint="Counted from this age, not a month sooner."
            value={form.ssAge}
            onChange={set('ssAge')}
            placeholder="—"
          />
          <div className="flex items-end">
            <button type="submit" className="btn-primary mb-0.5" disabled={save.isPending}>
              {save.isPending ? 'Saving…' : 'Save assumptions'}
            </button>
          </div>
        </div>
      </form>

      {save.isError && (
        <p role="alert" className="mt-4 rounded-xl border border-ember-400/30 bg-ember-400/10 px-4 py-2.5 text-sm text-ember-400">
          {save.error.message}
        </p>
      )}
    </section>
  )
}

type AssumptionsForm = ReturnType<typeof toForm>

function toForm(a: RetirementAssumptions) {
  return {
    realReturn: toPercentField(a.real_return_rate),
    inflation: toPercentField(a.inflation_rate),
    withdrawal: toPercentField(a.withdrawal_rate),
    currentAge: a.current_age === null ? '' : String(a.current_age),
    targetAge: a.target_retirement_age === null ? '' : String(a.target_retirement_age),
    ssIncome: a.annual_ss_income ?? '',
    ssAge: a.ss_start_age === null ? '' : String(a.ss_start_age),
    targetSpending: a.target_annual_spending ?? '',
  }
}

/** "0.05" -> "5". Trailing zeros trimmed so the field reads like a number. */
function toPercentField(fraction: string): string {
  const n = Number(fraction)
  if (!Number.isFinite(n)) return ''
  return String(Number((n * 100).toFixed(4)))
}

/** "5" -> "0.05", as a string: rates never pass through a JSON float. */
function fromPercentField(value: string): string {
  const n = Number(value)
  return Number.isFinite(n) ? String(n / 100) : '0'
}

function fromForm(f: AssumptionsForm): AssumptionsInput {
  const age = (v: string) => (v.trim() === '' ? null : Number(v))
  const money = (v: string) => (v.trim() === '' ? null : v.trim())
  return {
    real_return_rate: fromPercentField(f.realReturn),
    inflation_rate: fromPercentField(f.inflation),
    withdrawal_rate: fromPercentField(f.withdrawal),
    current_age: age(f.currentAge),
    target_retirement_age: age(f.targetAge),
    ss_start_age: age(f.ssAge),
    annual_ss_income: money(f.ssIncome),
    target_annual_spending: money(f.targetSpending),
  }
}

function Field({
  id, label, value, onChange, hint, prefix, suffix, placeholder,
}: {
  id: string
  label: string
  value: string
  onChange: (v: string) => void
  hint?: string
  prefix?: string
  suffix?: string
  placeholder?: string
}) {
  return (
    <div>
      <label className="label" htmlFor={id}>{label}</label>
      <div className="flex items-center gap-1.5">
        {prefix && <span className="text-sm text-mist-500">{prefix}</span>}
        <input
          id={id}
          className="field"
          inputMode="decimal"
          value={value}
          placeholder={placeholder}
          onChange={(e) => onChange(e.target.value)}
        />
        {suffix && <span className="text-sm text-mist-500">{suffix}</span>}
      </div>
      {hint && <p className="mt-1.5 text-xs text-mist-500">{hint}</p>}
    </div>
  )
}

// --------------------------------------------------------------------------
// Headline answers
// --------------------------------------------------------------------------

function Headline({ data }: { data: RetirementResponse }) {
  const p = data.projection
  const target = data.assumptions.target_annual_spending ?? data.assumptions.defaulted_spending

  return (
    <div className="grid gap-4 sm:grid-cols-3">
      <div className="glass p-5">
        <p className="text-sm text-mist-300">Financial independence</p>
        {p.already_fi ? (
          <>
            <p className="tabular mt-2 text-4xl font-semibold" style={{ color: STATUS.good }}>
              Now
            </p>
            <p className="mt-2 text-xs text-mist-500">
              What you hold already supports {formatMoney(target)} a year at{' '}
              {percent(data.assumptions.withdrawal_rate)}.
            </p>
          </>
        ) : p.fi_age !== null ? (
          <>
            <p className="tabular mt-2 text-4xl font-semibold" style={{ color: STATUS.good }}>
              Age {p.fi_age}
            </p>
            <p className="mt-2 text-xs text-mist-500">
              {p.fi_month && `${monthLabel(p.fi_month)} — `}the first month the portfolio
              supports {formatMoney(target)} a year.
            </p>
          </>
        ) : (
          <>
            <p className="mt-2 text-2xl font-semibold" style={{ color: CHART.textSecondary }}>
              Not within {Math.round(p.points.length / 12)} years
            </p>
            <p className="mt-2 text-xs text-mist-500">
              Not reached inside the projected horizon. That is the answer — the curve is
              not extrapolated past where it was computed.
            </p>
          </>
        )}
      </div>

      <div className="glass p-5">
        <p className="text-sm text-mist-300">
          Nest egg
          {data.assumptions.target_retirement_age !== null &&
            ` at ${data.assumptions.target_retirement_age}`}
        </p>
        <p className="tabular mt-2 text-3xl font-semibold" style={{ color: '#f2d492' }}>
          {p.nest_egg_at_target ? formatMoney(p.nest_egg_at_target) : '—'}
        </p>
        <p className="mt-2 text-xs text-mist-500">
          {p.nest_egg_at_target
            ? 'Excludes 529 balances — college money is not retirement money.'
            : 'Set a target retirement age to see this.'}
        </p>
      </div>

      <div className="glass p-5">
        <p className="text-sm text-mist-300">Supported spending</p>
        <p className="tabular mt-2 text-3xl font-semibold" style={{ color: '#f2d492' }}>
          {p.supported_at_target ? formatMoney(p.supported_at_target) : '—'}
        </p>
        <p className="mt-2 text-xs text-mist-500">
          Per year at {percent(data.assumptions.withdrawal_rate)}
          {data.assumptions.annual_ss_income &&
            data.assumptions.ss_start_age !== null &&
            `, plus Social Security from ${data.assumptions.ss_start_age}`}
          .
        </p>
      </div>
    </div>
  )
}

// --------------------------------------------------------------------------
// The chart
// --------------------------------------------------------------------------

const CHART_SERIES = [
  { key: 'contributed', label: 'You contributed', color: SERIES.income },
  { key: 'employer', label: 'Employer match', color: SERIES.spending },
  { key: 'growth', label: 'Assumed growth', color: SERIES.leftover },
] as const

/**
 * Stacked area of where the balance came from: opening balance sits below,
 * then contributions, employer match, and assumed growth.
 *
 * One y-axis, three fixed-order categorical colours from the app's validated
 * chart palette, a 2px surface gap between adjacent fills, and a legend —
 * identity is never carried by colour alone.
 */
function ProjectionChart({
  points, fiMonth,
}: {
  points: RetirementPoint[]
  fiMonth: string | null
}) {
  const [hover, setHover] = useState<number | null>(null)

  if (points.length < 2) {
    return (
      <p className="py-10 text-center text-sm" style={{ color: CHART.textMuted }}>
        Not enough projected months to draw a curve.
      </p>
    )
  }

  const W = 760
  const H = 260
  const PAD = { top: 12, right: 16, bottom: 28, left: 78 }
  const plotW = W - PAD.left - PAD.right
  const plotH = H - PAD.top - PAD.bottom

  // The opening balance is whatever the first point holds that is not yet
  // contribution or growth. Derived rather than assumed so the bands always
  // sum to the plotted total.
  const opening = Math.max(
    0,
    Number(points[0].retirement) -
      Number(points[0].contributed) -
      Number(points[0].employer_contributed) -
      Number(points[0].growth),
  )

  const bands = points.map((p) => {
    const c = Number(p.contributed)
    const e = Number(p.employer_contributed)
    const g = Number(p.growth)
    return { opening, contributed: c, employer: e, growth: g, total: opening + c + e + g }
  })

  const max = Math.max(...bands.map((b) => b.total)) || 1
  const x = (i: number) => PAD.left + (i / (points.length - 1)) * plotW
  const y = (v: number) => PAD.top + plotH - (v / max) * plotH

  /** An area between two cumulative levels, as a closed path. */
  type Band = (typeof bands)[number]
  function band(lower: (b: Band) => number, upper: (b: Band) => number) {
    const top = bands.map((b, i) => `${i === 0 ? 'M' : 'L'} ${x(i)} ${y(upper(b))}`).join(' ')
    // Traced back along the lower edge to close the shape.
    const bottom = bands
      .map((_, i) => {
        const j = bands.length - 1 - i
        return `L ${x(j)} ${y(lower(bands[j]))}`
      })
      .join(' ')
    return `${top} ${bottom} Z`
  }

  const fiIndex = fiMonth ? points.findIndex((p) => p.month === fiMonth) : -1
  const hovered = hover !== null ? points[hover] : null

  // Three gridlines plus the baseline: enough to read a value against, few
  // enough to stay recessive.
  const ticks = [0.25, 0.5, 0.75, 1].map((f) => max * f)

  return (
    <div>
      <div className="overflow-x-auto">
        <svg
          viewBox={`0 0 ${W} ${H}`}
          className="w-full min-w-[560px]"
          role="img"
          aria-label="Projected retirement balance, split into your contributions, employer match and assumed growth"
          onMouseLeave={() => setHover(null)}
        >
          {ticks.map((t) => (
            <g key={t}>
              <line x1={PAD.left} x2={W - PAD.right} y1={y(t)} y2={y(t)}
                stroke={CHART.grid} strokeWidth={1} />
              <text x={PAD.left - 10} y={y(t) + 4} textAnchor="end" fontSize="11"
                fill={CHART.textMuted}>
                {compactMoney(t)}
              </text>
            </g>
          ))}

          {/* Opening balance, in recessive furniture rather than a fourth
              categorical hue — it is context, not a series anyone compares. */}
          <path d={band(() => 0, (b) => b.opening)} fill="rgba(255,255,255,0.06)" />
          <path
            d={band((b) => b.opening, (b) => b.opening + b.contributed)}
            fill={SERIES.income}
          />
          <path
            d={band(
              (b) => b.opening + b.contributed,
              (b) => b.opening + b.contributed + b.employer,
            )}
            fill={SERIES.spending}
            stroke={CHART.surface}
            strokeWidth={2}
          />
          <path
            d={band((b) => b.opening + b.contributed + b.employer, (b) => b.total)}
            fill={SERIES.leftover}
            stroke={CHART.surface}
            strokeWidth={2}
          />

          {/* The FI marker: the single most important point on the chart, so it
              is labelled directly rather than left to the legend. */}
          {fiIndex >= 0 && (
            <g>
              <line x1={x(fiIndex)} x2={x(fiIndex)} y1={PAD.top} y2={PAD.top + plotH}
                stroke={STATUS.good} strokeWidth={2} strokeDasharray="4 3" />
              {/* Flips to the left of the line near the right edge, so the label
                  never runs off the plot when FI lands late in the horizon. */}
              <text
                x={x(fiIndex) + (x(fiIndex) > W - 100 ? -6 : 6)}
                y={PAD.top + 12}
                textAnchor={x(fiIndex) > W - 100 ? 'end' : 'start'}
                fontSize="11"
                fill={STATUS.good}
              >
                FI · age {points[fiIndex].age}
              </text>
            </g>
          )}

          {/* Crosshair. The hit target is a full-height column per point, which
              is far easier to hit than the curve itself. */}
          {hovered && hover !== null && (
            <line x1={x(hover)} x2={x(hover)} y1={PAD.top} y2={PAD.top + plotH}
              stroke={CHART.axis} strokeWidth={1} />
          )}
          {points.map((p, i) => (
            <rect
              key={p.month}
              x={x(i) - plotW / points.length / 2}
              y={PAD.top}
              width={plotW / points.length}
              height={plotH}
              fill="transparent"
              onMouseEnter={() => setHover(i)}
            />
          ))}

          <text x={PAD.left} y={H - 6} fontSize="11" fill={CHART.textMuted}>
            {monthLabel(points[0].month)}
          </text>
          <text x={W - PAD.right} y={H - 6} textAnchor="end" fontSize="11" fill={CHART.textMuted}>
            {monthLabel(points[points.length - 1].month)}
          </text>
        </svg>
      </div>

      <div className="mt-3 flex flex-wrap items-center gap-x-5 gap-y-2 text-xs text-mist-300">
        {CHART_SERIES.map((s) => (
          <span key={s.key} className="flex items-center gap-2">
            <span className="inline-block h-2.5 w-2.5 rounded-sm" style={{ background: s.color }} />
            {s.label}
          </span>
        ))}
        <span className="flex items-center gap-2">
          <span className="inline-block h-2.5 w-2.5 rounded-sm bg-white/10" />
          Starting balance
        </span>
      </div>

      {hovered && (
        <div className="mt-3 rounded-xl border border-white/5 bg-white/[0.03] px-4 py-3 text-sm">
          <p className="font-medium">
            {monthLabel(hovered.month)} · age {hovered.age}
          </p>
          <div className="mt-2 grid gap-x-6 gap-y-1 sm:grid-cols-2">
            <Reading label="Balance" value={hovered.retirement} />
            <Reading label="Supports per year" value={hovered.supported_spending} />
            <Reading label="You contributed" value={hovered.contributed} />
            <Reading label="Employer match" value={hovered.employer_contributed} />
            <Reading label="Assumed growth" value={hovered.growth} />
            {Number(hovered.education) > 0 && (
              <Reading label="529 (not retirement)" value={hovered.education} />
            )}
          </div>
        </div>
      )}
    </div>
  )
}

function Reading({ label, value }: { label: string; value: string }) {
  return (
    <div className="flex items-baseline justify-between gap-4">
      <span className="text-mist-300">{label}</span>
      <span className="tabular">{formatMoney(value)}</span>
    </div>
  )
}

/** Axis labels only: "$1.2M", "$450k". Never used for a figure being quoted. */
function compactMoney(n: number): string {
  if (n >= 1_000_000) return `$${(n / 1_000_000).toFixed(1)}M`
  if (n >= 1_000) return `$${Math.round(n / 1_000)}k`
  return `$${Math.round(n)}`
}

// --------------------------------------------------------------------------
// Gaps: what this projection is not counting
// --------------------------------------------------------------------------

/**
 * Untagged accounts and bound contribution limits. Both are things the engine
 * did to the numbers, and both would otherwise be invisible — an excluded
 * account silently omitted produces a confidently wrong figure.
 */
function Gaps({ data }: { data: RetirementResponse }) {
  const p = data.projection
  const hasExclusions = p.excluded_accounts.length > 0
  const hasCaps = p.cap_notes.length > 0

  if (!hasExclusions && !hasCaps && p.limits_configured) return null

  return (
    <section className="glass space-y-4 p-6">
      <h2 className="text-lg font-medium">What this is not counting</h2>

      {hasExclusions && (
        <div className="rounded-xl border border-amber-400/20 bg-amber-400/5 px-4 py-3">
          <p className="text-sm" style={{ color: STATUS.warning }}>
            {p.excluded_accounts.length}{' '}
            {p.excluded_accounts.length === 1 ? 'account is' : 'accounts are'} untagged and
            excluded — {formatMoney(p.excluded_value)} is missing from every figure above.
          </p>
          <p className="mt-1.5 text-xs text-mist-300">
            {p.excluded_accounts.join(', ')}. An untagged account has an unknown
            contribution limit and an unknown tax treatment, so guessing one would change
            every number on this page. Tag them on the{' '}
            <a className="underline hover:text-ember-400" href="/investments">
              Investments page
            </a>
            .
          </p>
        </div>
      )}

      {!p.limits_configured && (
        <p className="text-sm text-mist-300">
          IRS limits for {p.limits_year} are not configured in this build, so contributions
          are projected <strong>uncapped</strong>. Treat the headroom figures as
          unavailable rather than unlimited.
        </p>
      )}

      {hasCaps && (
        <div className="text-sm text-mist-300">
          <p>Contributions held at their {p.limits_year} IRS limit:</p>
          <ul className="mt-2 space-y-1">
            {p.cap_notes.map((n) => (
              <li key={n.group} className="tabular text-xs">
                {n.group === '401k' ? '401(k)' : n.group.toUpperCase()}: planned{' '}
                {formatMoney(n.planned_annual)}/yr, projected at{' '}
                {formatMoney(n.allowed_annual)}/yr.
              </li>
            ))}
          </ul>
        </div>
      )}
    </section>
  )
}

// --------------------------------------------------------------------------
// Required savings rate
// --------------------------------------------------------------------------

function SavingsRate({
  solve, assumptions,
}: {
  solve: RetirementResponse['required_savings']
  assumptions: RetirementAssumptions
}) {
  if (!solve) return null

  return (
    <section className="glass p-6">
      <h2 className="text-lg font-medium">
        To retire at {assumptions.target_retirement_age ?? '—'}
      </h2>

      {!solve.reachable ? (
        <p className="mt-2 text-sm text-mist-300">{solve.note}</p>
      ) : Number(solve.required_monthly) === 0 ? (
        <p className="mt-2 text-sm" style={{ color: STATUS.good }}>
          Your current contributions already get there. Nothing extra required.
        </p>
      ) : (
        <>
          <p className="tabular mt-3 text-3xl font-semibold" style={{ color: '#f2d492' }}>
            {formatMoney(solve.required_monthly)}
            <span className="ml-2 text-base font-normal text-mist-300">a month, in total</span>
          </p>
          <p className="mt-2 text-sm text-mist-300">
            {solve.required_rate
              ? `That is ${percent(solve.required_rate)} of your trailing-year gross income.`
              : 'No income recorded in the last year, so this cannot be expressed as a rate.'}{' '}
            Spread across your tagged accounts in the same proportion you already
            contribute — which account to fill is your call, not this page's.
          </p>
        </>
      )}
    </section>
  )
}

// --------------------------------------------------------------------------
// Monte Carlo
// --------------------------------------------------------------------------

function MonteCarlo({ result }: { result: NonNullable<RetirementResponse['monte_carlo']> }) {
  const rate = Number(result.survival_rate)
  const tone = rate >= 0.9 ? STATUS.good : rate >= 0.75 ? STATUS.warning : STATUS.critical

  return (
    <section className="glass p-6">
      <h2 className="text-lg font-medium">Sequence-of-returns test</h2>
      <p className="tabular mt-3 text-3xl font-semibold" style={{ color: tone }}>
        {(rate * 100).toFixed(0)}%
        <span className="ml-2 text-base font-normal text-mist-300">
          of {result.runs} sequences lasted {result.years} years
        </span>
      </p>
      <p className="mt-3 text-sm text-mist-300">{result.basis}</p>
      <p className="mt-2 text-xs text-mist-500">
        Median ending balance {formatMoney(result.median_ending_balance)}. Seed{' '}
        {result.seed} — the same inputs always produce this same result.
      </p>
    </section>
  )
}

// --------------------------------------------------------------------------
// Per-account contributions
// --------------------------------------------------------------------------

function Contributions({
  accounts, limitsNote, limitsConfigured,
}: {
  accounts: ContributionAccount[]
  limitsNote: string
  limitsConfigured: boolean
}) {
  if (accounts.length === 0) {
    return (
      <section className="glass p-6">
        <h2 className="text-lg font-medium">Contributions</h2>
        <p className="mt-2 text-sm text-mist-300">
          No investment accounts linked yet. Link one and its contribution plan appears
          here.
        </p>
      </section>
    )
  }

  return (
    <section className="glass p-6">
      <h2 className="text-lg font-medium">Contributions</h2>
      <p className="mt-1 mb-5 text-sm text-mist-300">
        What goes into each account every month. {limitsConfigured && limitsNote}
      </p>
      <ul className="space-y-3">
        {accounts.map((a) => (
          <ContributionRow key={a.id} account={a} limitsConfigured={limitsConfigured} />
        ))}
      </ul>
    </section>
  )
}

function ContributionRow({
  account, limitsConfigured,
}: {
  account: ContributionAccount
  limitsConfigured: boolean
}) {
  const qc = useQueryClient()
  const [open, setOpen] = useState(false)
  const [monthly, setMonthly] = useState(account.monthly_contribution)
  const [matchPct, setMatchPct] = useState(
    account.employer_match_pct ? toPercentField(account.employer_match_pct) : '',
  )
  const [salary, setSalary] = useState(account.annual_salary ?? '')
  const [matchLimit, setMatchLimit] = useState(account.employer_match_limit ?? '')
  const [benefNow, setBenefNow] = useState(
    account.beneficiary_current_age === null ? '' : String(account.beneficiary_current_age),
  )
  const [benefTarget, setBenefTarget] = useState(
    account.beneficiary_target_age === null ? '' : String(account.beneficiary_target_age),
  )

  const save = useMutation({
    mutationFn: () =>
      api.saveContribution(account.id, {
        monthly_contribution: monthly.trim() === '' ? '0' : monthly.trim(),
        employer_match_pct: matchPct.trim() === '' ? null : fromPercentField(matchPct),
        annual_salary: salary.trim() === '' ? null : salary.trim(),
        employer_match_limit: matchLimit.trim() === '' ? null : matchLimit.trim(),
        beneficiary_current_age: benefNow.trim() === '' ? null : Number(benefNow),
        beneficiary_target_age: benefTarget.trim() === '' ? null : Number(benefTarget),
      }),
    onSuccess: () => {
      setOpen(false)
      qc.invalidateQueries({ queryKey: ['retirement'] })
    },
  })

  const isEducation = account.tax_treatment === '529'
  const treatment = account.tax_treatment
    ? (TREATMENT_LABELS[account.tax_treatment] ?? account.tax_treatment)
    : null

  return (
    <li className="rounded-xl border border-white/5 bg-white/[0.02] px-4 py-3">
      <div className="flex flex-wrap items-center gap-4">
        <div className="min-w-0">
          <p className="truncate font-medium">
            {account.name}
            {account.mask && <span className="text-mist-500"> ••{account.mask}</span>}
          </p>
          <p className="text-xs text-mist-500">
            {treatment ?? (
              <span style={{ color: STATUS.warning }}>
                Untagged — excluded from the projection
              </span>
            )}
            {account.institution_name && ` · ${account.institution_name}`}
            {account.balance && ` · ${formatMoney(account.balance)}`}
          </p>
        </div>
        <div className="ml-auto flex items-center gap-5">
          <span className="tabular text-sm">
            {formatMoney(account.monthly_contribution)}
            <span className="text-mist-500">/mo</span>
          </span>
          <button
            className="text-xs text-mist-500 transition hover:text-ember-400"
            onClick={() => setOpen((v) => !v)}
          >
            {open ? 'Close' : 'Edit'}
          </button>
        </div>
      </div>

      {/* Headroom, shown only when a limit genuinely applies AND the running
          year's limits are known. A cap from a stale year is worse than none. */}
      {limitsConfigured && account.annual_limit && (
        <Headroom account={account} />
      )}

      {open && (
        <form
          className="mt-4 grid gap-4 border-t border-white/5 pt-4 sm:grid-cols-3"
          onSubmit={(e) => {
            e.preventDefault()
            save.mutate()
          }}
        >
          <Field
            id={`monthly-${account.id}`}
            label="Monthly contribution"
            prefix="$"
            value={monthly}
            onChange={setMonthly}
          />

          {isEducation ? (
            <>
              <Field
                id={`benef-now-${account.id}`}
                label="Beneficiary age today"
                hint="The 529 stops compounding at the target age."
                value={benefNow}
                onChange={setBenefNow}
              />
              <Field
                id={`benef-target-${account.id}`}
                label="College starts at age"
                value={benefTarget}
                onChange={setBenefTarget}
              />
            </>
          ) : (
            <>
              <Field
                id={`match-${account.id}`}
                label="Employer match"
                suffix="%"
                hint="Of salary, not of your contribution."
                value={matchPct}
                onChange={setMatchPct}
              />
              <Field
                id={`salary-${account.id}`}
                label="Annual salary"
                prefix="$"
                hint="Required for a match — a percentage needs something to apply to."
                value={salary}
                onChange={setSalary}
              />
              <Field
                id={`match-limit-${account.id}`}
                label="Annual match cap"
                prefix="$"
                hint="Optional. Blank means the plan caps nothing."
                value={matchLimit}
                onChange={setMatchLimit}
              />
            </>
          )}

          <div className="flex items-end sm:col-span-3">
            <button type="submit" className="btn-primary" disabled={save.isPending}>
              {save.isPending ? 'Saving…' : 'Save'}
            </button>
          </div>

          {save.isError && (
            <p role="alert" className="rounded-xl border border-ember-400/30 bg-ember-400/10 px-4 py-2.5 text-sm text-ember-400 sm:col-span-3">
              {save.error.message}
            </p>
          )}
        </form>
      )}
    </li>
  )
}

/**
 * Contribution headroom against the IRS limit.
 *
 * The bar is a proportion of a known whole, so it is a meter, not a chart, and
 * it carries its numbers as text — the fill is redundant encoding.
 */
function Headroom({ account }: { account: ContributionAccount }) {
  const planned = Number(account.planned_annual)
  const limit = Number(account.annual_limit)
  if (!Number.isFinite(limit) || limit <= 0) return null

  const share = Math.min(1, planned / limit)
  const over = planned > limit
  const tone = over ? STATUS.critical : share > 0.95 ? STATUS.warning : STATUS.good
  const group = account.tax_treatment ? LIMIT_GROUPS[account.tax_treatment] : null

  return (
    <div className="mt-3">
      <div className="h-1.5 w-full overflow-hidden rounded-full bg-white/5">
        <div
          className="h-full rounded-full"
          style={{ width: `${share * 100}%`, background: tone }}
        />
      </div>
      <p className="mt-1.5 text-xs text-mist-500">
        <span className="tabular">
          {formatMoney(account.planned_annual)} of {formatMoney(account.annual_limit)}
        </span>{' '}
        planned this year.
        {over && (
          <span style={{ color: STATUS.critical }}>
            {' '}
            Over the limit — the projection contributes only what is allowed.
          </span>
        )}
        {group && account.limit_shared && (
          <> That limit is shared across {group}, so this bar is the group's, not this account's alone.</>
        )}
      </p>
    </div>
  )
}

// --------------------------------------------------------------------------
// Omissions
// --------------------------------------------------------------------------

/**
 * What the model does not do, stated where the numbers are. These gaps should
 * be the reader's knowledge rather than their surprise — tax on withdrawals in
 * particular is large, real, and not modelled here.
 */
function Omissions({ items, basis }: { items: string[]; basis: string }) {
  return (
    <section className="glass p-6">
      <h2 className="text-lg font-medium">What this model leaves out</h2>
      <p className="mt-1 text-sm text-mist-300">{basis}</p>
      <ul className="mt-4 space-y-2">
        {items.map((item) => (
          <li key={item} className="flex gap-3 text-sm text-mist-300">
            <span aria-hidden className="text-mist-500">
              ·
            </span>
            {item}
          </li>
        ))}
      </ul>
    </section>
  )
}
