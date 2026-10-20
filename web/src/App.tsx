import { useCallback, useEffect, useState } from 'react'
import { ApiError, probe } from './api'
import InviteScreen from './InviteScreen'
import Session from './Session'

type Gate = 'checking' | 'invite' | 'closed' | 'in' | 'error'

export default function App() {
  const [gate, setGate] = useState<Gate>('checking')

  const check = useCallback(() => {
    probe()
      .then(() => setGate('in'))
      .catch((err) => {
        const type = err instanceof ApiError ? err.type : ''
        // unauthorized past the gate = AUTH_MODE=closed (production until M4 brings sign-in)
        setGate(type === 'gate_required' ? 'invite' : type === 'unauthorized' ? 'closed' : 'error')
      })
  }, [])

  useEffect(check, [check])

  return (
    <div className="min-h-dvh bg-neutral-950 text-neutral-100">
      {gate === 'checking' && <p className="p-4 text-neutral-400">Loading…</p>}
      {gate === 'invite' && <InviteScreen onEntered={check} />}
      {gate === 'closed' && (
        <p className="p-4">You are past the invite gate. Viewing opens when sign-in ships.</p>
      )}
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
