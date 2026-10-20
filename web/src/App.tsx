import { useCallback, useEffect, useState } from 'react'
import { ApiError, probe } from './api'
import InviteScreen from './InviteScreen'
import Session from './Session'

type Gate = 'checking' | 'invite' | 'in' | 'error'

export default function App() {
  const [gate, setGate] = useState<Gate>('checking')

  const check = useCallback(() => {
    probe()
      .then(() => setGate('in'))
      .catch((err) => setGate(err instanceof ApiError && err.type === 'gate_required' ? 'invite' : 'error'))
  }, [])

  useEffect(check, [check])

  return (
    <div className="min-h-dvh bg-neutral-950 text-neutral-100">
      {gate === 'checking' && <p className="p-4 text-neutral-400">Loading…</p>}
      {gate === 'invite' && <InviteScreen onEntered={check} />}
      {gate === 'error' && (
        <div className="space-y-3 p-4">
          <p>Could not reach GoalStream.</p>
          <button onClick={check} className="rounded-lg bg-neutral-800 px-4 py-2">Retry</button>
        </div>
      )}
      {gate === 'in' && <Session onGateLost={() => setGate('invite')} />}
    </div>
  )
}
