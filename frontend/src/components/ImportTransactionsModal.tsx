import { useMemo, useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { api, type Account, type ImportResult } from '../lib/api'
import { parseCsv } from '../lib/csv'
import { formatMoney } from '../lib/money'

// A generic CSV importer. The user maps their bank's columns onto date /
// description / amount; the amount can come from one signed column or from
// separate debit & credit columns (Capital One's shape). Everything is parsed
// and signed here — the backend receives clean {date, amount, description} rows
// and never has to know the source format.

type AmountMode = 'single' | 'debitCredit'
type DateFormat = 'auto' | 'iso' | 'us'

interface Mapping {
  date: number
  description: number
  amountMode: AmountMode
  amount: number
  debit: number
  credit: number
  positiveMeans: 'spending' | 'income'
  dateFormat: DateFormat
}

const NONE = -1

export function ImportTransactionsModal({
  accounts,
  onClose,
}: {
  accounts: Account[]
  onClose: () => void
}) {
  const qc = useQueryClient()
  const [accountID, setAccountID] = useState('')
  const [fileName, setFileName] = useState('')
  const [table, setTable] = useState<string[][] | null>(null)
  const [mapping, setMapping] = useState<Mapping | null>(null)
  const [result, setResult] = useState<ImportResult | null>(null)

  const headers = table?.[0] ?? []
  const dataRows = useMemo(() => table?.slice(1) ?? [], [table])

  const onFile = async (file: File) => {
    const text = await file.text()
    const parsed = parseCsv(text)
    if (parsed.length < 2) {
      setTable(null)
      setMapping(null)
      return
    }
    setFileName(file.name)
    setTable(parsed)
    setMapping(guessMapping(parsed[0]))
    setResult(null)
  }

  // Map every data row to a {date, amount, description}; invalid rows drop out
  // (the summary reports them). Memoised so the preview and the submit share
  // exactly one computation.
  const mapped = useMemo(() => {
    if (!mapping) return []
    return dataRows
      .map((cells) => mapRow(cells, mapping))
      .filter((r): r is MappedRow => r !== null)
  }, [dataRows, mapping])

  const importMut = useMutation({
    mutationFn: () =>
      api.importTransactions({
        account_id: accountID,
        rows: mapped.map((r) => ({
          date: r.date,
          amount: r.amount,
          description: r.description,
        })),
      }),
    onSuccess: (res) => {
      setResult(res)
      qc.invalidateQueries()
    },
  })

  const canImport = accountID !== '' && mapping !== null && mapped.length > 0

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4"
      onClick={onClose}
    >
      <div
        className="glass max-h-[90vh] w-full max-w-3xl overflow-auto p-6"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="mb-4 flex items-start justify-between">
          <div>
            <h2 className="text-lg font-medium">Import transactions from CSV</h2>
            <p className="mt-1 text-sm text-mist-300">
              For history older than an institution's sync window. Rows are
              de-duplicated against what's already here, so overlap is safe.
            </p>
          </div>
          <button className="btn-ghost px-2 py-1 text-sm text-mist-300" onClick={onClose}>
            Close
          </button>
        </div>

        {result ? (
          <ResultView result={result} onDone={onClose} />
        ) : (
          <div className="space-y-5">
            <div className="flex flex-wrap items-end gap-4">
              <div>
                <label className="label" htmlFor="import-account">
                  Into account
                </label>
                <select
                  id="import-account"
                  className="field w-64"
                  value={accountID}
                  onChange={(e) => setAccountID(e.target.value)}
                >
                  <option value="">Choose an account…</option>
                  {accounts.map((a) => (
                    <option key={a.id} value={a.id}>
                      {a.institution_name ? `${a.institution_name} — ` : ''}
                      {a.name}
                      {a.mask ? ` ••${a.mask}` : ''}
                    </option>
                  ))}
                </select>
              </div>
              <div>
                <span className="label">CSV file</span>
                <input
                  type="file"
                  accept=".csv,text/csv"
                  className="block text-sm text-mist-300 file:mr-3 file:rounded-lg file:border-0 file:bg-white/10 file:px-3 file:py-1.5 file:text-mist-100"
                  onChange={(e) => {
                    const f = e.target.files?.[0]
                    if (f) onFile(f)
                  }}
                />
              </div>
            </div>

            {mapping && (
              <>
                <ColumnMapper
                  headers={headers}
                  mapping={mapping}
                  onChange={setMapping}
                />
                <Preview mapped={mapped.slice(0, 6)} total={mapped.length} fileName={fileName} />
                <div className="flex items-center gap-3">
                  <button
                    className="btn-primary px-4 py-2 text-sm"
                    disabled={!canImport || importMut.isPending}
                    onClick={() => importMut.mutate()}
                  >
                    {importMut.isPending
                      ? 'Importing…'
                      : `Import ${mapped.length} row${mapped.length === 1 ? '' : 's'}`}
                  </button>
                  {!accountID && (
                    <span className="text-sm text-mist-500">Choose an account first.</span>
                  )}
                  {importMut.isError && (
                    <span role="alert" className="text-sm text-ember-400">
                      {importMut.error.message}
                    </span>
                  )}
                </div>
              </>
            )}
          </div>
        )}
      </div>
    </div>
  )
}

function ColumnMapper({
  headers,
  mapping,
  onChange,
}: {
  headers: string[]
  mapping: Mapping
  onChange: (m: Mapping) => void
}) {
  const set = (patch: Partial<Mapping>) => onChange({ ...mapping, ...patch })
  const colSelect = (
    value: number,
    onPick: (i: number) => void,
    allowNone = false,
  ) => (
    <select
      className="field"
      value={value}
      onChange={(e) => onPick(Number(e.target.value))}
    >
      {allowNone && <option value={NONE}>—</option>}
      {headers.map((h, i) => (
        <option key={i} value={i}>
          {h || `Column ${i + 1}`}
        </option>
      ))}
    </select>
  )

  return (
    <div className="space-y-4 rounded-xl border border-white/10 bg-white/5 p-4">
      <div className="grid gap-4 sm:grid-cols-2">
        <div>
          <span className="label">Date column</span>
          {colSelect(mapping.date, (i) => set({ date: i }))}
        </div>
        <div>
          <span className="label">Description column</span>
          {colSelect(mapping.description, (i) => set({ description: i }))}
        </div>
      </div>

      <div>
        <span className="label">Amount</span>
        <div className="mb-2 flex gap-4 text-sm">
          <label className="flex items-center gap-2">
            <input
              type="radio"
              checked={mapping.amountMode === 'debitCredit'}
              onChange={() => set({ amountMode: 'debitCredit' })}
            />
            Separate debit &amp; credit columns
          </label>
          <label className="flex items-center gap-2">
            <input
              type="radio"
              checked={mapping.amountMode === 'single'}
              onChange={() => set({ amountMode: 'single' })}
            />
            One signed amount column
          </label>
        </div>

        {mapping.amountMode === 'debitCredit' ? (
          <div className="grid gap-4 sm:grid-cols-2">
            <div>
              <span className="label text-mist-500">Debit (money out)</span>
              {colSelect(mapping.debit, (i) => set({ debit: i }))}
            </div>
            <div>
              <span className="label text-mist-500">Credit (money in)</span>
              {colSelect(mapping.credit, (i) => set({ credit: i }))}
            </div>
          </div>
        ) : (
          <div className="grid gap-4 sm:grid-cols-2">
            <div>
              <span className="label text-mist-500">Amount column</span>
              {colSelect(mapping.amount, (i) => set({ amount: i }))}
            </div>
            <div>
              <span className="label text-mist-500">A positive number means</span>
              <select
                className="field"
                value={mapping.positiveMeans}
                onChange={(e) =>
                  set({ positiveMeans: e.target.value as 'spending' | 'income' })
                }
              >
                <option value="spending">Money out (spending)</option>
                <option value="income">Money in (income/refund)</option>
              </select>
            </div>
          </div>
        )}
      </div>

      <div className="w-48">
        <span className="label">Date format</span>
        <select
          className="field"
          value={mapping.dateFormat}
          onChange={(e) => set({ dateFormat: e.target.value as DateFormat })}
        >
          <option value="auto">Auto-detect</option>
          <option value="iso">YYYY-MM-DD</option>
          <option value="us">MM/DD/YYYY</option>
        </select>
      </div>
    </div>
  )
}

function Preview({
  mapped,
  total,
  fileName,
}: {
  mapped: MappedRow[]
  total: number
  fileName: string
}) {
  return (
    <div>
      <p className="mb-2 text-sm text-mist-300">
        {total} valid row{total === 1 ? '' : 's'} from{' '}
        <span className="text-mist-100">{fileName}</span>. Check the signs — money
        out should be positive, money in negative.
      </p>
      <div className="overflow-x-auto rounded-xl border border-white/10">
        <table className="w-full text-sm">
          <thead className="text-left text-xs uppercase tracking-wide text-mist-500">
            <tr>
              <th className="px-3 py-2">Date</th>
              <th className="px-3 py-2">Description</th>
              <th className="px-3 py-2 text-right">Amount</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-white/5">
            {mapped.map((r, i) => (
              <tr key={i}>
                <td className="px-3 py-1.5 tabular text-mist-300">{r.date}</td>
                <td className="px-3 py-1.5 text-mist-200">{r.description}</td>
                <td
                  className={`px-3 py-1.5 text-right tabular ${
                    r.amount.startsWith('-') ? 'text-rune-300' : 'text-mist-100'
                  }`}
                >
                  {formatMoney(r.amount)}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {total > mapped.length && (
        <p className="mt-1 text-xs text-mist-500">…and {total - mapped.length} more.</p>
      )}
    </div>
  )
}

function ResultView({ result, onDone }: { result: ImportResult; onDone: () => void }) {
  return (
    <div className="space-y-4">
      <div className="rounded-xl border border-rune-500/30 bg-rune-500/10 p-4 text-sm text-rune-200">
        Imported <span className="font-medium">{result.imported}</span> transaction
        {result.imported === 1 ? '' : 's'}.
      </div>
      <ul className="space-y-1 text-sm text-mist-300">
        <li>Skipped as duplicates: {result.skipped_duplicates}</li>
        <li>Skipped as unreadable: {result.skipped_invalid}</li>
        <li>Imported but uncategorised (the sweep will handle them): {result.uncategorized}</li>
      </ul>
      <button className="btn-primary px-4 py-2 text-sm" onClick={onDone}>
        Done
      </button>
    </div>
  )
}

// --- mapping helpers -------------------------------------------------------

interface MappedRow {
  date: string
  amount: string
  description: string
}

function guessMapping(headers: string[]): Mapping {
  const find = (re: RegExp) => headers.findIndex((h) => re.test(h.toLowerCase()))
  const debit = find(/debit/)
  const credit = find(/credit/)
  const hasSplit = debit !== NONE && credit !== NONE
  return {
    date: Math.max(find(/date/), 0),
    description: Math.max(find(/descr|name|payee|memo|merchant/), 0),
    amountMode: hasSplit ? 'debitCredit' : 'single',
    amount: Math.max(find(/amount/), 0),
    debit: debit === NONE ? 0 : debit,
    credit: credit === NONE ? 0 : credit,
    positiveMeans: 'spending',
    dateFormat: 'auto',
  }
}

function mapRow(cells: string[], m: Mapping): MappedRow | null {
  const date = toISODate((cells[m.date] ?? '').trim(), m.dateFormat)
  const description = (cells[m.description] ?? '').trim()
  const amount = computeAmount(cells, m)
  if (!date || !amount || description === '') return null
  return { date, amount, description }
}

function toISODate(raw: string, fmt: DateFormat): string | null {
  const iso = /^(\d{4})-(\d{1,2})-(\d{1,2})/.exec(raw)
  const us = /^(\d{1,2})\/(\d{1,2})\/(\d{4})/.exec(raw)
  const pad = (s: string) => s.padStart(2, '0')
  if ((fmt === 'iso' || fmt === 'auto') && iso) {
    return `${iso[1]}-${pad(iso[2])}-${pad(iso[3])}`
  }
  if ((fmt === 'us' || fmt === 'auto') && us) {
    return `${us[3]}-${pad(us[1])}-${pad(us[2])}`
  }
  return null
}

// cleanNum strips currency formatting, returning a plain numeric string or ''
// when the cell is empty / not a number.
function cleanNum(raw: string | undefined): string {
  const s = (raw ?? '').replace(/[$,\s]/g, '').replace(/^\+/, '')
  return /^-?\d+(\.\d+)?$/.test(s) ? s : ''
}

function flipSign(numStr: string): string {
  return numStr.startsWith('-') ? numStr.slice(1) : `-${numStr}`
}

function computeAmount(cells: string[], m: Mapping): string | null {
  if (m.amountMode === 'single') {
    const n = cleanNum(cells[m.amount])
    if (n === '') return null
    return m.positiveMeans === 'income' ? flipSign(n) : n
  }
  // Debit = money out (positive), credit = money in (negative). Exactly one is
  // set on almost every real row; if both are, net them.
  const d = cleanNum(cells[m.debit])
  const c = cleanNum(cells[m.credit])
  if (d !== '' && c === '') return d
  if (c !== '' && d === '') return flipSign(c)
  if (d !== '' && c !== '') return (Number(d) - Number(c)).toFixed(2)
  return null
}
