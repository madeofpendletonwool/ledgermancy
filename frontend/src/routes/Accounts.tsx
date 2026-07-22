import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type Account, type PlaidItem } from '../lib/api'
import { formatMoney, formatRelative, isLiability } from '../lib/money'
import { ConnectAccount } from '../components/ConnectAccount'

export function Accounts() {
  const items = useQuery({ queryKey: ['items'], queryFn: api.items })
  const accounts = useQuery({ queryKey: ['accounts'], queryFn: api.accounts })

  const grouped = groupByInstitution(accounts.data ?? [])

  return (
    <div className="space-y-8">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold">Accounts</h1>
          <p className="mt-1 text-mist-300">
            Connected institutions and the balances they report.
          </p>
        </div>
        <ConnectAccount />
      </div>

      {items.isPending && <p className="text-sm text-mist-500">Loading…</p>}

      {items.data?.length === 0 && (
        <section className="glass p-10 text-center">
          <p className="text-lg font-medium">No accounts connected yet</p>
          <p className="mx-auto mt-2 max-w-md text-sm text-mist-300">
            Connect a bank to pull in your accounts and transaction history.
            Ledgermancy fetches as much history as your institution provides —
            usually up to two years.
          </p>
        </section>
      )}

      {items.data?.map((item) => (
        <InstitutionCard
          key={item.id}
          item={item}
          accounts={grouped.get(item.institution_name) ?? []}
        />
      ))}
    </div>
  )
}

function InstitutionCard({
  item,
  accounts,
}: {
  item: PlaidItem
  accounts: Account[]
}) {
  const qc = useQueryClient()

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

  return (
    <section className="glass overflow-hidden">
      <header className="flex flex-wrap items-center gap-4 border-b border-white/5 px-6 py-4">
        <div>
          <h2 className="font-medium">{item.institution_name || 'Institution'}</h2>
          <p className="mt-0.5 text-xs text-mist-500">
            synced {formatRelative(item.last_synced_at)}
            {!item.backfill_complete && ' · importing history…'}
            {item.history_days !== null && ` · ${item.history_days} days of history`}
          </p>
          {/* Plaid fixes an Item's history window at link time and it cannot be
              widened later, so a short span is flagged while relinking is still
              the remedy. */}
          {item.backfill_complete &&
            item.history_days !== null &&
            item.history_days < 330 && (
              <p className="mt-1 text-xs text-rune-300">
                Only {item.history_days} days of history — under a year. Plaid fixes
                this window when an account is linked, so unlink and relink to try
                for more.
              </p>
            )}
        </div>

        {needsAttention && (
          <span className="rounded-full border border-ember-400/30 bg-ember-400/10 px-3 py-1 text-xs text-ember-400">
            {item.status === 'login_required'
              ? 'Reconnect required'
              : item.status}
          </span>
        )}

        <div className="ml-auto flex items-center gap-2">
          {/* Sharing is per institution, so one spouse can keep an account
              private while everything else rolls up to the household. */}
          <label className="flex cursor-pointer items-center gap-2 text-xs text-mist-300">
            <input
              type="checkbox"
              className="accent-arcane-500"
              checked={item.is_shared}
              disabled={share.isPending}
              onChange={(e) => share.mutate(e.target.checked)}
            />
            Shared with household
          </label>

          <button
            className="btn-ghost px-3 py-1.5 text-xs"
            disabled={sync.isPending}
            onClick={() => sync.mutate()}
          >
            {sync.isPending ? 'Syncing…' : 'Sync now'}
          </button>

          <button
            className="px-2 py-1.5 text-xs text-mist-500 transition hover:text-ember-400"
            disabled={unlink.isPending}
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

      {sync.isSuccess && (
        <p className="border-b border-white/5 bg-verdant-400/5 px-6 py-2 text-xs text-verdant-400">
          Synced: {sync.data.added} added, {sync.data.modified} updated,{' '}
          {sync.data.removed} removed across {sync.data.accounts} accounts.
        </p>
      )}

      <ul className="divide-y divide-white/5">
        {accounts.map((a) => (
          <li key={a.id} className="flex items-center gap-4 px-6 py-3.5">
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

function groupByInstitution(accounts: Account[]): Map<string, Account[]> {
  const map = new Map<string, Account[]>()
  for (const a of accounts) {
    const key = a.institution_name ?? ''
    const list = map.get(key)
    if (list) list.push(a)
    else map.set(key, [a])
  }
  return map
}
