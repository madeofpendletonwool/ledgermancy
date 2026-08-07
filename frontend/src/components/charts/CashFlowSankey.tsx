import { useMemo, useState } from 'react'
import { sankey, sankeyLeft, sankeyLinkHorizontal } from 'd3-sankey'
import type {
  SankeyExtraProperties,
  SankeyLink,
  SankeyLinkMinimal,
  SankeyNode,
} from 'd3-sankey'
import type { CashFlow, CashFlowSource } from '../../lib/api'
import { formatMoney } from '../../lib/money'
import { CHART, SERIES, STATUS } from './tokens'
import { ChartBoundary } from './ChartBoundary'

const WIDTH = 820
const HEIGHT = 380
// Generous side padding: the chart carries text labels on BOTH sides (income
// source names left, spending category names + values right), so the plot area
// is inset to leave them room rather than letting them collide with flows.
const PAD = { top: 24, right: 176, bottom: 22, left: 156 }

const PLOT_W = WIDTH - PAD.left - PAD.right
const PLOT_H = HEIGHT - PAD.top - PAD.bottom

const NODE_WIDTH = 18
// Padding between stacked nodes. A Sankey shares its vertical space between two
// bands, so this is a touch tighter than a single-band chart would use.
const NODE_PADDING = 8

// Fold caps. The app's fold-to-Other rule is "past ~8"; a Sankey carries two
// bands in one frame, so the cap sits a little under that to stay readable.
// Top (cap - 1) real nodes plus a single folded "Other" row.
const INCOME_CAP = 6 // top 5 income sources + Other
const SPENDING_CAP = 7 // top 6 spending categories + Other

/** Which band a node belongs to — drives its colour, nothing else. */
type Band = 'income' | 'pool' | 'spending' | 'savings' | 'deficit'

/** What a flow represents — drives its colour, independent of the nodes it joins. */
type FlowKind = 'income' | 'spending' | 'savings' | 'deficit'

// User-defined node/link properties — the IN-EXCESS generics d3-sankey carries
// alongside its own computed fields. `amount` is the original decimal string
// (shown via formatMoney, never re-summed); the Sankey's own `value` field is a
// number d3 computes for layout. `fromIdx`/`toIdx` are captured at build time
// so hover logic never has to read link.source/.target, whose post-layout type
// is a number|string|node union that is awkward to narrow in TS.
interface CFNode extends SankeyExtraProperties {
  name: string
  band: Band
  amount: string
  count?: number
  /** A folded "Other" or synthetic node — labelled so, but same hue as its band. */
  synthetic?: boolean
}

interface CFLink extends SankeyExtraProperties {
  kind: FlowKind
  fromIdx: number
  toIdx: number
}

type LaidOutNode = SankeyNode<CFNode, CFLink>
type LaidOutLink = SankeyLink<CFNode, CFLink>

/**
 * The cash-flow Sankey — "where does my money actually go".
 *
 * Income sources on the left flow into a single "cash in" pool, which flows
 * out to spending categories on the right; whatever remains flows to a savings
 * node. When spending exceeds income, the shortfall is drawn honestly as a
 * deficit inflow rather than faking balance.
 *
 * Layout is d3-sankey's; the SVG is rendered here, in the hand-rolled style the
 * rest of the app's charts use (viewBox, recessive furniture, hit targets wider
 * than marks). A Sankey's spline routing and node balancing are not worth
 * hand-rolling, which is why this is the one chart that reaches for a library.
 *
 * Colour carries the THREE bands, not category identity: every income flow is
 * blue, every spending flow orange, savings green, deficit red. Which paycheck
 * or which grocery category a node is lives in its label — colouring each node
 * differently is the classic unreadable Sankey and is deliberately avoided.
 *
 * Every money figure comes from the server as a decimal string and is shown via
 * formatMoney; the only arithmetic in JS is sizing the display geometry
 * (proportional node heights and flow widths), which is explicitly allowed.
 */
function CashFlowSankeyUnguarded({ data, label }: { data: CashFlow; label: string }) {
  const [active, setActive] = useState<number | null>(null)

  const graph = useMemo(() => buildGraph(data), [data])

  // Which links and nodes belong to the hovered node. Computed once per hover
  // so the render only does set lookups, and so the union-typed link.source /
  // link.target never have to be read here.
  const focus = useMemo(() => {
    if (active === null || !graph) return null
    if (!graph.nodes.some((n) => n.index === active)) return null
    const linkIds = new Set<number>()
    const nodeIds = new Set<number>([active])
    for (const l of graph.links) {
      if (l.fromIdx === active || l.toIdx === active) {
        if (l.index != null) linkIds.add(l.index)
        nodeIds.add(l.fromIdx)
        nodeIds.add(l.toIdx)
      }
    }
    return { linkIds, nodeIds }
  }, [active, graph])

  if (!graph) {
    return <Empty>No income or spending in this period.</Empty>
  }

  const { nodes, links } = graph
  const flowPath = sankeyLinkHorizontal<CFNode, CFLink>()

  const colorForBand = (band: Band): string => {
    switch (band) {
      case 'income':
      case 'pool':
        return SERIES.income
      case 'spending':
        return SERIES.spending
      case 'savings':
        return SERIES.leftover
      case 'deficit':
        return STATUS.critical
    }
  }
  const colorForFlow = (kind: FlowKind): string => {
    switch (kind) {
      case 'income':
        return SERIES.income
      case 'spending':
        return SERIES.spending
      case 'savings':
        return SERIES.leftover
      case 'deficit':
        return STATUS.critical
    }
  }

  const linkOpacity = (link: LaidOutLink): number => {
    if (!focus || link.index == null) return 0.45
    return focus.linkIds.has(link.index) ? 0.75 : 0.1
  }

  const activeNode = active !== null ? nodes.find((n) => n.index === active) ?? null : null

  const leftoverN = Number(data.leftover)
  const deficit = leftoverN < 0
  const surplus = leftoverN > 0

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center gap-x-5 gap-y-1.5 text-xs">
        <LegendKey color={SERIES.income} label="Income" />
        <LegendKey color={SERIES.spending} label="Spending" />
        {surplus && <LegendKey color={SERIES.leftover} label="Left to save" />}
        {deficit && <LegendKey color={STATUS.critical} label="Deficit" />}
      </div>

      <div className="chart-scroll relative overflow-x-auto">
        <span className="sr-only">This chart scrolls horizontally — swipe to see the rest.</span>
        <svg
          viewBox={`0 0 ${WIDTH} ${HEIGHT}`}
          className="w-full min-w-[680px]"
          role="img"
          aria-label={ariaSummary(data, label)}
          onMouseLeave={() => setActive(null)}
        >
          {/* Flows first, so nodes sit on top of their own endpoints. */}
          {links.map((link) => (
            <path
              key={link.index}
              d={flowPath(link) ?? ''}
              fill={colorForFlow(link.kind)}
              fillOpacity={linkOpacity(link)}
              stroke={colorForFlow(link.kind)}
              strokeOpacity={linkOpacity(link) * 0.6}
              strokeWidth={0.5}
            />
          ))}

          {/* Nodes. */}
          {nodes.map((node) => {
            const w = node.x1! - node.x0!
            const h = node.y1! - node.y0!
            const dimmed = focus ? !focus.nodeIds.has(node.index!) : false
            return (
              <g key={node.index}>
                <rect
                  x={node.x0}
                  y={node.y0}
                  width={w}
                  height={h}
                  fill={colorForBand(node.band)}
                  fillOpacity={dimmed ? 0.35 : 0.95}
                />
                <NodeLabel node={node} />
              </g>
            )
          })}

          {/* Invisible hit targets over each node — a band tall enough that a
              thin node is still an easy target. */}
          {nodes.map((node) => (
            <rect
              key={`hit-${node.index}`}
              x={node.x0! - 6}
              y={Math.max(PAD.top, (node.y0! + node.y1!) / 2 - 16)}
              width={node.x1! - node.x0! + 12}
              height={32}
              fill="transparent"
              onMouseEnter={() => setActive(node.index ?? null)}
            />
          ))}
        </svg>

        {activeNode && (
          <div
            className="pointer-events-none absolute top-2 rounded-xl border border-white/10 bg-ink-900/95 px-3 py-2 text-xs shadow-xl backdrop-blur"
            style={{
              left: `${((activeNode.x0! + activeNode.x1!) / 2 / WIDTH) * 100}%`,
              transform: 'translateX(-50%)',
            }}
          >
            <p className="mb-1 font-medium text-mist-100">{activeNode.name}</p>
            <p className="tabular text-mist-300">{formatMoney(activeNode.amount)}</p>
            {activeNode.count != null && activeNode.count > 0 && (
              <p className="text-mist-500">
                {activeNode.count} txn{activeNode.count === 1 ? '' : 's'}
              </p>
            )}
            {activeNode.synthetic && (
              <p className="text-mist-500">
                {activeNode.band === 'spending' ? 'folded categories' : 'folded sources'}
              </p>
            )}
          </div>
        )}
      </div>
    </div>
  )
}

function NodeLabel({ node }: { node: LaidOutNode }) {
  const cy = (node.y0! + node.y1!) / 2
  const h = node.y1! - node.y0!

  // The pool node is a transit point, not a category — label it minimally with
  // a small caption above, so it never competes with the named nodes.
  if (node.band === 'pool') {
    return (
      <text
        x={(node.x0! + node.x1!) / 2}
        y={Math.max(PAD.top + 8, node.y0! - 6)}
        textAnchor="middle"
        fontSize="10"
        fill={CHART.textMuted}
      >
        cash in
      </text>
    )
  }

  const isLeft = node.band === 'income' || node.band === 'deficit'
  const anchor = isLeft ? 'end' : 'start'
  const x = isLeft ? node.x0! - 8 : node.x1! + 8
  const name = node.name
  const value = formatMoney(node.amount)

  // Two lines — name above, value below — vertically centred on the node. For
  // a very short node only the name fits, and the value moves to the tooltip.
  const showValue = h >= 12
  return (
    <g>
      <text
        x={x}
        y={showValue ? cy - 1 : cy + 3}
        textAnchor={anchor}
        fontSize="11"
        fill={CHART.textPrimary}
      >
        {truncate(name, 18)}
      </text>
      {showValue && (
        <text
          x={x}
          y={cy + 11}
          textAnchor={anchor}
          fontSize="11"
          fill={node.band === 'savings' ? SERIES.leftover : CHART.textSecondary}
        >
          {value}
        </text>
      )}
    </g>
  )
}

function LegendKey({ color, label }: { color: string; label: string }) {
  return (
    <span className="flex items-center gap-1.5" style={{ color: CHART.textSecondary }}>
      <span
        className="inline-block h-2.5 w-2.5 rounded-full"
        style={{ backgroundColor: color }}
      />
      {label}
    </span>
  )
}

function Empty({ children }: { children: React.ReactNode }) {
  return (
    <p className="py-12 text-center text-sm" style={{ color: CHART.textMuted }}>
      {children}
    </p>
  )
}

/**
 * Build the Sankey graph: nodes and links with display-side numeric values for
 * d3-sankey to lay out. Returns null for the one true empty state — no income
 * AND no spending — so the caller renders a message instead of a blank frame.
 * Every other case (income only, spending only, deficit) produces a valid,
 * balanced graph.
 *
 * The graph balances by construction: the pool's inflow equals its outflow in
 * both the surplus and deficit cases, so d3-sankey converges without distorting
 * node heights.
 */
function buildGraph(data: CashFlow): { nodes: LaidOutNode[]; links: LaidOutLink[] } | null {
  const incomeN = Number(data.income_total)
  const spendingN = Number(data.spending_total)
  const leftoverN = Number(data.leftover)
  const surplus = leftoverN > 0
  const deficit = leftoverN < 0

  if (incomeN <= 0 && spendingN <= 0) return null

  const incomeRows = foldIncome(data.income_sources, INCOME_CAP)
  const spendingRows = foldSpending(
    data.spending_categories,
    data.uncategorized_spending,
    SPENDING_CAP,
  )

  const nodes: CFNode[] = []
  const links: (CFLink & SankeyLinkMinimal<CFNode, CFLink>)[] = []
  const addLink = (
    fromIdx: number,
    toIdx: number,
    value: number,
    kind: FlowKind,
  ) => {
    // Every link carries a strictly-positive value; the build guarantees this
    // (zero rows are folded/dropped before reaching here), so the layout never
    // sees a zero-width flow.
    if (!(value > 0)) return
    links.push({ source: fromIdx, target: toIdx, value, kind, fromIdx, toIdx })
  }

  // Left column: income sources, then the deficit source when spending outran
  // income. The nodes are pushed first and their indices recorded; their links
  // into the pool are added once the pool's index is known, so fromIdx/toIdx are
  // always the real node positions (the hover logic relies on those, not on
  // link.source/.target).
  const inflowSpecs: { idx: number; value: number; kind: FlowKind }[] = []
  for (const s of incomeRows) {
    const idx = nodes.length
    nodes.push({
      name: s.name,
      band: 'income',
      amount: s.total,
      count: s.transaction_count,
      synthetic: s.synthetic,
    })
    inflowSpecs.push({ idx, value: Number(s.total), kind: 'income' })
  }
  if (deficit) {
    const idx = nodes.length
    const deficitAmount = Math.abs(leftoverN)
    nodes.push({
      name: 'Deficit (spent more than came in)',
      band: 'deficit',
      amount: deficitAmount.toFixed(2),
      synthetic: true,
    })
    inflowSpecs.push({ idx, value: deficitAmount, kind: 'deficit' })
  }

  // Middle: the pool, the aggregation point every inflow feeds and every
  // outflow leaves.
  const poolIdx = nodes.length
  nodes.push({ name: 'Cash in', band: 'pool', amount: data.income_total })
  for (const spec of inflowSpecs) addLink(spec.idx, poolIdx, spec.value, spec.kind)

  // Right column: spending categories (+ uncategorised gap), then savings when
  // income outran spending.
  for (const c of spendingRows) {
    const idx = nodes.length
    nodes.push({
      name: c.name,
      band: 'spending',
      amount: c.total,
      count: c.transaction_count,
      synthetic: c.synthetic,
    })
    addLink(poolIdx, idx, Number(c.total), 'spending')
  }
  if (surplus) {
    const idx = nodes.length
    nodes.push({ name: 'Left to save', band: 'savings', amount: data.leftover })
    addLink(poolIdx, idx, leftoverN, 'savings')
  }

  // Hand d3-sankey the layout. The input nodes/links are fresh per render, so
  // the mutation the layout performs never leaks across renders.
  const layout = sankey<CFNode, CFLink>()
    .nodeWidth(NODE_WIDTH)
    .nodePadding(NODE_PADDING)
    .nodeAlign(sankeyLeft)
    .extent([
      [PAD.left, PAD.top],
      [PAD.left + PLOT_W, PAD.top + PLOT_H],
    ])

  const result = layout({ nodes, links })
  return { nodes: result.nodes, links: result.links }
}

/** Top (cap - 1) income sources plus a folded "Other" row. Display-side sum. */
function foldIncome(
  sources: CashFlowSource[],
  cap: number,
): (CashFlowSource & { synthetic: boolean })[] {
  const sorted = [...sources].sort((a, b) => Number(b.total) - Number(a.total))
  const positive = sorted.filter((s) => Number(s.total) > 0)
  if (positive.length <= cap) return positive.map((s) => ({ ...s, synthetic: false }))
  return foldTail(positive, cap, 'sources', 'other-income')
}

/**
 * Top (cap - 1) spending categories plus a folded "Other" row, with the
 * uncategorised gap appended as its own node when non-zero. The gap is spending
 * whose category_id was null — it is in the period's spending total but absent
 * from the category rows, so it rides here to keep the flows balanced.
 */
function foldSpending(
  categories: CashFlowSource[],
  uncategorised: string,
  cap: number,
): (CashFlowSource & { synthetic: boolean })[] {
  const sorted = [...categories].sort((a, b) => Number(b.total) - Number(a.total))
  const positive = sorted.filter((c) => Number(c.total) > 0)
  const rows =
    positive.length <= cap
      ? positive.map((c) => ({ ...c, synthetic: false }))
      : foldTail(positive, cap, 'categories', 'other-spending')

  // The uncategorised gap is named distinctly from the real "Uncategorised"
  // category (which is a normal spending row) so the two can never be confused.
  if (Number(uncategorised) > 0) {
    return [
      ...rows,
      {
        category_id: 'uncategorised-gap',
        name: 'Not categorised',
        slug: 'uncategorised-gap',
        color: null,
        total: uncategorised,
        transaction_count: 0,
        synthetic: true,
      },
    ]
  }
  return rows
}

/** Splits a sorted-descending list into its head plus a folded "Other" row. */
function foldTail<T extends CashFlowSource>(
  rows: T[],
  cap: number,
  noun: string,
  id: string,
): (CashFlowSource & { synthetic: boolean })[] {
  const head = rows.slice(0, cap - 1)
  const tail = rows.slice(cap - 1)
  // Display-side sum only; every figure that feeds analysis is server-computed.
  const otherTotal = tail.reduce((sum, r) => sum + Number(r.total), 0)
  return [
    ...head.map((r) => ({ ...r, synthetic: false })),
    {
      category_id: id,
      name: `Other (${tail.length} ${noun})`,
      slug: id,
      color: null,
      total: otherTotal.toFixed(2),
      transaction_count: tail.reduce((sum, r) => sum + r.transaction_count, 0),
      synthetic: true,
    },
  ]
}

function truncate(s: string, max: number): string {
  return s.length <= max ? s : `${s.slice(0, max - 1)}…`
}

function ariaSummary(data: CashFlow, label: string): string {
  const parts = [
    `Cash flow for ${label}`,
    `${formatMoney(data.income_total)} income`,
    `${formatMoney(data.spending_total)} spending`,
  ]
  const leftoverN = Number(data.leftover)
  if (leftoverN > 0) parts.push(`${formatMoney(data.leftover)} left to save`)
  else if (leftoverN < 0) parts.push(`${formatMoney(data.leftover)} deficit`)
  return parts.join(', ')
}

// The export is the guarded chart: a throw inside costs the reader the chart,
// not the page (MAD-61).
export function CashFlowSankey(props: Parameters<typeof CashFlowSankeyUnguarded>[0]) {
  return (
    <ChartBoundary label="cash flow">
      <CashFlowSankeyUnguarded {...props} />
    </ChartBoundary>
  )
}
