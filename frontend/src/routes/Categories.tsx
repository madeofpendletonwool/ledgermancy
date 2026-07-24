import { useState } from 'react'
import type { ReactNode } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../lib/api'
import type { Category } from '../lib/api'

// A small preset palette for category chips. Hex strings map straight to the
// categories.color column; a null color falls back to a neutral dot.
const PALETTE = [
  '#9085e9', '#3987e5', '#199e70', '#d95926',
  '#e5a13a', '#c94f8f', '#6bbf59', '#4bb6c9',
]

// How a category counts. A transfer (card payment, move to savings) is money
// between your own accounts and is excluded from spending entirely — the fix
// for a category that was wrongly inflating your spend.
type CatType = 'spending' | 'income' | 'transfer'

const catType = (c: { is_income: boolean; is_transfer: boolean }): CatType =>
  c.is_income ? 'income' : c.is_transfer ? 'transfer' : 'spending'

const typeFlags = (t: CatType) => ({
  is_income: t === 'income',
  is_transfer: t === 'transfer',
})

// A dropdown for choosing how a category counts. Shared by add + edit.
function TypeSelect({
  value,
  onChange,
  id,
}: {
  value: CatType
  onChange: (t: CatType) => void
  id?: string
}) {
  return (
    <select
      id={id}
      className="field"
      value={value}
      onChange={(e) => onChange(e.target.value as CatType)}
    >
      <option value="spending">Spending</option>
      <option value="income">Income</option>
      <option value="transfer">Transfer (not spending)</option>
    </select>
  )
}

// A small pill describing a non-spending type or a fixed cost, for list rows.
function TypeBadge({ category }: { category: Category }) {
  const t = catType(category)
  if (t === 'income')
    return <Pill className="border-rune-500/40 text-rune-300">income</Pill>
  if (t === 'transfer')
    return <Pill className="border-arcane-500/40 text-arcane-300">transfer</Pill>
  if (category.is_fixed)
    return <Pill className="border-white/10 text-mist-500">fixed</Pill>
  return null
}

function Pill({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <span className={`rounded border px-1.5 py-0.5 text-[10px] ${className ?? ''}`}>
      {children}
    </span>
  )
}

export function Categories() {
  const categories = useQuery({ queryKey: ['categories'], queryFn: api.categories })

  const rows = categories.data ?? []
  // All of the household's own categories are managed here, whatever their type
  // — a custom transfer/income category needs to be visible so it can be fixed
  // or removed. System defaults are shown (read-only) below so their built-in
  // transfer categories are discoverable rather than accidentally duplicated.
  const custom = rows.filter((c) => !c.is_system)
  const system = rows.filter((c) => c.is_system)

  return (
    <div className="space-y-8">
      <div>
        <h1 className="text-2xl font-semibold">Categories</h1>
        <p className="mt-1 text-mist-300">
          Make your own categories, then set a charge’s category on the
          Transactions page — “apply to all from this merchant” makes it stick.
        </p>
      </div>

      <CreateCategory />

      <section className="glass p-6">
        <h2 className="mb-1 text-lg font-medium">Your categories</h2>
        <p className="mb-5 text-sm text-mist-300">
          Custom categories you can rename, recolor, or remove.
        </p>
        {categories.isPending ? (
          <Loading />
        ) : custom.length === 0 ? (
          <Empty>No custom categories yet. Add one above.</Empty>
        ) : (
          <div className="space-y-3">
            {custom.map((c) => (
              <CategoryRow key={c.id} category={c} />
            ))}
          </div>
        )}
      </section>

      <section className="glass p-6">
        <h2 className="mb-1 text-lg font-medium">Built-in categories</h2>
        <p className="mb-5 text-sm text-mist-300">
          Defaults that ship with the app. These can’t be edited, but you can
          recategorize any charge into your own.
        </p>
        <div className="flex flex-wrap gap-2">
          {system.map((c) => (
            <span
              key={c.id}
              className="flex items-center gap-2 rounded-full border border-white/10 px-3 py-1 text-sm text-mist-300"
            >
              <Dot color={c.color} />
              {c.name}
              <TypeBadge category={c} />
            </span>
          ))}
        </div>
      </section>
    </div>
  )
}

function CreateCategory() {
  const qc = useQueryClient()
  const [name, setName] = useState('')
  const [color, setColor] = useState<string>(PALETTE[0])
  const [type, setType] = useState<CatType>('spending')
  const [isFixed, setIsFixed] = useState(false)

  const create = useMutation({
    mutationFn: () =>
      api.createCategory({
        name: name.trim(),
        color,
        is_fixed: type === 'spending' && isFixed,
        ...typeFlags(type),
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['categories'] })
      setName('')
      setColor(PALETTE[0])
      setType('spending')
      setIsFixed(false)
    },
  })

  const canAdd = name.trim() !== ''

  return (
    <section className="glass p-6">
      <h2 className="mb-1 text-lg font-medium">Add a category</h2>
      <p className="mb-5 text-sm text-mist-300">
        e.g. “Childcare”. Set <em>Transfer</em> for money moving between your own
        accounts (a card payment, a transfer to savings) so it never counts as
        spending; <em>Income</em> for money coming in.
      </p>

      <div className="flex flex-wrap items-end gap-4">
        <div>
          <label className="label" htmlFor="cat-name">
            Name
          </label>
          <input
            id="cat-name"
            className="field w-56"
            placeholder="Childcare"
            maxLength={60}
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
        </div>

        <div>
          <label className="label" htmlFor="cat-type">
            Type
          </label>
          <TypeSelect id="cat-type" value={type} onChange={setType} />
        </div>

        <div>
          <span className="label">Color</span>
          <Swatches value={color} onChange={setColor} />
        </div>

        {type === 'spending' && (
          <label className="flex items-center gap-2 pb-2 text-sm">
            <input
              type="checkbox"
              checked={isFixed}
              onChange={(e) => setIsFixed(e.target.checked)}
            />
            Fixed cost
          </label>
        )}

        <button
          className="btn-primary px-4 py-2 text-sm"
          disabled={!canAdd || create.isPending}
          onClick={() => create.mutate()}
        >
          {create.isPending ? 'Adding…' : 'Add category'}
        </button>
      </div>

      {create.isError && (
        <p role="alert" className="mt-3 text-sm text-ember-400">
          {create.error.message}
        </p>
      )}
    </section>
  )
}

function CategoryRow({ category }: { category: Category }) {
  const qc = useQueryClient()
  const [editing, setEditing] = useState(false)
  const [name, setName] = useState(category.name)
  const [color, setColor] = useState<string>(category.color ?? PALETTE[0])
  const [type, setType] = useState<CatType>(catType(category))
  const [isFixed, setIsFixed] = useState(category.is_fixed)
  const [confirmDelete, setConfirmDelete] = useState(false)

  const invalidate = () => qc.invalidateQueries({ queryKey: ['categories'] })

  const save = useMutation({
    mutationFn: () =>
      api.updateCategory(category.id, {
        name: name.trim(),
        color,
        is_fixed: type === 'spending' && isFixed,
        ...typeFlags(type),
      }),
    onSuccess: () => {
      // Changing a category's type re-classifies every transaction already on
      // it, so spending/income figures refresh app-wide — invalidate broadly.
      qc.invalidateQueries()
      setEditing(false)
    },
  })
  const remove = useMutation({
    mutationFn: () => api.deleteCategory(category.id),
    onSuccess: invalidate,
  })

  if (editing) {
    return (
      <div className="rounded-xl border border-white/5 p-4">
        <div className="flex flex-wrap items-end gap-4">
          <div>
            <label className="label" htmlFor={`edit-${category.id}`}>
              Name
            </label>
            <input
              id={`edit-${category.id}`}
              className="field w-56"
              maxLength={60}
              value={name}
              onChange={(e) => setName(e.target.value)}
            />
          </div>
          <div>
            <label className="label" htmlFor={`edit-type-${category.id}`}>
              Type
            </label>
            <TypeSelect id={`edit-type-${category.id}`} value={type} onChange={setType} />
          </div>
          <div>
            <span className="label">Color</span>
            <Swatches value={color} onChange={setColor} />
          </div>
          {type === 'spending' && (
            <label className="flex items-center gap-2 pb-2 text-sm">
              <input
                type="checkbox"
                checked={isFixed}
                onChange={(e) => setIsFixed(e.target.checked)}
              />
              Fixed cost
            </label>
          )}
          <button
            className="btn-ghost px-3 py-1.5 text-sm"
            disabled={save.isPending || name.trim() === ''}
            onClick={() => save.mutate()}
          >
            Save
          </button>
          <button
            className="btn-ghost px-3 py-1.5 text-sm text-mist-300"
            onClick={() => {
              setName(category.name)
              setColor(category.color ?? PALETTE[0])
              setType(catType(category))
              setIsFixed(category.is_fixed)
              setEditing(false)
            }}
          >
            Cancel
          </button>
        </div>
        {save.isError && (
          <p role="alert" className="mt-2 text-sm text-ember-400">
            {save.error.message}
          </p>
        )}
      </div>
    )
  }

  return (
    <div className="flex flex-wrap items-center justify-between gap-3 rounded-xl border border-white/5 p-4">
      <span className="flex items-center gap-2 font-medium">
        <Dot color={category.color} />
        {category.name}
        <TypeBadge category={category} />
      </span>

      <div className="flex items-center gap-2">
        {confirmDelete ? (
          <>
            <span className="text-sm text-mist-300">
              Delete? Its charges become uncategorised.
            </span>
            <button
              className="btn-ghost px-3 py-1.5 text-sm text-ember-400"
              disabled={remove.isPending}
              onClick={() => remove.mutate()}
            >
              Confirm
            </button>
            <button
              className="btn-ghost px-3 py-1.5 text-sm text-mist-300"
              onClick={() => setConfirmDelete(false)}
            >
              Cancel
            </button>
          </>
        ) : (
          <>
            <button
              className="btn-ghost px-3 py-1.5 text-sm"
              onClick={() => setEditing(true)}
            >
              Edit
            </button>
            <button
              className="btn-ghost px-3 py-1.5 text-sm text-ember-400"
              onClick={() => setConfirmDelete(true)}
            >
              Delete
            </button>
          </>
        )}
      </div>
    </div>
  )
}

function Swatches({ value, onChange }: { value: string; onChange: (c: string) => void }) {
  return (
    <div className="flex items-center gap-1.5 pb-1">
      {PALETTE.map((c) => (
        <button
          key={c}
          type="button"
          aria-label={`Use color ${c}`}
          onClick={() => onChange(c)}
          className={`h-6 w-6 rounded-full transition ${
            value === c ? 'ring-2 ring-white ring-offset-2 ring-offset-ink-900' : ''
          }`}
          style={{ backgroundColor: c }}
        />
      ))}
    </div>
  )
}

function Dot({ color }: { color: string | null }) {
  return (
    <span
      className="inline-block h-2.5 w-2.5 shrink-0 rounded-full"
      style={{ backgroundColor: color ?? '#7b749c' }}
    />
  )
}

function Loading() {
  return <p className="py-8 text-center text-sm text-mist-500">Loading…</p>
}

function Empty({ children }: { children: ReactNode }) {
  return <p className="py-8 text-center text-sm text-mist-500">{children}</p>
}
