import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useRef, useState } from 'react'
import {
  classifyFiling,
  createLicence,
  isReasonCode,
  listJurisdictions,
  listLicences,
  setLicenceEvidenceReference,
  setLicenceReviewDue,
  setLicenceStatus,
  setLicenceValidityDates,
  setTenantLicence,
  type FilingResult,
  type LicenceDetail,
  type LicenceStatus,
  type Licensee,
} from '../../api/licensing'
import { listTenants } from '../../api/tenants'
import { Button } from '../../components/Button'
import { SelectField, TextInput, parseList } from '../../components/FormField'
import { Modal } from '../../components/Modal'
import { useDebouncedValue } from '../../lib/useDebouncedValue'
import { ApprovalRules, Disclaimer, EvidenceNote, FilingOutcome, LicensingErrorAlert } from './LicensingBits'
import { REASON_CODE_HELP } from './labels'

export type FilingKind = 'create_licence' | 'bind_tenant' | 'unbind_tenant' | 'status' | 'validity' | 'review_due' | 'evidence'

export const FILING_KIND_LABEL: Record<FilingKind, string> = {
  create_licence: 'Create a licence',
  bind_tenant: 'Bind a tenant to a licence',
  unbind_tenant: 'Unbind a tenant from its licence',
  status: 'Change licence status',
  validity: 'Change licence validity dates',
  review_due: 'Change licence review-due date',
  evidence: 'Change licence evidence reference',
}

const KINDS = Object.keys(FILING_KIND_LABEL) as FilingKind[]
const LICENSEES: Licensee[] = ['platform', 'tenant']
const STATUSES: LicenceStatus[] = ['active', 'suspended', 'expired']

/** Evidence reference rules the server applies (1..200 characters, no control characters, no "@"); a convenience check only. */
export function evidenceReferenceProblem(v: string): string | null {
  if (v.length < 1 || v.length > 200) return 'Use 1 to 200 characters, or leave blank to clear.'
  // eslint-disable-next-line no-control-regex
  if (/[\u0000-\u001f\u007f]/.test(v)) return 'Control characters are not allowed.'
  if (v.includes('@')) return 'An "@" is not allowed (do not enter an email address).'
  return null
}

function TenantPicker({ value, onChange, fixed }: { value: string; onChange: (id: string) => void; fixed?: string }) {
  const [search, setSearch] = useState('')
  const q = useDebouncedValue(search, 250)
  const tenants = useQuery({ queryKey: ['licensing', 'tenant-picker', q], queryFn: () => listTenants({ q: q || undefined, limit: 50 }), enabled: !fixed, retry: false })
  if (fixed) {
    return (
      <p className="rounded-md border border-border bg-surface-alt px-3 py-2 text-sm">
        Tenant: <span className="font-mono text-xs">{fixed}</span>
      </p>
    )
  }
  const options = [{ value: '', label: 'Choose a tenant' }, ...(tenants.data?.items ?? []).map((t) => ({ value: t.id, label: `${t.name} (${t.slug})` }))]
  return (
    <div className="flex flex-col gap-2">
      <TextInput label="Find tenant" value={search} onChange={(e) => setSearch(e.target.value)} autoComplete="off" />
      <SelectField label="Tenant" value={value} onChange={(e) => onChange(e.target.value)} options={options} />
      {tenants.error ? <p className="text-sm text-red-700">The tenant list could not be loaded.</p> : null}
    </div>
  )
}

function LicencePicker({ value, onChange, label = 'Licence' }: { value: string; onChange: (id: string) => void; label?: string }) {
  const licences = useQuery({ queryKey: ['licensing', 'licences'], queryFn: listLicences, retry: false })
  // No licence is preselected: the operator must make an explicit choice (G-14).
  const options = [{ value: '', label: 'Choose a licence' }, ...(licences.data ?? []).map((l) => ({ value: l.id, label: `${l.licence_number} (${l.licensee}, ${l.status})` }))]
  return (
    <>
      <SelectField label={label} value={value} onChange={(e) => onChange(e.target.value)} options={options} />
      {licences.error ? <p className="text-sm text-red-700">The licence list could not be loaded.</p> : null}
    </>
  )
}

/**
 * One dialog files every licensing change through the EXISTING change-filing endpoints. Nothing is preselected (G-14):
 * the licence, tenant, licensee, jurisdiction and status are explicit choices. The server classifies each change; a
 * widening one answers 202 and is shown as "pending approval", never as success.
 */
export function FileChangeDialog({
  onClose,
  initialKind,
  licence,
  tenantId,
}: {
  onClose: () => void
  initialKind?: FilingKind
  /** A licence the dialog was opened from (the target is then fixed, not a default). */
  licence?: LicenceDetail
  /** A tenant the dialog was opened from. */
  tenantId?: string
}) {
  const qc = useQueryClient()
  const [kind, setKind] = useState<FilingKind | ''>(initialKind ?? '')
  const [licenceId, setLicenceId] = useState(licence?.id ?? '')
  const [tenant, setTenant] = useState(tenantId ?? '')
  const [reason, setReason] = useState('')
  const [jurisdictionId, setJurisdictionId] = useState('')
  const [licensee, setLicensee] = useState<Licensee | ''>('')
  const [number, setNumber] = useState('')
  const [products, setProducts] = useState('')
  const [markets, setMarkets] = useState('')
  const [status, setStatus] = useState<LicenceStatus | ''>('')
  const [issued, setIssued] = useState(licence?.issued_at ?? '')
  const [expires, setExpires] = useState(licence?.expires_at ?? '')
  const [reviewDue, setReviewDue] = useState(licence?.review_due_at ?? '')
  const [evidence, setEvidence] = useState(licence?.licence_evidence_reference ?? '')
  const [result, setResult] = useState<FilingResult | null>(null)
  const sending = useRef(false)

  // Picking a licence loads its CURRENT lifecycle values into the lifecycle fields, so a blank field is always a
  // deliberate "clear" and never an accident of an empty form.
  function pickLicence(id: string) {
    setLicenceId(id)
    const l = qc.getQueryData<LicenceDetail[]>(['licensing', 'licences'])?.find((x) => x.id === id)
    setIssued(l?.issued_at ?? '')
    setExpires(l?.expires_at ?? '')
    setReviewDue(l?.review_due_at ?? '')
    setEvidence(l?.licence_evidence_reference ?? '')
  }

  const jurisdictions = useQuery({ queryKey: ['licensing', 'jurisdictions'], queryFn: listJurisdictions, enabled: kind === 'create_licence', retry: false })

  const validReason = isReasonCode(reason)
  const evidenceProblem = kind === 'evidence' && evidence !== '' ? evidenceReferenceProblem(evidence) : null
  const ready =
    validReason &&
    ((kind === 'create_licence' && jurisdictionId !== '' && licensee !== '' && number.trim() !== '') ||
      (kind === 'bind_tenant' && tenant !== '' && licenceId !== '') ||
      (kind === 'unbind_tenant' && tenant !== '') ||
      (kind === 'status' && licenceId !== '' && status !== '') ||
      (kind === 'validity' && licenceId !== '') ||
      (kind === 'review_due' && licenceId !== '') ||
      (kind === 'evidence' && licenceId !== '' && evidenceProblem === null))

  const m = useMutation({
    mutationFn: async (): Promise<unknown> => {
      const nil = (s: string) => (s === '' ? null : s)
      switch (kind) {
        case 'create_licence':
          return createLicence({ jurisdiction_id: jurisdictionId, licensee: licensee as Licensee, licence_number: number.trim(), permitted_products: parseList(products), permitted_markets: parseList(markets), reason_code: reason })
        case 'bind_tenant':
          return setTenantLicence(tenant, licenceId, reason)
        case 'unbind_tenant':
          return setTenantLicence(tenant, null, reason)
        case 'status':
          return setLicenceStatus(licenceId, status as LicenceStatus, reason)
        case 'validity':
          return setLicenceValidityDates(licenceId, nil(issued), nil(expires), reason)
        case 'review_due':
          return setLicenceReviewDue(licenceId, nil(reviewDue), reason)
        case 'evidence':
          return setLicenceEvidenceReference(licenceId, nil(evidence), reason)
        default:
          throw new Error('no kind')
      }
    },
    onSuccess: (body) => {
      setResult(classifyFiling(body))
      void qc.invalidateQueries({ queryKey: ['licensing'] })
    },
    onSettled: () => {
      sending.current = false
    },
  })

  function submit() {
    if (!ready || sending.current || result) return
    sending.current = true
    m.mutate()
  }

  return (
    <Modal title="File a licensing change" onClose={onClose} size="lg">
      <form
        className="flex flex-col gap-3"
        onSubmit={(e) => {
          e.preventDefault()
          submit()
        }}
      >
        <Disclaimer />
        <SelectField
          label="Kind of change"
          value={kind}
          disabled={!!initialKind || !!result}
          onChange={(e) => setKind(e.target.value as FilingKind | '')}
          options={[{ value: '', label: 'Choose a kind of change' }, ...KINDS.map((k) => ({ value: k, label: FILING_KIND_LABEL[k] }))]}
        />

        {kind === 'create_licence' && (
          <>
            <SelectField
              label="Jurisdiction"
              value={jurisdictionId}
              onChange={(e) => setJurisdictionId(e.target.value)}
              options={[{ value: '', label: 'Choose a jurisdiction' }, ...(jurisdictions.data ?? []).map((j) => ({ value: j.id, label: `${j.code} - ${j.name}` }))]}
            />
            <SelectField
              label="Licensee"
              value={licensee}
              onChange={(e) => setLicensee(e.target.value as Licensee | '')}
              options={[{ value: '', label: 'Choose the licensee' }, ...LICENSEES.map((l) => ({ value: l, label: l === 'platform' ? 'Platform is the licensee' : 'Tenant is the licensee' }))]}
              help="Must match the licensing model of any tenant it is bound to. Chosen explicitly; there is no default."
            />
            <TextInput label="Licence number" value={number} onChange={(e) => setNumber(e.target.value)} autoComplete="off" help="Exactly as supplied by the operator. Never invent one." />
            <TextInput label="Permitted products (optional)" value={products} onChange={(e) => setProducts(e.target.value)} help="Comma separated, as supplied by the operator." />
            <TextInput label="Permitted markets (optional)" value={markets} onChange={(e) => setMarkets(e.target.value)} help="Comma separated, as supplied by the operator." />
            <p className="text-xs text-slate-600">A new licence is always created as active (recorded) once two different people approve; creating it grants no market permission by itself.</p>
          </>
        )}

        {(kind === 'bind_tenant' || kind === 'unbind_tenant') && (
          <>
            <TenantPicker value={tenant} onChange={setTenant} fixed={tenantId} />
            {kind === 'bind_tenant' && <LicencePicker value={licenceId} onChange={pickLicence} />}
            {kind === 'unbind_tenant' && <p className="text-xs text-slate-600">An unbind is a narrowing change: it executes at once with no second approval.</p>}
          </>
        )}

        {(kind === 'status' || kind === 'validity' || kind === 'review_due' || kind === 'evidence') && (
          <>
            {licence ? (
              <p className="rounded-md border border-border bg-surface-alt px-3 py-2 text-sm">
                Licence: <span className="font-medium">{licence.licence_number}</span> <span className="font-mono text-xs">{licence.id}</span>
              </p>
            ) : (
              <LicencePicker value={licenceId} onChange={pickLicence} />
            )}
            {kind === 'status' && (
              <SelectField
                label="New status"
                value={status}
                onChange={(e) => setStatus(e.target.value as LicenceStatus | '')}
                options={[{ value: '', label: 'Choose a status' }, ...STATUSES.map((s) => ({ value: s, label: s }))]}
                help="A regulator revocation is recorded as expired with a reason code."
              />
            )}
            {kind === 'validity' && (
              <>
                <TextInput label="Issued on" type="date" value={issued} onChange={(e) => setIssued(e.target.value)} help="Blank clears the date." />
                <TextInput label="Expires on" type="date" value={expires} onChange={(e) => setExpires(e.target.value)} help="Blank clears the date. Clearing or postponing is a widening change." />
              </>
            )}
            {kind === 'review_due' && <TextInput label="Review due on" type="date" value={reviewDue} onChange={(e) => setReviewDue(e.target.value)} help="Blank clears the date. Clearing or postponing is a widening change." />}
            {kind === 'evidence' && (
              <>
                <TextInput
                  label="Evidence reference"
                  value={evidence}
                  onChange={(e) => setEvidence(e.target.value)}
                  autoComplete="off"
                  maxLength={200}
                  help="1 to 200 characters, no email address. Blank clears the reference."
                />
                {evidenceProblem && <p role="alert" className="text-sm text-red-700">{evidenceProblem}</p>}
                <EvidenceNote />
              </>
            )}
          </>
        )}

        {kind !== '' && (
          <>
            <TextInput label="Reason code" value={reason} onChange={(e) => setReason(e.target.value.trim())} autoComplete="off" maxLength={64} help={REASON_CODE_HELP} aria-invalid={reason !== '' && !validReason} />
            {reason !== '' && !validReason && <p role="alert" className="text-sm text-red-700">That is not a valid reason code.</p>}
            <p className="text-xs text-slate-600">The server decides whether this change widens or narrows. Filing needs a fresh second-factor check.</p>
            <ApprovalRules />
          </>
        )}

        {m.error && !result && <LicensingErrorAlert error={m.error} />}
        {result && <FilingOutcome result={result} />}

        <div className="flex justify-end gap-2">
          <Button type="button" variant="secondary" onClick={onClose}>
            {result ? 'Close' : 'Cancel'}
          </Button>
          {!result && (
            <Button type="submit" disabled={!ready} isLoading={m.isPending}>
              File change
            </Button>
          )}
        </div>
      </form>
    </Modal>
  )
}
