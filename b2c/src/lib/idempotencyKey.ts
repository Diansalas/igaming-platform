/**
 * Generates a fresh idempotency key for exactly one user-initiated submit
 * action (a bet-slip "Place Bet" click, a deposit form submission). Never
 * reused automatically on retry - a new user action always calls this
 * again, per the Stage 6 directive's exact contract.
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
