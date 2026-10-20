import { useState, type FormEvent } from 'react'
import { ApiError, enterInvite } from './api'

export default function InviteScreen({ onEntered }: { onEntered: () => void }) {
  const [code, setCode] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  async function submit(e: FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      await enterInvite(code.trim())
      onEntered()
    } catch (err) {
      setError(err instanceof ApiError && err.type === 'rate_limited'
        ? 'Too many attempts. Wait a minute and try again.'
        : err instanceof ApiError ? err.message : 'Something went wrong.')
    } finally {
      setBusy(false)
    }
  }

  return (
    <main className="flex min-h-dvh items-center justify-center px-4">
      <form onSubmit={submit} className="w-full max-w-sm space-y-4">
        <h1 className="text-2xl font-semibold">GoalStream</h1>
        <label className="block space-y-1">
          <span className="text-sm text-neutral-400">Invite code</span>
          <input
            value={code}
            onChange={(e) => setCode(e.target.value)}
            autoComplete="off"
            autoCapitalize="off"
            required
            className="w-full rounded-lg border border-neutral-700 bg-neutral-900 px-3 py-3 text-base"
          />
        </label>
        {error && <p role="alert" className="text-sm text-red-400">{error}</p>}
        <button
          disabled={busy || !code.trim()}
          className="w-full rounded-lg bg-emerald-600 py-3 font-medium disabled:opacity-50"
        >
          {busy ? 'Checking…' : 'Enter'}
        </button>
      </form>
    </main>
  )
}
