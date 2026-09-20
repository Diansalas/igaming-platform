/**
 * Generates a fresh idempotency key for one user-initiated ATTEMPT (a bet
 * slip composition, a deposit form submission) - not for one HTTP
 * request. The caller must hold the returned key and reuse it across any
 * retry of that SAME attempt (a network error, a timeout, a re-click
 * after a genuine server error): the whole point of an idempotency key is
 * that a retry after an ambiguous outcome (request may have actually
 * committed) never creates a second financial effect. Only call this
 * again when the user starts a GENUINELY NEW attempt (a different
 * selection, a cleared/reset form) - never on every submit click. See
 * BetSlipContext.tsx's setSelection for the pattern this is meant to
 * support: mint once per composition, hold it, never regenerate on retry.
 */
export function newIdempotencyKey(): string {
  if (typeof crypto !== 'undefined' && typeof crypto.randomUUID === 'function') {
    return crypto.randomUUID()
  }
  // Fallback for environments without crypto.randomUUID (older browsers) -
  // not cryptographically strong, but this key only needs to be unique
  // per submit action, not secret.
  return `key-${Date.now()}-${Math.random().toString(36).slice(2)}`
}
