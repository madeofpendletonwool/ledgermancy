import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../lib/api'
import type { Account, PiggyBank } from '../lib/api'
import { formatMoney, isLiability } from '../lib/money'
import { AnimatedNumber } from '../components/motion'
import { SkeletonRows, Reveal } from '../components/Skeleton'
import { EmptyState } from '../components/EmptyState'
import { STATUS } from '../components/charts/tokens'

/**
 * Piggy banks — lightweight, period-independent savings envelopes ("Car Repair
 * Fund", "Christmas 2026") that sit on an asset account and track a running
 * balance via deposit/withdraw events.
 *
 * THE THING THIS PAGE MUST NOT LET A USER BELIEVE: a piggy bank is an
 * ANNOTATION on part of an account balance, not a separate balance. The money
 * is already in the account (it arrived via a real transaction); depositing
 * here only earmarks a slice of it. Net worth never moves when a piggy bank
 * does, so the headline says so, and the "unassigned" hint per funding account
 * is what stops a household earmarking the same dollars twice.
 */
export function PiggyBanks() {
  const piggyBanks = useQuery({ queryKey: ['piggy-banks'], queryFn: api.piggyBanks })
  const accounts = useQuery({ queryKey: ['accounts'], queryFn: api.accounts })

  const rows = piggyBanks.data ?? []

  // The funding accounts that actually have at least one piggy bank, so the
  // per-account "unassigned" hint is shown only where it means something.
  const fundingAccountIds = Array.from(new Set(rows.map((p) => p.account_id)))

  return (
    <div className="space-y-8">
      <div>
        <h1 className="text-2xl font-semibold">Piggy banks</h1>
        <p className="mt-1 text-mist-300">
          Casual savings envelopes — &ldquo;car repair&rdquo;, &ldquo;Christmas&rdquo;,
          &ldquo;new laptop&rdquo; — on an account you already hold.
        </p>
        <p className="mt-2 text-xs text-mist-500">
          These don&rsquo;t change your net worth: the money is already in the
          account, and a piggy bank just earmarks part of it. Net worth counts the
          account once, not again for each jar.
        </p>
      </div>

      <section className="glass p-6">
        <h2 className="mb-1 text-lg font-medium">Your piggy banks</h2>
        <p className="mb-5 text-sm text-mist-300">
          Balances update from your deposit and withdraw events.
        </p>

        {piggyBanks.isPending ? (
          <SkeletonRows count={3} />
        ) : rows.length === 0 ? (
          <EmptyState
            title="No piggy banks yet"
            icon={<PiggyGlyph />}
            action={
              <a href="#add-piggy" className="btn-primary">
                Add a piggy bank
              </a>
            }
          >
            Set aside part of an account for something you&rsquo;re saving toward —
            no target date, no projection, just an envelope.
          </EmptyState>
        ) : (
          <Reveal>
            <div className="space-y-3">
              {rows.map((p) => (
                <PiggyCard
                  key={p.id}
                  piggy={p}
                  accountName={accountLabel(accounts.data, p.account_id)}
                />
              ))}
            </div>
          </Reveal>
        )}
      </section>

      {fundingAccountIds.length > 0 && (
        <section className="glass p-6">
          <h2 className="mb-1 text-lg font-medium">Unassigned balances</h2>
          <p className="mb-4 text-sm text-mist-300">
            What&rsquo;s left of each funding account after every piggy bank on it.
            Negative means you&rsquo;ve earmarked more than the account holds.
          </p>
          <div className="space-y-2">
            {fundingAccountIds.map((id) => (
              <AccountAllocationRow
                key={id}
                accountId={id}
                accountName={accountLabel(accounts.data, id)}
              />
            ))}
          </div>
        </section>
      )}

      <CreatePiggyBank accounts={accounts.data ?? []} isPending={accounts.isPending} />
    </div>
  )
}

function accountLabel(accounts: Account[] | undefined, id: string): string {
  const a = accounts?.find((x) => x.id === id)
  if (!a) return 'an account'
  let label = a.name
  if (a.mask) label += ` ••${a.mask}`
  return label
}

function PiggyCard({
  piggy,
  accountName,
}: {
  piggy: PiggyBank
  accountName: string
}) {
  const qc = useQueryClient()
  const [amount, setAmount] = useState('')
  const [showHistory, setShowHistory] = useState(false)

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ['piggy-banks'] })
    qc.invalidateQueries({ queryKey: ['piggy-bank-events', piggy.id] })
    qc.invalidateQueries({ queryKey: ['account-allocation', piggy.account_id] })
  }

  const deposit = useMutation({
    mutationFn: () => api.depositPiggyBank(piggy.id, amount),
    onSuccess: () => {
      setAmount('')
      invalidate()
    },
  })
  const withdraw = useMutation({
    mutationFn: () => api.withdrawPiggyBank(piggy.id, amount),
    onSuccess: () => {
      setAmount('')
      invalidate()
    },
  })
  const remove = useMutation({
    mutationFn: () => api.deletePiggyBank(piggy.id),
    onSuccess: () => invalidate(),
  })

  // Display-only percentage from two server-exact figures. An open-ended jar
  // (no target) renders no bar — there is nothing to be "toward".
  const targetNum = piggy.target_amount ? Number(piggy.target_amount) : 0
  const currentNum = Number(piggy.current_amount)
  const pct = targetNum > 0 ? Math.min((currentNum / targetNum) * 100, 100) : 0
  const achieved = targetNum > 0 && currentNum >= targetNum

  const moveError = deposit.error?.message ?? withdraw.error?.message

  return (
    <div className="rounded-xl border border-white/5 p-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <p className="font-medium">
            {piggy.name}
            {achieved && (
              <span className="ml-2 text-xs font-normal text-mist-500">funded</span>
            )}
          </p>
          <p className="mt-0.5 text-sm text-mist-400">
            on {accountName}{' '}
            <span className="tabular">
              · <AnimatedNumber value={piggy.current_amount} />
              {piggy.target_amount && (
                <>
                  {' '}
                  of <AnimatedNumber value={piggy.target_amount} />
                </>
              )}
            </span>
          </p>
        </div>
        <div className="flex items-center gap-3">
          <button
            className="btn-ghost px-2.5 py-1 text-xs"
            onClick={() => setShowHistory((v) => !v)}
          >
            {showHistory ? 'Hide history' : 'History'}
          </button>
          <button
            className="btn-ghost px-2.5 py-1 text-xs text-ember-400"
            disabled={remove.isPending}
            onClick={() => remove.mutate()}
          >
            Delete
          </button>
        </div>
      </div>

      {piggy.target_amount && (
        <div className="mt-3 flex items-center gap-3">
          <div className="h-1.5 flex-1 overflow-hidden rounded-full bg-white/5">
            <div
              className="h-full rounded-full"
              style={{ width: `${pct}%`, backgroundColor: STATUS.good }}
            />
          </div>
          <span className="tabular w-14 text-right text-xs text-mist-400">
            {Math.round(pct)}%
          </span>
        </div>
      )}

      <form
        className="mt-3 flex flex-wrap gap-2"
        onSubmit={(e) => {
          e.preventDefault()
          if (!amount) return
          deposit.mutate()
        }}
      >
        <input
          className="field w-32"
          placeholder="Amount"
          inputMode="decimal"
          value={amount}
          onChange={(e) => setAmount(e.target.value)}
        />
        <button
          className="btn-ghost px-3 py-1.5 text-sm"
          disabled={!amount || deposit.isPending}
          onClick={() => amount && deposit.mutate()}
        >
          {deposit.isPending ? '…' : 'Deposit'}
        </button>
        <button
          className="btn-ghost px-3 py-1.5 text-sm"
          disabled={!amount || withdraw.isPending}
          onClick={() => amount && withdraw.mutate()}
        >
          {withdraw.isPending ? '…' : 'Withdraw'}
        </button>
      </form>

      {moveError && (
        <p role="alert" className="mt-2 text-xs text-ember-400">
          {moveError}
        </p>
      )}

      {showHistory && <PiggyHistory piggyId={piggy.id} />}
    </div>
  )
}

function PiggyHistory({ piggyId }: { piggyId: string }) {
  const events = useQuery({
    queryKey: ['piggy-bank-events', piggyId],
    queryFn: () => api.piggyBankEvents(piggyId),
  })
  if (events.isPending) return <p className="mt-3 text-xs text-mist-500">Loading…</p>
  if ((events.data ?? []).length === 0)
    return <p className="mt-3 text-xs text-mist-500">No events yet.</p>
  return (
    <ul className="mt-3 space-y-1 rounded-lg bg-white/5 px-3 py-2 text-xs">
      {events.data?.map((e) => (
        <li key={e.id} className="flex items-center justify-between gap-3 tabular">
          <span className="text-mist-400">
            {new Date(e.created_at).toLocaleDateString('en-US', {
              month: 'short',
              day: 'numeric',
              year: 'numeric',
            })}
          </span>
          <span className="text-mist-300">{e.type}</span>
          <span
            className="font-medium"
            style={{ color: e.type === 'deposit' ? STATUS.good : STATUS.critical }}
          >
            {e.type === 'deposit' ? '+' : '−'}
            {formatMoney(e.amount)}
          </span>
        </li>
      ))}
    </ul>
  )
}

/**
 * The per-account unassigned balance. Reads the server's derived figure rather
 * than summing piggy banks in the browser — money is never summed here, and the
 * over-allocation flag is the loud warning the spec calls for.
 */
function AccountAllocationRow({
  accountId,
  accountName,
}: {
  accountId: string
  accountName: string
}) {
  const alloc = useQuery({
    queryKey: ['account-allocation', accountId],
    queryFn: () => api.accountAvailableForPiggy(accountId),
  })
  const row = alloc.data
  const over = row?.over_allocated ?? false
  return (
    <div
      className="flex flex-wrap items-center justify-between gap-3 rounded-lg px-3 py-2 text-sm"
      style={{
        backgroundColor: over ? `${STATUS.critical}14` : 'rgba(255,255,255,0.03)',
      }}
    >
      <span className="text-mist-300">{accountName}</span>
      <span className="tabular text-right" style={{ color: over ? STATUS.critical : undefined }}>
        {row ? `${formatMoney(row.available)} unassigned` : '—'}
        {over && row && (
          <span className="ml-2 text-xs font-normal">
            · earmarked {formatMoney(row.assigned)} of {formatMoney(row.account_balance)}
          </span>
        )}
      </span>
    </div>
  )
}

function CreatePiggyBank({
  accounts,
  isPending,
}: {
  accounts: Account[]
  isPending: boolean
}) {
  const qc = useQueryClient()
  const [name, setName] = useState('')
  const [accountID, setAccountID] = useState('')
  const [target, setTarget] = useState('')

  // Piggy banks sit on accounts that hold money, not debts — the same asset
  // vs. liability split the server enforces on create.
  const assetAccounts = accounts.filter((a) => !isLiability(a.type))

  const create = useMutation({
    mutationFn: () =>
      api.createPiggyBank({
        name: name.trim(),
        account_id: accountID,
        target_amount: target || undefined,
      }),
    onSuccess: () => {
      setName('')
      setTarget('')
      qc.invalidateQueries({ queryKey: ['piggy-banks'] })
      qc.invalidateQueries({ queryKey: ['account-allocation'] })
    },
  })

  const canAdd = name.trim() !== '' && accountID !== ''

  return (
    <section id="add-piggy" className="glass scroll-mt-24 p-6">
      <h2 className="mb-1 text-lg font-medium">Add a piggy bank</h2>
      <p className="mb-5 text-sm text-mist-300">
        Pick the account it draws from and name the envelope. A target is
        optional — leave it off for an open-ended jar.
      </p>

      <div className="grid gap-4 sm:grid-cols-2">
        <div>
          <label className="label" htmlFor="piggy-name">
            Name
          </label>
          <input
            id="piggy-name"
            className="field w-full"
            placeholder="Car repair fund"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
        </div>

        <div>
          <label className="label" htmlFor="piggy-account">
            Account
          </label>
          <select
            id="piggy-account"
            className="field w-full"
            value={accountID}
            onChange={(e) => setAccountID(e.target.value)}
          >
            <option value="">Choose an account…</option>
            {assetAccounts.map((a) => (
              <option key={a.id} value={a.id}>
                {a.name}
                {a.mask ? ` ••${a.mask}` : ''}
              </option>
            ))}
          </select>
          {!isPending && assetAccounts.length === 0 && (
            <p className="mt-1.5 text-xs text-mist-500">
              No checking or savings accounts yet — piggy banks need an account
              that holds money.
            </p>
          )}
        </div>

        <div>
          <label className="label" htmlFor="piggy-target">
            Target (optional)
          </label>
          <input
            id="piggy-target"
            className="field w-full"
            type="number"
            min="0"
            step="0.01"
            placeholder="Open-ended"
            value={target}
            onChange={(e) => setTarget(e.target.value)}
          />
        </div>
      </div>

      <div className="mt-5 flex items-center gap-3">
        <button
          className="btn-primary px-4 py-2 text-sm"
          disabled={!canAdd || create.isPending}
          onClick={() => create.mutate()}
        >
          {create.isPending ? 'Saving…' : 'Add piggy bank'}
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

/** Outline glyph for the piggy banks empty state. */
function PiggyGlyph() {
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
      <path d="M3 12a6 6 0 0 1 6-6h4a6 6 0 0 1 6 6v1a3 3 0 0 1-3 3H7a4 4 0 0 1-4-4Z" />
      <path d="M9 6V4M15 7V4M4 13H2M19 11l3-1" />
    </svg>
  )
}
