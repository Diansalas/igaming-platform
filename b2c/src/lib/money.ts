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
