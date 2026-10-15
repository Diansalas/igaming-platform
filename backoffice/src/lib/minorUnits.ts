/**
 * Formats a decimal STRING of integer minor units with the asset's exponent using BigInt arithmetic only (ADR 0114
 * principle 5: never floating point, no precision loss for 38-digit amounts). Anything that is not an optionally
 * signed integer string, or an exponent outside 0..36, is shown raw with an explicit qualifier instead of being
 * guessed at.
 */
export function formatMinorUnits(amount: string | null | undefined, exponent: number | null | undefined, assetCode: string | null | undefined): string {
  if (amount === null || amount === undefined) return 'Not available'
  const asset = typeof assetCode === 'string' && /^[A-Za-z0-9_.-]{1,16}$/.test(assetCode) ? assetCode : 'unknown asset'
  if (!/^-?\d+$/.test(amount) || exponent === null || exponent === undefined || !Number.isInteger(exponent) || exponent < 0 || exponent > 36) {
    return `${/^-?\d{1,40}$/.test(amount) ? amount : 'unprintable'} minor units of ${asset} (exponent unknown)`
  }
  const neg = amount.startsWith('-')
  const digits = BigInt(neg ? amount.slice(1) : amount).toString()
  if (exponent === 0) return `${neg ? '-' : ''}${digits} ${asset}`
  const padded = digits.padStart(exponent + 1, '0')
  const whole = padded.slice(0, padded.length - exponent)
  const frac = padded.slice(padded.length - exponent)
  return `${neg ? '-' : ''}${whole}.${frac} ${asset}`
}

/** "123.45 EUR (12345 minor units)": the major-unit form AND the exact minor units, so a reviewer sees both. */
export function formatAmountBoth(amount: string | null | undefined, exponent: number | null | undefined, assetCode: string | null | undefined): string {
  const major = formatMinorUnits(amount, exponent, assetCode)
  if (amount === null || amount === undefined || exponent === null || exponent === undefined || major.includes('exponent unknown') || !/^-?\d+$/.test(amount)) return major
  return `${major} (${amount} minor units)`
}

/** Whole-unit age such as "3d 4h" or "12m", from an ISO timestamp and a reference instant (default now). */
export function formatAge(iso: string | null | undefined, now: number = Date.now()): string {
  if (!iso) return 'Not available'
  const t = Date.parse(iso)
  if (Number.isNaN(t)) return 'Not available'
  let s = Math.max(0, Math.floor((now - t) / 1000))
  const d = Math.floor(s / 86400)
  s -= d * 86400
  const h = Math.floor(s / 3600)
  s -= h * 3600
  const m = Math.floor(s / 60)
  if (d > 0) return `${d}d ${h}h`
  if (h > 0) return `${h}h ${m}m`
  return `${m}m`
}
