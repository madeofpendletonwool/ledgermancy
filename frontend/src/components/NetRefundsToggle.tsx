/**
 * The "Net linked refunds" lens over the spending figures on /spending.
 *
 * One toggle governs the trend chart and the typical-month table, because they
 * answer the same question at two resolutions and a reader wants to switch
 * lenses once. It sits beside `RealToggle` and `OneTimeToggle`, which are the
 * other two reader-driven lenses on this page.
 *
 * The label says "linked" on purpose. This does not find refunds — nothing here
 * guesses. It honours the links a person made by hand on the transactions, and
 * only those whose type nets (today, the built-in Refund). A household that has
 * linked nothing sees identical figures with the toggle on and off, which is the
 * correct outcome and not a bug to go looking for.
 *
 * The hint text names WHERE the money comes off, because that is the part
 * readers get wrong: the refund is subtracted from the original charge's month,
 * not added as a negative to the month the credit landed in.
 *
 * The styling mirrors OneTimeToggle so the three lenses read as a set.
 */
export function NetRefundsToggle({
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
        Net linked refunds
      </button>
      <span className="text-xs text-mist-500">
        {enabled
          ? 'a refund you linked comes off the charge it refunds, in that charge’s own month'
          : 'every month as it happened — a refund is its own event, not a correction'}
      </span>
    </div>
  )
}
