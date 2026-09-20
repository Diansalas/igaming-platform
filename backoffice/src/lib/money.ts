/**
 * Formats an integer minor-units amount (e.g. cents) as a human-readable
 * decimal string using the asset's real decimal exponent - never a
 * hardcoded assumption. If the exponent is unknown, the raw minor-units
 * value is shown with an explicit qualifier rather than silently
 * formatted as whole units, which would misrepresent the real amount.
 *
 * This performs NO business logic (no rounding decisions, no currency
 * conversion) - it is presentation-only. The authoritative integer
 * amount is always what the server returns and what any mutation call
 * uses; this function never feeds back into a request.
 */
export function formatMoney(amountMinorUnits: number, assetCode: string, decimalExponent: number | undefined): string {
  if (decimalExponent === undefined) {
    return `${amountMinorUnits} ${assetCode} (exponent unknown)`
  }
  if (decimalExponent === 0) {
    return `${amountMinorUnits} ${assetCode}`
  }
  const divisor = 10 ** decimalExponent
  const major = amountMinorUnits / divisor
  return `${major.toFixed(decimalExponent)} ${assetCode}`
}
