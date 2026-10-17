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

describe('U-5 exact formatMoney / toMinorUnits (float-free)', () => {
  it('formats number, digit-string and bigint minor units identically and exactly', async () => {
    const { formatMoney } = await import('./money')
    expect(formatMoney(12345, 'USD')).toBe('123.45 USD')
    expect(formatMoney('12345', 'USD')).toBe('123.45 USD')
    expect(formatMoney(12345n, 'USD')).toBe('123.45 USD')
    expect(formatMoney('9007199254740993', 'USD')).toBe('90071992547409.93 USD')
    expect(formatMoney('9223372036854775807', 'JPY')).toBe('9223372036854775807 JPY')
    expect(formatMoney(-5, 'USD')).toBe('-0.05 USD')
    expect(formatMoney(5, 'XYZ')).toBe('5 XYZ (minor units)')
    expect(formatMoney(1.5, 'USD')).toBe('1.5 USD') // not an exact integer: shown raw, never scaled
  })
  it('toMinorUnits is exact, rejects excess precision, zero, garbage and unsafe sizes', async () => {
    const { toMinorUnits } = await import('./money')
    expect(toMinorUnits('1.15', 'USD')).toBe(115)
    expect(toMinorUnits('4.35', 'USD')).toBe(435)
    expect(toMinorUnits('0.07', 'EUR')).toBe(7)
    expect(toMinorUnits('500', 'JPY')).toBe(500)
    for (const bad of ['', '0', '0.00', '-1', '1.005', 'abc', '1e2', '90071992547409.92', '90071992547409.93']) {
      expect(toMinorUnits(bad, 'USD'), bad).toBeNull()
    }
    expect(toMinorUnits('90071992547409.91', 'USD')).toBe(9007199254740991)
    expect(toMinorUnits('10', 'XYZ')).toBeNull()
  })
})
