// Client-side checks that MIRROR the server rules for the three operating-basis payloads (internal/licensingchange/
// operating_basis_kinds.go and launchgov.LintFreeText). They exist only to save a round trip: the server stays authoritative and
// every refusal is rendered. A value this file accepts can still be refused (an unknown country, an unknown operation, a
// licensee mismatch, a lint shape this file does not know); a value it refuses would have been refused by the server too.

export const MAX_REF = 200
export const MAX_NOTE = 1000
export const MAX_CODE = 64
export const MAX_CODES = 64
export const REVIEW_DUE_SOON_DAYS = 30

const encoder = new TextEncoder()
const bytes = (s: string) => encoder.encode(s).length
// eslint-disable-next-line no-control-regex
const CONTROL = /[\u0000-\u001f\u007f-\u009f\u200b-\u200f\u2028\u2029\u202a-\u202e\u2060-\u2064\ufeff]/
const EMAIL = /[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+/
const OBFUSCATED_EMAIL = /[a-z0-9._%+-]+\s*(?:\(at\)|\[at\]|\{at\}|\bat\b)\s*[a-z0-9][a-z0-9-]*\s*(?:\.|\(dot\)|\[dot\]|\{dot\}|\bdot\b)\s*[a-z]{2,}/i
const IBAN = /\b[A-Z]{2}\d{2}[ ]?[A-Z0-9]{4}[ ]?(?:[A-Z0-9]{2,4}[ ]?){2,8}[A-Z0-9]{1,4}\b/
const WALLET = /\b0x[0-9a-fA-F]{40}\b|\b(?:bc1|tb1)[ac-hj-np-z02-9]{11,71}\b|\b[13][a-km-zA-HJ-NP-Z1-9]{25,34}\b|\bT[1-9A-HJ-NP-Za-km-z]{33}\b/
const PHONE = /\+\d[\d\s().-]{6,}\d|\(?\d{2,4}\)?[\s.-]\d{3,4}[\s.-]\d{3,4}/
const ISO_DATE = /\b\d{4}-\d{2}-\d{2}\b/g
const DIGIT_RUN = /\d(?:[^A-Za-z0-9]*\d){8,}/

/** An opaque, trimmed reference or note of 1..max bytes with no control characters and no contact-detail or account shapes. */
export function refProblem(v: string, max: number = MAX_REF): string | null {
  if (v === '') return 'This is required.'
  if (v !== v.trim()) return 'Remove the spaces at the start or end.'
  if (bytes(v) > max) return `Use at most ${max} bytes.`
  if (CONTROL.test(v)) return 'Control characters are not allowed.'
  if (v.includes('@')) return 'An "@" is not allowed (do not enter an email address).'
  const s = v.normalize('NFKC')
  if (EMAIL.test(s) || OBFUSCATED_EMAIL.test(s)) return 'Do not enter an email address.'
  if (IBAN.test(s)) return 'Do not enter an IBAN.'
  if (WALLET.test(s)) return 'Do not enter a wallet address.'
  const t = s.replace(ISO_DATE, ' ')
  if (PHONE.test(t) || DIGIT_RUN.test(t)) return 'Do not enter a phone, card or account number (a run of nine or more digits).'
  return null
}

/** Optional reference: blank is fine (sent as null); a non-blank value follows refProblem. */
export function optRefProblem(v: string, max: number = MAX_REF): string | null {
  return v === '' ? null : refProblem(v, max)
}

const ROLE = /^[a-z][a-z0-9_.:-]{1,99}$/
/** A responsible FUNCTION code (for example head_of_compliance), never a person's name. */
export function roleProblem(v: string): string | null {
  if (v === '') return 'This is required.'
  return ROLE.test(v) ? null : 'Use a function code: lower-case letters, digits, _ . : - (2 to 100 characters, starting with a letter), not a person\'s name.'
}

export function countryProblem(v: string): string | null {
  if (v === '') return 'This is required.'
  return /^[A-Z]{2}$/.test(v) ? null : 'Use two upper-case letters (ISO 3166-1 alpha-2). The server checks that the code exists.'
}

/** operation_code and product_code: 1..64 bytes, trimmed, no control characters. The server checks the vocabulary row exists. */
export function codeProblem(v: string): string | null {
  if (v === '') return 'This is required.'
  if (v !== v.trim() || CONTROL.test(v)) return 'No leading or trailing spaces and no control characters.'
  return bytes(v) > MAX_CODE ? `Use at most ${MAX_CODE} bytes.` : null
}

/** A comma separated list of product codes: at most 64, each valid, no duplicates. Blank is an empty list. */
export function productListProblem(v: string): string | null {
  const items = splitList(v)
  if (items.length > MAX_CODES) return `Use at most ${MAX_CODES} entries.`
  const seen = new Set<string>()
  for (const i of items) {
    const p = codeProblem(i)
    if (p) return `"${i.slice(0, 20)}": ${p}`
    if (seen.has(i)) return 'An entry is repeated.'
    seen.add(i)
  }
  return null
}

export function splitList(v: string): string[] {
  return v.split(',').map((s) => s.trim()).filter((s) => s !== '')
}

/** YYYY-MM-DD and a real calendar date. */
export function dateProblem(v: string): string | null {
  if (v === '') return 'This is required.'
  if (!/^\d{4}-\d{2}-\d{2}$/.test(v)) return 'Use a date (YYYY-MM-DD).'
  const d = new Date(`${v}T00:00:00Z`)
  return Number.isNaN(d.getTime()) || d.toISOString().slice(0, 10) !== v ? 'That is not a real date.' : null
}

export const UUID_RE = /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/

/** Local calendar date of `now` as YYYY-MM-DD. Display hint only: the server compares against the database clock. */
export function localDate(now: Date = new Date()): string {
  const p = (n: number) => String(n).padStart(2, '0')
  return `${now.getFullYear()}-${p(now.getMonth() + 1)}-${p(now.getDate())}`
}

function dayNumber(iso: string): number {
  return Date.parse(`${iso}T00:00:00Z`) / 86_400_000 // UTC midnights: an exact integer
}

export type ReviewState = 'none' | 'ok' | 'due_soon' | 'overdue' | 'rereview'

/**
 * Display classification of a basis review. `rereview` (flagged) and `overdue` both mean the resolver no longer permits. The due
 * date itself still stands (overdue is strictly after it), mirroring the resolver. A hint only: the server decides.
 */
export function reviewState(reviewDueAt: string | null, needsRereview: boolean, today: string = localDate()): { state: ReviewState; daysLeft: number | null } {
  if (needsRereview) return { state: 'rereview', daysLeft: null }
  if (!reviewDueAt || dateProblem(reviewDueAt) !== null) return { state: 'none', daysLeft: null }
  const left = dayNumber(reviewDueAt) - dayNumber(today)
  if (left < 0) return { state: 'overdue', daysLeft: left }
  if (left <= REVIEW_DUE_SOON_DAYS) return { state: 'due_soon', daysLeft: left }
  return { state: 'ok', daysLeft: left }
}

/**
 * The class the server will compute for a review record: widening iff the due date moves later or the re-review flag goes from
 * true to false; otherwise narrowing. A hint to show the approval path; the database classifies.
 */
export function reviewClass(oldDue: string | null, oldFlag: boolean, newDue: string, newFlag: boolean): 'widening' | 'narrowing' {
  const later = oldDue === null || newDue > oldDue
  if (later && newDue !== oldDue) return 'widening'
  if (oldFlag && !newFlag) return 'widening'
  return 'narrowing'
}
