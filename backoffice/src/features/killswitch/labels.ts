// Fixed UI-owned vocabulary for the payment kill switch console (ADR 0114 S6). No server MESSAGE is rendered and
// every enum falls back to 'Unrecognised' plus a safe code. The one deliberate exception is `reason_code`: it is a
// staff-entered string (up to 64 bytes) that the console shows verbatim, as escaped React text in a monospace
// element (never as HTML, never interpreted). It is not passed through safeCode(), because a legitimate reason
// may contain characters outside the token set; the audit log holds the same value. No legal or regulatory claim
// is made anywhere.
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
  'Up to 64 bytes (a plain ASCII letter, digit or underscore is one byte; accented and other non-ASCII characters take two or more), for example "provider_incident". It is recorded in the audit log. Do not enter names or personal data.'

/**
 * What engaging an ALREADY ENGAGED provider/operation pair really does (migrations 0105/0106, EngageKillSwitch): it
 * is not an error and not a no-op. The reason is overwritten and the version is bumped. An open release request is
 * NOT cancelled by a same-scope re-engage: it stays open but frozen at the old version, so the server refuses to
 * approve it; cancel it and file a new one.
 */
export const NOTICE_REENGAGE =
  'If this provider and operation are already engaged, engaging again does not fail: it overwrites the stored reason code and raises the switch version. ' +
  'An open release request is not cancelled by that, but it stays tied to the old version, so the server will refuse to approve it. Cancel it and file a new one.'

export const NOTICE_TAKEOVER_PLATFORM =
  'As platform staff, engaging a switch that tenant staff engaged takes it over: it becomes platform-engaged, any open tenant release request is cancelled, and tenant staff can no longer change or release it.'

export const NOTICE_TAKEOVER_TENANT =
  'If platform staff engage a switch that you engaged, it becomes platform-engaged and any open release request is cancelled. After that only platform staff can change or release it, and the server refuses a tenant engage or release request for it.'

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
