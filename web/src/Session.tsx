import { useRef, useState } from 'react'
import {
  ApiError, endSession, getSummary, nextClip, sendFeedback, startSession,
  type Clip, type Rating, type Summary as S,
} from './api'
import { addExclude } from './playback'
import Player from './Player'
import Summary from './Summary'

type Phase =
  | { k: 'idle' }
  | { k: 'loading' }
  | { k: 'watching'; clip: Clip }
  | { k: 'summary'; summary: S }
  | { k: 'error'; message: string }

const RATINGS: [Rating, string][] = [['loved', 'Loved it'], ['okay', 'Okay'], ['not_for_me', 'Not for me']]

// Session is the viewing loop: start → next → play → rate or skip → next … → summary.
// The exclude list (served but not rated) lives here, in component state (CONVENTIONS).
export default function Session({ onGateLost, onSignedOut }: { onGateLost: () => void; onSignedOut: () => void }) {
  const [phase, setPhase] = useState<Phase>({ k: 'idle' })
  const [exclude, setExclude] = useState<string[]>([])
  const session = useRef('')
  const stats = useRef({ watchedFull: false, replays: 0 })
  const lastStep = useRef<() => Promise<void>>(async () => {})

  // lost handles the two errors no Retry can fix: gate_required → invite screen; unauthorized
  // (the refresh already failed) → sign-in screen.
  function lost(err: unknown) {
    if (!(err instanceof ApiError)) return false
    if (err.type === 'gate_required') onGateLost()
    else if (err.type === 'unauthorized') onSignedOut()
    else return false
    return true
  }

  // run wraps every API step: lost() first; anything else → an error with a Retry that runs the
  // same step again. A media failure never lands here — the Player skips those silently.
  // Retrying a rate after its feedback landed is safe: feedback is idempotent.
  function run(step: () => Promise<void>) {
    lastStep.current = step
    setPhase({ k: 'loading' })
    step().catch((err) => {
      if (lost(err)) return
      setPhase({ k: 'error', message: err instanceof ApiError ? err.message : 'Something went wrong.' })
    })
  }

  async function advance(excl: string[]) {
    const pick = await nextClip(session.current, excl)
    if (pick.exhausted || !pick.clip) return finish()
    stats.current = { watchedFull: false, replays: 0 }
    setPhase({ k: 'watching', clip: pick.clip })
  }

  async function finish() {
    await endSession(session.current)
    setPhase({ k: 'summary', summary: await getSummary(session.current) })
  }

  const start = () => run(async () => {
    session.current = (await startSession()).id
    setExclude([])
    await advance([])
  })

  const rate = (clip: Clip, response: Rating) => run(async () => {
    await sendFeedback({
      session_id: session.current, clip_id: clip.id, response,
      replay_count: stats.current.replays, watched_full: stats.current.watchedFull,
    })
    await advance(exclude) // rated clips are seen: the server already leaves them out
  })

  function skip(clip: Clip) {
    const excl = addExclude(exclude, clip.id)
    setExclude(excl)
    run(() => advance(excl))
  }

  // /play itself failed (not the media). Retry remounts the Player for the same clip.
  function playFailed(clip: Clip, err: unknown) {
    if (lost(err)) return
    lastStep.current = async () => setPhase({ k: 'watching', clip })
    setPhase({ k: 'error', message: err instanceof ApiError ? err.message : 'Something went wrong.' })
  }

  switch (phase.k) {
    case 'idle':
      return (
        <main className="flex min-h-dvh flex-col items-center justify-center gap-6 px-4">
          <h1 className="text-2xl font-semibold">GoalStream</h1>
          <button onClick={start} className="w-full max-w-sm rounded-lg bg-emerald-600 py-3 font-medium">Start watching</button>
        </main>
      )
    case 'loading':
      return <p className="p-4 text-neutral-400">Loading…</p>
    case 'error':
      return (
        <div className="space-y-3 p-4">
          <p>{phase.message}</p>
          <button onClick={() => run(lastStep.current)} className="rounded-lg bg-neutral-800 px-4 py-2">Retry</button>
        </div>
      )
    case 'summary':
      return <Summary summary={phase.summary} onNew={() => setPhase({ k: 'idle' })} />
    case 'watching': {
      const { clip } = phase
      return (
        <main className="mx-auto max-w-md">
          <Player
            key={clip.id}
            clipId={clip.id}
            onEnded={() => { stats.current.watchedFull = true }}
            onReplay={() => { stats.current.replays++ }}
            onFailed={() => skip(clip)}
            onApiError={(err) => playFailed(clip, err)}
          />
          <div className="space-y-4 px-4 py-4">
            <h2 className="text-lg font-medium">{clip.title}</h2>
            <div className="grid grid-cols-3 gap-2">
              {RATINGS.map(([r, label]) => (
                <button key={r} onClick={() => rate(clip, r)} className="rounded-lg bg-neutral-800 py-3 text-sm font-medium">
                  {label}
                </button>
              ))}
            </div>
            <div className="flex justify-between text-sm">
              <button onClick={() => skip(clip)} className="px-2 py-2 text-neutral-400">Skip</button>
              <button onClick={() => run(finish)} className="px-2 py-2 text-neutral-400">Done</button>
            </div>
          </div>
        </main>
      )
    }
  }
}
