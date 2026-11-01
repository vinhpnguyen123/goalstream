// Time-to-first-frame, stalls and HLS rung switches of the <video> player — paste into the
// DevTools console (Safari Web Inspector for an iPhone) on the running app, watch clips, then
// call ttffReport(). Not app code: the M4.5 baseline, re-run unchanged against HLS in Phase B.
//
// ttff_ms  = src assigned (loadstart), or the user's tap if autoplay was blocked → first `playing`
// stalls   = `waiting` events after the first frame that ended in `playing` — seeks and the swap
//            to the next clip excluded (so a stall escaped by skipping is not counted); stall_ms = total
// ttff_ms is null for a clip left before its first frame — not a failure
// rungs    = HLS only: the rendition each segment came from, in order ("360p×3 → 720p×9");
//            switches = how many times it changed. Empty for mp4.
// Logs clip ids only — never a URL: the master's is a presigned bearer credential.
//
// ttffClips([id, …]) makes GET /next serve exactly these clips, in order, then `exhausted` —
// so MP4 and HLS can be measured on the same clips. In this console session only.
(() => {
  // pasting again resets the runs instead of stacking a second set of listeners
  if (window.ttffReport) return (window.__ttff.length = 0), console.log('ttff: reset — watch clips, then run ttffReport()')
  const runs = (window.__ttff = [])
  let cur = null
  let lastPlay = '?' // clip id of the latest /play: with hls.js, currentSrc is a blob: URL
  const now = () => performance.now()
  // media events do not bubble, but a capturing listener on document still sees them
  const on = (type, f) =>
    document.addEventListener(type, (e) => e.target instanceof HTMLVideoElement && f(e.target), true)

  new PerformanceObserver((list) => {
    for (const e of list.getEntries()) {
      const play = e.name.match(/\/clips\/([0-9a-f-]{36})\/play/)
      if (play) lastPlay = play[1]
      const seg = e.name.match(/\/clips\/([0-9a-f-]{36})\/hls\/[^/]+\/(\d+p)\/seg_\d+\.ts/)
      if (seg && cur && cur.id === seg[1]) cur.rungs.push(seg[2])
    }
  }).observe({ type: 'resource' })

  on('loadstart', (v) => {
    // swapping src fires `waiting` on the outgoing clip: a stall still open at the next load was the switch
    if (cur && cur.stallAt != null) cur.stalls--, (cur.stallAt = null)
    const fromSrc = v.currentSrc.match(/\/clips\/([0-9a-f-]{36})\//)
    cur = { id: fromSrc ? fromSrc[1] : lastPlay, t0: now(), ttff: null, stalls: 0, stallMs: 0, stallAt: null, rungs: [] }
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

  // "360p,360p,720p" → "360p×2 → 720p×1"; switches = runs of equal rungs − 1
  const squash = (rs) => {
    const out = []
    for (const r of rs) out.length && out.at(-1)[0] === r ? out.at(-1)[1]++ : out.push([r, 1])
    return { path: out.map(([r, n]) => `${r}×${n}`).join(' → '), switches: Math.max(0, out.length - 1) }
  }

  window.ttffReport = () => {
    console.table(runs.map((r) => {
      const s = squash(r.rungs)
      return { clip: r.id.slice(0, 8), ttff_ms: r.ttff, stalls: r.stalls, stall_ms: Math.round(r.stallMs), rungs: s.path, switches: s.switches }
    }))
    const t = runs.map((r) => r.ttff).filter((x) => x != null).sort((a, b) => a - b)
    const pick = (q) => t[Math.min(t.length - 1, Math.floor(q * t.length))]
    console.log(`n=${t.length} p50=${pick(0.5)} ms p95=${pick(0.95)} ms max=${t[t.length - 1]} ms ` +
      `stalls=${runs.reduce((s, r) => s + r.stalls, 0)} stall_ms=${Math.round(runs.reduce((s, r) => s + r.stallMs, 0))} ` +
      `switches=${runs.reduce((s, r) => s + squash(r.rungs).switches, 0)}`)
  }

  window.ttffClips = (ids) => {
    const queue = [...ids]
    const real = window.fetch
    window.fetch = (input, init) => {
      const u = typeof input === 'string' ? input : input.url
      if (!/\/sessions\/[^/]+\/next/.test(u)) return real(input, init)
      const id = queue.shift()
      const data = id
        ? { clip: { id, title: 'clip ' + id.slice(0, 8), match_minute: 0, event_type: 'Goal', source_type: '' }, mode: 'fixed', reason: 'ttffClips', exhausted: false }
        : { clip: null, mode: 'fixed', reason: 'ttffClips', exhausted: true }
      return Promise.resolve(new Response(JSON.stringify({ data }), { status: 200, headers: { 'Content-Type': 'application/json' } }))
    }
    console.log(`ttff: /next will serve ${ids.length} fixed clips, then exhausted`)
  }
  console.log('ttff: recording — watch clips, then run ttffReport()')
})()
