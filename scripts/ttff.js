// Time-to-first-frame and stalls of the <video> player — paste into the DevTools console
// (Safari Web Inspector for an iPhone) on the running app, watch clips, then call ttffReport().
// Not app code: the M4.5 baseline, re-run unchanged against HLS in Phase B.
//
// ttff_ms  = src assigned (loadstart), or the user's tap if autoplay was blocked → first `playing`
// stalls   = `waiting` events after the first frame that ended in `playing` — seeks and the swap
//            to the next clip excluded (so a stall escaped by skipping is not counted); stall_ms = total
// ttff_ms is null for a clip left before its first frame — not a failure
// Logs the clip id only — never the URL, which is a presigned bearer credential.
(() => {
  // pasting again resets the runs instead of stacking a second set of listeners
  if (window.ttffReport) return (window.__ttff.length = 0), console.log('ttff: reset — watch clips, then run ttffReport()')
  const runs = (window.__ttff = [])
  let cur = null
  const now = () => performance.now()
  // media events do not bubble, but a capturing listener on document still sees them
  const on = (type, f) =>
    document.addEventListener(type, (e) => e.target instanceof HTMLVideoElement && f(e.target), true)

  on('loadstart', (v) => {
    // swapping src fires `waiting` on the outgoing clip: a stall still open at the next load was the switch
    if (cur && cur.stallAt != null) cur.stalls--, (cur.stallAt = null)
    const id = new URL(v.currentSrc).pathname.split('/').slice(-2, -1)[0] || '?'
    cur = { id, t0: now(), ttff: null, stalls: 0, stallMs: 0, stallAt: null }
    runs.push(cur)
  })
  on('play', () => {
    if (cur && cur.ttff == null) cur.t0 = Math.max(cur.t0, now()) // blocked autoplay: count from the tap
  })
  on('playing', () => {
    if (!cur) return
    if (cur.ttff == null) cur.ttff = Math.round(now() - cur.t0)
    if (cur.stallAt != null) (cur.stallMs += now() - cur.stallAt), (cur.stallAt = null)
    cur.seek = false
  })
  // a seek is not a stall: `waiting` can fire after `seeked`, so the flag lasts until playback resumes
  on('seeking', () => cur && (cur.seek = true))
  on('waiting', () => {
    if (cur && cur.ttff != null && !cur.seek) cur.stalls++, (cur.stallAt = now())
  })

  window.ttffReport = () => {
    console.table(runs.map((r) => ({ clip: r.id.slice(0, 8), ttff_ms: r.ttff, stalls: r.stalls, stall_ms: Math.round(r.stallMs) })))
    const t = runs.map((r) => r.ttff).filter((x) => x != null).sort((a, b) => a - b)
    const pick = (q) => t[Math.min(t.length - 1, Math.floor(q * t.length))]
    console.log(`n=${t.length} p50=${pick(0.5)} ms p95=${pick(0.95)} ms max=${t[t.length - 1]} ms ` +
      `stalls=${runs.reduce((s, r) => s + r.stalls, 0)}`)
  }
  console.log('ttff: recording — watch clips, then run ttffReport()')
})()
