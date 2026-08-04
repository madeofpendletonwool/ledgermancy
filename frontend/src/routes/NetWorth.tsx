import { useState, type FormEvent } from 'react'
import { Link } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { motion, useReducedMotion } from 'motion/react'
import { api, type NetWorthPoint } from '../lib/api'
import { formatMoney, isAmortizingDebt } from '../lib/money'
import { AttachDocuments } from '../components/AttachDocuments'
import { NetWorthComposition } from '../components/charts/NetWorthComposition'
import { lineDraw } from '../components/charts/motion'
import { AnimatedNumber } from '../components/motion'
import { CHART, SERIES, SINGLE_SERIES, STATUS } from '../components/charts/tokens'

export function NetWorth() {
  const qc = useQueryClient()
  const current = useQuery({ queryKey: ['networth'], queryFn: api.netWorth })
  const history = useQuery({ queryKey: ['networth-history'], queryFn: () => api.netWorthHistory() })
  const holdings = useQuery({ queryKey: ['holdings'], queryFn: api.holdings })
  const liabilities = useQuery({ queryKey: ['liabilities'], queryFn: api.liabilities })
  const manual = useQuery({ queryKey: ['manual-assets'], queryFn: api.manualAssets })

  const snapshot = useMutation({
    mutationFn: api.snapshotNetWorth,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['networth'] })
      qc.invalidateQueries({ queryKey: ['networth-history'] })
    },
  })

  const nw = current.data
  const b = nw?.breakdown

  return (
    <div className="space-y-8">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold">Net worth</h1>
          <p className="mt-1 text-mist-300">Everything you own, minus everything you owe.</p>
        </div>
        <button
          className="btn-ghost text-sm"
          disabled={snapshot.isPending}
          onClick={() => snapshot.mutate()}
        >
          {snapshot.isPending ? 'Recording…' : 'Record today'}
        </button>
      </div>

      <div className="grid gap-4 sm:grid-cols-3">
        <Tile label="Assets" value={nw ? nw.assets_total : null} />
        <Tile label="Liabilities" value={nw ? nw.liabilities_total : null} tone="debt" />
        <Tile
          label="Net worth"
          value={nw ? nw.net_worth : null}
          tone={nw && Number(nw.net_worth) < 0 ? 'debt' : 'good'}
          large
        />
      </div>

      <section className="glass p-6">
        <h2 className="mb-1 text-lg font-medium">Over time</h2>
        <p className="mb-5 text-sm text-mist-300">
          Recorded daily. Balances have no history of their own, so the line starts
          the day Ledgermancy did.
        </p>
        <NetWorthChart data={history.data ?? []} />
      </section>

      <section className="glass p-6">
        <h2 className="mb-1 text-lg font-medium">Composition over time</h2>
        <p className="mb-5 text-sm text-mist-300">
          Assets above the line, liabilities below — the gap between the two
          stacks is the net worth plotted above. Each snapshot carries the
          breakdown it was recorded with, so the make-up of a past month
          survives even after accounts are closed or re-linked.
        </p>
        <NetWorthComposition data={history.data ?? []} />
      </section>

      <ByPerson />

      {b && (
        <section className="glass p-6">
          <h2 className="mb-5 text-lg font-medium">What it is made of</h2>
          <div className="grid gap-x-8 gap-y-3 sm:grid-cols-2">
            <div className="space-y-2">
              <p className="text-xs font-medium tracking-wide text-mist-500 uppercase">Assets</p>
              <Row label="Cash & deposits" value={b.cash} />
              <Row label="Investments" value={b.investments} />
              {Number(b.other_assets) !== 0 && <Row label="Other" value={b.other_assets} />}
              <Row label="Manual assets" value={b.manual_assets} />
            </div>
            <div className="space-y-2">
              <p className="text-xs font-medium tracking-wide text-mist-500 uppercase">Liabilities</p>
              <Row label="Credit cards" value={b.credit_debt} debt />
              <Row label="Loans" value={b.loan_debt} debt />
              <Row label="Manual debt" value={b.manual_debt} debt />
            </div>
          </div>
        </section>
      )}

      <ManualAssets assets={manual.data ?? []} />

      {(holdings.data?.length ?? 0) > 0 && (
        <section className="glass overflow-hidden">
          <div className="px-6 pt-6 pb-4">
            <h2 className="text-lg font-medium">Holdings</h2>
            <p className="mt-1 text-sm text-mist-300">
              Positions across your investment accounts.
            </p>
          </div>
          <div className="overflow-x-auto">
            <table className="w-full text-sm">
              <thead>
                <tr className="border-y border-white/5 text-left text-xs text-mist-500">
                  <th className="px-6 py-2.5 font-medium">Security</th>
                  <th className="px-6 py-2.5 text-right font-medium">Quantity</th>
                  <th className="px-6 py-2.5 text-right font-medium">Value</th>
                  <th className="px-6 py-2.5 text-right font-medium">Gain</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-white/5">
                {holdings.data!.map((h) => (
                  <tr key={h.id}>
                    <td className="px-6 py-2.5">
                      <span className="font-medium">{h.security_name ?? 'Unknown'}</span>
                      {h.ticker && <span className="ml-2 text-xs text-mist-500">{h.ticker}</span>}
                      <span className="block text-xs text-mist-500">{h.account_name}</span>
                    </td>
                    <td className="tabular px-6 py-2.5 text-right text-mist-300">
                      {trimQuantity(h.quantity)}
                    </td>
                    <td className="tabular px-6 py-2.5 text-right">{formatMoney(h.value)}</td>
                    <td
                      className="tabular px-6 py-2.5 text-right"
                      style={{
                        color: h.gain
                          ? Number(h.gain) >= 0
                            ? STATUS.good
                            : STATUS.critical
                          : CHART.textMuted,
                      }}
                    >
                      {h.gain ? formatMoney(h.gain) : '—'}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>
      )}

      {(liabilities.data?.length ?? 0) > 0 && (
        <section className="glass overflow-hidden">
          <div className="px-6 pt-6 pb-4">
            <h2 className="text-lg font-medium">Debt</h2>
            <p className="mt-1 text-sm text-mist-300">Balances, rates and what is due next.</p>
          </div>
          <ul className="divide-y divide-white/5">
            {liabilities.data!.map((l) => (
              <li key={l.id} className="flex flex-wrap items-center gap-4 px-6 py-3.5">
                <div className="min-w-0">
                  <p className="truncate font-medium">
                    {l.account_name}
                    {l.mask && <span className="text-mist-500"> ••{l.mask}</span>}
                  </p>
                  <p className="text-xs text-mist-500">
                    {l.kind}
                    {l.institution_name && ` · ${l.institution_name}`}
                    {l.next_payment_due_date && ` · due ${l.next_payment_due_date}`}
                  </p>
                </div>
                <div className="ml-auto flex items-center gap-6">
                  {/* Most banks report no rate, so the absent case is the common
                      one and needs to offer the fix rather than render blank. */}
                  {l.apr ? (
                    <span className="tabular text-sm text-mist-300">
                      {isAmortizingDebt(l.account_type) ? (
                        <>{Number(l.apr).toFixed(3)}% rate</>
                      ) : (
                        <>{Number(l.apr).toFixed(2)}% APR</>
                      )}
                    </span>
                  ) : (
                    <Link to="/accounts" className="text-xs text-mist-500 underline">
                      add a rate
                    </Link>
                  )}
                  <span className="tabular font-medium" style={{ color: STATUS.critical }}>
                    {formatMoney(l.balance)}
                  </span>
                </div>
              </li>
            ))}
          </ul>
        </section>
      )}
    </div>
  )
}

/** Net worth over time. A single series, so one colour and no legend. */
/**
 * Assets broken down by the person they are held for.
 *
 * A BREAKDOWN, never a second total. A child's 529 was already counted in
 * household assets and stays there — this only says whose slice it is, which is
 * why "unassigned" is shown rather than hidden: the two must visibly reconcile.
 */
function ByPerson() {
  const byPerson = useQuery({
    queryKey: ['networth-by-person'],
    queryFn: api.netWorthByPerson,
  })

  const people = byPerson.data?.people.filter(
    (p) => p.total !== '0.00' || p.is_dependent,
  )
  if (!people?.length) return null

  // A single stacked bar makes the household split scannable. Colour is
  // redundant here — identity rides on the list below (the app's own
  // AllocationList rule) — so every person wears the single-series hue stepped
  // only in opacity, and the unassigned remainder recedes.
  const grandTotal = Number(byPerson.data?.total ?? 0)
  const segments = [
    ...people.map((p, i) => ({
      key: p.person_id,
      label: p.person_name,
      value: Number(p.total),
      opacity: Math.max(0.35, 1 - i * 0.12),
      muted: false,
    })),
    {
      key: '__unassigned__',
      label: 'Not assigned to anyone',
      value: Number(byPerson.data?.unassigned ?? 0),
      opacity: 1,
      muted: true,
    },
  ].filter((s) => s.value > 0)

  return (
    <section className="glass p-6">
      <h2 className="mb-1 text-lg font-medium">Whose money</h2>
      <p className="mb-5 text-sm text-mist-300">
        A breakdown of the assets above, not an extra total. Anything not held
        for a specific person shows as unassigned.
      </p>

      {grandTotal > 0 && segments.length > 0 && (
        <div className="mb-5">
          <div className="flex h-4 gap-px overflow-hidden rounded-md bg-white/5">
            {segments.map((s) => (
              <span
                key={s.key}
                className="h-full"
                style={{
                  width: `${(s.value / grandTotal) * 100}%`,
                  backgroundColor: s.muted ? CHART.axis : SINGLE_SERIES,
                  opacity: s.muted ? 1 : s.opacity,
                }}
                title={`${s.label} · ${formatMoney(String(s.value))}`}
              />
            ))}
          </div>
          <p className="mt-1.5 text-[11px] text-mist-500">
            Each segment is one person&rsquo;s share; the pale tail is unassigned.
            Names and amounts in the list below.
          </p>
        </div>
      )}

      <ul className="divide-y divide-white/5">
        {people.map((p) => (
          <li key={p.person_id} className="flex flex-wrap items-center gap-3 py-3">
            <div className="min-w-0 flex-1">
              <p className="truncate font-medium">
                {p.person_name}
                {p.is_dependent && (
                  <span className="ml-2 rounded-full bg-white/5 px-2 py-0.5 text-xs text-mist-300">
                    dependent
                  </span>
                )}
              </p>
              {Number(p.custodial_total) > 0 && (
                <p className="truncate text-xs text-mist-500">
                  {formatMoney(p.custodial_total)} custodial — theirs, and kept out
                  of the household's retirement total
                </p>
              )}
              {p.age !== null && (
                <p className="text-xs text-mist-500">{p.age} years old</p>
              )}
            </div>
            <span className="shrink-0 tabular">{formatMoney(p.total)}</span>
          </li>
        ))}
      </ul>

      {byPerson.data && (
        <div className="mt-4 flex items-center justify-between border-t border-white/10 pt-3 text-sm">
          <span className="text-mist-500">Not assigned to anyone</span>
          <span className="tabular text-mist-300">
            {formatMoney(byPerson.data.unassigned)}
          </span>
        </div>
      )}
    </section>
  )
}

function NetWorthChart({ data }: { data: NetWorthPoint[] }) {
  const reduce = useReducedMotion() ?? false

  if (data.length < 2) {
    return (
      <p className="py-10 text-center text-sm" style={{ color: CHART.textMuted }}>
        {data.length === 1
          ? 'One reading so far — the trend appears once there are at least two.'
          : 'No readings yet.'}
      </p>
    )
  }

  const W = 760
  const H = 220
  const PAD = { top: 12, right: 12, bottom: 24, left: 72 }
  const plotW = W - PAD.left - PAD.right
  const plotH = H - PAD.top - PAD.bottom

  const values = data.map((d) => Number(d.net_worth))
  const lo = Math.min(...values)
  const hi = Math.max(...values)

  // Fit the domain to the data with a margin, rather than forcing zero in.
  //
  // A bar chart must start at zero because bar *length* encodes magnitude. A
  // line encodes value by position, so truncating is legitimate — and here it
  // is necessary: a household carrying a mortgage sits well below zero, and
  // anchoring to zero squashed the entire trend into the bottom fifth of the
  // plot where no movement was visible. Zero is still pulled into the domain
  // whenever the data comes near it, so a crossing into positive net worth is
  // never hidden.
  const margin = (hi - lo || Math.abs(hi) || 1) * 0.15
  const nearZero = lo - margin <= 0 && hi + margin >= 0
  const min = nearZero ? Math.min(lo - margin, 0) : lo - margin
  const max = nearZero ? Math.max(hi + margin, 0) : hi + margin
  const span = max - min || 1

  const x = (i: number) => PAD.left + (i / (data.length - 1)) * plotW
  const y = (v: number) => PAD.top + plotH - ((v - min) / span) * plotH

  const path = data
    .map((d, i) => `${i === 0 ? 'M' : 'L'} ${x(i)} ${y(Number(d.net_worth))}`)
    .join(' ')

  return (
    <div className="overflow-x-auto">
      <svg viewBox={`0 0 ${W} ${H}`}         className="w-full max-sm:min-w-0 sm:min-w-[520px]" role="img"
        aria-label="Net worth over time">
        {/* A zero line, because net worth can legitimately be negative and the
            sign is the most important thing on the chart. */}
        {min < 0 && (
          <line x1={PAD.left} x2={W - PAD.right} y1={y(0)} y2={y(0)}
            stroke={CHART.axis} strokeWidth={1} strokeDasharray="3 3" />
        )}
        <text x={PAD.left - 10} y={y(max) + 4} textAnchor="end" fontSize="11" fill={CHART.textMuted}>
          {formatMoney(String(max))}
        </text>
        <text x={PAD.left - 10} y={y(min) + 4} textAnchor="end" fontSize="11" fill={CHART.textMuted}>
          {formatMoney(String(min))}
        </text>
        <motion.path
          fill="none"
          stroke={SERIES.leftover}
          strokeWidth={2}
          {...lineDraw(path, reduce)}
        />
        {data.map((d, i) => (
          <circle key={d.as_of} cx={x(i)} cy={y(Number(d.net_worth))} r={4}
            fill={SERIES.leftover} stroke={CHART.surface} strokeWidth={2}>
            <title>{`${d.as_of}: ${formatMoney(d.net_worth)}`}</title>
          </circle>
        ))}
        <text x={PAD.left} y={H - 6} fontSize="11" fill={CHART.textMuted}>{data[0].as_of}</text>
        <text x={W - PAD.right} y={H - 6} textAnchor="end" fontSize="11" fill={CHART.textMuted}>
          {data[data.length - 1].as_of}
        </text>
      </svg>
    </div>
  )
}

function ManualAssets({ assets }: { assets: import('../lib/api').ManualAsset[] }) {
  const qc = useQueryClient()
  const [name, setName] = useState('')
  const [value, setValue] = useState('')
  const [kind, setKind] = useState('home')
  const [isLiability, setIsLiability] = useState(false)

  const refresh = () => {
    qc.invalidateQueries({ queryKey: ['manual-assets'] })
    qc.invalidateQueries({ queryKey: ['networth'] })
  }

  const create = useMutation({
    mutationFn: api.createManualAsset,
    onSuccess: () => {
      setName('')
      setValue('')
      refresh()
    },
  })
  const remove = useMutation({ mutationFn: api.deleteManualAsset, onSuccess: refresh })

  function onSubmit(e: FormEvent) {
    e.preventDefault()
    create.mutate({ name, kind, value, is_liability: isLiability })
  }

  return (
    <section className="glass p-6">
      <h2 className="text-lg font-medium">Manual assets</h2>
      <p className="mt-1 mb-4 text-sm text-mist-300">
        Things Plaid cannot see — home equity, vehicles, a private loan.
      </p>

      {assets.length > 0 && (
        <ul className="mb-5 divide-y divide-white/5">
          {assets.map((a) => (
            <li key={a.id} className="flex items-center gap-4 py-2.5 text-sm">
              <span className="font-medium">{a.name}</span>
              <span className="text-xs text-mist-500">{a.kind}</span>
              <span
                className="tabular ml-auto"
                style={{ color: a.is_liability ? STATUS.critical : undefined }}
              >
                {a.is_liability ? '−' : ''}
                {formatMoney(a.value)}
              </span>
              {/* Title deeds, valuations and purchase paperwork belong with the
                  asset they describe. */}
              <AttachDocuments
                target={{ kind: 'manual_asset', id: a.id }}
                label="Attach a document"
              />
              <button
                className="text-xs text-mist-500 transition hover:text-ember-400"
                onClick={() => remove.mutate(a.id)}
              >
                Remove
              </button>
            </li>
          ))}
        </ul>
      )}

      <form onSubmit={onSubmit} className="flex flex-wrap items-end gap-3">
        <div className="min-w-[10rem] flex-1">
          <label className="label" htmlFor="asset-name">Name</label>
          <input id="asset-name" className="field" required value={name}
            onChange={(e) => setName(e.target.value)} placeholder="Home" />
        </div>
        <div>
          <label className="label" htmlFor="asset-kind">Kind</label>
          <select id="asset-kind" className="field" value={kind}
            onChange={(e) => setKind(e.target.value)}>
            <option value="home">Home</option>
            <option value="vehicle">Vehicle</option>
            <option value="cash">Cash</option>
            <option value="collectible">Collectible</option>
            <option value="other">Other</option>
          </select>
        </div>
        <div>
          <label className="label" htmlFor="asset-value">Value</label>
          {/* Sent as a string: a JSON number would drag it through a float. */}
          <input id="asset-value" className="field" required inputMode="decimal"
            value={value} onChange={(e) => setValue(e.target.value)} placeholder="425000.00" />
        </div>
        <label className="flex items-center gap-2 pb-3 text-sm text-mist-300">
          <input type="checkbox" className="accent-arcane-500" checked={isLiability}
            onChange={(e) => setIsLiability(e.target.checked)} />
          This is a debt
        </label>
        <button type="submit" className="btn-primary mb-0.5" disabled={create.isPending}>
          {create.isPending ? 'Adding…' : 'Add'}
        </button>
      </form>

      {create.isError && (
        <p role="alert" className="mt-3 rounded-xl border border-ember-400/30 bg-ember-400/10 px-4 py-2.5 text-sm text-ember-400">
          {create.error.message}
        </p>
      )}
    </section>
  )
}

function Tile({
  label, value, tone, large, format, fallback = '—',
}: {
  label: string
  value: string | number | null | undefined
  tone?: 'good' | 'debt'
  large?: boolean
  format?: (n: number) => string
  fallback?: string
}) {
  const color = tone === 'debt' ? STATUS.critical : tone === 'good' ? STATUS.good : '#f2d492'
  return (
    <div className="glass p-5">
      <p className="text-sm text-mist-300">{label}</p>
      <p className={`tabular mt-2 font-semibold ${large ? 'text-4xl' : 'text-3xl'}`} style={{ color }}>
        <AnimatedNumber value={value} format={format} fallback={fallback} />
      </p>
    </div>
  )
}

function Row({ label, value, debt }: { label: string; value: string; debt?: boolean }) {
  return (
    <div className="flex items-baseline justify-between text-sm">
      <span className="text-mist-300">{label}</span>
      <span className="tabular" style={{ color: debt ? STATUS.critical : undefined }}>
        {formatMoney(value)}
      </span>
    </div>
  )
}

/** Trims trailing zeros from a share quantity: "213.0000000000" -> "213". */
function trimQuantity(q: string): string {
  if (!q.includes('.')) return q
  return q.replace(/\.?0+$/, '')
}
