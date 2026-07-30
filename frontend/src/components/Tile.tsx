import { STATUS } from './charts/tokens'

/**
 * A headline number in its own glass panel — the row of four at the top of the
 * Spending, merchant and category views.
 *
 * Extracted from the identical copies in Spending.tsx and MerchantDetail.tsx when
 * the category page would have made a third. Dashboard has a `Tile` of its own
 * that is deliberately NOT this one: it is a compact figure inside an existing
 * card, with no panel of its own, so unifying them would mean a variant prop that
 * changes everything about the component.
 */
export function Tile({
  label,
  value,
  hint,
  tone,
}: {
  label: string
  value: string
  hint?: string
  tone?: 'good' | 'critical'
}) {
  const color =
    tone === 'critical' ? STATUS.critical : tone === 'good' ? STATUS.good : undefined

  return (
    <div className="glass p-5">
      <p className="text-sm text-mist-300">{label}</p>
      <p
        className="tabular mt-2 text-3xl font-semibold"
        style={{ color: color ?? '#f2d492' }}
      >
        {value}
      </p>
      {hint && <p className="mt-1 text-xs text-mist-500">{hint}</p>}
    </div>
  )
}
