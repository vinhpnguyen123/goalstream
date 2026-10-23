import { useCallback, useEffect, useRef, useState } from 'react'
import { ApiError, listRatings, putRating, type CurrentRating, type Rating } from './api'

const RATINGS: [Rating, string][] = [['loved', 'Loved'], ['okay', 'Okay'], ['not_for_me', 'Not for me']]

const message = (err: unknown) => (err instanceof ApiError ? err.message : 'Something went wrong.')

// History lists the user's current ratings, newest change first, and re-rates in place. A
// re-rated row keeps its position — it would otherwise jump from under the user's finger; the
// server order applies on the next visit. onLost handles gate_required / unauthorized.
export default function History({ onBack, onLost }: { onBack: () => void; onLost: (err: unknown) => boolean }) {
  const [items, setItems] = useState<CurrentRating[]>([])
  const [next, setNext] = useState<string | null>(null)
  const [state, setState] = useState<'loading' | 'ready' | 'more' | 'error'>('loading')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState<Record<string, boolean>>({})
  const [rowError, setRowError] = useState<Record<string, string>>({})

  // onLost is a new function on every parent render; read it through a ref so `load` stays
  // stable and the initial fetch runs once, not on every render.
  const lost = useRef(onLost)
  useEffect(() => { lost.current = onLost })

  const load = useCallback((cursor?: string) => {
    setState(cursor ? 'more' : 'loading')
    listRatings(cursor)
      .then((page) => {
        setItems((prev) => (cursor ? [...prev, ...page.items] : page.items))
        setNext(page.next_cursor)
        setState('ready')
      })
      .catch((err) => {
        if (lost.current(err)) return
        setError(message(err))
        setState('error')
      })
  }, [])

  useEffect(() => load(), [load])

  // The server answers before the row changes: a failed PUT leaves the old rating showing.
  async function rerate(clipId: string, response: Rating) {
    setBusy((b) => ({ ...b, [clipId]: true }))
    setRowError((e) => ({ ...e, [clipId]: '' }))
    try {
      await putRating(clipId, response)
      setItems((rows) => rows.map((r) => (r.clip_id === clipId ? { ...r, response } : r)))
    } catch (err) {
      if (lost.current(err)) return
      setRowError((e) => ({ ...e, [clipId]: message(err) }))
    } finally {
      setBusy((b) => ({ ...b, [clipId]: false }))
    }
  }

  return (
    <main className="mx-auto max-w-md space-y-4 px-4 py-6">
      <div className="flex items-center justify-between">
        <h1 className="text-2xl font-semibold">Rating history</h1>
        <button onClick={onBack} className="py-2 text-sm text-neutral-400">Back</button>
      </div>
      {state === 'loading' && <p className="text-neutral-400">Loading…</p>}
      {state === 'error' && (
        <div className="space-y-3">
          <p>{error}</p>
          <button onClick={() => load()} className="rounded-lg bg-neutral-800 px-4 py-2">Retry</button>
        </div>
      )}
      {state !== 'loading' && state !== 'error' && items.length === 0 && (
        <p className="text-neutral-400">No ratings yet.</p>
      )}
      <ul className="space-y-3">
        {items.map((r) => (
          <li key={r.clip_id} className="space-y-2 rounded-lg bg-neutral-900 p-3">
            <p className="font-medium">{r.title}</p>
            <div className="grid grid-cols-3 gap-2">
              {RATINGS.map(([value, label]) => (
                <button
                  key={value}
                  onClick={() => rerate(r.clip_id, value)}
                  disabled={busy[r.clip_id] || r.response === value}
                  aria-pressed={r.response === value}
                  className={`rounded-lg py-2 text-sm font-medium disabled:cursor-default ${
                    r.response === value ? 'bg-emerald-600' : 'bg-neutral-800 disabled:opacity-50'}`}
                >
                  {label}
                </button>
              ))}
            </div>
            {rowError[r.clip_id] && <p role="alert" className="text-sm text-red-400">{rowError[r.clip_id]}</p>}
          </li>
        ))}
      </ul>
      {next && state !== 'error' && (
        <button onClick={() => load(next)} disabled={state === 'more'}
          className="w-full rounded-lg bg-neutral-800 py-3 text-sm disabled:opacity-50">
          {state === 'more' ? 'Loading…' : 'Load more'}
        </button>
      )}
    </main>
  )
}
