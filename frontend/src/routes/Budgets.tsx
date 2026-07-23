import { useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../lib/api'
import type { BudgetProgress, BudgetProposal, Category } from '../lib/api'
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
    mutationFn: ({ categoryID, amount }: { categoryID: string; amount: string }) =>
      api.setBudget(categoryID, amount),
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
                onSave={(amount) =>
                  setBudget.mutate({ categoryID: b.category_id, amount })
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
        onAdd={(categoryID, amount) => setBudget.mutate({ categoryID, amount })}
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

function BudgetRow({
  budget,
  onSave,
  onDelete,
  saving,
  deleting,
}: {
  budget: BudgetProgress
  onSave: (amount: string) => void
  onDelete: () => void
  saving: boolean
  deleting: boolean
}) {
  const [editing, setEditing] = useState(false)
  const [amount, setAmount] = useState(budget.budgeted)

  const budgetedNum = Number(budget.budgeted)
  const spentNum = Number(budget.spent)
  // Display-only percentage: divide two server-exact figures for a bar width.
  // Guard the zero-budget case so it never renders NaN.
  const pct = budgetedNum > 0 ? (spentNum / budgetedNum) * 100 : 0
  const over = spentNum > budgetedNum
  const fill = over ? STATUS.critical : STATUS.good

  const save = () => {
    onSave(amount)
    setEditing(false)
  }

  return (
    <div className="rounded-xl border border-white/5 p-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <span className="flex items-center gap-2 font-medium">
          <span
            className="inline-block h-2 w-2 shrink-0 rounded-full"
            style={{ backgroundColor: budget.color ?? STATUS.good }}
          />
          {budget.name}
        </span>

        {editing ? (
          <div className="flex items-center gap-2">
            <input
              className="field w-28"
              type="number"
              min="0"
              step="0.01"
              value={amount}
              onChange={(e) => setAmount(e.target.value)}
              aria-label={`Budget amount for ${budget.name}`}
            />
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
                setEditing(false)
              }}
            >
              Cancel
            </button>
          </div>
        ) : (
          <div className="flex items-center gap-3">
            <span className="tabular text-sm text-mist-300">
              {formatMoney(budget.spent)} of {formatMoney(budget.budgeted)}
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
            ? `${formatMoney(String(spentNum - budgetedNum))} over`
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
  onAdd: (categoryID: string, amount: string) => void
  saving: boolean
}) {
  const [categoryID, setCategoryID] = useState('')
  const [amount, setAmount] = useState('')

  const canAdd = categoryID !== '' && amount !== '' && Number(amount) > 0

  const submit = () => {
    if (!canAdd) return
    onAdd(categoryID, amount)
    setCategoryID('')
    setAmount('')
  }

  return (
    <section className="glass p-6">
      <h2 className="mb-1 text-lg font-medium">Add a budget</h2>
      <p className="mb-5 text-sm text-mist-300">
        Set a monthly target for a spending category.
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
              Monthly amount
            </label>
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
          </div>

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
