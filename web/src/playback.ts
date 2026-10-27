// Player decisions as pure functions, so they are tested without a browser (plan §8).

import type Hls from 'hls.js'

export type ErrorAction = 'represign' | 'fallback' | 'skip'

// loadHls fetches the hls.js chunk on demand. A failed download (network blip, a deploy that
// renamed the chunk under an open tab) resolves to null — "no hls.js" — rather than rejecting:
// chooseEngine then picks native HLS, or src, whose media error falls back to mp4. Never a dead
// end with nothing attached to the <video>.
export async function loadHls(importer: () => Promise<{ default: typeof Hls }> = () => import('hls.js')): Promise<typeof Hls | null> {
  try {
    return (await importer()).default
  } catch {
    return null
  }
}

export type Engine = 'hlsjs' | 'native' | 'src'

// chooseEngine picks how a /play URL is played (plan §8), in this order:
//   hls and Media Source Extensions available → hls.js (adaptive switching in JS)
//   hls and the browser plays HLS itself      → native: video.src = master (iOS Safari: no MSE
//                                                before 17.1)
//   otherwise (mp4)                            → video.src
// An hls URL in a browser with neither is still handed to video.src: it errors, and the error
// rules below fall back to mp4 — never a dead end.
export function chooseEngine(type: 'mp4' | 'hls', mseHls: boolean, nativeHls: boolean): Engine {
  if (type !== 'hls') return 'src'
  if (mseHls) return 'hlsjs'
  return nativeHls ? 'native' : 'src'
}

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
