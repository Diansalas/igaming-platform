// Display vocabularies for ADR 0112. Closed lists copied from the OpenAPI spec; reason-code catalogues
// are the server's RECOMMENDATION catalogues (LaunchCreateRequest/LaunchDecideRequest descriptions) -
// the server stays the authority and answers 400 for a code outside its closed set.
import type { BadgeTone } from '../../components/Badge'
import type {
  LaunchAction,
  LicensingModel,
  LicensingStatus,
  RequestableAction,
  RequestStatus,
  SubjectStatus,
} from '../../api/launch'

/** Shown on every screen that renders licensing, readiness or decision data (ADR 0112 section 9). */
export const LAUNCH_DISCLAIMER =
  'A recorded decision is not a legal authorisation. The operator and legal/compliance decide whether a brand may operate.'

export const LEGACY_BASELINE_LABEL = 'Legacy baseline - no launch decision recorded through ADR 0112'

export const CONDITIONS_NOTE_WARNING =
  'Do not enter names, ID numbers, card or account numbers. This field is not a PII store. Nothing in it is machine-enforced, and it is visible to the tenant and to platform staff.'

/** Fixed closure notice (ADR 0112 5.1 / residual 14(c)): terminal, funds stay until HD-CTF-4 is decided. */
export const FUNDS_STRANDED_NOTICE =
  'Closure is terminal. Any player balances stay stranded until HD-CTF-4 (the unclaimed-funds policy) is decided; no process exists today to return them.'

/** Fixed suspension notice (ADR 0112 12.14(b4)): inaccessible, not stranded. */
export const FUNDS_INACCESSIBLE_NOTICE =
  'While suspended, player funds are inaccessible, not stranded. They stay in the ledger and become available again if the brand is reactivated (always a four-eyes decision).'

export const FUNDS_OMISSIONS_NOTICE =
  'The funds figures do not include in-flight deposits or open casino rounds.'

const SUBJECT_STATUS_LABELS: Record<SubjectStatus, string> = {
  pending_launch: 'Pending launch',
  active: 'Active',
  suspended: 'Suspended',
  closed: 'Closed',
}

export function subjectStatusLabel(status: string): string {
  return SUBJECT_STATUS_LABELS[status as SubjectStatus] ?? `Unrecognised status (${status})`
}

export function subjectStatusTone(status: string): BadgeTone {
  switch (status) {
    case 'active':
      return 'success'
    case 'pending_launch':
      return 'info'
    case 'suspended':
      return 'warning'
    case 'closed':
      return 'neutral'
    default:
      return 'danger'
  }
}

export const SUBJECT_STATUS_FILTER_OPTIONS = [
  { value: 'pending_launch', label: 'Pending launch' },
  { value: 'active', label: 'Active' },
  { value: 'suspended', label: 'Suspended' },
  { value: 'closed', label: 'Closed' },
]

const REQUEST_STATUS_LABELS: Record<RequestStatus, string> = {
  pending: 'Pending approval',
  executing: 'Executing',
  executed: 'Executed',
  refused_at_execution: 'Refused at execution',
  rejected: 'Rejected',
  cancelled: 'Cancelled',
  expired: 'Expired',
  superseded: 'Superseded by a suspension',
}

export function requestStatusLabel(status: string): string {
  return REQUEST_STATUS_LABELS[status as RequestStatus] ?? `Unrecognised (${status})`
}

export function requestStatusTone(status: string): BadgeTone {
  switch (status) {
    case 'pending':
    case 'executing':
      return 'info'
    case 'executed':
      return 'success'
    case 'refused_at_execution':
    case 'rejected':
      return 'danger'
    case 'cancelled':
    case 'expired':
    case 'superseded':
      return 'warning'
    default:
      return 'neutral'
  }
}

export const REQUEST_STATUS_FILTER_OPTIONS = [
  { value: 'pending', label: 'Pending approval' },
  { value: 'executed', label: 'Executed' },
  { value: 'rejected', label: 'Rejected' },
  { value: 'cancelled', label: 'Cancelled' },
  { value: 'expired', label: 'Expired' },
  { value: 'superseded', label: 'Superseded' },
  { value: 'refused_at_execution', label: 'Refused at execution' },
]

const ACTION_LABELS: Record<LaunchAction, string> = {
  activate: 'Activate',
  reactivate: 'Reactivate',
  suspend: 'Suspend',
  close: 'Close',
  ratify: 'Ratify legacy baseline',
}

export function actionLabel(action: string): string {
  return ACTION_LABELS[action as LaunchAction] ?? action
}

export const LICENSING_MODEL_OPTIONS: { value: LicensingModel; label: string }[] = [
  { value: 'platform_licence', label: 'Platform licence' },
  { value: 'tenant_licence', label: "Tenant's own licence" },
  { value: 'other_manually_approved', label: 'Other (manually approved)' },
  { value: 'not_applicable_recorded_determination', label: 'Not applicable (recorded determination)' },
]

export const LICENSING_STATUS_OPTIONS: { value: LicensingStatus; label: string }[] = [
  { value: 'in_force', label: 'In force' },
  { value: 'applied_pending', label: 'Applied, pending' },
  { value: 'conditional', label: 'Conditional' },
  { value: 'suspended', label: 'Suspended' },
  { value: 'expired', label: 'Expired' },
  { value: 'not_required_per_determination', label: 'Not required per determination' },
]

export function licensingModelLabel(v: string | undefined): string {
  return LICENSING_MODEL_OPTIONS.find((o) => o.value === v)?.label ?? (v ? `Unrecognised (${v})` : '-')
}
export function licensingStatusLabel(v: string | undefined): string {
  return LICENSING_STATUS_OPTIONS.find((o) => o.value === v)?.label ?? (v ? `Unrecognised (${v})` : '-')
}

/** Models whose basis is a recorded external determination (reference + responsible function). */
export function needsDetermination(model: string): boolean {
  return model === 'other_manually_approved' || model === 'not_applicable_recorded_determination'
}
/** Models that point at a licence registry row (optional for other_manually_approved; never for not_applicable). */
export function allowsLicenceId(model: string): boolean {
  return model !== 'not_applicable_recorded_determination' && model !== ''
}

type Opt = { value: string; label: string }
const o = (value: string): Opt => ({ value, label: value.replace(/_/g, ' ') })

/** Closed reason catalogues per action (OpenAPI LaunchCreateRequest.reason_code). other_documented needs a note. */
export const REQUEST_REASON_CODES: Record<RequestableAction | 'suspend', Opt[]> = {
  activate: [o('initial_launch')],
  reactivate: [o('issue_resolved'), o('relaunch_after_review')],
  close: [o('offboarding'), o('licence_issue'), o('operator_request'), o('compliance_decision')],
  ratify: [o('legacy_ratification')],
  suspend: [o('compliance_hold'), o('security_incident'), o('operational_incident'), o('licence_issue'), o('operator_request'), o('other_documented')],
}
export const APPROVE_REASON_CODES: Opt[] = [o('reviewed_ok')]
export const REJECT_REASON_CODES: Opt[] = [o('insufficient_information'), o('readiness_concerns'), o('policy_concern'), o('other_documented')]
export const CANCEL_REASON_CODES: Opt[] = [o('requester_withdrew'), o('superseded_by_new_request'), o('other_documented')]

export function reasonLabel(code: string | undefined): string {
  return code ? code.replace(/_/g, ' ') : '-'
}

/** Human text for the derived readiness checks (ADR 0112 5.1). */
export const CHECK_TITLES: Record<string, string> = {
  'R-JUR-CONFIG': 'Jurisdiction configuration in effect',
  'R-LIC-VALID': 'Licence valid (licence-backed model)',
  'R-RULESETS': 'KYC, AML and RG rule sets set',
  'R-CURRENCIES': 'Allowed currencies are active assets',
  'R-PAY-METHODS': 'Payment methods and approved provider credential',
  'R-CATALOGUE': 'Casino catalogue available',
  'R-MARKET-CONFIG': 'Operating market configured',
  'R-PROVIDER-TIER': 'Process components production-eligible',
  'R-ALERTS': 'P1 alert routing has a recipient',
  'R-PLAYER-FUNDS': 'Player funds, open bets and held withdrawals',
  'R-RG-CONFIG': 'RG limit enforcement on, with compliance floors for every currency',
  // ADR 0116 A7. Transaction monitoring is NOT IMPLEMENTED: its item always fails, on purpose.
  'R-AML-QUORUM': 'AML officer quorum (two distinct officers)',
  'R-AML-SCREENING': 'AML screening provider and policy configured',
  'R-AML-MONITORING': 'AML transaction monitoring (not implemented)',
}

export function checkTitle(code: string): string {
  return CHECK_TITLES[code] ?? code
}

/**
 * Splits a decimal-string minor-units value into a thousands-grouped string using ONLY string
 * operations. Never Number()/parseFloat: a 1.8e19 balance must survive exactly. Anything that is
 * not an optionally-signed digit run is returned unchanged (the API schema guarantees ^-?[0-9]+$).
 */
export function groupDecimalString(value: string): string {
  const m = /^(-?)([0-9]+)$/.exec(value)
  if (!m) return value
  const digits = m[2].replace(/^0+(?=[0-9])/, '')
  return m[1] + digits.replace(/\B(?=([0-9]{3})+(?![0-9]))/g, ',')
}
