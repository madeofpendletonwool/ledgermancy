import { describe, expect, it } from 'vitest'

import { ErrorBoundary } from './ErrorBoundary'

/**
 * The reset contract, exercised through the static lifecycle hooks.
 *
 * There is no DOM environment in this suite (vitest runs in node here), so this
 * cannot mount a tree and throw inside it — the rendering half was checked in a
 * browser against deliberate throws. What it does pin is the part that is easy
 * to get subtly wrong and impossible to notice: WHEN a caught error clears. Too
 * eager and the boundary swallows the error it was just handed and re-renders
 * the component that is still throwing; too lazy and a failed page follows the
 * user around the app after they navigate away from it.
 */

type State = { caught: { error: unknown } | null; seenResetKey: unknown }

// The class's own statics, typed for direct call. React invokes these itself;
// calling them here is the only way to reach them without a renderer.
const derivedFromError = ErrorBoundary.getDerivedStateFromError.bind(ErrorBoundary)
const derivedFromProps = (props: { resetKey: unknown }, state: State) =>
  (
    ErrorBoundary as unknown as {
      getDerivedStateFromProps: (p: unknown, s: State) => Partial<State> | null
    }
  ).getDerivedStateFromProps(props, state)

const clean: State = { caught: null, seenResetKey: '/spending' }
const failed: State = { caught: { error: new Error('boom') }, seenResetKey: '/spending' }

describe('ErrorBoundary', () => {
  it('boxes whatever was thrown, including a falsy throw', () => {
    expect(derivedFromError(new Error('boom'))).toEqual({ caught: { error: expect.any(Error) } })
    // `throw null` still has to read as "something failed", not as "no error".
    expect(derivedFromError(null)).toEqual({ caught: { error: null } })
  })

  it('holds the error while the reset key is unchanged', () => {
    // The re-render that first shows the fallback runs with the same props, so
    // returning anything here would swallow the error immediately.
    expect(derivedFromProps({ resetKey: '/spending' }, failed)).toBeNull()
  })

  it('clears the error when the reset key changes', () => {
    expect(derivedFromProps({ resetKey: '/budgets' }, failed)).toEqual({
      caught: null,
      seenResetKey: '/budgets',
    })
  })

  it('tracks the reset key even when nothing has failed', () => {
    // Otherwise the first navigation after a clean render would look like a
    // change on the *second* navigation instead.
    expect(derivedFromProps({ resetKey: '/budgets' }, clean)).toEqual({
      caught: null,
      seenResetKey: '/budgets',
    })
    expect(derivedFromProps({ resetKey: '/spending' }, clean)).toBeNull()
  })
})
