import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  api,
  PAYMENT_CADENCES,
  type Account,
  type Liability,
  type PlaidItem,
} from '../lib/api'
import { SkeletonCard } from '../components/Skeleton'
import {
  formatDate,
  formatMoney,
  formatRelative,
  isAmortizingDebt,
  isLiability,
} from '../lib/money'
import { OFFLINE_WRITE_HINT, useOnline } from '../lib/offline'
import { ConnectAccount } from '../components/ConnectAccount'
import { ReconnectAccount } from '../components/ReconnectAccount'
import { AddManualAccount, ManualAccountsCard } from '../components/ManualAccounts'
import { BalanceTrend } from '../components/charts/AccountBalanceChart'

export function Accounts() {
  const items = useQuery({ queryKey: ['items'], queryFn: api.items })
  const accounts = useQuery({ queryKey: ['accounts'], queryFn: api.accounts })
  // Fetched once here rather than per account row: this is a handful of debts,
  // and one query keeps every row's terms consistent after a save.
  const liabilities = useQuery({ queryKey: ['liabilities'], queryFn: api.liabilities })

  const grouped = groupByItem(accounts.data ?? [])
  const manualAccounts = grouped.get(MANUAL_GROUP) ?? []
  const termsByAccount = new Map<string, Liability>(
    (liabilities.data ?? []).map((l) => [l.account_id, l]),
  )

  return (
    <div className="space-y-8">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold">Accounts</h1>
          <p className="mt-1 text-mist-300">
            Connected institutions and the balances they report, plus anything
            you track by hand.
          </p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <ConnectAccount />
          <AddManualAccount />
        </div>
      </div>

      {items.isPending && (
        <div className="space-y-4">
          <SkeletonCard />
          <SkeletonCard />
        </div>
      )}

      {/* Only an empty state when there is genuinely nothing — a household
          running entirely on manual accounts has connected no institutions and
          should not be told it has no accounts. */}
      {items.data?.length === 0 && manualAccounts.length === 0 && (
        <section className="glass p-10 text-center">
          <p className="text-lg font-medium">No accounts yet</p>
          <p className="mx-auto mt-2 max-w-md text-sm text-mist-300">
            Connect a bank to pull in your accounts and transaction history —
            Ledgermancy fetches as much history as your institution provides,
            usually up to two years. For anything that cannot be linked, add it
            manually and keep the balance yourself.
          </p>
        </section>
      )}

      {items.data?.map((item) => (
        <InstitutionCard
          key={item.id}
          item={item}
          accounts={grouped.get(item.id) ?? []}
          termsByAccount={termsByAccount}
        />
      ))}

      <ManualAccountsCard accounts={manualAccounts} />
    </div>
  )
}

function InstitutionCard({
  item,
  accounts,
  termsByAccount,
}: {
  item: PlaidItem
  accounts: Account[]
  termsByAccount: Map<string, Liability>
}) {
  const qc = useQueryClient()
  // Syncing, sharing and unlinking all reach Plaid. Offline they cannot start,
  // so they are switched off rather than left to fail at the network layer.
  const online = useOnline()

  const refreshAll = () => {
    qc.invalidateQueries({ queryKey: ['items'] })
    qc.invalidateQueries({ queryKey: ['accounts'] })
    qc.invalidateQueries({ queryKey: ['transactions'] })
  }

  const sync = useMutation({ mutationFn: () => api.syncItem(item.id), onSuccess: refreshAll })
  const share = useMutation({
    mutationFn: (isShared: boolean) => api.setItemSharing(item.id, isShared),
    onSuccess: refreshAll,
  })
  const unlink = useMutation({ mutationFn: () => api.deleteItem(item.id), onSuccess: refreshAll })

  const needsAttention = item.status !== 'active'
  // Only a credential problem is fixable through Link. 'error' is a generic
  // failure — reconnecting would not address it, so it stays a plain warning.
  const canReconnect =
    item.status === 'login_required' || item.status === 'revoked'

  return (
    <section className="glass overflow-hidden">
      <header className="flex flex-wrap items-center gap-4 border-b border-white/5 px-6 py-4">
        <div>
          <h2 className="font-medium">
            {item.institution_name || 'Institution'}
            {/* Both spouses linking the same bank yields two cards with the
                same name; say which one is not yours. */}
            {!item.is_own && (
              <span className="ml-2 text-xs font-normal text-mist-500">
                linked by household member
              </span>
            )}
          </h2>
          <p className="mt-0.5 text-xs text-mist-500">
            synced {formatRelative(item.last_synced_at)}
            {!item.backfill_complete && ' · importing history…'}
            {item.history_days !== null && ` · ${item.history_days} days of history`}
          </p>
          {/* Plaid fixes an Item's history window at link time and it cannot be
              widened later, so a short span is worth flagging. Relinking is only
              a remedy when the institution itself offers more, though: some hard-
              cap what they share regardless (Capital One, for example, allows only
              90 days), so the note stops short of promising relinking will help. */}
          {item.backfill_complete &&
            item.history_days !== null &&
            item.history_days < 330 && (
              <p className="mt-1 text-xs text-rune-300">
                Only {item.history_days} days of history — under a year. Some
                institutions limit how far back they share (Capital One, for
                example, caps at 90 days), and there this is expected. Otherwise,
                unlinking and relinking may pull more.
              </p>
            )}
        </div>

        {needsAttention && (
          <span className="rounded-full border border-ember-400/30 bg-ember-400/10 px-3 py-1 text-xs text-ember-400">
            {item.status === 'login_required'
              ? 'Reconnect required'
              : item.status === 'revoked'
                ? 'Access revoked'
                : item.status}
          </span>
        )}

        <div className="ml-auto flex flex-wrap items-center gap-2">
          {/* Sharing is per institution, so one spouse can keep an account
              private while everything else rolls up to the household. */}
          <label className="flex cursor-pointer items-center gap-2 text-xs text-mist-300">
            <input
              type="checkbox"
              className="accent-arcane-500"
              checked={item.is_shared}
              disabled={share.isPending || !online}
              title={online ? undefined : OFFLINE_WRITE_HINT}
              onChange={(e) => share.mutate(e.target.checked)}
            />
            Shared with household
          </label>

          <button
            className="btn-ghost px-3 py-1.5 text-xs"
            disabled={sync.isPending || !online}
            title={online ? undefined : OFFLINE_WRITE_HINT}
            onClick={() => sync.mutate()}
          >
            {sync.isPending ? 'Syncing…' : 'Sync now'}
          </button>

          <button
            className="px-2 py-1.5 text-xs text-mist-500 transition hover:text-ember-400"
            disabled={unlink.isPending || !online}
            title={online ? undefined : OFFLINE_WRITE_HINT}
            onClick={() => {
              if (
                confirm(
                  `Unlink ${item.institution_name}? This deletes its accounts and transactions from Ledgermancy.`,
                )
              ) {
                unlink.mutate()
              }
            }}
          >
            Unlink
          </button>
        </div>
      </header>

      {canReconnect && (
        <div className="border-b border-white/5 bg-ember-400/5 px-6 py-4">
          <ReconnectAccount
            itemId={item.id}
            institutionName={item.institution_name}
          />
        </div>
      )}

      {sync.isSuccess && (
        <p className="border-b border-white/5 bg-verdant-400/5 px-6 py-2 text-xs text-verdant-400">
          Synced: {sync.data.added} added, {sync.data.modified} updated,{' '}
          {sync.data.removed} removed across {sync.data.accounts} accounts.
        </p>
      )}

      <ul className="divide-y divide-white/5">
        {accounts.map((a) => (
          <li key={a.id} className="px-6 py-3.5">
            <div className="flex items-center gap-4">
              <div className="min-w-0">
                <p className="truncate font-medium">
                  {a.name}
                  {a.mask && <span className="text-mist-500"> ••{a.mask}</span>}
                </p>
                <p className="text-xs text-mist-500">
                  {a.subtype ?? a.type}
                  {!a.is_own && ' · shared by household member'}
                </p>
              </div>
              <div className="ml-auto text-right">
                <p
                  className={`tabular font-medium ${
                    isLiability(a.type) ? 'text-ember-400' : 'text-rune-300'
                  }`}
                >
                  {formatMoney(a.current_balance, a.currency)}
                </p>
                {isLiability(a.type) && (
                  <p className="text-xs text-mist-500">owed</p>
                )}
              </div>
            </div>
            {isLiability(a.type) && (
              <DebtTerms account={a} terms={termsByAccount.get(a.id)} />
            )}
            {a.type === 'depository' && <DepositYield account={a} />}
            <BalanceTrend accountId={a.id} currency={a.currency} />
          </li>
        ))}
        {accounts.length === 0 && (
          <li className="px-6 py-4 text-sm text-mist-500">
            No accounts reported yet.
          </li>
        )}
      </ul>
    </section>
  )
}

/**
 * The rate and monthly payment behind one debt account.
 *
 * Plaid serves its Liabilities product at a minority of institutions — Capital
 * One and most credit unions decline it outright — so for most households this
 * is the only place an APR ever comes from, and without one a debt-payoff goal
 * can report no payoff date and no interest cost.
 *
 * Typed values always beat synced ones and survive every sync, which is why
 * clearing both fields has to be possible: it is the only way to hand the
 * account back to the bank's own figures.
 *
 * Collapsed by default. This is a list of accounts, and unfolding a form under
 * every card and loan would turn it into something else.
 */
function DebtTerms({
  account,
  terms,
}: {
  account: Account
  terms?: Liability
}) {
  const qc = useQueryClient()
  const online = useOnline()
  const [open, setOpen] = useState(false)
  const [apr, setApr] = useState('')
  const [payment, setPayment] = useState('')
  const [dueDate, setDueDate] = useState('')
  // Index into PAYMENT_CADENCES; -1 is "no schedule".
  const [cadence, setCadence] = useState(-1)

  const knownAPR = terms?.apr_source ? terms.apr : null
  const knownPayment = terms?.payment_source ? terms.minimum_payment : null

  // Decides whether this account's rate is an APR or a note rate, and so what
  // every label below calls it. See isAmortizingDebt for why the two differ.
  const isAmortizing = isAmortizingDebt(account.type)

  const save = useMutation({
    mutationFn: (input: Parameters<typeof api.setAccountTerms>[1]) =>
      api.setAccountTerms(account.id, input),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['liabilities'] })
      // Every payoff schedule built on this account changes, and a scheduled
      // payment is a bill, so the calendar and the forecast move too. Net worth
      // does not: those totals are balance-driven and a rate never moves them.
      qc.invalidateQueries({ queryKey: ['goals'] })
      qc.invalidateQueries({ queryKey: ['obligations'] })
      qc.invalidateQueries({ queryKey: ['upcoming-obligations'] })
      setOpen(false)
    },
  })

  const start = () => {
    // Seed from what is known so an edit is an edit, not a retype. A Plaid-
    // supplied figure seeds too — accepting it unchanged just pins it.
    setApr(knownAPR ?? '')
    setPayment(knownPayment ?? '')
    setDueDate(terms?.schedule?.anchor_date ?? terms?.next_payment_due_date ?? '')
    setCadence(
      terms?.schedule
        ? PAYMENT_CADENCES.findIndex(
            (c) =>
              c.interval_count === terms.schedule!.interval_count &&
              c.interval_unit === terms.schedule!.interval_unit,
          )
        : -1,
    )
    setOpen(true)
  }

  if (!open) {
    return (
      <div className="mt-1.5 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-mist-500">
        {terms?.apr_source ? (
          <span>
            {Number(terms.apr).toFixed(isAmortizing ? 3 : 2)}%{' '}
            {isAmortizing ? 'rate' : 'APR'}
            {terms.apr_source === 'manual' && ' (yours)'}
          </span>
        ) : (
          // The user-facing half of the silent-skip problem: the app knew the
          // bank reported nothing and said so nowhere.
          <span>Your bank doesn't report a rate for this account</span>
        )}
        {terms?.payment_source && (
          <span>
            · {formatMoney(terms.minimum_payment ?? '0', account.currency)}
            {terms.schedule ? ` ${terms.schedule.label}` : '/mo'}
            {terms.payment_source === 'manual' && ' (yours)'}
          </span>
        )}
        {terms?.next_payment_due_date && (
          <span>· next due {formatDate(terms.next_payment_due_date)}</span>
        )}
        <button type="button" className="underline" onClick={start}>
          {terms?.apr_source === 'manual' || terms?.payment_source === 'manual'
            ? 'Edit terms'
            : 'Add terms'}
        </button>
      </div>
    )
  }

  return (
    <div className="mt-2 rounded-lg bg-white/5 p-3">
      <div className="grid gap-3 sm:grid-cols-2">
        <div>
          <label className="label" htmlFor={`apr-${account.id}`}>
            {isAmortizing ? 'Interest rate (%)' : 'APR (%)'}
          </label>
          <input
            id={`apr-${account.id}`}
            className="field w-full"
            type="number"
            min="0"
            max="200"
            step="0.001"
            // 18.99 for 18.99%, never 0.1899 — the placeholder is doing real
            // work here, since the wrong form satisfies every validation rule
            // and quietly reports the card as almost interest-free. The loan
            // placeholder carries three decimals because note rates are quoted
            // in eighths (6.775, 5.875) and a two-decimal step would refuse
            // them.
            placeholder={isAmortizing ? '6.775' : '18.99'}
            value={apr}
            onChange={(e) => setApr(e.target.value)}
          />
          {isAmortizing && (
            <p className="mt-1 text-xs text-mist-500">
              Your note rate, not the APR disclosed at closing.
            </p>
          )}
        </div>
        <div>
          <label className="label" htmlFor={`payment-${account.id}`}>
            Monthly payment
          </label>
          <input
            id={`payment-${account.id}`}
            className="field w-full"
            type="number"
            min="0"
            step="0.01"
            placeholder="250.00"
            value={payment}
            onChange={(e) => setPayment(e.target.value)}
          />
          {isAmortizing && (
            // Escrow is collected alongside the payment and never applied to the
            // loan, but ComputePayoff subtracts whatever it is given from the
            // balance. A payment entered with escrow included therefore retires
            // principal that is really sitting in a tax account, and reports a
            // 30-year mortgage as clearing years early.
            <p className="mt-1 text-xs text-mist-500">
              Principal and interest only — leave out escrow for taxes and
              insurance.
            </p>
          )}
        </div>
      </div>

      <div className="mt-3 grid gap-3 sm:grid-cols-2">
        <div>
          <label className="label" htmlFor={`due-${account.id}`}>
            Next payment due
          </label>
          <input
            id={`due-${account.id}`}
            className="field w-full"
            type="date"
            value={dueDate}
            onChange={(e) => setDueDate(e.target.value)}
          />
        </div>
        <div>
          <label className="label" htmlFor={`cadence-${account.id}`}>
            Repeats
          </label>
          <select
            id={`cadence-${account.id}`}
            className="field w-full"
            value={cadence}
            onChange={(e) => setCadence(Number(e.target.value))}
          >
            <option value={-1}>Don't schedule this</option>
            {PAYMENT_CADENCES.map((c, i) => (
              <option key={c.label} value={i}>
                {c.label}
              </option>
            ))}
          </select>
        </div>
      </div>

      <p className="mt-2 text-xs text-mist-500">
        What you enter here is kept, and a later sync will never overwrite it.
        Clear the rate and payment to go back to whatever your bank reports.
        {/* The month-end behaviour is worth stating, because it is the one
            people expect to get wrong and it does the right thing. */}
        {cadence >= 0 && PAYMENT_CADENCES[cadence]?.interval_unit === 'month' && (
          <>
            {' '}
            A payment dated on the 31st lands on the last day of shorter months
            and returns to the 31st afterwards.
          </>
        )}
      </p>
      {cadence >= 0 && (
        <p className="mt-1 text-xs text-mist-500">
          This payment will appear on your schedule and count toward cash-flow
          forecasting, like your other bills.
        </p>
      )}

      {save.isError && (
        <p className="mt-2 text-xs text-ember-400">
          {(save.error as Error).message}
        </p>
      )}

      <div className="mt-3 flex items-center gap-3">
        <button
          type="button"
          className="btn-primary"
          disabled={!online || save.isPending || (cadence >= 0 && dueDate === '')}
          title={online ? undefined : OFFLINE_WRITE_HINT}
          onClick={() =>
            save.mutate({
              apr: apr === '' ? null : apr,
              minimum_payment: payment === '' ? null : payment,
              schedule:
                cadence >= 0 && dueDate !== ''
                  ? {
                      anchor_date: dueDate,
                      interval_count: PAYMENT_CADENCES[cadence].interval_count,
                      interval_unit: PAYMENT_CADENCES[cadence].interval_unit,
                    }
                  : null,
            })
          }
        >
          {save.isPending ? 'Saving…' : 'Save'}
        </button>
        <button
          type="button"
          className="text-xs text-mist-400 underline"
          onClick={() => setOpen(false)}
        >
          Cancel
        </button>
      </div>
    </div>
  )
}

/**
 * The yield on one deposit account.
 *
 * Plaid does not serve deposit APYs reliably, so this is the only route a rate
 * ever takes into the cash-drag detector — and the detector is deliberately
 * SILENT without one. An empty field means "nobody has said", never "this earns
 * nothing", which is why clearing it has to be possible: it is how a household
 * says it no longer knows.
 *
 * The detector compares each account against the household's own best rate, so
 * entering a rate on ONE savings account is what turns the whole feature on.
 * That is stated in the copy rather than left to be discovered.
 *
 * Collapsed by default, like DebtTerms above and for the same reason.
 */
function DepositYield({ account }: { account: Account }) {
  const qc = useQueryClient()
  const online = useOnline()
  const [open, setOpen] = useState(false)
  const [apy, setApy] = useState('')

  const save = useMutation({
    mutationFn: (value: string | null) => api.setDepositApy(account.id, value),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['accounts'] })
      qc.invalidateQueries({ queryKey: ['idle-cash'] })
      qc.invalidateQueries({ queryKey: ['allocation-buckets'] })
      setOpen(false)
    },
  })

  if (!open) {
    return (
      <div className="mt-2 flex flex-wrap items-center gap-x-2 gap-y-1 text-xs text-mist-500">
        {account.deposit_apy ? (
          <span className="text-mist-300">{account.deposit_apy}% APY</span>
        ) : (
          <span>No yield recorded — unknown, not zero.</span>
        )}
        <button
          type="button"
          className="text-arcane-300 underline"
          onClick={() => {
            setApy(account.deposit_apy ?? '')
            setOpen(true)
          }}
        >
          {account.deposit_apy ? 'Edit yield' : 'Add yield'}
        </button>
      </div>
    )
  }

  return (
    <div className="mt-3 rounded-xl border border-white/10 bg-white/[0.03] p-3">
      <label className="label" htmlFor={`apy-${account.id}`}>
        Annual yield (APY)
      </label>
      <input
        id={`apy-${account.id}`}
        className="field w-full"
        type="number"
        min="0"
        max="25"
        step="0.01"
        // 4.50 for 4.5%, never 0.045 — the wrong form passes every validation
        // rule and reports the account as earning almost nothing, which is
        // exactly the drag the detector would then shout about.
        placeholder="4.50"
        value={apy}
        onChange={(e) => setApy(e.target.value)}
      />
      <p className="mt-2 text-xs text-mist-500">
        Used to spot cash earning less than it could. Idle-cash figures compare
        every account against the best rate you already earn somewhere, so
        entering one here is what switches the check on. Clear it to go back to
        unknown.
      </p>
      {save.isError && (
        <p className="mt-2 text-xs text-ember-400">{(save.error as Error).message}</p>
      )}
      <div className="mt-3 flex items-center gap-3">
        <button
          type="button"
          className="btn-primary"
          disabled={!online || save.isPending}
          title={online ? undefined : OFFLINE_WRITE_HINT}
          onClick={() => save.mutate(apy === '' ? null : apy)}
        >
          {save.isPending ? 'Saving…' : 'Save'}
        </button>
        <button
          type="button"
          className="text-xs text-mist-400 underline"
          onClick={() => setOpen(false)}
        >
          Cancel
        </button>
      </div>
    </div>
  )
}

/** The group key for manual accounts, which have no item id to group by. A
 *  constant rather than a null key so they all land in one card. */
const MANUAL_GROUP = 'manual'

// Keyed by item, not by institution name. Two members of a household can each
// link the same bank — grouping by name gave both "Capital One" cards the union
// of everyone's Capital One accounts, so every account appeared on both.
//
// Manual accounts have no item at all, so they share one synthetic key: they
// belong to no institution, but they do share the set of things you can do to
// them, which is what the grouping is really for.
function groupByItem(accounts: Account[]): Map<string, Account[]> {
  const map = new Map<string, Account[]>()
  for (const a of accounts) {
    const key = a.item_id ?? MANUAL_GROUP
    const list = map.get(key)
    if (list) list.push(a)
    else map.set(key, [a])
  }
  return map
}
