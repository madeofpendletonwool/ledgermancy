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

export function Categories() {
  const categories = useQuery({ queryKey: ['categories'], queryFn: api.categories })

  const rows = categories.data ?? []
  // Only spending categories are worth managing here; income/transfer are
  // system-level accounting concerns, not user budgeting categories.
  const custom = rows.filter((c) => !c.is_system && !c.is_income && !c.is_transfer)
  const system = rows.filter((c) => c.is_system && !c.is_income && !c.is_transfer)

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
              {c.is_fixed && (
                <span className="rounded border border-white/10 px-1.5 py-0.5 text-[10px] text-mist-500">
                  fixed
                </span>
              )}
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
  const [isFixed, setIsFixed] = useState(false)

  const create = useMutation({
    mutationFn: () => api.createCategory({ name: name.trim(), color, is_fixed: isFixed }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['categories'] })
      setName('')
      setColor(PALETTE[0])
      setIsFixed(false)
    },
  })

  const canAdd = name.trim() !== ''

  return (
    <section className="glass p-6">
      <h2 className="mb-1 text-lg font-medium">Add a category</h2>
      <p className="mb-5 text-sm text-mist-300">
        e.g. “Childcare”. Fixed categories count toward your fixed-cost total.
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
          <span className="label">Color</span>
          <Swatches value={color} onChange={setColor} />
        </div>

        <label className="flex items-center gap-2 pb-2 text-sm">
          <input
            type="checkbox"
            checked={isFixed}
            onChange={(e) => setIsFixed(e.target.checked)}
          />
          Fixed cost
        </label>

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
  const [isFixed, setIsFixed] = useState(category.is_fixed)
  const [confirmDelete, setConfirmDelete] = useState(false)

  const invalidate = () => qc.invalidateQueries({ queryKey: ['categories'] })

  const save = useMutation({
    mutationFn: () =>
      api.updateCategory(category.id, { name: name.trim(), color, is_fixed: isFixed }),
    onSuccess: () => {
      invalidate()
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
            <span className="label">Color</span>
            <Swatches value={color} onChange={setColor} />
          </div>
          <label className="flex items-center gap-2 pb-2 text-sm">
            <input
              type="checkbox"
              checked={isFixed}
              onChange={(e) => setIsFixed(e.target.checked)}
            />
            Fixed cost
          </label>
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
        {category.is_fixed && (
          <span className="rounded border border-white/10 px-1.5 py-0.5 text-[10px] text-mist-500">
            fixed
          </span>
        )}
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
