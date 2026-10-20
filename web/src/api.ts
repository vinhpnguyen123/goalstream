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

// api returns the envelope's `data`, or throws ApiError. The gs_invite cookie is HttpOnly and
// same-origin: the browser sends it, JS never sees it.
export async function api<T>(path: string, init: RequestInit = {}): Promise<T> {
  let res: Response
  try {
    res = await fetch('/api/v1' + path, {
      ...init,
      headers: { 'Content-Type': 'application/json', ...init.headers },
    })
  } catch {
    throw new ApiError(0, 'network', 'Could not reach the server.')
  }
  if (res.status === 204) return undefined as T // POST /gate: success carries no body
  const body = await res.json().catch(() => null)
  if (!res.ok || body === null) {
    throw new ApiError(res.status, body?.error?.type ?? 'network', body?.error?.message ?? 'Something went wrong.')
  }
  return body.data as T
}

export const enterInvite = (code: string) =>
  api<unknown>('/gate', { method: 'POST', body: JSON.stringify({ code }) })

// probe answers whether the gate lets us through. ponytail: GET /clips?limit=1 doubles as the
// check — the endpoint set is closed, so no /me until M4 brings one.
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
