import { NavLink, useLocation, useNavigate } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { Wordmark } from './Brand'
import { DropdownMenu } from './DropdownMenu'
import { OfflineBanner } from './OfflineBanner'
import { InstallPrompt } from './PwaPrompts'
import { AnimatedOutlet } from './motion'
import { MobileTabBar } from './MobileTabBar'
import { api, isAdult } from '../lib/api'
import { useLogout, useSession } from '../lib/session'

export type NavLeaf = { to: string; label: string; end?: boolean }
export type NavGroup = { label: string; items: NavLeaf[] }

// Dashboard is a bare link; everything else is grouped behind a dropdown so the
// top bar stays to a handful of triggers rather than a dozen flat tabs.
const DASHBOARD: NavLeaf = { to: '/', label: 'Dashboard', end: true }

// A child login gets its own single destination rather than the adult tree with
// items removed. Hiding nav items is not a permission model — every route below
// is guarded server-side — but a child should also never SEE a household
// surface they cannot open.
const CHILD_HOME: NavLeaf = { to: '/', label: 'My money', end: true }

const NAV_GROUPS: NavGroup[] = [
  {
    label: 'Analyze',
    items: [
      { to: '/spending', label: 'Spending' },
      { to: '/insights', label: 'Insights' },
      { to: '/digest', label: 'Digest' },
      { to: '/report', label: 'Report' },
    ],
  },
  {
    label: 'Plan',
    items: [
      { to: '/budgets', label: 'Budgets' },
      { to: '/schedule', label: 'Schedule' },
      { to: '/goals', label: 'Goals' },
      { to: '/shared', label: 'Shared expenses' },
      { to: '/retirement', label: 'Retirement' },
    ],
  },
  {
    label: 'Accounts',
    items: [
      { to: '/accounts', label: 'Accounts' },
      { to: '/transactions', label: 'Transactions' },
      { to: '/categories', label: 'Categories' },
      { to: '/merchants', label: 'Merchants' },
      { to: '/documents', label: 'Documents' },
      // Under Accounts rather than Analyze: a paystub is a record the household
      // keeps, alongside its transactions and its paperwork, not a derived view
      // of one.
      { to: '/paystubs', label: 'Paystubs' },
      { to: '/net-worth', label: 'Net worth' },
      { to: '/investments', label: 'Investments' },
    ],
  },
]

// The assistant only exists when an AI provider is configured; it lives with the
// other insight-y views under Analyze.
function useNavGroups(): NavGroup[] {
  const { data: user } = useSession()
  const capabilities = useQuery({
    queryKey: ['capabilities'],
    queryFn: api.capabilities,
    staleTime: Infinity,
  })
  // A child has no groups at all: their whole app is one page.
  if (!isAdult(user)) return []
  if (!capabilities.data?.ai_enabled) return NAV_GROUPS
  return NAV_GROUPS.map((group) =>
    group.label === 'Analyze'
      ? { ...group, items: [...group.items, { to: '/assistant', label: 'Assistant' }] }
      : group,
  )
}

/** The home link, which is the child's entire navigation. */
function useHomeLink(): NavLeaf {
  const { data: user } = useSession()
  return isAdult(user) ? DASHBOARD : CHILD_HOME
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

// Block link used inside a dropdown panel.
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
  const home = useHomeLink()

  const signOut = () =>
    logout.mutate(undefined, { onSuccess: () => navigate('/login') })

  return (
    <div className="min-h-screen">
      {/* z-40 keeps the bar — and the nav dropdowns, which live inside the
          stacking context it creates — above every page layer. Page content
          lifts itself as high as z-20 (the transactions filter bar), so a
          matching z-20 here lost the tie to whatever came later in the DOM.
          Modals are fixed at z-50 and still cover it. */}
      <header className="sticky top-0 z-40 border-b border-white/10 bg-ink-950/70 backdrop-blur-xl">
        <div className="mx-auto flex max-w-6xl items-center gap-6 px-4 py-4 sm:px-6">
          <NavLink to="/" aria-label="Ledgermancy home" className="rounded-lg">
            <Wordmark />
          </NavLink>

          <nav className="hidden items-center gap-1 lg:flex">
            <NavLink to={home.to} end={home.end} className={navLinkClass}>
              {home.label}
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

            <div>
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
          </div>
        </div>

        {/* Inside the sticky header, below the nav row, so it stays pinned to
            the top of the viewport. A disclosure that the figures are stale is
            worth little if it scrolls away from the figures. */}
        <OfflineBanner />
      </header>

      <main className="mx-auto max-w-6xl px-4 pt-6 pb-28 sm:px-6 sm:pt-10 sm:pb-28 lg:pb-10">
        <InstallPrompt />
        <AnimatedOutlet />
      </main>

      {/* Mobile-only bottom tab bar (PWA). Hidden for child logins, whose
          whole app is a single page with no groups. */}
      {navGroups.length > 0 && <MobileTabBar home={home} groups={navGroups} />}
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
