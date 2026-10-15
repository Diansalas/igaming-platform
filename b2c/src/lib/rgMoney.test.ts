import { describe, expect, it } from 'vitest'
import { formatMinorString, majorToMinorString, minorGreater } from './money'

describe('string-exact RG money helpers', () => {
  it('converts a major amount to minor units without a float', () => {
    expect(majorToMinorString('123.45', 'USD')).toBe('12345')
    expect(majorToMinorString('5', 'EUR')).toBe('500')
    expect(majorToMinorString('0.5', 'USD')).toBe('50')
    expect(majorToMinorString('500', 'JPY')).toBe('500')
    // 0.1 + 0.2 style float artefacts cannot occur
    expect(majorToMinorString('1.15', 'USD')).toBe('115')
    expect(majorToMinorString('9007199254740993', 'JPY')).toBe('9007199254740993')
  })

  it('refuses what it cannot send exactly', () => {
    for (const bad of ['', ' ', '0', '0.00', '-5', '1e3', '12.345', 'abc', '1,5', '.5', '5.']) {
      expect(majorToMinorString(bad, 'USD'), bad).toBeNull()
    }
    expect(majorToMinorString('10', 'XYZ')).toBeNull() // unknown exponent: never guessed
  })

  it('accepts exactly 38 digits and refuses 39', () => {
    expect(majorToMinorString('9'.repeat(38), 'JPY')).toBe('9'.repeat(38))
    expect(majorToMinorString('9'.repeat(39), 'JPY')).toBeNull()
  })

  it('formats exactly', () => {
    expect(formatMinorString('12345', 'USD')).toBe('123.45 USD')
    expect(formatMinorString('5', 'USD')).toBe('0.05 USD')
    expect(formatMinorString('100', 'JPY')).toBe('100 JPY')
    expect(formatMinorString('123', 'XYZ')).toBe('123 XYZ (minor units)')
    expect(formatMinorString('12345678901234567890123456789012345678', 'USD')).toBe('123456789012345678901234567890123456.78 USD')
  })

  it('compares decimal strings numerically', () => {
    expect(minorGreater('10', '9')).toBe(true)
    expect(minorGreater('9', '10')).toBe(false)
    expect(minorGreater('100', '100')).toBe(false)
    expect(minorGreater('0100', '99')).toBe(true)
    expect(minorGreater('123456789012345678901234567890', '123456789012345678901234567889')).toBe(true)
  })
})
