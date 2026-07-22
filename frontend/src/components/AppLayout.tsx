import { NavLink, Outlet, useNavigate } from 'react-router-dom'
import { Wordmark } from './Brand'
import { useLogout, useSession } from '../lib/session'

const NAV = [
  { to: '/', label: 'Dashboard', end: true },
  { to: '/spending', label: 'Spending', end: false },
  { to: '/net-worth', label: 'Net worth', end: false },
  { to: '/accounts', label: 'Accounts', end: false },
  { to: '/transactions', label: 'Transactions', end: false },
  { to: '/report', label: 'Report', end: false },
  { to: '/household', label: 'Household', end: false },
]

export function AppLayout() {
  const { data: user } = useSession()
  const logout = useLogout()
  const navigate = useNavigate()

  return (
    <div className="min-h-screen">
      <header className="sticky top-0 z-10 border-b border-white/10 bg-ink-950/70 backdrop-blur-xl">
        <div className="mx-auto flex max-w-6xl items-center gap-6 px-6 py-4">
          <Wordmark />

          <nav className="flex items-center gap-1">
            {NAV.map((item) => (
              <NavLink
                key={item.to}
                to={item.to}
                end={item.end}
                className={({ isActive }) =>
                  `rounded-lg px-3 py-1.5 text-sm transition ${
                    isActive
                      ? 'bg-white/10 text-mist-100'
                      : 'text-mist-300 hover:bg-white/5 hover:text-mist-100'
                  }`
                }
              >
                {item.label}
              </NavLink>
            ))}
          </nav>

          <div className="ml-auto flex items-center gap-4">
            <span className="hidden text-sm text-mist-300 sm:inline">
              {user?.display_name}
            </span>
            <button
              className="btn-ghost px-3 py-1.5 text-sm"
              disabled={logout.isPending}
              onClick={() =>
                logout.mutate(undefined, { onSuccess: () => navigate('/login') })
              }
            >
              Sign out
            </button>
          </div>
        </div>
      </header>

      <main className="mx-auto max-w-6xl px-6 py-10">
        <Outlet />
      </main>
    </div>
  )
}
