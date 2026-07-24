import { useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../lib/api'
import type {
  BudgetProgress,
  BudgetProposal,
  Category,
  SafeToSpend,
} from '../lib/api'
import { formatMoney } from '../lib/money'
import { STATUS } from '../components/charts/tokens'

/** Month options: the current month plus the previous eleven. */
function recentMonths(count = 12) {
  const now = new Date()
  return Array.from({ length: count }, (_, i) => {
    const d = new Date(now.getFullYear(), now.getMonth() - i, 1)
    const value = `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}`
    return {
      value,
      label: d.toLocaleDateString('en-US', { month: 'long', year: 'numeric' }),
      from: `${value}-01`,
      to: new Date(d.getFullYear(), d.getMonth() + 1, 0).toISOString().slice(0, 10),
    }
  })
}

/**
 * Sums a set of server-exact decimal strings for a display-only total. Like
 * `sumBalances` on the Dashboard, this is safe because the figure is only ever
 * rendered, never sent back as a headline number — the per-budget arithmetic
 * that matters is done in SQL.
 */
function sumStrings(values: string[]): string {
  return values.reduce((total, v) => total + Number(v ?? 0), 0).toFixed(2)
}

export function Budgets() {
  const qc = useQueryClient()
  const months = recentMonths()
  const [monthValue, setMonthValue] = useState(months[0].value)
  const month = months.find((m) => m.value === monthValue) ?? months[0]
  const range = { from: month.from, to: month.to }

  const budgets = useQuery({
    queryKey: ['budgets', range.from, range.to],
    queryFn: () => api.budgets(range),
  })
  const categories = useQuery({
    queryKey: ['categories'],
    queryFn: api.categories,
  })

  const invalidate = () =>
    qc.invalidateQueries({ queryKey: ['budgets', range.from, range.to] })

  const setBudget = useMutation({
    mutationFn: ({
      categoryID,
      amount,
      period,
      rollover,
    }: {
      categoryID: string
      amount: string
      period?: 'weekly' | 'monthly' | 'yearly'
      rollover?: boolean
    }) => api.setBudget(categoryID, amount, period ?? 'monthly', rollover ?? false),
    onSuccess: invalidate,
  })
  const deleteBudget = useMutation({
    mutationFn: (id: string) => api.deleteBudget(id),
    onSuccess: invalidate,
  })

  const rows = budgets.data ?? []

  // Categories you can budget: spending only — income and transfers are not
  // budgeted. Exclude any that already have a budget so "Add budget" never
  // offers a duplicate (POST is an upsert; re-adding would just overwrite).
  const budgetable = useMemo(() => {
    const budgeted = new Set((budgets.data ?? []).map((b) => b.category_id))
    return (categories.data ?? []).filter(
      (c) => !c.is_income && !c.is_transfer && !budgeted.has(c.id),
    )
  }, [categories.data, budgets.data])

  const totalBudgeted = sumStrings(rows.map((b) => b.budgeted))
  const totalSpent = sumStrings(rows.map((b) => b.spent))
  const totalRemaining = sumStrings(rows.map((b) => b.remaining))

  return (
    <div className="space-y-8">
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold">Budgets</h1>
          <p className="mt-1 text-mist-300">
            What you planned to spend, and how the month is tracking.
          </p>
        </div>

        <div>
          <label className="label" htmlFor="month">
            Month
          </label>
          <select
            id="month"
            className="field"
            value={monthValue}
            onChange={(e) => setMonthValue(e.target.value)}
          >
            {months.map((m) => (
              <option key={m.value} value={m.value}>
                {m.label}
              </option>
            ))}
          </select>
        </div>
      </div>

      <SafeToSpendCard />

      <div className="grid gap-4 sm:grid-cols-3">
        <Tile label="Budgeted" value={formatMoney(totalBudgeted)} />
        <Tile label="Spent" value={formatMoney(totalSpent)} />
        <Tile
          label="Remaining"
          value={formatMoney(totalRemaining)}
          tone={Number(totalRemaining) < 0 ? 'critical' : 'good'}
        />
      </div>

      <section className="glass p-6">
        <h2 className="mb-1 text-lg font-medium">Category budgets</h2>
        <p className="mb-5 text-sm text-mist-300">{month.label}</p>

        {budgets.isPending ? (
          <Loading />
        ) : rows.length === 0 ? (
          <Empty>
            No budgets set yet. Add one below to start tracking a category
            against a monthly target.
          </Empty>
        ) : (
          <div className="space-y-3">
            {rows.map((b) => (
              <BudgetRow
                key={b.budget_id}
                budget={b}
                onSave={(amount, period, rollover) =>
                  setBudget.mutate({
                    categoryID: b.category_id,
                    amount,
                    period,
                    rollover,
                  })
                }
                onDelete={() => deleteBudget.mutate(b.budget_id)}
                saving={setBudget.isPending}
                deleting={deleteBudget.isPending}
              />
            ))}
          </div>
        )}
      </section>

      <SuggestBudgets />

      <AddBudget
        categories={budgetable}
        loading={categories.isPending}
        onAdd={(categoryID, amount, period, rollover) =>
          setBudget.mutate({ categoryID, amount, period, rollover })
        }
        saving={setBudget.isPending}
      />

      {(setBudget.isError || deleteBudget.isError) && (
        <p role="alert" className="text-sm text-ember-400">
          {(setBudget.error ?? deleteBudget.error)?.message}
        </p>
      )}
    </div>
  )
}

// SafeToSpendCard shows the headline "free to spend this month" figure with its
// full breakdown, so the number is legible rather than magic: typical income,
// less fixed bills, less what's already budgeted, less goal savings. The figure
// is household-level and based on typical income, so it does not track the month
// selector above.
function SafeToSpendCard() {
  const q = useQuery({ queryKey: ['safe-to-spend'], queryFn: api.safeToSpend })

  if (q.isPending) {
    return (
      <section className="glass p-6">
        <Loading />
      </section>
    )
  }
  if (q.isError || !q.data) {
    return null
  }

  const d: SafeToSpend = q.data
  const safe = Number(d.safe_to_spend)
  const tone = safe < 0 ? 'text-ember-400' : 'text-fern-300'

  const parts: { label: string; value: string; sign: '−' | '' }[] = [
    { label: 'Typical income', value: d.expected_income, sign: '' },
    { label: 'Fixed bills', value: d.fixed_costs, sign: '−' },
    { label: 'Budgeted', value: d.budgeted_discretionary, sign: '−' },
    { label: 'Goal savings', value: d.goal_contributions, sign: '−' },
  ]

  return (
    <section className="glass p-6">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <div>
          <h2 className="text-lg font-medium">Safe to spend</h2>
          <p className="mt-1 text-sm text-mist-300">
            What's free this month after bills, budgets, and goals.
          </p>
        </div>
        <span className={`text-3xl font-semibold tabular ${tone}`}>
          {formatMoney(d.safe_to_spend)}
        </span>
      </div>

      <div className="mt-5 flex flex-wrap gap-x-6 gap-y-2 text-sm">
        {parts.map((p) => (
          <span key={p.label} className="flex items-baseline gap-1.5">
            <span className="text-mist-500">{p.label}</span>
            <span className="tabular text-mist-200">
              {p.sign}
              {formatMoney(p.value)}
            </span>
          </span>
        ))}
      </div>

      {safe > 0 && (
        <p className="mt-3 text-xs text-mist-500">
          Assign this to budgets or goals to reach a zero-based plan, where every
          dollar has a job.
        </p>
      )}

      {d.income_months < 3 && (
        <p className="mt-3 text-xs text-mist-500">
          Based on only {d.income_months}{' '}
          {d.income_months === 1 ? 'month' : 'months'} of income history, so this
          is a rough estimate.
        </p>
      )}
    </section>
  )
}

function BudgetRow({
  budget,
  onSave,
  onDelete,
  saving,
  deleting,
}: {
  budget: BudgetProgress
  onSave: (
    amount: string,
    period: 'weekly' | 'monthly' | 'yearly',
    rollover: boolean,
  ) => void
  onDelete: () => void
  saving: boolean
  deleting: boolean
}) {
  const [editing, setEditing] = useState(false)
  const [amount, setAmount] = useState(budget.budgeted)
  const [period, setPeriod] = useState(
    budget.period as 'weekly' | 'monthly' | 'yearly',
  )
  const [rollover, setRollover] = useState(budget.rollover)

  const spentNum = Number(budget.spent)
  // The ceiling this month is `available` (amount + any carried balance) for a
  // rollover budget, or just the amount otherwise. Bar and remaining measure
  // against that. Guard the zero/negative case so the width never renders NaN.
  const ceilingNum = Number(budget.available)
  const pct = ceilingNum > 0 ? (spentNum / ceilingNum) * 100 : spentNum > 0 ? 100 : 0
  const over = spentNum > ceilingNum
  const fill = over ? STATUS.critical : STATUS.good
  const carryNum = Number(budget.carryover)

  const save = () => {
    // Rollover is a monthly-only concept; drop it when switching to another period.
    onSave(amount, period, period === 'monthly' ? rollover : false)
    setEditing(false)
  }

  // A short label for the budget's cadence, e.g. "weekly · $100".
  const periodLabel =
    budget.period === 'weekly'
      ? 'weekly'
      : budget.period === 'yearly'
        ? 'yearly'
        : 'monthly'

  return (
    <div className="rounded-xl border border-white/5 p-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <span className="flex items-center gap-2 font-medium">
          <span
            className="inline-block h-2 w-2 shrink-0 rounded-full"
            style={{ backgroundColor: budget.color ?? STATUS.good }}
          />
          {budget.name}
          {budget.period !== 'monthly' && (
            <span className="rounded border border-white/10 bg-white/5 px-1.5 py-0.5 text-[10px] text-mist-400">
              {periodLabel}
            </span>
          )}
          {budget.rollover && (
            <span
              className="rounded border border-white/10 bg-white/5 px-1.5 py-0.5 text-[10px] text-mist-400"
              title="Unspent amount rolls into next month"
            >
              rollover
            </span>
          )}
          {budget.rollover && carryNum !== 0 && (
            <span
              className="tabular text-[10px]"
              style={{ color: carryNum < 0 ? STATUS.critical : STATUS.good }}
            >
              {carryNum < 0 ? '' : '+'}
              {formatMoney(budget.carryover)} carried
            </span>
          )}
        </span>

        {editing ? (
          <div className="flex flex-wrap items-center gap-2">
            <input
              className="field w-28"
              type="number"
              min="0"
              step="0.01"
              value={amount}
              onChange={(e) => setAmount(e.target.value)}
              aria-label={`Budget amount for ${budget.name}`}
            />
            <select
              className="field"
              value={period}
              onChange={(e) =>
                setPeriod(e.target.value as 'weekly' | 'monthly' | 'yearly')
              }
              aria-label={`Budget period for ${budget.name}`}
            >
              <option value="weekly">weekly</option>
              <option value="monthly">monthly</option>
              <option value="yearly">yearly</option>
            </select>
            {period === 'monthly' && (
              <label className="flex items-center gap-1.5 text-sm text-mist-300">
                <input
                  type="checkbox"
                  checked={rollover}
                  onChange={(e) => setRollover(e.target.checked)}
                />
                Roll over
              </label>
            )}
            <button
              className="btn-ghost px-3 py-1.5 text-sm"
              disabled={saving}
              onClick={save}
            >
              Save
            </button>
            <button
              className="btn-ghost px-3 py-1.5 text-sm text-mist-300"
              onClick={() => {
                setAmount(budget.budgeted)
                setPeriod(budget.period as 'weekly' | 'monthly' | 'yearly')
                setRollover(budget.rollover)
                setEditing(false)
              }}
            >
              Cancel
            </button>
          </div>
        ) : (
          <div className="flex items-center gap-3">
            <span className="tabular text-sm text-mist-300">
              {formatMoney(budget.spent)} of {formatMoney(budget.available)}
            </span>
            <button
              className="btn-ghost px-3 py-1.5 text-sm"
              onClick={() => setEditing(true)}
            >
              Edit
            </button>
            <button
              className="btn-ghost px-3 py-1.5 text-sm text-ember-400"
              disabled={deleting}
              onClick={onDelete}
            >
              Delete
            </button>
          </div>
        )}
      </div>

      <div className="mt-3 flex items-center gap-3">
        <div className="h-1.5 flex-1 overflow-hidden rounded-full bg-white/5">
          <div
            className="h-full rounded-full"
            style={{
              width: `${Math.min(pct, 100)}%`,
              backgroundColor: fill,
            }}
          />
        </div>
        <span
          className="tabular w-24 text-right text-xs"
          style={{ color: over ? STATUS.critical : undefined }}
        >
          {over
            ? `${formatMoney(String(spentNum - ceilingNum))} over`
            : `${formatMoney(budget.remaining)} left`}
        </span>
      </div>
    </div>
  )
}

// A proposal is checked by default unless the category already has a budget that
// already covers its average — no point re-proposing what's already handled.
function defaultChecked(p: BudgetProposal): boolean {
  if (p.already_budgeted && p.current_budget) {
    return Number(p.current_budget) < Number(p.computed_average)
  }
  return true
}

// SuggestBudgets fetches a proposed target per spending category on demand, lets
// the user review/edit/deselect, and applies the chosen rows through the same
// single-write budget mutation the manual form uses.
function SuggestBudgets() {
  const qc = useQueryClient()
  const [amounts, setAmounts] = useState<Record<string, string>>({})
  const [checked, setChecked] = useState<Record<string, boolean>>({})

  const suggest = useMutation({
    mutationFn: api.suggestBudgets,
    onSuccess: (data) => {
      const a: Record<string, string> = {}
      const c: Record<string, boolean> = {}
      for (const p of data.proposals) {
        a[p.category_id] = p.suggested_amount
        c[p.category_id] = defaultChecked(p)
      }
      setAmounts(a)
      setChecked(c)
    },
  })

  const apply = useMutation({
    mutationFn: async (rows: { categoryID: string; amount: string }[]) => {
      // Sequential single writes, exactly like manual entry — keeps validation
      // and audit identical to POST /api/budgets.
      for (const row of rows) await api.setBudget(row.categoryID, row.amount)
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['budgets'] })
      suggest.reset()
    },
  })

  const data = suggest.data
  const selectedRows = (data?.proposals ?? [])
    .filter((p) => checked[p.category_id])
    .map((p) => ({ categoryID: p.category_id, amount: amounts[p.category_id] }))
    .filter((r) => r.amount !== '' && Number(r.amount) > 0)

  const flexible = (data?.proposals ?? []).filter((p) => !p.is_fixed)
  const fixed = (data?.proposals ?? []).filter((p) => p.is_fixed)

  return (
    <section className="glass p-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 className="text-lg font-medium">Suggest budgets</h2>
          <p className="mt-1 text-sm text-mist-300">
            Propose a round target for each category from your last year of
            spending. Review, tweak, and apply the ones you want.
          </p>
        </div>
        {!data && (
          <button
            className="btn-primary px-4 py-2 text-sm"
            disabled={suggest.isPending}
            onClick={() => suggest.mutate()}
          >
            {suggest.isPending ? 'Thinking…' : 'Suggest budgets'}
          </button>
        )}
      </div>

      {suggest.isError && (
        <p role="alert" className="mt-4 text-sm text-ember-400">
          {suggest.error.message}
        </p>
      )}

      {data && data.proposals.length === 0 && (
        <Empty>
          Not enough spending history yet to suggest budgets. Add a budget
          manually below.
        </Empty>
      )}

      {data && data.proposals.length > 0 && (
        <div className="mt-5 space-y-5">
          <p className="text-xs text-mist-500">
            {data.ai_tailored
              ? 'AI-tailored targets, anchored on your exact averages.'
              : 'Rule-based targets rounded from your exact averages.'}
          </p>

          <div className="space-y-2">
            {flexible.map((p) => (
              <ProposalRow
                key={p.category_id}
                proposal={p}
                amount={amounts[p.category_id] ?? ''}
                checked={checked[p.category_id] ?? false}
                onAmount={(v) =>
                  setAmounts((m) => ({ ...m, [p.category_id]: v }))
                }
                onToggle={(v) =>
                  setChecked((m) => ({ ...m, [p.category_id]: v }))
                }
              />
            ))}
          </div>

          {fixed.length > 0 && (
            <div className="space-y-2">
              <p className="text-xs text-mist-400">
                Fixed costs — usually budgeted at their actual amount.
              </p>
              {fixed.map((p) => (
                <ProposalRow
                  key={p.category_id}
                  proposal={p}
                  amount={amounts[p.category_id] ?? ''}
                  checked={checked[p.category_id] ?? false}
                  onAmount={(v) =>
                    setAmounts((m) => ({ ...m, [p.category_id]: v }))
                  }
                  onToggle={(v) =>
                    setChecked((m) => ({ ...m, [p.category_id]: v }))
                  }
                />
              ))}
            </div>
          )}

          <div className="flex flex-wrap items-center gap-3">
            <button
              className="btn-primary px-4 py-2 text-sm"
              disabled={selectedRows.length === 0 || apply.isPending}
              onClick={() => apply.mutate(selectedRows)}
            >
              {apply.isPending
                ? 'Applying…'
                : `Apply selected (${selectedRows.length})`}
            </button>
            <button
              className="btn-ghost px-3 py-2 text-sm text-mist-300"
              onClick={() => suggest.reset()}
            >
              Cancel
            </button>
            {apply.isError && (
              <span role="alert" className="text-sm text-ember-400">
                {apply.error.message}
              </span>
            )}
          </div>
        </div>
      )}
    </section>
  )
}

function ProposalRow({
  proposal,
  amount,
  checked,
  onAmount,
  onToggle,
}: {
  proposal: BudgetProposal
  amount: string
  checked: boolean
  onAmount: (value: string) => void
  onToggle: (value: boolean) => void
}) {
  return (
    <div className="rounded-xl border border-white/5 p-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <label className="flex items-center gap-2 font-medium">
          <input
            type="checkbox"
            className="h-4 w-4 accent-arcane-500"
            checked={checked}
            onChange={(e) => onToggle(e.target.checked)}
          />
          {proposal.category_name}
          {proposal.already_budgeted && (
            <span className="text-xs font-normal text-mist-500">
              now {formatMoney(proposal.current_budget)}
            </span>
          )}
        </label>

        <div className="flex items-center gap-1">
          <span className="text-mist-500">$</span>
          <input
            className="field w-28"
            type="number"
            min="0"
            step="0.01"
            value={amount}
            onChange={(e) => onAmount(e.target.value)}
            aria-label={`Budget amount for ${proposal.category_name}`}
          />
        </div>
      </div>
      <p className="mt-2 text-sm text-mist-400">{proposal.rationale}</p>
    </div>
  )
}

function AddBudget({
  categories,
  loading,
  onAdd,
  saving,
}: {
  categories: Category[]
  loading: boolean
  onAdd: (
    categoryID: string,
    amount: string,
    period: 'weekly' | 'monthly' | 'yearly',
    rollover: boolean,
  ) => void
  saving: boolean
}) {
  const [categoryID, setCategoryID] = useState('')
  const [amount, setAmount] = useState('')
  const [period, setPeriod] = useState<'weekly' | 'monthly' | 'yearly'>('monthly')
  const [rollover, setRollover] = useState(false)
  // Percentage mode lets you size a monthly budget as a share of typical income
  // (the input convenience for percentage/zero-based budgeting); the stored value
  // is still the resulting dollar amount.
  const [percentMode, setPercentMode] = useState(false)
  const [percent, setPercent] = useState('')

  const income = useQuery({ queryKey: ['safe-to-spend'], queryFn: api.safeToSpend })
  const monthlyIncome = Number(income.data?.expected_income ?? 0)

  // The dollar amount that will be saved: the percent of income in percent mode,
  // otherwise the typed amount.
  const percentAmount =
    monthlyIncome > 0 && Number(percent) > 0
      ? ((monthlyIncome * Number(percent)) / 100).toFixed(2)
      : ''
  const effectiveAmount = percentMode ? percentAmount : amount
  const usingPercent = percentMode && period === 'monthly'

  const canAdd = categoryID !== '' && Number(effectiveAmount) > 0

  const submit = () => {
    if (!canAdd) return
    onAdd(categoryID, effectiveAmount, period, period === 'monthly' ? rollover : false)
    setCategoryID('')
    setAmount('')
    setPercent('')
    setPercentMode(false)
    setPeriod('monthly')
    setRollover(false)
  }

  return (
    <section className="glass p-6">
      <h2 className="mb-1 text-lg font-medium">Add a budget</h2>
      <p className="mb-5 text-sm text-mist-300">
        Set a weekly, monthly, or yearly target for a spending category.
      </p>

      {loading ? (
        <Loading />
      ) : categories.length === 0 ? (
        <Empty>Every spending category already has a budget.</Empty>
      ) : (
        <div className="flex flex-wrap items-end gap-3">
          <div>
            <label className="label" htmlFor="add-category">
              Category
            </label>
            <select
              id="add-category"
              className="field"
              value={categoryID}
              onChange={(e) => setCategoryID(e.target.value)}
            >
              <option value="">Choose a category…</option>
              {categories.map((c) => (
                <option key={c.id} value={c.id}>
                  {c.name}
                </option>
              ))}
            </select>
          </div>

          <div>
            <label className="label" htmlFor="add-amount">
              {usingPercent ? '% of income' : 'Amount'}
            </label>
            {usingPercent ? (
              <div className="flex items-center gap-1">
                <input
                  id="add-amount"
                  className="field w-24"
                  type="number"
                  min="0"
                  max="100"
                  step="1"
                  placeholder="15"
                  value={percent}
                  onChange={(e) => setPercent(e.target.value)}
                />
                <span className="text-sm text-mist-400">
                  {percentAmount ? `= ${formatMoney(percentAmount)}` : '%'}
                </span>
              </div>
            ) : (
              <input
                id="add-amount"
                className="field w-40"
                type="number"
                min="0"
                step="0.01"
                placeholder="450.00"
                value={amount}
                onChange={(e) => setAmount(e.target.value)}
              />
            )}
            {period === 'monthly' && (
              <label className="mt-1 flex items-center gap-1.5 text-xs text-mist-500">
                <input
                  type="checkbox"
                  checked={percentMode}
                  onChange={(e) => setPercentMode(e.target.checked)}
                  disabled={monthlyIncome <= 0}
                />
                as % of income
              </label>
            )}
          </div>

          <div>
            <label className="label" htmlFor="add-period">
              Period
            </label>
            <select
              id="add-period"
              className="field"
              value={period}
              onChange={(e) =>
                setPeriod(e.target.value as 'weekly' | 'monthly' | 'yearly')
              }
            >
              <option value="weekly">Weekly</option>
              <option value="monthly">Monthly</option>
              <option value="yearly">Yearly</option>
            </select>
          </div>

          {period === 'monthly' && (
            <label className="flex items-center gap-1.5 pb-2 text-sm text-mist-300">
              <input
                type="checkbox"
                checked={rollover}
                onChange={(e) => setRollover(e.target.checked)}
              />
              Roll over unspent
            </label>
          )}

          <button
            className="btn-primary px-4 py-2 text-sm"
            disabled={!canAdd || saving}
            onClick={submit}
          >
            Add budget
          </button>
        </div>
      )}
    </section>
  )
}

function Tile({
  label,
  value,
  tone,
}: {
  label: string
  value: string
  tone?: 'good' | 'critical'
}) {
  const color =
    tone === 'critical' ? STATUS.critical : tone === 'good' ? STATUS.good : undefined

  return (
    <div className="glass p-5">
      <p className="text-sm text-mist-300">{label}</p>
      <p
        className="tabular mt-2 text-3xl font-semibold"
        style={{ color: color ?? '#f2d492' }}
      >
        {value}
      </p>
    </div>
  )
}

function Loading() {
  return <p className="py-8 text-center text-sm text-mist-500">Loading…</p>
}

function Empty({ children }: { children: ReactNode }) {
  return <p className="py-8 text-center text-sm text-mist-500">{children}</p>
}
