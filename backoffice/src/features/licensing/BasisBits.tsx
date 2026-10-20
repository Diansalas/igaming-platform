import { Link } from 'react-router-dom'
import type { BasisFilingResult } from '../../api/operatingBases'
import { Badge } from '../../components/Badge'
import { BASIS_DISCLAIMER, NO_LICENCE_NOTICE, REVIEW_LABEL, basisKindLabel, basisStatus } from './basisLabels'
import { UUID_RE, reviewState } from './basisRules'
import { Pending, Success } from './LicensingBits'
import { safeToken } from './labels'

export function BasisDisclaimer() {
  return (
    <p role="note" aria-label="Operating basis disclaimer" className="rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-sm text-amber-900">
      {BASIS_DISCLAIMER}
    </p>
  )
}

/** Always rendered for a no_licence_documented basis; there is no way to dismiss it. */
export function NoLicenceNotice() {
  return (
    <div role="note" aria-label="No-licence basis notice" className="rounded-md border-2 border-amber-500 bg-amber-50 px-3 py-2 text-sm font-medium text-amber-950">
      {NO_LICENCE_NOTICE}
    </div>
  )
}

export function BasisKindText({ kind }: { kind: string }) {
  return <>{basisKindLabel(kind)}</>
}

export function BasisStatusBadge({ status }: { status: string }) {
  const s = basisStatus(status)
  return <Badge tone={s.tone}>{s.label}</Badge>
}

export function ReviewIndicator({ reviewDueAt, needsRereview, today }: { reviewDueAt: string | null; needsRereview: boolean; today?: string }) {
  const r = reviewState(reviewDueAt, needsRereview, today)
  const l = REVIEW_LABEL[r.state]
  return (
    <span className="inline-flex flex-wrap items-center gap-2">
      <span>{reviewDueAt ?? 'Not recorded'}</span>
      <Badge tone={l.tone}>{l.label}</Badge>
    </span>
  )
}

/** A 201 with executed:false is "pending approval", never success; only executed:true on an executed request is "applied". */
export function BasisFilingOutcome({ result }: { result: BasisFilingResult }) {
  if (result.kind === 'pending') {
    const c = result.response.change_request
    return (
      <Pending>
        Pending approval. Nothing has changed yet. This change needs {c.required_approvals} approvals from different people before it takes effect.{' '}
        <Link className="font-medium underline" to={`/licensing/changes/${encodeURIComponent(c.id)}`}>
          Open request {safeToken(c.id).slice(0, 8)}
        </Link>
      </Pending>
    )
  }
  if (result.kind === 'applied') {
    const c = result.response.change_request
    return (
      <Success>
        Applied at once (a narrowing change needs no second approval).{' '}
        <Link className="font-medium underline" to={`/licensing/changes/${encodeURIComponent(c.id)}`}>
          Open record
        </Link>
      </Success>
    )
  }
  return <Pending>The server answered, but the answer could not be recognised. Nothing is confirmed. Check the licensing changes list before trying again.</Pending>
}

const SUMMARY_FIELDS: [string, string][] = [
  ['tenant_id', 'Tenant'],
  ['brand_id', 'Brand'],
  ['country_code', 'Country'],
  ['operation_code', 'Operation'],
  ['product_code', 'Product'],
  ['basis_kind', 'Basis'],
  ['licence_id', 'Licence'],
  ['jurisdiction_id', 'Jurisdiction relied on'],
  ['determination_reference', 'Determination reference (as entered)'],
  ['determined_by_role', 'Determined by (function)'],
  ['basis_evidence_reference', 'Evidence reference (supplied by the operator, not verified)'],
  ['restrictions_note', 'Restrictions note'],
  ['restricted_product_codes', 'Products not covered'],
  ['review_due_at', 'Review due'],
  ['needs_rereview', 'Flag for re-review'],
  ['review_reference', 'Review reference (as entered)'],
]
const NULL_TEXT: Record<string, string> = { brand_id: 'Whole tenant', product_code: 'All products' }

function valueText(key: string, v: unknown): string {
  if (v === null || v === undefined) return NULL_TEXT[key] ?? 'None'
  if (typeof v === 'boolean') return v ? 'Yes' : 'No'
  if (typeof v === 'string') return key === 'basis_kind' ? basisKindLabel(v) : v
  if (Array.isArray(v) && v.every((x) => typeof x === 'string')) return v.length ? (v as string[]).join(', ') : 'None'
  return 'Not printable'
}

export const BASIS_CHANGE_KINDS = ['operating_basis_set', 'operating_basis_withdraw', 'operating_basis_review_record']

/** A readable view of an operating-basis change payload, so approvers decide on words and not on raw JSON. Text only (React escapes it). */
export function BasisPayloadSummary({ kind, payload }: { kind: string; payload: Record<string, unknown> }) {
  if (!BASIS_CHANGE_KINDS.includes(kind)) return null
  const rows = SUMMARY_FIELDS.filter(([k]) => Object.prototype.hasOwnProperty.call(payload, k))
  const tenant = typeof payload.tenant_id === 'string' && UUID_RE.test(payload.tenant_id) ? payload.tenant_id : null
  return (
    <section aria-label="Operating basis summary" className="flex flex-col gap-2">
      <BasisDisclaimer />
      {kind === 'operating_basis_set' && payload.basis_kind === 'no_licence_documented' && <NoLicenceNotice />}
      {kind === 'operating_basis_withdraw' && <p className="text-sm text-slate-700">Withdrawing leaves this scope relying on no basis: it stops permitting and does not fall back to the tenant's licence.</p>}
      <dl className="grid grid-cols-2 gap-3 text-sm sm:grid-cols-3">
        {rows.map(([k, label]) => (
          <div key={k}>
            <dt className="text-slate-500">{label}</dt>
            <dd className="mt-1 break-words">{valueText(k, payload[k])}</dd>
          </div>
        ))}
      </dl>
      {tenant && (
        <Link className="text-sm text-brand-700 hover:underline" to={`/licensing/operating-bases/tenants/${encodeURIComponent(tenant)}`}>
          See this tenant's operating bases
        </Link>
      )}
    </section>
  )
}
