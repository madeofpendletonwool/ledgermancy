import { useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../lib/api'
import type { LinkType, TransactionLink } from '../lib/api'
import { formatDate, formatTransactionAmount } from '../lib/money'
import { SkeletonRows } from './Skeleton'

/**
 * The links panel: what this transaction has been connected to, and how to
 * connect it to something else.
 *
 * The one thing to hold on to while reading this file: a link is an
 * ANNOTATION. Nothing here edits either transaction. Linking an $80 credit to
 * an $80 charge does not change the charge's amount, date or category — it
 * records that a person says the two are one event, and the only figure that
 * can move is one a reader has explicitly asked to net (the "Net linked
 * refunds" toggle on /spending). Unlinking undoes it completely, because there
 * was never anything to undo but the statement itself.
 *
 * DIRECTION IS STATED, NOT GUESSED. "This refunds that" and "this is refunded
 * by that" are the same pair and the opposite edge, and picking the wrong one
 * makes the netting view subtract the charge from the credit. So the picker
 * spells the sentence out in the words of the chosen link type rather than
 * offering an abstract "reverse" checkbox, and the app never infers a direction
 * from the two amounts' signs — a user who means something unusual is allowed
 * to mean it.
 *
 * Lazy, like AttachPanel: the queries only run once the panel is open, so a page
 * of fifty rows costs nothing until someone clicks.
 */
export function LinkPanel({
  transactionId,
  onClose,
}: {
  transactionId: string
  onClose: () => void
}) {
  const qc = useQueryClient()

  const links = useQuery({
    queryKey: ['transaction-links', transactionId],
    queryFn: () => api.transactionLinks(transactionId),
  })
  const types = useQuery({ queryKey: ['link-types'], queryFn: api.linkTypes })

  // Netting is computed server-side from the links, so any write here can move
  // a figure on the Spending page when that reader has the lens on. Invalidate
  // the two queries the toggle governs rather than leaving them stale.
  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ['transaction-links'] })
    for (const key of ['trend', 'averages']) {
      qc.invalidateQueries({ queryKey: [key] })
    }
  }

  const unlink = useMutation({
    mutationFn: (linkID: string) => api.unlinkTransaction(transactionId, linkID),
    onSuccess: invalidate,
  })

  return (
    <div className="absolute right-0 top-full z-40 mt-2 w-96 rounded-xl border border-white/10 bg-ink-850 p-4 shadow-xl">
      <h3 className="text-sm font-medium text-mist-200">Linked transactions</h3>
      <p className="mt-1 text-[11px] text-mist-500">
        A link records how two charges relate. It never changes either one.
      </p>

      {links.isPending ? (
        <SkeletonRows count={2} />
      ) : links.data && links.data.length > 0 ? (
        <ul className="mt-3 space-y-1.5">
          {links.data.map((link) => (
            <LinkRow
              key={link.id}
              link={link}
              onUnlink={() => unlink.mutate(link.id)}
              busy={unlink.isPending}
            />
          ))}
        </ul>
      ) : (
        <p className="mt-3 text-xs text-mist-500">Not linked to anything yet.</p>
      )}

      {unlink.isError && (
        <p className="mt-2 text-xs text-red-300">
          {(unlink.error as Error).message}
        </p>
      )}

      <AddLink
        transactionId={transactionId}
        types={types.data ?? []}
        existing={links.data ?? []}
        onLinked={invalidate}
      />

      <button
        className="mt-4 w-full rounded-lg py-1.5 text-xs text-mist-500 transition hover:text-mist-300"
        onClick={onClose}
      >
        Close
      </button>
    </div>
  )
}

/**
 * One existing link.
 *
 * `relation` arrives already oriented for this end — the server chose between
 * the type's outward and inward phrasing — so the sentence is rendered rather
 * than assembled. That is what keeps the two readings of one edge from drifting:
 * there is no branch here that could pick the wrong verb.
 */
function LinkRow({
  link,
  onUnlink,
  busy,
}: {
  link: TransactionLink
  onUnlink: () => void
  busy: boolean
}) {
  const amount = formatTransactionAmount(link.transaction.amount, link.transaction.currency)

  return (
    <li className="rounded-lg bg-white/5 px-2.5 py-2">
      <div className="flex items-start justify-between gap-2">
        <div className="min-w-0 flex-1">
          <p className="text-[11px] lowercase text-mist-500">{link.relation}</p>
          <p className="truncate text-xs text-mist-200">
            {link.transaction.merchant}
          </p>
          <p className="mt-0.5 text-[11px] text-mist-500">
            {formatDate(link.transaction.date)} · {link.transaction.account_name} ·{' '}
            <span className={amount.isIncome ? 'text-emerald-300' : ''}>
              {amount.text}
            </span>
          </p>
        </div>
        <button
          className="shrink-0 text-[11px] text-mist-500 transition hover:text-red-300 disabled:opacity-60"
          disabled={busy}
          onClick={onUnlink}
        >
          Unlink
        </button>
      </div>
      {/* Only shown for a link the netting lens acts on, and only as a fact
          about what COULD happen — the lens is off by default and the figures
          are unchanged until the reader turns it on. */}
      {link.nets_spend && (
        <p className="mt-1 text-[11px] text-mist-500">
          Counts against the charge when “Net linked refunds” is on.
        </p>
      )}
    </li>
  )
}

/**
 * The add-a-link form: find the other transaction, choose the relationship,
 * choose which way round it reads.
 *
 * Search rather than a dropdown of everything. The other end of a link is
 * usually a specific charge the user has in mind and often months old, which is
 * exactly the case a list of the recent fifty cannot serve. The query goes
 * through the same `q` grammar the transactions search bar uses, so `over:50`
 * and `since:-90d` work here too.
 */
function AddLink({
  transactionId,
  types,
  existing,
  onLinked,
}: {
  transactionId: string
  types: LinkType[]
  existing: TransactionLink[]
  onLinked: () => void
}) {
  const [query, setQuery] = useState('')
  const [picked, setPicked] = useState<string | null>(null)
  const [typeID, setTypeID] = useState('')
  const [outward, setOutward] = useState(true)

  // Two searches a second while someone types would be a lot of ledger scans for
  // very little; three characters is where a free-text search starts returning
  // something worth reading anyway.
  const term = query.trim()
  const results = useQuery({
    queryKey: ['transactions', 'link-search', term],
    queryFn: () => api.transactions({ q: term, limit: 15 }),
    enabled: term.length >= 3,
  })

  // The anchor cannot link to itself, and one link per pair means a transaction
  // already linked is not a candidate either. Filtering them out beats letting
  // the server reject the click with a 409.
  const taken = useMemo(
    () => new Set([transactionId, ...existing.map((l) => l.transaction.id)]),
    [transactionId, existing],
  )
  const candidates = (results.data ?? []).filter((t) => !taken.has(t.id))

  const type = types.find((t) => t.id === typeID)

  const link = useMutation({
    mutationFn: () =>
      api.linkTransaction(transactionId, {
        transaction_id: picked!,
        link_type_id: typeID,
        direction: outward ? 'outward' : 'inward',
      }),
    onSuccess: () => {
      setPicked(null)
      setQuery('')
      onLinked()
    },
  })

  const pickedRow = candidates.find((t) => t.id === picked)

  return (
    <div className="mt-4 border-t border-white/10 pt-3">
      <label className="label text-xs" htmlFor="link-search">
        Link to another transaction
      </label>
      <input
        id="link-search"
        className="field py-2 text-xs"
        placeholder="Search by merchant, or over:50, since:-90d…"
        value={query}
        onChange={(e) => {
          setQuery(e.target.value)
          setPicked(null)
        }}
      />

      {term.length >= 3 && (
        <div className="mt-2 max-h-40 overflow-y-auto rounded-lg border border-white/5">
          {results.isPending ? (
            <SkeletonRows count={2} />
          ) : candidates.length === 0 ? (
            <p className="px-2.5 py-2 text-[11px] text-mist-500">
              Nothing matched. Only transactions you can see are linkable.
            </p>
          ) : (
            <ul>
              {candidates.map((t) => {
                const amount = formatTransactionAmount(t.amount, t.currency)
                return (
                  <li key={t.id}>
                    <button
                      type="button"
                      className={`flex w-full items-center justify-between gap-2 px-2.5 py-1.5 text-left text-[11px] transition hover:bg-white/5 ${
                        picked === t.id ? 'bg-white/10' : ''
                      }`}
                      onClick={() => setPicked(t.id)}
                    >
                      <span className="min-w-0 flex-1 truncate text-mist-200">
                        {t.merchant}
                      </span>
                      <span className="shrink-0 text-mist-500">
                        {formatDate(t.date)}
                      </span>
                      <span
                        className={`tabular shrink-0 ${
                          amount.isIncome ? 'text-emerald-300' : 'text-mist-300'
                        }`}
                      >
                        {amount.text}
                      </span>
                    </button>
                  </li>
                )
              })}
            </ul>
          )}
        </div>
      )}

      {picked && (
        <div className="mt-3 space-y-2">
          <select
            className="field py-2 text-xs"
            value={typeID}
            aria-label="Relationship"
            onChange={(e) => setTypeID(e.target.value)}
          >
            <option value="">Choose a relationship…</option>
            {types.map((t) => (
              <option key={t.id} value={t.id}>
                {t.name}
              </option>
            ))}
          </select>

          {/* The direction picker, spelled out as the sentence it will store.
              A symmetric type ("relates to") reads identically both ways, so it
              is offered as one option rather than two that look like a bug. */}
          {type && (
            <fieldset className="space-y-1">
              <legend className="text-[11px] text-mist-500">
                Which way round?
              </legend>
              {(type.outward === type.inward
                ? [{ out: true, phrase: type.outward }]
                : [
                    { out: true, phrase: type.outward },
                    { out: false, phrase: type.inward },
                  ]
              ).map((option) => (
                <label
                  key={String(option.out)}
                  className="flex items-start gap-2 text-[11px] text-mist-300"
                >
                  <input
                    type="radio"
                    name="link-direction"
                    className="mt-0.5"
                    checked={outward === option.out}
                    onChange={() => setOutward(option.out)}
                  />
                  <span>
                    This transaction <em className="not-italic text-mist-100">{option.phrase}</em>{' '}
                    {pickedRow?.merchant ?? 'the selected one'}
                  </span>
                </label>
              ))}
            </fieldset>
          )}

          {type?.nets_spend && (
            <p className="text-[11px] text-mist-500">
              Refund links can be netted against the charge they refund on the
              Spending page. Nothing changes until you turn that on.
            </p>
          )}

          {link.isError && (
            <p className="text-xs text-red-300">{(link.error as Error).message}</p>
          )}

          <button
            className="btn-primary w-full px-3 py-1.5 text-xs"
            disabled={!typeID || link.isPending}
            onClick={() => link.mutate()}
          >
            {link.isPending ? 'Linking…' : 'Link'}
          </button>
        </div>
      )}
    </div>
  )
}
