import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '../lib/api'
import type {
  Account,
  Category,
  Rule,
  RuleAction,
  RuleActionType,
  RuleChange,
  RuleInput,
  RuleTestResult,
  RuleTrigger,
  RuleTriggerType,
  Tag,
} from '../lib/api'
import { formatDate, formatMoney } from '../lib/money'
import { SkeletonRows, Reveal } from '../components/Skeleton'
import { EmptyState } from '../components/EmptyState'
import { STATUS } from '../components/charts/tokens'

/**
 * Rules — user-editable IF-THEN over transactions.
 *
 * WHAT THIS PAGE IS FOR. Before it, the only way to say "everything from this
 * merchant is Groceries" was to fix one row at a time, or to rely on
 * automation nobody could see or change. A rule is the household saying it once,
 * in its own words, and having it hold — for what arrives tomorrow and, on
 * demand, for everything already stored.
 *
 * THE TWO THINGS A READER MUST BELIEVE, which is why the page leads with them:
 *
 *   1. Nothing here overwrites a category you set by hand. Not on a sync, not on
 *      a re-run, not ever. The server enforces that as a predicate, not as good
 *      manners.
 *   2. Running a rule twice does not do it twice. No duplicated tag, no note
 *      appended forever.
 *
 * Test before Run, always: the tester walks the same code as the run and shows
 * exactly what would move, so nobody has to find out by pressing the button.
 */
export function Rules() {
  const rules = useQuery({ queryKey: ['rules'], queryFn: api.rules })
  const rows = rules.data ?? []

  // The pickers' vocabularies. Fetched once here and handed down, so a page with
  // ten rules does not fetch the category list ten times.
  const categories = useQuery({ queryKey: ['categories'], queryFn: api.categories })
  const tags = useQuery({ queryKey: ['tags'], queryFn: api.tags })
  const accounts = useQuery({ queryKey: ['accounts'], queryFn: api.accounts })
  const lookups: Lookups = {
    categories: categories.data ?? [],
    tags: tags.data ?? [],
    accounts: accounts.data ?? [],
  }

  const [creating, setCreating] = useState(false)

  return (
    <div className="space-y-8">
      <div>
        <h1 className="text-2xl font-semibold">Rules</h1>
        <p className="mt-1 text-mist-300">
          If a charge looks like this, do that — every time, automatically.
        </p>
        <p className="mt-2 max-w-3xl text-xs text-mist-500">
          Rules run when a transaction arrives, and you can run one over
          everything already stored. A rule never overwrites a category you set
          by hand, and running one twice does not do it twice — so testing and
          re-running are always safe.
        </p>
      </div>

      <section className="glass p-6">
        <div className="mb-5 flex flex-wrap items-start justify-between gap-3">
          <div>
            <h2 className="text-lg font-medium">Your rules</h2>
            <p className="mt-1 text-sm text-mist-300">
              Higher priority runs first, and later rules see what earlier ones
              did.
            </p>
          </div>
          {rows.length > 0 && !creating && (
            <button className="btn-primary px-4 py-2 text-sm" onClick={() => setCreating(true)}>
              New rule
            </button>
          )}
        </div>

        {rules.isPending ? (
          <SkeletonRows count={3} />
        ) : rows.length === 0 ? (
          <EmptyState
            title="No rules yet"
            icon={<RuleGlyph />}
            action={
              !creating && (
                <button className="btn-primary" onClick={() => setCreating(true)}>
                  Write a rule
                </button>
              )
            }
          >
            Start with something you keep fixing by hand — a merchant that always
            lands in the wrong category, or charges you always want tagged.
          </EmptyState>
        ) : (
          <Reveal>
            <div className="space-y-3">
              {rows.map((rule) => (
                <RuleCard key={rule.id} rule={rule} lookups={lookups} />
              ))}
            </div>
          </Reveal>
        )}
      </section>

      {creating && (
        <section className="glass p-6">
          <h2 className="mb-1 text-lg font-medium">New rule</h2>
          <p className="mb-5 text-sm text-mist-300">
            Every condition has to hold for the rule to fire. Actions run in
            order, top to bottom.
          </p>
          <RuleEditor lookups={lookups} onDone={() => setCreating(false)} />
        </section>
      )}
    </div>
  )
}

/** The three id-keyed vocabularies a rule's operands point into. */
interface Lookups {
  categories: Category[]
  tags: Tag[]
  accounts: Account[]
}

/** Every surface a rule can move refetches after a run: the ledger's categories
 *  and tag chips, the tag totals, and the rules list itself. */
function invalidateRules(qc: ReturnType<typeof useQueryClient>) {
  qc.invalidateQueries({ queryKey: ['rules'] })
  qc.invalidateQueries({ queryKey: ['transactions'] })
  qc.invalidateQueries({ queryKey: ['tags'] })
  qc.invalidateQueries({ queryKey: ['by-tag'] })
}

function RuleCard({ rule, lookups }: { rule: Rule; lookups: Lookups }) {
  const qc = useQueryClient()
  const [editing, setEditing] = useState(false)
  const [preview, setPreview] = useState<RuleTestResult | null>(null)

  const test = useMutation({
    mutationFn: () => api.testRule(rule.id),
    onSuccess: setPreview,
  })
  const run = useMutation({
    mutationFn: () => api.runRule(rule.id),
    onSuccess: () => {
      invalidateRules(qc)
      // Re-testing after a run is what shows idempotence rather than asserting
      // it: the same rule, still matching, with nothing left to change.
      test.mutate()
    },
  })
  const remove = useMutation({
    mutationFn: () => api.deleteRule(rule.id),
    onSuccess: () => invalidateRules(qc),
  })

  return (
    <div className="rounded-xl border border-white/5 p-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0">
          <p className="font-medium">
            {rule.name}
            {!rule.active && (
              <span className="ml-2 rounded-full border border-white/10 px-2 py-0.5 text-xs font-normal text-mist-400">
                off
              </span>
            )}
          </p>
          {rule.description && (
            <p className="mt-0.5 text-sm text-mist-400">{rule.description}</p>
          )}
          <p className="mt-1.5 text-sm text-mist-300">
            <span className="text-mist-500">If </span>
            {rule.triggers.map((t, i) => (
              <span key={t.id}>
                {i > 0 && <span className="text-mist-500"> and </span>}
                {describeTrigger(t, lookups)}
              </span>
            ))}
            <span className="text-mist-500">, then </span>
            {rule.actions.map((a, i) => (
              <span key={a.id}>
                {i > 0 && <span className="text-mist-500">, </span>}
                {describeAction(a, lookups)}
              </span>
            ))}
          </p>
        </div>

        <div className="flex shrink-0 items-center gap-2">
          <button
            className="btn-ghost px-2.5 py-1 text-xs"
            disabled={test.isPending}
            onClick={() => test.mutate()}
          >
            {test.isPending ? 'Testing…' : 'Test'}
          </button>
          <button
            className="btn-ghost px-2.5 py-1 text-xs"
            disabled={run.isPending}
            title="Applies this rule to transactions you can already see. Safe to press twice — the second time changes nothing."
            onClick={() => run.mutate()}
          >
            {run.isPending ? 'Running…' : 'Run now'}
          </button>
          <button
            className="btn-ghost px-2.5 py-1 text-xs"
            onClick={() => setEditing((v) => !v)}
          >
            {editing ? 'Cancel' : 'Edit'}
          </button>
          <button
            className="btn-ghost px-2.5 py-1 text-xs text-ember-400"
            disabled={remove.isPending}
            title="Deletes the rule. What it already did stays — those categories and tags are your data now."
            onClick={() => remove.mutate()}
          >
            {remove.isPending ? 'Deleting…' : 'Delete'}
          </button>
        </div>
      </div>

      {(test.isError || run.isError || remove.isError) && (
        <p role="alert" className="mt-3 text-sm text-ember-400">
          {(test.error ?? run.error ?? remove.error)?.message}
        </p>
      )}

      {run.isSuccess && !run.isPending && (
        <p className="mt-3 text-sm text-mist-300">
          Changed {run.data.changed}{' '}
          {run.data.changed === 1 ? 'transaction' : 'transactions'} of{' '}
          {run.data.matched} matched.
          {run.data.changed === 0 && ' Everything this rule wanted was already true.'}
        </p>
      )}

      {preview && <TestResult result={preview} lookups={lookups} />}
      {editing && (
        <RuleEditor rule={rule} lookups={lookups} onDone={() => setEditing(false)} />
      )}
    </div>
  )
}

/** The dry run, rendered. Counts first, because the count is the answer; the
 *  rows below are the evidence for it. */
function TestResult({ result, lookups }: { result: RuleTestResult; lookups: Lookups }) {
  return (
    <div className="mt-4 border-t border-white/5 pt-4">
      <p className="text-sm text-mist-300">
        Fires on <span className="tabular font-medium">{result.matched}</span> of{' '}
        <span className="tabular">{result.scanned}</span> transactions you can
        see, and would change{' '}
        <span className="tabular font-medium">{result.would_change}</span>.
      </p>
      {result.matched > 0 && result.would_change === 0 && (
        <p className="mt-1 text-xs text-mist-500">
          Nothing to do — everything this rule wants is already true. Running it
          again is a no-op.
        </p>
      )}

      {result.matches.length > 0 && (
        <ul className="mt-3 divide-y divide-white/5 text-sm">
          {result.matches.map((m) => (
            <li key={m.transaction_id} className="py-2">
              <div className="flex items-center gap-3">
                <span className="w-24 shrink-0 text-mist-500">{formatDate(m.date)}</span>
                <span className="min-w-0 flex-1 truncate">{m.merchant}</span>
                <span className="hidden shrink-0 text-xs text-mist-500 sm:block">
                  {m.account_name}
                </span>
                <span className="tabular w-24 shrink-0 text-right">
                  {formatMoney(m.amount)}
                </span>
              </div>
              <div className="mt-1 flex flex-wrap gap-x-4 gap-y-1 pl-24 text-xs">
                {m.changes.map((c, i) => (
                  <ChangeLine key={i} change={c} lookups={lookups} />
                ))}
              </div>
            </li>
          ))}
        </ul>
      )}

      {result.truncated && (
        <p className="mt-3 text-xs text-mist-500">
          Showing the first {result.matches.length}. The counts above cover all{' '}
          {result.matched}.
        </p>
      )}
    </div>
  )
}

/** One action's outcome. The reason is shown for anything that did not apply —
 *  an unexplained "nothing happened" is what makes people stop trusting
 *  automation. */
function ChangeLine({ change, lookups }: { change: RuleChange; lookups: Lookups }) {
  const colour =
    change.outcome === 'applied'
      ? undefined
      : change.outcome === 'refused'
        ? STATUS.critical
        : undefined

  return (
    <span className={change.outcome === 'applied' ? 'text-mist-300' : 'text-mist-500'}>
      <span style={colour ? { color: colour } : undefined}>
        {describeAction({ type: change.action, value: change.value }, lookups)}
      </span>
      {change.outcome !== 'applied' && (
        <span className="text-mist-500"> — {change.reason || change.outcome}</span>
      )}
    </span>
  )
}

// --- Plain-language rendering ---------------------------------------------

const TRIGGER_LABELS: Record<RuleTriggerType, string> = {
  description_contains: 'description contains',
  description_starts: 'description starts with',
  description_ends: 'description ends with',
  description_is: 'description is',
  amount_more: 'amount is more than',
  amount_less: 'amount is less than',
  amount_exactly: 'amount is exactly',
  merchant_is: 'merchant is',
  category_is: 'category is',
  has_no_category: 'it has no category',
  account_is: 'account is',
  has_attachments: 'it has attachments',
}

const ACTION_LABELS: Record<RuleActionType, string> = {
  set_category: 'set category to',
  add_tag: 'add tag',
  set_notes: 'set notes to',
  append_notes: 'add a note',
}

/** What kind of operand a condition takes, which is what the editor renders and
 *  what the sentence below prints. */
function triggerOperand(type: RuleTriggerType): 'text' | 'amount' | 'category' | 'account' | 'none' {
  switch (type) {
    case 'amount_more':
    case 'amount_less':
    case 'amount_exactly':
      return 'amount'
    case 'category_is':
      return 'category'
    case 'account_is':
      return 'account'
    case 'has_no_category':
    case 'has_attachments':
      return 'none'
    default:
      return 'text'
  }
}

function actionOperand(type: RuleActionType): 'category' | 'tag' | 'text' {
  if (type === 'set_category') return 'category'
  if (type === 'add_tag') return 'tag'
  return 'text'
}

/** Resolves an id operand to the name the user knows it by. Falls back to the
 *  raw value rather than rendering nothing: a rule pointing at something deleted
 *  should look wrong, not empty. */
function nameFor(kind: 'category' | 'tag' | 'account', id: string, lookups: Lookups): string {
  const list =
    kind === 'category' ? lookups.categories : kind === 'tag' ? lookups.tags : lookups.accounts
  return list.find((x) => x.id === id)?.name ?? id
}

function describeTrigger(trigger: RuleTrigger, lookups: Lookups) {
  const operand = triggerOperand(trigger.type)
  const label = TRIGGER_LABELS[trigger.type] ?? trigger.type
  const not = trigger.invert ? (operand === 'none' ? "doesn't hold: " : 'not ') : ''

  let value = ''
  if (operand === 'amount') value = formatMoney(trigger.value)
  else if (operand === 'category') value = nameFor('category', trigger.value, lookups)
  else if (operand === 'account') value = nameFor('account', trigger.value, lookups)
  else if (operand === 'text') value = `“${trigger.value}”`

  return (
    <span>
      {not}
      {label}
      {value && ' '}
      {value && <span className="text-mist-100">{value}</span>}
    </span>
  )
}

function describeAction(
  action: Pick<RuleAction, 'type' | 'value'>,
  lookups: Lookups,
) {
  const operand = actionOperand(action.type)
  const label = ACTION_LABELS[action.type] ?? action.type
  const value =
    operand === 'category'
      ? nameFor('category', action.value, lookups)
      : operand === 'tag'
        ? nameFor('tag', action.value, lookups)
        : `“${action.value}”`

  return (
    <span>
      {label} <span className="text-mist-100">{value}</span>
    </span>
  )
}

// --- The editor ------------------------------------------------------------

/** A draft row. Separate from the API shape because a half-built row has no id
 *  and its value is whatever the user has typed so far. */
interface DraftTrigger {
  type: RuleTriggerType
  value: string
  invert: boolean
}
interface DraftAction {
  type: RuleActionType
  value: string
  stop_on_fail: boolean
}

/**
 * Create and edit share one component, because they are the same form: an update
 * REPLACES the condition and action lists, so "edit" is "here is the whole rule
 * again". A delta editor would make the client responsible for diffing against a
 * list it may have fetched a minute ago.
 */
function RuleEditor({
  rule,
  lookups,
  onDone,
}: {
  rule?: Rule
  lookups: Lookups
  onDone: () => void
}) {
  const qc = useQueryClient()
  const [name, setName] = useState(rule?.name ?? '')
  const [description, setDescription] = useState(rule?.description ?? '')
  const [active, setActive] = useState(rule?.active ?? true)
  const [priority, setPriority] = useState(String(rule?.priority ?? 0))
  const [triggers, setTriggers] = useState<DraftTrigger[]>(
    rule?.triggers.map((t) => ({ type: t.type, value: t.value, invert: t.invert })) ?? [
      { type: 'description_contains', value: '', invert: false },
    ],
  )
  const [actions, setActions] = useState<DraftAction[]>(
    rule?.actions.map((a) => ({
      type: a.type,
      value: a.value,
      stop_on_fail: a.stop_on_fail,
    })) ?? [{ type: 'set_category', value: '', stop_on_fail: false }],
  )

  const body = (): RuleInput => ({
    name: name.trim(),
    description: description.trim() || null,
    active,
    priority: Number(priority) || 0,
    triggers,
    actions,
  })

  const save = useMutation({
    mutationFn: () => (rule ? api.updateRule(rule.id, body()) : api.createRule(body())),
    onSuccess: () => {
      invalidateRules(qc)
      onDone()
    },
  })

  const patchTrigger = (i: number, patch: Partial<DraftTrigger>) =>
    setTriggers((rows) => rows.map((r, j) => (j === i ? { ...r, ...patch } : r)))
  const patchAction = (i: number, patch: Partial<DraftAction>) =>
    setActions((rows) => rows.map((r, j) => (j === i ? { ...r, ...patch } : r)))

  return (
    <div className="mt-4 space-y-5 border-t border-white/5 pt-4">
      <div className="grid gap-3 sm:grid-cols-3">
        <div className="sm:col-span-2">
          <label className="label" htmlFor={`rule-name-${rule?.id ?? 'new'}`}>
            Name
          </label>
          <input
            id={`rule-name-${rule?.id ?? 'new'}`}
            className="field w-full"
            placeholder="Blue Bottle is coffee"
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
        </div>
        <div>
          <label className="label" htmlFor={`rule-priority-${rule?.id ?? 'new'}`}>
            Priority
          </label>
          <input
            id={`rule-priority-${rule?.id ?? 'new'}`}
            className="field w-full"
            type="number"
            step="1"
            value={priority}
            onChange={(e) => setPriority(e.target.value)}
          />
        </div>
        <div className="sm:col-span-3">
          <label className="label" htmlFor={`rule-desc-${rule?.id ?? 'new'}`}>
            Description (optional)
          </label>
          <input
            id={`rule-desc-${rule?.id ?? 'new'}`}
            className="field w-full"
            placeholder="What this is for"
            value={description}
            onChange={(e) => setDescription(e.target.value)}
          />
        </div>
      </div>

      {/* Conditions */}
      <div>
        <p className="label">If all of these are true</p>
        <div className="mt-2 space-y-2">
          {triggers.map((t, i) => (
            <div key={i} className="flex flex-wrap items-center gap-2">
              <select
                className="field"
                aria-label="Condition"
                value={t.type}
                onChange={(e) =>
                  // Switching type clears the operand: an amount left in a
                  // category slot is a rule that cannot save, and silently
                  // carrying it over is how that happens.
                  patchTrigger(i, { type: e.target.value as RuleTriggerType, value: '' })
                }
              >
                {(Object.keys(TRIGGER_LABELS) as RuleTriggerType[]).map((type) => (
                  <option key={type} value={type}>
                    {TRIGGER_LABELS[type]}
                  </option>
                ))}
              </select>

              <OperandInput
                kind={triggerOperand(t.type)}
                value={t.value}
                lookups={lookups}
                onChange={(value) => patchTrigger(i, { value })}
              />

              <label className="flex items-center gap-2 text-sm text-mist-300">
                <input
                  type="checkbox"
                  className="h-4 w-4 accent-arcane-500"
                  checked={t.invert}
                  onChange={(e) => patchTrigger(i, { invert: e.target.checked })}
                />
                not
              </label>

              {triggers.length > 1 && (
                <button
                  className="btn-ghost px-2 py-1 text-xs text-ember-400"
                  onClick={() => setTriggers((rows) => rows.filter((_, j) => j !== i))}
                >
                  Remove
                </button>
              )}
            </div>
          ))}
        </div>
        <button
          className="btn-ghost mt-2 px-2.5 py-1 text-xs"
          onClick={() =>
            setTriggers((rows) => [
              ...rows,
              { type: 'description_contains', value: '', invert: false },
            ])
          }
        >
          Add condition
        </button>
      </div>

      {/* Actions */}
      <div>
        <p className="label">Then, in order</p>
        <div className="mt-2 space-y-2">
          {actions.map((a, i) => (
            <div key={i} className="flex flex-wrap items-center gap-2">
              <select
                className="field"
                aria-label="Action"
                value={a.type}
                onChange={(e) =>
                  patchAction(i, { type: e.target.value as RuleActionType, value: '' })
                }
              >
                {(Object.keys(ACTION_LABELS) as RuleActionType[]).map((type) => (
                  <option key={type} value={type}>
                    {ACTION_LABELS[type]}
                  </option>
                ))}
              </select>

              <OperandInput
                kind={actionOperand(a.type)}
                value={a.value}
                lookups={lookups}
                onChange={(value) => patchAction(i, { value })}
              />

              <label
                className="flex items-center gap-2 text-sm text-mist-300"
                title="If this action can't be applied — the category was set by hand, or what it names is gone — skip the rest of this rule for that transaction."
              >
                <input
                  type="checkbox"
                  className="h-4 w-4 accent-arcane-500"
                  checked={a.stop_on_fail}
                  onChange={(e) => patchAction(i, { stop_on_fail: e.target.checked })}
                />
                stop if refused
              </label>

              {actions.length > 1 && (
                <button
                  className="btn-ghost px-2 py-1 text-xs text-ember-400"
                  onClick={() => setActions((rows) => rows.filter((_, j) => j !== i))}
                >
                  Remove
                </button>
              )}
            </div>
          ))}
        </div>
        <button
          className="btn-ghost mt-2 px-2.5 py-1 text-xs"
          onClick={() =>
            setActions((rows) => [
              ...rows,
              { type: 'set_category', value: '', stop_on_fail: false },
            ])
          }
        >
          Add action
        </button>
      </div>

      <div className="flex flex-wrap items-center gap-3">
        <label className="flex items-center gap-2 text-sm text-mist-300">
          <input
            type="checkbox"
            className="h-4 w-4 accent-arcane-500"
            checked={active}
            onChange={(e) => setActive(e.target.checked)}
          />
          Active
        </label>
        <button
          className="btn-primary px-4 py-2 text-sm"
          disabled={name.trim() === '' || save.isPending}
          onClick={() => save.mutate()}
        >
          {save.isPending ? 'Saving…' : rule ? 'Save' : 'Create rule'}
        </button>
        <button className="btn-ghost px-3 py-2 text-sm" onClick={onDone}>
          Cancel
        </button>
        {save.isError && (
          <span role="alert" className="text-sm text-ember-400">
            {save.error.message}
          </span>
        )}
      </div>
    </div>
  )
}

/**
 * The operand field, which is a different control per kind.
 *
 * An amount is a text input with a decimal step rather than a free field,
 * because the server takes amounts as decimal STRINGS — money never passes
 * through a float, and that starts here.
 */
function OperandInput({
  kind,
  value,
  lookups,
  onChange,
}: {
  kind: 'text' | 'amount' | 'category' | 'account' | 'tag' | 'none'
  value: string
  lookups: Lookups
  onChange: (value: string) => void
}) {
  if (kind === 'none') return null

  if (kind === 'amount') {
    return (
      <input
        className="field w-32"
        aria-label="Amount"
        type="number"
        min="0"
        step="0.01"
        value={value}
        onChange={(e) => onChange(e.target.value)}
      />
    )
  }

  if (kind === 'text') {
    return (
      <input
        className="field min-w-48 flex-1"
        aria-label="Value"
        value={value}
        onChange={(e) => onChange(e.target.value)}
      />
    )
  }

  const options =
    kind === 'category' ? lookups.categories : kind === 'tag' ? lookups.tags : lookups.accounts

  return (
    <select
      className="field min-w-48"
      aria-label={kind}
      value={value}
      onChange={(e) => onChange(e.target.value)}
    >
      <option value="">Choose a {kind}…</option>
      {options.map((o) => (
        <option key={o.id} value={o.id}>
          {o.name}
        </option>
      ))}
    </select>
  )
}

/** Outline glyph for the rules empty state: a branch. */
function RuleGlyph() {
  return (
    <svg
      className="h-5 w-5"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={2}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d="M5 4v6a3 3 0 0 0 3 3h11" />
      <path d="M15 9l4 4-4 4" />
      <circle cx="5" cy="20" r="1.5" />
    </svg>
  )
}
