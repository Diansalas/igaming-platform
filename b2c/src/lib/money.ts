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
import { formatMinor, minorGreater, parseDecimalToMinor, toWireInteger, type MinorAmount } from './amount'

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

export function formatMoney(amountMinorUnits: MinorAmount, assetCode: string): string {
  const exponent = knownExponentFor(assetCode)
  const raw = typeof amountMinorUnits === 'bigint' ? amountMinorUnits.toString() : String(amountMinorUnits)
  if (exponent === undefined) {
    return `${raw} ${assetCode} (minor units)`
  }
  // Exact BigInt/string formatting (lib/amount.ts); an amount that is not an exact integer is shown raw, never guessed.
  const text = formatMinor(amountMinorUnits, exponent)
  return text === null ? `${raw} ${assetCode}` : `${text} ${assetCode}`
}

/**
 * Converts a user-entered major-unit string (e.g. "25.00") to integer minor units for a request body whose DTO field
 * is an int64 JSON number. Exact string arithmetic; returns null if the text is not a positive amount, has more
 * fractional digits than the asset's exponent (never rounded), the asset's exponent is unknown - guessing an exponent
 * on the OUTBOUND path (unlike display's explicit "(minor units)" fallback) would silently submit a wrong amount - or
 * the amount cannot be carried exactly in a JS number (> 2^53-1 minor units). Every asset code this app currently
 * offers (DepositPage's selector, the bet slip's brandConfig.defaultAssetCode) is in KNOWN_EXPONENTS.
 */
export function toMinorUnits(majorAmountInput: string, assetCode: string): number | null {
  const exponent = knownExponentFor(assetCode)
  if (exponent === undefined) return null
  const minor = parseDecimalToMinor(majorAmountInput, exponent)
  if (minor === null || minor <= 0n) return null
  return toWireInteger(minor)
}

// ---- string-exact helpers for NUMERIC(38,0) values (responsible-gaming limits) ----------------------
// RG limit values are decimal strings of minor units (up to 38 digits). They never pass through a float.

/**
 * Converts a user-entered major-unit string ("25", "25.5", "25.00") to a decimal string of minor units for
 * a request, or null if it is not a positive amount with at most the asset's decimals, if the asset's
 * exponent is unknown (guessing on the outbound path would submit a wrong amount), or if it exceeds 38 digits.
 */
export function majorToMinorString(majorAmountInput: string, assetCode: string): string | null {
  const exponent = knownExponentFor(assetCode)
  if (exponent === undefined) return null
  const minor = parseDecimalToMinor(majorAmountInput, exponent)
  return minor === null || minor <= 0n ? null : minor.toString()
}

/** Formats a decimal string of minor units for display, exactly (no float). Unknown exponents are shown as raw minor units. */
export function formatMinorString(minor: string, assetCode: string): string {
  const exponent = knownExponentFor(assetCode)
  if (!/^\d+$/.test(minor)) return `${minor} ${assetCode}`
  if (exponent === undefined) return `${minor} ${assetCode} (minor units)`
  const text = formatMinor(minor, exponent)
  return text === null ? `${minor} ${assetCode}` : `${text} ${assetCode}`
}

export { minorGreater }
