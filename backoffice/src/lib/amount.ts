// Exact money arithmetic primitives (U-5). CLAUDE.md: never floating point for money; integer minor units with a
// per-asset exponent; NUMERIC(38,0) for crypto-scale values. Everything here uses BigInt / string arithmetic only.
//
// THIS FILE IS BYTE-IDENTICAL in b2c/src/lib/amount.ts and backoffice/src/lib/amount.ts (the two apps are separate
// packages with no shared workspace); a test in each app asserts the copies match, so edit both together.
//
// Wire model: the platform API sends most money as decimal STRINGS of integer minor units, but some DTOs still
// declare `integer int64` (wallet balances, deposits, withdrawals, bets, casino rounds, capability limits). JSON.parse
// silently corrupts integers above 2^53, so apiFetch parses through parseJsonExact(), which turns any integer literal
// outside the safe range into an exact decimal string. Amount fields are therefore typed `number | string` (MinorAmount)
// and are only ever interpreted through parseMinor()/formatMinor()/compareMinor() below - never with Number().

/** A minor-units amount as it can appear on the wire after parseJsonExact: a safe-integer JSON number or an exact digit string. */
export type MinorAmount = number | string | bigint

const MAX_DIGITS = 40 // NUMERIC(38,0) plus headroom; bounds BigInt work on hostile input.
const MAX_EXPONENT = 36
const MAX_SAFE = BigInt(Number.MAX_SAFE_INTEGER)
const MIN_SAFE = -MAX_SAFE

export function isValidExponent(exponent: unknown): exponent is number {
  return typeof exponent === 'number' && Number.isInteger(exponent) && exponent >= 0 && exponent <= MAX_EXPONENT
}

/**
 * Interprets a wire/state amount as an exact integer of minor units. Accepts a bigint, a JSON number that is a safe
 * integer (anything else cannot be exact, so it is refused), or an optionally signed digit string (leading zeros ok).
 * Returns null for anything else (empty, garbage, fractional, exponent notation, NaN, unsafe numbers, > 40 digits).
 */
export function parseMinor(value: unknown): bigint | null {
  if (typeof value === 'bigint') return value
  if (typeof value === 'number') return Number.isSafeInteger(value) ? BigInt(value) : null
  if (typeof value !== 'string') return null
  const m = /^(-?)(\d+)$/.exec(value)
  if (!m || m[2].length > MAX_DIGITS) return null
  const n = BigInt(m[2])
  return m[1] === '-' && n !== 0n ? -n : n
}

export interface ParseDecimalOptions {
  /** Accept a leading '-' (adjustments / corrections). Default false: player-entered amounts are never negative. */
  allowNegative?: boolean
}

/**
 * Parses the user's decimal text ("25", "25.5", "0.05") into integer minor units by exact string arithmetic.
 * Rejects (null): empty/garbage, signs/exponents/separators, more fractional digits than `exponent` (never rounds),
 * an invalid exponent, and results over 38 digits. Zero is returned as 0n; callers decide whether zero is acceptable.
 */
export function parseDecimalToMinor(input: string, exponent: number, options: ParseDecimalOptions = {}): bigint | null {
  if (typeof input !== 'string' || !isValidExponent(exponent)) return null
  const m = /^(-?)(\d+)(?:\.(\d+))?$/.exec(input.trim())
  if (!m) return null
  const [, sign, whole, frac = ''] = m
  if (sign === '-' && !options.allowNegative) return null
  if (frac.length > exponent) return null
  const digits = (whole + frac.padEnd(exponent, '0')).replace(/^0+(?=\d)/, '')
  if (digits.length > 38) return null
  const n = BigInt(digits)
  return sign === '-' && n !== 0n ? -n : n
}

export interface FormatOptions {
  /** Insert ',' thousands separators in the whole part (string operation, locale-independent). */
  group?: boolean
}

/**
 * Formats integer minor units as an exact decimal string with exactly `exponent` fractional digits. Returns null when
 * the amount is not an exact integer (see parseMinor) or the exponent is invalid - callers show an explicit fallback
 * rather than a guess.
 */
export function formatMinor(value: unknown, exponent: number, options: FormatOptions = {}): string | null {
  const n = parseMinor(value)
  if (n === null || !isValidExponent(exponent)) return null
  const neg = n < 0n
  const digits = (neg ? -n : n).toString()
  const padded = digits.padStart(exponent + 1, '0')
  let whole = padded.slice(0, padded.length - exponent)
  if (options.group) whole = whole.replace(/\B(?=(\d{3})+(?!\d))/g, ',')
  const frac = exponent === 0 ? '' : `.${padded.slice(padded.length - exponent)}`
  return `${neg ? '-' : ''}${whole}${frac}`
}

/** -1 | 0 | 1, or null if either side is not an exact integer amount. Comparison is by BigInt, never by float. */
export function compareMinor(a: unknown, b: unknown): -1 | 0 | 1 | null {
  const x = parseMinor(a)
  const y = parseMinor(b)
  if (x === null || y === null) return null
  return x < y ? -1 : x > y ? 1 : 0
}

/** True only when both are exact amounts and a > b. */
export function minorGreater(a: unknown, b: unknown): boolean {
  return compareMinor(a, b) === 1
}

/**
 * Converts exact minor units to a JSON number for a request body whose DTO field is `integer int64`. Returns null
 * when the value cannot be carried exactly in a JS number (outside +/-(2^53-1)): the caller must refuse the input
 * rather than send a rounded amount. (Amounts above 2^53 minor units are not offered to players today.)
 */
export function toWireInteger(value: bigint): number | null {
  if (value > MAX_SAFE || value < MIN_SAFE) return null
  return Number(value) // exact: bounded to the safe-integer range just above
}

const NUMBER_TOKEN = /-?\d+(\.\d+)?([eE][+-]?\d+)?/y

/**
 * JSON.parse that never loses integer precision: any integer literal outside the safe range (which JSON.parse would
 * round to a float) is replaced by the same digits as a JSON string, so the exact value survives as a MinorAmount
 * string. Safe integers, fractions and exponent literals are parsed as usual; text inside strings is untouched.
 */
export function parseJsonExact<T = unknown>(text: string): T {
  if (!/\d{16}/.test(text)) return JSON.parse(text) as T // fast path: no integer literal can exceed 2^53
  let out = ''
  let i = 0
  let last = 0
  const n = text.length
  while (i < n) {
    const c = text.charCodeAt(i)
    if (c === 34) {
      // string literal: skip to its closing quote, honouring escapes
      i++
      while (i < n && text.charCodeAt(i) !== 34) i += text.charCodeAt(i) === 92 ? 2 : 1
      i++
    } else if (c === 45 || (c >= 48 && c <= 57)) {
      NUMBER_TOKEN.lastIndex = i
      const m = NUMBER_TOKEN.exec(text)
      const tok = m ? m[0] : text[i]
      if (m && m[1] === undefined && m[2] === undefined) {
        const v = BigInt(tok)
        if (v > MAX_SAFE || v < MIN_SAFE) {
          out += `${text.slice(last, i)}"${tok}"`
          last = i + tok.length
        }
      }
      i += tok.length
    } else {
      i++
    }
  }
  return JSON.parse(out + text.slice(last)) as T
}
