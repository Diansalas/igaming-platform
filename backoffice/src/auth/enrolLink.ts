// Reads the one-time token from an emailed link, keeps it in module memory only, and rewrites the address bar
// immediately (history.replaceState: the current session-history entry only; the browser's global history may still
// hold the original URL).
//
// Two link shapes exist (ADR 0119 contract sections 1.3 and 5); the FRAGMENT form is the ONLY supported one for both,
// because a fragment is never sent to a server or in a Referer header:
//   - enrolment / MFA reset:  /#/enrol?token=...
//   - staff invitation:       /#/enrol?token=...&invite=1   (what the invitation email contains)
//                             /#/accept-invitation?token=... (accepted as an equivalent)
// The two tokens are kept apart: an enrolment token is only ever offered to the redeem endpoint, an invitation token
// only to the accept endpoint, so a link of one kind can never be replayed against the other.
let pendingEnrolToken: string | null = null
let pendingInviteToken: string | null = null

export const ENROL_PATH = '/enrol'
export const ACCEPT_INVITATION_PATH = '/accept-invitation'

export function captureEnrolLinkToken(
  loc: Pick<Location, 'pathname' | 'search' | 'hash'> = window.location,
  hist: Pick<History, 'replaceState'> = window.history,
): void {
  let token: string | null = null
  let invite = false
  let matched = false
  const h = loc.hash
  if (h.startsWith('#/enrol') || h.startsWith('#/accept-invitation')) {
    matched = true
    const q = h.indexOf('?')
    if (q >= 0) {
      const params = new URLSearchParams(h.slice(q + 1))
      token = params.get('token')
      invite = h.startsWith('#/accept-invitation') || params.get('invite') === '1'
    } else {
      invite = h.startsWith('#/accept-invitation')
    }
  } else if (loc.pathname === ENROL_PATH || loc.pathname === ACCEPT_INVITATION_PATH) {
    // Path/query form is NOT a supported link shape: a token in a query string reaches server logs and
    // Referer headers. Never keep it; strip the URL and let the page show the fixed invalid-link message.
    matched = true
    invite = loc.pathname === ACCEPT_INVITATION_PATH
  }
  if (!matched) return
  if (token) {
    if (invite) pendingInviteToken = token
    else pendingEnrolToken = token
  }
  hist.replaceState(null, '', invite ? ACCEPT_INVITATION_PATH : ENROL_PATH)
}

/** Non-destructive read (StrictMode mounts effects twice); call clearEnrolLinkToken once consumed. */
export function getEnrolLinkToken(): string | null {
  return pendingEnrolToken
}

export function clearEnrolLinkToken(): void {
  pendingEnrolToken = null
}

/** Non-destructive read of the invitation token; call clearInviteLinkToken once consumed or abandoned. */
export function getInviteLinkToken(): string | null {
  return pendingInviteToken
}

export function clearInviteLinkToken(): void {
  pendingInviteToken = null
}
