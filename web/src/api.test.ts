import { beforeEach, describe, expect, it, vi } from 'vitest'

// A fake server: /auth/refresh answers `refreshWith`; every other path answers 200 only to the
// current valid token. Each test re-imports api.ts so the in-memory token starts empty.
let valid = 'tok-1'
let refreshWith: () => Response
const calls: { path: string; auth: string | null }[] = []

const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status })
const unauthorized = () => json(401, { error: { type: 'unauthorized', message: 'no' } })

beforeEach(() => {
  vi.resetModules()
  valid = 'tok-1'
  calls.length = 0
  refreshWith = () => json(200, { data: { access_token: valid, expires_in: 900 } })
  vi.stubGlobal('fetch', async (url: string, init: RequestInit) => {
    const path = url.replace('/api/v1', '')
    const auth = (init.headers as Record<string, string>).Authorization ?? null
    calls.push({ path, auth })
    await new Promise((r) => setTimeout(r, path === '/slow' ? 40 : 5)) // keep callers overlapping
    if (path === '/auth/refresh') return refreshWith()
    if (path === '/auth/login') return json(401, { error: { type: 'invalid_credentials', message: 'no' } })
    return auth === 'Bearer ' + valid ? json(200, { data: path }) : unauthorized()
  })
})

const refreshes = () => calls.filter((c) => c.path === '/auth/refresh').length

describe('api', () => {
  it('single-flights refresh across concurrent 401s, and both retries carry the new token', async () => {
    const { api, signedIn } = await import('./api')
    await expect(Promise.all([api('/a'), api('/b'), api('/c')])).resolves.toEqual(['/a', '/b', '/c'])
    expect(refreshes()).toBe(1)
    expect(calls.filter((c) => c.auth === 'Bearer tok-1')).toHaveLength(3)
    expect(signedIn()).toBe(true)
  })

  it('a failed refresh throws unauthorized and does not retry', async () => {
    refreshWith = unauthorized
    const { api, signedIn } = await import('./api')
    await expect(api('/a')).rejects.toMatchObject({ type: 'unauthorized' })
    expect(calls.map((c) => c.path)).toEqual(['/a', '/auth/refresh'])
    expect(signedIn()).toBe(false)
  })

  it('a refresh refused by the gate surfaces gate_required', async () => {
    refreshWith = () => json(401, { error: { type: 'gate_required', message: 'no' } })
    const { api } = await import('./api')
    await expect(api('/a')).rejects.toMatchObject({ type: 'gate_required' })
  })

  it('a retry that is still rejected throws without a second refresh', async () => {
    refreshWith = () => json(200, { data: { access_token: 'not-valid', expires_in: 900 } })
    const { api } = await import('./api')
    await expect(api('/a')).rejects.toMatchObject({ type: 'unauthorized' })
    expect(refreshes()).toBe(1)
  })

  it('a 401 from /auth/* never triggers a refresh', async () => {
    const { login } = await import('./api')
    await expect(login('a@b.c', 'password12')).rejects.toMatchObject({ type: 'invalid_credentials' })
    expect(refreshes()).toBe(0)
  })

  it('a 401 that lands after another caller refreshed retries without refreshing again', async () => {
    const { api } = await import('./api')
    await api('/a') // refresh #1 → tok-1
    valid = 'tok-2'
    // /slow is sent with tok-1 and its 401 lands only after /c has refreshed to tok-2
    await expect(Promise.all([api('/slow'), api('/c')])).resolves.toEqual(['/slow', '/c'])
    expect(refreshes()).toBe(2)
  })
})
