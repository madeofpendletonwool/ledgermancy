import { useState, type FormEvent } from 'react'
import { Link, useNavigate, useSearchParams } from 'react-router-dom'
import { AmbientGlyphs, Wordmark } from '../components/Brand'
import { useRegister } from '../lib/session'

const MIN_PASSWORD = 12

export function Register() {
  const navigate = useNavigate()
  const register = useRegister()
  const [params] = useSearchParams()

  // An invited spouse arrives at /register?invite=<token>. With a token we are
  // joining an existing household; without one we are creating the first.
  const inviteToken = params.get('invite') ?? ''
  const isJoining = inviteToken !== ''

  const [displayName, setDisplayName] = useState('')
  const [householdName, setHouseholdName] = useState('')
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')

  const tooShort = password.length > 0 && password.length < MIN_PASSWORD

  function onSubmit(e: FormEvent) {
    e.preventDefault()
    if (tooShort) return
    register.mutate(
      {
        email,
        password,
        display_name: displayName,
        ...(isJoining
          ? { invite_token: inviteToken }
          : { household_name: householdName || undefined }),
      },
      { onSuccess: () => navigate('/') },
    )
  }

  return (
    <div className="relative flex min-h-screen items-center justify-center px-4 py-12">
      <AmbientGlyphs />

      <div className="relative w-full max-w-md">
        <div className="mb-8 flex justify-center">
          <Wordmark size="lg" />
        </div>

        <form onSubmit={onSubmit} className="glass p-8">
          <h1 className="text-xl font-semibold">
            {isJoining ? 'Join the household' : 'Create your household'}
          </h1>
          <p className="mt-1 mb-6 text-sm text-mist-300">
            {isJoining
              ? 'You were invited. Set up your own sign-in below.'
              : 'The first account creates the household. Everyone after joins by invitation.'}
          </p>

          <div className="space-y-4">
            <div>
              <label className="label" htmlFor="displayName">
                Your name
              </label>
              <input
                id="displayName"
                className="field"
                required
                value={displayName}
                onChange={(e) => setDisplayName(e.target.value)}
              />
            </div>

            {!isJoining && (
              <div>
                <label className="label" htmlFor="householdName">
                  Household name{' '}
                  <span className="font-normal text-mist-500">(optional)</span>
                </label>
                <input
                  id="householdName"
                  className="field"
                  placeholder="The Pendleton Household"
                  value={householdName}
                  onChange={(e) => setHouseholdName(e.target.value)}
                />
              </div>
            )}

            <div>
              <label className="label" htmlFor="email">
                Email
              </label>
              <input
                id="email"
                type="email"
                className="field"
                autoComplete="username"
                required
                value={email}
                onChange={(e) => setEmail(e.target.value)}
              />
            </div>

            <div>
              <label className="label" htmlFor="password">
                Password
              </label>
              <input
                id="password"
                type="password"
                className="field"
                autoComplete="new-password"
                required
                minLength={MIN_PASSWORD}
                value={password}
                onChange={(e) => setPassword(e.target.value)}
              />
              <p
                className={`mt-1.5 text-xs ${tooShort ? 'text-ember-400' : 'text-mist-500'}`}
              >
                At least {MIN_PASSWORD} characters. Length beats symbols.
              </p>
            </div>
          </div>

          {register.isError && (
            <p
              role="alert"
              className="mt-4 rounded-xl border border-ember-400/30 bg-ember-400/10 px-4 py-2.5 text-sm text-ember-400"
            >
              {register.error.message}
            </p>
          )}

          <button
            type="submit"
            className="btn-primary mt-6 w-full"
            disabled={register.isPending || tooShort}
          >
            {register.isPending ? 'Inscribing…' : 'Create account'}
          </button>

          <p className="mt-6 text-center text-sm text-mist-500">
            Already have an account?{' '}
            <Link to="/login" className="text-arcane-400 hover:text-arcane-500">
              Sign in
            </Link>
          </p>
        </form>
      </div>
    </div>
  )
}
