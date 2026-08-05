/**
 * The shape of a merchant avatar.
 *
 * Lives here rather than in `Monogram.tsx` for the same reason
 * `merchantDetailPath` lives beside it and not beside its route: two components
 * render into this slot — the monogram and, when the opt-in fetcher has one, a
 * real logo — and a logo a pixel off the monogram it replaces makes every list
 * shift as images arrive. One definition is what stops the two drifting.
 */

export type AvatarSize = 'xs' | 'sm' | 'md' | 'lg' | 'xl'

/** Box size, corner radius and glyph size move together so proportions hold. */
export const AVATAR_BOX: Record<AvatarSize, string> = {
  xs: 'size-5 rounded-md text-[11px]',
  sm: 'size-6 rounded-md text-xs',
  md: 'size-8 rounded-lg text-[15px]',
  lg: 'size-10 rounded-lg text-lg',
  xl: 'size-12 rounded-xl text-xl',
}
