import { describe, expect, it } from 'vitest'
import { formatAge, formatMinorUnits } from './minorUnits'

describe('formatMinorUnits', () => {
  it('formats with BigInt, no float rounding, for large amounts', () => {
    expect(formatMinorUnits('12345', 2, 'EUR')).toBe('123.45 EUR')
    expect(formatMinorUnits('5', 2, 'EUR')).toBe('0.05 EUR')
    expect(formatMinorUnits('0', 8, 'BTC')).toBe('0.00000000 BTC')
    expect(formatMinorUnits('123456789012345678901234567890123456', 18, 'ETH')).toBe('123456789012345678.901234567890123456 ETH')
    expect(formatMinorUnits('9007199254740993', 0, 'X')).toBe('9007199254740993 X')
    expect(formatMinorUnits('-250', 2, 'EUR')).toBe('-2.50 EUR')
  })
  it('never guesses: bad input or unknown exponent is labelled raw', () => {
    expect(formatMinorUnits('12.5', 2, 'EUR')).toContain('unprintable')
    expect(formatMinorUnits('100', null, 'EUR')).toBe('100 minor units of EUR (exponent unknown)')
    expect(formatMinorUnits('100', 99, 'EUR')).toContain('exponent unknown')
    expect(formatMinorUnits(null, 2, 'EUR')).toBe('Not available')
    expect(formatMinorUnits('100', 2, '<script>')).toBe('1.00 unknown asset')
  })
})

describe('formatAge', () => {
  const now = Date.parse('2026-10-15T12:00:00Z')
  it('renders days, hours, minutes', () => {
    expect(formatAge('2026-10-12T08:00:00Z', now)).toBe('3d 4h')
    expect(formatAge('2026-10-15T09:30:00Z', now)).toBe('2h 30m')
    expect(formatAge('2026-10-15T11:48:00Z', now)).toBe('12m')
    expect(formatAge('garbage', now)).toBe('Not available')
    expect(formatAge('2026-10-16T00:00:00Z', now)).toBe('0m')
  })
})
