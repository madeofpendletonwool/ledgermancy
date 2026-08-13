import { describe, expect, it } from 'vitest'

import {
  activeToken,
  applySuggestion,
  suggestFor,
  type SearchOperator,
} from './searchGrammar'

const op = (name: string, takes_value = true): SearchOperator => ({
  name,
  takes_value,
  help: `help for ${name}`,
})

const OPERATORS = [
  op('merchant'),
  op('merchant_starts'),
  op('category'),
  op('has_no_category', false),
  op('has_notes', false),
  op('over'),
  op('since'),
]

describe('activeToken', () => {
  it('finds the word the caret is inside', () => {
    expect(activeToken('over:10 merch', 13)).toEqual({ start: 8, end: 13, text: 'merch' })
    expect(activeToken('over:10 merch', 10)).toEqual({ start: 8, end: 13, text: 'merch' })
  })

  // A caret immediately after a word belongs to that word. That is where you are
  // standing the instant you finish typing it, which is when suggestions matter.
  it('treats a caret at the end of a word as being in it', () => {
    expect(activeToken('merch', 5)).toEqual({ start: 0, end: 5, text: 'merch' })
  })

  it('gives an empty token in whitespace', () => {
    expect(activeToken('over:10 ', 8)).toEqual({ start: 8, end: 8, text: '' })
    expect(activeToken('a  b', 2)).toEqual({ start: 2, end: 2, text: '' })
  })

  // A caret out of range clamps into the string rather than producing a range
  // that would splice text outside it.
  it('clamps a caret outside the string', () => {
    expect(activeToken('abc', 99)).toEqual({ start: 0, end: 3, text: 'abc' })
    expect(activeToken('abc', -5)).toEqual({ start: 0, end: 3, text: 'abc' })
  })
})

describe('suggestFor', () => {
  it('ranks prefix matches above substring matches', () => {
    expect(suggestFor('cat', OPERATORS).map((o) => o.name)).toEqual([
      'category',
      'has_no_category',
    ])
  })

  it('ignores a leading dash, which is the negation the user already typed', () => {
    expect(suggestFor('-merch', OPERATORS).map((o) => o.name)).toEqual([
      'merchant',
      'merchant_starts',
    ])
  })

  it('is case-insensitive', () => {
    expect(suggestFor('MERCH', OPERATORS).map((o) => o.name)).toEqual([
      'merchant',
      'merchant_starts',
    ])
  })

  // Once there is a colon the user is typing a value — their own merchant names
  // and dates — and this list knows nothing about those.
  it('stops suggesting once the token has a colon', () => {
    expect(suggestFor('merchant:st', OPERATORS)).toEqual([])
  })

  it('suggests nothing for an empty token, so the popover stays shut', () => {
    expect(suggestFor('', OPERATORS)).toEqual([])
    expect(suggestFor('-', OPERATORS)).toEqual([])
  })

  it('honours the limit', () => {
    expect(suggestFor('a', OPERATORS, 2)).toHaveLength(2)
  })
})

describe('applySuggestion', () => {
  it('leaves the caret after the colon of a value operator', () => {
    const got = applySuggestion('merch', 5, op('merchant'))
    expect(got).toEqual({ value: 'merchant:', caret: 9 })
  })

  // A flag is a complete term, so it gets the space that starts the next one.
  it('completes a flag with a trailing space', () => {
    const got = applySuggestion('has_no', 6, op('has_no_category', false))
    expect(got).toEqual({ value: 'has_no_category ', caret: 16 })
  })

  it('replaces only the token the caret is in', () => {
    const got = applySuggestion('starbucks over sinc', 19, op('since'))
    expect(got).toEqual({ value: 'starbucks over since:', caret: 21 })
  })

  it('splices into the middle of a query without disturbing the rest', () => {
    const value = 'starbucks merch over:10'
    const got = applySuggestion(value, 15, op('merchant'))
    expect(got.value).toBe('starbucks merchant: over:10')
    expect(got.caret).toBe(19)
  })

  it('keeps a negation the user has already committed to', () => {
    const got = applySuggestion('-merch', 6, op('merchant'))
    expect(got).toEqual({ value: '-merchant:', caret: 10 })
  })
})
