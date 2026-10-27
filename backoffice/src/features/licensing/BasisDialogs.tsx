import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useRef, useState, type ReactNode } from 'react'
import { isReasonCode, listJurisdictions, listLicences } from '../../api/licensing'
import {
  classifyBasisFiling,
  fileBasisChange,
  type BasisFilingResult,
  type BasisKind,
  type BasisScopePayload,
  type FileBasisChangeBody,
  type OperatingBasis,
} from '../../api/operatingBases'
import { listBrands } from '../../api/tenants'
import { Button } from '../../components/Button'
import { SelectField, TextInput } from '../../components/FormField'
import { Modal } from '../../components/Modal'
import { BASIS_APPROVAL_NOTE, BASIS_EVIDENCE_NOTE, BASIS_KIND_LABEL } from './basisLabels'
import {
  UUID_RE,
  codeProblem,
  countryProblem,
  dateProblem,
  optRefProblem,
  productListProblem,
  refProblem,
  reviewClass,
  roleProblem,
  splitList,
  MAX_NOTE,
} from './basisRules'
import { BasisDisclaimer, BasisFilingOutcome, NoLicenceNotice } from './BasisBits'
import { ApprovalRules, LicensingErrorAlert } from './LicensingBits'
import { REASON_CODE_HELP } from './labels'

/** One filing mutation shared by the three dialogs: single-flight, result classified from the BODY, step-up handled by apiFetch. */
function useBasisFiling() {
  const qc = useQueryClient()
  const sending = useRef(false)
  const [result, setResult] = useState<BasisFilingResult | null>(null)
  const m = useMutation({
    mutationFn: (body: FileBasisChangeBody) => fileBasisChange(body),
    onSuccess: (body) => {
      setResult(classifyBasisFiling(body))
      void qc.invalidateQueries({ queryKey: ['licensing'] })
    },
    onSettled: () => {
      sending.current = false
    },
  })
  function submit(ready: boolean, body: () => FileBasisChangeBody) {
    if (!ready || sending.current || result) return
    sending.current = true
    m.mutate(body())
  }
  return { m, result, submit }
}

function Shell({
  title,
  onClose,
  onSubmit,
  ready,
  result,
  pending,
  error,
  submitLabel,
  children,
}: {
  title: string
  onClose: () => void
  onSubmit: () => void
  ready: boolean
  result: BasisFilingResult | null
  pending: boolean
  error: unknown
  submitLabel: string
  children: ReactNode
}) {
  return (
    <Modal title={title} onClose={onClose} size="lg">
      <form
        className="flex flex-col gap-3"
        onSubmit={(e) => {
          e.preventDefault()
          onSubmit()
        }}
      >
        <BasisDisclaimer />
        {children}
        <p className="text-xs text-slate-600">Filing needs a fresh second-factor check. The server decides whether this change widens or narrows and who must approve it.</p>
        {error && !result ? <LicensingErrorAlert error={error} /> : null}
        {result && <BasisFilingOutcome result={result} />}
        <div className="flex justify-end gap-2">
          <Button type="button" variant="secondary" onClick={onClose}>
            {result ? 'Close' : 'Cancel'}
          </Button>
          {!result && (
            <Button type="submit" disabled={!ready} isLoading={pending}>
              {submitLabel}
            </Button>
          )}
        </div>
      </form>
    </Modal>
  )
}

function ReasonField({ value, onChange }: { value: string; onChange: (v: string) => void }) {
  const ok = isReasonCode(value)
  return (
    <>
      <TextInput label="Reason code" value={value} onChange={(e) => onChange(e.target.value.trim())} autoComplete="off" maxLength={64} help={REASON_CODE_HELP} aria-invalid={value !== '' && !ok} />
      {value !== '' && !ok && <p role="alert" className="text-sm text-red-700">That is not a valid reason code.</p>}
    </>
  )
}

function Problem({ text }: { text: string | null }) {
  return text ? <p role="alert" className="text-sm text-red-700">{text}</p> : null
}

const KINDS: BasisKind[] = ['platform_licence', 'partner_licence', 'no_licence_documented']

/**
 * File an operating basis (kind operating_basis_set). EVERY field is an explicit choice: nothing is preselected, the basis kind
 * included. Client checks mirror the server rules; the server stays authoritative. The tenant is the target the operator opened
 * the dialog from (never typed in, never defaulted).
 */
export function FileBasisDialog({ tenantId, onClose }: { tenantId: string; onClose: () => void }) {
  const { m, result, submit } = useBasisFiling()
  const [brandScope, setBrandScope] = useState<'' | 'tenant' | 'brand'>('')
  const [brandId, setBrandId] = useState('')
  const [country, setCountry] = useState('')
  const [operation, setOperation] = useState('')
  const [productScope, setProductScope] = useState<'' | 'all' | 'one'>('')
  const [product, setProduct] = useState('')
  const [kind, setKind] = useState<BasisKind | ''>('')
  const [licenceId, setLicenceId] = useState('')
  const [jurisdictionId, setJurisdictionId] = useState('')
  const [reference, setReference] = useState('')
  const [role, setRole] = useState('')
  const [evidence, setEvidence] = useState('')
  const [note, setNote] = useState('')
  const [restricted, setRestricted] = useState('')
  const [reviewDue, setReviewDue] = useState('')
  const [reason, setReason] = useState('')

  const brands = useQuery({ queryKey: ['licensing', 'basis-brands', tenantId], queryFn: () => listBrands(tenantId, { limit: 200 }), enabled: brandScope === 'brand', retry: false })
  const licences = useQuery({ queryKey: ['licensing', 'licences'], queryFn: listLicences, enabled: kind === 'platform_licence' || kind === 'partner_licence', retry: false })
  const jurisdictions = useQuery({ queryKey: ['licensing', 'jurisdictions'], queryFn: listJurisdictions, enabled: kind === 'no_licence_documented', retry: false })

  const isNoLicence = kind === 'no_licence_documented'
  const isLicence = kind === 'platform_licence' || kind === 'partner_licence'
  const wantLicensee = kind === 'platform_licence' ? 'platform' : 'tenant'
  const licenceOptions = (licences.data ?? []).filter((l) => l.licensee === wantLicensee)

  const problems = {
    brand: brandScope === '' ? 'Choose whether this applies to the whole tenant or one brand.' : brandScope === 'brand' && !UUID_RE.test(brandId) ? 'Choose a brand.' : null,
    country: countryProblem(country),
    operation: codeProblem(operation),
    product: productScope === '' ? 'Choose all products or one product.' : productScope === 'one' ? codeProblem(product) : null,
    kind: kind === '' ? 'Choose the basis.' : null,
    licence: isLicence && !UUID_RE.test(licenceId) ? 'Choose a licence.' : null,
    jurisdiction: isNoLicence && !UUID_RE.test(jurisdictionId) ? 'Choose the jurisdiction relied on.' : null,
    reference: refProblem(reference),
    role: roleProblem(role),
    evidence: optRefProblem(evidence),
    note: optRefProblem(note, MAX_NOTE),
    restricted: productListProblem(restricted),
    // Mandatory for a documented no-licence basis; optional for the two licence kinds (blank = none).
    review: isNoLicence ? dateProblem(reviewDue) : reviewDue === '' ? null : dateProblem(reviewDue),
  }
  const ready = isReasonCode(reason) && Object.values(problems).every((p) => p === null) && kind !== ''

  function body(): FileBasisChangeBody {
    const nil = (s: string) => (s === '' ? null : s)
    return {
      kind: 'operating_basis_set',
      reason_code: reason,
      payload: {
        tenant_id: tenantId,
        brand_id: brandScope === 'brand' ? brandId : null,
        country_code: country,
        operation_code: operation,
        product_code: productScope === 'one' ? product : null,
        basis_kind: kind as BasisKind,
        licence_id: isLicence ? licenceId : null,
        jurisdiction_id: isNoLicence ? jurisdictionId : null,
        determination_reference: reference,
        determined_by_role: role,
        basis_evidence_reference: nil(evidence),
        restrictions_note: nil(note),
        restricted_product_codes: splitList(restricted),
        review_due_at: nil(reviewDue),
      },
    }
  }

  const touched = (v: string) => v !== ''
  return (
    <Shell title="File an operating basis" onClose={onClose} onSubmit={() => submit(ready, body)} ready={ready} result={result} pending={m.isPending} error={m.error} submitLabel="File basis">
      <p className="rounded-md border border-border bg-surface-alt px-3 py-2 text-sm">
        Tenant: <span className="font-mono text-xs">{tenantId}</span>
      </p>

      <SelectField
        label="Brand scope"
        value={brandScope}
        onChange={(e) => setBrandScope(e.target.value as '' | 'tenant' | 'brand')}
        options={[{ value: '', label: 'Choose the brand scope' }, { value: 'tenant', label: 'The whole tenant (all brands)' }, { value: 'brand', label: 'One brand only' }]}
        help="A brand-specific basis wins over a tenant-wide one for that brand."
      />
      {brandScope === 'brand' && (
        <SelectField
          label="Brand"
          value={brandId}
          onChange={(e) => setBrandId(e.target.value)}
          options={[{ value: '', label: 'Choose a brand' }, ...(brands.data?.items ?? []).map((b) => ({ value: b.id, label: `${b.name} (${b.slug})` }))]}
        />
      )}
      {brands.error ? <p className="text-sm text-red-700">The brand list could not be loaded.</p> : null}
      <TextInput label="Country code" value={country} onChange={(e) => setCountry(e.target.value)} autoComplete="off" maxLength={2} help="Two upper-case letters (ISO 3166-1 alpha-2). This names a scope; it does not say the country is permitted." />
      {touched(country) && <Problem text={problems.country} />}
      <TextInput label="Operation code" value={operation} onChange={(e) => setOperation(e.target.value)} autoComplete="off" maxLength={64} help="An operation from the platform vocabulary, for example wagering, deposit, withdrawal or registration. The server refuses an unknown one." />
      {touched(operation) && <Problem text={problems.operation} />}
      <SelectField
        label="Product scope"
        value={productScope}
        onChange={(e) => setProductScope(e.target.value as '' | 'all' | 'one')}
        options={[{ value: '', label: 'Choose the product scope' }, { value: 'all', label: 'All products' }, { value: 'one', label: 'One product only' }]}
      />
      {productScope === 'one' && <TextInput label="Product code" value={product} onChange={(e) => setProduct(e.target.value)} autoComplete="off" maxLength={64} />}
      {productScope === 'one' && touched(product) && <Problem text={problems.product} />}

      <SelectField
        label="Basis"
        value={kind}
        onChange={(e) => {
          setKind(e.target.value as BasisKind | '')
          setLicenceId('')
          setJurisdictionId('')
        }}
        options={[{ value: '', label: 'Choose the basis' }, ...KINDS.map((k) => ({ value: k, label: BASIS_KIND_LABEL[k] }))]}
        help="Chosen explicitly. There is no default basis and a missing licence never implies one."
      />
      {isNoLicence && <NoLicenceNotice />}
      {isLicence && (
        <>
          <SelectField
            label="Licence"
            value={licenceId}
            onChange={(e) => setLicenceId(e.target.value)}
            options={[{ value: '', label: 'Choose a licence' }, ...licenceOptions.map((l) => ({ value: l.id, label: `${l.licence_number} (${l.status})` }))]}
            help={kind === 'platform_licence' ? 'Only licences whose licensee is the platform are listed.' : 'Only licences whose licensee is a tenant are listed. A partner licence may serve one tenant only.'}
          />
          {licences.error ? <p className="text-sm text-red-700">The licence list could not be loaded.</p> : null}
        </>
      )}
      {isNoLicence && (
        <>
          <SelectField
            label="Jurisdiction relied on"
            value={jurisdictionId}
            onChange={(e) => setJurisdictionId(e.target.value)}
            options={[{ value: '', label: 'Choose a jurisdiction' }, ...(jurisdictions.data ?? []).map((j) => ({ value: j.id, label: `${j.code} - ${j.name}` }))]}
            help="The regime the documented decision relies on. Choosing it does not approve it."
          />
          {jurisdictions.error ? <p className="text-sm text-red-700">The jurisdiction list could not be loaded.</p> : null}
        </>
      )}

      <TextInput label="Determination reference" value={reference} onChange={(e) => setReference(e.target.value)} autoComplete="off" maxLength={200} help="An opaque document id of the recorded determination. No names, contact details or document content." />
      {touched(reference) && <Problem text={problems.reference} />}
      <TextInput label="Determined by (function)" value={role} onChange={(e) => setRole(e.target.value)} autoComplete="off" maxLength={100} help="The responsible function, for example head_of_compliance. Never a person's name." />
      {touched(role) && <Problem text={problems.role} />}
      <TextInput label="Evidence reference (optional)" value={evidence} onChange={(e) => setEvidence(e.target.value)} autoComplete="off" maxLength={200} help="Blank means none." />
      {touched(evidence) && <Problem text={problems.evidence} />}
      <p role="note" aria-label="Basis evidence note" className="text-xs text-slate-600">{BASIS_EVIDENCE_NOTE}</p>
      <TextInput label="Restrictions note (optional)" value={note} onChange={(e) => setNote(e.target.value)} autoComplete="off" maxLength={MAX_NOTE} help="Bounded note, no personal data. Blank means none." />
      {touched(note) && <Problem text={problems.note} />}
      <TextInput label="Products not covered (optional)" value={restricted} onChange={(e) => setRestricted(e.target.value)} autoComplete="off" help="Comma separated product codes this basis does NOT cover. Blank means none." />
      <Problem text={problems.restricted} />
      <TextInput
        label={isNoLicence ? 'Review due on (required)' : 'Review due on (optional)'}
        type="date"
        value={reviewDue}
        onChange={(e) => setReviewDue(e.target.value)}
        help={isNoLicence ? 'Mandatory for a documented decision without a licence. The basis stops permitting the day after this date.' : 'Blank means no review date.'}
      />

      {kind !== '' && (
        <>
          <ReasonField value={reason} onChange={setReason} />
          <p role="note" aria-label="Basis approval note" className="text-xs text-slate-600">{BASIS_APPROVAL_NOTE}</p>
          <ApprovalRules />
        </>
      )}
    </Shell>
  )
}

function scopeOf(b: OperatingBasis): BasisScopePayload {
  return { tenant_id: b.tenant_id, brand_id: b.brand_id, country_code: b.country_code, operation_code: b.operation_code, product_code: b.product_code }
}

function ScopeSummary({ basis }: { basis: OperatingBasis }) {
  return (
    <p className="rounded-md border border-border bg-surface-alt px-3 py-2 text-sm">
      Scope: country <span className="font-mono text-xs">{basis.country_code}</span>, operation <span className="font-mono text-xs">{basis.operation_code}</span>, product{' '}
      <span className="font-mono text-xs">{basis.product_code ?? 'all'}</span>, {basis.brand_id ? <>brand <span className="font-mono text-xs">{basis.brand_id}</span></> : 'whole tenant'}.
    </p>
  )
}

/** Withdraw (kind operating_basis_withdraw): narrowing, single actor, executes in the same call. */
export function WithdrawBasisDialog({ basis, onClose }: { basis: OperatingBasis; onClose: () => void }) {
  const { m, result, submit } = useBasisFiling()
  const [reason, setReason] = useState('')
  const ready = isReasonCode(reason)
  return (
    <Shell
      title="Withdraw operating basis"
      onClose={onClose}
      onSubmit={() => submit(ready, () => ({ kind: 'operating_basis_withdraw', reason_code: reason, payload: scopeOf(basis) }))}
      ready={ready}
      result={result}
      pending={m.isPending}
      error={m.error}
      submitLabel="Withdraw basis"
    >
      <ScopeSummary basis={basis} />
      <p className="rounded-md border border-border bg-surface-alt px-3 py-2 text-sm text-slate-700">
        Withdrawing is a narrowing change by one platform administrator and takes effect at once. This scope then relies on no basis, does not fall back to the tenant's licence or to a tenant-wide basis, and stops permitting. The history is kept.
      </p>
      <ReasonField value={reason} onChange={setReason} />
    </Shell>
  )
}

/** Record a review (operating_basis_review_record): explicit new date, explicit re-review choice, mandatory reference. */
export function ReviewBasisDialog({ basis, onClose }: { basis: OperatingBasis; onClose: () => void }) {
  const { m, result, submit } = useBasisFiling()
  const [flag, setFlag] = useState<'' | 'clear' | 'flag'>('')
  const [due, setDue] = useState('')
  const [reference, setReference] = useState('')
  const [reason, setReason] = useState('')
  const problems = { flag: flag === '' ? 'Choose the outcome.' : null, due: dateProblem(due), reference: refProblem(reference) }
  const ready = isReasonCode(reason) && Object.values(problems).every((p) => p === null)
  const cls = flag !== '' && problems.due === null ? reviewClass(basis.review_due_at, basis.needs_rereview, due, flag === 'flag') : null
  return (
    <Shell
      title="Record an operating basis review"
      onClose={onClose}
      onSubmit={() => submit(ready, () => ({ kind: 'operating_basis_review_record', reason_code: reason, payload: { ...scopeOf(basis), review_due_at: due, needs_rereview: flag === 'flag', review_reference: reference } }))}
      ready={ready}
      result={result}
      pending={m.isPending}
      error={m.error}
      submitLabel="File review"
    >
      <ScopeSummary basis={basis} />
      <p className="text-sm text-slate-700">
        Current review date: <span className="font-mono text-xs">{basis.review_due_at ?? 'not recorded'}</span>
        {basis.needs_rereview ? ', currently flagged for re-review' : ''}. A review does not make the decision lawful; it records that named staff looked at it again.
      </p>
      {basis.basis_kind === 'no_licence_documented' && <NoLicenceNotice />}
      <SelectField
        label="Review outcome"
        value={flag}
        onChange={(e) => setFlag(e.target.value as '' | 'clear' | 'flag')}
        options={[{ value: '', label: 'Choose the outcome' }, { value: 'clear', label: 'Reviewed: no re-review flag' }, { value: 'flag', label: 'Flag for re-review (stops permitting at once)' }]}
      />
      <TextInput label="New review due on" type="date" value={due} onChange={(e) => setDue(e.target.value)} help="Required, chosen explicitly. Nothing is pre-filled." />
      <TextInput label="Review reference" value={reference} onChange={(e) => setReference(e.target.value)} autoComplete="off" maxLength={200} help="An opaque document id of this review. No names or contact details." />
      {reference !== '' && <Problem text={problems.reference} />}
      <p role="note" aria-label="Basis evidence note" className="text-xs text-slate-600">{BASIS_EVIDENCE_NOTE}</p>
      <ReasonField value={reason} onChange={setReason} />
      {cls && (
        <p role="note" aria-label="Review class hint" className="text-sm text-slate-700">
          This looks like a {cls} change{cls === 'widening' ? ': it needs 2 approvals from 2 different people' : ': it takes effect at once'}. The server classifies it and decides.
        </p>
      )}
    </Shell>
  )
}
