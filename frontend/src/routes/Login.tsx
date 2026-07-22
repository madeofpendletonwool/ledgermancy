import { useState, type FormEvent } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { AmbientGlyphs, Wordmark } from '../components/Brand'
import { useLogin } from '../lib/session'

export function Login() {
  const navigate = useNavigate()
  const login = useLogin()
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')

  function onSubmit(e: FormEvent) {
    e.preventDefault()
    login.mutate({ email, password }, { onSuccess: () => navigate('/') })
  }

  return (
    <div className="relative flex min-h-screen items-center justify-center px-4 py-12">
      <AmbientGlyphs />

      <div className="relative w-full max-w-md">
        <div className="mb-8 flex justify-center">
          <Wordmark size="lg" />
        </div>

        <form onSubmit={onSubmit} className="glass p-8">
          <h1 className="text-xl font-semibold">Welcome back</h1>
          <p className="mt-1 mb-6 text-sm text-mist-300">
            Sign in to consult the ledger.
          </p>

          <div className="space-y-4">
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
                autoComplete="current-password"
                required
                value={password}
                onChange={(e) => setPassword(e.target.value)}
              />
            </div>
          </div>

          {login.isError && (
            <p
              role="alert"
              className="mt-4 rounded-xl border border-ember-400/30 bg-ember-400/10 px-4 py-2.5 text-sm text-ember-400"
            >
              {login.error.message}
            </p>
          )}

          <button type="submit" className="btn-primary mt-6 w-full" disabled={login.isPending}>
            {login.isPending ? 'Consulting…' : 'Sign in'}
          </button>

          <p className="mt-6 text-center text-sm text-mist-500">
            Setting this up for the first time?{' '}
            <Link to="/register" className="text-arcane-400 hover:text-arcane-500">
              Create your household
            </Link>
          </p>
        </form>
      </div>
    </div>
  )
}
