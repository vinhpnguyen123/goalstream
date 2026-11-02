import { describe, expect, it } from 'vitest'
import { addExclude, chooseEngine, loadHls, MAX_EXCLUDE, onMediaError } from './playback'

describe('onMediaError', () => {
  const exp = '2026-10-20T10:00:00Z'
  const at = Date.parse(exp)

  it('re-presigns an expired URL, whether or not it already fell back', () => {
    expect(onMediaError({ expiresAt: exp, now: at, fellBack: false })).toBe('represign')
    expect(onMediaError({ expiresAt: exp, now: at + 1, fellBack: true })).toBe('represign')
  })
  it('falls back to mp4 once on a real failure', () => {
    expect(onMediaError({ expiresAt: exp, now: at - 1, fellBack: false })).toBe('fallback')
  })
  it('skips on the second real failure', () => {
    expect(onMediaError({ expiresAt: exp, now: at - 1, fellBack: true })).toBe('skip')
  })
})

describe('addExclude', () => {
  it('appends without mutating and ignores duplicates', () => {
    const list = ['a']
    expect(addExclude(list, 'b')).toEqual(['a', 'b'])
    expect(list).toEqual(['a'])
    expect(addExclude(['a', 'b'], 'a')).toEqual(['a', 'b'])
  })
  it('drops the oldest past the cap', () => {
    const full = Array.from({ length: MAX_EXCLUDE }, (_, i) => String(i))
    const next = addExclude(full, 'new')
    expect(next).toHaveLength(MAX_EXCLUDE)
    expect(next[0]).toBe('1')
    expect(next.at(-1)).toBe('new')
  })
})

describe('chooseEngine', () => {
  it('plays mp4 by src whatever the browser supports', () => {
    expect(chooseEngine('mp4', true, true)).toBe('src')
    expect(chooseEngine('mp4', false, false)).toBe('src')
  })
  it('prefers hls.js when MSE is available, even if the browser plays HLS natively', () => {
    expect(chooseEngine('hls', true, true)).toBe('hlsjs')
    expect(chooseEngine('hls', true, false)).toBe('hlsjs')
  })
  it('uses native HLS without MSE (iOS Safari before 17.1)', () => {
    expect(chooseEngine('hls', false, true)).toBe('native')
  })
  it('hands hls to src when nothing plays it, so the error path falls back to mp4', () => {
    expect(chooseEngine('hls', false, false)).toBe('src')
  })
})

describe('loadHls', () => {
  it('returns null instead of rejecting when the chunk fails to download', async () => {
    await expect(loadHls(() => Promise.reject(new TypeError('Failed to fetch dynamically imported module')))).resolves.toBeNull()
  })
  it('then hls goes native where the browser plays it, else to src — whose error falls back to mp4', () => {
    expect(chooseEngine('hls', false, true)).toBe('native')
    expect(chooseEngine('hls', false, false)).toBe('src')
    expect(onMediaError({ expiresAt: '2999-01-01T00:00:00Z', now: Date.now(), fellBack: false })).toBe('fallback')
  })
  it('returns the class when the chunk loads', async () => {
    const Fake = class {} as never
    await expect(loadHls(() => Promise.resolve({ default: Fake }))).resolves.toBe(Fake)
  })
})
