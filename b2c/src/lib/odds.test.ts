import { describe, expect, it } from 'vitest'
import { estimatePotentialReturnMinorUnits, formatDecimalOdds } from './odds'

// Regression test for a real P0 found by architect review: the server's
// numerator/denominator ratio IS the decimal odds value (already includes
// the stake), but this module originally added 1, overstating every price
// and return shown in the UI by the size of the stake itself (e.g. 2.50
// odds displayed as 3.50). internal/sportsbook/orchestrator.go's
// computePotentialReturn is the source of truth this test pins against.
describe('formatDecimalOdds', () => {
  it('renders numerator/denominator directly as decimal odds, never +1', () => {
    expect(formatDecimalOdds(250, 100)).toBe('2.50')
    expect(formatDecimalOdds(150, 100)).toBe('1.50')
    expect(formatDecimalOdds(500, 100)).toBe('5.00')
  })

  it('returns a placeholder for a non-positive denominator', () => {
    expect(formatDecimalOdds(250, 0)).toBe('-')
  })
})

describe('estimatePotentialReturnMinorUnits', () => {
  it('matches the server-side stake * numerator/denominator formula exactly', () => {
    // 1000 minor units at 2.50 decimal odds -> 2500, not 3500.
    expect(estimatePotentialReturnMinorUnits(1000, 250, 100)).toBe(2500)
  })

  it('rounds down, never up', () => {
    expect(estimatePotentialReturnMinorUnits(3, 250, 100)).toBe(7) // 7.5 -> 7
  })

  it('returns 0 for a non-positive stake or denominator', () => {
    expect(estimatePotentialReturnMinorUnits(0, 250, 100)).toBe(0)
    expect(estimatePotentialReturnMinorUnits(1000, 250, 0)).toBe(0)
  })
})
