import type { LaunchTransition, RequestableAction } from '../../api/launch'
import type { NavPermissions } from '../../auth/permissions'

export interface ActionContext {
  status: string
  subjectKind: 'tenant' | 'brand'
  /** True when the latest recorded transition of this subject is the legacy baseline. */
  legacyBaseline: boolean
  perms: NavPermissions
  /** True for a platform-scoped caller (a tenant-scoped caller may act on brands of its own tenant only). */
  platformScope: boolean
  /** A non-suspend request is already pending for this subject (the server allows one). */
  hasPending: boolean
}

export interface AvailableActions {
  request: RequestableAction[]
  suspend: boolean
}

/**
 * Which buttons to OFFER. A convenience mirror of ADR 0112 section 3.1 and 6.1 - the server is the
 * authority and re-checks the state machine, the permission and the tenant scope on every call;
 * if it refuses, the refusal is shown (see errors.ts).
 */
export function availableActions(c: ActionContext): AvailableActions {
  const none: AvailableActions = { request: [], suspend: false }
  // A tenant subject is platform-only on both sides.
  if (c.subjectKind === 'tenant' && !c.platformScope) return none
  const request: RequestableAction[] = []
  if (c.perms.launchRequest && !c.hasPending) {
    switch (c.status) {
      case 'pending_launch':
        request.push('activate', 'close')
        break
      case 'active':
        request.push('close')
        if (c.legacyBaseline) request.push('ratify')
        break
      case 'suspended':
        request.push('reactivate', 'close')
        if (c.legacyBaseline) request.push('ratify')
        break
      default:
        break // closed is terminal; an unrecognised status offers nothing
    }
  }
  return { request, suspend: c.perms.launchSuspend && c.status === 'active' }
}

/** The history is newest first; the subject's own rows only (the tenant list has no brand filter). */
export function subjectTransitions(items: LaunchTransition[], subjectKind: 'tenant' | 'brand'): LaunchTransition[] {
  return items
    .filter((t) => t.subject_kind === subjectKind)
    .slice()
    .sort((a, b) => b.created_at.localeCompare(a.created_at))
}

export function isLegacyBaseline(items: LaunchTransition[]): boolean {
  return items.length > 0 && items[0].kind === 'legacy_baseline'
}
