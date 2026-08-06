import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  api,
  ApiError,
  type AllocationBucket,
  type AllocationBucketResult,
  type AllocationPlanSummary,
  type AllocationResult,
  type AllocationRunInput,
  type AllocationSplitInput,
  type CollegeResult,
  type IdleCashReport,
} from '../lib/api'
import { formatMoney } from '../lib/money'
import {
  PlanComparisonPanel,
  PlanLikelihoodPanel,
  PlanTrackerPanel,
} from './PlanLikelihood'

/**
 * The bucket allocator: the centerpiece of the Advisor page.
 *
 * The thing a real advisor does that a calculator cannot — split a lump and a
 * monthly surplus across Roth / 529 / brokerage / debt / emergency fund, then
 * say what each one actually does — with contribution caps, eligibility, and the
 * arithmetic behind every figure available on demand.
 *
 * Three rules the UI has to keep, because the engine is careful about them and a
 * careless surface would undo the care:
 *
 *  1. ALLOCATED AND APPLIED ARE DIFFERENT NUMBERS. A Roth line can be asked for
 *     $8,000 and receive $7,500 (the cap) or $0 (not eligible). Showing one
 *     figure would make the difference invisible, and the difference is the
 *     whole answer.
 *  2. NOTHING HERE IS CALLED A P50. This is the projected value at the assumed
 *     return. Doc 33's fan chart will render beside it, and the two are
 *     different statistics.
 *  3. EVERY CAVEAT THE SERVER SENDS IS RENDERED. The notes are not decoration;
 *     they are the difference between a projection and a claim.
 */

/** One editable row of the split. Percentages are strings, like the wire. */
interface Line {
  accountId: string
  lumpPct: string
  monthlyPct: string
  /** A FRACTION as a string; empty means "use the household rate". */
  returnRate: string
}

const DEFAULT_HORIZON = 20

export function BucketAllocator() {
  const buckets = useQuery({ queryKey: ['allocation-buckets'], queryFn: api.allocationBuckets })
  const idle = useQuery({ queryKey: ['idle-cash'], queryFn: api.idleCash })

  const [lump, setLump] = useState('0')
  const [monthly, setMonthly] = useState('0')
  const [horizon, setHorizon] = useState(DEFAULT_HORIZON)
  const [target, setTarget] = useState('')
  const [lines, setLines] = useState<Record<string, Line>>({})
  const [result, setResult] = useState<AllocationResult | null>(null)

  const options = buckets.data?.buckets ?? []

  const lineFor = (id: string): Line =>
    lines[id] ?? { accountId: id, lumpPct: '0', monthlyPct: '0', returnRate: '' }

  const setLine = (id: string, patch: Partial<Line>) =>
    setLines((prev) => ({ ...prev, [id]: { ...lineFor(id), ...patch } }))

  const lumpTotal = sumPct(options, lines, 'lumpPct')
  const monthlyTotal = sumPct(options, lines, 'monthlyPct')

  const run = useMutation({
    mutationFn: () => api.runAllocation(buildInput(lump, monthly, horizon, target, options, lines)),
    onSuccess: setResult,
  })

  // A changed split invalidates the result on screen. Leaving a stale projection
  // beside edited inputs is how somebody reads a number that answers a question
  // they are no longer asking.
  useEffect(() => {
    setResult(null)
  }, [lump, monthly, horizon, target, lines])

  return (
    <div className="space-y-6">
      <section className="glass p-6">
        <h2 className="text-lg font-medium">Allocate</h2>
        <p className="mt-1 text-sm text-mist-500">
          Split a lump sum and a monthly surplus across your accounts, and see what each
          one does. Contribution caps and income limits are applied; nothing here moves
          money.
        </p>

        {buckets.isPending && <p className="mt-4 text-sm text-mist-500">Loading accounts…</p>}
        {buckets.isError && (
          <p className="mt-4 text-sm text-ember-400">Could not load your accounts.</p>
        )}

        {buckets.data && (
          <>
            <div className="mt-5 grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
              <Field
                label="Lump sum"
                value={lump}
                onChange={setLump}
                hint="A one-off amount to place now."
              />
              <Field
                label="Monthly surplus"
                value={monthly}
                onChange={setMonthly}
                hint="What you can add every month."
              />
              <label className="block">
                <span className="mb-1 block text-sm text-mist-300">Horizon (years)</span>
                <input
                  className="field w-full"
                  type="number"
                  min={1}
                  max={60}
                  value={horizon}
                  onChange={(e) => setHorizon(Number(e.target.value))}
                />
                <span className="mt-1 block text-xs text-mist-500">How far to project.</span>
              </label>
              <Field
                label="Target (optional)"
                value={target}
                onChange={setTarget}
                hint="A number to measure the plan against."
              />
            </div>

            {idle.data?.has_benchmark && idle.data.items.length > 0 && (
              <IdleCashPanel
                report={idle.data}
                onIncludeInLump={(amount) => setLump(amount)}
              />
            )}

            <SplitTable
              options={options}
              lineFor={lineFor}
              setLine={setLine}
              lumpTotal={lumpTotal}
              monthlyTotal={monthlyTotal}
              onNormalize={() => setLines(normalize(options, lines))}
              householdRate={buckets.data.real_return_rate}
            />

            {buckets.data.excluded_accounts.length > 0 && (
              <p className="mt-4 text-xs text-mist-500">
                Not offered as buckets because they have no confirmed tax treatment:{' '}
                {buckets.data.excluded_accounts.join(', ')}. Tag them on the Investments
                page to include them.
              </p>
            )}
            {!buckets.data.magi_known && (
              <p className="mt-2 text-xs text-mist-500">
                No modified AGI on file, so Roth eligibility is reported as unknown rather
                than assumed. Add it under Assumptions to have it checked.
              </p>
            )}

            <div className="mt-5 flex flex-wrap items-center gap-3">
              <button
                type="button"
                className="btn-primary"
                onClick={() => run.mutate()}
                disabled={run.isPending}
              >
                {run.isPending ? 'Projecting…' : 'Project this plan'}
              </button>
              {run.isError && (
                <span className="text-sm text-ember-400">
                  {run.error instanceof ApiError ? run.error.message : 'Could not project.'}
                </span>
              )}
            </div>
          </>
        )}
      </section>

      {result && <ResultView result={result} input={buildInput(lump, monthly, horizon, target, options, lines)} />}

      <SavedPlans
        onOpen={(plan) => {
          setLump(plan.inputs.lump)
          setMonthly(plan.inputs.monthly)
          setHorizon(plan.inputs.horizon_years)
          setTarget(plan.inputs.target ?? '')
          const next: Record<string, Line> = {}
          for (const b of plan.inputs.buckets) {
            next[b.account_id] = {
              accountId: b.account_id,
              lumpPct: b.lump_pct,
              monthlyPct: b.monthly_pct,
              returnRate: b.real_return_rate ?? '',
            }
          }
          setLines(next)
          setResult(plan.result ?? null)
        }}
      />
    </div>
  )
}

// --------------------------------------------------------------------------
// The split table
// --------------------------------------------------------------------------

function SplitTable({
  options,
  lineFor,
  setLine,
  lumpTotal,
  monthlyTotal,
  onNormalize,
  householdRate,
}: {
  options: AllocationBucket[]
  lineFor: (id: string) => Line
  setLine: (id: string, patch: Partial<Line>) => void
  lumpTotal: number
  monthlyTotal: number
  onNormalize: () => void
  householdRate: string
}) {
  if (options.length === 0) {
    return (
      <p className="mt-5 text-sm text-mist-400">
        No accounts to allocate to yet. Link or add an account first.
      </p>
    )
  }

  return (
    <div className="mt-6">
      <div className="mb-2 flex flex-wrap items-baseline justify-between gap-2">
        <h3 className="text-sm font-medium">The split</h3>
        <div className="flex items-center gap-3 text-xs">
          <TotalBadge label="lump" total={lumpTotal} />
          <TotalBadge label="monthly" total={monthlyTotal} />
          <button type="button" className="text-arcane-300 hover:underline" onClick={onNormalize}>
            Normalise to 100%
          </button>
        </div>
      </div>

      <div className="overflow-x-auto rounded-xl border border-white/10">
        <table className="w-full text-sm">
          <thead className="bg-white/[0.03] text-left text-xs text-mist-400">
            <tr>
              <th className="p-3 font-medium">Account</th>
              <th className="p-3 font-medium">Balance</th>
              <th className="p-3 font-medium">Lump %</th>
              <th className="p-3 font-medium">Monthly %</th>
              <th className="p-3 font-medium">Return</th>
            </tr>
          </thead>
          <tbody>
            {options.map((b) => {
              const line = lineFor(b.account_id)
              return (
                <tr key={b.account_id} className="border-t border-white/5">
                  <td className="p-3">
                    <div className="text-mist-200">{b.name}</div>
                    <div className="text-xs text-mist-500">
                      {kindLabel(b)}
                      {b.institution ? ` · ${b.institution}` : ''}
                      {Number(b.existing_monthly) > 0
                        ? ` · already ${formatMoney(b.existing_monthly)}/mo`
                        : ''}
                    </div>
                  </td>
                  <td className="p-3 text-mist-300">{formatMoney(b.balance)}</td>
                  <td className="p-3">
                    <PctInput
                      value={line.lumpPct}
                      onChange={(v) => setLine(b.account_id, { lumpPct: v })}
                    />
                  </td>
                  <td className="p-3">
                    <PctInput
                      value={line.monthlyPct}
                      onChange={(v) => setLine(b.account_id, { monthlyPct: v })}
                    />
                  </td>
                  <td className="p-3">
                    {b.kind === 'investment' ? (
                      <input
                        className="field w-24"
                        inputMode="decimal"
                        placeholder={householdRate}
                        value={line.returnRate}
                        onChange={(e) => setLine(b.account_id, { returnRate: e.target.value })}
                        aria-label={`Assumed real return for ${b.name}`}
                      />
                    ) : (
                      <span className="text-xs text-mist-500">{nonInvestmentRate(b)}</span>
                    )}
                  </td>
                </tr>
              )
            })}
          </tbody>
        </table>
      </div>
      <p className="mt-2 text-xs text-mist-500">
        Returns are REAL (net of inflation) and a blank one uses your household
        assumption of {householdRate}. A debt's "return" is the interest it avoids; cash
        accrues at its own yield, which can be negative in real terms.
      </p>
    </div>
  )
}

function TotalBadge({ label, total }: { label: string; total: number }) {
  const over = total > 100.01
  return (
    <span className={over ? 'text-ember-400' : 'text-mist-500'}>
      {label}: {round2(total)}%{over ? ' — over 100%' : ''}
    </span>
  )
}

function PctInput({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  return (
    <input
      className="field w-20"
      inputMode="decimal"
      value={value}
      onChange={(e) => onChange(e.target.value)}
    />
  )
}

// --------------------------------------------------------------------------
// The result
// --------------------------------------------------------------------------

function ResultView({ result, input }: { result: AllocationResult; input: AllocationRunInput }) {
  return (
    <>
      <section className="glass p-6">
        <div className="flex flex-wrap items-baseline justify-between gap-3">
          <h2 className="text-lg font-medium">In {result.horizon_years} years</h2>
          <span className="text-xs text-mist-500">as of {result.as_of}</span>
        </div>

        <div className="mt-4 grid gap-4 sm:grid-cols-3">
          <Headline
            label="Projected assets"
            value={formatMoney(result.projected_assets)}
            note="investments + cash; debt is not counted as an asset"
          />
          <Headline
            label="Doing nothing"
            value={formatMoney(result.baseline_assets)}
            note="the same horizon without this plan"
          />
          <Headline
            label="This plan adds"
            value={formatMoney(result.delta)}
            note={
              Number(result.total_interest_avoided) > 0
                ? `plus ${formatMoney(result.total_interest_avoided)} of interest avoided`
                : undefined
            }
          />
        </div>

        {result.target_nest_egg && (
          <p className="mt-4 text-sm">
            <span className={result.target_met ? 'text-rune-300' : 'text-ember-400'}>
              {result.target_met ? 'Reaches' : 'Falls short of'}
            </span>{' '}
            your target of {formatMoney(result.target_nest_egg)}.
          </p>
        )}

        <p className="mt-4 text-xs text-mist-500">{result.basis}</p>
      </section>

      <section className="glass p-6">
        <h3 className="text-lg font-medium">Per bucket</h3>
        <ul className="mt-4 space-y-3">
          {result.buckets.map((b) => (
            <BucketCard key={b.account_id} bucket={b} />
          ))}
        </ul>
      </section>

      {result.cap_notes.length > 0 && (
        <section className="glass p-6">
          <h3 className="text-lg font-medium">Contribution caps</h3>
          <ul className="mt-3 space-y-2 text-sm">
            {result.cap_notes.map((c) => (
              <li key={c.group} className="rounded-xl border border-white/10 bg-white/[0.03] p-3">
                <span className="text-mist-200">{c.group.toUpperCase()}</span>: planned{' '}
                {formatMoney(c.planned_annual)}/yr, allowed {formatMoney(c.allowed_annual)}/yr —{' '}
                <span className="text-ember-400">{formatMoney(c.spill_annual)} spilled</span> and
                has to go somewhere else.
              </li>
            ))}
          </ul>
          <p className="mt-3 text-xs text-mist-500">{result.cap_basis}</p>
        </section>
      )}

      {result.goals.length > 0 && (
        <section className="glass p-6">
          <h3 className="text-lg font-medium">What it does for your goals</h3>
          <ul className="mt-3 space-y-2 text-sm">
            {result.goals.map((g) => (
              <li key={g.goal_id} className="rounded-xl border border-white/10 bg-white/[0.03] p-3">
                <div className="flex flex-wrap items-baseline justify-between gap-2">
                  <span className="text-mist-200">{g.name}</span>
                  <span className={g.on_track ? 'text-rune-300' : 'text-ember-400'}>
                    {g.achieved ? 'achieved' : g.on_track ? 'on track' : 'behind'}
                  </span>
                </div>
                <p className="mt-1 text-xs text-mist-500">
                  {formatMoney(g.current)} of {formatMoney(g.target)}
                  {g.linked
                    ? ` · this plan adds ${formatMoney(g.plan_monthly)}/mo`
                    : ' · no account linked, so the plan cannot fund it'}
                  {!g.open_ended && ` · needs ${formatMoney(g.required_monthly)}/mo`}
                  {Number(g.shortfall) > 0 && ` · short ${formatMoney(g.shortfall)}/mo`}
                </p>
              </li>
            ))}
          </ul>
        </section>
      )}

      {result.college.length > 0 && (
        <section className="glass p-6">
          <h3 className="text-lg font-medium">College</h3>
          <ul className="mt-3 space-y-3">
            {result.college.map((c) => (
              <CollegeCard key={c.goal_id} college={c} />
            ))}
          </ul>
        </section>
      )}

      {result.horizon_flags.length > 0 && (
        <section className="glass p-6">
          <h3 className="text-lg font-medium">Worth a second look</h3>
          <ul className="mt-3 space-y-2 text-sm text-mist-300">
            {result.horizon_flags.map((f) => (
              <li key={f.goal_id} className="rounded-xl border border-ember-500/20 bg-ember-500/5 p-3">
                {f.message}
              </li>
            ))}
          </ul>
        </section>
      )}

      {result.notes.length > 0 && (
        <section className="glass p-6">
          <h3 className="text-sm font-medium text-mist-300">What this does not know</h3>
          <ul className="mt-2 space-y-1.5 text-xs text-mist-500">
            {result.notes.map((n, i) => (
              <li key={i}>{n}</li>
            ))}
          </ul>
        </section>
      )}

      <SavePlanForm input={input} />
    </>
  )
}

function BucketCard({ bucket }: { bucket: AllocationBucketResult }) {
  const [open, setOpen] = useState(false)
  const shortfall = Number(bucket.allocated_lump) - Number(bucket.applied_lump)

  return (
    <li className="rounded-xl border border-white/10 bg-white/[0.03] p-4">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <span className="text-mist-200">{bucket.name}</span>
        <span className="text-xs text-mist-500">{engineLabel(bucket.engine)}</span>
      </div>

      {bucket.kind === 'debt' ? (
        <DebtBody bucket={bucket} />
      ) : (
        <p className="mt-2 text-sm text-mist-300">
          {formatMoney(bucket.projected_value)} at the horizon —{' '}
          {formatMoney(bucket.contributed)} put in, {formatMoney(bucket.growth)} growth, from{' '}
          {formatMoney(bucket.start_balance)} today.
        </p>
      )}

      <p className="mt-1 text-xs text-mist-500">
        Allocated {formatMoney(bucket.allocated_lump)} up front and{' '}
        {formatMoney(bucket.allocated_monthly)}/mo
        {shortfall > 0.005 && (
          <span className="text-ember-400">
            {' '}
            — only {formatMoney(bucket.applied_lump)} of the lump could be applied
          </span>
        )}
        .
      </p>

      {bucket.eligibility && bucket.eligibility !== 'eligible' && (
        <p className="mt-2 rounded-lg border border-ember-500/20 bg-ember-500/5 p-2 text-xs text-mist-300">
          <span className="font-medium">{eligibilityLabel(bucket.eligibility)}.</span>{' '}
          {bucket.eligibility_note}
        </p>
      )}

      {bucket.notes.map((n, i) => (
        <p key={i} className="mt-2 text-xs text-mist-500">
          {n}
        </p>
      ))}

      <button
        type="button"
        className="mt-2 text-xs text-arcane-300 hover:underline"
        onClick={() => setOpen((v) => !v)}
      >
        {open ? 'Hide the arithmetic' : 'Show the arithmetic'}
      </button>
      {open && (
        <p className="mt-2 rounded-lg border border-white/10 bg-black/20 p-3 text-xs leading-relaxed text-mist-400">
          {bucket.formula}
        </p>
      )}
    </li>
  )
}

function DebtBody({ bucket }: { bucket: AllocationBucketResult }) {
  const base = bucket.payoff_base
  const plan = bucket.payoff_plan
  if (!base || !plan) {
    return <p className="mt-2 text-sm text-mist-400">No payoff schedule available.</p>
  }
  if (plan.never_pays_off) {
    return (
      <p className="mt-2 text-sm text-ember-400">
        Never pays off, even with this plan — the interest alone is{' '}
        {formatMoney(plan.monthly_interest)} a month.
      </p>
    )
  }
  return (
    <p className="mt-2 text-sm text-mist-300">
      Paid off in {plan.months} months
      {base.never_pays_off
        ? ' (it never would at the current payment)'
        : ` instead of ${base.months} — ${bucket.months_saved} months sooner`}
      , saving {formatMoney(bucket.interest_avoided)} of interest.
    </p>
  )
}

function CollegeCard({ college }: { college: CollegeResult }) {
  if (!college.projectable) {
    return (
      <li className="rounded-xl border border-white/10 bg-white/[0.03] p-4">
        <div className="text-mist-200">{college.name}</div>
        <p className="mt-1 text-xs text-mist-500">{college.note}</p>
      </li>
    )
  }
  return (
    <li className="rounded-xl border border-white/10 bg-white/[0.03] p-4">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <span className="text-mist-200">{college.name}</span>
        <span
          className={college.first_shortfall_year === 0 ? 'text-rune-300' : 'text-ember-400'}
        >
          {college.funded_pct}% funded
        </span>
      </div>
      <p className="mt-1 text-sm text-mist-300">
        Enrollment in {college.years_to_enrollment} years;{' '}
        {formatMoney(college.balance_at_enrollment)} available against{' '}
        {formatMoney(college.total_cost)} for {college.years} years.
      </p>
      {college.monthly_needed && (
        <p className="mt-1 text-sm text-mist-300">
          {formatMoney(college.monthly_needed)} a month more would fund all of it.
        </p>
      )}

      <table className="mt-3 w-full text-xs">
        <thead className="text-left text-mist-500">
          <tr>
            <th className="py-1 font-medium">Year</th>
            <th className="py-1 font-medium">Cost</th>
            <th className="py-1 font-medium">Covered</th>
            <th className="py-1 font-medium">Short</th>
          </tr>
        </thead>
        <tbody>
          {college.years_detail.map((y) => (
            <tr key={y.year} className="border-t border-white/5">
              <td className="py-1 text-mist-400">{y.year}</td>
              <td className="py-1 text-mist-300">{formatMoney(y.cost)}</td>
              <td className="py-1 text-mist-300">{formatMoney(y.covered)}</td>
              <td className={'py-1 ' + (Number(y.shortfall) > 0 ? 'text-ember-400' : 'text-mist-500')}>
                {formatMoney(y.shortfall)}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      <p className="mt-2 text-xs text-mist-500">{college.basis}</p>
    </li>
  )
}

// --------------------------------------------------------------------------
// Idle cash
// --------------------------------------------------------------------------

function IdleCashPanel({
  report,
  onIncludeInLump,
}: {
  report: IdleCashReport
  onIncludeInLump: (amount: string) => void
}) {
  return (
    <div className="mt-5 rounded-xl border border-arcane-500/20 bg-arcane-500/5 p-4">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <h3 className="text-sm font-medium">Idle cash</h3>
        <button
          type="button"
          className="text-xs text-arcane-300 hover:underline"
          onClick={() => onIncludeInLump(report.total_idle)}
        >
          Include {formatMoney(report.total_idle)} in the lump
        </button>
      </div>
      <ul className="mt-2 space-y-1 text-xs text-mist-400">
        {report.items.map((i) => (
          <li key={i.account_id}>{i.detail}</li>
        ))}
      </ul>
      <p className="mt-2 text-xs text-mist-500">
        {formatMoney(report.total_annual_drag)} a year in total. {report.basis}
      </p>
    </div>
  )
}

// --------------------------------------------------------------------------
// Saved plans
// --------------------------------------------------------------------------

function SavePlanForm({ input }: { input: AllocationRunInput }) {
  const queryClient = useQueryClient()
  const [name, setName] = useState('')
  const save = useMutation({
    mutationFn: () => api.saveAllocationPlan(name.trim(), input),
    onSuccess: () => {
      setName('')
      queryClient.invalidateQueries({ queryKey: ['allocation-plans'] })
    },
  })

  return (
    <section className="glass p-6">
      <h3 className="text-sm font-medium">Save this plan</h3>
      <p className="mt-1 text-xs text-mist-500">
        Only the inputs are saved. Reopening it recomputes against your balances as they
        stand then — a stored projection is a figure that quietly stops being true.
      </p>
      <form
        className="mt-3 flex gap-2"
        onSubmit={(e) => {
          e.preventDefault()
          if (name.trim()) save.mutate()
        }}
      >
        <input
          className="field flex-1"
          placeholder="Name this plan…"
          value={name}
          onChange={(e) => setName(e.target.value)}
        />
        <button type="submit" className="btn-ghost" disabled={!name.trim() || save.isPending}>
          Save
        </button>
      </form>
      {save.isError && (
        <p className="mt-2 text-sm text-ember-400">
          {save.error instanceof ApiError ? save.error.message : 'Could not save.'}
        </p>
      )}
    </section>
  )
}

function SavedPlans({
  onOpen,
}: {
  onOpen: (plan: Awaited<ReturnType<typeof api.allocationPlan>>) => void
}) {
  const queryClient = useQueryClient()
  const plans = useQuery({ queryKey: ['allocation-plans'], queryFn: api.allocationPlans })
  const [opening, setOpening] = useState<string | null>(null)

  const open = useMutation({
    mutationFn: (id: string) => api.allocationPlan(id),
    onSuccess: onOpen,
    onSettled: () => setOpening(null),
  })
  const remove = useMutation({
    mutationFn: (id: string) => api.deleteAllocationPlan(id),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['allocation-plans'] }),
  })

  // Doc 33's surfaces hang off a SAVED plan, because a distribution and a drift
  // report are both statements about a decision the household has made. An
  // unsaved projection is still a question.
  const [likelihoodFor, setLikelihoodFor] = useState<AllocationPlanSummary | null>(null)
  const [trackerFor, setTrackerFor] = useState<string | null>(null)

  const list = plans.data ?? []
  if (list.length === 0) return null

  return (
    <>
    <section className="glass p-6">
      <h3 className="text-lg font-medium">Saved plans</h3>
      <ul className="mt-3 space-y-2 text-sm">
        {list.map((p) => (
          <li
            key={p.id}
            className="flex items-center justify-between gap-3 rounded-xl border border-white/10 bg-white/[0.03] p-3"
          >
            <div className="min-w-0">
              <div className="text-mist-200">{p.name}</div>
              <div className="text-xs text-mist-500">
                {formatMoney(p.inputs.lump)} up front, {formatMoney(p.inputs.monthly)}/mo over{' '}
                {p.inputs.horizon_years} years
              </div>
            </div>
            <div className="flex shrink-0 gap-3 text-xs">
              <button
                type="button"
                className="text-arcane-300 hover:underline"
                onClick={() => {
                  setOpening(p.id)
                  open.mutate(p.id)
                }}
              >
                {opening === p.id ? 'Opening…' : 'Open'}
              </button>
              <button
                type="button"
                className="text-arcane-300 hover:underline"
                onClick={() => {
                  setLikelihoodFor(likelihoodFor?.id === p.id ? null : p)
                  setTrackerFor(null)
                }}
              >
                Likelihood
              </button>
              <button
                type="button"
                className="text-arcane-300 hover:underline"
                onClick={() => {
                  setTrackerFor(trackerFor === p.id ? null : p.id)
                  setLikelihoodFor(null)
                }}
              >
                Track
              </button>
              <button
                type="button"
                className="text-mist-500 hover:text-mist-300"
                onClick={() => remove.mutate(p.id)}
              >
                Delete
              </button>
            </div>
          </li>
        ))}
      </ul>
    </section>

    {likelihoodFor && <PlanLikelihoodPanel key={likelihoodFor.id} plan={likelihoodFor} />}
    {trackerFor && <PlanTrackerPanel key={trackerFor} planId={trackerFor} />}
    <PlanComparisonPanel plans={list} />
    </>
  )
}

// --------------------------------------------------------------------------
// Small helpers
// --------------------------------------------------------------------------

function Field({
  label,
  value,
  onChange,
  hint,
}: {
  label: string
  value: string
  onChange: (v: string) => void
  hint: string
}) {
  return (
    <label className="block">
      <span className="mb-1 block text-sm text-mist-300">{label}</span>
      <input
        className="field w-full"
        inputMode="decimal"
        value={value}
        onChange={(e) => onChange(e.target.value)}
      />
      <span className="mt-1 block text-xs text-mist-500">{hint}</span>
    </label>
  )
}

function Headline({ label, value, note }: { label: string; value: string; note?: string }) {
  return (
    <div className="rounded-xl border border-white/10 bg-white/[0.03] p-4">
      <div className="text-xs text-mist-500">{label}</div>
      <div className="mt-1 text-xl font-medium text-mist-100">{value}</div>
      {note && <div className="mt-1 text-xs text-mist-500">{note}</div>}
    </div>
  )
}

function buildInput(
  lump: string,
  monthly: string,
  horizon: number,
  target: string,
  options: AllocationBucket[],
  lines: Record<string, Line>,
): AllocationRunInput {
  const splits: AllocationSplitInput[] = []
  for (const b of options) {
    const line = lines[b.account_id]
    if (!line) continue
    // A bucket allocated nothing is left out of the request entirely rather than
    // sent as a zero. Zero-percent lines would clutter the result with buckets
    // the user did not choose, and the engine's "unallocated" figure already
    // accounts for money that went nowhere.
    if (numberOr0(line.lumpPct) === 0 && numberOr0(line.monthlyPct) === 0) continue
    splits.push({
      account_id: b.account_id,
      lump_pct: line.lumpPct || '0',
      monthly_pct: line.monthlyPct || '0',
      real_return_rate: line.returnRate.trim() === '' ? null : line.returnRate.trim(),
    })
  }
  return {
    lump: lump.trim() === '' ? '0' : lump.trim(),
    monthly: monthly.trim() === '' ? '0' : monthly.trim(),
    horizon_years: horizon,
    target_nest_egg: target.trim() === '' ? null : target.trim(),
    splits,
  }
}

/**
 * Normalise scales the non-zero lines so each column sums to 100.
 *
 * It scales rather than redistributing evenly: the RELATIVE split is the user's
 * decision and the only thing wrong with it is the total. A normaliser that
 * flattened everything to equal shares would be the UI overruling the choice it
 * was asked to tidy.
 */
function normalize(options: AllocationBucket[], lines: Record<string, Line>): Record<string, Line> {
  const lumpTotal = sumPct(options, lines, 'lumpPct')
  const monthlyTotal = sumPct(options, lines, 'monthlyPct')
  const next: Record<string, Line> = {}
  for (const b of options) {
    const line = lines[b.account_id]
    if (!line) continue
    next[b.account_id] = {
      ...line,
      lumpPct: lumpTotal > 0 ? String(round2((numberOr0(line.lumpPct) / lumpTotal) * 100)) : line.lumpPct,
      monthlyPct:
        monthlyTotal > 0
          ? String(round2((numberOr0(line.monthlyPct) / monthlyTotal) * 100))
          : line.monthlyPct,
    }
  }
  return next
}

function sumPct(
  options: AllocationBucket[],
  lines: Record<string, Line>,
  key: 'lumpPct' | 'monthlyPct',
): number {
  let total = 0
  for (const b of options) {
    const line = lines[b.account_id]
    if (line) total += numberOr0(line[key])
  }
  return total
}

function numberOr0(raw: string): number {
  const n = Number(raw)
  return Number.isFinite(n) ? n : 0
}

function round2(n: number): number {
  return Math.round(n * 100) / 100
}

function kindLabel(b: AllocationBucket): string {
  switch (b.kind) {
    case 'investment':
      return b.treatment ? b.treatment.replace(/_/g, ' ') : 'investment'
    case 'debt':
      return b.apr ? `debt at ${b.apr}%` : 'debt, rate unknown'
    default:
      return b.deposit_apy ? `cash at ${b.deposit_apy}%` : 'cash, yield unknown'
  }
}

function nonInvestmentRate(b: AllocationBucket): string {
  if (b.kind === 'debt') return b.apr ? `${b.apr}% APR` : 'rate unknown'
  return b.deposit_apy ? `${b.deposit_apy}% APY` : 'yield unknown'
}

/**
 * eligibilityLabel names the status in words a household can act on.
 *
 * "Ineligible" is deliberately worded as a DIRECT contribution being disallowed,
 * which is the true statement: a backdoor Roth is legal, common above the
 * phase-out, and not modelled here. "You cannot put money here" would be false.
 */
function eligibilityLabel(status: string): string {
  switch (status) {
    case 'ineligible':
      return 'Not eligible for a direct contribution'
    case 'phased_out':
      return 'Inside the income phase-out'
    default:
      return 'Eligibility not checked'
  }
}

function engineLabel(engine: AllocationBucketResult['engine']): string {
  switch (engine) {
    case 'amortization':
      return 'amortization'
    case 'accrual':
      return 'accrual, no volatility'
    default:
      return 'compounding'
  }
}
