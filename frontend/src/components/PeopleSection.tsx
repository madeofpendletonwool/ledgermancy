import { useState, type FormEvent } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  api,
  type Allowance,
  type Person,
  type Role,
  type CreatedInvite,
} from '../lib/api'
import { formatMoney } from '../lib/money'
import { SkeletonRows } from '../components/Skeleton'

/**
 * People: everyone the household's money can be about, whether or not they can
 * sign in.
 *
 * The distinction this UI has to make legible is that a PERSON is not a LOGIN.
 * A six-year-old with a 529 is a person with no login, and the copy says so
 * rather than leaving "no account" reading like something is broken.
 */
export function PeopleSection() {
  const qc = useQueryClient()
  const people = useQuery({ queryKey: ['people'], queryFn: api.people })
  const members = useQuery({ queryKey: ['members'], queryFn: api.members })

  const [editing, setEditing] = useState<Person | null>(null)
  const [adding, setAdding] = useState(false)
  const [allowanceFor, setAllowanceFor] = useState<Person | null>(null)
  const [loginFor, setLoginFor] = useState<Person | null>(null)

  const invalidate = () => {
    qc.invalidateQueries({ queryKey: ['people'] })
    qc.invalidateQueries({ queryKey: ['members'] })
  }

  const remove = useMutation({
    mutationFn: api.deletePerson,
    onSuccess: invalidate,
  })

  const setRole = useMutation({
    mutationFn: ({ userId, role }: { userId: string; role: Role }) =>
      api.setMemberRole(userId, role),
    onSuccess: invalidate,
  })

  const adults = people.data?.filter((p) => !p.is_dependent) ?? []
  const dependents = people.data?.filter((p) => p.is_dependent) ?? []

  return (
    <section className="glass p-6">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 className="text-lg font-medium">People</h2>
          <p className="mt-1 text-sm text-mist-300">
            Everyone this household's money is about. A child does not need a
            login to have a 529, savings bonds, or a goal — add them here first,
            then give them an account later if you want to.
          </p>
        </div>
        <button className="btn-ghost shrink-0 text-sm" onClick={() => setAdding(true)}>
          Add person
        </button>
      </div>

      {people.isPending && <SkeletonRows count={2} />}

      {!!adults.length && (
        <PersonGroup
          title="Adults"
          people={adults}
          onEdit={setEditing}
          onAllowance={setAllowanceFor}
          onEnableLogin={setLoginFor}
          onDelete={(p) => remove.mutate(p.id)}
          onRoleChange={(userId, role) => setRole.mutate({ userId, role })}
          roleBusy={setRole.isPending}
        />
      )}

      {!!dependents.length && (
        <PersonGroup
          title="Children & dependents"
          people={dependents}
          onEdit={setEditing}
          onAllowance={setAllowanceFor}
          onEnableLogin={setLoginFor}
          onDelete={(p) => remove.mutate(p.id)}
          onRoleChange={(userId, role) => setRole.mutate({ userId, role })}
          roleBusy={setRole.isPending}
        />
      )}

      {remove.isError && (
        <p role="alert" className="mt-4 text-sm text-ember-400">
          {remove.error.message}
        </p>
      )}
      {setRole.isError && (
        <p role="alert" className="mt-4 text-sm text-ember-400">
          {setRole.error.message}
        </p>
      )}

      {(adding || editing) && (
        <PersonDialog
          person={editing}
          onClose={() => {
            setAdding(false)
            setEditing(null)
          }}
          onSaved={invalidate}
        />
      )}

      {allowanceFor && (
        <AllowanceDialog person={allowanceFor} onClose={() => setAllowanceFor(null)} />
      )}

      {loginFor && (
        <EnableLoginDialog
          person={loginFor}
          onClose={() => setLoginFor(null)}
          onIssued={invalidate}
        />
      )}

      {members.data && members.data.length > 0 && (
        <p className="mt-6 text-xs text-mist-500">
          {members.data.length} of {people.data?.length ?? 0} people can sign in.
        </p>
      )}
    </section>
  )
}

function PersonGroup({
  title,
  people,
  onEdit,
  onAllowance,
  onEnableLogin,
  onDelete,
  onRoleChange,
  roleBusy,
}: {
  title: string
  people: Person[]
  onEdit: (p: Person) => void
  onAllowance: (p: Person) => void
  onEnableLogin: (p: Person) => void
  onDelete: (p: Person) => void
  onRoleChange: (userId: string, role: Role) => void
  roleBusy: boolean
}) {
  return (
    <div className="mt-6">
      <h3 className="text-sm font-medium text-mist-300">{title}</h3>
      <ul className="mt-2 divide-y divide-white/5">
        {people.map((p) => (
          <li key={p.id} className="flex flex-wrap items-center gap-3 py-3">
            <span className="flex h-9 w-9 shrink-0 items-center justify-center rounded-full bg-arcane-500/20 text-sm font-medium text-arcane-400">
              {p.display_name.charAt(0).toUpperCase()}
            </span>

            <div className="min-w-0 flex-1">
              <p className="truncate font-medium">{p.display_name}</p>
              <p className="truncate text-sm text-mist-500">
                {p.age !== null ? `${p.age} years old` : 'No birthdate set'}
                {p.email && ` · ${p.email}`}
                {!p.has_login && ' · no login'}
              </p>
            </div>

            {p.user_id && p.role && (
              <select
                className="field w-auto py-1 text-xs"
                value={p.role}
                disabled={roleBusy}
                onChange={(e) => onRoleChange(p.user_id!, e.target.value as Role)}
              >
                <option value="owner">Owner</option>
                <option value="member">Member</option>
                <option value="child">Child</option>
              </select>
            )}

            <div className="flex shrink-0 gap-3 text-xs">
              <button className="text-mist-300 hover:underline" onClick={() => onEdit(p)}>
                Edit
              </button>
              <button
                className="text-mist-300 hover:underline"
                onClick={() => onAllowance(p)}
              >
                Allowance
              </button>
              {!p.has_login && (
                <button
                  className="text-rune-300 hover:underline"
                  onClick={() => onEnableLogin(p)}
                >
                  Enable login
                </button>
              )}
              {!p.has_login && (
                <button
                  className="text-ember-400 hover:underline"
                  onClick={() => onDelete(p)}
                >
                  Remove
                </button>
              )}
            </div>
          </li>
        ))}
      </ul>
    </div>
  )
}

function Dialog({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-ink-950/70 p-4">
      <div className="glass w-full max-w-md p-6">
        <h3 className="text-lg font-medium">{title}</h3>
        {children}
      </div>
    </div>
  )
}

function PersonDialog({
  person,
  onClose,
  onSaved,
}: {
  person: Person | null
  onClose: () => void
  onSaved: () => void
}) {
  const [name, setName] = useState(person?.display_name ?? '')
  const [birthdate, setBirthdate] = useState(person?.birthdate ?? '')
  const [dependent, setDependent] = useState(person?.is_dependent ?? true)

  const save = useMutation({
    mutationFn: () => {
      const input = {
        display_name: name,
        birthdate: birthdate || null,
        is_dependent: dependent,
      }
      return person ? api.updatePerson(person.id, input) : api.createPerson(input)
    },
    onSuccess: () => {
      onSaved()
      onClose()
    },
  })

  function onSubmit(e: FormEvent) {
    e.preventDefault()
    save.mutate()
  }

  return (
    <Dialog title={person ? `Edit ${person.display_name}` : 'Add a person'}>
      <form onSubmit={onSubmit} className="mt-4 space-y-4">
        <label className="block">
          <span className="text-sm text-mist-300">Name</span>
          <input
            required
            className="field mt-1"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
        </label>

        <label className="block">
          <span className="text-sm text-mist-300">Birthdate</span>
          <input
            type="date"
            className="field mt-1"
            value={birthdate}
            onChange={(e) => setBirthdate(e.target.value)}
          />
          {/* The reason to bother. A stored age is right the day you type it and
              wrong every year after; a birthdate stays correct by itself. */}
          <span className="mt-1 block text-xs text-mist-500">
            Optional, but it keeps every age-based projection correct on its own —
            a 529's college horizon, catch-up contribution limits, and "at 67"
            instead of "in 30 years".
          </span>
        </label>

        <label className="flex items-center gap-2">
          <input
            type="checkbox"
            checked={dependent}
            onChange={(e) => setDependent(e.target.checked)}
          />
          <span className="text-sm text-mist-300">
            A dependent — groups them separately and keeps their custodial
            accounts out of the household's retirement total
          </span>
        </label>

        {save.isError && (
          <p role="alert" className="text-sm text-ember-400">
            {save.error.message}
          </p>
        )}

        <div className="flex justify-end gap-3">
          <button type="button" className="btn-ghost" onClick={onClose}>
            Cancel
          </button>
          <button type="submit" className="btn-primary" disabled={save.isPending}>
            {save.isPending ? 'Saving…' : 'Save'}
          </button>
        </div>
      </form>
    </Dialog>
  )
}

function AllowanceDialog({ person, onClose }: { person: Person; onClose: () => void }) {
  const qc = useQueryClient()
  const allowance = useQuery({
    queryKey: ['allowance', person.id],
    queryFn: () => api.allowance(person.id),
  })
  const entries = useQuery({
    queryKey: ['allowance-entries', person.id],
    queryFn: () => api.allowanceEntries(person.id),
  })

  return (
    <Dialog title={`${person.display_name}'s allowance`}>
      <div className="mt-4 max-h-[70vh] space-y-6 overflow-y-auto">
        {allowance.data && (
          <>
            <AllowanceBalance allowance={allowance.data} />
            <AllowanceScheduleForm
              personId={person.id}
              allowance={allowance.data}
              onSaved={() =>
                qc.invalidateQueries({ queryKey: ['allowance', person.id] })
              }
            />
            <AddEntryForm
              personId={person.id}
              onAdded={() => {
                qc.invalidateQueries({ queryKey: ['allowance', person.id] })
                qc.invalidateQueries({ queryKey: ['allowance-entries', person.id] })
              }}
            />
          </>
        )}

        {!!entries.data?.length && (
          <div>
            <h4 className="text-sm font-medium text-mist-300">History</h4>
            <ul className="mt-2 divide-y divide-white/5 text-sm">
              {entries.data.map((e) => (
                <li key={e.id} className="flex items-center gap-3 py-2">
                  <span className="capitalize text-mist-300">{e.kind}</span>
                  <span className="text-xs text-mist-500">{e.occurred_on}</span>
                  <span
                    className={`ml-auto tabular ${
                      e.amount.startsWith('-') ? 'text-ember-400' : 'text-rune-300'
                    }`}
                  >
                    {formatMoney(e.amount)}
                  </span>
                </li>
              ))}
            </ul>
          </div>
        )}
      </div>

      <div className="mt-6 flex justify-end">
        <button className="btn-ghost" onClick={onClose}>
          Done
        </button>
      </div>
    </Dialog>
  )
}

export function AllowanceBalance({ allowance }: { allowance: Allowance }) {
  return (
    <div className="rounded-xl border border-rune-400/25 bg-rune-400/5 p-4">
      <p className="text-xs uppercase tracking-wide text-mist-500">Balance</p>
      <p className="mt-1 text-2xl font-semibold tabular">
        {formatMoney(allowance.balance)}
      </p>
      <p className="mt-2 text-xs text-mist-500">
        Spent this month: {formatMoney(allowance.spent_this_month)}
        {allowance.limit_remaining !== null &&
          ` · ${formatMoney(allowance.limit_remaining)} left of the limit`}
      </p>
      {/* This is a record, not a bank balance, and the UI must not let a child
          think otherwise. */}
      <p className="mt-2 text-xs text-mist-500">
        A record kept here, not a bank account.
      </p>
    </div>
  )
}

function AllowanceScheduleForm({
  personId,
  allowance,
  onSaved,
}: {
  personId: string
  allowance: Allowance
  onSaved: () => void
}) {
  const [amount, setAmount] = useState(allowance.amount ?? '')
  const [cadence, setCadence] = useState(allowance.cadence ?? '')
  const [limit, setLimit] = useState(allowance.monthly_limit ?? '')
  const [autoPost, setAutoPost] = useState(allowance.auto_post)

  const save = useMutation({
    mutationFn: () =>
      api.saveAllowance(personId, {
        amount: amount || null,
        cadence: cadence || null,
        monthly_limit: limit || null,
        auto_post: autoPost,
      }),
    onSuccess: onSaved,
  })

  return (
    <form
      className="space-y-3"
      onSubmit={(e) => {
        e.preventDefault()
        save.mutate()
      }}
    >
      <h4 className="text-sm font-medium text-mist-300">Schedule</h4>
      <div className="flex gap-3">
        <input
          className="field flex-1"
          placeholder="Amount"
          inputMode="decimal"
          value={amount}
          onChange={(e) => setAmount(e.target.value)}
        />
        <select
          className="field flex-1"
          value={cadence}
          onChange={(e) => setCadence(e.target.value)}
        >
          <option value="">No schedule</option>
          <option value="weekly">Weekly</option>
          <option value="biweekly">Every 2 weeks</option>
          <option value="monthly">Monthly</option>
        </select>
      </div>

      <input
        className="field"
        placeholder="Monthly spending limit (optional)"
        inputMode="decimal"
        value={limit}
        onChange={(e) => setLimit(e.target.value)}
      />

      <label className="flex items-center gap-2">
        <input
          type="checkbox"
          checked={autoPost}
          onChange={(e) => setAutoPost(e.target.checked)}
        />
        <span className="text-sm text-mist-300">
          Pay it automatically — otherwise you record each one yourself
        </span>
      </label>

      {save.isError && (
        <p role="alert" className="text-sm text-ember-400">
          {save.error.message}
        </p>
      )}

      <button className="btn-primary w-full" disabled={save.isPending}>
        {save.isPending ? 'Saving…' : 'Save schedule'}
      </button>
    </form>
  )
}

function AddEntryForm({
  personId,
  onAdded,
}: {
  personId: string
  onAdded: () => void
}) {
  const [kind, setKind] = useState('allowance')
  const [amount, setAmount] = useState('')
  const [note, setNote] = useState('')

  const add = useMutation({
    mutationFn: () =>
      api.addAllowanceEntry(personId, {
        kind: kind as never,
        amount,
        note: note || null,
      }),
    onSuccess: () => {
      setAmount('')
      setNote('')
      onAdded()
    },
  })

  return (
    <form
      className="space-y-3"
      onSubmit={(e) => {
        e.preventDefault()
        add.mutate()
      }}
    >
      <h4 className="text-sm font-medium text-mist-300">Record something</h4>
      <div className="flex gap-3">
        <select
          className="field flex-1"
          value={kind}
          onChange={(e) => setKind(e.target.value)}
        >
          <option value="allowance">Allowance</option>
          <option value="chore">Chore</option>
          <option value="gift">Gift</option>
          <option value="spend">Spent</option>
          <option value="correction">Correction</option>
        </select>
        <input
          required
          className="field flex-1"
          placeholder="Amount"
          inputMode="decimal"
          value={amount}
          onChange={(e) => setAmount(e.target.value)}
        />
      </div>
      <input
        className="field"
        placeholder="Note (optional)"
        value={note}
        onChange={(e) => setNote(e.target.value)}
      />

      {add.isError && (
        <p role="alert" className="text-sm text-ember-400">
          {add.error.message}
        </p>
      )}

      <button className="btn-ghost w-full" disabled={add.isPending}>
        {add.isPending ? 'Recording…' : 'Record'}
      </button>
    </form>
  )
}

/**
 * Issues an invite bound to an existing person, so accepting it attaches the
 * new login to them rather than creating a second, empty copy.
 */
function EnableLoginDialog({
  person,
  onClose,
  onIssued,
}: {
  person: Person
  onClose: () => void
  onIssued: () => void
}) {
  const [email, setEmail] = useState('')
  const [role, setRole] = useState<Role>(person.is_dependent ? 'child' : 'member')
  const [issued, setIssued] = useState<CreatedInvite | null>(null)

  const create = useMutation({
    mutationFn: () => api.createInvite({ email, role, person_id: person.id }),
    onSuccess: (invite) => {
      setIssued(invite)
      onIssued()
    },
  })

  const link = issued
    ? `${window.location.origin}/register?invite=${encodeURIComponent(issued.token)}`
    : null

  return (
    <Dialog title={`Give ${person.display_name} a login`}>
      {!issued ? (
        <form
          className="mt-4 space-y-4"
          onSubmit={(e) => {
            e.preventDefault()
            create.mutate()
          }}
        >
          <label className="block">
            <span className="text-sm text-mist-300">Email</span>
            <input
              type="email"
              required
              className="field mt-1"
              placeholder="you+ellie@gmail.com"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
            />
            {/* A synthetic address would break password reset and every
                security notification silently. Say so here rather than letting
                someone discover it at the worst moment. */}
            <span className="mt-1 block text-xs text-mist-500">
              This has to be a real address that receives mail — it is where
              password resets and security alerts go. For a young child, use a
              plus-address on your own inbox, like you+ellie@gmail.com.
            </span>
          </label>

          <label className="block">
            <span className="text-sm text-mist-300">Access level</span>
            <select
              className="field mt-1"
              value={role}
              onChange={(e) => setRole(e.target.value as Role)}
            >
              <option value="child">
                Child — their allowance, their goals, their accounts only
              </option>
              <option value="member">Member — full access to the household</option>
            </select>
          </label>

          {create.isError && (
            <p role="alert" className="text-sm text-ember-400">
              {create.error.message}
            </p>
          )}

          <div className="flex justify-end gap-3">
            <button type="button" className="btn-ghost" onClick={onClose}>
              Cancel
            </button>
            <button className="btn-primary" disabled={create.isPending}>
              {create.isPending ? 'Sealing…' : 'Create invite'}
            </button>
          </div>
        </form>
      ) : (
        <div className="mt-4 space-y-4">
          <p className="text-sm text-mist-300">
            Send this to {issued.email}. It is shown once and cannot be retrieved
            later.
          </p>
          <code className="block truncate rounded-lg bg-ink-950/60 px-3 py-2 text-xs text-mist-300">
            {link}
          </code>
          <div className="flex justify-end gap-3">
            <button
              className="btn-ghost"
              onClick={() => link && navigator.clipboard?.writeText(link)}
            >
              Copy
            </button>
            <button className="btn-primary" onClick={onClose}>
              Done
            </button>
          </div>
        </div>
      )}
    </Dialog>
  )
}
