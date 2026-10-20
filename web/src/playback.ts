// Player decisions as pure functions, so they are tested without a browser (plan §8).

export type ErrorAction = 'represign' | 'fallback' | 'skip'

// onMediaError decides what a <video> error means, in order:
//   the presigned URL has expired (paused past PRESIGN_TTL, then seeked → 403)
//                         → 'represign': fetch /play again and resume; not a failure
//   first real failure    → 'fallback': /play?format=mp4 once (L3, the structural fallback)
//   second real failure   → 'skip': move on silently, never a raw error
export function onMediaError(s: { expiresAt: string; now: number; fellBack: boolean }): ErrorAction {
  if (s.now >= Date.parse(s.expiresAt)) return 'represign'
  return s.fellBack ? 'skip' : 'fallback'
}

export const MAX_EXCLUDE = 100 // GET /next rejects more (docs/API.md)

// addExclude returns a new list with id appended. Past MAX_EXCLUDE the oldest entry goes: a
// clip skipped 100 clips ago may come back, which beats ending the session.
export function addExclude(list: string[], id: string): string[] {
  if (list.includes(id)) return list
  return [...list, id].slice(-MAX_EXCLUDE)
}
