# Ledgermancy — colour reference

Source of truth: `frontend/src/index.css` (the `@theme` block). If you change a
value there, change it here too.

Two palettes exist and they are **not** interchangeable:

- **Brand palette** (this page) — UI, logo, marketing. Chosen for mood.
- **Chart palette** (`frontend/src/components/charts/tokens.ts`) — data
  visualisation only. Chosen by a colourblind-safety validator, not by eye. Do
  not use brand colours in charts or chart colours in the logo.

---

## The three that matter for a logo

| Role | Hex | Notes |
| --- | --- | --- |
| **Arcane violet** | `#8b5cf6` | The primary. Magic, the "-mancy" half. |
| **Rune gold** | `#e8b962` | The accent. Money, warmth, emphasis. Use sparingly. |
| **Ink** | `#07060d` | The near-black ground everything sits on. |

The wordmark gradient runs **violet → gold**, left to right:

```
#a78bfa  →  #e8b962
```

"Ledger" is plain white-ish (`#e8e6f5`); "mancy" carries the gradient.

---

## Full palette

### Ink — backgrounds, deepest to lightest

| Token | Hex | Used for |
| --- | --- | --- |
| `ink-950` | `#07060d` | Page background |
| `ink-900` | `#0d0b16` | Inputs, tooltips |
| `ink-850` | `#12101f` | Cards / glass surfaces (**the chart surface**) |
| `ink-800` | `#191630` | Raised surfaces |
| `ink-700` | `#241f45` | Borders on dark |
| `ink-600` | `#322b5e` | Dividers |

### Arcane — the primary violet

| Token | Hex | Used for |
| --- | --- | --- |
| `arcane-400` | `#a78bfa` | Links, gradient start, glyphs |
| `arcane-500` | `#8b5cf6` | **Primary brand colour**, button top |
| `arcane-600` | `#7c3aed` | Button bottom, background glow |

### Rune — the gold accent

| Token | Hex | Used for |
| --- | --- | --- |
| `rune-300` | `#f2d492` | Money figures, headline numbers |
| `rune-400` | `#e8b962` | **Accent brand colour**, gradient end |
| `rune-500` | `#d99e3a` | Deep accent, background glow |

### Mist — text

| Token | Hex | Used for |
| --- | --- | --- |
| `mist-100` | `#e8e6f5` | Primary text |
| `mist-300` | `#b3aed0` | Secondary text |
| `mist-500` | `#7b749c` | Muted text, placeholders |

### Signal

| Token | Hex | Used for |
| --- | --- | --- |
| `verdant-400` | `#4ade80` | Positive — income, completed steps |
| `ember-400` | `#fb7185` | Negative — debt, errors, overspend |

---

## Gradients and effects

```css
/* Wordmark: "mancy" */
linear-gradient(to right, #a78bfa, #e8b962)

/* Primary button */
linear-gradient(to bottom, #8b5cf6, #7c3aed)

/* Page background — two soft glows over ink-950 */
radial-gradient(ellipse 80% 55% at 50% -10%, #7c3aed at ~20% opacity, transparent)
radial-gradient(ellipse 60% 45% at 90% 100%, #d99e3a at ~8% opacity, transparent)
```

Card surfaces are `ink-850` at 60% opacity with a backdrop blur, a `white/10`
hairline border, and a soft inner top highlight — a frosted "spell page".

---

## The mark

A coin crossed by a rune: an outer circle, a fainter inner circle, an
eight-pointed star/asterisk struck through it, and a solid gold centre dot.
Strokes carry the violet→gold gradient; the centre dot is `rune-400`.

Live SVG: `frontend/src/components/Brand.tsx` (`<Sigil />`).

---

## If you are designing a logo

- The app is **dark-only**. Any mark needs to work on `#07060d` first. Make a
  mono version for light backgrounds rather than assuming the gradient inverts.
- Gold is the accent, not a co-star — roughly one part gold to four parts
  violet. Gold is reserved for money.
- Avoid `verdant-400` and `ember-400` in the logo. They mean "up" and "down" in
  the product, and a logo that borrows them will read as a status.
- The gradient needs ~24px of run to be visible. Below that, use flat
  `arcane-500` with a `rune-400` detail.

---

## Chart colours (do not use for branding)

Listed only so the difference is clear. These passed a validator for lightness
band, chroma floor, colourblind separation, normal-vision separation, and
contrast against `#12101f`. The brand violets and golds **failed** that same
check — several were indistinguishable from each other.

| Role | Hex |
| --- | --- |
| Series 1 — income | `#3987e5` |
| Series 2 — spending | `#d95926` |
| Series 3 — leftover | `#199e70` |
| Single-series bars | `#9085e9` |
| good / warning / serious / critical | `#0ca30c` · `#fab219` · `#ec835a` · `#d03b3b` |
