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

/**
 * Formats a minor-units amount that arrives as a DECIMAL STRING with no
 * accompanying `decimal_exponent` (the BonusChangeRequest and Grant API
 * schemas carry `amount_at_request`/`remaining_bonus_balance` as
 * decimal-string minor units - correctly avoiding float precision loss on
 * the wire per CLAUDE.md's money rules - but, unlike WithdrawalAdmin/
 * AdminBet/AdminRound, do not also return the asset's decimal_exponent).
 *
 * Never guesses a scale by parsing the string into a JS `number` and
 * dividing (which would both reintroduce float precision loss for large
 * crypto amounts - exactly what a decimal-string wire format exists to
 * avoid - and require assuming an exponent nothing here actually knows).
 * Rendering e.g. "5000 EUR" when the real amount is "50.00 EUR" would be
 * a materially misleading amount at a four-eyes financial-approval
 * decision point - worse than an explicit "raw minor units" label. Kept
 * as its own function (rather than overloading formatMoney) so the two
 * call sites read honestly rather than silently formatting a value this
 * app cannot actually scale correctly.
 *
 * This is a presentation-only gap fix: the real fix is the API adding
 * decimal_exponent to these two schemas, mirroring the existing
 * WithdrawalAdmin precedent - flagged for backend/API follow-up, not
 * something this frontend-only change can complete on its own.
 */
export function formatUnscaledAmount(amountMinorUnits: string, assetCode: string): string {
  return `${amountMinorUnits} ${assetCode} (raw minor units, exponent unknown)`
}
