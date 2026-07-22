import { useState, type FormEvent } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, type CreatedInvite } from '../lib/api'

export function Household() {
  const qc = useQueryClient()
  const household = useQuery({ queryKey: ['household'], queryFn: api.household })
  const members = useQuery({ queryKey: ['members'], queryFn: api.members })
  const invites = useQuery({ queryKey: ['invites'], queryFn: api.invites })

  const [email, setEmail] = useState('')
  // The invite token comes back exactly once, so it is held here to be copied.
  // It is deliberately not refetched or cached anywhere else.
  const [issued, setIssued] = useState<CreatedInvite | null>(null)

  const createInvite = useMutation({
    mutationFn: api.createInvite,
    onSuccess: (invite) => {
      setIssued(invite)
      setEmail('')
      qc.invalidateQueries({ queryKey: ['invites'] })
    },
  })

  const revoke = useMutation({
    mutationFn: api.deleteInvite,
    onSuccess: () => qc.invalidateQueries({ queryKey: ['invites'] }),
  })

  function onInvite(e: FormEvent) {
    e.preventDefault()
    createInvite.mutate(email)
  }

  const inviteLink = issued
    ? `${window.location.origin}/register?invite=${encodeURIComponent(issued.token)}`
    : null

  return (
    <div className="space-y-8">
      <div>
        <h1 className="text-2xl font-semibold">{household.data?.name ?? 'Household'}</h1>
        <p className="mt-1 text-mist-300">
          Everyone here shares the household view of your finances.
        </p>
      </div>

      <section className="glass p-6">
        <h2 className="text-lg font-medium">Members</h2>
        <ul className="mt-4 divide-y divide-white/5">
          {members.isPending && <li className="py-3 text-sm text-mist-500">Loading…</li>}
          {members.data?.map((m) => (
            <li key={m.id} className="flex items-center gap-4 py-3">
              <span className="flex h-9 w-9 items-center justify-center rounded-full bg-arcane-500/20 text-sm font-medium text-arcane-400">
                {m.display_name.charAt(0).toUpperCase()}
              </span>
              <div className="min-w-0">
                <p className="truncate font-medium">{m.display_name}</p>
                <p className="truncate text-sm text-mist-500">{m.email}</p>
              </div>
              <span className="ml-auto text-xs text-mist-500">
                joined {new Date(m.created_at).toLocaleDateString()}
              </span>
            </li>
          ))}
        </ul>
      </section>

      <section className="glass p-6">
        <h2 className="text-lg font-medium">Invite someone</h2>
        <p className="mt-1 text-sm text-mist-300">
          Registration is invite-only, so this is the only way to add a member.
        </p>

        <form onSubmit={onInvite} className="mt-4 flex flex-col gap-3 sm:flex-row">
          <input
            type="email"
            required
            className="field sm:flex-1"
            placeholder="spouse@example.com"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
          />
          <button type="submit" className="btn-primary" disabled={createInvite.isPending}>
            {createInvite.isPending ? 'Sealing…' : 'Create invite'}
          </button>
        </form>

        {createInvite.isError && (
          <p
            role="alert"
            className="mt-4 rounded-xl border border-ember-400/30 bg-ember-400/10 px-4 py-2.5 text-sm text-ember-400"
          >
            {createInvite.error.message}
          </p>
        )}

        {inviteLink && (
          <div className="mt-4 rounded-xl border border-rune-400/25 bg-rune-400/5 p-4">
            <p className="text-sm font-medium text-rune-300">
              Invite link for {issued?.email}
            </p>
            <p className="mt-1 text-xs text-mist-300">
              Copy this now — it is shown once and cannot be retrieved later.
            </p>
            <div className="mt-3 flex flex-col gap-2 sm:flex-row">
              <code className="min-w-0 flex-1 truncate rounded-lg bg-ink-950/60 px-3 py-2 text-xs text-mist-300">
                {inviteLink}
              </code>
              <button
                type="button"
                className="btn-ghost shrink-0 px-3 py-2 text-sm"
                onClick={() => navigator.clipboard?.writeText(inviteLink)}
              >
                Copy
              </button>
            </div>
          </div>
        )}

        {!!invites.data?.length && (
          <div className="mt-6">
            <h3 className="text-sm font-medium text-mist-300">Pending invites</h3>
            <ul className="mt-2 divide-y divide-white/5">
              {invites.data.map((inv) => (
                <li key={inv.id} className="flex items-center gap-4 py-2.5 text-sm">
                  <span className="truncate">{inv.email}</span>
                  <span className="ml-auto shrink-0 text-xs text-mist-500">
                    expires {new Date(inv.expires_at).toLocaleDateString()}
                  </span>
                  <button
                    className="shrink-0 text-xs text-ember-400 hover:underline disabled:opacity-50"
                    disabled={revoke.isPending}
                    onClick={() => revoke.mutate(inv.id)}
                  >
                    Revoke
                  </button>
                </li>
              ))}
            </ul>
          </div>
        )}
      </section>
    </div>
  )
}
