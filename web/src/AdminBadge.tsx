import { useEffect, useState } from 'react'
import { adminStats, type Stats } from './api'

// AdminBadge shows the HLS migration's progress (M7) to an admin, outside production. For
// anyone else /admin/stats is a 404 and the badge renders nothing. Fetched once per sign-in:
// a reload refreshes it. ponytail: no polling, add it if watching a batch live from the UI matters.
export default function AdminBadge() {
  const [stats, setStats] = useState<Stats | null>(null)
  useEffect(() => {
    adminStats().then(setStats).catch(() => {}) // a broken badge must not break the app
  }, [])
  if (!stats) return null
  const { mp4, hls } = stats.clips_by_source_type
  return (
    <p className="mr-auto text-xs text-neutral-500" title="clips by delivery format (admin)">
      mp4 {mp4} · hls {hls}
    </p>
  )
}
