import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { describe, expect, it } from 'vitest'
import {
  compareMinor,
  formatMinor,
  isValidExponent,
  minorGreater,
  parseDecimalToMinor,
  parseJsonExact,
  parseMinor,
  toWireInteger,
} from './amount'

const EXPONENTS = [0, 2, 3, 8, 18]
const BIG = '9007199254740993' // 2^53 + 1: not representable as a JS number
const HUGE = '123456789012345678901234567890123456' // 36 digits

describe('parseMinor', () => {
  it('accepts safe integers, digit strings (leading zeros, negatives) and bigint', () => {
    expect(parseMinor(0)).toBe(0n)
    expect(parseMinor(12345)).toBe(12345n)
    expect(parseMinor('00123')).toBe(123n)
    expect(parseMinor('-5')).toBe(-5n)
    expect(parseMinor('-0')).toBe(0n)
    expect(parseMinor(BIG)).toBe(9007199254740993n)
    expect(parseMinor(7n)).toBe(7n)
  })
  it('refuses unsafe numbers, floats, garbage, empty and oversized input', () => {
    for (const bad of [9007199254740993, 1.5, NaN, Infinity, '', ' ', '1.5', '1e3', '0x10', 'abc', '--1', '+1', ' 1', null, undefined, {}, [], '9'.repeat(41)]) {
      expect(parseMinor(bad), String(bad)).toBeNull()
    }
  })
})

describe('parseDecimalToMinor', () => {
  it.each(EXPONENTS)('exponent %i: whole, fractional, zero and leading zeros are exact', (e) => {
    const unit = 10n ** BigInt(e)
    expect(parseDecimalToMinor('25', e)).toBe(25n * unit)
    expect(parseDecimalToMinor('0', e)).toBe(0n)
    expect(parseDecimalToMinor('0007', e)).toBe(7n * unit)
    if (e > 0) {
      expect(parseDecimalToMinor('0.1', e)).toBe(unit / 10n)
      expect(parseDecimalToMinor(`1.${'0'.repeat(e - 1)}1`, e)).toBe(unit + 1n)
      expect(parseDecimalToMinor(`0.${'0'.repeat(e)}1`, e), 'one digit too many').toBeNull()
    } else {
      expect(parseDecimalToMinor('1.0', e), 'no fractional digits allowed at exponent 0').toBeNull()
    }
  })
  it('never goes through a float: classic float-trap values are exact', () => {
    expect(parseDecimalToMinor('1.15', 2)).toBe(115n) // 1.15*100 === 114.99999999999999 in IEEE-754
    expect(parseDecimalToMinor('0.07', 2)).toBe(7n) // 0.07*100 === 7.000000000000001
    expect(parseDecimalToMinor('4.35', 2)).toBe(435n) // 4.35*100 === 434.99999999999994
  })
  it('handles values above 2^53 and 38-digit values at exponent 18', () => {
    expect(parseDecimalToMinor('90071992547409.93', 2)).toBe(9007199254740993n)
    expect(parseDecimalToMinor('123456789012345678.123456789012345678', 18)).toBe(123456789012345678123456789012345678n)
    expect(parseDecimalToMinor('9'.repeat(38), 0)).toBe(10n ** 38n - 1n)
    expect(parseDecimalToMinor('9'.repeat(39), 0)).toBeNull()
  })
  it('rejects more fractional digits than the exponent (never rounds, never truncates)', () => {
    expect(parseDecimalToMinor('1.005', 2)).toBeNull()
    expect(parseDecimalToMinor('1.000', 2)).toBeNull()
    expect(parseDecimalToMinor('1.5', 0)).toBeNull()
  })
  it('rejects empty/garbage/odd syntax', () => {
    for (const bad of ['', ' ', '.', '.5', '5.', '1,5', '1,000', '1e2', '+5', 'abc', '1 2', '0x1', 'NaN', 'Infinity', '--1', '1.2.3', '١٢']) {
      expect(parseDecimalToMinor(bad, 2), bad).toBeNull()
    }
    expect(parseDecimalToMinor('1', -1)).toBeNull()
    expect(parseDecimalToMinor('1', 2.5)).toBeNull()
    expect(parseDecimalToMinor('1', 37)).toBeNull()
  })
  it('negatives only when explicitly allowed; -0 is 0', () => {
    expect(parseDecimalToMinor('-1.50', 2)).toBeNull()
    expect(parseDecimalToMinor('-1.50', 2, { allowNegative: true })).toBe(-150n)
    expect(parseDecimalToMinor('-0.00', 2, { allowNegative: true })).toBe(0n)
  })
  it('trims surrounding whitespace only', () => {
    expect(parseDecimalToMinor('  2.5 ', 2)).toBe(250n)
  })
})

describe('formatMinor', () => {
  it.each(EXPONENTS)('exponent %i: pads, places the point and keeps full precision', (e) => {
    expect(formatMinor(0, e)).toBe(e === 0 ? '0' : `0.${'0'.repeat(e)}`)
    expect(formatMinor(1, e)).toBe(e === 0 ? '1' : `0.${'0'.repeat(e - 1)}1`)
    expect(formatMinor(10n ** BigInt(e) * 12n, e)).toBe(e === 0 ? '12' : `12.${'0'.repeat(e)}`)
  })
  it('is exact above 2^53 and for 38-digit values', () => {
    expect(formatMinor(BIG, 2)).toBe('90071992547409.93')
    expect(formatMinor(HUGE, 18)).toBe('123456789012345678.901234567890123456')
    expect(formatMinor('9'.repeat(38), 18)).toBe(`${'9'.repeat(20)}.${'9'.repeat(18)}`)
  })
  it('handles leading zeros and negatives', () => {
    expect(formatMinor('000123', 2)).toBe('1.23')
    expect(formatMinor('-5', 2)).toBe('-0.05')
    expect(formatMinor('-123456', 3)).toBe('-123.456')
    expect(formatMinor('-0', 2)).toBe('0.00')
  })
  it('optionally groups thousands with string operations only', () => {
    expect(formatMinor('123456789', 2, { group: true })).toBe('1,234,567.89')
    expect(formatMinor('-100000', 0, { group: true })).toBe('-100,000')
    expect(formatMinor('999', 0, { group: true })).toBe('999')
  })
  it('returns null (caller shows a raw fallback) for non-exact amounts or bad exponents', () => {
    expect(formatMinor(1.5, 2)).toBeNull()
    expect(formatMinor(9007199254740993, 2)).toBeNull() // already-rounded float: refuse rather than print a wrong amount
    expect(formatMinor('', 2)).toBeNull()
    expect(formatMinor('abc', 2)).toBeNull()
    expect(formatMinor('12', 99)).toBeNull()
    expect(formatMinor('12', -1)).toBeNull()
  })
})

describe('round trip parse -> format -> parse', () => {
  const samples = ['0', '1', '9', '10', '100', '12345', BIG, HUGE, '9'.repeat(38), '000042', '-7', '-9007199254740993']
  for (const e of EXPONENTS) {
    it(`exponent ${e}`, () => {
      for (const s of samples) {
        const n = parseMinor(s)!
        const text = formatMinor(n, e)!
        expect(parseDecimalToMinor(text, e, { allowNegative: true }), `${s} -> ${text}`).toBe(n)
      }
    })
  }
  it('format -> parse -> format is stable for user decimal text', () => {
    for (const e of EXPONENTS) {
      const text = formatMinor(HUGE, e)!
      expect(formatMinor(parseDecimalToMinor(text, e)!, e)).toBe(text)
    }
  })
})

describe('compareMinor / minorGreater', () => {
  it('compares by BigInt, correct above 2^53 where floats collapse', () => {
    expect(9007199254740993 === 9007199254740992).toBe(true) // the float hazard being guarded against
    expect(compareMinor('9007199254740993', '9007199254740992')).toBe(1)
    expect(compareMinor('9007199254740992', '9007199254740993')).toBe(-1)
    expect(compareMinor('007', 7)).toBe(0)
    expect(compareMinor('-1', '0')).toBe(-1)
    expect(minorGreater(HUGE, '123456789012345678901234567890123455')).toBe(true)
    expect(minorGreater('9', '10')).toBe(false)
  })
  it('is null / false when either side is not exact', () => {
    expect(compareMinor('x', 1)).toBeNull()
    expect(compareMinor(1.5, 1)).toBeNull()
    expect(minorGreater('x', '1')).toBe(false)
  })
})

describe('toWireInteger', () => {
  it('returns an exact number inside the safe range and null outside it', () => {
    expect(toWireInteger(12345n)).toBe(12345)
    expect(toWireInteger(9007199254740991n)).toBe(9007199254740991)
    expect(toWireInteger(-9007199254740991n)).toBe(-9007199254740991)
    expect(toWireInteger(9007199254740992n)).toBeNull()
    expect(toWireInteger(10n ** 30n)).toBeNull()
  })
})

describe('isValidExponent', () => {
  it('accepts 0..36 integers only', () => {
    expect(isValidExponent(0)).toBe(true)
    expect(isValidExponent(18)).toBe(true)
    expect(isValidExponent(36)).toBe(true)
    for (const bad of [-1, 37, 1.5, NaN, '2', null, undefined]) expect(isValidExponent(bad)).toBe(false)
  })
})

describe('parseJsonExact (JSON number precision guard)', () => {
  it('plain JSON.parse corrupts an int64 above 2^53 - the hazard', () => {
    expect((JSON.parse(`{"a":${BIG}}`) as { a: number }).a.toString()).not.toBe(BIG)
  })
  it('keeps unsafe integers exact as digit strings and leaves everything else alone', () => {
    const text = `{"available_balance":${BIG},"cash_balance":12345,"neg":-9223372036854775808,"frac":1.5,"exp":1e30,"safe":9007199254740991,"list":[${BIG},3],"s":"id ${BIG} \\" ${BIG}","n":null,"b":true}`
    const v = parseJsonExact<Record<string, unknown>>(text)
    expect(v.available_balance).toBe(BIG)
    expect(v.cash_balance).toBe(12345)
    expect(v.neg).toBe('-9223372036854775808')
    expect(v.frac).toBe(1.5)
    expect(v.exp).toBe(1e30)
    expect(v.safe).toBe(9007199254740991)
    expect(v.list).toEqual([BIG, 3])
    expect(v.s).toBe(`id ${BIG} " ${BIG}`) // digits inside a string literal are untouched
    expect(v.n).toBeNull()
    expect(v.b).toBe(true)
    expect(formatMinor(v.available_balance, 2)).toBe('90071992547409.93')
  })
  it('max int64 survives exactly', () => {
    const v = parseJsonExact<{ x: string }>('{"x":9223372036854775807}')
    expect(v.x).toBe('9223372036854775807')
    expect(formatMinor(v.x, 0)).toBe('9223372036854775807')
  })
  it('behaves exactly like JSON.parse for ordinary payloads, including errors', () => {
    const t = '{"a":[1,2,{"b":"c"}],"d":null}'
    expect(parseJsonExact(t)).toEqual(JSON.parse(t))
    expect(() => parseJsonExact('{"a":' + BIG)).toThrow()
    expect(() => parseJsonExact('nope')).toThrow()
  })
})

describe('shared module drift guard', () => {
  it('b2c/src/lib/amount.ts and backoffice/src/lib/amount.ts are byte-identical', () => {
    const b2c = readFileSync(resolve(__dirname, '../../../b2c/src/lib/amount.ts'), 'utf8')
    const bo = readFileSync(resolve(__dirname, '../../../backoffice/src/lib/amount.ts'), 'utf8')
    expect(bo).toBe(b2c)
  })
})
