import { useEffect, useState } from 'react'
import { NavLink, Outlet, useLocation, useNavigate } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { Wordmark } from './Brand'
import { api } from '../lib/api'
import { useLogout, useSession } from '../lib/session'

type NavItem = { to: string; label: string; end: boolean }

const NAV: NavItem[] = [
  { to: '/', label: 'Dashboard', end: true },
  { to: '/spending', label: 'Spending', end: false },
  { to: '/net-worth', label: 'Net worth', end: false },
  { to: '/accounts', label: 'Accounts', end: false },
  { to: '/transactions', label: 'Transactions', end: false },
  { to: '/report', label: 'Report', end: false },
  { to: '/alerts', label: 'Alerts', end: false },
  { to: '/household', label: 'Household', end: false },
  { to: '/security', label: 'Security', end: false },
]

// The assistant tab only appears when an AI provider is configured; slotted in
// after Report so it sits with the other insight-y views.
function useNavItems(): NavItem[] {
  const capabilities = useQuery({
    queryKey: ['capabilities'],
    queryFn: api.capabilities,
    staleTime: Infinity,
  })
  if (!capabilities.data?.ai_enabled) return NAV
  const items = [...NAV]
  const at = items.findIndex((i) => i.to === '/alerts')
  items.splice(at, 0, { to: '/assistant', label: 'Assistant', end: false })
  return items
}

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
  const navItems = useNavItems()
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
            {navItems.map((item) => (
              <NavLink key={item.to} to={item.to} end={item.end} className={navLinkClass}>
                {item.label}
              </NavLink>
            ))}
          </nav>

          <div className="ml-auto flex items-center gap-4">
            <NotificationBell />
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
              {navItems.map((item) => (
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

// NotificationBell links to the Alerts page and shows the unread event count.
// The count is polled on a slow interval — alerts are not time-critical, and a
// minute-scale refresh keeps it fresh without hammering the API.
function NotificationBell() {
  const unread = useQuery({
    queryKey: ['alerts', 'unread'],
    queryFn: api.unreadAlertCount,
    refetchInterval: 60_000,
    refetchOnWindowFocus: true,
  })
  const count = unread.data?.count ?? 0

  return (
    <NavLink
      to="/alerts"
      aria-label={count > 0 ? `Alerts, ${count} unread` : 'Alerts'}
      className="relative rounded-lg p-1.5 text-mist-300 transition hover:bg-white/5 hover:text-mist-100"
    >
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
        <path d="M18 8A6 6 0 0 0 6 8c0 7-3 9-3 9h18s-3-2-3-9" />
        <path d="M13.73 21a2 2 0 0 1-3.46 0" />
      </svg>
      {count > 0 && (
        <span className="absolute -right-0.5 -top-0.5 flex h-4 min-w-4 items-center justify-center rounded-full bg-arcane-500 px-1 text-[10px] font-semibold text-white">
          {count > 99 ? '99+' : count}
        </span>
      )}
    </NavLink>
  )
}
