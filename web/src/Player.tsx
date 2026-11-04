import { useCallback, useEffect, useRef } from 'react'
import type Hls from 'hls.js'
import { playURL } from './api'
import { chooseEngine, HLS_CONFIG, loadHls, onMediaError } from './playback'

type Props = {
  clipId: string
  onEnded: () => void // the clip played to its end (watched_full)
  onReplay: () => void // played again after ending (replay_count)
  onFailed: () => void // second media failure: the session skips it silently
  onApiError: (err: unknown) => void // /play itself failed (network, gate, 404)
}

const HLS_MIME = 'application/vnd.apple.mpegurl'

// Player plays one clip by presigned URL — MP4, or HLS through hls.js / the browser's own HLS —
// and applies the error rules of plan §8.
export default function Player(props: Props) {
  const { clipId } = props
  const video = useRef<HTMLVideoElement>(null)
  const hls = useRef<Hls | null>(null)
  const st = useRef({ expiresAt: '', fellBack: false, ended: false, gen: 0 })
  // The latest callbacks, read at event time. Without this, a parent re-render hands in new
  // closures, `load` changes, and the effect below would restart the clip.
  const cb = useRef(props)
  useEffect(() => { cb.current = props })

  // handleError applies §8 to a media failure: re-presign an expired URL, fall back to mp4
  // once, then skip. It is called by <video> for src playback and by hls.js for its own fatal
  // errors. Through a ref so `load` can hand it to hls.js before it is declared.
  const onError = useRef(() => {})

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
    // hls.js (~575 kB) is loaded only when an hls URL arrives: production serves mp4 only
    // (Phase B is never deployed) and never downloads it.
    const HlsJS = p.type === 'hls' ? await loadHls() : null
    const v = video.current
    if (gen !== st.current.gen || !v) return
    st.current.expiresAt = p.expires_at
    hls.current?.destroy() // detaches from the element; the next source starts clean
    hls.current = null
    if (at > 0) v.addEventListener('loadedmetadata', () => { v.currentTime = at }, { once: true })
    if (HlsJS && chooseEngine(p.type, HlsJS.isSupported(), v.canPlayType(HLS_MIME) !== '') === 'hlsjs') {
      // Only the master is presigned — or, with HLS_BASE_URL (M8), an unsigned nginx URL; hls.js
      // fetches renditions and segments relative to it, unsigned, with the invite cookie as the
      // credential (M9). Quality switching happens here, in JS.
      const h = new HlsJS(HLS_CONFIG)
      hls.current = h
      h.on(HlsJS.Events.ERROR, (_event, data) => {
        if (data.fatal && h === hls.current) onError.current()
      })
      h.loadSource(p.url)
      h.attachMedia(v)
    } else {
      v.src = p.url // mp4, or native HLS (iOS Safari)
    }
    v.play().catch(() => {}) // autoplay with sound may be blocked (iOS): the user taps play
  }, [clipId])

  useEffect(() => {
    onError.current = () => {
      const v = video.current
      if (!v) return
      const action = onMediaError({ expiresAt: st.current.expiresAt, now: Date.now(), fellBack: st.current.fellBack })
      if (action === 'skip') return cb.current.onFailed()
      if (action === 'fallback') st.current.fellBack = true
      load(st.current.fellBack, v.currentTime)
    }
  }, [load])

  useEffect(() => {
    st.current = { expiresAt: '', fellBack: false, ended: false, gen: st.current.gen }
    load(false, 0)
    return () => { hls.current?.destroy(); hls.current = null } // clip changed or unmounted
  }, [load])

  return (
    <video
      ref={video}
      controls
      playsInline
      // hls.js reports its own failures (above); the element's error event while it is attached
      // would double-count one failure into a skip.
      onError={() => { if (!hls.current && video.current?.currentSrc) onError.current() }}
      onEnded={() => { st.current.ended = true; cb.current.onEnded() }}
      onPlay={() => { if (st.current.ended) { st.current.ended = false; cb.current.onReplay() } }}
      className="aspect-video w-full bg-black"
    />
  )
}
