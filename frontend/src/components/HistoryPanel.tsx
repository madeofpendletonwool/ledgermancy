import { useQuery } from '@tanstack/react-query'

import { api, type ObjectChange, type ObjectChangeKind } from '../lib/api'
import { formatDate, formatRelative } from '../lib/money'

/**
 * Per-object change history ("who changed what, when") for any audited object.
 *
 * Reads the single visibility-scoped /api/audit endpoint and renders the
 * field-level diffs as old → new, with the actor and a relative timestamp. The
 * same panel serves every object kind — transaction, budget, goal — so the
 * History surface reads identically wherever it appears.
 *
 * Values are the raw JSONB the server stored. A null old_value means the field
 * was set on create; a null new_value means it was cleared. The synthetic
 * `created` field is an object's first appearance and renders as a single line
 * rather than a diff.
 */
export function HistoryPanel({
  kind,
  objectId,
}: {
  kind: ObjectChangeKind
  objectId: string
}) {
  const history = useQuery({
    queryKey: ['object-history', kind, objectId],
    queryFn: () => api.objectHistory(kind, objectId),
  })

  if (!history.data || history.data.length === 0) {
    return (
      <p className="mt-3 text-xs text-mist-500">
        No edit history yet.
      </p>
    )
  }

  return (
    <ul className="mt-3 divide-y divide-white/5 rounded-lg border border-white/5 text-sm">
      {history.data.map((row, i) => (
        <li key={i} className="flex flex-wrap items-baseline gap-x-2 gap-y-1 px-3 py-2">
          {row.field === 'created' ? (
            <CreatedRow row={row} />
          ) : (
            <>
              <span className="font-medium text-mist-200">{fieldLabel(row.field)}</span>
              <span className="tabular text-mist-300">
                <span className="text-mist-500 line-through decoration-white/10">
                  {renderValue(row.old_value)}
                </span>
                <span className="mx-1 text-mist-500">→</span>
                <span>{renderValue(row.new_value)}</span>
              </span>
            </>
          )}
          <span className="ml-auto text-xs text-mist-500" title={row.created_at}>
            {row.actor_display_name ?? 'Someone'} · {formatRelative(row.created_at)}
          </span>
        </li>
      ))}
    </ul>
  )
}

function CreatedRow({ row }: { row: ObjectChange }) {
  return (
    <span className="text-mist-300">
      Created by{' '}
      <span className="font-medium text-mist-200">
        {row.actor_display_name ?? 'someone'}
      </span>
    </span>
  )
}

// Friendly labels for the tracked fields. Falls back to the raw field name so a
// newly-wired field still reads sensibly before its label is added here.
const FIELD_LABELS: Record<string, string> = {
  amount: 'Amount',
  date: 'Date',
  name: 'Name',
  merchant_name: 'Merchant',
  category_id: 'Category',
  notes: 'Notes',
  period: 'Period',
  rollover: 'Rollover',
  target_amount: 'Target',
  target_date: 'Target date',
  account_id: 'Account',
  college_years: 'College years',
}

function fieldLabel(field: string): string {
  return FIELD_LABELS[field] ?? field
}

// renderValue turns one stored JSONB value into something a person can read.
// Most values are strings already (money, dates, uuids are all serialised as
// text server-side); null is the one that carries meaning and reads as a dash.
function renderValue(value: unknown): string {
  if (value === null || value === undefined) return '—'
  if (typeof value === 'boolean') return value ? 'yes' : 'no'
  if (typeof value === 'number') return String(value)
  if (typeof value === 'string') {
    // A date: format it rather than dumping the ISO form.
    if (/^\d{4}-\d{2}-\d{2}$/.test(value)) return formatDate(value)
    // A UUID: shorten rather than print 36 opaque chars.
    if (/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(value)) {
      return value.slice(0, 8)
    }
    return value
  }
  return String(value)
}
