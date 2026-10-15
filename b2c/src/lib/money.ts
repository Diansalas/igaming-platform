// Presentation-only money formatting. This app has no per-asset decimal
// exponent registry endpoint exposed to it this stage (unlike the Back
// Office's withdrawal admin endpoints, which added `decimal_exponent`
// fields) - every wallet/bet/deposit amount the sportsbook/wallet APIs
// return is documented as minor units with no exponent alongside it. To
// avoid silently misrepresenting an amount (e.g. showing cents as whole
// units), known common fiat asset codes are mapped to their standard
// exponent here; anything unrecognized is shown as a raw minor-units
// value with an explicit qualifier rather than a guessed conversion. This
// performs NO business logic - the authoritative integer amount is always
// what the server returns, and this function's output never feeds back
// into a request.
const KNOWN_EXPONENTS: Record<string, number> = {
  USD: 2,
  EUR: 2,
  GBP: 2,
  BRL: 2,
  MXN: 2,
  JPY: 0,
}

export function knownExponentFor(assetCode: string): number | undefined {
  return KNOWN_EXPONENTS[assetCode.toUpperCase()]
}

export function formatMoney(amountMinorUnits: number, assetCode: string): string {
  const exponent = knownExponentFor(assetCode)
  if (exponent === undefined) {
    return `${amountMinorUnits} ${assetCode} (minor units)`
  }
  if (exponent === 0) {
    return `${amountMinorUnits} ${assetCode}`
  }
  const divisor = 10 ** exponent
  return `${(amountMinorUnits / divisor).toFixed(exponent)} ${assetCode}`
}

/**
 * Converts a user-entered major-unit string (e.g. "25.00") to integer
 * minor units for a request. Returns null if not a valid positive amount
 * OR if assetCode's exponent is unknown - guessing an exponent on the
 * OUTBOUND path (unlike display's explicit "(minor units)" fallback)
 * would silently submit a wrong amount, so this refuses instead. Every
 * asset code this app currently offers (DepositPage's selector, the bet
 * slip's brandConfig.defaultAssetCode) is in KNOWN_EXPONENTS.
 */
export function toMinorUnits(majorAmountInput: string, assetCode: string): number | null {
  const trimmed = majorAmountInput.trim()
  if (trimmed === '') return null
  const value = Number(trimmed)
  if (!Number.isFinite(value) || value <= 0) return null
  const exponent = knownExponentFor(assetCode)
  if (exponent === undefined) return null
  return Math.round(value * 10 ** exponent)
}

// ---- string-exact helpers for NUMERIC(38,0) values (responsible-gaming limits) ----------------------
// RG limit values are decimal strings of minor units (up to 38 digits). They never pass through a float:
// parsing and display use string arithmetic only.

/**
 * Converts a user-entered major-unit string ("25", "25.5", "25.00") to a decimal string of minor units for
 * a request, or null if it is not a positive amount with at most the asset's decimals, if the asset's
 * exponent is unknown (guessing on the outbound path would submit a wrong amount), or if it exceeds 38 digits.
 */
export function majorToMinorString(majorAmountInput: string, assetCode: string): string | null {
  const exponent = knownExponentFor(assetCode)
  if (exponent === undefined) return null
  const m = /^(\d+)(?:\.(\d+))?$/.exec(majorAmountInput.trim())
  if (!m) return null
  const whole = m[1]
  const frac = m[2] ?? ''
  if (frac.length > exponent) return null
  const digits = (whole + frac.padEnd(exponent, '0')).replace(/^0+/, '')
  if (digits === '' || digits.length > 38) return null
  return digits
}

/** Formats a decimal string of minor units for display, exactly (no float). Unknown exponents are shown as raw minor units. */
export function formatMinorString(minor: string, assetCode: string): string {
  const exponent = knownExponentFor(assetCode)
  if (!/^\d+$/.test(minor)) return `${minor} ${assetCode}`
  if (exponent === undefined) return `${minor} ${assetCode} (minor units)`
  if (exponent === 0) return `${minor} ${assetCode}`
  const padded = minor.padStart(exponent + 1, '0')
  return `${padded.slice(0, -exponent)}.${padded.slice(-exponent)} ${assetCode}`
}

/** True when decimal string a is strictly greater than b (both non-negative integers, no float). */
export function minorGreater(a: string, b: string): boolean {
  const x = a.replace(/^0+/, '') || '0'
  const y = b.replace(/^0+/, '') || '0'
  if (x.length !== y.length) return x.length > y.length
  return x > y
}
