import type { CSSProperties } from 'react'

/**
 * A category's icon — self-hosted, privacy-pure (MAD-39).
 *
 * A small inline-SVG icon set covers the system categories (groceries, dining,
 * transport, home, …) plus common custom-category names. Every glyph ships in
 * the JS bundle: zero outbound font/image requests, consistent with the
 * self-hosted type setup. The icon replaces the bare colour-dot that used to
 * sit beside a category name; where no icon matches, the same identity-colour
 * dot returns, so an unmatched category reads exactly as it did before.
 *
 * Colour is the category's own seeded identity colour (categories.color), the
 * same value the dot already used. These colours never enter a chart's data
 * marks — they only ever sit beside a text label, which the app's own
 * AllocationList/CategoryBars comments explicitly permit. The validated chart
 * palette in `charts/tokens.ts` is untouched.
 *
 * Iconography is intentionally stroke-based line art at a 24×24 box: it reads
 * at 16px on a chart axis and at 20px in a budget row, and stays legible
 * against the ink surface in either the category colour or a neutral tint.
 */

type IconKey =
  | 'income'
  | 'transfer-in'
  | 'transfer-out'
  | 'credit-card-payment'
  | 'rent-and-utilities'
  | 'loan-payments'
  | 'food-and-drink'
  | 'groceries'
  | 'transportation'
  | 'travel'
  | 'general-merchandise'
  | 'entertainment'
  | 'personal-care'
  | 'medical'
  | 'general-services'
  | 'home-improvement'
  | 'government-and-non-profit'
  | 'bank-fees'
  | 'uncategorised'
  | 'other'

/** Direct slug → icon. Slugs come from the system category seed (00002). */
const SLUG_MAP: Record<string, IconKey> = {
  income: 'income',
  'transfer-in': 'transfer-in',
  'transfer-out': 'transfer-out',
  'credit-card-payment': 'credit-card-payment',
  'rent-and-utilities': 'rent-and-utilities',
  'loan-payments': 'loan-payments',
  'food-and-drink': 'food-and-drink',
  groceries: 'groceries',
  transportation: 'transportation',
  travel: 'travel',
  'general-merchandise': 'general-merchandise',
  entertainment: 'entertainment',
  'personal-care': 'personal-care',
  medical: 'medical',
  'general-services': 'general-services',
  'home-improvement': 'home-improvement',
  'government-and-non-profit': 'government-and-non-profit',
  'bank-fees': 'bank-fees',
  uncategorised: 'uncategorised',
  other: 'other',
}

/**
 * Name → icon, for custom categories (no system slug). Ordered specific-first
 * so "Credit Card Payment" beats a generic "payment"; medical before personal
 * care so "Medicare" doesn't become a spa drop; fees before income so "Interest
 * charge" isn't read as a deposit. Tested against the lowercased name.
 */
const NAME_RULES: Array<[RegExp, IconKey]> = [
  [/transfer[ -]?in/i, 'transfer-in'],
  [/transfer[ -]?out/i, 'transfer-out'],
  [/credit card|card payment/i, 'credit-card-payment'],
  [/grocer/i, 'groceries'],
  [/food|dining|restaurant|coffee|cafe|eatery|fast.?food|drink|\bbar\b/i, 'food-and-drink'],
  [
    /gas|fuel|transp|car|auto|uber|lyft|taxi|rideshare|parking|transit|\bbus\b|train|subway|metro/i,
    'transportation',
  ],
  [/travel|flight|\bair\b|hotel|trip|vacation|lodging|airbnb/i, 'travel'],
  [
    /rent|mortgage|utilit|electric|water bill|internet|wifi|broadband|phone bill|housing/,
    'rent-and-utilities',
  ],
  [/loan|student|tuition/i, 'loan-payments'],
  [/entertain|movie|film|cinema|music|stream|concert|game|theat|netflix|spotify/i, 'entertainment'],
  [
    /medic|doctor|dental|dentist|pharmac|hospital|clinic|health|therapy|vision|optom/,
    'medical',
  ],
  [/salon|barber|hair|spa|groom|beauty|cosmet|nail|personal care|wellness/i, 'personal-care'],
  [/shop|store|merch|retail|amazon|mall|market|warehouse/i, 'general-merchandise'],
  [/service|repair|plumb|electrician|clean|landscap|mechanic/i, 'general-services'],
  [/home improv|hardware|reno|paint|construct|tools|\bdiy\b|maintenance/i, 'home-improvement'],
  [/\btax\b|government|\bgov\b|irs|dmv|court/i, 'government-and-non-profit'],
  [/bank fee|overdraft|interest charge|service fee|atm fee|late fee|\bfee\b/i, 'bank-fees'],
  [/income|salary|payroll|wage|deposit|paycheck|dividend|refund|reimburse/i, 'income'],
  [/transfer/i, 'transfer-out'],
  [/uncateg/i, 'uncategorised'],
]

function iconKeyFor(slug?: string | null, name?: string | null): IconKey | undefined {
  if (slug) {
    const hit = SLUG_MAP[slug]
    if (hit) return hit
  }
  if (name) {
    const n = name.toLowerCase()
    for (const [re, key] of NAME_RULES) {
      if (re.test(n)) return key
    }
  }
  return undefined
}

const GLYPH_PROPS = {
  viewBox: '0 0 24 24',
  fill: 'none',
  stroke: 'currentColor',
  strokeWidth: 1.8,
  strokeLinecap: 'round' as const,
  strokeLinejoin: 'round' as const,
  'aria-hidden': true,
}

function Glyph({
  name,
  className,
  style,
}: {
  name: IconKey
  className?: string
  style?: CSSProperties
}) {
  return (
    <svg {...GLYPH_PROPS} className={className} style={style}>
      {PATHS[name]}
    </svg>
  )
}

/**
 * One path-set per icon. Drawn on a 24×24 grid at stroke-width 1.8 with round
 * joins, so each scales cleanly from 16px (a chart axis) to 20px (a budget row).
 * Kept geometric so they read as a family; no per-icon stroke tuning.
 */
const PATHS: Record<IconKey, React.ReactNode> = {
  // Money in: down arrow into a tray.
  income: (
    <>
      <path d="M12 3v9" />
      <path d="M8.5 8.5L12 12l3.5-3.5" />
      <path d="M5 19h14" />
    </>
  ),
  // Money moving between accounts, arriving: down-left arrow.
  'transfer-in': (
    <>
      <path d="M19 5L7 17" />
      <path d="M13 17H7v-6" />
    </>
  ),
  // Money moving between accounts, leaving: up-right arrow.
  'transfer-out': (
    <>
      <path d="M5 19L17 7" />
      <path d="M11 7h6v6" />
    </>
  ),
  'credit-card-payment': (
    <>
      <rect x="3" y="5" width="18" height="14" rx="2" />
      <path d="M3 10h18" />
    </>
  ),
  // Home — covers rent + household utilities.
  'rent-and-utilities': (
    <>
      <path d="M4 11l8-7 8 7" />
      <path d="M6 9.5V19h12V9.5" />
      <path d="M10 19v-5h4v5" />
    </>
  ),
  // Banknote — a fixed loan obligation.
  'loan-payments': (
    <>
      <rect x="3" y="7" width="18" height="10" rx="1.5" />
      <circle cx="12" cy="12" r="2.6" />
      <path d="M6.5 9.5v5 M17.5 9.5v5" />
    </>
  ),
  // Bowl with steam — dining / food & drink.
  'food-and-drink': (
    <>
      <path d="M4 11h16a8 8 0 0 1-16 0z" />
      <path d="M8 4c0 1.4 1 1.4 1 2.8 M12 3.5c0 1.4 1 1.4 1 2.8 M16 4c0 1.4 1 1.4 1 2.8" />
    </>
  ),
  // Shopping cart.
  groceries: (
    <>
      <path d="M3 4h2l2 11h11l2-7H6" />
      <circle cx="10" cy="20" r="1.3" />
      <circle cx="17" cy="20" r="1.3" />
    </>
  ),
  // Car silhouette.
  transportation: (
    <>
      <path d="M5 11l1.5-4.5A2 2 0 0 1 8.4 5h7.2a2 2 0 0 1 1.9 1.5L19 11" />
      <path d="M3 11h18v5H3z" />
      <path d="M3 14.5h18" />
      <circle cx="7.5" cy="17" r="1.2" />
      <circle cx="16.5" cy="17" r="1.2" />
    </>
  ),
  // Suitcase — travel / trips.
  travel: (
    <>
      <rect x="6" y="8" width="12" height="11" rx="1.5" />
      <path d="M10 8V5.5A1.5 1.5 0 0 1 11.5 4h1A1.5 1.5 0 0 1 14 5.5V8" />
      <path d="M6 12.5h12" />
    </>
  ),
  // Shopping bag — general merchandise / retail.
  'general-merchandise': (
    <>
      <path d="M6 8h12l-1 12H7z" />
      <path d="M9 8V6a3 3 0 0 1 6 0v2" />
    </>
  ),
  // Play button — entertainment / media.
  entertainment: (
    <>
      <rect x="3" y="5" width="18" height="14" rx="2" />
      <path d="M10 9.5l5 2.5-5 2.5z" />
    </>
  ),
  // Teardrop — personal care / grooming / spa.
  'personal-care': <path d="M12 3s6 6.5 6 11a6 6 0 0 1-12 0c0-4.5 6-11 6-11z" />,
  // Medical cross — health.
  medical: (
    <>
      <rect x="4" y="4" width="16" height="16" rx="3" />
      <path d="M12 8v8 M8 12h8" />
    </>
  ),
  // Briefcase — general / professional services.
  'general-services': (
    <>
      <rect x="3" y="8" width="18" height="11" rx="2" />
      <path d="M9 8V6a2 2 0 0 1 2-2h2a2 2 0 0 1 2 2v2" />
      <path d="M3 13h18" />
    </>
  ),
  // Gear — home improvement / maintenance / DIY.
  'home-improvement': (
    <>
      <circle cx="12" cy="12" r="3.4" />
      <path d="M12 3v3.2 M12 17.8V21 M3 12h3.2 M17.8 12H21 M5.6 5.6l2.3 2.3 M16.1 16.1l2.3 2.3 M5.6 18.4l2.3-2.3 M16.1 7.9l2.3-2.3" />
    </>
  ),
  // Classical landmark — government / civic / non-profit.
  'government-and-non-profit': (
    <>
      <path d="M3 9h18" />
      <path d="M12 3l9 5H3z" />
      <path d="M6 9v8 M10 9v8 M14 9v8 M18 9v8" />
      <path d="M4 19h16" />
    </>
  ),
  // Coin stack — bank fees / charges.
  'bank-fees': (
    <>
      <ellipse cx="12" cy="6" rx="6" ry="2.4" />
      <path d="M6 6v4c0 1.3 2.7 2.4 6 2.4s6-1.1 6-2.4V6" />
      <path d="M6 10v4c0 1.3 2.7 2.4 6 2.4s6-1.1 6-2.4v-4" />
    </>
  ),
  // Horizontal ellipsis — unknown / folded.
  uncategorised: (
    <>
      <circle cx="6" cy="12" r="1.4" />
      <circle cx="12" cy="12" r="1.4" />
      <circle cx="18" cy="12" r="1.4" />
    </>
  ),
  other: (
    <>
      <circle cx="6" cy="12" r="1.4" />
      <circle cx="12" cy="12" r="1.4" />
      <circle cx="18" cy="12" r="1.4" />
    </>
  ),
}

/**
 * Render a category's icon, or its identity-colour dot when nothing matches.
 *
 * The outer span holds a stable box (the `className`, default `size-4`) whether
 * the category resolves to an icon or to the fallback dot, so a list where some
 * rows icon and some dot stays aligned. Decorative: the category name is always
 * rendered as text beside this, so `aria-hidden` is correct.
 */
export function CategoryIcon({
  slug,
  name,
  color,
  className = 'size-4',
}: {
  slug?: string | null
  name?: string | null
  color?: string | null
  className?: string
}) {
  const key = iconKeyFor(slug, name)
  // Matches the dot's previous default opacity so an unmatched category looks
  // identical to before the icon set existed.
  const tint = color ?? 'rgba(255,255,255,0.3)'
  return (
    <span
      aria-hidden
      className={`inline-flex shrink-0 items-center justify-center ${className}`}
    >
      {key ? (
        <Glyph name={key} className="h-full w-full" style={{ color: tint }} />
      ) : (
        <span className="block size-2 rounded-full" style={{ backgroundColor: tint }} />
      )}
    </span>
  )
}
