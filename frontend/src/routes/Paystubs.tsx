import { useMemo, useRef, useState } from 'react'
import type { ReactNode } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, ApiError } from '../lib/api'
import type {
  ContributionHeadroom,
  Employer,
  EmployerInput,
  PayFrequency,
  Paystub,
  PaystubInput,
  PaystubLineInput,
  PaystubProposal,
  PayrollCategory,
  PayrollSummary,
} from '../lib/api'
import { formatDate, formatMoney } from '../lib/money'
import { PaycheckBreakdown } from '../components/charts/PaycheckBreakdown'
import { EmptyState } from '../components/EmptyState'
import { Reveal, SkeletonRows, SkeletonTiles } from '../components/Skeleton'
import { Tile } from '../components/Tile'
import { STATUS } from '../components/charts/tokens'

/**
 * Paystubs — the pre-tax side of the ledger.
 *
 * Everything the rest of this app shows is post-tax: Plaid reports the deposit
 * that lands, which is what survived withholding, retirement deferrals and
 * premiums. This page is the record of the other 30–45%.
 *
 * Two rules the UI has to carry faithfully, because the server enforces both
 * and a client that implied otherwise would be lying:
 *
 *   * An UNCONFIRMED stub counts for nothing. It is not in the savings rate,
 *     the tax summary or the contribution totals. The review queue is where it
 *     lives until somebody says the figures are right.
 *   * A stub that does not BALANCE cannot be confirmed. Gross minus the
 *     deductions has to equal net, within a cent; the form shows the gap live
 *     so the missing line is obvious before the save is attempted.
 */
export function Paystubs() {
  const [year, setYear] = useState<number>(() => new Date().getFullYear())

  const years = useQuery({ queryKey: ['paystub-years'], queryFn: api.paystubYears })
  const summary = useQuery({
    queryKey: ['payroll-summary', year],
    queryFn: () => api.payrollSummary(year),
  })
  const stubs = useQuery({
    queryKey: ['paystubs', year],
    queryFn: () => api.paystubs(year),
  })
  const employers = useQuery({ queryKey: ['employers'], queryFn: api.employers })
  const taxonomy = useQuery({
    queryKey: ['payroll-taxonomy'],
    queryFn: api.payrollTaxonomy,
    staleTime: Infinity,
  })

  const rows = stubs.data ?? []
  const pending = rows.filter((s) => s.is_own && !s.confirmed)
  const confirmed = rows.filter((s) => s.confirmed)

  // The year list only holds years with CONFIRMED stubs, so a first import
  // would otherwise have nowhere to be selected from.
  const yearOptions = useMemo(() => {
    const set = new Set<number>(years.data ?? [])
    set.add(new Date().getFullYear())
    set.add(year)
    return [...set].sort((a, b) => b - a)
  }, [years.data, year])

  return (
    <div className="space-y-8">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold">Paystubs</h1>
          <p className="mt-1 max-w-2xl text-mist-300">
            What you earned before anything came out of it. Your accounts only
            ever see the deposit, so withholding, retirement contributions and
            premiums are invisible everywhere else in the app.
          </p>
        </div>
        <label className="flex items-center gap-2 text-sm text-mist-300">
          Tax year
          <select
            className="input w-28"
            value={year}
            onChange={(e) => setYear(Number(e.target.value))}
          >
            {yearOptions.map((y) => (
              <option key={y} value={y}>
                {y}
              </option>
            ))}
          </select>
        </label>
      </div>

      {summary.isPending ? (
        <SkeletonTiles count={4} />
      ) : summary.data ? (
        <SummaryTiles summary={summary.data} />
      ) : null}

      {summary.data && summary.data.has_data && (
        <HeadroomPanel summary={summary.data} />
      )}

      {pending.length > 0 && (
        <section className="glass border border-amber-400/20 p-6">
          <h2 className="mb-1 text-lg font-medium">Waiting for review</h2>
          <p className="mb-5 text-sm text-mist-300">
            These count for nothing until you confirm them — not in your savings
            rate, your tax summary or your contribution totals. Check the
            figures against the stub and confirm.
          </p>
          <div className="space-y-3">
            {pending.map((s) => (
              <PaystubCard key={s.id} stub={s} year={year} categories={taxonomy.data?.categories ?? []} employers={employers.data ?? []} defaultOpen />
            ))}
          </div>
        </section>
      )}

      <section className="glass p-6">
        <h2 className="mb-1 text-lg font-medium">Your paystubs</h2>
        <p className="mb-5 text-sm text-mist-300">
          Confirmed stubs, newest first. Everything on this page is computed
          from these.
        </p>

        {stubs.isPending ? (
          <SkeletonRows count={3} />
        ) : confirmed.length === 0 ? (
          <EmptyState title="No confirmed paystubs yet">
            Import a PDF stub or type one in below. Nothing is sent anywhere to
            read a PDF — the text is pulled out on this machine.
          </EmptyState>
        ) : (
          <Reveal>
            <div className="space-y-3">
              {confirmed.map((s) => (
                <PaystubCard
                  key={s.id}
                  stub={s}
                  year={year}
                  categories={taxonomy.data?.categories ?? []}
                  employers={employers.data ?? []}
                />
              ))}
            </div>
          </Reveal>
        )}
      </section>

      <AddPaystub
        year={year}
        employers={employers.data ?? []}
        categories={taxonomy.data?.categories ?? []}
      />

      <EmployersPanel
        employers={employers.data ?? []}
        frequencies={taxonomy.data?.pay_frequencies ?? []}
      />

      {summary.data?.has_data && <TaxSummaryPanel year={year} />}
    </div>
  )
}

// ---------------------------------------------------------------------------
// Summary
// ---------------------------------------------------------------------------

function SummaryTiles({ summary }: { summary: PayrollSummary }) {
  if (!summary.has_data) {
    return (
      <section className="glass p-6">
        <EmptyState title={`Nothing recorded for ${summary.tax_year}`}>
          {summary.unconfirmed_count > 0
            ? `${summary.unconfirmed_count} stub${summary.unconfirmed_count === 1 ? '' : 's'} are waiting for review. Confirmed stubs are the only ones that count towards anything.`
            : 'Add a paystub below and this fills in.'}
        </EmptyState>
      </section>
    )
  }

  const rate = summary.effective_tax_rate

  return (
    <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
      <Tile
        label="Gross pay"
        value={summary.gross}
        format={(n) => formatMoney(String(n))}
        hint={`${summary.paystub_count} paystub${summary.paystub_count === 1 ? '' : 's'}`}
      />
      <Tile
        label="Take-home"
        value={summary.net}
        format={(n) => formatMoney(String(n))}
        hint="What actually reached your accounts"
      />
      <Tile
        label="Effective tax rate"
        value={rate === null ? null : Number(rate) * 100}
        format={(n) => `${n.toFixed(1)}%`}
        hint="Income tax and FICA over gross"
      />
      <Tile
        label="Total compensation"
        value={summary.total_compensation}
        format={(n) => formatMoney(String(n))}
        hint={
          Number(summary.employer_total) > 0
            ? `Includes ${formatMoney(summary.employer_total)} your employer paid`
            : 'Gross plus anything your employer added'
        }
      />
    </div>
  )
}

function HeadroomPanel({ summary }: { summary: PayrollSummary }) {
  return (
    <section className="glass p-6">
      <h2 className="mb-1 text-lg font-medium">Contribution room</h2>
      <p className="mb-5 text-sm text-mist-300">
        Against the {summary.tax_year} IRS annual limits. A traditional and a
        Roth account share one limit, and so do two jobs in the same year — this
        pools them.
      </p>

      {!summary.limits_configured ? (
        <p className="rounded-lg border border-amber-400/20 bg-amber-400/5 p-4 text-sm text-mist-200">
          This app does not have the IRS limits for {summary.tax_year} yet — we
          have them through {summary.latest_limit_year}. Rather than measure
          your contributions against another year's numbers, no room is shown.
        </p>
      ) : (
        <>
          <div className="space-y-4">
            {summary.headroom.map((h) => (
              <HeadroomRow key={h.group} headroom={h} />
            ))}
          </div>
          {!summary.age_known && (
            <p className="mt-4 text-xs text-mist-500">
              No birthdate on file, so no catch-up allowance is included. If
              you're 50 or over your real limit is higher — add a birthdate in
              Household and this updates.
            </p>
          )}
        </>
      )}
    </section>
  )
}

function HeadroomRow({ headroom }: { headroom: ContributionHeadroom }) {
  const contributed = Number(headroom.contributed)
  const limit = Number(headroom.limit)
  // Display-only: a bar width from two server-exact figures.
  const pct = limit > 0 ? Math.min((contributed / limit) * 100, 100) : 0
  const over = Number(headroom.over_by) > 0

  return (
    <div>
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <p className="font-medium">{headroom.label}</p>
        <p className="tabular text-sm text-mist-300">
          {formatMoney(headroom.contributed)} of {formatMoney(headroom.limit)}
        </p>
      </div>
      <div className="mt-2 h-2 overflow-hidden rounded-full bg-white/5">
        <div
          className="h-full rounded-full"
          style={{
            width: `${pct}%`,
            backgroundColor: over ? STATUS.critical : STATUS.good,
          }}
        />
      </div>
      <p className="mt-1.5 text-xs text-mist-400">
        {over ? (
          <span style={{ color: STATUS.critical }}>
            {formatMoney(headroom.over_by)} over the limit. An excess deferral
            has to be withdrawn before the filing deadline or it is taxed twice
            — talk to your payroll department.
          </span>
        ) : headroom.periods_left && headroom.per_period ? (
          <>
            {formatMoney(headroom.remaining)} to go, {headroom.periods_left} pay
            period{headroom.periods_left === 1 ? '' : 's'} left —{' '}
            <span className="tabular">{formatMoney(headroom.per_period)}</span>{' '}
            a paycheque to max it.
          </>
        ) : (
          <>{formatMoney(headroom.remaining)} to go.</>
        )}
      </p>
    </div>
  )
}

// ---------------------------------------------------------------------------
// One paystub
// ---------------------------------------------------------------------------

function PaystubCard({
  stub,
  year,
  categories,
  employers,
  defaultOpen = false,
}: {
  stub: Paystub
  year: number
  categories: PayrollCategory[]
  employers: Employer[]
  defaultOpen?: boolean
}) {
  const qc = useQueryClient()
  const [open, setOpen] = useState(defaultOpen)
  const [editing, setEditing] = useState(false)
  const [error, setError] = useState('')

  const refresh = () => {
    qc.invalidateQueries({ queryKey: ['paystubs'] })
    qc.invalidateQueries({ queryKey: ['payroll-summary'] })
    qc.invalidateQueries({ queryKey: ['paystub-years'] })
    qc.invalidateQueries({ queryKey: ['payroll-tax-summary'] })
  }

  const confirm = useMutation({
    mutationFn: (confirmed: boolean) => api.confirmPaystub(stub.id, confirmed),
    onSuccess: () => {
      setError('')
      refresh()
    },
    // The 422 here is the balance refusal and its message names the gap, so it
    // is shown verbatim rather than replaced with something vaguer.
    onError: (e) => setError(e instanceof ApiError ? e.message : String(e)),
  })
  const share = useMutation({
    mutationFn: (isShared: boolean) => api.setPaystubSharing(stub.id, isShared),
    onSuccess: refresh,
  })
  const remove = useMutation({
    mutationFn: () => api.deletePaystub(stub.id),
    onSuccess: refresh,
  })

  if (editing) {
    return (
      <div className="rounded-xl border border-white/10 p-4">
        <PaystubForm
          employers={employers}
          categories={categories}
          initial={stub}
          submitLabel="Save changes"
          onCancel={() => setEditing(false)}
          onSubmit={async (input) => {
            await api.updatePaystub(stub.id, input)
            setEditing(false)
            refresh()
          }}
        />
      </div>
    )
  }

  return (
    <div className="rounded-xl border border-white/5 p-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <p className="font-medium">
            {stub.employer_name}
            {!stub.is_own && (
              <span className="ml-2 rounded-full border border-white/10 px-2 py-0.5 text-xs text-mist-400">
                shared with you
              </span>
            )}
          </p>
          <p className="mt-0.5 text-sm text-mist-400">
            Paid {formatDate(stub.pay_date)} · {formatDate(stub.period_start)} to{' '}
            {formatDate(stub.period_end)}
          </p>
        </div>
        <div className="text-right">
          <p className="tabular text-lg font-semibold">{formatMoney(stub.net)}</p>
          <p className="tabular text-xs text-mist-400">
            of {formatMoney(stub.gross)} gross
          </p>
        </div>
      </div>

      {!stub.confirmed && (
        <p className="mt-3 rounded-lg border border-amber-400/20 bg-amber-400/5 px-3 py-2 text-xs text-mist-200">
          Not confirmed, so this stub counts towards nothing yet.
          {!stub.balances && (
            <>
              {' '}
              Gross minus the deductions is{' '}
              <span className="tabular">{formatMoney(stub.residual)}</span> away
              from net — a line is probably missing.
            </>
          )}
        </p>
      )}

      <div className="mt-4">
        <PaycheckBreakdown stub={stub} />
      </div>

      <div className="mt-4 flex flex-wrap items-center gap-2 text-sm">
        <button className="btn-ghost" onClick={() => setOpen(!open)}>
          {open ? 'Hide detail' : 'Show detail'}
        </button>
        {stub.is_own && (
          <>
            <button className="btn-ghost" onClick={() => setEditing(true)}>
              Edit
            </button>
            <button
              className={stub.confirmed ? 'btn-ghost' : 'btn-primary'}
              onClick={() => confirm.mutate(!stub.confirmed)}
              disabled={confirm.isPending}
            >
              {stub.confirmed ? 'Unconfirm' : 'Confirm'}
            </button>
            <button
              className="btn-ghost"
              onClick={() => share.mutate(!stub.is_shared)}
              disabled={share.isPending}
              title={
                stub.is_shared
                  ? 'Visible to everyone in the household'
                  : 'Only you can see this'
              }
            >
              {stub.is_shared ? 'Shared' : 'Private'}
            </button>
            <button
              className="btn-ghost text-mist-400"
              onClick={() => remove.mutate()}
              disabled={remove.isPending}
            >
              Delete
            </button>
          </>
        )}
      </div>

      {error && (
        <p className="mt-3 text-sm" style={{ color: STATUS.critical }}>
          {error}
        </p>
      )}

      {open && (
        <div className="mt-4 space-y-4 border-t border-white/5 pt-4">
          <LineTable stub={stub} />
          {stub.is_own && <DepositMatcher stub={stub} year={year} />}
        </div>
      )}
    </div>
  )
}

function LineTable({ stub }: { stub: Paystub }) {
  const deductions = stub.lines.filter((l) => !l.is_employer)
  const employer = stub.lines.filter((l) => l.is_employer)

  return (
    <div className="space-y-4 text-sm">
      <div>
        <p className="mb-2 text-xs uppercase tracking-wide text-mist-500">
          Taken out of this paycheque
        </p>
        {deductions.length === 0 ? (
          <p className="text-mist-400">No deductions recorded.</p>
        ) : (
          <table className="w-full">
            <thead>
              <tr className="text-left text-xs text-mist-500">
                <th className="pb-1 font-normal">Line</th>
                <th className="pb-1 text-right font-normal">This period</th>
                <th className="pb-1 text-right font-normal">Year to date</th>
              </tr>
            </thead>
            <tbody>
              {deductions.map((l) => (
                <tr key={l.id} className="border-t border-white/5">
                  <td className="py-1.5">
                    {l.label}
                    <span className="ml-2 text-xs text-mist-500">
                      {l.category_label}
                      {l.pre_tax && ' · pre-tax'}
                    </span>
                  </td>
                  <td className="tabular py-1.5 text-right">
                    {formatMoney(l.amount)}
                  </td>
                  <td className="tabular py-1.5 text-right text-mist-400">
                    {l.ytd_amount ? formatMoney(l.ytd_amount) : '—'}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      {employer.length > 0 && (
        <div>
          <p className="mb-2 text-xs uppercase tracking-wide text-mist-500">
            Paid by your employer on top of gross
          </p>
          <table className="w-full">
            <tbody>
              {employer.map((l) => (
                <tr key={l.id} className="border-t border-white/5">
                  <td className="py-1.5">{l.label}</td>
                  <td className="tabular py-1.5 text-right">
                    {formatMoney(l.amount)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}

/**
 * The bank deposit this stub's net pay produced.
 *
 * Candidates are ranked by how close they are to net; the link is only ever
 * written by a click here. Two identical net deposits in a two-earner household
 * are common, and an auto-match would corrupt both records at once — so the
 * server proposes and a human decides, always.
 */
function DepositMatcher({ stub, year }: { stub: Paystub; year: number }) {
  const qc = useQueryClient()
  const [looking, setLooking] = useState(false)

  const matches = useQuery({
    queryKey: ['paystub-deposits', stub.id],
    queryFn: () => api.paystubDepositMatches(stub.id),
    enabled: looking,
  })
  const link = useMutation({
    mutationFn: (transactionID: string | null) =>
      api.linkPaystubDeposit(stub.id, transactionID),
    onSuccess: () => {
      setLooking(false)
      qc.invalidateQueries({ queryKey: ['paystubs', year] })
    },
  })

  if (stub.deposit) {
    return (
      <div className="text-sm">
        <p className="mb-1 text-xs uppercase tracking-wide text-mist-500">
          Matched deposit
        </p>
        <p className="text-mist-300">
          {formatMoney(stub.deposit.amount)} on {formatDate(stub.deposit.date)}{' '}
          <button
            className="btn-ghost ml-2"
            onClick={() => link.mutate(null)}
            disabled={link.isPending}
          >
            Unlink
          </button>
        </p>
      </div>
    )
  }

  return (
    <div className="text-sm">
      <p className="mb-1 text-xs uppercase tracking-wide text-mist-500">
        Bank deposit
      </p>
      {!looking ? (
        <button className="btn-ghost" onClick={() => setLooking(true)}>
          Find the deposit this paid
        </button>
      ) : matches.isPending ? (
        <p className="text-mist-400">Looking…</p>
      ) : (matches.data ?? []).length === 0 ? (
        <p className="text-mist-400">
          No deposit near {formatDate(stub.pay_date)} looks like this stub's net
          pay. If the deposit is split between accounts, link the larger half.
        </p>
      ) : (
        <ul className="space-y-1.5">
          {(matches.data ?? []).map((m) => (
            <li
              key={m.transaction_id}
              className="flex flex-wrap items-center gap-x-3 gap-y-1"
            >
              <span className="tabular">{formatMoney(m.amount)}</span>
              <span className="text-mist-400">
                {formatDate(m.date)} · {m.label} · {m.account_name}
              </span>
              {!m.exact && (
                <span className="text-xs text-mist-500">
                  {formatMoney(m.delta)} off net pay
                </span>
              )}
              <button
                className="btn-ghost ml-auto"
                onClick={() => link.mutate(m.transaction_id)}
                disabled={link.isPending}
              >
                This one
              </button>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

// ---------------------------------------------------------------------------
// Adding a paystub
// ---------------------------------------------------------------------------

function AddPaystub({
  year,
  employers,
  categories,
}: {
  year: number
  employers: Employer[]
  categories: PayrollCategory[]
}) {
  const qc = useQueryClient()
  const [proposal, setProposal] = useState<PaystubProposal | null>(null)
  const [mode, setMode] = useState<'idle' | 'manual'>('idle')

  const refresh = () => {
    qc.invalidateQueries({ queryKey: ['paystubs'] })
    qc.invalidateQueries({ queryKey: ['payroll-summary'] })
    qc.invalidateQueries({ queryKey: ['paystub-years'] })
  }

  const showForm = mode === 'manual' || proposal !== null

  return (
    <section id="add-paystub" className="glass p-6">
      <h2 className="mb-1 text-lg font-medium">Add a paystub</h2>
      <p className="mb-5 text-sm text-mist-300">
        Import a PDF or type one in. Every line type is on the form, so a paper
        stub or an employer we can't read is fully capturable.
      </p>

      {!showForm && (
        <div className="space-y-5">
          <PDFImport onParsed={setProposal} />
          <button className="btn-ghost" onClick={() => setMode('manual')}>
            Or enter one by hand
          </button>
        </div>
      )}

      {showForm && (
        <PaystubForm
          employers={employers}
          categories={categories}
          proposal={proposal}
          year={year}
          submitLabel="Save paystub"
          onCancel={() => {
            setProposal(null)
            setMode('idle')
          }}
          onSubmit={async (input) => {
            await api.createPaystub(input)
            setProposal(null)
            setMode('idle')
            refresh()
          }}
        />
      )}
    </section>
  )
}

/**
 * The PDF importer.
 *
 * The copy here is load-bearing rather than reassurance: the extraction really
 * is local. A paystub carries a salary, an employer and usually an SSN, and the
 * app deliberately does not have a path that sends one to an AI provider — a
 * scanned stub with no text layer is typed in instead.
 */
function PDFImport({ onParsed }: { onParsed: (p: PaystubProposal) => void }) {
  const input = useRef<HTMLInputElement>(null)
  const [error, setError] = useState('')

  const parse = useMutation({
    mutationFn: (file: File) => api.parsePaystubPDF(file),
    onSuccess: (p) => {
      setError('')
      onParsed(p)
    },
    onError: (e) => setError(e instanceof ApiError ? e.message : String(e)),
  })

  return (
    <div>
      <input
        ref={input}
        type="file"
        accept="application/pdf"
        className="hidden"
        onChange={(e) => {
          const file = e.target.files?.[0]
          if (file) parse.mutate(file)
          e.target.value = ''
        }}
      />
      <button
        className="btn-primary"
        onClick={() => input.current?.click()}
        disabled={parse.isPending}
      >
        {parse.isPending ? 'Reading…' : 'Import a PDF stub'}
      </button>
      <p className="mt-2 text-xs text-mist-500">
        Read on this machine — the text your payroll provider already put in the
        PDF is pulled out locally. The file is not stored and nothing is sent to
        any service. What comes back is a draft for you to check.
      </p>
      {error && (
        <p className="mt-2 text-sm" style={{ color: STATUS.critical }}>
          {error}
        </p>
      )}
    </div>
  )
}

type FormLine = PaystubLineInput & { key: string }

let lineKeySeed = 0
const nextLineKey = () => `line-${lineKeySeed++}`

/**
 * The manual-entry form, also the review screen for an imported stub.
 *
 * One component for both on purpose: reviewing an import IS entering a stub
 * with the boxes pre-filled, and two forms would be two places for the balance
 * check to drift.
 */
function PaystubForm({
  employers,
  categories,
  initial,
  proposal,
  year,
  submitLabel,
  onSubmit,
  onCancel,
}: {
  employers: Employer[]
  categories: PayrollCategory[]
  initial?: Paystub
  proposal?: PaystubProposal | null
  year?: number
  submitLabel: string
  onSubmit: (input: PaystubInput) => Promise<void>
  onCancel: () => void
}) {
  const today = new Date().toISOString().slice(0, 10)
  const [employerID, setEmployerID] = useState(
    initial?.employer_id ?? employers[0]?.id ?? '',
  )
  const [payDate, setPayDate] = useState(
    initial?.pay_date ?? proposal?.pay_date ?? today,
  )
  const [periodStart, setPeriodStart] = useState(
    initial?.period_start ?? proposal?.period_start ?? today,
  )
  const [periodEnd, setPeriodEnd] = useState(
    initial?.period_end ?? proposal?.period_end ?? today,
  )
  const [gross, setGross] = useState(initial?.gross ?? proposal?.gross ?? '')
  const [net, setNet] = useState(initial?.net ?? proposal?.net ?? '')
  const [ytdGross, setYtdGross] = useState(
    initial?.ytd_gross ?? proposal?.ytd_gross ?? '',
  )
  const [ytdNet, setYtdNet] = useState(initial?.ytd_net ?? proposal?.ytd_net ?? '')
  const [isShared, setIsShared] = useState(initial?.is_shared ?? false)
  const [error, setError] = useState('')
  const [saving, setSaving] = useState(false)

  const [lines, setLines] = useState<FormLine[]>(() => {
    const source = initial?.lines ?? proposal?.lines ?? []
    return source.map((l) => ({
      key: nextLineKey(),
      category: l.category,
      label: l.label,
      amount: l.amount,
      ytd_amount: l.ytd_amount ?? '',
      pre_tax: l.pre_tax,
      is_employer: l.is_employer,
    }))
  })

  const specFor = (category: string) =>
    categories.find((c) => c.category === category)

  // The live balance. Computed in the browser purely to tell the user where
  // they are before they submit — the server recomputes it in exact decimal and
  // is the only thing that decides whether a stub may be confirmed.
  const residual = useMemo(() => {
    const g = Number(gross)
    const n = Number(net)
    if (!Number.isFinite(g) || !Number.isFinite(n)) return null
    const deducted = lines.reduce((sum, l) => {
      if (l.is_employer) return sum
      const v = Number(l.amount)
      return Number.isFinite(v) ? sum + v : sum
    }, 0)
    return g - deducted - n
  }, [gross, net, lines])

  const balances = residual !== null && Math.abs(residual) <= 0.01

  const addLine = () => {
    const first = categories[0]
    setLines([
      ...lines,
      {
        key: nextLineKey(),
        category: first?.category ?? 'other',
        label: '',
        amount: '',
        ytd_amount: '',
        pre_tax: first?.pre_tax_by_default ?? false,
        is_employer: first?.employer_only ?? false,
      },
    ])
  }

  const updateLine = (key: string, patch: Partial<FormLine>) =>
    setLines(lines.map((l) => (l.key === key ? { ...l, ...patch } : l)))

  const submit = async (confirm: boolean) => {
    setSaving(true)
    setError('')
    try {
      await onSubmit({
        employer_id: employerID,
        period_start: periodStart,
        period_end: periodEnd,
        pay_date: payDate,
        gross,
        net,
        ytd_gross: ytdGross || undefined,
        ytd_net: ytdNet || undefined,
        source: initial ? undefined : proposal ? 'pdf' : 'manual',
        confirm,
        is_shared: isShared,
        document_id: proposal?.document_id ?? undefined,
        lines: lines
          .filter((l) => l.amount !== '')
          .map(({ key: _key, ...l }) => ({
            ...l,
            ytd_amount: l.ytd_amount || undefined,
          })),
      })
    } catch (e) {
      setError(e instanceof ApiError ? e.message : String(e))
    } finally {
      setSaving(false)
    }
  }

  if (employers.length === 0) {
    return (
      <EmptyState title="Add an employer first">
        A paystub belongs to an employer, and the pay cadence set there is what
        turns "how much room is left" into "how much per paycheque".
      </EmptyState>
    )
  }

  return (
    <div className="space-y-5">
      {proposal && (
        <div className="space-y-2 rounded-lg border border-white/10 bg-white/[0.02] p-4 text-sm">
          <p className="text-mist-200">
            Read off the PDF. Nothing has been saved — check every figure
            against the stub before you confirm.
            {proposal.employer_name_hint && (
              <>
                {' '}
                The stub looks like it is from{' '}
                <span className="text-mist-100">
                  {proposal.employer_name_hint}
                </span>
                .
              </>
            )}
          </p>
          {proposal.warnings.map((w) => (
            <p key={w} className="text-xs text-amber-200/90">
              {w}
            </p>
          ))}
          {proposal.unmatched.length > 0 && (
            <div className="text-xs text-mist-400">
              <p className="mb-1">
                Lines we couldn't classify — add them below if they are
                deductions:
              </p>
              <ul className="space-y-0.5">
                {proposal.unmatched.map((u) => (
                  <li key={u} className="tabular">
                    {u}
                  </li>
                ))}
              </ul>
            </div>
          )}
        </div>
      )}

      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
        <Field label="Employer">
          <select
            className="input"
            value={employerID}
            onChange={(e) => setEmployerID(e.target.value)}
          >
            {employers.map((e) => (
              <option key={e.id} value={e.id}>
                {e.name}
              </option>
            ))}
          </select>
        </Field>
        <Field label="Pay date">
          <input
            type="date"
            className="input"
            value={payDate}
            onChange={(e) => setPayDate(e.target.value)}
          />
        </Field>
        <Field label="Period start">
          <input
            type="date"
            className="input"
            value={periodStart}
            onChange={(e) => setPeriodStart(e.target.value)}
          />
        </Field>
        <Field label="Period end">
          <input
            type="date"
            className="input"
            value={periodEnd}
            onChange={(e) => setPeriodEnd(e.target.value)}
          />
        </Field>
        <Field label="Gross pay">
          <input
            className="input tabular"
            inputMode="decimal"
            placeholder="3000.00"
            value={gross}
            onChange={(e) => setGross(e.target.value)}
          />
        </Field>
        <Field label="Net pay">
          <input
            className="input tabular"
            inputMode="decimal"
            placeholder="2028.15"
            value={net}
            onChange={(e) => setNet(e.target.value)}
          />
        </Field>
        <Field label="Gross year to date" hint="Optional">
          <input
            className="input tabular"
            inputMode="decimal"
            value={ytdGross}
            onChange={(e) => setYtdGross(e.target.value)}
          />
        </Field>
        <Field label="Net year to date" hint="Optional">
          <input
            className="input tabular"
            inputMode="decimal"
            value={ytdNet}
            onChange={(e) => setYtdNet(e.target.value)}
          />
        </Field>
      </div>

      <div>
        <div className="mb-2 flex items-center justify-between">
          <p className="text-sm font-medium">Deductions and contributions</p>
          <button className="btn-ghost" onClick={addLine}>
            Add a line
          </button>
        </div>

        {lines.length === 0 ? (
          <p className="text-sm text-mist-400">
            No lines yet. Add one for each item on the stub.
          </p>
        ) : (
          <div className="space-y-2">
            {lines.map((l) => {
              const spec = specFor(l.category)
              return (
                <div
                  key={l.key}
                  className="grid items-center gap-2 rounded-lg border border-white/5 p-2 sm:grid-cols-[minmax(0,1.4fr)_minmax(0,1.2fr)_7rem_7rem_auto]"
                >
                  <select
                    className="input"
                    value={l.category}
                    onChange={(e) => {
                      const next = specFor(e.target.value)
                      updateLine(l.key, {
                        category: e.target.value,
                        pre_tax: next?.pre_tax_by_default ?? false,
                        is_employer: next?.employer_only ?? false,
                      })
                    }}
                  >
                    {categories.map((c) => (
                      <option key={c.category} value={c.category}>
                        {c.label}
                      </option>
                    ))}
                  </select>
                  <input
                    className="input"
                    placeholder={spec?.label ?? 'Label on the stub'}
                    value={l.label}
                    onChange={(e) => updateLine(l.key, { label: e.target.value })}
                  />
                  <input
                    className="input tabular"
                    inputMode="decimal"
                    placeholder="Amount"
                    value={l.amount}
                    onChange={(e) => updateLine(l.key, { amount: e.target.value })}
                  />
                  <input
                    className="input tabular"
                    inputMode="decimal"
                    placeholder="YTD"
                    value={l.ytd_amount ?? ''}
                    onChange={(e) =>
                      updateLine(l.key, { ytd_amount: e.target.value })
                    }
                  />
                  <div className="flex items-center gap-3 text-xs text-mist-400">
                    {/* Hidden rather than disabled where the taxonomy settles
                        it: a tax is not a pre-tax deduction of itself, and a
                        Roth deferral is post-tax by definition. */}
                    {spec && !spec.pre_tax_locked && !l.is_employer && (
                      <label className="flex items-center gap-1">
                        <input
                          type="checkbox"
                          checked={l.pre_tax}
                          onChange={(e) =>
                            updateLine(l.key, { pre_tax: e.target.checked })
                          }
                        />
                        Pre-tax
                      </label>
                    )}
                    {spec?.employer_only && <span>Employer paid</span>}
                    <button
                      className="btn-ghost"
                      onClick={() =>
                        setLines(lines.filter((x) => x.key !== l.key))
                      }
                      aria-label="Remove line"
                    >
                      ✕
                    </button>
                  </div>
                </div>
              )
            })}
          </div>
        )}
      </div>

      <div className="rounded-lg border border-white/10 p-3 text-sm">
        {residual === null ? (
          <p className="text-mist-400">
            Enter gross and net pay and this checks that the stub adds up.
          </p>
        ) : balances ? (
          <p style={{ color: STATUS.good }}>
            This stub balances — gross minus the deductions equals net.
          </p>
        ) : (
          <p className="text-mist-200">
            <span className="tabular">
              {formatMoney(Math.abs(residual).toFixed(2))}
            </span>{' '}
            {residual > 0 ? 'is unaccounted for' : 'more is deducted than net accounts for'}
            . A stub can be saved as a draft like this, but it can't be
            confirmed until it reconciles — an unbalanced stub would put that
            gap into every total derived from it.
          </p>
        )}
      </div>

      <label className="flex items-center gap-2 text-sm text-mist-300">
        <input
          type="checkbox"
          checked={isShared}
          onChange={(e) => setIsShared(e.target.checked)}
        />
        Share with the household
        <span className="text-xs text-mist-500">
          Off by default — a salary is not shared unless you say so.
        </span>
      </label>

      {error && (
        <p className="text-sm" style={{ color: STATUS.critical }}>
          {error}
        </p>
      )}

      <div className="flex flex-wrap gap-2">
        <button
          className="btn-primary"
          disabled={saving || !balances}
          onClick={() => submit(true)}
          title={balances ? undefined : 'The stub has to balance before it can be confirmed'}
        >
          {saving ? 'Saving…' : `${submitLabel} and confirm`}
        </button>
        <button className="btn-ghost" disabled={saving} onClick={() => submit(false)}>
          Save as a draft
        </button>
        <button className="btn-ghost" disabled={saving} onClick={onCancel}>
          Cancel
        </button>
      </div>
      {year !== undefined && payDate.slice(0, 4) !== String(year) && (
        <p className="text-xs text-mist-500">
          This pay date is in {payDate.slice(0, 4)}; switch the tax year at the
          top of the page to see it after saving.
        </p>
      )}
    </div>
  )
}

// ---------------------------------------------------------------------------
// Employers
// ---------------------------------------------------------------------------

function EmployersPanel({
  employers,
  frequencies,
}: {
  employers: Employer[]
  frequencies: { value: PayFrequency; label: string; periods_per_year: number }[]
}) {
  const qc = useQueryClient()
  const [adding, setAdding] = useState(false)
  const [error, setError] = useState('')

  const refresh = () => qc.invalidateQueries({ queryKey: ['employers'] })
  const remove = useMutation({
    mutationFn: (id: string) => api.deleteEmployer(id),
    onSuccess: () => {
      setError('')
      refresh()
    },
    onError: (e) => setError(e instanceof ApiError ? e.message : String(e)),
  })

  return (
    <section className="glass p-6">
      <h2 className="mb-1 text-lg font-medium">Employers</h2>
      <p className="mb-5 text-sm text-mist-300">
        Pay cadence is what turns "how much room is left under the limit" into
        "how much per paycheque". An EIN is only needed for the tax summary; it
        is encrypted with the same key as everything else sensitive here.
      </p>

      {employers.length === 0 ? (
        <EmptyState title="No employers yet">
          Add the company that pays you, then add a paystub.
        </EmptyState>
      ) : (
        <div className="space-y-2">
          {employers.map((e) => (
            <div
              key={e.id}
              className="flex flex-wrap items-center justify-between gap-3 rounded-xl border border-white/5 p-3"
            >
              <div>
                <p className="font-medium">{e.name}</p>
                <p className="text-xs text-mist-400">
                  {frequencies.find((f) => f.value === e.pay_frequency)?.label ??
                    e.pay_frequency}
                  {' · '}
                  {e.paystub_count} paystub{e.paystub_count === 1 ? '' : 's'}
                  {e.ein_masked && ` · EIN ${e.ein_masked}`}
                </p>
              </div>
              <button
                className="btn-ghost text-mist-400"
                onClick={() => remove.mutate(e.id)}
                disabled={remove.isPending}
              >
                Delete
              </button>
            </div>
          ))}
        </div>
      )}

      {error && (
        <p className="mt-3 text-sm" style={{ color: STATUS.critical }}>
          {error}
        </p>
      )}

      {adding ? (
        <div className="mt-5">
          <EmployerForm
            frequencies={frequencies}
            onCancel={() => setAdding(false)}
            onSubmit={async (input) => {
              await api.createEmployer(input)
              setAdding(false)
              refresh()
            }}
          />
        </div>
      ) : (
        <button className="btn-ghost mt-5" onClick={() => setAdding(true)}>
          Add an employer
        </button>
      )}
    </section>
  )
}

function EmployerForm({
  frequencies,
  onSubmit,
  onCancel,
}: {
  frequencies: { value: PayFrequency; label: string }[]
  onSubmit: (input: EmployerInput) => Promise<void>
  onCancel: () => void
}) {
  const [name, setName] = useState('')
  const [ein, setEin] = useState('')
  const [address, setAddress] = useState('')
  const [frequency, setFrequency] = useState<PayFrequency>('biweekly')
  const [error, setError] = useState('')
  const [saving, setSaving] = useState(false)

  return (
    <div className="space-y-4">
      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <Field label="Name">
          <input
            className="input"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="Acme Manufacturing"
          />
        </Field>
        <Field label="Pay cadence">
          <select
            className="input"
            value={frequency}
            onChange={(e) => setFrequency(e.target.value as PayFrequency)}
          >
            {frequencies.map((f) => (
              <option key={f.value} value={f.value}>
                {f.label}
              </option>
            ))}
          </select>
        </Field>
        <Field label="EIN" hint="Optional, for the tax summary">
          <input
            className="input tabular"
            value={ein}
            onChange={(e) => setEin(e.target.value)}
            placeholder="12-3456789"
          />
        </Field>
        <Field label="Address" hint="Optional">
          <input
            className="input"
            value={address}
            onChange={(e) => setAddress(e.target.value)}
          />
        </Field>
      </div>

      {error && (
        <p className="text-sm" style={{ color: STATUS.critical }}>
          {error}
        </p>
      )}

      <div className="flex gap-2">
        <button
          className="btn-primary"
          disabled={saving}
          onClick={async () => {
            setSaving(true)
            setError('')
            try {
              await onSubmit({
                name,
                ein: ein || undefined,
                address: address || undefined,
                pay_frequency: frequency,
              })
            } catch (e) {
              setError(e instanceof ApiError ? e.message : String(e))
            } finally {
              setSaving(false)
            }
          }}
        >
          {saving ? 'Saving…' : 'Add employer'}
        </button>
        <button className="btn-ghost" onClick={onCancel}>
          Cancel
        </button>
      </div>
    </div>
  )
}

// ---------------------------------------------------------------------------
// Tax summary
// ---------------------------------------------------------------------------

/**
 * The annual figures, mapped onto W-2 boxes.
 *
 * The disclaimer is rendered from the server's own string rather than written
 * here, because it also travels with the data in the API response — this page
 * will be printed, and a table of W-2 box numbers with no caveat on it looks
 * exactly like a W-2.
 */
function TaxSummaryPanel({ year }: { year: number }) {
  const [open, setOpen] = useState(false)
  const summary = useQuery({
    queryKey: ['payroll-tax-summary', year],
    queryFn: () => api.payrollTaxSummary(year),
    enabled: open,
  })

  return (
    <section className="glass p-6">
      <h2 className="mb-1 text-lg font-medium">Tax summary for {year}</h2>
      <p className="mb-5 text-sm text-mist-300">
        Your confirmed paystubs, added up and laid out the way a W-2 is, so you
        can check the real form when it arrives. It is not a W-2 and nothing has
        been filed with anyone.
      </p>

      {!open ? (
        <button className="btn-ghost" onClick={() => setOpen(true)}>
          Show the summary
        </button>
      ) : summary.isPending ? (
        <SkeletonRows count={4} />
      ) : !summary.data || summary.data.employers.length === 0 ? (
        <EmptyState title="Nothing to summarise yet">
          Confirm at least one paystub for {year} and this fills in.
        </EmptyState>
      ) : (
        <div className="space-y-6">
          {summary.data.employers.map((e) => (
            <div key={e.employer_id}>
              <p className="font-medium">{e.employer_name}</p>
              {(e.ein || e.address) && (
                <p className="mb-2 text-xs text-mist-400">
                  {e.ein && `EIN ${e.ein}`}
                  {e.ein && e.address && ' · '}
                  {e.address}
                </p>
              )}
              <table className="w-full text-sm">
                <tbody>
                  {e.boxes.map((b) => (
                    <tr key={`${b.box}-${b.code}`} className="border-t border-white/5">
                      <td className="w-16 py-1.5 text-mist-500">
                        Box {b.box}
                        {b.code && ` ${b.code}`}
                      </td>
                      <td className="py-1.5">{b.label}</td>
                      <td className="tabular py-1.5 text-right">
                        {formatMoney(b.amount)}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          ))}
          <p className="rounded-lg border border-white/10 bg-white/[0.02] p-3 text-xs text-mist-400">
            {summary.data.disclaimer}
          </p>
        </div>
      )}
    </section>
  )
}

// ---------------------------------------------------------------------------

function Field({
  label,
  hint,
  children,
}: {
  label: string
  hint?: string
  children: ReactNode
}) {
  return (
    <label className="block text-sm">
      <span className="mb-1 block text-mist-300">
        {label}
        {hint && <span className="ml-1 text-xs text-mist-500">{hint}</span>}
      </span>
      {children}
    </label>
  )
}
