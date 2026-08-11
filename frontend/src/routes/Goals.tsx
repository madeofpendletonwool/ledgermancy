import { useState } from 'react'
import type { ReactNode } from 'react'
import { Link } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../lib/api'
import type {
  Goal,
  GoalInput,
  GoalKind,
  GoalPayoff,
  GoalProposal,
  Liability,
} from '../lib/api'
import { formatDate, formatMoney, isAmortizingDebt, isLiability } from '../lib/money'
import { AttachDocuments } from '../components/AttachDocuments'
import { PayoffScheduleChart } from '../components/charts/PayoffScheduleChart'
import { AnimatedNumber } from '../components/motion'
import { SkeletonRows, Reveal } from '../components/Skeleton'
import { EmptyState } from '../components/EmptyState'
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
          What you're saving toward and what you're paying off, and whether
          you're on track to get there.
        </p>
      </div>

      {capabilities.data?.ai_enabled && <NLGoalParser />}

      <section className="glass p-6">
        <h2 className="mb-1 text-lg font-medium">Your goals</h2>
        <p className="mb-5 text-sm text-mist-300">
          Progress updates automatically from your balances and cashflow.
        </p>

        {goals.isPending ? (
          <SkeletonRows count={3} />
        ) : rows.length === 0 ? (
          <EmptyState
            title="No goals yet"
            icon={<GoalGlyph />}
            action={
              <a href="#add-goal" className="btn-primary">
                Add a goal
              </a>
            }
          >
            Track a savings target or a debt payoff here. Progress updates from
            your balances and cashflow.
          </EmptyState>
        ) : (
          <Reveal>
            <div className="space-y-3">
              {rows.map((g) => (
                <GoalCard key={g.id} goal={g} />
              ))}
            </div>
          </Reveal>
        )}
      </section>

      <CreateGoal />
    </div>
  )
}

function statusChip(goal: Goal): { label: string; tone: 'good' | 'critical' | 'muted' } {
  if (goal.achieved) return { label: goal.kind === 'debt_payoff' ? 'Paid off' : 'Achieved', tone: 'good' }
  // A debt that never retires outranks every other status: it is the one thing
  // on this page a user must not scroll past.
  if (goal.payoff?.available && goal.payoff.never_pays_off) {
    return { label: 'Never paid off', tone: 'critical' }
  }
  // Nothing is known about the schedule, so claiming either status would be a
  // guess. The card explains itself below.
  if (goal.payoff && !goal.payoff.available) return { label: 'No terms', tone: 'muted' }
  if (goal.open_ended) return { label: 'Open-ended', tone: 'muted' }
  if (goal.on_track) return { label: 'On track', tone: 'good' }
  return { label: `${formatMoney(goal.shortfall)}/mo short`, tone: 'critical' }
}

function GoalCard({ goal }: { goal: Goal }) {
  const qc = useQueryClient()
  const [showFunding, setShowFunding] = useState(false)
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
            {goal.scope === 'person' && (
              <span className="ml-2 text-xs font-normal text-mist-500">
                someone's own
              </span>
            )}
          </p>
          <p className="mt-0.5 text-sm text-mist-400 tabular">
            {goal.kind === 'debt_payoff' ? (
              <>
                <AnimatedNumber value={goal.current_amount} /> of{' '}
                <AnimatedNumber value={goal.target_amount} /> paid off
              </>
            ) : (
              <>
                <AnimatedNumber value={goal.current_amount} /> of{' '}
                <AnimatedNumber value={goal.target_amount} />
                {goal.kind === 'college' &&
                  ` — one year of ${goal.college_years}, in today's dollars`}
              </>
            )}
            {goal.target_date && ` · by ${formatDate(goal.target_date)}`}
          </p>
          {/* The target above is ONE year, so the bar reads as progress toward
              one year. Saying so beside it is the difference between a figure a
              parent can use and one they will misread as the whole degree. */}
          {goal.college_basis && (
            <p className="mt-1 text-xs text-mist-500">{goal.college_basis}</p>
          )}
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
          {goal.scope === 'household' && (
            <button
              className="btn-ghost px-2.5 py-1 text-xs"
              onClick={() => setShowFunding((v) => !v)}
            >
              {showFunding ? 'Hide funding' : 'Who funded it'}
            </button>
          )}
          {/* A quote, a contract or a policy behind the goal it justifies. */}
          <AttachDocuments target={{ kind: 'goal', id: goal.id }} />
          <RemindGoalToggle goal={goal} />
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

      {goal.payoff && !goal.achieved && <PayoffDetail payoff={goal.payoff} />}

      {showFunding && <GoalFunding goal={goal} />}
    </div>
  )
}

/**
 * The per-goal reminders opt-out (MAD-85). On by default; off silences the
 * behind-schedule coaching for this one goal. Rendered inline beside the goal's
 * other actions because it is a single boolean with no consequence worth a
 * dialog of its own.
 */
function RemindGoalToggle({ goal }: { goal: Goal }) {
  const qc = useQueryClient()
  const save = useMutation({
    mutationFn: (remind: boolean) => api.setGoalRemind(goal.id, remind),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['goals'] }),
  })
  return (
    <label className="flex items-center gap-1.5 text-xs text-mist-400" title="Coach this goal in the reminders feed">
      <input
        type="checkbox"
        className="accent-arcane-500"
        checked={goal.remind}
        disabled={save.isPending}
        onChange={(e) => save.mutate(e.target.checked)}
      />
      Remind
    </label>
  )
}

/**
 * The payoff schedule for a debt goal: when it ends, what the interest costs,
 * and what it would take to hit the deadline.
 *
 * Every figure arrives finished from the server. Nothing here divides a balance
 * by a number of months — amortization is not something to approximate in a
 * browser, and a wrong payoff date is worse than none.
 */
function PayoffDetail({ payoff }: { payoff: GoalPayoff }) {
  if (!payoff.available) {
    return (
      <div className="mt-3 rounded-lg bg-white/5 px-3 py-2 text-xs text-mist-400">
        {payoff.reason}{' '}
        <Link to="/accounts" className="underline">
          Go to Accounts
        </Link>
        .
        {payoff.target_reachable && (
          <>
            {' '}
            To clear {formatMoney(payoff.balance)} by your target date you'd need{' '}
            <span className="font-medium text-mist-200">
              {formatMoney(payoff.required_monthly)}
            </span>{' '}
            a month.
          </>
        )}
      </div>
    )
  }

  // THE sentence this feature exists to say. Someone paying less than their
  // interest is not "behind schedule" — they have no schedule, and the balance
  // grows every month they keep it up.
  if (payoff.never_pays_off) {
    return (
      <div
        className="mt-3 rounded-lg border px-3 py-2.5 text-sm"
        style={{
          borderColor: `${STATUS.critical}55`,
          backgroundColor: `${STATUS.critical}14`,
          color: STATUS.critical,
        }}
      >
        <p className="font-medium">
          At {formatMoney(payoff.monthly_payment)}/mo this debt is never paid off
          — the interest alone is {formatMoney(payoff.monthly_interest)}/mo.
        </p>
        <p className="mt-1 text-xs text-mist-300">
          {formatMoney(payoff.balance)} at {payoff.apr}%{' '}
          {isAmortizingDebt(payoff.account_type) ? 'interest' : 'APR'}. Anything
          at or below the monthly interest leaves the balance where it is or
          higher.
          {payoff.target_reachable && (
            <>
              {' '}
              Clearing it by your target date takes{' '}
              {formatMoney(payoff.required_monthly)} a month.
            </>
          )}
        </p>
      </div>
    )
  }

  return (
    <div className="mt-3 space-y-3">
      {payoff.schedule && payoff.schedule.length > 0 && (
        <div className="rounded-lg bg-white/5 px-3 py-3">
          <PayoffScheduleChart
            schedule={payoff.schedule}
            startBalance={payoff.balance}
            monthlyPayment={payoff.monthly_payment}
          />
        </div>
      )}
      <div className="grid gap-x-6 gap-y-1.5 rounded-lg bg-white/5 px-3 py-2.5 text-xs sm:grid-cols-2">
        <PayoffFact label="Paid off">
          {payoff.payoff_date ? formatDate(payoff.payoff_date) : '—'}
          <span className="ml-1.5 text-mist-500">
            ({payoff.months} {payoff.months === 1 ? 'payment' : 'payments'})
          </span>
        </PayoffFact>
        <PayoffFact label="Interest to come">
          {formatMoney(payoff.total_interest)}
        </PayoffFact>
        <PayoffFact label="Paying">
          {formatMoney(payoff.monthly_payment)}/mo
          {payoff.apr_source === ''
            ? ''
            : payoff.apr_source === 'manual'
              ? ` at the ${payoff.apr}% you entered`
              : ` at ${payoff.apr}% ${
                  isAmortizingDebt(payoff.account_type) ? 'interest' : 'APR'
                }`}
        </PayoffFact>
        {payoff.target_reachable && (
          <PayoffFact label="To hit your date">
            {formatMoney(payoff.required_monthly)}/mo
          </PayoffFact>
        )}
        {/* A schedule with no rate is arithmetically sound but optimistic — every
            date above is the earliest possible one. Saying so is the difference
            between a floor and a promise. */}
        {payoff.apr_source === '' && (
          <p className="text-mist-500 sm:col-span-2">
            No rate on file, so this assumes the debt is interest-free — the real
            payoff date is later.{' '}
            <Link to="/accounts" className="underline">
              Add the APR
            </Link>{' '}
            for a true schedule.
          </p>
        )}
      </div>
    </div>
  )
}

function PayoffFact({ label, children }: { label: string; children: ReactNode }) {
  return (
    <p className="flex items-baseline justify-between gap-3">
      <span className="text-mist-500">{label}</span>
      <span className="text-right tabular text-mist-200">{children}</span>
    </p>
  )
}

/**
 * Who funded a shared goal.
 *
 * ATTRIBUTION, not progress. The bar above still shows the goal's real
 * standing — for an account-linked goal that is the account balance, whether or
 * not anybody logged a contribution here. This panel answers a different
 * question ("who put money in") and says so, because summing contributions into
 * progress would create a second figure that drifts from the first the moment
 * someone forgets to log one.
 */
function GoalFunding({ goal }: { goal: Goal }) {
  const qc = useQueryClient()
  const funding = useQuery({
    queryKey: ['goal-contributions', goal.id],
    queryFn: () => api.goalContributions(goal.id),
  })

  const [amount, setAmount] = useState('')

  const add = useMutation({
    mutationFn: () => api.addGoalContribution(goal.id, { amount }),
    onSuccess: () => {
      setAmount('')
      qc.invalidateQueries({ queryKey: ['goal-contributions', goal.id] })
    },
  })

  return (
    <div className="mt-4 rounded-xl bg-white/5 p-4">
      <div className="flex items-baseline justify-between gap-3">
        <h4 className="text-sm font-medium text-mist-300">Funded by</h4>
        <span className="text-xs text-mist-500">
          {formatMoney(funding.data?.total)} logged
        </span>
      </div>

      {funding.data?.contributors.length === 0 && (
        <p className="mt-2 text-xs text-mist-500">
          Nothing logged yet. Progress above still comes from the goal itself.
        </p>
      )}

      <ul className="mt-3 space-y-2">
        {funding.data?.contributors.map((c) => (
          <li key={c.person_id} className="flex items-center gap-3 text-sm">
            <span className="w-24 shrink-0 truncate">{c.person_name}</span>
            <div className="h-1.5 flex-1 overflow-hidden rounded-full bg-white/5">
              <div
                className="h-full rounded-full bg-arcane-500"
                style={{ width: `${c.share_pct}%` }}
              />
            </div>
            <span className="w-24 shrink-0 text-right tabular text-mist-300">
              {formatMoney(c.total)}
            </span>
          </li>
        ))}
      </ul>

      <form
        className="mt-4 flex gap-2"
        onSubmit={(e) => {
          e.preventDefault()
          add.mutate()
        }}
      >
        <input
          required
          className="field flex-1"
          placeholder="I put in…"
          inputMode="decimal"
          value={amount}
          onChange={(e) => setAmount(e.target.value)}
        />
        <button className="btn-ghost shrink-0" disabled={add.isPending}>
          {add.isPending ? 'Logging…' : 'Log'}
        </button>
      </form>

      {add.isError && (
        <p role="alert" className="mt-2 text-xs text-ember-400">
          {add.error.message}
        </p>
      )}

      <p className="mt-3 text-xs text-mist-500">
        This records who contributed. It doesn't move the progress bar — that
        comes from the goal's own balance.
      </p>
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

/**
 * useDebtAccounts is the account list a payoff goal may link to: credit cards
 * and loans with something owed on them.
 *
 * It filters on account TYPE, matching the server's gate in resolvePayoffTarget
 * exactly. It used to filter on "has a liabilities row" — i.e. "did Plaid serve
 * loan terms for this account?" — which is a different question with a different
 * answer. Plaid supports its Liabilities product at a minority of institutions,
 * so for a household with a mortgage and two credit cards the answer was no
 * three times and this picker was empty.
 *
 * The balance check mirrors the server's too: an account with nothing owed is
 * rejected on submit, so offering it would be a trap.
 *
 * Terms are fetched alongside, to hint when a chosen debt has none, but they
 * never decide what is offered — and `isPending` deliberately ignores that query
 * so the picker renders as soon as the accounts arrive.
 */
function useDebtAccounts() {
  const accounts = useQuery({ queryKey: ['accounts'], queryFn: api.accounts })
  const liabilities = useQuery({ queryKey: ['liabilities'], queryFn: api.liabilities })

  const all = accounts.data ?? []
  const termsByAccount = new Map<string, Liability>(
    (liabilities.data ?? []).map((l) => [l.account_id, l]),
  )
  return {
    accounts: all,
    debts: all.filter(
      (a) => isLiability(a.type) && Number(a.current_balance ?? 0) > 0,
    ),
    // Separated from `debts` so the empty state can tell "you have no debts" from
    // "your debts are all paid off" — two very different things to be told.
    hasDebtAccounts: all.some((a) => isLiability(a.type)),
    termsByAccount,
    isPending: accounts.isPending,
  }
}

function CreateGoal() {
  const {
    accounts,
    debts,
    hasDebtAccounts,
    termsByAccount,
    isPending: accountsPending,
  } = useDebtAccounts()
  const categories = useQuery({ queryKey: ['categories'], queryFn: api.categories })

  const [kind, setKind] = useState<GoalKind>('savings')
  const [name, setName] = useState('')
  const [amount, setAmount] = useState('')
  const [date, setDate] = useState('')
  const [accountID, setAccountID] = useState('')
  const [categoryID, setCategoryID] = useState('')
  const [personal, setPersonal] = useState(false)
  // A goal that belongs to someone else in the household — a child's bike fund
  // is the case this exists for. Mutually exclusive with "just for me".
  const [forPerson, setForPerson] = useState('')
  const people = useQuery({ queryKey: ['people'], queryFn: api.people })

  const isPayoff = kind === 'debt_payoff'
  const isCollege = kind === 'college'
  const [collegeYears, setCollegeYears] = useState('4')

  const create = useCreateGoal(() => {
    setName('')
    setAmount('')
    setDate('')
    setAccountID('')
    setCategoryID('')
    setPersonal(false)
  })

  // A payoff goal needs the debt, not an amount: the balance to eliminate is
  // captured server-side from the account so it can never disagree with it.
  const canAdd =
    name.trim() !== '' &&
    (isPayoff ? accountID !== '' : amount !== '' && Number(amount) > 0)

  const submit = () => {
    if (!canAdd) return
    create.mutate({
      name: name.trim(),
      kind,
      target_amount: isPayoff ? undefined : amount,
      target_date: date || undefined,
      college_years: isCollege ? Number(collegeYears) : undefined,
      scope: forPerson ? 'person' : personal ? 'user' : 'household',
      person_id: forPerson || undefined,
      account_id: accountID || null,
      category_id: isPayoff ? null : categoryID || null,
    })
  }

  return (
    <section id="add-goal" className="glass scroll-mt-24 p-6">
      <h2 className="mb-1 text-lg font-medium">Add a goal</h2>
      <p className="mb-5 text-sm text-mist-300">
        {isPayoff
          ? 'Pick the debt. The balance to clear, the rate and the payment come from the account, and the payoff date is worked out from them.'
          : isCollege
            ? 'Enter ONE YEAR of cost in today’s dollars and link the 529. Each year is inflated separately and drawn down on the Advisor page, which is where the per-year shortfall lives — a single lump target cannot say “funded through sophomore year”.'
            : 'Set a target. Link an account to track progress by its balance, or leave it unlinked to track your accumulated surplus.'}
      </p>

      <div className="grid gap-4 sm:grid-cols-2">
        <div>
          <label className="label" htmlFor="goal-kind">
            Goal type
          </label>
          <select
            id="goal-kind"
            className="field w-full"
            value={kind}
            onChange={(e) => {
              const next = e.target.value as GoalKind
              setKind(next)
              // The two kinds mean different things by "account", so a selection
              // made under one must not carry into the other.
              setAccountID('')
            }}
          >
            <option value="savings">Save toward something</option>
            <option value="debt_payoff">Pay off a debt</option>
            <option value="college">Fund college</option>
          </select>
        </div>

        {isCollege && (
          <div>
            <label className="label" htmlFor="goal-college-years">
              Years of study
            </label>
            <input
              id="goal-college-years"
              className="field w-full"
              type="number"
              min="1"
              max="10"
              value={collegeYears}
              onChange={(e) => setCollegeYears(e.target.value)}
            />
            <p className="mt-1 text-xs text-mist-500">
              Four is the usual answer, not the only one — transfers and
              five-year programmes exist.
            </p>
          </div>
        )}

        <div>
          <label className="label" htmlFor="goal-name">
            Name
          </label>
          <input
            id="goal-name"
            className="field w-full"
            placeholder={isPayoff ? 'Clear the credit card' : 'Trip to Japan'}
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
        </div>

        {isPayoff ? (
          <div>
            <label className="label" htmlFor="goal-debt">
              Debt to pay off
            </label>
            <select
              id="goal-debt"
              className="field w-full"
              value={accountID}
              onChange={(e) => setAccountID(e.target.value)}
            >
              <option value="">Choose a debt…</option>
              {debts.map((a) => (
                <option key={a.id} value={a.id}>
                  {a.name}
                  {a.mask ? ` ••${a.mask}` : ''}
                </option>
              ))}
            </select>
            {/* Two distinct empty states. "You have no debts" and "your debts
                are all cleared" want opposite reactions from the reader, and
                showing the first to somebody who just paid off their card reads
                as the app having lost track of it. */}
            {!accountsPending && debts.length === 0 && (
              <p className="mt-1.5 text-xs text-mist-500">
                {hasDebtAccounts
                  ? 'Your credit cards and loans are all at a zero balance — nothing to pay off.'
                  : 'No linked credit cards or loans yet — connect one to track a payoff.'}
              </p>
            )}
            {/* Most banks report no rate, so this is the ordinary case rather
                than an error. Say what it costs and where to fix it, and make
                clear the goal still works without it. */}
            {accountID && termsByAccount.get(accountID)?.apr_source === '' && (
              <p className="mt-1.5 text-xs text-mist-500">
                No rate on file for this one —{' '}
                <Link to="/accounts" className="underline">
                  add it on Accounts
                </Link>{' '}
                to get a payoff date and interest total. You can still set the
                goal without it.
              </p>
            )}
          </div>
        ) : (
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
        )}

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

        {!isPayoff && (
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
              {accounts.map((a) => (
                <option key={a.id} value={a.id}>
                  {a.name}
                  {a.mask ? ` ••${a.mask}` : ''}
                </option>
              ))}
            </select>
          </div>
        )}

        {!isPayoff && (
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
        )}

        <div>
          <label className="label" htmlFor="goal-person">
            Whose goal
          </label>
          <select
            id="goal-person"
            className="field"
            value={forPerson}
            onChange={(e) => {
              setForPerson(e.target.value)
              if (e.target.value) setPersonal(false)
            }}
          >
            <option value="">The household</option>
            {people.data?.map((p) => (
              <option key={p.id} value={p.id}>
                {p.display_name}
              </option>
            ))}
          </select>
        </div>

        <label className="flex items-end gap-2 pb-2 text-sm text-mist-300">
          <input
            type="checkbox"
            className="h-4 w-4 accent-arcane-500"
            checked={personal}
            disabled={!!forPerson}
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
    const isPayoff = proposal.kind === 'debt_payoff'
    create.mutate({
      name: proposal.name,
      kind: proposal.kind,
      // A payoff proposal carries no amount: the server captures the linked
      // account's balance, the same as the form does.
      target_amount: isPayoff ? undefined : proposal.target_amount,
      target_date: proposal.target_date ?? undefined,
      account_id: proposal.account_id,
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
        Say it in your own words — “save $10k for a trip by December”, or “pay
        off my credit card by December”.
      </p>

      <div className="flex flex-wrap items-end gap-3">
        <input
          className="field min-w-0 flex-1"
          placeholder="Describe a goal…"
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
          <p className="text-sm font-medium text-mist-100">
            {proposal.kind === 'debt_payoff' ? 'A debt to pay off' : 'A goal to save'}
          </p>
          <p className="mt-1 text-sm text-mist-300">
            <span className="font-medium">{proposal.name}</span> —{' '}
            {proposal.kind === 'debt_payoff'
              ? proposal.account_name
              : formatMoney(proposal.target_amount)}
            {proposal.target_date && ` by ${formatDate(proposal.target_date)}`}
          </p>
          {proposal.kind === 'debt_payoff' && (
            <p className="mt-1 text-xs text-mist-500">
              The balance to clear is read from that account when you confirm.
            </p>
          )}
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

/** Outline glyph for the goals empty state. */
function GoalGlyph() {
  return (
    <svg
      className="h-5 w-5"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={2}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d="M12 2v4M12 18v4M2 12h4M18 12h4" />
      <circle cx="12" cy="12" r="4" />
    </svg>
  )
}
