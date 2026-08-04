import type { ReactNode } from 'react'

/**
 * On-brand empty state (MAD-37).
 *
 * Replaces the old one-line `Empty()` helpers — a muted sentence — with a small
 * panel: an optional glyph, a short title saying what lives here, one line of
 * honest explanation, and the primary call-to-action where one exists. The tone
 * stays blunt (no fake cheer) — only the framing changes.
 *
 * Pass `action` for a button or link styled by the caller (`.btn-primary` /
 * `.btn-ghost` / a `<Link>`). When there is nothing to do — "no spending this
 * month", "nothing due in this window" — omit it; a one-line informational
 * empty state without an action is legitimate, and inventing a CTA there would
 * be the padding the issue asks us to avoid.
 */
export function EmptyState({
  icon,
  title,
  children,
  action,
  className = '',
}: {
  icon?: ReactNode
  title: string
  children?: ReactNode
  action?: ReactNode
  className?: string
}) {
  return (
    <div className={`py-10 text-center ${className}`}>
      {icon && (
        <div className="mx-auto mb-3 flex h-10 w-10 items-center justify-center rounded-full border border-white/10 bg-white/[0.03] text-mist-400">
          {icon}
        </div>
      )}
      <p className="text-sm font-medium text-mist-200">{title}</p>
      {children && (
        <p className="mx-auto mt-1 max-w-md text-sm text-mist-400">{children}</p>
      )}
      {action && <div className="mt-4 flex justify-center">{action}</div>}
    </div>
  )
}
