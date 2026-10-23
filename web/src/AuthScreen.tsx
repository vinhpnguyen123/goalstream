import { useState, type FormEvent } from 'react'
import { ApiError, login, register } from './api'

const MESSAGES: Record<string, string> = {
  invalid_credentials: 'Wrong email or password.',
  email_taken: 'An account with this email already exists.',
  rate_limited: 'Too many attempts. Wait a minute and try again.',
}

// AuthScreen is sign-in and registration in one form. Registering does not log in on the
// server (docs/API.md), so the client logs in right after with the same credentials.
export default function AuthScreen({ onSignedIn }: { onSignedIn: () => void }) {
  const [mode, setMode] = useState<'login' | 'register'>('login')
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [name, setName] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  async function submit(e: FormEvent) {
    e.preventDefault()
    setBusy(true)
    setError('')
    try {
      if (mode === 'register') {
        await register(email.trim(), password, name.trim() || undefined)
        setMode('login') // the account exists now: if login fails, a resubmit must not re-register
      }
      await login(email.trim(), password)
      onSignedIn()
    } catch (err) {
      setError(err instanceof ApiError ? MESSAGES[err.type] ?? err.message : 'Something went wrong.')
    } finally {
      setBusy(false)
    }
  }

  const input = 'w-full rounded-lg border border-neutral-700 bg-neutral-900 px-3 py-3 text-base'
  return (
    <main className="flex min-h-dvh items-center justify-center px-4">
      <form onSubmit={submit} className="w-full max-w-sm space-y-4">
        <h1 className="text-2xl font-semibold">{mode === 'login' ? 'Sign in' : 'Create account'}</h1>
        <label className="block space-y-1">
          <span className="text-sm text-neutral-400">Email</span>
          <input type="email" value={email} onChange={(e) => setEmail(e.target.value)}
            autoComplete="email" autoCapitalize="off" required maxLength={254} className={input} />
        </label>
        <label className="block space-y-1">
          <span className="text-sm text-neutral-400">Password{mode === 'register' && ' (10–128 characters)'}</span>
          <input type="password" value={password} onChange={(e) => setPassword(e.target.value)}
            autoComplete={mode === 'login' ? 'current-password' : 'new-password'}
            required minLength={10} maxLength={128} className={input} />
        </label>
        {mode === 'register' && (
          <label className="block space-y-1">
            <span className="text-sm text-neutral-400">Display name (optional)</span>
            <input value={name} onChange={(e) => setName(e.target.value)} autoComplete="nickname" className={input} />
          </label>
        )}
        {error && <p role="alert" className="text-sm text-red-400">{error}</p>}
        <button disabled={busy} className="w-full rounded-lg bg-emerald-600 py-3 font-medium disabled:opacity-50">
          {busy ? 'Working…' : mode === 'login' ? 'Sign in' : 'Create account'}
        </button>
        <button type="button" onClick={() => { setMode(mode === 'login' ? 'register' : 'login'); setError('') }}
          className="w-full py-2 text-sm text-neutral-400">
          {mode === 'login' ? 'No account? Create one' : 'Have an account? Sign in'}
        </button>
      </form>
    </main>
  )
}
