import { useEffect, useId, useRef, useState, type ReactNode } from 'react'
import { useLocation } from 'react-router-dom'

type DropdownMenuProps = {
  /** Text/content shown inside the trigger button, left of the chevron. */
  label: ReactNode
  /** Highlights the trigger — used when a child route is the active page. */
  active?: boolean
  /** Which edge of the trigger the panel aligns to. */
  align?: 'left' | 'right'
  /** Tailwind classes for the trigger button (styling is owned by the caller). */
  triggerClassName?: string
  /** Accessible name for the menu popup. */
  menuLabel: string
  /** Receives `close` so items can dismiss the menu when chosen. */
  children: (close: () => void) => ReactNode
}

// Hand-built dropdown: there's no popover primitive or click-outside hook in the
// codebase, so dismissal (outside-click, Escape, route change) lives here. Shared
// by the grouped top-nav menus and the account menu.
export function DropdownMenu({
  label,
  active = false,
  align = 'left',
  triggerClassName = '',
  menuLabel,
  children,
}: DropdownMenuProps) {
  const [open, setOpen] = useState(false)
  const wrapperRef = useRef<HTMLDivElement>(null)
  const location = useLocation()
  const menuId = useId()

  const close = () => setOpen(false)

  // Close on outside pointer-down and on Escape, but only while open.
  useEffect(() => {
    if (!open) return
    function onPointerDown(e: MouseEvent) {
      if (!wrapperRef.current?.contains(e.target as Node)) setOpen(false)
    }
    function onKeyDown(e: KeyboardEvent) {
      if (e.key === 'Escape') setOpen(false)
    }
    document.addEventListener('mousedown', onPointerDown)
    document.addEventListener('keydown', onKeyDown)
    return () => {
      document.removeEventListener('mousedown', onPointerDown)
      document.removeEventListener('keydown', onKeyDown)
    }
  }, [open])

  // Any navigation dismisses the menu, mirroring the mobile drawer's behavior.
  useEffect(() => {
    setOpen(false)
  }, [location.pathname])

  return (
    <div ref={wrapperRef} className="relative">
      <button
        type="button"
        aria-haspopup="menu"
        aria-expanded={open}
        aria-controls={open ? menuId : undefined}
        onClick={() => setOpen((v) => !v)}
        className={`inline-flex items-center gap-1 rounded-lg px-3 py-1.5 text-sm transition ${
          active || open
            ? 'bg-white/10 text-mist-100'
            : 'text-mist-300 hover:bg-white/5 hover:text-mist-100'
        } ${triggerClassName}`}
      >
        {label}
        <svg
          className={`h-3.5 w-3.5 transition-transform motion-reduce:transition-none ${
            open ? 'rotate-180' : ''
          }`}
          viewBox="0 0 24 24"
          fill="none"
          stroke="currentColor"
          strokeWidth={2}
          strokeLinecap="round"
          strokeLinejoin="round"
          aria-hidden="true"
        >
          <path d="M6 9l6 6 6-6" />
        </svg>
      </button>

      {open && (
        <div
          id={menuId}
          role="menu"
          aria-label={menuLabel}
          className={`absolute top-full z-30 mt-2 min-w-48 rounded-2xl border border-white/10 bg-ink-950/90 p-1.5 shadow-xl shadow-black/40 backdrop-blur-xl ${
            align === 'right' ? 'right-0' : 'left-0'
          }`}
        >
          {children(close)}
        </div>
      )}
    </div>
  )
}
