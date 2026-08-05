import { useState, type FormEvent } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  api,
  type AssetDetail,
  type AssetValuation,
  type Liability,
  type ManualAsset,
} from '../lib/api'
import { formatMoney } from '../lib/money'
import { CHART, SERIES, STATUS } from './charts/tokens'

/**
 * The expanded panel behind one manual asset: what it is, what it has been
 * worth, and how to update it.
 *
 * The rule this file exists to make visible — and the one to preserve when
 * editing it — is that AN ESTIMATE IS A PROPOSAL. The suggestion box shows a
 * figure, the curve behind it, and two buttons. Nothing here changes a value
 * until the user presses one. A bond is the deliberate exception and says so:
 * its value is arithmetic over published rates, not a guess, so the app keeps
 * it current on its own.
 */
export function AssetDetailPanel({
  asset,
  liabilities,
}: {
  asset: ManualAsset
  liabilities: Liability[]
}) {
  const detail = useQuery({
    queryKey: ['asset-detail', asset.id],
    queryFn: () => api.assetDetail(asset.id),
  })
  const valuations = useQuery({
    queryKey: ['asset-valuations', asset.id],
    queryFn: () => api.assetValuations(asset.id),
  })

  const isBond = asset.kind === 'bond' || !!asset.bond_series

  return (
    <div className="space-y-6 border-t border-white/5 bg-white/[0.015] px-6 py-5">
      {isBond ? (
        <BondValuation assetID={asset.id} />
      ) : (
        <RevaluationFlow asset={asset} />
      )}

      <ValueHistory history={valuations.data ?? []} loading={valuations.isLoading} />

      <LoanLink asset={asset} liabilities={liabilities} />

      {detail.data && <DetailForm asset={asset} detail={detail.data} kind={asset.kind} />}
    </div>
  )
}

/* -------------------------------------------------------------------------
 * Revaluation
 * ---------------------------------------------------------------------- */

/**
 * Current value, the app's suggestion where it has one, and a field to enter
 * your own. The suggestion is fetched but never applied automatically.
 */
function RevaluationFlow({ asset }: { asset: ManualAsset }) {
  const qc = useQueryClient()
  const [value, setValue] = useState('')

  const suggestion = useQuery({
    queryKey: ['asset-suggestion', asset.id],
    queryFn: () => api.assetSuggestion(asset.id),
  })

  const record = useMutation({
    mutationFn: (input: { value: string; source: string; note?: string }) =>
      api.recordValuation(asset.id, input),
    onSuccess: () => {
      setValue('')
      qc.invalidateQueries({ queryKey: ['manual-assets'] })
      qc.invalidateQueries({ queryKey: ['networth'] })
      qc.invalidateQueries({ queryKey: ['asset-valuations', asset.id] })
      qc.invalidateQueries({ queryKey: ['asset-suggestion', asset.id] })
    },
  })

  const s = suggestion.data

  return (
    <section>
      <h3 className="text-sm font-medium">Update its value</h3>
      <p className="mt-1 text-xs text-mist-500">
        Recorded as of today, and kept in the history below so the trend
        survives.
      </p>

      {s?.ok && s.value && (
        <div className="mt-3 rounded-xl border border-arcane-500/25 bg-arcane-500/5 p-4">
          <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
            <span className="text-xs tracking-wide text-mist-500 uppercase">
              Suggested
            </span>
            <span className="tabular text-lg font-medium">{formatMoney(s.value)}</span>
            {s.change && (
              <span
                className="tabular text-sm"
                style={{
                  color: Number(s.change) < 0 ? STATUS.critical : STATUS.good,
                }}
              >
                {Number(s.change) >= 0 ? '+' : ''}
                {formatMoney(s.change)}
              </span>
            )}
          </div>

          {/* The basis is shown in full rather than behind a tooltip. A
              depreciation figure the user cannot interrogate is
              indistinguishable from a guess. */}
          <p className="mt-2 text-xs leading-relaxed text-mist-300">{s.basis}</p>

          <div className="mt-3 flex flex-wrap gap-2">
            <button
              className="btn-primary text-xs"
              disabled={record.isPending}
              onClick={() =>
                record.mutate({
                  value: s.value!,
                  source: 'estimated',
                  note: s.basis,
                })
              }
            >
              {record.isPending ? 'Saving…' : 'Accept this figure'}
            </button>
            <button
              className="btn-ghost text-xs"
              onClick={() => setValue(s.value ?? '')}
            >
              Edit before saving
            </button>
          </div>
        </div>
      )}

      {s && !s.ok && s.reason && (
        <p className="mt-3 text-xs text-mist-500">{s.reason}</p>
      )}

      <form
        className="mt-4 flex flex-wrap items-end gap-3"
        onSubmit={(e: FormEvent) => {
          e.preventDefault()
          record.mutate({ value, source: 'manual' })
        }}
      >
        <div>
          <label className="label" htmlFor={`value-${asset.id}`}>
            New value
          </label>
          {/* Sent as a string: a JSON number would drag it through a float. */}
          <input
            id={`value-${asset.id}`}
            className="field"
            required
            inputMode="decimal"
            value={value}
            onChange={(e) => setValue(e.target.value)}
            placeholder={asset.value}
          />
        </div>
        <button type="submit" className="btn-ghost mb-0.5 text-sm" disabled={record.isPending}>
          {record.isPending ? 'Saving…' : 'Save'}
        </button>
      </form>

      {record.isError && (
        <p role="alert" className="mt-2 text-xs" style={{ color: STATUS.critical }}>
          {record.error.message}
        </p>
      )}
    </section>
  )
}

/* -------------------------------------------------------------------------
 * Bonds
 * ---------------------------------------------------------------------- */

/**
 * A bond's value, with the published rates that produced it.
 *
 * Redemption value is the headline because it is what enters net worth — what
 * the bond could actually be turned into today. Accrued value sits beside it,
 * labelled, whenever the two differ, so the three months of interest a young
 * bond forfeits is visible rather than swallowed.
 */
function BondValuation({ assetID }: { assetID: string }) {
  const [showRates, setShowRates] = useState(false)
  const bond = useQuery({
    queryKey: ['bond-value', assetID],
    queryFn: () => api.bondValue(assetID),
    retry: false,
  })

  if (bond.isLoading) {
    return <p className="text-xs text-mist-500">Valuing…</p>
  }
  if (bond.isError) {
    return (
      <p className="text-xs text-mist-500">
        {bond.error.message} — add the series, issue date and what you paid.
      </p>
    )
  }

  const b = bond.data
  if (!b) return null

  const differs = b.accrued_value !== b.redemption_value

  return (
    <section>
      <div className="flex flex-wrap items-baseline gap-x-6 gap-y-2">
        <div>
          <p className="text-xs tracking-wide text-mist-500 uppercase">
            Redemption value
          </p>
          <p className="tabular text-2xl font-semibold">
            {formatMoney(b.redemption_value)}
          </p>
        </div>
        {differs && (
          <div>
            <p className="text-xs tracking-wide text-mist-500 uppercase">Accrued</p>
            <p className="tabular text-lg text-mist-300">
              {formatMoney(b.accrued_value)}
            </p>
          </div>
        )}
      </div>

      {b.penalty_applied && (
        <p className="mt-2 text-xs text-mist-300">
          Under five years old: cashing it now forfeits the last three months of
          interest, which is the gap between the two figures. Net worth counts
          the redemption value — what you could actually get for it today.
        </p>
      )}

      {b.months_to_doubling !== undefined && b.months_to_doubling > 0 && (
        <p className="mt-2 text-xs text-mist-300">
          Treasury guarantees this bond is worth at least twice what you paid at
          20 years — {b.months_to_doubling} month
          {b.months_to_doubling === 1 ? '' : 's'} away. The value steps up then
          rather than climbing to it.
        </p>
      )}

      {b.doubling_applied && (
        <p className="mt-2 text-xs" style={{ color: STATUS.good }}>
          The 20-year doubling guarantee has been applied.
        </p>
      )}

      {b.matured && (
        <p className="mt-2 text-xs text-mist-300">
          Reached final maturity{b.final_maturity ? ` in ${b.final_maturity}` : ''} and
          is no longer earning interest.
        </p>
      )}

      {/* A refusal, stated rather than papered over with a plausible number. */}
      {!b.ok && (
        <p
          className="mt-3 rounded-xl border px-3 py-2 text-xs"
          style={{ borderColor: `${STATUS.warning}55`, color: STATUS.warning }}
        >
          Valued through {b.valued_through}. {b.reason}
        </p>
      )}

      {b.basis && <p className="mt-3 text-xs leading-relaxed text-mist-500">{b.basis}</p>}

      {b.rates.length > 0 && (
        <div className="mt-3">
          <button
            className="text-xs text-mist-500 underline transition hover:text-mist-300"
            onClick={() => setShowRates((v) => !v)}
          >
            {showRates ? 'Hide' : 'Show'} the {b.rates.length} rate periods behind this
          </button>

          {showRates && (
            <div className="mt-2 overflow-x-auto">
              <table className="w-full text-xs">
                <thead>
                  <tr className="border-y border-white/5 text-left text-mist-500">
                    <th className="py-1.5 pr-4 font-medium">Six months from</th>
                    <th className="py-1.5 pr-4 font-medium">Rate announced</th>
                    <th className="py-1.5 pr-4 text-right font-medium">Fixed</th>
                    <th className="py-1.5 pr-4 text-right font-medium">Inflation</th>
                    <th className="py-1.5 text-right font-medium">Composite</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-white/5">
                  {b.rates.map((r) => (
                    <tr key={r.period_start}>
                      <td className="py-1.5 pr-4">{r.period_start}</td>
                      <td className="py-1.5 pr-4 text-mist-500">{r.announced}</td>
                      <td className="tabular py-1.5 pr-4 text-right">{r.fixed_rate}%</td>
                      <td className="tabular py-1.5 pr-4 text-right">
                        {r.inflation_rate === null ? '—' : `${r.inflation_rate}%`}
                      </td>
                      <td className="tabular py-1.5 text-right">{r.composite_rate}%</td>
                    </tr>
                  ))}
                </tbody>
              </table>
              <p className="mt-2 text-[11px] text-mist-500">
                Every rate here is published by the Treasury and can be checked
                at treasurydirect.gov. Nothing is estimated.
              </p>
            </div>
          )}
        </div>
      )}
    </section>
  )
}

/* -------------------------------------------------------------------------
 * History
 * ---------------------------------------------------------------------- */

/**
 * The value trend for one asset. A home that appreciated $80k over five years
 * should be visible as a trend, not a single number.
 */
function ValueHistory({
  history,
  loading,
}: {
  history: AssetValuation[]
  loading: boolean
}) {
  if (loading) return null

  return (
    <section>
      <h3 className="text-sm font-medium">Value over time</h3>
      {history.length < 2 ? (
        <p className="mt-1 text-xs text-mist-500">
          {history.length === 1
            ? 'One reading so far — the trend appears once there are at least two.'
            : 'No readings yet.'}
        </p>
      ) : (
        <>
          <HistoryChart history={history} />
          <ul className="mt-3 divide-y divide-white/5 text-xs">
            {[...history].reverse().slice(0, 5).map((v) => (
              <li key={v.as_of} className="flex items-baseline gap-3 py-1.5">
                <span className="text-mist-500">{v.as_of}</span>
                <span className="tabular">{formatMoney(v.value)}</span>
                <span className="ml-auto text-mist-500">{sourceLabel(v.source)}</span>
              </li>
            ))}
          </ul>
        </>
      )}
    </section>
  )
}

function HistoryChart({ history }: { history: AssetValuation[] }) {
  const W = 640
  const H = 140
  const PAD = { top: 10, right: 10, bottom: 20, left: 66 }
  const plotW = W - PAD.left - PAD.right
  const plotH = H - PAD.top - PAD.bottom

  const values = history.map((h) => Number(h.value))
  const lo = Math.min(...values)
  const hi = Math.max(...values)
  // A line encodes value by position, so fitting the domain to the data is
  // legitimate — and necessary, because an asset's value moves by a few percent
  // and anchoring to zero would flatten every change out of existence.
  const margin = (hi - lo || Math.abs(hi) || 1) * 0.15
  const min = lo - margin
  const max = hi + margin
  const span = max - min || 1

  const x = (i: number) => PAD.left + (i / (history.length - 1)) * plotW
  const y = (v: number) => PAD.top + plotH - ((v - min) / span) * plotH

  const path = history
    .map((h, i) => `${i === 0 ? 'M' : 'L'} ${x(i)} ${y(Number(h.value))}`)
    .join(' ')

  return (
    <div className="mt-2 overflow-x-auto">
      <svg viewBox={`0 0 ${W} ${H}`} className="w-full" role="img" aria-label="Asset value over time">
        <text x={PAD.left - 8} y={y(max) + 4} textAnchor="end" fontSize="10" fill={CHART.textMuted}>
          {formatMoney(String(max))}
        </text>
        <text x={PAD.left - 8} y={y(min) + 4} textAnchor="end" fontSize="10" fill={CHART.textMuted}>
          {formatMoney(String(min))}
        </text>
        <path d={path} fill="none" stroke={SERIES.leftover} strokeWidth={2} />
        {history.map((h, i) => (
          <circle
            key={h.as_of}
            cx={x(i)}
            cy={y(Number(h.value))}
            r={3}
            fill={SERIES.leftover}
            stroke={CHART.surface}
            strokeWidth={2}
          >
            <title>{`${h.as_of}: ${formatMoney(h.value)} (${sourceLabel(h.source)})`}</title>
          </circle>
        ))}
        <text x={PAD.left} y={H - 5} fontSize="10" fill={CHART.textMuted}>
          {history[0].as_of}
        </text>
        <text x={W - PAD.right} y={H - 5} textAnchor="end" fontSize="10" fill={CHART.textMuted}>
          {history[history.length - 1].as_of}
        </text>
      </svg>
    </div>
  )
}

function sourceLabel(source: string): string {
  switch (source) {
    case 'manual':
      return 'you entered it'
    case 'estimated':
      return 'computed'
    case 'api':
      return 'external source'
    default:
      return source
  }
}

/* -------------------------------------------------------------------------
 * Loan link and equity
 * ---------------------------------------------------------------------- */

/**
 * Ties an asset to the debt secured against it.
 *
 * Equity is shown, never summed. Net worth already counts the asset on one side
 * and the loan on the other, so adding equity to a total would count the asset
 * twice.
 */
function LoanLink({ asset, liabilities }: { asset: ManualAsset; liabilities: Liability[] }) {
  const qc = useQueryClient()

  const link = useMutation({
    mutationFn: (id: string | null) => api.linkAssetLoan(asset.id, id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['manual-assets'] }),
  })

  return (
    <section>
      <h3 className="text-sm font-medium">Loan against it</h3>

      {asset.equity !== undefined && (
        <div className="mt-2">
          <div className="flex flex-wrap items-baseline gap-x-3 text-sm">
            <span className="tabular font-medium">{formatMoney(asset.equity)}</span>
            <span className="text-xs text-mist-500">
              of {formatMoney(asset.value)} owned
              {asset.loan_balance && `, ${formatMoney(asset.loan_balance)} still owed`}
            </span>
          </div>

          {asset.underwater ? (
            <p className="mt-1 text-xs" style={{ color: STATUS.critical }}>
              You owe more on this than it is worth.
            </p>
          ) : (
            <div className="mt-2 h-2 overflow-hidden rounded-full bg-white/5">
              <span
                className="block h-full rounded-full"
                style={{
                  width: `${Number(asset.paid_fraction ?? 0) * 100}%`,
                  backgroundColor: STATUS.good,
                }}
              />
            </div>
          )}

          <p className="mt-1.5 text-[11px] text-mist-500">
            A view of two figures net worth already counts separately — equity is
            not added on top.
          </p>
        </div>
      )}

      <select
        className="field mt-3 max-w-sm"
        value={asset.loan_account_id ?? ''}
        onChange={(e) => link.mutate(e.target.value || null)}
        disabled={link.isPending}
      >
        <option value="">No loan linked</option>
        {liabilities.map((l) => (
          <option key={l.id} value={l.id}>
            {l.account_name}
          </option>
        ))}
      </select>
    </section>
  )
}

/* -------------------------------------------------------------------------
 * Class-specific detail forms
 * ---------------------------------------------------------------------- */

/** The form is driven by `kind`, so a bond never shows an odometer field. */
function DetailForm({
  asset,
  detail,
  kind,
}: {
  asset: ManualAsset
  detail: AssetDetail
  kind: string
}) {
  const qc = useQueryClient()
  const [form, setForm] = useState<Partial<AssetDetail>>(detail)

  const save = useMutation({
    mutationFn: (input: Partial<AssetDetail>) => api.saveAssetDetail(asset.id, input),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['asset-detail', asset.id] })
      qc.invalidateQueries({ queryKey: ['asset-suggestion', asset.id] })
      qc.invalidateQueries({ queryKey: ['bond-value', asset.id] })
      qc.invalidateQueries({ queryKey: ['manual-assets'] })
    },
  })

  const set = (patch: Partial<AssetDetail>) => setForm((f) => ({ ...f, ...patch }))
  const num = (v: string) => (v === '' ? null : Number(v))

  const isBond = kind === 'bond' || !!form.bond_series
  const paperEEWarning =
    form.bond_series === 'ee_savings' &&
    form.purchase_price &&
    form.face_value &&
    Number(form.purchase_price) === Number(form.face_value)

  return (
    <section>
      <h3 className="text-sm font-medium">Details</h3>

      <form
        className="mt-3 grid gap-3 sm:grid-cols-3"
        onSubmit={(e: FormEvent) => {
          e.preventDefault()
          save.mutate(form)
        }}
      >
        {kind === 'home' && (
          <>
            <Field label="Address" className="sm:col-span-3">
              <input className="field" value={form.address ?? ''}
                onChange={(e) => set({ address: e.target.value })} />
            </Field>
            <Field label="Beds">
              <input className="field" inputMode="decimal" value={form.beds ?? ''}
                onChange={(e) => set({ beds: e.target.value || null })} />
            </Field>
            <Field label="Baths">
              <input className="field" inputMode="decimal" value={form.baths ?? ''}
                onChange={(e) => set({ baths: e.target.value || null })} />
            </Field>
            <Field label="Square feet">
              <input className="field" inputMode="numeric" value={form.sqft ?? ''}
                onChange={(e) => set({ sqft: num(e.target.value) })} />
            </Field>
          </>
        )}

        {kind === 'vehicle' && (
          <>
            <Field label="Model year">
              <input className="field" inputMode="numeric" value={form.year ?? ''}
                onChange={(e) => set({ year: num(e.target.value) })} />
            </Field>
            <Field label="Make">
              <input className="field" value={form.make ?? ''}
                onChange={(e) => set({ make: e.target.value })} />
            </Field>
            <Field label="Model">
              <input className="field" value={form.model ?? ''}
                onChange={(e) => set({ model: e.target.value })} />
            </Field>
            <Field label="Mileage">
              <input className="field" inputMode="numeric" value={form.mileage ?? ''}
                onChange={(e) => set({ mileage: num(e.target.value) })} />
            </Field>
            <Field label="Miles a year" hint="Used to age the odometer between updates.">
              <input className="field" inputMode="numeric" value={form.annual_mileage ?? ''}
                onChange={(e) => set({ annual_mileage: num(e.target.value) })} />
            </Field>
            <Field label="Condition">
              <select className="field" value={form.condition ?? ''}
                onChange={(e) => set({ condition: e.target.value || null })}>
                <option value="">—</option>
                <option value="excellent">Excellent</option>
                <option value="good">Good</option>
                <option value="fair">Fair</option>
                <option value="poor">Poor</option>
              </select>
            </Field>
          </>
        )}

        {isBond && (
          <>
            <Field label="Series">
              <select className="field" value={form.bond_series ?? ''}
                onChange={(e) => set({ bond_series: e.target.value || null })}>
                <option value="">—</option>
                <option value="i_savings">Series I savings bond</option>
                <option value="ee_savings">Series EE savings bond</option>
                <option value="treasury">Marketable Treasury</option>
                <option value="other">Other</option>
              </select>
            </Field>
            <Field label="Issue date">
              <input className="field" type="date" value={form.issue_date ?? ''}
                onChange={(e) => set({ issue_date: e.target.value || null })} />
            </Field>
            <Field label="Face value" hint="The denomination printed on it.">
              <input className="field" inputMode="decimal" value={form.face_value ?? ''}
                onChange={(e) => set({ face_value: e.target.value || null })} />
            </Field>
            <Field label="What you paid" hint="Not the face value, if they differ.">
              <input className="field" inputMode="decimal" value={form.purchase_price ?? ''}
                onChange={(e) => set({ purchase_price: e.target.value || null })} />
            </Field>

            {form.bond_series === 'treasury' && (
              <>
                <Field label="Coupon rate %">
                  <input className="field" inputMode="decimal" value={form.coupon_rate ?? ''}
                    onChange={(e) => set({ coupon_rate: e.target.value || null })} />
                </Field>
                <Field label="Maturity date">
                  <input className="field" type="date" value={form.maturity_date ?? ''}
                    onChange={(e) => set({ maturity_date: e.target.value || null })} />
                </Field>
              </>
            )}

            {/* The single most common way a savings bond ends up recorded at
                twice its cost. Worth interrupting for. */}
            {paperEEWarning && (
              <p
                className="text-xs sm:col-span-3"
                style={{ color: STATUS.warning }}
              >
                Paper EE bonds were sold at <strong>half</strong> their face
                value — a $100 bond cost $50. If this is a paper bond, what you
                paid should be half the face value, or it will be valued at
                twice what it is worth.
              </p>
            )}
          </>
        )}

        <div className="sm:col-span-3">
          <button type="submit" className="btn-ghost text-sm" disabled={save.isPending}>
            {save.isPending ? 'Saving…' : 'Save details'}
          </button>
          {save.isError && (
            <span className="ml-3 text-xs" style={{ color: STATUS.critical }}>
              {save.error.message}
            </span>
          )}
        </div>
      </form>
    </section>
  )
}

function Field({
  label,
  hint,
  className,
  children,
}: {
  label: string
  hint?: string
  className?: string
  children: React.ReactNode
}) {
  return (
    <div className={className}>
      <label className="label">{label}</label>
      {children}
      {hint && <p className="mt-1 text-[11px] text-mist-500">{hint}</p>}
    </div>
  )
}
