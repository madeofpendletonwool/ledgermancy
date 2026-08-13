import { useState } from 'react'
import { Link } from 'react-router-dom'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../lib/api'
import type { Tag } from '../lib/api'
import { formatDate, formatMoney } from '../lib/money'
import { AnimatedNumber } from '../components/motion'
import { SkeletonRows, Reveal } from '../components/Skeleton'
import { EmptyState } from '../components/EmptyState'
import { STATUS } from '../components/charts/tokens'

/**
 * Tags — free-form labels that cut ACROSS categories.
 *
 * THE DISTINCTION THIS PAGE EXISTS TO MAKE VISIBLE: a category answers "what
 * kind of spending is this?" and a transaction has exactly one. A tag answers
 * "what was this FOR?" and a transaction can carry several. A vacation spans
 * Food, Travel and Lodging; a remodel spans fees, materials and appliances.
 * Neither the category nor the merchant view can express either, which is why
 * this is a second axis rather than more categories.
 *
 * With an expected amount a tag becomes an ENVELOPE — "the trip was meant to
 * cost 3,000" — and the bar below reads spent against that. Both figures come
 * from the server; nothing on this page adds money up.
 */
export function Tags() {
  const tags = useQuery({ queryKey: ['tags'], queryFn: api.tags })
  const rows = tags.data ?? []

  return (
    <div className="space-y-8">
      <div>
        <h1 className="text-2xl font-semibold">Tags</h1>
        <p className="mt-1 text-mist-300">
          Labels that cut across categories — &ldquo;Summer Vacation&rdquo;,
          &ldquo;Reimbursable&rdquo;, &ldquo;Kitchen Remodel&rdquo;.
        </p>
        <p className="mt-2 text-xs text-mist-500">
          A transaction has one category but any number of tags, so tagging a
          charge never changes what kind of spending it is. Give a tag an
          expected amount and it becomes an envelope you can track against.
        </p>
      </div>

      <section className="glass p-6">
        <h2 className="mb-1 text-lg font-medium">Your tags</h2>
        <p className="mb-5 text-sm text-mist-300">
          Totals count only the transactions you can see, and follow the same
          rules as every other spending figure — excluded and pending rows out,
          transfers out.
        </p>

        {tags.isPending ? (
          <SkeletonRows count={3} />
        ) : rows.length === 0 ? (
          <EmptyState
            title="No tags yet"
            icon={<TagGlyph />}
            action={
              <a href="#add-tag" className="btn-primary">
                Add a tag
              </a>
            }
          >
            Name something that spans several categories — a trip, a project, a
            person you get reimbursed by — then stick it on charges from the
            transactions list.
          </EmptyState>
        ) : (
          <Reveal>
            <div className="space-y-3">
              {rows.map((t) => (
                <TagCard key={t.id} tag={t} />
              ))}
            </div>
          </Reveal>
        )}
      </section>

      <CreateTag />
    </div>
  )
}

function TagCard({ tag }: { tag: Tag }) {
  const qc = useQueryClient()
  const [editing, setEditing] = useState(false)
  const [showCharges, setShowCharges] = useState(false)

  const remove = useMutation({
    mutationFn: () => api.deleteTag(tag.id),
    onSuccess: () => invalidateTags(qc),
  })

  // Display-only percentage from two server-exact figures. A tag with no
  // expected amount renders no bar — there is nothing to be "toward".
  const expected = tag.expected_amount ? Number(tag.expected_amount) : 0
  const spent = Number(tag.total)
  const pct = expected > 0 ? Math.min((spent / expected) * 100, 100) : 0
  const over = expected > 0 && spent > expected

  return (
    <div className="rounded-xl border border-white/5 p-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <p className="font-medium">
            {tag.name}
            {over && (
              <span className="ml-2 text-xs font-normal" style={{ color: STATUS.critical }}>
                over
              </span>
            )}
          </p>
          {tag.description && (
            <p className="mt-0.5 text-sm text-mist-400">{tag.description}</p>
          )}
          <p className="mt-0.5 text-sm text-mist-400">
            <span className="tabular">
              <AnimatedNumber value={tag.total} />
              {tag.expected_amount && (
                <>
                  {' '}
                  of <AnimatedNumber value={tag.expected_amount} />
                </>
              )}
            </span>{' '}
            · {tag.transaction_count}{' '}
            {tag.transaction_count === 1 ? 'transaction' : 'transactions'}
          </p>
        </div>

        <div className="flex shrink-0 items-center gap-2">
          <button
            className="btn-ghost px-2.5 py-1 text-xs"
            onClick={() => setShowCharges((v) => !v)}
          >
            {showCharges ? 'Hide charges' : 'Charges'}
          </button>
          <button
            className="btn-ghost px-2.5 py-1 text-xs"
            onClick={() => setEditing((v) => !v)}
          >
            {editing ? 'Cancel' : 'Edit'}
          </button>
          <button
            className="btn-ghost px-2.5 py-1 text-xs text-ember-400"
            disabled={remove.isPending}
            title="Deleting a tag unlabels its transactions. The transactions themselves stay."
            onClick={() => remove.mutate()}
          >
            {remove.isPending ? 'Deleting…' : 'Delete'}
          </button>
        </div>
      </div>

      {expected > 0 && (
        <div className="mt-3 h-1.5 overflow-hidden rounded-full bg-white/5">
          <div
            className="h-full rounded-full"
            style={{
              width: `${pct}%`,
              backgroundColor: over ? STATUS.critical : '#9085e9',
            }}
          />
        </div>
      )}

      {remove.isError && (
        <p role="alert" className="mt-3 text-sm text-ember-400">
          {remove.error.message}
        </p>
      )}

      {editing && <EditTag tag={tag} onDone={() => setEditing(false)} />}
      {showCharges && <TagCharges tagID={tag.id} />}
    </div>
  )
}

/** Every surface that reads a tag's figures refetches after a write: the list
 *  here, the by-tag breakdown on Spending, and the chips on the ledger rows. */
function invalidateTags(qc: ReturnType<typeof useQueryClient>) {
  qc.invalidateQueries({ queryKey: ['tags'] })
  qc.invalidateQueries({ queryKey: ['by-tag'] })
  qc.invalidateQueries({ queryKey: ['transactions'] })
  qc.invalidateQueries({ queryKey: ['tag-detail'] })
}

function EditTag({ tag, onDone }: { tag: Tag; onDone: () => void }) {
  const qc = useQueryClient()
  const [name, setName] = useState(tag.name)
  const [description, setDescription] = useState(tag.description ?? '')
  const [expected, setExpected] = useState(tag.expected_amount ?? '')

  const save = useMutation({
    mutationFn: () =>
      api.updateTag(tag.id, {
        name: name.trim(),
        description: description.trim() || null,
        // An emptied field clears the target rather than leaving the old one:
        // the form shows exactly what will be stored.
        expected_amount: expected.trim() || null,
      }),
    onSuccess: () => {
      invalidateTags(qc)
      onDone()
    },
  })

  return (
    <div className="mt-4 grid gap-3 border-t border-white/5 pt-4 sm:grid-cols-3">
      <div>
        <label className="label" htmlFor={`tag-name-${tag.id}`}>
          Name
        </label>
        <input
          id={`tag-name-${tag.id}`}
          className="field w-full"
          value={name}
          onChange={(e) => setName(e.target.value)}
        />
      </div>
      <div>
        <label className="label" htmlFor={`tag-desc-${tag.id}`}>
          Description
        </label>
        <input
          id={`tag-desc-${tag.id}`}
          className="field w-full"
          placeholder="What it's for"
          value={description}
          onChange={(e) => setDescription(e.target.value)}
        />
      </div>
      <div>
        <label className="label" htmlFor={`tag-expected-${tag.id}`}>
          Expected total
        </label>
        <input
          id={`tag-expected-${tag.id}`}
          className="field w-full"
          type="number"
          min="0"
          step="0.01"
          placeholder="No target"
          value={expected}
          onChange={(e) => setExpected(e.target.value)}
        />
      </div>
      <div className="flex items-center gap-3 sm:col-span-3">
        <button
          className="btn-primary px-4 py-2 text-sm"
          disabled={name.trim() === '' || save.isPending}
          onClick={() => save.mutate()}
        >
          {save.isPending ? 'Saving…' : 'Save'}
        </button>
        {save.isError && (
          <span role="alert" className="text-sm text-ember-400">
            {save.error.message}
          </span>
        )}
      </div>
    </div>
  )
}

/** The charges behind one tag. Fetched on demand — a tag list of twenty must
 *  not cost twenty transaction queries on load. */
function TagCharges({ tagID }: { tagID: string }) {
  const detail = useQuery({
    queryKey: ['tag-detail', tagID],
    queryFn: () => api.tagDetail(tagID),
  })
  const rows = detail.data?.transactions ?? []

  if (detail.isPending) {
    return (
      <div className="mt-4 border-t border-white/5 pt-4">
        <SkeletonRows count={3} />
      </div>
    )
  }

  return (
    <div className="mt-4 border-t border-white/5 pt-4">
      {rows.length === 0 ? (
        <p className="text-sm text-mist-500">
          Nothing carries this tag yet. Add it to a charge from the{' '}
          <Link to="/transactions" className="text-rune-300 hover:text-rune-100">
            transactions list
          </Link>
          .
        </p>
      ) : (
        <ul className="divide-y divide-white/5 text-sm">
          {rows.map((t) => (
            <li key={t.id} className="flex items-center gap-3 py-2">
              <span className="w-24 shrink-0 text-mist-500">{formatDate(t.date)}</span>
              <span className="min-w-0 flex-1 truncate">{t.merchant}</span>
              <span className="hidden shrink-0 text-xs text-mist-500 sm:block">
                {t.account_name}
              </span>
              <span className="tabular w-24 shrink-0 text-right">
                {formatMoney(t.amount)}
              </span>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

function CreateTag() {
  const qc = useQueryClient()
  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [expected, setExpected] = useState('')

  const create = useMutation({
    mutationFn: () =>
      api.createTag({
        name: name.trim(),
        description: description.trim() || null,
        expected_amount: expected.trim() || null,
      }),
    onSuccess: () => {
      setName('')
      setDescription('')
      setExpected('')
      invalidateTags(qc)
    },
  })

  return (
    <section id="add-tag" className="glass scroll-mt-24 p-6">
      <h2 className="mb-1 text-lg font-medium">Add a tag</h2>
      <p className="mb-5 text-sm text-mist-300">
        Name it after the thing, not the category — the trip, the project, the
        person. An expected total is optional; set one to track it as an
        envelope.
      </p>

      <div className="grid gap-4 sm:grid-cols-3">
        <div>
          <label className="label" htmlFor="tag-name">
            Name
          </label>
          <input
            id="tag-name"
            className="field w-full"
            placeholder="Summer Vacation"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
        </div>
        <div>
          <label className="label" htmlFor="tag-description">
            Description (optional)
          </label>
          <input
            id="tag-description"
            className="field w-full"
            placeholder="Two weeks in Portugal"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
          />
        </div>
        <div>
          <label className="label" htmlFor="tag-expected">
            Expected total (optional)
          </label>
          <input
            id="tag-expected"
            className="field w-full"
            type="number"
            min="0"
            step="0.01"
            placeholder="No target"
            value={expected}
            onChange={(e) => setExpected(e.target.value)}
          />
        </div>
      </div>

      <div className="mt-5 flex items-center gap-3">
        <button
          className="btn-primary px-4 py-2 text-sm"
          disabled={name.trim() === '' || create.isPending}
          onClick={() => create.mutate()}
        >
          {create.isPending ? 'Saving…' : 'Add tag'}
        </button>
        {create.isError && (
          <span role="alert" className="text-sm text-ember-400">
            {create.error.message}
          </span>
        )}
      </div>
    </section>
  )
}

/** Outline glyph for the tags empty state. */
function TagGlyph() {
  return (
    <svg
      className="h-5 w-5"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={2}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d="M3 8V4a1 1 0 0 1 1-1h4l12 12-5 5L3 8Z" />
      <circle cx="7.5" cy="7.5" r="1.25" />
    </svg>
  )
}
