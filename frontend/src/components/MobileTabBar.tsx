import { useEffect, useState } from 'react'
import { NavLink, useLocation } from 'react-router-dom'
import { AnimatePresence, motion, useReducedMotion } from 'motion/react'
import type { NavGroup, NavLeaf } from './AppLayout'

type MobileTabBarProps = {
  /** The index/home leaf — rendered as a direct tab with no submenu. */
  home: NavLeaf
  /** The category hubs, each opening a bottom sheet of its leaves. */
  groups: NavGroup[]
}

// The mobile twin of the desktop top-bar IA in AppLayout.tsx: four category
// hubs anchored to the bottom of the screen so an installed PWA reads as a
// native app rather than a site with a hamburger. Dashboard is a bare link;
// the three groups each open a bottom sheet (the mobile analogue of the
// desktop DropdownMenu). Desktop nav is untouched — this whole component is
// wrapped in `lg:hidden`.

// A leaf is "active" when the pathname is it exactly, or nests underneath it
// (e.g. /categories/:categoryId under /categories). `end` opts out for the
// index route, which must match exactly.
function leafActive(to: string, pathname: string, end?: boolean): boolean {
  return end ? pathname === to : pathname === to || pathname.startsWith(to + '/')
}

function groupActive(group: NavGroup, pathname: string): boolean {
  return group.items.some((item) => leafActive(item.to, pathname, item.end))
}

const tabClass = (active: boolean) =>
  `flex flex-1 flex-col items-center justify-center gap-1 px-1 py-2 text-[11px] font-medium transition-colors motion-reduce:transition-none min-h-[56px] ${
    active ? 'text-arcane-400' : 'text-mist-300'
  }`

const sheetItemClass = (active: boolean) =>
  `flex items-center rounded-xl px-4 py-3 text-sm transition-colors ${
    active
      ? 'bg-white/10 text-mist-100'
      : 'text-mist-300 hover:bg-white/5 hover:text-mist-100'
  }`

export function MobileTabBar({ home, groups }: MobileTabBarProps) {
  const { pathname } = useLocation()
  // The label of the group whose sheet is open, or null. Only one sheet is
  // open at a time; tapping its tab again collapses it.
  const [open, setOpen] = useState<string | null>(null)
  const reduce = useReducedMotion()

  // Any navigation collapses an open sheet, mirroring the desktop dropdowns
  // and the old mobile drawer.
  useEffect(() => {
    setOpen(null)
  }, [pathname])

  // Escape dismisses the open sheet; the open tab and the scrim also close it.
  useEffect(() => {
    if (!open) return
    function onKey(e: KeyboardEvent) {
      if (e.key === 'Escape') setOpen(null)
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [open])

  const homeActive = leafActive(home.to, pathname, home.end)
  const openGroup = open ? (groups.find((g) => g.label === open) ?? null) : null

  const scrim = reduce ? (
    open ? (
      <div
        aria-hidden="true"
        onClick={() => setOpen(null)}
        className="fixed inset-0 z-40 bg-black/50"
      />
    ) : null
  ) : (
    <AnimatePresence>
      {open && (
        <motion.div
          key="scrim"
          aria-hidden="true"
          onClick={() => setOpen(null)}
          className="fixed inset-0 z-40 bg-black/50"
          initial={{ opacity: 0 }}
          animate={{ opacity: 1 }}
          exit={{ opacity: 0 }}
          transition={{ duration: 0.15 }}
        />
      )}
    </AnimatePresence>
  )

  return (
    <div className="lg:hidden">
      {scrim}

      {/* z-40 matches the sticky header; they never spatially overlap. The
          sheet renders inside the bar (above the tab row) so anchoring needs
          no offset math. Page content lifts to z-20, modals sit at z-50 and
          still cover this. */}
      <nav
        aria-label="Primary"
        className="fixed inset-x-0 bottom-0 z-40 border-t border-white/10 bg-ink-950/85 backdrop-blur-xl"
        style={{ paddingBottom: 'env(safe-area-inset-bottom)' }}
      >
        {openGroup && (
          <div
            role="menu"
            aria-label={openGroup.label}
            className="max-h-[50vh] overflow-y-auto border-b border-white/10 bg-ink-950/95 px-3 py-2 backdrop-blur-xl"
          >
            {openGroup.items.map((item) => {
              const active = leafActive(item.to, pathname, item.end)
              return (
                <NavLink
                  key={item.to}
                  to={item.to}
                  role="menuitem"
                  aria-current={active ? 'page' : undefined}
                  onClick={() => setOpen(null)}
                  className={sheetItemClass(active)}
                >
                  {item.label}
                </NavLink>
              )
            })}
          </div>
        )}

        <div className="mx-auto flex max-w-6xl items-stretch">
          <NavLink
            to={home.to}
            end={home.end}
            aria-current={homeActive ? 'page' : undefined}
            onClick={() => setOpen(null)}
            className={tabClass(homeActive)}
          >
            <HomeIcon />
            <span>{home.label}</span>
          </NavLink>

          {groups.map((group) => {
            const isActive = groupActive(group, pathname)
            const isOpen = open === group.label
            return (
              <button
                key={group.label}
                type="button"
                aria-haspopup="menu"
                aria-expanded={isOpen}
                onClick={() =>
                  setOpen((cur) => (cur === group.label ? null : group.label))
                }
                className={tabClass(isActive || isOpen)}
              >
                <CategoryIcon label={group.label} />
                <span>{group.label}</span>
              </button>
            )
          })}
        </div>
      </nav>
    </div>
  )
}

// Category glyph picked by group label so the four tabs read at a glance.
// Each is a Lucide-style outline so it matches the stroke weight of the
// NotificationBell and the (now removed) hamburger icon.
function HomeIcon() {
  return (
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
      <path d="m3 9 9-7 9 7v11a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z" />
      <path d="M9 22V12h6v10" />
    </svg>
  )
}

function CategoryIcon({ label }: { label: string }) {
  switch (label) {
    case 'Analyze':
      return (
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
          <path d="M3 3v18h18" />
          <path d="M18 17V9" />
          <path d="M13 17V5" />
          <path d="M8 17v-3" />
        </svg>
      )
    case 'Plan':
      return (
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
          <circle cx="12" cy="12" r="9" />
          <circle cx="12" cy="12" r="5" />
          <circle cx="12" cy="12" r="1" />
        </svg>
      )
    case 'Accounts':
      return (
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
          <rect width="20" height="14" x="2" y="5" rx="2" />
          <path d="M2 10h20" />
        </svg>
      )
    default:
      return (
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
          <circle cx="12" cy="12" r="9" />
        </svg>
      )
  }
}
