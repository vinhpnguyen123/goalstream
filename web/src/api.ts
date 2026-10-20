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
