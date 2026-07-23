import { useState } from 'react'
import { InsightFeed } from '../components/InsightFeed'

/**
 * The full proactive feed. Shows every active insight, with a toggle to include
 * ones you've dismissed. Not AI-gated — the feed is deterministic and exists
 * with or without an AI key.
 */
export function Insights() {
  const [showDismissed, setShowDismissed] = useState(false)

  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-baseline justify-between gap-3">
        <div>
          <h1 className="text-2xl font-semibold">Insights</h1>
          <p className="mt-1 text-mist-300">
            Things worth a look — spending changes, budget pace, recurring charges.
          </p>
        </div>
        <label className="flex items-center gap-2 text-sm text-mist-300">
          <input
            type="checkbox"
            className="accent-arcane-500"
            checked={showDismissed}
            onChange={(e) => setShowDismissed(e.target.checked)}
          />
          Show dismissed
        </label>
      </div>

      <section className="glass p-6">
        <InsightFeed variant="page" state={showDismissed ? 'all' : 'unread'} />
      </section>
    </div>
  )
}
