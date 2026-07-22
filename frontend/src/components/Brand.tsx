/** Wordmark plus the ambient sigil used on the auth screens. */
export function Wordmark({ size = 'md' }: { size?: 'md' | 'lg' }) {
  return (
    <div className="flex items-center gap-3">
      {/* The mark carries fine detail — bar chart, wand, sparkles — that the
          placeholder glyph it replaced did not, so it is given more room than
          a simple icon would need. Below about 40px it reads as a smudge. */}
      <Sigil className={size === 'lg' ? 'h-16 w-16' : 'h-10 w-10'} />
      <span
        className={
          size === 'lg'
            ? 'text-3xl font-semibold tracking-tight'
            : 'text-xl font-semibold tracking-tight'
        }
      >
        Ledger
        <span className="bg-gradient-to-r from-arcane-400 to-rune-400 bg-clip-text text-transparent">
          mancy
        </span>
      </span>
    </div>
  )
}

/**
 * The Ledgermancy mark.
 *
 * Kept as a component rather than a bare <img> so every call site gets the
 * same sizing and alt-text treatment. It is decorative wherever a text
 * wordmark sits beside it, hence aria-hidden by default.
 */
export function Sigil({
  className = 'h-8 w-8',
  alt,
}: {
  className?: string
  alt?: string
}) {
  return (
    <img
      src="/logo.png"
      className={`${className} select-none object-contain`}
      alt={alt ?? ''}
      aria-hidden={alt ? undefined : true}
      draggable={false}
    />
  )
}

/** Decorative glyphs drifting behind the auth card. */
export function AmbientGlyphs() {
  const glyphs = [
    { char: '✦', top: '12%', left: '14%', delay: '0s', size: 'text-2xl' },
    { char: '✧', top: '68%', left: '8%', delay: '1.4s', size: 'text-xl' },
    { char: '❋', top: '22%', left: '84%', delay: '2.6s', size: 'text-3xl' },
    { char: '✧', top: '78%', left: '78%', delay: '0.8s', size: 'text-2xl' },
    { char: '✦', top: '46%', left: '92%', delay: '3.4s', size: 'text-lg' },
  ]
  return (
    <div className="pointer-events-none absolute inset-0 overflow-hidden" aria-hidden="true">
      {glyphs.map((g, i) => (
        <span
          key={i}
          className={`animate-drift absolute text-arcane-400/30 ${g.size}`}
          style={{ top: g.top, left: g.left, animationDelay: g.delay }}
        >
          {g.char}
        </span>
      ))}
    </div>
  )
}
