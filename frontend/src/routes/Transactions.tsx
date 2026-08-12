import { useEffect, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient, keepPreviousData } from '@tanstack/react-query'
import {
  api,
  type Account,
  type Category,
  type ManualTransactionInput,
  type Transaction,
} from '../lib/api'
import { formatDate, formatTransactionAmount } from '../lib/money'
import { AttachPanel, PaperclipIcon } from '../components/AttachDocuments'
import { HistoryPanel } from '../components/HistoryPanel'
import { MerchantLink } from '../components/MerchantLink'
import { MerchantAvatar } from '../components/MerchantAvatar'
import { SplitPanel } from '../components/SplitTransaction'
import { ImportTransactionsModal } from '../components/ImportTransactionsModal'
import { enterProps } from '../lib/motion'
import { OFFLINE_WRITE_HINT, useOnline } from '../lib/offline'
import { motion } from 'motion/react'

const PAGE_SIZE = 50

/**
 * Trails a fast-changing value, so a filter can update the URL on every keystroke
 * while the request behind it fires once the typing settles.
 */
function useDebounced<T>(value: T, delayMs: number): T {
  const [settled, setSettled] = useState(value)
  useEffect(() => {
    const timer = setTimeout(() => setSettled(value), delayMs)
    return () => clearTimeout(timer)
  }, [value, delayMs])
  return settled
}

// modalState drives the add/edit dialog: null = closed, otherwise create or a
// specific manual row being edited.
type ModalState =
  | { mode: 'create' }
  | { mode: 'edit'; transaction: Transaction }
  | null

export function Transactions() {
  const today = new Date().toISOString().slice(0, 10)
  const yearAgo = new Date(Date.now() - 365 * 864e5).toISOString().slice(0, 10)

  // Filters live in the URL so the Dashboard/Spending charts can deep-link into
  // a filtered view (one day, one category) and the browser back button
  // restores it. searchParams is the single source of truth; there is no
  // duplicate local filter state to keep in sync.
  const [searchParams, setSearchParams] = useSearchParams()
  const from = searchParams.get('from') || yearAgo
  const to = searchParams.get('to') || today
  const accountIDs = (searchParams.get('accounts') || '').split(',').filter(Boolean)
  const categoryFilter = searchParams.get('category') || ''
  // A resolved merchant key, set when arriving from a merchant's breakdown. Shown
  // as a removable chip rather than another dropdown: it is a filter you were
  // handed, not one you chose from a list, and the list of every merchant would be
  // hundreds long.
  const merchantFilter = searchParams.get('merchant') || ''
  const search = searchParams.get('q') || ''
  const onlyUncat = searchParams.get('uncat') === '1'
  // Excluded rows are hidden by default so the ledger reads like the reports it
  // feeds. This is the only way to see them again — and therefore the only way
  // to un-exclude one.
  const showExcluded = searchParams.get('excluded') === '1'
  const page = Math.max(0, Number(searchParams.get('page') || '0') || 0)

  const [modal, setModal] = useState<ModalState>(null)
  const [importing, setImporting] = useState(false)
  // The transaction whose merchant name is being fixed in the rename dialog.
  // Separate from `modal` (the full add/edit form), because the rename path is
  // the one a Plaid-synced row can take: it re-points the descriptor rather than
  // editing columns the next sync would overwrite.
  const [editingName, setEditingName] = useState<Transaction | null>(null)
  // The list itself reads fine from cache offline; adding and importing do not.
  const online = useOnline()

  // patchParams writes filter changes back to the URL. Any change other than an
  // explicit page move resets to page 0, so a new filter never lands you past
  // the end of the (now shorter) result set.
  const patchParams = (patch: Record<string, string | null>, keepPage = false) => {
    setSearchParams(
      (prev) => {
        const next = new URLSearchParams(prev)
        for (const [k, v] of Object.entries(patch)) {
          if (v === null || v === '') next.delete(k)
          else next.set(k, v)
        }
        if (!keepPage) next.delete('page')
        return next
      },
      { replace: true },
    )
  }
  const setPage = (p: number) => patchParams({ page: p <= 0 ? null : String(p) }, true)
  const toggleAccount = (id: string) => {
    const set = new Set(accountIDs)
    if (set.has(id)) {
      set.delete(id)
    } else {
      set.add(id)
    }
    patchParams({ accounts: [...set].join(',') || null })
  }

  const accounts = useQuery({ queryKey: ['accounts'], queryFn: api.accounts })
  const categories = useQuery({ queryKey: ['categories'], queryFn: api.categories })

  // id → category, for showing each row's resolved (app) category, and the list
  // a user can pick from when recategorising.
  const categoryById = new Map((categories.data ?? []).map((c) => [c.id, c]))
  const spendCats = spendCategories(categories.data ?? [])

  // Search is debounced into the query key rather than the URL: the URL updates on
  // every keystroke so the view stays shareable and the back button works, but a
  // request per character would be one per character.
  const debouncedSearch = useDebounced(search, 250)

  const transactions = useQuery({
    queryKey: [
      'transactions',
      from,
      to,
      accountIDs.join(','),
      categoryFilter,
      merchantFilter,
      debouncedSearch,
      onlyUncat,
      showExcluded,
      page,
    ],
    queryFn: () =>
      api.transactions({
        from,
        to,
        accounts: accountIDs,
        category_id: categoryFilter || undefined,
        merchant: merchantFilter || undefined,
        q: debouncedSearch || undefined,
        uncategorised: onlyUncat || undefined,
        include_excluded: showExcluded || undefined,
        limit: PAGE_SIZE,
        offset: page * PAGE_SIZE,
      }),
    // Keeps the previous page on screen while the next loads, so paging does
    // not flash an empty table.
    placeholderData: keepPreviousData,
  })

  const rows = transactions.data ?? []
  const isLastPage = rows.length < PAGE_SIZE

  // Attachment counts for the whole page in one request, so a paperclip badge
  // costs one round trip rather than fifty. A vault that is switched off
  // answers 503; the empty fallback below just means no badges.
  const documentCounts = useQuery({
    queryKey: ['document-counts', rows.map((t) => t.id)],
    queryFn: () => api.documentCounts(rows.map((t) => t.id)),
    enabled: rows.length > 0,
    retry: false,
  })
  const countFor = (id: string) => documentCounts.data?.[id] ?? 0

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold">Transactions</h1>
          <p className="mt-1 text-mist-300">
            Everything Ledgermancy has pulled in, newest first.
          </p>
        </div>
        <div className="flex gap-2">
          <button
            className="btn-ghost px-4 py-2 text-sm"
            disabled={!online}
            title={online ? undefined : OFFLINE_WRITE_HINT}
            onClick={() => setImporting(true)}
          >
            Import CSV
          </button>
          <button
            className="btn-primary px-4 py-2 text-sm"
            disabled={!online}
            title={online ? undefined : OFFLINE_WRITE_HINT}
            onClick={() => setModal({ mode: 'create' })}
          >
            Add transaction
          </button>
        </div>
      </div>

      {/* relative z-20 lifts this whole bar (and the account dropdown that
          overflows it) above the transactions panel below, which is a sibling
          `glass` layer that would otherwise paint on top of the popover. */}
      <div className="glass relative z-20 flex flex-wrap items-end gap-4 p-4">
        <div>
          <label className="label" htmlFor="from">
            From
          </label>
          <input
            id="from"
            type="date"
            className="field"
            value={from}
            onChange={(e) => patchParams({ from: e.target.value })}
          />
        </div>
        <div>
          <label className="label" htmlFor="to">
            To
          </label>
          <input
            id="to"
            type="date"
            className="field"
            value={to}
            onChange={(e) => patchParams({ to: e.target.value })}
          />
        </div>
        <div>
          <span className="label">Accounts</span>
          <AccountMultiSelect
            accounts={accounts.data ?? []}
            selected={accountIDs}
            onToggle={toggleAccount}
            onClear={() => patchParams({ accounts: null })}
          />
        </div>
        <div>
          <label className="label" htmlFor="category">
            Category
          </label>
          <select
            id="category"
            className="field"
            value={categoryFilter}
            onChange={(e) => patchParams({ category: e.target.value || null })}
          >
            <option value="">All categories</option>
            {(categories.data ?? []).map((c) => (
              <option key={c.id} value={c.id}>
                {c.name}
              </option>
            ))}
          </select>
        </div>

        <div className="min-w-40 flex-1">
          <label className="label" htmlFor="search">
            Search
          </label>
          <input
            id="search"
            type="search"
            className="field"
            placeholder="Merchant or description…"
            value={search}
            onChange={(e) => patchParams({ q: e.target.value || null })}
          />
        </div>

        <label className="flex items-center gap-2 pb-2 text-sm text-mist-300">
          <input
            type="checkbox"
            checked={onlyUncat}
            onChange={(e) => patchParams({ uncat: e.target.checked ? '1' : null })}
          />
          Needs a category
        </label>

        <label
          className="flex items-center gap-2 pb-2 text-sm text-mist-300"
          title="Rows you've hidden from reports. Turn this on to find one and put it back."
        >
          <input
            type="checkbox"
            checked={showExcluded}
            onChange={(e) => patchParams({ excluded: e.target.checked ? '1' : null })}
          />
          Show excluded
        </label>

        <p className="ml-auto text-sm text-mist-500">
          {transactions.isFetching ? 'Updating…' : `${rows.length} shown`}
        </p>
      </div>

      {merchantFilter && (
        <div className="flex items-center gap-2 text-sm text-mist-300">
          <span>Showing only</span>
          {/* The label comes off the first row, so the chip names the merchant
              without a second request. With no rows there is nothing to name it
              after, and the key itself is the honest fallback. */}
          <span className="inline-flex items-center gap-1.5 rounded-full border border-rune-400/30 bg-rune-400/10 px-2.5 py-1 text-xs text-rune-200">
            {rows[0]?.merchant ?? merchantFilter}
            <button
              type="button"
              className="text-rune-300/70 transition-colors hover:text-rune-100"
              title="Show every merchant again"
              onClick={() => patchParams({ merchant: null })}
            >
              ×
            </button>
          </span>
        </div>
      )}

      <section className="glass overflow-hidden">
        {rows.length === 0 && !transactions.isFetching ? (
          <p className="px-6 py-12 text-center text-sm text-mist-500">
            {search || merchantFilter
              ? 'No transactions match these filters. Try widening the dates or clearing the search.'
              : 'No transactions in this range. Connect an account or add one by hand.'}
          </p>
        ) : (
          <ul className="divide-y divide-white/5">
            {rows.map((t, i) => (
              <TransactionRow
                key={t.id}
                transaction={t}
                index={i}
                categoryById={categoryById}
                spendCats={spendCats}
                documentCount={countFor(t.id)}
                onEdit={() => setModal({ mode: 'edit', transaction: t })}
                onEditName={() => setEditingName(t)}
              />
            ))}
          </ul>
        )}
      </section>

      <div className="flex items-center justify-between">
        <button
          className="btn-ghost text-sm"
          disabled={page === 0}
          onClick={() => setPage(page - 1)}
        >
          ← Previous
        </button>
        <span className="text-sm text-mist-500">Page {page + 1}</span>
        <button
          className="btn-ghost text-sm"
          disabled={isLastPage}
          onClick={() => setPage(page + 1)}
        >
          Next →
        </button>
      </div>

      {modal && (
        <ManualTransactionModal
          state={modal}
          accounts={accounts.data ?? []}
          defaultAccountID={accountIDs[0] ?? ''}
          onClose={() => setModal(null)}
        />
      )}

      {importing && (
        <ImportTransactionsModal
          accounts={accounts.data ?? []}
          onClose={() => setImporting(false)}
        />
      )}

      {editingName && (
        <EditMerchantNameDialog
          transaction={editingName}
          onClose={() => setEditingName(null)}
        />
      )}
    </div>
  )
}

// AccountMultiSelect is a checkbox dropdown over the household's accounts. An
// empty selection means "all accounts", so the common case needs no clicks. It
// uses a native <details> for the popover — no outside-click plumbing, and it
// closes on its own when another one opens is not needed here.
function AccountMultiSelect({
  accounts,
  selected,
  onToggle,
  onClear,
}: {
  accounts: Account[]
  selected: string[]
  onToggle: (id: string) => void
  onClear: () => void
}) {
  const label =
    selected.length === 0
      ? 'All accounts'
      : selected.length === 1
        ? (accounts.find((a) => a.id === selected[0])?.name ?? '1 account')
        : `${selected.length} accounts`

  return (
    <details className="relative">
      <summary className="field flex w-56 cursor-pointer list-none items-center justify-between">
        <span className="truncate">{label}</span>
        <span className="ml-2 text-mist-500">▾</span>
      </summary>
      <div className="absolute z-30 mt-1 max-h-72 w-64 overflow-auto rounded-2xl border border-white/10 bg-ink-950/90 p-1.5 shadow-xl shadow-black/40 backdrop-blur-xl">
        <button
          className="w-full rounded-lg px-2 py-1.5 text-left text-sm text-mist-300 hover:bg-white/5"
          onClick={onClear}
        >
          All accounts
        </button>
        {groupByInstitution(accounts).map(([institution, accts]) => (
          <div key={institution} className="mt-1">
            <p className="px-2 pt-1 text-[11px] uppercase tracking-wide text-mist-500">
              {institution}
            </p>
            {accts.map((a) => (
              <label
                key={a.id}
                className="flex cursor-pointer items-center gap-2 rounded-lg px-2 py-1.5 text-sm hover:bg-white/5"
              >
                <input
                  type="checkbox"
                  checked={selected.includes(a.id)}
                  onChange={() => onToggle(a.id)}
                />
                <span className="truncate">
                  {a.name}
                  {a.mask ? ` ••${a.mask}` : ''}
                </span>
              </label>
            ))}
          </div>
        ))}
      </div>
    </details>
  )
}

/** Groups accounts by institution for the filter dropdown's optgroups. */
function groupByInstitution(accounts: Account[]): [string, Account[]][] {
  const groups = new Map<string, Account[]>()
  for (const a of accounts) {
    const key = a.institution_name ?? 'Other'
    const list = groups.get(key)
    if (list) list.push(a)
    else groups.set(key, [a])
  }
  return [...groups.entries()]
}

function TransactionRow({
  transaction: t,
  index,
  categoryById,
  spendCats,
  documentCount,
  onEdit,
  onEditName,
}: {
  transaction: Transaction
  index: number
  categoryById: Map<string, Category>
  spendCats: Category[]
  documentCount: number
  onEdit: () => void
  onEditName: () => void
}) {
  const qc = useQueryClient()
  const amount = formatTransactionAmount(t.amount, t.currency)
  const isManual = t.source === 'manual'
  const isScheduled = t.source === 'scheduled'
  const [editingCat, setEditingCat] = useState(false)
  const online = useOnline()

  const remove = useMutation({
    mutationFn: () => api.deleteTransaction(t.id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['transactions'] }),
  })

  // The app's resolved category (falls back to "Uncategorised"), not Plaid's raw
  // guess. A category the list didn't include (e.g. an income/transfer) shows by
  // whatever name we have, or the fallback.
  const current = t.category_id ? categoryById.get(t.category_id) : undefined
  const currentLabel = current?.name ?? 'Uncategorised'

  return (
    <motion.li
      {...enterProps(index)}
      className="flex items-center gap-3 px-4 py-3.5 sm:gap-4 sm:px-6"
    >
      {/* Desktop-only date column. On a phone the date moves into the
          subtitle line below the merchant — a w-24 column here is ~110px
          the merchant name cannot spare on a 360px viewport, and keeping it
          inline is what was forcing the name to truncate to almost nothing. */}
      <div className="hidden w-24 shrink-0 text-sm text-mist-500 sm:block">
        {formatDate(t.date)}
      </div>

      <MerchantAvatar
        name={t.merchant}
        merchantKey={t.merchant_key_resolved}
      />

      {/*
        The merchant column. Two lines:

          line 1 — merchant name (+ status badges), with the amount pinned to
                   the right on mobile. On desktop the amount lives in its own
                   column further right so figures align down the page.
          line 2 — when + where. The date leads on mobile (there is no date
                   column there) and the account follows.

        Stacking is what gives the name room on a phone: line 1 is the name's
        own row, not one of six things fighting for a single line.
      */}
      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-2">
          <p className="flex min-w-0 flex-1 items-center gap-2 truncate font-medium">
            {/* An inline link, not a whole-row one: this row already holds the
                category button, the ⋯ menu and edit/delete, and nesting those
                inside a link is neither valid nor usable.
                merchant_key_resolved, never merchant_key — the raw descriptor
                would strand every fragment of a grouped merchant but one. */}
            <MerchantLink
              name={t.merchant}
              merchantKey={t.merchant_key_resolved}
            />
            {isManual && (
              <span className="shrink-0 rounded-full border border-arcane-500/30 bg-arcane-500/10 px-2 py-0.5 text-[10px] font-medium uppercase tracking-wide text-arcane-400">
                Manual
              </span>
            )}
            {/* Posted by the scheduled worker from an obligation. Read-only,
                like a Plaid row and for the same reason: an edit here would be
                undone the next time the obligation posted, so the honest place
                to change it is the obligation. */}
            {isScheduled && (
              <span
                className="shrink-0 rounded-full border border-rune-400/30 bg-rune-400/10 px-2 py-0.5 text-[10px] font-medium uppercase tracking-wide text-rune-300"
                title="Posted automatically from a scheduled obligation. Edit the obligation to change it."
              >
                Scheduled
              </span>
            )}
            {t.is_one_time && (
              <span
                className="shrink-0 rounded-full border border-rune-400/30 bg-rune-400/10 px-2 py-0.5 text-[10px] font-medium uppercase tracking-wide text-rune-300"
                title="Counted in this month, but left out of the averages that predict future months."
              >
                One-time
              </span>
            )}
            {t.excluded_from_reports && (
              <span
                className="shrink-0 rounded-full border border-white/15 bg-white/5 px-2 py-0.5 text-[10px] font-medium uppercase tracking-wide text-mist-400"
                title="Hidden from every report."
              >
                Excluded
              </span>
            )}
            {t.pending && (
              <span className="shrink-0 rounded-full border border-rune-400/30 bg-rune-400/10 px-2 py-0.5 text-[10px] font-medium uppercase tracking-wide text-rune-300">
                Pending
              </span>
            )}
          </p>
          {/* Mobile amount — sits at the end of line 1. Desktop renders the
              amount in its own aligned column instead (below). */}
          <span
            className={`tabular shrink-0 font-medium sm:hidden ${
              amount.isIncome ? 'text-verdant-400' : 'text-mist-100'
            }`}
          >
            {amount.text}
          </span>
        </div>

        <p className="truncate text-xs text-mist-500">
          <span className="sm:hidden">{formatDate(t.date)} · </span>
          {t.account_name}
          {t.institution_name && ` · ${t.institution_name}`}
        </p>

        {t.possible_duplicate && (
          <p className="mt-1 flex flex-wrap items-center gap-2 text-xs text-ember-400">
            <span>Possible duplicate — a matching synced charge arrived.</span>
            <button
              className="btn-ghost px-2 py-0.5 text-xs text-ember-400"
              disabled={remove.isPending}
              onClick={() => remove.mutate()}
            >
              Delete my entry
            </button>
          </p>
        )}
      </div>

      {editingCat ? (
        <CategoryEditor
          transaction={t}
          spendCats={spendCats}
          currentID={t.category_id}
          onDone={() => setEditingCat(false)}
        />
      ) : (
        <button
          className="hidden shrink-0 rounded-full border border-white/10 px-2.5 py-1 text-xs text-mist-300 transition hover:border-white/25 hover:text-mist-100 disabled:opacity-60 disabled:hover:border-white/10 disabled:hover:text-mist-300 sm:inline"
          disabled={!online}
          title={online ? 'Change category' : OFFLINE_WRITE_HINT}
          onClick={() => setEditingCat(true)}
        >
          {currentLabel}
        </button>
      )}

      {/* Desktop amount column — aligned figures down the page. The mobile
          amount is the inline span on line 1 above. */}
      <div
        className={`tabular hidden w-28 shrink-0 text-right font-medium sm:block ${
          amount.isIncome ? 'text-verdant-400' : 'text-mist-100'
        }`}
      >
        {amount.text}
      </div>

      {/* A row-level count of attachments, so the vault is still visible at a
          glance now that the paperclip itself lives in the menu. Desktop only:
          on mobile the count is unreachable here, but the ⋯ menu labels the
          Documents item with "(n)" so the info is not lost. */}
      {documentCount > 0 && (
        <span
          className="hidden shrink-0 items-center gap-1 text-xs text-mist-400 sm:flex"
          title={`${documentCount} attached`}
        >
          <PaperclipIcon />
          <span className="tabular">{documentCount}</span>
        </span>
      )}

      {/* Everything that acts on this row — splitting, documents, how it
          counts, and (for manual rows) edit/delete — behind one ⋯ so the row
          stays scannable. On mobile this is the ONLY per-row control, which is
          what keeps edit/delete from being clipped off-screen. Applies to
          synced rows too: a loan payoff arrives from Plaid. */}
      <RowMenu
        transaction={t}
        documentCount={documentCount}
        canEdit={isManual}
        onEdit={onEdit}
        onEditName={onEditName}
        onDelete={() => remove.mutate()}
        deletePending={remove.isPending}
      />

      {/* Edit/delete inline for hand-entered rows. Desktop only — on mobile
          the same actions live in the ⋯ menu above. Plaid rows stay read-only
          except category, which has its own path. */}
      {isManual && (
        <div className="hidden shrink-0 items-center gap-1 sm:flex">
          <button
            className="btn-ghost px-2 py-1 text-xs text-mist-300"
            onClick={onEdit}
          >
            Edit
          </button>
          <button
            className="btn-ghost px-2 py-1 text-xs text-ember-400"
            disabled={remove.isPending}
            onClick={() => remove.mutate()}
          >
            Delete
          </button>
        </div>
      )}
    </motion.li>
  )
}

// RowMenu is the row's single ⋯ control: splitting, documents, and the counting
// flags. They used to be three separate buttons on every row, which made a list
// of fifty transactions read as a wall of controls rather than a ledger — the
// row's job is to show what happened, and acting on it is the rare case.
//
// The two popovers are rendered here rather than by their own trigger buttons,
// so all three share one anchor and only one can be open at a time.
//
// On the counting flags:
//
// "One-time" is the one that matters. Ledgermancy predicts a typical month by
// averaging trailing ones, and a genuine but non-repeating event — a loan
// payoff, a tax bill, a car bought outright — is real spending that is terrible
// evidence about next month. Flagging it keeps it in the month it happened
// (Spending still shows it; the money left) while taking it out of the baselines
// that drive safe-to-spend and bill detection.
//
// "Exclude" is the stronger, rarer claim: this did not really happen to us.
// Excluded rows drop out of the ledger too, which is why the list has a "Show
// excluded" filter — without it the flag would be a one-way door.
function RowMenu({
  transaction: t,
  documentCount,
  canEdit = false,
  onEdit,
  onEditName,
  onDelete,
  deletePending = false,
}: {
  transaction: Transaction
  documentCount: number
  /**
   * Hand-entered rows are editable and deletable; synced rows are not. On
   * mobile these actions live ONLY here (the inline buttons are sm+), so the
   * menu is where a phone user goes to delete a manual transaction.
   */
  canEdit?: boolean
  onEdit?: () => void
  /** Open the rename/re-map dialog. Offered for any row with a merchant_key
   *  that does not already have a full row editor (manual rows do). */
  onEditName?: () => void
  onDelete?: () => void
  deletePending?: boolean
}) {
  const qc = useQueryClient()
  const online = useOnline()
  // The rename path needs a descriptor to re-point. Manual rows already have a
  // full editor; scheduled rows are owned by the obligation that posts them.
  const canEditName =
    Boolean(t.merchant_key) && t.source !== 'manual' && t.source !== 'scheduled'
  // Which popover this row is showing, if any — the menu itself or one of the
  // two it opens.
  const [panel, setPanel] = useState<'menu' | 'split' | 'documents' | null>(null)
  const open = panel !== null
  const close = () => setPanel(null)

  const set = useMutation({
    mutationFn: (flags: { is_one_time?: boolean; excluded_from_reports?: boolean }) =>
      api.setTransactionFlags(t.id, flags),
    onSuccess: () => {
      close()
      // Both flags move report totals, so every surface computed from them has
      // to refetch — including safe-to-spend, whose whole point here is to stop
      // reflecting the outlier.
      for (const key of [
        'transactions',
        'summary',
        'by-category',
        'averages',
        'safe-to-spend',
        'trend',
        'recurring',
      ]) {
        qc.invalidateQueries({ queryKey: [key] })
      }
    },
  })

  return (
    <div className="relative shrink-0">
      <button
        type="button"
        className="btn-ghost px-2 py-1 text-xs text-mist-400"
        title="Split, documents, and how this counts"
        aria-label="Row actions"
        aria-haspopup="menu"
        aria-expanded={open}
        onClick={() => setPanel((p) => (p === null ? 'menu' : null))}
      >
        ⋯
      </button>

      {open && (
        /* Click-away layer. Sits under the menu and both popovers but over
           everything else, so the next click anywhere closes rather than
           acting on the page. */
        <div className="fixed inset-0 z-30" onClick={close} aria-hidden />
      )}

      {panel === 'split' && (
        <SplitPanel transactionId={t.id} amount={t.amount} onClose={close} />
      )}

      {panel === 'documents' && (
        <AttachPanel target={{ kind: 'transaction', id: t.id }} onClose={close} />
      )}

      {panel === 'menu' && (
        <>
          <div
            role="menu"
            className="glass absolute right-0 z-40 mt-1 w-64 space-y-1 p-2 text-left text-xs"
          >
            {/* Fix a wrong merchant name. Plaid occasionally hands back a
                descriptor that is just noise (a row of asterisks, a processor
                code) for a real business; this re-points the descriptor at the
                right canonical merchant so the name is correct here and on
                every report, and stays fixed across future syncs. Offered for
                any row with a descriptor that is not already fully editable
                (manual rows have their own editor). Scheduled rows are posted
                by the obligation worker, so their identity is changed there. */}
            {canEditName && onEditName && (
              <>
                <button
                  type="button"
                  role="menuitem"
                  className="w-full rounded px-2 py-1.5 text-left transition hover:bg-white/5 disabled:opacity-60"
                  disabled={!online}
                  title={online ? undefined : OFFLINE_WRITE_HINT}
                  onClick={() => {
                    close()
                    onEditName()
                  }}
                >
                  <span className="block font-medium text-mist-100">
                    Edit name…
                  </span>
                  <span className="block text-mist-500">
                    Correct this merchant's real name.
                  </span>
                </button>

                <div className="my-1 border-t border-white/10" role="none" />
              </>
            )}

            {/* Whose share this charge was. An attribution overlay — it never
                changes what the household spent. */}
            <button
              type="button"
              role="menuitem"
              className="w-full rounded px-2 py-1.5 text-left transition hover:bg-white/5 disabled:opacity-60"
              disabled={!online}
              title={online ? undefined : OFFLINE_WRITE_HINT}
              onClick={() => setPanel('split')}
            >
              <span className="block font-medium text-mist-100">Split…</span>
              <span className="block text-mist-500">
                Record whose share this was.
              </span>
            </button>

            {/* Receipts, invoices and warranties belong next to the charge they
                explain, so the vault is reachable from the row rather than only
                from the Documents page. */}
            <button
              type="button"
              role="menuitem"
              className="w-full rounded px-2 py-1.5 text-left transition hover:bg-white/5"
              onClick={() => setPanel('documents')}
            >
              <span className="block font-medium text-mist-100">
                {documentCount > 0 ? `Documents (${documentCount})` : 'Attach a receipt…'}
              </span>
              <span className="block text-mist-500">
                {documentCount > 0
                  ? 'See or change what is attached.'
                  : 'Keep the paperwork with the charge.'}
              </span>
            </button>

            <div className="my-1 border-t border-white/10" role="none" />

            <button
              type="button"
              role="menuitem"
              className="w-full rounded px-2 py-1.5 text-left transition hover:bg-white/5 disabled:opacity-60"
              disabled={set.isPending || !online}
              title={online ? undefined : OFFLINE_WRITE_HINT}
              onClick={() => set.mutate({ is_one_time: !t.is_one_time })}
            >
              <span className="block font-medium text-mist-100">
                {t.is_one_time ? 'Treat as usual spending' : 'Mark as one-time'}
              </span>
              <span className="block text-mist-500">
                {t.is_one_time
                  ? 'Let this count towards typical-month averages again.'
                  : 'Keep it in this month, but out of future estimates.'}
              </span>
            </button>

            <button
              type="button"
              role="menuitem"
              className="w-full rounded px-2 py-1.5 text-left transition hover:bg-white/5 disabled:opacity-60"
              disabled={set.isPending || !online}
              title={online ? undefined : OFFLINE_WRITE_HINT}
              onClick={() =>
                set.mutate({ excluded_from_reports: !t.excluded_from_reports })
              }
            >
              <span className="block font-medium text-mist-100">
                {t.excluded_from_reports
                  ? 'Include in reports'
                  : 'Exclude from reports'}
              </span>
              <span className="block text-mist-500">
                {t.excluded_from_reports
                  ? 'Count this everywhere again.'
                  : 'Hide it everywhere, as if it never happened.'}
              </span>
            </button>

            {/* Edit/delete for hand-entered rows. On desktop these are also
                inline buttons at the row's end; here they are the mobile path,
                and the only way to delete a manual transaction on a phone. */}
            {canEdit && (onEdit || onDelete) && (
              <>
                <div className="my-1 border-t border-white/10" role="none" />

                {onEdit && (
                  <button
                    type="button"
                    role="menuitem"
                    className="w-full rounded px-2 py-1.5 text-left transition hover:bg-white/5 disabled:opacity-60"
                    disabled={!online}
                    title={online ? undefined : OFFLINE_WRITE_HINT}
                    onClick={() => {
                      close()
                      onEdit()
                    }}
                  >
                    <span className="block font-medium text-mist-100">
                      Edit…
                    </span>
                    <span className="block text-mist-500">
                      Change the amount, date, or merchant.
                    </span>
                  </button>
                )}

                {onDelete && (
                  <button
                    type="button"
                    role="menuitem"
                    className="w-full rounded px-2 py-1.5 text-left transition hover:bg-white/5 disabled:opacity-60"
                    disabled={deletePending || !online}
                    title={online ? undefined : OFFLINE_WRITE_HINT}
                    onClick={() => {
                      close()
                      onDelete()
                    }}
                  >
                    <span className="block font-medium text-ember-400">
                      Delete…
                    </span>
                    <span className="block text-mist-500">
                      Remove this transaction for good.
                    </span>
                  </button>
                )}
              </>
            )}

            {set.isError && (
              <p className="px-2 py-1 text-ember-400">
                Could not save that. Try again.
              </p>
            )}
          </div>
        </>
      )}
    </div>
  )
}

// CategoryEditor is the inline recategorise control. It writes through the
// existing recategorise endpoint; "apply to all from this merchant" both
// remembers the choice for future syncs and retroactively fixes every existing
// charge from that merchant (handled server-side).
function CategoryEditor({
  transaction: t,
  spendCats,
  currentID,
  onDone,
}: {
  transaction: Transaction
  spendCats: Category[]
  currentID: string | null
  onDone: () => void
}) {
  const qc = useQueryClient()
  const [categoryID, setCategoryID] = useState(currentID ?? '')
  const [applyToMerchant, setApplyToMerchant] = useState(false)
  // Gate on merchant_key (what the server caches by), not merchant_name — many
  // Plaid rows have a key derived from the name with no merchant_name set.
  const hasMerchant = Boolean(t.merchant_key)
  const merchantLabel = t.merchant

  const save = useMutation({
    mutationFn: () => api.recategorise(t.id, categoryID, applyToMerchant && hasMerchant),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['transactions'] })
      // Category totals feed the reports, so refresh those too.
      qc.invalidateQueries({ queryKey: ['by-category'] })
      qc.invalidateQueries({ queryKey: ['summary'] })
      qc.invalidateQueries({ queryKey: ['averages'] })
      onDone()
    },
  })

  return (
    <div className="flex shrink-0 flex-wrap items-center justify-end gap-2">
      <select
        className="field py-1 text-xs"
        value={categoryID}
        aria-label="Category"
        onChange={(e) => setCategoryID(e.target.value)}
      >
        <option value="" disabled>
          Choose…
        </option>
        {spendCats.map((c) => (
          <option key={c.id} value={c.id}>
            {c.name}
          </option>
        ))}
      </select>

      {hasMerchant && (
        <label className="flex items-center gap-1 text-xs text-mist-400">
          <input
            type="checkbox"
            checked={applyToMerchant}
            onChange={(e) => setApplyToMerchant(e.target.checked)}
          />
          All from {merchantLabel}
        </label>
      )}

      <button
        className="btn-ghost px-2 py-1 text-xs"
        disabled={save.isPending || categoryID === ''}
        onClick={() => save.mutate()}
      >
        Save
      </button>
      <button className="btn-ghost px-2 py-1 text-xs text-mist-300" onClick={onDone}>
        Cancel
      </button>
    </div>
  )
}

// splitAmount turns a signed decimal string into a magnitude + direction for
// the form, so an edited refund starts on the right toggle. abs() on a string
// is done by stripping a leading '-'.
function splitAmount(amount: string): { magnitude: string; income: boolean } {
  const income = amount.trim().startsWith('-')
  return { magnitude: amount.replace(/^-/, ''), income }
}

function ManualTransactionModal({
  state,
  accounts,
  defaultAccountID,
  onClose,
}: {
  state: Exclude<ModalState, null>
  accounts: Account[]
  defaultAccountID: string
  onClose: () => void
}) {
  const qc = useQueryClient()
  const categories = useQuery({ queryKey: ['categories'], queryFn: api.categories })

  const editing = state.mode === 'edit' ? state.transaction : null
  const initialAmount = editing ? splitAmount(editing.amount) : null

  const today = new Date().toISOString().slice(0, 10)
  const [accountID, setAccountID] = useState(
    editing?.account_id ?? defaultAccountID ?? accounts[0]?.id ?? '',
  )
  const [date, setDate] = useState(editing ? editing.date.slice(0, 10) : today)
  const [merchant, setMerchant] = useState(
    editing ? (editing.merchant_name ?? editing.name) : '',
  )
  const [magnitude, setMagnitude] = useState(initialAmount?.magnitude ?? '')
  const [income, setIncome] = useState(initialAmount?.income ?? false)
  const [categoryID, setCategoryID] = useState(editing?.category_id ?? '')
  const [notes, setNotes] = useState(editing?.notes ?? '')

  const save = useMutation({
    mutationFn: (input: ManualTransactionInput) =>
      editing ? api.updateTransaction(editing.id, input) : api.createTransaction(input),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['transactions'] })
      onClose()
    },
  })

  const canSave =
    accountID !== '' && merchant.trim() !== '' && magnitude !== '' && Number(magnitude) > 0

  const submit = () => {
    if (!canSave) return
    // The toggle sets the sign: expense = money out (positive, Plaid's
    // convention), income/refund = negative.
    const signed = income ? `-${magnitude}` : magnitude
    const name = merchant.trim()
    save.mutate({
      account_id: accountID,
      date,
      amount: signed,
      name,
      merchant_name: name,
      category_id: categoryID || null,
      notes: notes.trim() || null,
    })
  }

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4"
      role="dialog"
      aria-modal="true"
      onClick={onClose}
    >
      <div
        className="glass w-full max-w-lg space-y-4 p-6"
        onClick={(e) => e.stopPropagation()}
      >
        <div>
          <h2 className="text-lg font-medium">
            {editing ? 'Edit transaction' : 'Add transaction'}
          </h2>
          <p className="mt-1 text-sm text-mist-300">
            Reconcile a charge your bank feed missed. This corrects your spending
            totals only — it never changes an account balance.
          </p>
        </div>

        <div className="grid gap-4 sm:grid-cols-2">
          <div className="sm:col-span-2">
            <label className="label" htmlFor="mtx-account">
              Account
            </label>
            <select
              id="mtx-account"
              className="field w-full"
              value={accountID}
              onChange={(e) => setAccountID(e.target.value)}
            >
              <option value="" disabled>
                Select an account
              </option>
              {groupByInstitution(accounts).map(([institution, accts]) => (
                <optgroup key={institution} label={institution}>
                  {accts.map((a) => (
                    <option key={a.id} value={a.id}>
                      {a.name}
                      {a.mask ? ` ••${a.mask}` : ''}
                    </option>
                  ))}
                </optgroup>
              ))}
            </select>
          </div>

          <div>
            <label className="label" htmlFor="mtx-date">
              Date
            </label>
            <input
              id="mtx-date"
              type="date"
              className="field w-full"
              value={date}
              onChange={(e) => setDate(e.target.value)}
            />
          </div>

          <div>
            <label className="label" htmlFor="mtx-amount">
              Amount
            </label>
            <input
              id="mtx-amount"
              type="number"
              min="0"
              step="0.01"
              className="field w-full"
              placeholder="11.86"
              value={magnitude}
              onChange={(e) => setMagnitude(e.target.value)}
            />
          </div>

          <div className="sm:col-span-2">
            <span className="label">Type</span>
            <div className="mt-1 flex overflow-hidden rounded-lg border border-white/10">
              <button
                type="button"
                className={`flex-1 px-3 py-2 text-sm ${
                  !income ? 'bg-arcane-500/20 text-arcane-400' : 'text-mist-300'
                }`}
                onClick={() => setIncome(false)}
              >
                Expense (money out)
              </button>
              <button
                type="button"
                className={`flex-1 px-3 py-2 text-sm ${
                  income ? 'bg-verdant-400/20 text-verdant-400' : 'text-mist-300'
                }`}
                onClick={() => setIncome(true)}
              >
                Income / refund
              </button>
            </div>
          </div>

          <div className="sm:col-span-2">
            <label className="label" htmlFor="mtx-merchant">
              Merchant / description
            </label>
            <input
              id="mtx-merchant"
              className="field w-full"
              placeholder="Capital One — Amazon charge"
              value={merchant}
              onChange={(e) => setMerchant(e.target.value)}
            />
          </div>

          <div className="sm:col-span-2">
            <label className="label" htmlFor="mtx-category">
              Category (optional)
            </label>
            <select
              id="mtx-category"
              className="field w-full"
              value={categoryID}
              onChange={(e) => setCategoryID(e.target.value)}
            >
              <option value="">Uncategorised</option>
              {spendCategories(categories.data ?? []).map((c) => (
                <option key={c.id} value={c.id}>
                  {c.name}
                </option>
              ))}
            </select>
          </div>

          <div className="sm:col-span-2">
            <label className="label" htmlFor="mtx-notes">
              Notes (optional)
            </label>
            <input
              id="mtx-notes"
              className="field w-full"
              placeholder="Reconciled from July statement"
              value={notes}
              onChange={(e) => setNotes(e.target.value)}
            />
          </div>
        </div>

        {/* The change log is only meaningful on an existing row; a create has
            no history yet, and showing the empty state in the add form is noise. */}
        {editing && (
          <details className="text-sm">
            <summary className="cursor-pointer text-mist-300">History</summary>
            <HistoryPanel kind="transaction" objectId={editing.id} />
          </details>
        )}

        <div className="flex items-center gap-3">
          <button
            className="btn-primary px-4 py-2 text-sm"
            disabled={!canSave || save.isPending}
            onClick={submit}
          >
            {save.isPending ? 'Saving…' : editing ? 'Save changes' : 'Add transaction'}
          </button>
          <button
            className="btn-ghost px-3 py-2 text-sm text-mist-300"
            onClick={onClose}
          >
            Cancel
          </button>
          {save.isError && (
            <span role="alert" className="text-sm text-ember-400">
              {save.error.message}
            </span>
          )}
        </div>
      </div>
    </div>
  )
}

// EditMerchantNameDialog fixes a wrong merchant name on a synced/imported row.
//
// Plaid sometimes returns a descriptor that is just noise — a row of asterisks,
// a processor code, "#******* QPS" — for a charge that is really a known
// business. The name on the row can't simply be rewritten: the next sync's
// upsert overwrites transactions.merchant_name and merchant_key (only a manual
// category and the flags/notes are preserved). So the durable fix is to map the
// bad descriptor at the merchant layer — the same place a manual merge from the
// Merchants page lands. Re-pointing the descriptor:
//
//   * changes the name shown here and on every report (reports read the
//     canonical name), for this charge AND every past and future charge Plaid
//     sends with the same descriptor;
//   * survives sync, because the merchant_aliases table is never touched by it;
//   * lets the user "match the name to existing merchants" by folding the
//     descriptor into an existing canonical merchant, picked from a typeahead.
//
// That is the same rename/merge flow MerchantDetail already uses; this dialog
// just opens it from a transaction row.
function EditMerchantNameDialog({
  transaction: t,
  onClose,
}: {
  transaction: Transaction
  onClose: () => void
}) {
  const qc = useQueryClient()
  // The resolved name the row is currently showing — the canonical merchant
  // name when the descriptor has been grouped/renamed, else the raw bank text.
  // Reading the resolved name (not merchant_name ?? name) is what makes the
  // dialog reopen showing the post-rename state instead of the stale raw text.
  const currentDisplay = t.merchant
  const [name, setName] = useState(currentDisplay)
  // Set only by clicking a suggestion: saving then folds this descriptor into
  // that existing merchant instead of creating a new one. Cleared the moment
  // the text is hand-edited, so it never outlives the exact name it matched.
  const [matchID, setMatchID] = useState<string | null>(null)

  const merchantsList = useQuery({
    queryKey: ['merchants'],
    queryFn: api.merchantGroups,
  })

  const query = name.trim().toLowerCase()
  const suggestions =
    query.length >= 2 && merchantsList.data
      ? merchantsList.data
          .filter(
            (m) =>
              m.canonical_name.toLowerCase().includes(query) &&
              // Exclude the merchant this descriptor already resolves to —
              // merging into it would be a no-op. When the descriptor is not
              // yet grouped, the resolved key is the raw descriptor (not an
              // entity id) and matches nothing here.
              m.id !== t.merchant_key_resolved,
          )
          .slice(0, 6)
      : []

  const trimmed = name.trim()
  const changed =
    trimmed.toLowerCase() !== currentDisplay.toLowerCase()
  // There is something to save when the name changed OR a merge target was
  // picked (folding the descriptor into an existing merchant even if its name
  // happens to read the same as the current one).
  const canSave = trimmed !== '' && (changed || matchID !== null)

  const save = useMutation({
    mutationFn: async () => {
      // The RAW descriptor is what an alias attaches to. Sending the resolved
      // key instead would strand every other fragment of a grouped merchant.
      const key = t.merchant_key ?? ''
      if (matchID) {
        const result = await api.mergeMerchants({
          merchant_keys: [key],
          entity_id: matchID,
        })
        return result.entity_id
      }
      const result = await api.mergeMerchants({
        merchant_keys: [key],
        canonical_name: trimmed,
      })
      return result.entity_id
    },
    onSuccess: () => {
      // The merchant name feeds the row, every report total keyed by it, and
      // the merchant pages — refetch them all so nothing reads the old name.
      qc.invalidateQueries({ queryKey: ['transactions'] })
      qc.invalidateQueries({ queryKey: ['merchants'] })
      qc.invalidateQueries({ queryKey: ['merchant-keys'] })
      qc.invalidateQueries({ queryKey: ['merchant-detail'] })
      qc.invalidateQueries({ queryKey: ['top-merchants'] })
      qc.invalidateQueries({ queryKey: ['by-category'] })
      qc.invalidateQueries({ queryKey: ['summary'] })
      qc.invalidateQueries({ queryKey: ['averages'] })
      qc.invalidateQueries({ queryKey: ['recurring'] })
      onClose()
    },
  })

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4"
      role="dialog"
      aria-modal="true"
      onClick={onClose}
    >
      <form
        className="glass w-full max-w-lg space-y-4 p-6"
        onClick={(e) => e.stopPropagation()}
        onSubmit={(e) => {
          e.preventDefault()
          if (canSave && !save.isPending) save.mutate()
        }}
      >
        <div>
          <h2 className="text-lg font-medium">Edit merchant name</h2>
          <p className="mt-1 text-sm text-mist-300">
            Correct the real business behind this charge. The same descriptor
            arrives on every charge from this source, so this fixes the name
            here and on future ones too — not just this row.
          </p>
        </div>

        <div>
          <label className="label" htmlFor="emn-current">
            Currently shows as
          </label>
          <input
            id="emn-current"
            className="field w-full opacity-70"
            value={currentDisplay}
            readOnly
            tabIndex={-1}
          />
        </div>

        <div className="relative">
          <label className="label" htmlFor="emn-name">
            Real merchant name
          </label>
          <input
            id="emn-name"
            className="field w-full"
            maxLength={80}
            autoFocus
            value={name}
            onChange={(e) => {
              setName(e.target.value)
              setMatchID(null)
            }}
          />
          {suggestions.length > 0 && (
            <div className="absolute z-30 mt-1 w-full overflow-hidden rounded-2xl border border-white/10 bg-ink-950/90 p-1.5 shadow-xl shadow-black/40 backdrop-blur-xl">
              {suggestions.map((m) => (
                <button
                  key={m.id}
                  type="button"
                  className="block w-full truncate rounded-lg px-2 py-1.5 text-left text-sm text-mist-300 hover:bg-white/5"
                  onClick={() => {
                    setName(m.canonical_name)
                    setMatchID(m.id)
                  }}
                >
                  {m.canonical_name}
                </button>
              ))}
            </div>
          )}
        </div>

        {matchID && (
          <p className="text-xs text-arcane-300">
            Saving folds this charge into the existing “{name.trim()}”, so its
            history is counted with theirs.
          </p>
        )}

        <div className="flex items-center gap-3">
          <button
            type="submit"
            className="btn-primary px-4 py-2 text-sm"
            disabled={!canSave || save.isPending}
          >
            {save.isPending ? 'Saving…' : 'Save'}
          </button>
          <button
            type="button"
            className="btn-ghost px-3 py-2 text-sm text-mist-300"
            onClick={onClose}
          >
            Cancel
          </button>
          {save.isError && (
            <span role="alert" className="text-sm text-ember-400">
              {save.error.message}
            </span>
          )}
        </div>
      </form>
    </div>
  )
}

/** Categories a manual transaction can be filed under — transfers are noise. */
function spendCategories(categories: Category[]): Category[] {
  return categories.filter((c) => !c.is_transfer)
}
