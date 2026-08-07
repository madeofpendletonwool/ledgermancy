import { Component, type ErrorInfo, type ReactNode } from 'react'
import { Sigil } from './Brand'

/**
 * Error boundaries (MAD-61).
 *
 * React unmounts the entire tree when a render throws and nothing catches it.
 * Without a boundary, one null field from an API shape change or one chart that
 * indexes past the end of an array replaces the whole app with a blank page —
 * no message, no way back except a manual reload.
 *
 * Three boundaries, deliberately nested smallest-blast-radius first:
 *
 *   - `ChartBoundary` (charts/ChartBoundary.tsx) — a chart fails, the page's
 *     numbers stay on screen.
 *   - `RouteErrorBoundary` — a page fails, the shell and nav survive so the
 *     user can navigate away.
 *   - `AppErrorBoundary` — everything else, including a throw from the router
 *     itself or from the two public screens that live outside `AppLayout`.
 *
 * `errorElement` is NOT usable here. It is a data-router feature, and this app
 * mounts `<BrowserRouter>` + `<Routes>`; routes rendered that way get no error
 * handling from react-router at all, whatever the version. Hence class
 * components — still the only way to catch a render error in React 19.
 *
 * A boundary that renders nothing is worse than the white screen, because it
 * hides the failure. Every fallback below says something happened and offers a
 * way forward.
 */

type FallbackRender = (message: string, retry: () => void) => ReactNode

type Props = {
  children: ReactNode
  /** Rendered in place of `children` once a render below has thrown. */
  fallback: FallbackRender
  /** Names the boundary in the console line. */
  label: string
  /**
   * Changing this clears a caught error *without* remounting the boundary.
   *
   * The alternative — `key={pathname}` on the boundary — also resets it, but
   * remounts everything beneath, which would tear down the `AnimatePresence`
   * in the route outlet on every navigation and lose the crossfade.
   */
  resetKey?: unknown
}

// `caught` is a box rather than a bare `unknown` so that `throw null` and
// `throw undefined` are still distinguishable from "nothing has thrown".
// `seenResetKey` mirrors the last `resetKey` prop, which is what makes the
// reset a render-phase comparison rather than a setState after the fact.
type State = { caught: { error: unknown } | null; seenResetKey: unknown }

export class ErrorBoundary extends Component<Props, State> {
  state: State = { caught: null, seenResetKey: this.props.resetKey }

  static getDerivedStateFromError(error: unknown): Partial<State> {
    return { caught: { error } }
  }

  static getDerivedStateFromProps(props: Props, state: State): Partial<State> | null {
    if (props.resetKey === state.seenResetKey) return null
    // The re-render that first shows the error runs with the same props, so
    // this cannot swallow the error it was just told about.
    return { caught: null, seenResetKey: props.resetKey }
  }

  componentDidCatch(error: unknown, info: ErrorInfo) {
    // The app has no client-side error reporting service, and adding one is a
    // dependency decision that is not ours to make silently. The console is
    // where this goes until there is somewhere better.
    console.error(`[${this.props.label}] render failed`, error, info.componentStack)
  }

  retry = () => this.setState({ caught: null })

  render(): ReactNode {
    const { caught } = this.state
    if (caught) return this.props.fallback(messageOf(caught.error), this.retry)
    return this.props.children
  }
}

/** Anything can be thrown, not just an `Error`. Get a sentence out of it. */
function messageOf(error: unknown): string {
  if (error instanceof Error && error.message) return error.message
  if (typeof error === 'string' && error) return error
  return 'No further detail was available.'
}

/**
 * The outermost boundary, wrapped around the router in main.tsx.
 *
 * It sits outside the router, so it cannot offer a link — the only honest
 * affordance at this level is a reload.
 */
export function AppErrorBoundary({ children }: { children: ReactNode }) {
  return (
    <ErrorBoundary label="app" fallback={renderAppFallback}>
      {children}
    </ErrorBoundary>
  )
}

function renderAppFallback(message: string): ReactNode {
  return (
    <div className="flex min-h-screen items-center justify-center px-6">
      <div role="alert" className="glass max-w-md p-8 text-center">
        <Sigil className="mx-auto h-12 w-12" />
        <h1 className="mt-4 text-lg font-medium text-mist-100">Something went wrong.</h1>
        <p className="mt-2 text-sm text-mist-300">
          Ledgermancy hit an error it couldn’t recover from. Reloading usually clears
          it, and nothing you have recorded is affected.
        </p>
        <p className="mt-3 text-xs break-words text-mist-500">{message}</p>
        <button
          type="button"
          className="btn-primary mt-6"
          onClick={() => window.location.reload()}
        >
          Reload
        </button>
      </div>
    </div>
  )
}

/**
 * The per-page boundary, wrapped around the route outlet inside `AppLayout`.
 *
 * Pass the current pathname as `resetKey`: a page that has failed clears itself
 * when the user navigates, so the failure does not follow them around the app.
 */
export function RouteErrorBoundary({
  resetKey,
  children,
}: {
  resetKey: unknown
  children: ReactNode
}) {
  return (
    <ErrorBoundary label="route" fallback={renderRouteFallback} resetKey={resetKey}>
      {children}
    </ErrorBoundary>
  )
}

function renderRouteFallback(message: string, retry: () => void): ReactNode {
  return (
    <section role="alert" className="glass p-6">
      <h2 className="text-base font-medium text-mist-100">
        Something went wrong loading this page.
      </h2>
      <p className="mt-2 text-sm text-mist-300">
        The rest of the app is still working — try this page again, or pick another
        from the menu above.
      </p>
      <p className="mt-3 text-xs break-words text-mist-500">{message}</p>
      <div className="mt-5 flex flex-wrap gap-2">
        <button type="button" className="btn-primary" onClick={retry}>
          Try again
        </button>
        <button
          type="button"
          className="btn-ghost"
          onClick={() => window.location.reload()}
        >
          Reload the app
        </button>
      </div>
    </section>
  )
}
