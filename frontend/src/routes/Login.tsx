import { useState, type FormEvent } from 'react'
import { Link, useNavigate } from 'react-router-dom'
import { AmbientGlyphs, Wordmark } from '../components/Brand'
import { isMFARequired } from '../lib/api'
import { useLogin, useVerifyMFA } from '../lib/session'

export function Login() {
  const navigate = useNavigate()
  const login = useLogin()
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  // Switches to the second step. The pending login itself lives in an httpOnly
  // cookie the server set, so nothing sensitive is held in component state.
  const [needsCode, setNeedsCode] = useState(false)

  function onSubmit(e: FormEvent) {
    e.preventDefault()
    login.mutate(
      { email, password },
      {
        onSuccess: (result) => {
          if (isMFARequired(result)) {
            setNeedsCode(true)
            // Drop the password from memory the moment it is no longer needed.
            setPassword('')
            return
          }
          navigate('/')
        },
      },
    )
  }

  if (needsCode) {
    return <MFAStep onDone={() => navigate('/')} onRestart={() => setNeedsCode(false)} />
  }

  return (
    <Shell>
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

        {login.isError && <ErrorNote>{login.error.message}</ErrorNote>}

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
    </Shell>
  )
}

/** The second factor step. */
function MFAStep({ onDone, onRestart }: { onDone: () => void; onRestart: () => void }) {
  const verify = useVerifyMFA()
  const [code, setCode] = useState('')
  const [useRecovery, setUseRecovery] = useState(false)

  function onSubmit(e: FormEvent) {
    e.preventDefault()
    verify.mutate(
      useRecovery ? { recovery_code: code } : { code },
      { onSuccess: onDone },
    )
  }

  return (
    <Shell>
      <form onSubmit={onSubmit} className="glass p-8">
        <h1 className="text-xl font-semibold">One more thing</h1>
        <p className="mt-1 mb-6 text-sm text-mist-300">
          {useRecovery
            ? 'Enter one of the recovery codes you saved when you set this up.'
            : 'Enter the 6-digit code from your authenticator app.'}
        </p>

        <div>
          <label className="label" htmlFor="code">
            {useRecovery ? 'Recovery code' : 'Authentication code'}
          </label>
          <input
            id="code"
            className="field text-center text-lg tracking-[0.3em]"
            // one-time-code lets iOS and Android offer the code from the
            // keyboard; numeric mode gives phones the right keypad.
            autoComplete="one-time-code"
            inputMode={useRecovery ? 'text' : 'numeric'}
            pattern={useRecovery ? undefined : '[0-9]*'}
            maxLength={useRecovery ? 11 : 6}
            placeholder={useRecovery ? 'XXXXX-XXXXX' : '000000'}
            autoFocus
            required
            value={code}
            onChange={(e) => setCode(e.target.value)}
          />
        </div>

        {verify.isError && <ErrorNote>{verify.error.message}</ErrorNote>}

        <button type="submit" className="btn-primary mt-6 w-full" disabled={verify.isPending}>
          {verify.isPending ? 'Verifying…' : 'Verify'}
        </button>

        <div className="mt-6 flex flex-col gap-2 text-center text-sm">
          <button
            type="button"
            className="text-arcane-400 hover:text-arcane-500"
            onClick={() => {
              setUseRecovery((v) => !v)
              setCode('')
            }}
          >
            {useRecovery
              ? 'Use your authenticator app instead'
              : 'Lost your phone? Use a recovery code'}
          </button>
          <button type="button" className="text-mist-500 hover:text-mist-300" onClick={onRestart}>
            Start over
          </button>
        </div>
      </form>
    </Shell>
  )
}

function Shell({ children }: { children: React.ReactNode }) {
  return (
    <div className="relative flex min-h-screen items-center justify-center px-4 py-12">
      <AmbientGlyphs />
      <div className="relative w-full max-w-md">
        <div className="mb-8 flex justify-center">
          <Wordmark size="lg" />
        </div>
        {children}
      </div>
    </div>
  )
}

function ErrorNote({ children }: { children: React.ReactNode }) {
  return (
    <p
      role="alert"
      className="mt-4 rounded-xl border border-ember-400/30 bg-ember-400/10 px-4 py-2.5 text-sm text-ember-400"
    >
      {children}
    </p>
  )
}
