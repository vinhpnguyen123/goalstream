import { describe, expect, it } from 'vitest'
import { addExclude, MAX_EXCLUDE, onMediaError } from './playback'

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
