// Reads the one-time enrolment token from the `/#/enrol?token=...` link
// (ADR 0119 contract section 1.3; the fragment form is the ONLY supported one),
// keeps it in module memory only, and rewrites the address bar immediately
// (history.replaceState: the current session-history entry only; the browser's
// global history may still hold the original URL).
let pendingToken: string | null = null

export const ENROL_PATH = '/enrol'

export function captureEnrolLinkToken(
  loc: Pick<Location, 'pathname' | 'search' | 'hash'> = window.location,
  hist: Pick<History, 'replaceState'> = window.history,
): void {
  let token: string | null = null
  let matched = false
  if (loc.hash.startsWith('#/enrol')) {
    matched = true
    const q = loc.hash.indexOf('?')
    if (q >= 0) token = new URLSearchParams(loc.hash.slice(q + 1)).get('token')
  } else if (loc.pathname === ENROL_PATH) {
    // Path/query form is NOT a supported link shape: a token in a query string reaches server logs and
    // Referer headers. Never keep it; strip the URL and let EnrolPage show the fixed invalid-link page.
    matched = true
  }
  if (!matched) return
  if (token) pendingToken = token
  hist.replaceState(null, '', ENROL_PATH)
}

/** Non-destructive read (StrictMode mounts effects twice); call clearEnrolLinkToken once consumed. */
export function getEnrolLinkToken(): string | null {
  return pendingToken
}

export function clearEnrolLinkToken(): void {
  pendingToken = null
}
