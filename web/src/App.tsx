import { useCallback, useEffect, useState } from 'react'
import { ApiError, logout, probe, signedIn } from './api'
import AuthScreen from './AuthScreen'
import InviteScreen from './InviteScreen'
import Session from './Session'

type Gate = 'checking' | 'invite' | 'login' | 'in' | 'error'

export default function App() {
  const [gate, setGate] = useState<Gate>('checking')
  const [signOutError, setSignOutError] = useState('')

  const check = useCallback(() => {
    probe()
      .then(() => setGate('in'))
      .catch((err) => {
        const type = err instanceof ApiError ? err.type : ''
        // unauthorized here means the refresh already failed (CONVENTIONS § Frontend)
        setGate(type === 'gate_required' ? 'invite' : type === 'unauthorized' ? 'login' : 'error')
      })
  }, [])

  useEffect(check, [check])

  async function signOut() {
    setSignOutError('')
    try {
      await logout()
      setGate('login')
    } catch {
      setSignOutError('Sign out failed. Try again.') // still signed in: the refresh cookie is live
    }
  }

  return (
    <div className="min-h-dvh bg-neutral-950 text-neutral-100">
      {gate === 'checking' && <p className="p-4 text-neutral-400">Loading…</p>}
      {gate === 'invite' && <InviteScreen onEntered={check} />}
      {gate === 'login' && <AuthScreen onSignedIn={() => setGate('in')} />}
      {gate === 'error' && (
        <div className="space-y-3 p-4">
          <p>Could not reach GoalStream.</p>
          <button onClick={check} className="rounded-lg bg-neutral-800 px-4 py-2">Retry</button>
        </div>
      )}
      {gate === 'in' && (
        <>
          {/* AUTH_MODE=dev issues no token, so there is nothing to sign out of */}
          {signedIn() && (
            <header className="flex items-center justify-end gap-3 px-4 pt-2">
              {signOutError && <p role="alert" className="text-sm text-red-400">{signOutError}</p>}
              <button onClick={signOut} className="py-2 text-sm text-neutral-400">Sign out</button>
            </header>
          )}
          <Session onGateLost={() => setGate('invite')} onSignedOut={() => setGate('login')} />
        </>
      )}
    </div>
  )
}
