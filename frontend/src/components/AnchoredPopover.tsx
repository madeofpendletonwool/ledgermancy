import {
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type ReactNode,
  type RefObject,
} from 'react'
import { createPortal } from 'react-dom'

/** Breathing room kept between the popover and the edge of the viewport. */
const VIEWPORT_MARGIN = 8

type Box = { top: number; left: number; width: number; height: number }

/**
 * A popover anchored to a trigger but rendered into <body>.
 *
 * Portalled out of necessity, not taste. `.glass` carries a backdrop-filter,
 * which does two things to anything rendered inside it:
 *
 *   1. `overflow-hidden` on the card clips an absolutely-positioned panel, so a
 *      menu opened on the last row is cut off at the card's edge.
 *   2. A backdrop-filter ancestor becomes the containing block for `position:
 *      fixed` descendants — so a `fixed inset-0` click-away layer is sized to
 *      the card rather than the viewport, and clicking anywhere outside the
 *      card (the header, the page margins) misses it entirely and the menu
 *      stays open.
 *
 * Both were live bugs on the transactions list. Escaping to <body> fixes them
 * together. `routes/Documents.tsx` portals its lightbox for the same reason.
 *
 * The wrapper reproduces the trigger's rect exactly, so children keep whatever
 * `absolute right-0 top-full` positioning they already had and land where they
 * always did — just measured against the viewport instead of a clipped card.
 */
export function AnchoredPopover({
  anchorRef,
  onClose,
  children,
  overlayClassName = '',
}: {
  /** The trigger's wrapper. The panel is positioned against its rect. */
  anchorRef: RefObject<HTMLElement | null>
  onClose: () => void
  /** Positioned children — typically `absolute right-0` panels. */
  children: ReactNode
  /** Extra classes for the click-away layer, e.g. to dim the page behind. */
  overlayClassName?: string
}) {
  const [box, setBox] = useState<Box | null>(null)
  // How far up the panel had to move to stay on screen. This is what keeps a
  // menu opened on the last row of a long page fully visible.
  const [shiftY, setShiftY] = useState(0)
  const panelRef = useRef<HTMLDivElement>(null)

  // Track the trigger. `scroll` is captured so a scrollable ancestor moving the
  // row is seen too, not just the window — otherwise the panel detaches from
  // the row it belongs to.
  useLayoutEffect(() => {
    const place = () => {
      const rect = anchorRef.current?.getBoundingClientRect()
      if (!rect) return
      setBox({
        top: rect.top,
        left: rect.left,
        width: rect.width,
        height: rect.height,
      })
    }
    place()
    window.addEventListener('scroll', place, true)
    window.addEventListener('resize', place)
    return () => {
      window.removeEventListener('scroll', place, true)
      window.removeEventListener('resize', place)
    }
  }, [anchorRef])

  // Measure what actually rendered and lift it if it runs off the bottom.
  // Measuring beats knowing each panel's height: the five panels this wraps are
  // different sizes and some of them grow as their content loads.
  useLayoutEffect(() => {
    const panel = panelRef.current?.firstElementChild
    if (!panel) return

    const fit = () => {
      // The rect already has the current lift baked in, so subtract it back out
      // to reason about where the panel naturally wants to sit. Without this the
      // correction would compound on every pass instead of converging.
      const rect = panel.getBoundingClientRect()
      const naturalTop = rect.top - shiftY
      const naturalBottom = rect.bottom - shiftY

      const overflow = naturalBottom - (window.innerHeight - VIEWPORT_MARGIN)
      if (overflow <= 0) {
        setShiftY(0)
        return
      }
      // Never lift so far that the top goes off the other edge: a panel taller
      // than the viewport is pinned to the top rather than centred on nothing.
      setShiftY(-Math.min(overflow, Math.max(0, naturalTop - VIEWPORT_MARGIN)))
    }

    fit()
    const observer = new ResizeObserver(fit)
    observer.observe(panel)
    return () => observer.disconnect()
    // `box` is a dependency because a scroll changes where the panel sits and
    // therefore whether it fits. `shiftY` is read inside to undo itself.
  }, [box, shiftY])

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  if (!box) return null

  return createPortal(
    <>
      {/* The real click-away layer: in <body>, so it covers the whole viewport
          and the next click anywhere dismisses rather than acting on the page. */}
      <div
        className={`fixed inset-0 z-40 ${overlayClassName}`}
        onClick={onClose}
        aria-hidden
      />
      {/* The wrapper is a transparent stand-in for the trigger's box, so it must
          not swallow clicks: without pointer-events-none a second click on the
          ⋯ button would land here instead of on the overlay and the menu would
          refuse to close. The panels themselves take their events back. */}
      <div
        ref={panelRef}
        className="pointer-events-none fixed z-[45] [&>*]:pointer-events-auto"
        style={{
          top: box.top,
          left: box.left,
          width: box.width,
          height: box.height,
          transform: shiftY ? `translateY(${shiftY}px)` : undefined,
        }}
      >
        {children}
      </div>
    </>,
    document.body,
  )
}
