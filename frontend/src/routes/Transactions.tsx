import { useEffect, useRef, useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient, keepPreviousData } from '@tanstack/react-query'
import {
  api,
  ApiError,
  type Account,
  type Category,
  type ManualTransactionInput,
  type Tag,
  type Transaction,
} from '../lib/api'
import { formatDate, formatTransactionAmount } from '../lib/money'
import { AnchoredPopover } from '../components/AnchoredPopover'
import { AttachPanel, PaperclipIcon } from '../components/AttachDocuments'
import { BulkActionBar } from '../components/BulkActionBar'
import { HistoryPanel } from '../components/HistoryPanel'
import { MerchantLink } from '../components/MerchantLink'
import { MerchantAvatar } from '../components/MerchantAvatar'
import { SplitPanel } from '../components/SplitTransaction'
import { LinkPanel } from '../components/LinkTransactions'
import { TransactionSearchBar } from '../components/TransactionSearchBar'
import { ImportTransactionsModal } from '../components/ImportTransactionsModal'
import { enterProps } from '../lib/motion'
import { OFFLINE_WRITE_HINT, useOnline } from '../lib/offline'
import { motion } from 'motion/react'

// How many rows a page may hold. 500 is the ceiling the list endpoint clamps to
// (see parseInt in ledger_handlers.go), so nothing here can ask for more than
// the server will give. 50 stays the default: it is what the page has always
// shown, and it is the size that loads instantly on a phone.
const PAGE_SIZE_OPTIONS = [50, 100, 200, 500]
const DEFAULT_PAGE_SIZE = 50

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
  // Tag filters. `tags` is OR-ed across the selected ids (the same shape as the
  // account filter), and `untagged` is its opposite — the backlog of charges
  // nobody has labelled. Both are ignored together when neither is set.
  const tagIDs = (searchParams.get('tags') || '').split(',').filter(Boolean)
  const onlyUntagged = searchParams.get('untagged') === '1'
  const page = Math.max(0, Number(searchParams.get('page') || '0') || 0)
  // Whitelisted rather than clamped, so a hand-edited `?size=9999` falls back to
  // the default instead of quietly becoming some other number.
  const requestedSize = Number(searchParams.get('size'))
  const pageSize = PAGE_SIZE_OPTIONS.includes(requestedSize)
    ? requestedSize
    : DEFAULT_PAGE_SIZE

  const [modal, setModal] = useState<ModalState>(null)
  const [importing, setImporting] = useState(false)
  // The transaction whose merchant name is being fixed in the rename dialog.
  // Separate from `modal` (the full add/edit form), because the rename path is
  // the one a Plaid-synced row can take: it re-points the descriptor rather than
  // editing columns the next sync would overwrite.
  const [editingName, setEditingName] = useState<Transaction | null>(null)
  // The list itself reads fine from cache offline; adding and importing do not.
  const online = useOnline()

  // Multi-select is opt-in. A ledger of fifty rows is something you read far
  // more often than something you act on in bulk, so the checkbox column only
  // exists once you have asked for it — the default row keeps every pixel for
  // the merchant name.
  const [selectMode, setSelectMode] = useState(false)
  const [selectedIDs, setSelectedIDs] = useState<string[]>([])
  // Anchor for shift-click ranges: the last row toggled on its own.
  const lastPicked = useRef<number | null>(null)

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
  const toggleTag = (id: string) => {
    const set = new Set(tagIDs)
    if (set.has(id)) {
      set.delete(id)
    } else {
      set.add(id)
    }
    patchParams({ tags: [...set].join(',') || null })
  }

  const accounts = useQuery({ queryKey: ['accounts'], queryFn: api.accounts })
  const categories = useQuery({ queryKey: ['categories'], queryFn: api.categories })
  const tags = useQuery({ queryKey: ['tags'], queryFn: api.tags })

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
      tagIDs.join(','),
      onlyUntagged,
      page,
      pageSize,
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
        tags: tagIDs,
        untagged: onlyUntagged || undefined,
        limit: pageSize,
        offset: page * pageSize,
      }),
    // Keeps the previous page on screen while the next loads, so paging does
    // not flash an empty table.
    placeholderData: keepPreviousData,
    // A 400 is the search grammar rejecting a value (`over:banana`). Retrying it
    // three times cannot change the answer and only delays telling the user.
    retry: (attempt, error) =>
      !(error instanceof ApiError && error.status === 400) && attempt < 2,
  })

  const rows = transactions.data ?? []
  const isLastPage = rows.length < pageSize
  // The one error worth putting in front of the user rather than logging: their
  // query said something the parser could not resolve, and the message names it.
  const searchError =
    transactions.error instanceof ApiError && transactions.error.status === 400
      ? transactions.error.message
      : null

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

  // --- Selection ----------------------------------------------------------
  //
  // Everything below is scoped to the rows currently on screen. A bulk action
  // must never reach something the user cannot see, so the selection is dropped
  // whenever the result set itself changes — a different filter, page or page
  // size. A refetch does not change any of those, which is what lets a selection
  // survive an action and be acted on again ("tag these, now categorise them").
  const selectedSet = new Set(selectedIDs)
  const pageIDs = rows.map((t) => t.id)
  const selectedOnPage = pageIDs.filter((id) => selectedSet.has(id))
  const allSelected = rows.length > 0 && selectedOnPage.length === rows.length

  const filterKey = [
    from,
    to,
    accountIDs.join(','),
    categoryFilter,
    merchantFilter,
    debouncedSearch,
    String(onlyUncat),
    String(showExcluded),
    tagIDs.join(','),
    String(onlyUntagged),
    String(page),
    String(pageSize),
  ].join('|')
  useEffect(() => {
    setSelectedIDs([])
    lastPicked.current = null
  }, [filterKey])

  // Shift-click extends from the last row picked on its own, matching how every
  // other list on a desktop behaves. The anchor's own state decides the whole
  // range's, so dragging back over a range clears it rather than flipping each
  // row individually.
  const toggleSelected = (index: number, shiftKey: boolean) => {
    const anchor = lastPicked.current
    setSelectedIDs((prev) => {
      const next = new Set(prev)
      if (shiftKey && anchor !== null && anchor !== index) {
        const [lo, hi] = anchor < index ? [anchor, index] : [index, anchor]
        const turningOn = !next.has(rows[index].id)
        for (let i = lo; i <= hi; i++) {
          const id = rows[i]?.id
          if (!id) continue
          if (turningOn) next.add(id)
          else next.delete(id)
        }
      } else {
        const id = rows[index].id
        if (next.has(id)) next.delete(id)
        else next.add(id)
      }
      return [...next]
    })
    if (!shiftKey) lastPicked.current = index
  }

  const toggleSelectAll = () => {
    setSelectedIDs(allSelected ? [] : pageIDs)
    lastPicked.current = null
  }

  const leaveSelectMode = () => {
    setSelectMode(false)
    setSelectedIDs([])
    lastPicked.current = null
  }

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
          {/* The switch that reveals the checkbox column. Off by default and
              off again the moment you leave, so a row that is only being read
              carries nothing it does not need. */}
          <button
            className="btn-ghost px-4 py-2 text-sm"
            disabled={!online}
            title={
              online
                ? 'Tick rows to tag, categorise or reclassify them together'
                : OFFLINE_WRITE_HINT
            }
            aria-pressed={selectMode}
            onClick={() => (selectMode ? leaveSelectMode() : setSelectMode(true))}
          >
            {selectMode ? 'Done' : 'Select'}
          </button>
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

        {/* A multi-select rather than the category dropdown's single choice,
            because tags are multi-valued by nature: "the trip or the remodel"
            is a question that has no shape in a <select>. Hidden entirely until
            the household has a tag, so the bar does not carry a control that
            can only say "none". */}
        {(tags.data ?? []).length > 0 && (
          <div>
            <span className="label">Tags</span>
            <TagMultiSelect
              tags={tags.data ?? []}
              selected={tagIDs}
              onToggle={toggleTag}
              onClear={() => patchParams({ tags: null })}
            />
          </div>
        )}

        {/* A bare word here still searches merchant and description, exactly as
            this box always did. The chips above compose with whatever is typed,
            so the grammar is an addition rather than a replacement. */}
        <TransactionSearchBar
          value={search}
          onChange={(next) => patchParams({ q: next || null })}
        />

        <label className="flex items-center gap-2 pb-2 text-sm text-mist-300">
          <input
            type="checkbox"
            checked={onlyUncat}
            onChange={(e) => patchParams({ uncat: e.target.checked ? '1' : null })}
          />
          Needs a category
        </label>

        {(tags.data ?? []).length > 0 && (
          <label
            className="flex items-center gap-2 pb-2 text-sm text-mist-300"
            title="Charges nobody has labelled yet — the backlog for a trip or a project."
          >
            <input
              type="checkbox"
              checked={onlyUntagged}
              onChange={(e) => patchParams({ untagged: e.target.checked ? '1' : null })}
            />
            Untagged
          </label>
        )}

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

      {searchError && (
        <p className="text-sm text-amber-300/90" role="status">
          {searchError}
        </p>
      )}

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
          <>
            {/* Only in select mode, so the list gains a header row exactly when
                there is something for it to say. */}
            {selectMode && (
              <div className="flex items-center gap-3 border-b border-white/10 px-4 py-2.5 text-xs text-mist-400 sm:gap-4 sm:px-6">
                <SelectAllCheckbox
                  checked={allSelected}
                  partial={selectedOnPage.length > 0 && !allSelected}
                  onChange={toggleSelectAll}
                />
                <span>
                  {selectedOnPage.length} of {rows.length} selected
                </span>
                <span className="ml-auto hidden text-mist-500 sm:inline">
                  Shift-click to pick a range
                </span>
              </div>
            )}
            <ul className="divide-y divide-white/5">
              {rows.map((t, i) => (
                <TransactionRow
                  key={t.id}
                  transaction={t}
                  index={i}
                  categoryById={categoryById}
                  spendCats={spendCats}
                  documentCount={countFor(t.id)}
                  selectMode={selectMode}
                  selected={selectedSet.has(t.id)}
                  onToggleSelect={toggleSelected}
                  onEdit={() => setModal({ mode: 'edit', transaction: t })}
                  onEditName={() => setEditingName(t)}
                />
              ))}
            </ul>
          </>
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
        <div className="flex items-center gap-3">
          <span className="text-sm text-mist-500">Page {page + 1}</span>
          {/* Changing the size goes through patchParams without keepPage, so it
              lands you back on page 1 — page 7 of fifties is not page 7 of
              five-hundreds, and staying put would scroll you somewhere you did
              not ask to be. */}
          <label className="flex items-center gap-2 text-sm text-mist-500">
            <span className="sr-only sm:not-sr-only">Per page</span>
            <select
              className="field py-1 text-sm"
              value={pageSize}
              aria-label="Transactions per page"
              onChange={(e) =>
                patchParams({
                  size:
                    Number(e.target.value) === DEFAULT_PAGE_SIZE
                      ? null
                      : e.target.value,
                })
              }
            >
              {PAGE_SIZE_OPTIONS.map((n) => (
                <option key={n} value={n}>
                  {n}
                </option>
              ))}
            </select>
          </label>
        </div>
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

      {selectMode && selectedIDs.length > 0 && (
        <BulkActionBar
          selectedIDs={selectedIDs}
          spendCats={spendCats}
          onClear={() => {
            setSelectedIDs([])
            lastPicked.current = null
          }}
        />
      )}
    </div>
  )
}

/**
 * "Select every row on this page", with the third state a plain checkbox has no
 * attribute for: `indeterminate` is DOM-only, so it has to be written through a
 * ref rather than passed as a prop.
 */
function SelectAllCheckbox({
  checked,
  partial,
  onChange,
}: {
  checked: boolean
  partial: boolean
  onChange: () => void
}) {
  const ref = useRef<HTMLInputElement>(null)
  useEffect(() => {
    if (ref.current) ref.current.indeterminate = partial
  }, [partial])

  return (
    <input
      ref={ref}
      type="checkbox"
      className="shrink-0"
      checked={checked}
      onChange={onChange}
      aria-label="Select every transaction on this page"
    />
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

// TagMultiSelect is the tag filter: a checkbox dropdown, same <details> popover
// as AccountMultiSelect. An empty selection means "every tag and none", i.e. no
// filter, so the common case needs no clicks. Selecting several ORs them —
// picking "Vacation" and "Remodel" shows both, which is what a household asks
// for; an AND across tags would return the tiny set of charges that are both.
function TagMultiSelect({
  tags,
  selected,
  onToggle,
  onClear,
}: {
  tags: Tag[]
  selected: string[]
  onToggle: (id: string) => void
  onClear: () => void
}) {
  const label =
    selected.length === 0
      ? 'All tags'
      : selected.length === 1
        ? (tags.find((t) => t.id === selected[0])?.name ?? '1 tag')
        : `${selected.length} tags`

  return (
    <details className="relative">
      <summary className="field flex w-48 cursor-pointer list-none items-center justify-between">
        <span className="truncate">{label}</span>
        <span className="ml-2 text-mist-500">▾</span>
      </summary>
      <div className="absolute z-30 mt-1 max-h-72 w-60 overflow-auto rounded-2xl border border-white/10 bg-ink-950/90 p-1.5 shadow-xl shadow-black/40 backdrop-blur-xl">
        <button
          className="w-full rounded-lg px-2 py-1.5 text-left text-sm text-mist-300 hover:bg-white/5"
          onClick={onClear}
        >
          All tags
        </button>
        {tags.map((t) => (
          <label
            key={t.id}
            className="flex cursor-pointer items-center gap-2 rounded-lg px-2 py-1.5 text-sm hover:bg-white/5"
          >
            <input
              type="checkbox"
              checked={selected.includes(t.id)}
              onChange={() => onToggle(t.id)}
            />
            <span className="truncate">{t.name}</span>
          </label>
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
  selectMode,
  selected,
  onToggleSelect,
  onEdit,
  onEditName,
}: {
  transaction: Transaction
  index: number
  categoryById: Map<string, Category>
  spendCats: Category[]
  documentCount: number
  /** Whether the checkbox column exists at all. Off, the row is untouched. */
  selectMode: boolean
  selected: boolean
  onToggleSelect: (index: number, shiftKey: boolean) => void
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
      {/* The checkbox exists only in select mode — no reserved gutter, no
          hidden column. The click is read off the input rather than the row so
          the merchant link, the category pill and the ⋯ menu all keep working
          exactly as they do outside select mode. */}
      {selectMode && (
        <input
          type="checkbox"
          className="shrink-0"
          checked={selected}
          aria-label={`Select ${t.merchant}`}
          onChange={() => undefined}
          onClick={(e) => onToggleSelect(index, e.shiftKey)}
        />
      )}

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

        {/* Read-only chips on their own line. Editing them lives in the ⋯ menu
            with the row's other actions, so a ledger of fifty rows still reads
            as a ledger rather than as fifty tag pickers. */}
        {t.tags.length > 0 && (
          <p className="mt-1 flex flex-wrap items-center gap-1.5">
            {t.tags.map((tag) => (
              <span
                key={tag.id}
                className="rounded-full border border-rune-400/30 bg-rune-400/10 px-2 py-0.5 text-[10px] text-rune-200"
              >
                {tag.name}
              </span>
            ))}
          </p>
        )}

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
  // The ⋯ button's wrapper. Every panel is portalled to <body> and positioned
  // against this rect, so nothing the row is nested in can clip it.
  const anchorRef = useRef<HTMLDivElement>(null)
  // The rename path needs a descriptor to re-point. Manual rows already have a
  // full editor; scheduled rows are owned by the obligation that posts them.
  const canEditName =
    Boolean(t.merchant_key) && t.source !== 'manual' && t.source !== 'scheduled'
  // Which popover this row is showing, if any — the menu itself or one of the
  // three it opens.
  const [panel, setPanel] = useState<
    'menu' | 'split' | 'documents' | 'tags' | 'links' | null
  >(null)
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
    <div ref={anchorRef} className="relative shrink-0">
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

      {/* Every panel goes through one anchor, portalled to <body>. The row list
          is a `.glass overflow-hidden` card, which both clips an absolutely
          positioned panel (a menu on the last row was cut off) and — because
          backdrop-filter makes it the containing block for fixed children —
          shrank the old click-away layer down to the card, so clicking the
          sidebar or the header never dismissed anything. See AnchoredPopover.
          The children keep their own `absolute right-0` classes: the anchor
          reproduces this button's rect, so they land exactly where they did. */}
      {open && (
        <AnchoredPopover anchorRef={anchorRef} onClose={close}>
          {panel === 'split' && (
            <SplitPanel transactionId={t.id} amount={t.amount} onClose={close} />
          )}

          {panel === 'documents' && (
            <AttachPanel target={{ kind: 'transaction', id: t.id }} onClose={close} />
          )}

          {panel === 'tags' && <TagPanel transaction={t} onClose={close} />}

          {panel === 'links' && <LinkPanel transactionId={t.id} onClose={close} />}

          {panel === 'menu' && (
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

              {/* What this charge was FOR, as opposed to what kind of spending it
                  is. Offered on every row including synced ones: a hotel charge
                  from Plaid is exactly what "Summer Vacation" has to land on. */}
              <button
                type="button"
                role="menuitem"
                className="w-full rounded px-2 py-1.5 text-left transition hover:bg-white/5 disabled:opacity-60"
                disabled={!online}
                title={online ? undefined : OFFLINE_WRITE_HINT}
                onClick={() => setPanel('tags')}
              >
                <span className="block font-medium text-mist-100">
                  {t.tags.length > 0 ? `Tags (${t.tags.length})` : 'Add tags…'}
                </span>
                <span className="block text-mist-500">
                  {t.tags.length > 0
                    ? 'Change what this charge was for.'
                    : 'Label it — a trip, a project, a person.'}
                </span>
              </button>

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

              {/* How this charge relates to ANOTHER charge — the refund that
                  cancelled it, the duplicate it turned out to be, the dinner it
                  paid for. Sits beside the documents action because it is the
                  same kind of thing: something attached to the row that leaves
                  the row itself untouched. Offered on every row including synced
                  ones, for the same reason tags are. */}
              <button
                type="button"
                role="menuitem"
                className="w-full rounded px-2 py-1.5 text-left transition hover:bg-white/5 disabled:opacity-60"
                disabled={!online}
                title={online ? undefined : OFFLINE_WRITE_HINT}
                onClick={() => setPanel('links')}
              >
                <span className="block font-medium text-mist-100">
                  Link to another transaction…
                </span>
                <span className="block text-mist-500">
                  A refund, a duplicate, something it paid for.
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
          )}
        </AnchoredPopover>
      )}
    </div>
  )
}

// TagPanel is the row's tag editor: a checkbox per household tag, confirmed as
// a set. It writes the whole set rather than deltas, which is what the UI
// actually is — the user ticks boxes and presses Save.
//
// "All from this merchant" ADDS these tags to every charge from the same
// merchant. It deliberately does NOT mirror the category version's replace: a
// category is single-valued so applying one to a merchant necessarily
// overwrites, while replacing a tag set across a merchant would silently strip
// labels somebody put there for unrelated reasons ("Reimbursable" wiped by
// "Groceries 2026"). Adding is the only version that cannot destroy work, and
// the checkbox says so.
function TagPanel({
  transaction: t,
  onClose,
}: {
  transaction: Transaction
  onClose: () => void
}) {
  const qc = useQueryClient()
  const tags = useQuery({ queryKey: ['tags'], queryFn: api.tags })
  const [selected, setSelected] = useState<string[]>(t.tags.map((tag) => tag.id))
  const [applyToMerchant, setApplyToMerchant] = useState(false)
  // Gate on merchant_key (what the server matches on), not the display name —
  // many synced rows have a key derived from the name with no merchant_name.
  const hasMerchant = Boolean(t.merchant_key)

  const save = useMutation({
    mutationFn: () =>
      api.setTransactionTags(t.id, selected, applyToMerchant && hasMerchant),
    onSuccess: () => {
      // Tag totals feed the Tags page and the by-tag breakdown, and applying to
      // a merchant changes rows other than this one — so the whole list
      // refetches rather than patching this row in place.
      for (const key of ['transactions', 'tags', 'by-tag', 'tag-detail']) {
        qc.invalidateQueries({ queryKey: [key] })
      }
      onClose()
    },
  })

  const toggle = (id: string) =>
    setSelected((prev) =>
      prev.includes(id) ? prev.filter((x) => x !== id) : [...prev, id],
    )

  const rows = tags.data ?? []

  return (
    <div className="absolute right-0 z-40 mt-1 w-64 rounded-xl border border-white/10 bg-ink-950/95 p-4 shadow-xl backdrop-blur-xl">
      <p className="mb-1 text-xs font-medium text-mist-100">What was this for?</p>
      <p className="mb-3 text-[11px] text-mist-500">
        Tags sit alongside the category, never instead of it.
      </p>

      {tags.isPending ? (
        <p className="text-xs text-mist-500">Loading…</p>
      ) : rows.length === 0 ? (
        <p className="text-xs text-mist-500">
          No tags yet. Create one on the{' '}
          <Link to="/tags" className="text-rune-300 hover:text-rune-100">
            Tags page
          </Link>
          .
        </p>
      ) : (
        <div className="max-h-48 space-y-1 overflow-auto">
          {rows.map((tag) => (
            <label
              key={tag.id}
              className="flex cursor-pointer items-center gap-2 rounded px-1 py-1 text-xs hover:bg-white/5"
            >
              <input
                type="checkbox"
                checked={selected.includes(tag.id)}
                onChange={() => toggle(tag.id)}
              />
              <span className="truncate">{tag.name}</span>
            </label>
          ))}
        </div>
      )}

      {rows.length > 0 && hasMerchant && (
        <label
          className="mt-3 flex items-start gap-2 text-[11px] text-mist-400"
          title="Adds these tags to every charge from this merchant. It never removes tags already there."
        >
          <input
            type="checkbox"
            className="mt-0.5"
            checked={applyToMerchant}
            onChange={(e) => setApplyToMerchant(e.target.checked)}
          />
          <span>Also add to all from {t.merchant}</span>
        </label>
      )}

      {save.isError && (
        <p role="alert" className="mt-2 text-[11px] text-ember-400">
          {save.error.message}
        </p>
      )}

      <div className="mt-3 flex items-center justify-end gap-2">
        <button className="btn-ghost px-2 py-1 text-xs text-mist-300" onClick={onClose}>
          Cancel
        </button>
        <button
          className="btn-ghost px-2 py-1 text-xs"
          disabled={save.isPending || rows.length === 0}
          onClick={() => save.mutate()}
        >
          {save.isPending ? 'Saving…' : 'Save'}
        </button>
      </div>
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
