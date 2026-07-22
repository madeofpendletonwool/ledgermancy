import { useEffect, useState } from 'react'
import { NavLink, Outlet, useLocation, useNavigate } from 'react-router-dom'
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
  { to: '/security', label: 'Security', end: false },
]

const navLinkClass = ({ isActive }: { isActive: boolean }) =>
  `rounded-lg px-3 py-1.5 text-sm transition ${
    isActive
      ? 'bg-white/10 text-mist-100'
      : 'text-mist-300 hover:bg-white/5 hover:text-mist-100'
  }`

export function AppLayout() {
  const { data: user } = useSession()
  const logout = useLogout()
  const navigate = useNavigate()
  const location = useLocation()
  const [menuOpen, setMenuOpen] = useState(false)

  // Close the mobile drawer whenever the route changes so it never lingers
  // open on top of a freshly navigated page.
  useEffect(() => {
    setMenuOpen(false)
  }, [location.pathname])

  return (
    <div className="min-h-screen">
      <header className="sticky top-0 z-20 border-b border-white/10 bg-ink-950/70 backdrop-blur-xl">
        <div className="mx-auto flex max-w-6xl items-center gap-6 px-4 py-4 sm:px-6">
          <Wordmark />

          <nav className="hidden items-center gap-1 lg:flex">
            {NAV.map((item) => (
              <NavLink key={item.to} to={item.to} end={item.end} className={navLinkClass}>
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
            <button
              className="btn-ghost px-2.5 py-1.5 lg:hidden"
              aria-label="Toggle navigation"
              aria-expanded={menuOpen}
              aria-controls="mobile-nav"
              onClick={() => setMenuOpen((open) => !open)}
            >
              {menuOpen ? (
                <svg
                  className="h-5 w-5"
                  viewBox="0 0 24 24"
                  fill="none"
                  stroke="currentColor"
                  strokeWidth={2}
                  strokeLinecap="round"
                  aria-hidden="true"
                >
                  <path d="M6 6l12 12M18 6L6 18" />
                </svg>
              ) : (
                <svg
                  className="h-5 w-5"
                  viewBox="0 0 24 24"
                  fill="none"
                  stroke="currentColor"
                  strokeWidth={2}
                  strokeLinecap="round"
                  aria-hidden="true"
                >
                  <path d="M4 7h16M4 12h16M4 17h16" />
                </svg>
              )}
            </button>
          </div>
        </div>

        {menuOpen && (
          <nav
            id="mobile-nav"
            className="border-t border-white/10 bg-ink-950/70 px-4 py-3 backdrop-blur-xl lg:hidden"
          >
            <div className="mx-auto flex max-w-6xl flex-col gap-1">
              {NAV.map((item) => (
                <NavLink
                  key={item.to}
                  to={item.to}
                  end={item.end}
                  onClick={() => setMenuOpen(false)}
                  className={({ isActive }) =>
                    `block rounded-lg px-4 py-2.5 text-sm transition ${
                      isActive
                        ? 'bg-white/10 text-mist-100'
                        : 'text-mist-300 hover:bg-white/5 hover:text-mist-100'
                    }`
                  }
                >
                  {item.label}
                </NavLink>
              ))}
            </div>
          </nav>
        )}
      </header>

      <main className="mx-auto max-w-6xl px-4 py-6 sm:px-6 sm:py-10">
        <Outlet />
      </main>
    </div>
  )
}
