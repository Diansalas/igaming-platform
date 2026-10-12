// Fixed UI-owned vocabulary for the payment kill switch console (ADR 0114 S6). No server text is rendered:
// every enum falls back to 'Unrecognised' plus a safe code. No legal or regulatory claim is made anywhere.
import type { BadgeTone } from '../../components/Badge'

/**
 * HARDCODED on purpose: no API field says whether the payment stack behind a switch is a mock. Every payment
 * provider adapter in this repository is a MOCK today, so the wording says so. When a non-mock payment provider
 * exists this MUST be derived from a server field (not authorised yet; pinned by a test).
 */
export const MOCK_BANNER_TITLE = 'MOCK payment stack'
export const MOCK_BANNER_BODY =
  'The payment providers behind these switches are MOCK adapters: no real payment provider is connected. ' +
  'A switch here still blocks dispatch to the mock provider, so use it to rehearse the control, not as evidence of a live one.'

export const CONSEQUENCE_ENGAGE = [
  'Engaging stops the platform sending new outbound requests to the provider in scope (for the operations in scope).',
  'It does NOT release or move any funds.',
  'It does NOT cancel any withdrawal: withdrawals already requested or approved stay as they are.',
  'Releasing it later needs a request and a second person to approve.',
]

export const CONSEQUENCE_RELEASE_REQUEST =
  'Filing a release request does not release the switch. It stays engaged until a different person approves the request. Approving also does not move funds or restart anything by itself.'

export const REASON_HELP =
  'Up to 64 characters, for example "provider_incident". It is recorded in the audit log. Do not enter names or personal data.'

export const PLATFORM_STAFF = 'Platform staff'

const SAFE_TOKEN = /^[A-Za-z0-9_.*-]{1,64}$/

export function safeCode(v: unknown): string {
  return typeof v === 'string' && SAFE_TOKEN.test(v) ? v : 'unprintable'
}

export interface EnumView {
  label: string
  code?: string
  known: boolean
}

function enumView(map: Record<string, string>, v: unknown): EnumView {
  if (typeof v === 'string' && Object.prototype.hasOwnProperty.call(map, v)) return { label: map[v], known: true }
  return { label: 'Unrecognised', code: safeCode(v), known: false }
}

export const describeOperation = (v: unknown) => enumView({ deposit: 'Deposits', payout: 'Payouts', '*': 'All operations' }, v)
export const describeScope = (v: unknown) => enumView({ tenant: 'Tenant staff', platform: 'Platform staff' }, v)
export const describeRequestStatus = (v: unknown) =>
  enumView({ open: 'Open', approved: 'Approved', cancelled: 'Cancelled', expired: 'Expired' }, v)

export function requestStatusTone(v: unknown): BadgeTone {
  return v === 'open' ? 'warning' : v === 'approved' ? 'success' : v === 'cancelled' || v === 'expired' ? 'neutral' : 'danger'
}

/** The provider is an open string on the wire ('*' or a provider id); only a safe token is ever printed. */
export function describeProvider(v: unknown): { label: string; code?: string } {
  if (v === '*') return { label: 'All providers' }
  return { label: 'Provider', code: safeCode(v) }
}

/**
 * Actor rendering (ADR 0114 section 9): null with scope platform is 'Platform staff' (the server hides the id
 * from tenant viewers). A platform viewer gets the id, labelled by scope. Null with any other scope is never
 * guessed at: 'Not shown'.
 */
export function describeActor(id: string | null | undefined, scope: string | null | undefined): string {
  if (id === null || id === undefined || id === '') {
    if (scope === 'platform') return PLATFORM_STAFF
    return 'Not shown'
  }
  const code = safeCode(id)
  if (scope === 'platform') return `${PLATFORM_STAFF} (${code})`
  if (scope === 'tenant') return `Tenant staff (${code})`
  return `Unrecognised scope (${code})`
}
