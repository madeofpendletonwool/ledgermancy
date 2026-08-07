import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { ApiError, api, isAdult, isMFARequired, isOwner } from './api'

/**
 * These cover the transport, not the endpoint list: how a request is shaped,
 * how a failure is turned into an error the UI can branch on, and how the CSRF
 * token is obtained and attached. Adding an endpoint should not need a test
 * here; changing `request()` should break several.
 *
 * `document` is stubbed rather than provided by jsdom because api.ts touches
 * exactly one DOM property — `document.cookie` — and a whole DOM
 * implementation to supply one string is a dependency that earns nothing.
 */

let fetchMock: ReturnType<typeof vi.fn>

/** The cookie jar `readCookie` sees. */
function setCookie(value: string) {
  vi.stubGlobal('document', { cookie: value })
}

/**
 * Responses are BUILT per call, never shared. A Response body can only be read
 * once, so handing the same object to two requests fails with "Body is
 * unusable" rather than anything to do with the code under test.
 */
function replyAlways(build: () => Response) {
  fetchMock.mockImplementation(async () => build())
}

/** Queues one response per call, in order. */
function reply(...builders: Array<() => Response>) {
  for (const build of builders) {
    fetchMock.mockImplementationOnce(async () => build())
  }
}

function jsonResponse(body: unknown, init: ResponseInit = {}) {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
    ...init,
  })
}

/** A body-less response. `null` rather than '' because 204 forbids a body. */
function emptyResponse(status = 204, init: ResponseInit = {}) {
  return new Response(null, { status, ...init })
}

/** The nth call `request()` made, as [path, init]. */
function callAt(index: number): [string, RequestInit] {
  return fetchMock.mock.calls[index] as [string, RequestInit]
}

function headersOf(index: number): Record<string, string> {
  return (callAt(index)[1].headers ?? {}) as Record<string, string>
}

/** The rejection of a call, typed — `rejects.toMatchObject` cannot see `message`. */
async function rejection(promise: Promise<unknown>): Promise<ApiError> {
  const err = await promise.then(
    () => {
      throw new Error('expected the request to reject')
    },
    (e: unknown) => e,
  )
  expect(err).toBeInstanceOf(ApiError)
  return err as ApiError
}

beforeEach(() => {
  fetchMock = vi.fn()
  vi.stubGlobal('fetch', fetchMock)
  setCookie('ledgermancy_csrf=token-from-cookie')
})

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('request shaping', () => {
  it('sends a GET with the session cookie and no body', async () => {
    replyAlways(() => jsonResponse([{ id: 'acct-1' }]))

    await expect(api.accounts()).resolves.toEqual([{ id: 'acct-1' }])

    const [path, init] = callAt(0)
    expect(path).toBe('/api/accounts')
    expect(init.method).toBe('GET')
    expect(init.body).toBeUndefined()
    // Sessions are an httpOnly cookie; without this every request is anonymous.
    expect(init.credentials).toBe('include')
  })

  it('sets Content-Type only when there is a body', async () => {
    replyAlways(() => jsonResponse({}))

    await api.accounts()
    expect(headersOf(0)['Content-Type']).toBeUndefined()

    await api.login({ email: 'a@example.com', password: 'pw' })
    expect(headersOf(1)['Content-Type']).toBe('application/json')
  })

  it('serialises the body as JSON', async () => {
    replyAlways(() => jsonResponse({ id: 'u-1' }))

    await api.login({ email: 'a@example.com', password: 'pw' })

    expect(callAt(0)[1].body).toBe(
      JSON.stringify({ email: 'a@example.com', password: 'pw' }),
    )
  })

  it('sends a body-less POST with no body at all', async () => {
    // Not "{}" — an endpoint that reads no body should not be handed one.
    replyAlways(() => emptyResponse())

    await api.logout()

    expect(callAt(0)[1].body).toBeUndefined()
    expect(headersOf(0)['Content-Type']).toBeUndefined()
  })

  it('carries the id through a path-parameter route', async () => {
    replyAlways(() => emptyResponse())

    await api.revokeSession('sess-1')

    expect(callAt(0)[0]).toBe('/api/auth/sessions/sess-1')
    expect(callAt(0)[1].method).toBe('DELETE')
  })
})

describe('query string building', () => {
  it('includes only the params that carry a value', async () => {
    replyAlways(() => jsonResponse([]))

    await api.transactions({
      from: '2026-01-01',
      to: '',
      limit: 50,
      offset: undefined,
      category_id: undefined,
    })

    const url = new URL(callAt(0)[0], 'https://example.test')
    expect(url.pathname).toBe('/api/transactions')
    expect([...url.searchParams]).toEqual([
      ['from', '2026-01-01'],
      ['limit', '50'],
    ])
  })

  it('comma-joins an account filter', async () => {
    replyAlways(() => jsonResponse([]))

    await api.transactions({ accounts: ['acct-1', 'acct-2'] })

    expect(
      new URL(callAt(0)[0], 'https://example.test').searchParams.get('accounts'),
    ).toBe('acct-1,acct-2')
  })

  it('sends an empty account filter as a bare `accounts=`', async () => {
    // DEVIATION, pinned deliberately. The JSDoc on TransactionQuery says an
    // empty array "drops out entirely"; it does not. withQuery skips a value
    // that IS the empty string, and `[]` is not — it only serialises to one.
    //
    // Harmless against this server: parseUUIDList trims the raw value and
    // returns nil, so `?accounts=` and no param select the same rows. It is
    // recorded rather than fixed because the fix belongs to whoever owns
    // withQuery's contract, and because the two URLs are distinct service
    // worker cache keys for identical data.
    replyAlways(() => jsonResponse([]))

    await api.transactions({ accounts: [] })

    expect(callAt(0)[0]).toBe('/api/transactions?accounts=')
  })

  it('percent-encodes a free-text search', async () => {
    replyAlways(() => jsonResponse([]))

    await api.transactions({ q: 'joe & sons #2' })

    const raw = callAt(0)[0]
    expect(raw).not.toContain(' ')
    expect(new URL(raw, 'https://example.test').searchParams.get('q')).toBe(
      'joe & sons #2',
    )
  })

  it('omits the question mark when nothing is set', async () => {
    replyAlways(() => jsonResponse([]))

    await api.transactions({})

    expect(callAt(0)[0]).toBe('/api/transactions')
  })
})

describe('CSRF', () => {
  it('echoes the cookie on unsafe methods', async () => {
    replyAlways(() => jsonResponse({}))

    await api.login({ email: 'a@example.com', password: 'pw' })
    expect(headersOf(0)['X-CSRF-Token']).toBe('token-from-cookie')

    await api.setItemSharing('item-1', true)
    expect(callAt(1)[1].method).toBe('PATCH')
    expect(headersOf(1)['X-CSRF-Token']).toBe('token-from-cookie')

    await api.revokeSession('sess-1')
    expect(callAt(2)[1].method).toBe('DELETE')
    expect(headersOf(2)['X-CSRF-Token']).toBe('token-from-cookie')
  })

  it('does not send a token on safe methods', async () => {
    replyAlways(() => jsonResponse({}))

    await api.accounts()

    expect(headersOf(0)['X-CSRF-Token']).toBeUndefined()
  })

  it('bootstraps a token when the client has no cookie yet', async () => {
    setCookie('')
    reply(
      () => jsonResponse({ csrf_token: 'bootstrapped' }),
      () => jsonResponse({ id: 'u-1' }),
    )

    await api.login({ email: 'a@example.com', password: 'pw' })

    expect(callAt(0)[0]).toBe('/api/auth/csrf')
    expect(callAt(0)[1].credentials).toBe('include')
    expect(headersOf(1)['X-CSRF-Token']).toBe('bootstrapped')
  })

  it('surfaces a failed bootstrap as an ApiError', async () => {
    setCookie('')
    reply(() => emptyResponse(500))

    const err = await rejection(
      api.login({ email: 'a@example.com', password: 'pw' }),
    )
    expect(err.status).toBe(500)

    // The real request must not have gone out unsigned.
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })

  it('re-reads the cookie on every request', async () => {
    // The backend rotates the token on login. Caching it in module state would
    // sign every post-login write with a token the server has already retired.
    replyAlways(() => jsonResponse({}))

    await api.login({ email: 'a@example.com', password: 'pw' })
    setCookie('ledgermancy_csrf=rotated-after-login')
    await api.scanMerchants()

    expect(headersOf(0)['X-CSRF-Token']).toBe('token-from-cookie')
    expect(headersOf(1)['X-CSRF-Token']).toBe('rotated-after-login')
  })

  it('picks the right cookie out of a jar', async () => {
    // `not_ledgermancy_csrf` must not match: the name has to start the string
    // or follow "; ", or a decoy cookie could pin a token of someone else's
    // choosing onto the victim's requests.
    setCookie('not_ledgermancy_csrf=decoy; other=x; ledgermancy_csrf=real; z=1')
    replyAlways(() => jsonResponse({}))

    await api.scanMerchants()

    expect(headersOf(0)['X-CSRF-Token']).toBe('real')
  })

  it('URL-decodes the cookie value', async () => {
    setCookie('ledgermancy_csrf=a%2Bb%3Dc')
    replyAlways(() => jsonResponse({}))

    await api.scanMerchants()

    expect(headersOf(0)['X-CSRF-Token']).toBe('a+b=c')
  })
})

describe('error handling', () => {
  it('raises the server message with its status', async () => {
    replyAlways(() =>
      jsonResponse({ error: 'that email is already registered' }, { status: 409 }),
    )

    const err = await rejection(
      api.login({ email: 'a@example.com', password: 'pw' }),
    )

    expect(err.status).toBe(409)
    expect(err.message).toBe('that email is already registered')
  })

  it('is a real Error, so it survives a rethrow and reads in a log', async () => {
    replyAlways(() => jsonResponse({ error: 'nope' }, { status: 401 }))

    const err = await rejection(api.accounts())

    expect(err).toBeInstanceOf(Error)
    // Callers branch on the status — the 401 handler is what signs the user out.
    expect(err.name).toBe('ApiError')
    expect(err.status).toBe(401)
  })

  it('falls back to the status text when the body is not JSON', async () => {
    // A proxy or a crashed process answers with HTML. Parsing that and
    // throwing a SyntaxError would hide the 502 behind "Unexpected token <".
    replyAlways(
      () =>
        new Response('<html>502 Bad Gateway</html>', {
          status: 502,
          statusText: 'Bad Gateway',
        }),
    )

    const err = await rejection(api.accounts())

    expect(err.status).toBe(502)
    expect(err.message).toBe('Bad Gateway')
  })

  it('falls back to the status text when JSON carries no error field', async () => {
    replyAlways(() =>
      jsonResponse({ detail: 'something' }, { status: 400, statusText: 'Bad Request' }),
    )

    const err = await rejection(api.accounts())

    expect(err.status).toBe(400)
    expect(err.message).toBe('Bad Request')
  })
})

describe('response bodies', () => {
  it('parses a JSON body', async () => {
    replyAlways(() => jsonResponse({ id: 'u-1', role: 'owner' }))

    await expect(api.me()).resolves.toEqual({ id: 'u-1', role: 'owner' })
  })

  it.each([200, 201, 202, 204])(
    'treats an empty body on %i as success, not failure',
    async (status) => {
      // The regression: this once checked only for 204, so the queued-work
      // endpoints that answer 202 with nothing looked like they had failed and
      // the UI reported the action had not taken — while it had.
      replyAlways(() => emptyResponse(status))

      await expect(api.scanMerchants()).resolves.toBeUndefined()
    },
  )
})

describe('offline writes', () => {
  it('refuses a write the browser cannot deliver', async () => {
    vi.stubGlobal('navigator', { onLine: false })

    const err = await rejection(api.scanMerchants())
    expect(err.status).toBe(503)

    // Nothing queued, nothing attempted — the point is a sentence the user can
    // act on instead of a bare "Failed to fetch" out of TanStack Query.
    expect(fetchMock).not.toHaveBeenCalled()
  })

  it('still allows a read, which the service worker may answer', async () => {
    vi.stubGlobal('navigator', { onLine: false })
    replyAlways(() => jsonResponse([{ id: 'acct-1' }]))

    await expect(api.accounts()).resolves.toEqual([{ id: 'acct-1' }])
  })

  it('allows a write when the browser reports a connection', async () => {
    vi.stubGlobal('navigator', { onLine: true })
    replyAlways(() => emptyResponse(202))

    await expect(api.scanMerchants()).resolves.toBeUndefined()
  })
})

describe('role helpers', () => {
  // These decide what renders, never what is permitted — every restricted
  // route is guarded server-side. Pinned so a fourth role cannot quietly
  // acquire the adult navigation by defaulting to true.
  it('counts owners and members as adults', () => {
    expect(isAdult({ role: 'owner' })).toBe(true)
    expect(isAdult({ role: 'member' })).toBe(true)
    expect(isAdult({ role: 'child' })).toBe(false)
    expect(isAdult(null)).toBe(false)
    expect(isAdult(undefined)).toBe(false)
  })

  it('counts only the owner as owner', () => {
    expect(isOwner({ role: 'owner' })).toBe(true)
    expect(isOwner({ role: 'member' })).toBe(false)
    expect(isOwner({ role: 'child' })).toBe(false)
    expect(isOwner(null)).toBe(false)
  })

  it('narrows a login stopped at the second factor', () => {
    expect(isMFARequired({ mfa_required: true })).toBe(true)
    expect(
      isMFARequired({
        id: 'u-1',
        household_id: 'h-1',
        email: 'a@example.com',
        display_name: 'A',
        role: 'owner',
        person_id: null,
      }),
    ).toBe(false)
  })
})
