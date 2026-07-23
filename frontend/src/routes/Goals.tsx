import { useState } from 'react'
import type { ReactNode } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../lib/api'
import type { Goal, GoalInput, GoalProposal } from '../lib/api'
import { formatDate, formatMoney } from '../lib/money'
import { STATUS } from '../components/charts/tokens'

export function Goals() {
  const goals = useQuery({ queryKey: ['goals'], queryFn: api.goals })
  const capabilities = useQuery({
    queryKey: ['capabilities'],
    queryFn: api.capabilities,
    staleTime: Infinity,
  })

  const rows = goals.data ?? []

  return (
    <div className="space-y-8">
      <div>
        <h1 className="text-2xl font-semibold">Goals</h1>
        <p className="mt-1 text-mist-300">
          What you're saving toward, and whether you're on track to get there.
        </p>
      </div>

      {capabilities.data?.ai_enabled && <NLGoalParser />}

      <section className="glass p-6">
        <h2 className="mb-1 text-lg font-medium">Your goals</h2>
        <p className="mb-5 text-sm text-mist-300">
          Progress updates automatically from your balances and cashflow.
        </p>

        {goals.isPending ? (
          <Loading />
        ) : rows.length === 0 ? (
          <Empty>No goals yet. Add one below to start tracking a target.</Empty>
        ) : (
          <div className="space-y-3">
            {rows.map((g) => (
              <GoalCard key={g.id} goal={g} />
            ))}
          </div>
        )}
      </section>

      <CreateGoal />
    </div>
  )
}

function statusChip(goal: Goal): { label: string; tone: 'good' | 'critical' | 'muted' } {
  if (goal.achieved) return { label: 'Achieved', tone: 'good' }
  if (goal.open_ended) return { label: 'Open-ended', tone: 'muted' }
  if (goal.on_track) return { label: 'On track', tone: 'good' }
  return { label: `${formatMoney(goal.shortfall)}/mo short`, tone: 'critical' }
}

function GoalCard({ goal }: { goal: Goal }) {
  const qc = useQueryClient()
  const archive = useMutation({
    mutationFn: () => api.archiveGoal(goal.id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['goals'] }),
  })

  const targetNum = Number(goal.target_amount)
  const currentNum = Number(goal.current_amount)
  // Display-only percentage: two server-exact figures for a bar width.
  const pct = targetNum > 0 ? Math.min((currentNum / targetNum) * 100, 100) : 0
  const chip = statusChip(goal)
  const chipColor =
    chip.tone === 'critical'
      ? STATUS.critical
      : chip.tone === 'good'
        ? STATUS.good
        : undefined

  return (
    <div className="rounded-xl border border-white/5 p-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <p className="font-medium">
            {goal.name}
            {goal.scope === 'user' && (
              <span className="ml-2 text-xs font-normal text-mist-500">
                personal
              </span>
            )}
          </p>
          <p className="mt-0.5 text-sm text-mist-400">
            {formatMoney(goal.current_amount)} of {formatMoney(goal.target_amount)}
            {goal.target_date && ` · by ${formatDate(goal.target_date)}`}
          </p>
        </div>
        <div className="flex items-center gap-3">
          <span
            className="rounded-full px-2.5 py-1 text-xs font-medium"
            style={{
              color: chipColor ?? '#a8b0c0',
              backgroundColor: chipColor ? `${chipColor}1a` : 'rgba(255,255,255,0.05)',
            }}
          >
            {chip.label}
          </span>
          <button
            className="btn-ghost px-2.5 py-1 text-xs text-ember-400"
            disabled={archive.isPending}
            onClick={() => archive.mutate()}
          >
            Archive
          </button>
        </div>
      </div>

      <div className="mt-3 flex items-center gap-3">
        <div className="h-1.5 flex-1 overflow-hidden rounded-full bg-white/5">
          <div
            className="h-full rounded-full"
            style={{ width: `${pct}%`, backgroundColor: chipColor ?? STATUS.good }}
          />
        </div>
        {!goal.open_ended && !goal.achieved && (
          <span className="tabular w-40 text-right text-xs text-mist-400">
            need {formatMoney(goal.required_monthly)}/mo · {goal.months_left}mo left
          </span>
        )}
      </div>
    </div>
  )
}

// useCreateGoal wraps the create mutation and refreshes the list on success.
function useCreateGoal(onDone?: () => void) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (input: GoalInput) => api.createGoal(input),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['goals'] })
      onDone?.()
    },
  })
}

function CreateGoal() {
  const accounts = useQuery({ queryKey: ['accounts'], queryFn: api.accounts })
  const categories = useQuery({ queryKey: ['categories'], queryFn: api.categories })

  const [name, setName] = useState('')
  const [amount, setAmount] = useState('')
  const [date, setDate] = useState('')
  const [accountID, setAccountID] = useState('')
  const [categoryID, setCategoryID] = useState('')
  const [personal, setPersonal] = useState(false)

  const create = useCreateGoal(() => {
    setName('')
    setAmount('')
    setDate('')
    setAccountID('')
    setCategoryID('')
    setPersonal(false)
  })

  const canAdd = name.trim() !== '' && amount !== '' && Number(amount) > 0

  const submit = () => {
    if (!canAdd) return
    create.mutate({
      name: name.trim(),
      target_amount: amount,
      target_date: date || undefined,
      scope: personal ? 'user' : 'household',
      account_id: accountID || null,
      category_id: categoryID || null,
    })
  }

  return (
    <section className="glass p-6">
      <h2 className="mb-1 text-lg font-medium">Add a goal</h2>
      <p className="mb-5 text-sm text-mist-300">
        Set a target. Link an account to track progress by its balance, or leave
        it unlinked to track your accumulated surplus.
      </p>

      <div className="grid gap-4 sm:grid-cols-2">
        <div>
          <label className="label" htmlFor="goal-name">
            Name
          </label>
          <input
            id="goal-name"
            className="field w-full"
            placeholder="Trip to Japan"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
        </div>

        <div>
          <label className="label" htmlFor="goal-amount">
            Target amount
          </label>
          <input
            id="goal-amount"
            className="field w-full"
            type="number"
            min="0"
            step="0.01"
            placeholder="10000.00"
            value={amount}
            onChange={(e) => setAmount(e.target.value)}
          />
        </div>

        <div>
          <label className="label" htmlFor="goal-date">
            Target date (optional)
          </label>
          <input
            id="goal-date"
            className="field w-full"
            type="date"
            value={date}
            onChange={(e) => setDate(e.target.value)}
          />
        </div>

        <div>
          <label className="label" htmlFor="goal-account">
            Linked account (optional)
          </label>
          <select
            id="goal-account"
            className="field w-full"
            value={accountID}
            onChange={(e) => setAccountID(e.target.value)}
          >
            <option value="">Track accumulated surplus</option>
            {(accounts.data ?? []).map((a) => (
              <option key={a.id} value={a.id}>
                {a.name}
                {a.mask ? ` ••${a.mask}` : ''}
              </option>
            ))}
          </select>
        </div>

        <div>
          <label className="label" htmlFor="goal-category">
            Related category (optional)
          </label>
          <select
            id="goal-category"
            className="field w-full"
            value={categoryID}
            onChange={(e) => setCategoryID(e.target.value)}
          >
            <option value="">None</option>
            {(categories.data ?? [])
              .filter((c) => !c.is_income && !c.is_transfer)
              .map((c) => (
                <option key={c.id} value={c.id}>
                  {c.name}
                </option>
              ))}
          </select>
        </div>

        <label className="flex items-end gap-2 pb-2 text-sm text-mist-300">
          <input
            type="checkbox"
            className="h-4 w-4 accent-arcane-500"
            checked={personal}
            onChange={(e) => setPersonal(e.target.checked)}
          />
          Just for me (personal goal)
        </label>
      </div>

      <div className="mt-5 flex items-center gap-3">
        <button
          className="btn-primary px-4 py-2 text-sm"
          disabled={!canAdd || create.isPending}
          onClick={submit}
        >
          {create.isPending ? 'Saving…' : 'Add goal'}
        </button>
        {create.isError && (
          <span role="alert" className="text-sm text-ember-400">
            {create.error.message}
          </span>
        )}
      </div>
    </section>
  )
}

// NLGoalParser lets a user describe a goal in plain English. The parse is a
// proposal only — it renders a confirmation card and only on Confirm writes
// through the same createGoal path the form uses. Identical confirm-before-save
// UX to the alerts NL parser.
function NLGoalParser() {
  const [text, setText] = useState('')
  const [proposal, setProposal] = useState<GoalProposal | null>(null)

  const parse = useMutation({
    mutationFn: (t: string) => api.parseGoal(t),
    onSuccess: setProposal,
  })
  const create = useCreateGoal(() => {
    setProposal(null)
    setText('')
    parse.reset()
  })

  const confirm = () => {
    if (!proposal) return
    create.mutate({
      name: proposal.name,
      target_amount: proposal.target_amount,
      target_date: proposal.target_date ?? undefined,
    })
  }

  const clear = () => {
    setProposal(null)
    parse.reset()
    create.reset()
  }

  return (
    <section className="glass p-6">
      <h2 className="text-lg font-medium">Describe a goal</h2>
      <p className="mt-1 mb-4 text-sm text-mist-300">
        Say it in your own words — “save $10k for a trip by December”.
      </p>

      <div className="flex flex-wrap items-end gap-3">
        <input
          className="field min-w-0 flex-1"
          placeholder="Describe a savings goal…"
          value={text}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && text.trim()) parse.mutate(text)
          }}
        />
        <button
          className="btn-primary px-4 py-2 text-sm"
          disabled={parse.isPending || text.trim() === ''}
          onClick={() => parse.mutate(text)}
        >
          {parse.isPending ? 'Reading…' : 'Parse'}
        </button>
      </div>

      {parse.isError && (
        <p role="alert" className="mt-3 text-sm text-ember-400">
          {parse.error.message}
        </p>
      )}

      {proposal && (
        <div className="mt-5 rounded-xl border border-white/10 bg-white/5 p-4">
          <p className="text-sm font-medium text-mist-100">A goal to save</p>
          <p className="mt-1 text-sm text-mist-300">
            <span className="font-medium">{proposal.name}</span> —{' '}
            {formatMoney(proposal.target_amount)}
            {proposal.target_date && ` by ${formatDate(proposal.target_date)}`}
          </p>
          <div className="mt-4 flex flex-wrap items-center gap-2">
            <button
              className="btn-primary px-4 py-1.5 text-sm"
              disabled={create.isPending}
              onClick={confirm}
            >
              {create.isPending ? 'Saving…' : 'Confirm'}
            </button>
            <button
              className="btn-ghost px-3 py-1.5 text-sm text-mist-300"
              onClick={clear}
            >
              Cancel
            </button>
            {create.isError && (
              <span role="alert" className="text-sm text-ember-400">
                {create.error.message}
              </span>
            )}
          </div>
        </div>
      )}
    </section>
  )
}

function Loading() {
  return <p className="py-8 text-center text-sm text-mist-500">Loading…</p>
}

function Empty({ children }: { children: ReactNode }) {
  return <p className="py-8 text-center text-sm text-mist-500">{children}</p>
}
