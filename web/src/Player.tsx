import { useCallback, useEffect, useRef } from 'react'
import { playURL } from './api'
import { onMediaError } from './playback'

type Props = {
  clipId: string
  onEnded: () => void // the clip played to its end (watched_full)
  onReplay: () => void // played again after ending (replay_count)
  onFailed: () => void // second media failure: the session skips it silently
  onApiError: (err: unknown) => void // /play itself failed (network, gate, 404)
}

// Player plays one clip by presigned URL and applies the error rules of plan §8.
export default function Player(props: Props) {
  const { clipId } = props
  const video = useRef<HTMLVideoElement>(null)
  const st = useRef({ expiresAt: '', fellBack: false, ended: false, gen: 0 })
  // The latest callbacks, read at event time. Without this, a parent re-render hands in new
  // closures, `load` changes, and the effect below would restart the clip.
  const cb = useRef(props)
  useEffect(() => { cb.current = props })

  // load fetches a fresh URL and resumes at `at`. gen drops a response that arrives after the
  // clip changed, so a slow /play never swaps the wrong clip in.
  const load = useCallback(async (mp4: boolean, at: number) => {
    const gen = ++st.current.gen
    let p
    try {
      p = await playURL(clipId, mp4)
    } catch (err) {
      if (gen === st.current.gen) cb.current.onApiError(err)
      return
    }
    const v = video.current
    if (gen !== st.current.gen || !v) return
    st.current.expiresAt = p.expires_at
    // ponytail: M2 serves only mp4; M5 adds hls.js / native HLS on p.type === 'hls'.
    v.src = p.url
    if (at > 0) v.addEventListener('loadedmetadata', () => { v.currentTime = at }, { once: true })
    v.play().catch(() => {}) // autoplay with sound may be blocked (iOS): the user taps play
  }, [clipId])

  useEffect(() => {
    st.current = { expiresAt: '', fellBack: false, ended: false, gen: st.current.gen }
    load(false, 0)
  }, [load])

  function handleError() {
    const v = video.current
    if (!v?.currentSrc) return
    const action = onMediaError({ expiresAt: st.current.expiresAt, now: Date.now(), fellBack: st.current.fellBack })
    if (action === 'skip') return cb.current.onFailed()
    if (action === 'fallback') st.current.fellBack = true
    load(st.current.fellBack, v.currentTime)
  }

  return (
    <video
      ref={video}
      controls
      playsInline
      onError={handleError}
      onEnded={() => { st.current.ended = true; cb.current.onEnded() }}
      onPlay={() => { if (st.current.ended) { st.current.ended = false; cb.current.onReplay() } }}
      className="aspect-video w-full bg-black"
    />
  )
}
