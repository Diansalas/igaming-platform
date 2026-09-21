import { useCallback, useRef } from 'react'

/**
 * Client-side double-submit guard for mutating admin actions - most
 * importantly the financial approval actions (withdrawal approve/reject,
 * bonus change-request approve/reject) where a stray second request is a
 * genuine operational risk (a confusing duplicate-decision error surfaced
 * to the approver, a second audit entry, a race against the server's own
 * four-eyes/idempotency checks) even though the SERVER remains the
 * authoritative, idempotent control (unique constraints, state-machine
 * transitions, `withdrawal_approvals`/`bonus_change_approvals` - see
 * CLAUDE.md's idempotency rule). This is defense-in-depth only, never a
 * substitute for server-side enforcement.
 *
 * Why this exists ON TOP OF `ConfirmDialog`'s existing
 * `disabled={isSubmitting}` on its Confirm button: `isSubmitting` reflects
 * a mutation's `isPending` flag, which only becomes true once React has
 * re-rendered after the first click's state change. Two click events
 * landing close enough together (a genuine double-click, or a user
 * clicking again while waiting on a slow connection) can both be
 * dispatched before that re-render commits, invoking the click handler
 * twice against the still-stale "not submitting" state. A `useRef` flip
 * is synchronous and immediate regardless of React's render timing, so it
 * closes that race outright rather than narrowing it.
 */
export function useOnceGuard(): { run: (action: () => void) => void; release: () => void } {
  const runningRef = useRef(false)

  const run = useCallback((action: () => void) => {
    if (runningRef.current) return
    runningRef.current = true
    action()
  }, [])

  const release = useCallback(() => {
    runningRef.current = false
  }, [])

  return { run, release }
}
