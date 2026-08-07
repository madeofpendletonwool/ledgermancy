import type { ReactNode } from 'react'
import { ErrorBoundary } from '../ErrorBoundary'

/**
 * The boundary every chart is exported behind (MAD-61).
 *
 * The charts are the highest-risk render surface in the app: `d3-sankey`, a
 * dozen hand-rolled SVG components, and several that index into arrays derived
 * from API data. A chart that throws should cost the reader the chart, not the
 * page — the figures it was illustrating sit right beside it and are still
 * true.
 *
 * The wrap happens at each chart's own export rather than at the ~39 places
 * charts are used, so a new call site cannot forget it and the call sites stay
 * readable. See any file in this directory for the shape.
 *
 * `label` is the chart's name as a reader would say it. It lands mid-sentence
 * in the fallback ("The cash flow chart couldn’t be drawn"), so it is a
 * lower-case noun phrase with no "chart" on the end.
 */
export function ChartBoundary({ label, children }: { label: string; children: ReactNode }) {
  return (
    <ErrorBoundary
      label={`chart:${label}`}
      fallback={(message, retry) => (
        <div
          role="alert"
          className="rounded-xl border border-white/10 bg-white/[0.03] p-4 text-sm"
        >
          <p className="text-mist-100">The {label} chart couldn’t be drawn.</p>
          <p className="mt-1 text-xs text-mist-500">
            The figures on this page are unaffected.{' '}
            <button
              type="button"
              onClick={retry}
              className="underline underline-offset-2 transition hover:text-mist-300"
            >
              Try again
            </button>
          </p>
          <p className="mt-2 text-xs break-words text-mist-500">{message}</p>
        </div>
      )}
    >
      {children}
    </ErrorBoundary>
  )
}
