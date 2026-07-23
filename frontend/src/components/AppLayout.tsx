import { useEffect, useState } from 'react'
import { NavLink, Outlet, useLocation, useNavigate } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { Wordmark } from './Brand'
import { DropdownMenu } from './DropdownMenu'
import { api } from '../lib/api'
import { useLogout, useSession } from '../lib/session'

type NavLeaf = { to: string; label: string; end?: boolean }
type NavGroup = { label: string; items: NavLeaf[] }

// Dashboard is a bare link; everything else is grouped behind a dropdown so the
// top bar stays to a handful of triggers rather than a dozen flat tabs.
const DASHBOARD: NavLeaf = { to: '/', label: 'Dashboard', end: true }

const NAV_GROUPS: NavGroup[] = [
  {
    label: 'Analyze',
    items: [
      { to: '/spending', label: 'Spending' },
      { to: '/insights', label: 'Insights' },
      { to: '/report', label: 'Report' },
    ],
  },
  {
    label: 'Plan',
    items: [
      { to: '/budgets', label: 'Budgets' },
      { to: '/goals', label: 'Goals' },
    ],
  },
  {
    label: 'Accounts',
    items: [
      { to: '/accounts', label: 'Accounts' },
      { to: '/transactions', label: 'Transactions' },
      { to: '/categories', label: 'Categories' },
      { to: '/net-worth', label: 'Net worth' },
    ],
  },
]

// The assistant only exists when an AI provider is configured; it lives with the
// other insight-y views under Analyze.
function useNavGroups(): NavGroup[] {
  const capabilities = useQuery({
    queryKey: ['capabilities'],
    queryFn: api.capabilities,
    staleTime: Infinity,
  })
  if (!capabilities.data?.ai_enabled) return NAV_GROUPS
  return NAV_GROUPS.map((group) =>
    group.label === 'Analyze'
      ? { ...group, items: [...group.items, { to: '/assistant', label: 'Assistant' }] }
      : group,
  )
}

// A group is "active" when the current route is one of its leaves.
function groupIsActive(group: NavGroup, pathname: string): boolean {
  return group.items.some(
    (item) => pathname === item.to || pathname.startsWith(item.to + '/'),
  )
}

const navLinkClass = ({ isActive }: { isActive: boolean }) =>
  `rounded-lg px-3 py-1.5 text-sm transition ${
    isActive
      ? 'bg-white/10 text-mist-100'
      : 'text-mist-300 hover:bg-white/5 hover:text-mist-100'
  }`

// Block link used inside a dropdown panel and the mobile drawer.
const menuItemClass = ({ isActive }: { isActive: boolean }) =>
  `block rounded-lg px-3 py-2 text-sm transition ${
    isActive
      ? 'bg-white/10 text-mist-100'
      : 'text-mist-300 hover:bg-white/5 hover:text-mist-100'
  }`

export function AppLayout() {
  const { data: user } = useSession()
  const logout = useLogout()
  const navigate = useNavigate()
  const location = useLocation()
  const navGroups = useNavGroups()
  const [menuOpen, setMenuOpen] = useState(false)

  const signOut = () =>
    logout.mutate(undefined, { onSuccess: () => navigate('/login') })

  // Close the mobile drawer whenever the route changes so it never lingers
  // open on top of a freshly navigated page.
  useEffect(() => {
    setMenuOpen(false)
  }, [location.pathname])

  return (
    <div className="min-h-screen">
      <header className="sticky top-0 z-20 border-b border-white/10 bg-ink-950/70 backdrop-blur-xl">
        <div className="mx-auto flex max-w-6xl items-center gap-6 px-4 py-4 sm:px-6">
          <NavLink to="/" aria-label="Ledgermancy home" className="rounded-lg">
            <Wordmark />
          </NavLink>

          <nav className="hidden items-center gap-1 lg:flex">
            <NavLink to={DASHBOARD.to} end={DASHBOARD.end} className={navLinkClass}>
              {DASHBOARD.label}
            </NavLink>
            {navGroups.map((group) => (
              <DropdownMenu
                key={group.label}
                label={group.label}
                menuLabel={group.label}
                active={groupIsActive(group, location.pathname)}
              >
                {(close) =>
                  group.items.map((item) => (
                    <NavLink
                      key={item.to}
                      to={item.to}
                      role="menuitem"
                      onClick={close}
                      className={menuItemClass}
                    >
                      {item.label}
                    </NavLink>
                  ))
                }
              </DropdownMenu>
            ))}
          </nav>

          <div className="ml-auto flex items-center gap-2">
            <NotificationBell />

            <div className="hidden lg:block">
              <DropdownMenu
                label={user?.display_name ?? 'Account'}
                menuLabel="Account"
                align="right"
              >
                {(close) => (
                  <>
                    <NavLink
                      to="/settings"
                      role="menuitem"
                      onClick={close}
                      className={menuItemClass}
                    >
                      Settings
                    </NavLink>
                    <button
                      type="button"
                      role="menuitem"
                      disabled={logout.isPending}
                      onClick={() => {
                        close()
                        signOut()
                      }}
                      className="block w-full rounded-lg px-3 py-2 text-left text-sm text-mist-300 transition hover:bg-white/5 hover:text-mist-100 disabled:opacity-50"
                    >
                      Sign out
                    </button>
                  </>
                )}
              </DropdownMenu>
            </div>

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
            <div className="mx-auto flex max-w-6xl flex-col gap-3">
              <NavLink
                to={DASHBOARD.to}
                end={DASHBOARD.end}
                onClick={() => setMenuOpen(false)}
                className={mobileLinkClass}
              >
                {DASHBOARD.label}
              </NavLink>

              {navGroups.map((group) => (
                <div key={group.label} className="flex flex-col gap-1">
                  <span className="px-4 text-xs font-medium uppercase tracking-wide text-mist-500">
                    {group.label}
                  </span>
                  {group.items.map((item) => (
                    <NavLink
                      key={item.to}
                      to={item.to}
                      onClick={() => setMenuOpen(false)}
                      className={mobileLinkClass}
                    >
                      {item.label}
                    </NavLink>
                  ))}
                </div>
              ))}

              <div className="mt-2 flex flex-col gap-1 border-t border-white/10 pt-3">
                <NavLink
                  to="/settings"
                  onClick={() => setMenuOpen(false)}
                  className={mobileLinkClass}
                >
                  Settings
                </NavLink>
                <button
                  type="button"
                  disabled={logout.isPending}
                  onClick={() => {
                    setMenuOpen(false)
                    signOut()
                  }}
                  className="block rounded-lg px-4 py-2.5 text-left text-sm text-mist-300 transition hover:bg-white/5 hover:text-mist-100 disabled:opacity-50"
                >
                  Sign out
                </button>
              </div>
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

const mobileLinkClass = ({ isActive }: { isActive: boolean }) =>
  `block rounded-lg px-4 py-2.5 text-sm transition ${
    isActive
      ? 'bg-white/10 text-mist-100'
      : 'text-mist-300 hover:bg-white/5 hover:text-mist-100'
  }`

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
