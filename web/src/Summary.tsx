import type { Summary as S } from './api'

export default function Summary({ summary, onNew }: { summary: S; onNew: () => void }) {
  const rows: [string, number][] = [
    ['Rated', summary.rated], ['Loved', summary.loved], ['Okay', summary.okay], ['Not for me', summary.not_for_me],
  ]
  return (
    <main className="mx-auto max-w-md space-y-6 px-4 py-8">
      <h1 className="text-2xl font-semibold">Session summary</h1>
      <dl className="grid grid-cols-2 gap-3">
        {rows.map(([label, n]) => (
          <div key={label} className="rounded-lg bg-neutral-900 p-4">
            <dt className="text-sm text-neutral-400">{label}</dt>
            <dd className="text-3xl font-semibold">{n}</dd>
          </div>
        ))}
      </dl>
      <button onClick={onNew} className="w-full rounded-lg bg-emerald-600 py-3 font-medium">New session</button>
    </main>
  )
}
