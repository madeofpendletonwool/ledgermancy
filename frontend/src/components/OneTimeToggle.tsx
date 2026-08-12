/**
 * The "Hide one-time charges" lens over the trailing spending charts.
 *
 * One toggle governs the three trend-fed charts and the two heatmap-fed charts
 * on /spending — not five — because they all answer the same trailing-twelve
 * question and a reader wants to switch lenses once, not five times. It lives
 * beside `RealToggle` at the top of those views.
 *
 * The label is deliberately a lens, not a correction: the money did leave, and
 * a one-time charge (a loan payoff, an annual true-up) is real spend in the
 * month it landed. What this toggle does is let the reader ask the other
 * question — "what does the trailing year look like without the things I have
 * flagged as not repeating?" — and flip back the moment they want the payoff
 * back in view. The flag itself is set by hand on the transaction; this toggle
 * only decides whether the trailing charts honour it.
 *
 * The styling mirrors RealToggle's Option so the two reader-driven surfaces
 * read as a pair.
 */
export function OneTimeToggle({
  enabled,
  onChange,
}: {
  enabled: boolean
  onChange: (v: boolean) => void
}) {
  return (
    <div className="flex flex-wrap items-center gap-2">
      <button
        type="button"
        aria-pressed={enabled}
        onClick={() => onChange(!enabled)}
        className={`rounded-lg px-3 py-1.5 text-sm transition ${
          enabled
            ? 'bg-white/10 text-mist-100'
            : 'text-mist-300 hover:bg-white/5 hover:text-mist-100'
        }`}
      >
        Hide one-time charges
      </button>
      <span className="text-xs text-mist-500">
        {enabled
          ? 'one-time charges hidden from the trailing charts — the money still left, and stays on each transaction'
          : 'the trailing year as it actually happened, loan payoffs and all'}
      </span>
    </div>
  )
}
