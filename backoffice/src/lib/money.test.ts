import { describe, expect, it } from 'vitest'
import { formatMoney, formatUnscaledAmount } from './money'

describe('formatMoney', () => {
  it('scales an integer minor-units amount by the asset exponent', () => {
    expect(formatMoney(10000, 'USD', 2)).toBe('100.00 USD')
  })

  it('renders a zero-exponent asset with no decimal point', () => {
    expect(formatMoney(500, 'JPY', 0)).toBe('500 JPY')
  })

  it('supports high-precision (e.g. crypto) exponents', () => {
    expect(formatMoney(150000000, 'BTC', 8)).toBe('1.50000000 BTC')
  })

  it('never guesses a scale when the exponent is unknown - shows raw minor units with an explicit qualifier', () => {
    expect(formatMoney(10000, 'USD', undefined)).toBe('10000 USD (exponent unknown)')
  })
})

describe('formatUnscaledAmount', () => {
  it('always renders the raw decimal-string value with an explicit qualifier, never a scaled guess', () => {
    expect(formatUnscaledAmount('5000', 'USD')).toBe('5000 USD (raw minor units, exponent unknown)')
  })
})
