// The fetch layer (docs/CONVENTIONS.md § Frontend). Components never call fetch directly.
// Every path is relative to this origin — the dev proxy or the Vercel rewrite forwards it.

export class ApiError extends Error {
  status: number
  type: string // a docs/API.md error type, or 'network' when no envelope came back

  constructor(status: number, type: string, message: string) {
    super(message)
    this.status = status
    this.type = type
  }
}

// The access token lives in memory only (CONVENTIONS): a reload drops it, and the HttpOnly
// gs_refresh cookie gets a new one on the first 401. AUTH_MODE=dev never issues one.
let accessToken: string | null = null
let refreshing: Promise<void> | null = null

export const signedIn = () => accessToken !== null

// call is one request: the envelope's `data`, or ApiError. The gs_invite and gs_refresh cookies
// are HttpOnly and same-origin: the browser sends them, JS never sees them.
async function call<T>(path: string, init: RequestInit): Promise<T> {
  let res: Response
  try {
    res = await fetch('/api/v1' + path, {
      ...init,
      headers: {
        'Content-Type': 'application/json',
        ...(accessToken && { Authorization: 'Bearer ' + accessToken }),
        ...init.headers,
      },
    })
  } catch {
    throw new ApiError(0, 'network', 'Could not reach the server.')
  }
  if (res.status === 204) return undefined as T // POST /gate, /auth/logout: no body
  const body = await res.json().catch(() => null)
  if (!res.ok || body === null) {
    throw new ApiError(res.status, body?.error?.type ?? 'network', body?.error?.message ?? 'Something went wrong.')
  }
  return body.data as T
}

// refresh is single-flight: every caller that hits a 401 while one is in flight shares it, so
// two tabs' worth of requests (or StrictMode's double mount) spend one rotation, not several.
// A failed refresh rethrows its own error, so gate_required still reaches the invite screen.
function refresh(): Promise<void> {
  refreshing ??= call<Token>('/auth/refresh', { method: 'POST' })
    .then((t) => { accessToken = t.access_token })
    .catch((err) => { accessToken = null; throw err })
    .finally(() => { refreshing = null })
  return refreshing
}

// api is call plus auth: an `unauthorized` outside /auth/* refreshes once and retries once.
// The retry never refreshes again, so a token the server keeps rejecting cannot loop.
export async function api<T>(path: string, init: RequestInit = {}): Promise<T> {
  const sent = accessToken
  try {
    return await call<T>(path, init)
  } catch (err) {
    if (!(err instanceof ApiError && err.type === 'unauthorized') || path.startsWith('/auth/')) throw err
  }
  // Another caller already refreshed while this request was in flight: just retry with its token.
  if (accessToken === sent) await refresh()
  return call<T>(path, init)
}

type Token = { access_token: string; expires_in: number }

export async function login(email: string, password: string) {
  accessToken = (await api<Token>('/auth/login', { method: 'POST', body: JSON.stringify({ email, password }) })).access_token
}

export const register = (email: string, password: string, display_name?: string) =>
  api<unknown>('/auth/register', { method: 'POST', body: JSON.stringify({ email, password, display_name }) })

// logout is done only when the server confirms (204): until then the HttpOnly gs_refresh cookie
// is still live, and a reload would sign straight back in. A failure throws and keeps the token.
export async function logout() {
  await api<unknown>('/auth/logout', { method: 'POST' })
  accessToken = null
}

export const enterInvite = (code: string) =>
  api<unknown>('/gate', { method: 'POST', body: JSON.stringify({ code }) })

// probe answers whether the gate and auth let us through; on a fresh page load its 401 is what
// triggers the refresh. ponytail: GET /clips?limit=1 doubles as the check — the endpoint set is
// closed, there is no /me.
export const probe = () => api<unknown>('/clips?limit=1')

export type Clip = { id: string; title: string; match_minute: number; event_type: string; source_type: string }
export type Pick = { clip: Clip | null; mode: string; reason: string; exhausted: boolean }
export type PlayURL = { type: 'mp4' | 'hls'; url: string; expires_at: string }
export type Rating = 'loved' | 'okay' | 'not_for_me'
export type Summary = {
  rated: number; loved: number; okay: number; not_for_me: number
  started_at: string; ended_at: string | null
}

export const startSession = () => api<{ id: string; started_at: string }>('/sessions', { method: 'POST' })
export const endSession = (id: string) => api<unknown>(`/sessions/${id}/end`, { method: 'POST' })
export const getSummary = (id: string) => api<Summary>(`/sessions/${id}/summary`)

export const nextClip = (sessionId: string, exclude: string[]) =>
  api<Pick>(`/sessions/${sessionId}/next` + (exclude.length ? '?exclude=' + exclude.join(',') : ''))

// playURL returns a presigned URL: the browser then fetches the bytes from storage directly.
export const playURL = (clipId: string, mp4 = false) =>
  api<PlayURL>(`/clips/${clipId}/play` + (mp4 ? '?format=mp4' : ''))

export const sendFeedback = (b: {
  session_id: string; clip_id: string; response: Rating; replay_count: number; watched_full: boolean
}) => api<{ changed: boolean; previous?: Rating | null }>('/feedback', { method: 'POST', body: JSON.stringify(b) })

export type CurrentRating = { clip_id: string; title: string; response: Rating; rated_at: string }

export const listRatings = (cursor?: string) =>
  api<{ items: CurrentRating[]; next_cursor: string | null }>(
    '/ratings?limit=20' + (cursor ? '&cursor=' + encodeURIComponent(cursor) : ''))

// putRating re-rates from the history screen, outside any session. Idempotent by state.
export const putRating = (clipId: string, response: Rating) =>
  api<{ changed: boolean; previous?: Rating | null }>(`/ratings/${clipId}`, {
    method: 'PUT', body: JSON.stringify({ response }),
  })

export type Stats = { clips_by_source_type: { mp4: number; hls: number }; jobs_by_status: Record<string, number> }

// adminStats is null for anyone who is not an admin, and in production: both answer 404, by
// design indistinguishable (docs/API.md). Any other error still throws.
export async function adminStats(): Promise<Stats | null> {
  try {
    return await api<Stats>('/admin/stats')
  } catch (err) {
    if (err instanceof ApiError && err.status === 404) return null
    throw err
  }
}
