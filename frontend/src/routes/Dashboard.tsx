import { Link } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { api } from '../lib/api'
import { useSession } from '../lib/session'
import { formatMoney, isLiability } from '../lib/money'

/**
 * The dashboard shows what is actually known today. Spending totals need the
 * categorisation work in the next phase, so those tiles say so rather than
 * displaying a number that would be wrong.
 */
export function Dashboard() {
  const { data: user } = useSession()
  const household = useQuery({ queryKey: ['household'], queryFn: api.household })
  const accounts = useQuery({ queryKey: ['accounts'], queryFn: api.accounts })
  const items = useQuery({ queryKey: ['items'], queryFn: api.items })

  const rows = accounts.data ?? []
  const hasData = rows.length > 0

  // Cash and debt are summed here only to display them. Every figure that
  // feeds real analysis — monthly spend, savings rate, net worth — is computed
  // server-side in exact decimal, never in JavaScript.
  const cash = sumBalances(rows.filter((a) => !isLiability(a.type)))
  const debt = sumBalances(rows.filter((a) => isLiability(a.type)))

  const importing = items.data?.some((i) => !i.backfill_complete)
  const needsAttention = items.data?.filter((i) => i.status !== 'active') ?? []

  return (
    <div className="space-y-8">
      <div>
        <h1 className="text-2xl font-semibold">
          Good to see you, {user?.display_name}
        </h1>
        <p className="mt-1 text-mist-300">
          {household.data?.name ?? 'Loading household…'}
        </p>
      </div>

      {needsAttention.map((item) => (
        <div
          key={item.id}
          className="rounded-xl border border-ember-400/30 bg-ember-400/10 px-4 py-3 text-sm text-ember-400"
        >
          {item.institution_name} needs to be reconnected before it can sync
          again. <Link to="/accounts" className="underline">Go to accounts</Link>
        </div>
      ))}

      {importing && (
        <div className="rounded-xl border border-rune-400/30 bg-rune-400/10 px-4 py-3 text-sm text-rune-300">
          Importing your transaction history — this can take a minute.
        </div>
      )}

      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <StatTile
          label="Accounts linked"
          value={hasData ? String(rows.length) : '0'}
          hint={
            items.data?.length
              ? `${items.data.length} institution${items.data.length === 1 ? '' : 's'}`
              : 'Connect Plaid to begin'
          }
        />
        <StatTile
          label="Cash & investments"
          value={hasData ? formatMoney(cash) : '—'}
          hint="Across depository and investment accounts"
        />
        <StatTile
          label="Debt"
          value={hasData ? formatMoney(debt) : '—'}
          hint="Credit cards and loans"
          tone={hasData && Number(debt) > 0 ? 'warn' : 'default'}
        />
        <StatTile
          label="This month's spend"
          value="—"
          hint="Needs categorisation (next phase)"
        />
      </div>

      <section className="glass p-6">
        <h2 className="text-lg font-medium">
          {hasData ? 'What’s next' : 'Get started'}
        </h2>
        <p className="mt-2 max-w-2xl text-sm text-mist-300">
          {hasData
            ? 'Your accounts and transaction history are flowing in. Next comes categorisation, which turns those transactions into spending by category, budgets, and the monthly leftover figure.'
            : 'Connect your first account to pull in balances and as much transaction history as your institution provides.'}
        </p>

        {!hasData && (
          <Link to="/accounts" className="btn-primary mt-5 inline-flex">
            Connect an account
          </Link>
        )}

        <ol className="mt-6 space-y-3">
          <Step done label="Household & secure sign-in" />
          <Step done label="Ledger schema & exact-decimal money handling" />
          <Step done={hasData} label="Plaid link, transaction sync & historical backfill" />
          <Step label="Categorisation, budgets & the spending dashboard" />
          <Step label="Net worth, investments & debt" />
          <Step label="Financial summary export" />
        </ol>
      </section>
    </div>
  )
}

/** Sums decimal strings for display only. See the note in the component. */
function sumBalances(accounts: { current_balance: string | null }[]): string {
  return accounts
    .reduce((total, a) => total + Number(a.current_balance ?? 0), 0)
    .toFixed(2)
}

function StatTile({
  label,
  value,
  hint,
  tone = 'default',
}: {
  label: string
  value: string
  hint: string
  tone?: 'default' | 'warn'
}) {
  return (
    <div className="glass p-5">
      <p className="text-sm text-mist-300">{label}</p>
      <p
        className={`tabular mt-2 text-3xl font-semibold ${
          tone === 'warn' ? 'text-ember-400' : 'text-rune-300'
        }`}
      >
        {value}
      </p>
      <p className="mt-1 text-xs text-mist-500">{hint}</p>
    </div>
  )
}

function Step({ label, done = false }: { label: string; done?: boolean }) {
  return (
    <li className="flex items-center gap-3 text-sm">
      <span
        className={`flex h-5 w-5 shrink-0 items-center justify-center rounded-full border text-[11px] ${
          done
            ? 'border-verdant-400/40 bg-verdant-400/15 text-verdant-400'
            : 'border-white/15 text-mist-500'
        }`}
      >
        {done ? '✓' : '○'}
      </span>
      <span className={done ? 'text-mist-300' : 'text-mist-500'}>{label}</span>
    </li>
  )
}
