import { useMutation, useQuery } from '@tanstack/react-query'
import { useRef, useState } from 'react'
import {
  createLaunchRequest,
  getLaunchReadiness,
  listJurisdictions,
  listLicences,
  type LaunchCreateBody,
  type LaunchResult,
  type LicensingModel,
  type LicensingStatus,
  type ReadinessParams,
  type RequestableAction,
} from '../../api/launch'
import { Button } from '../../components/Button'
import { SelectField, TextInput } from '../../components/FormField'
import { Modal } from '../../components/Modal'
import { LoadingSpinner } from '../../components/LoadingSpinner'
import { useDebouncedValue } from '../../lib/useDebouncedValue'
import { LaunchDisclaimer, LaunchErrorAlert, ReadinessChecklist, WarningBox } from './LaunchBits'
import {
  CONDITIONS_NOTE_WARNING,
  FUNDS_STRANDED_NOTICE,
  LICENSING_MODEL_OPTIONS,
  LICENSING_STATUS_OPTIONS,
  REQUEST_REASON_CODES,
  actionLabel,
  allowsLicenceId,
  needsDetermination,
} from './labels'

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i

function parseIds(text: string): string[] {
  return text
    .split(/[\s,]+/)
    .map((s) => s.trim())
    .filter(Boolean)
}

export interface RequestDialogProps {
  tenantId: string
  /** Absent: the tenant itself is the subject (platform staff only; the server enforces it). */
  brandId?: string
  action: RequestableAction
  /** True for a platform-scoped caller: only they can read the jurisdiction and licence registries. */
  canReadRegistries: boolean
  onClose: () => void
  onDone: (result: LaunchResult) => void
}

/**
 * Request form for activate / reactivate / close / ratify. The server computes and snapshots the
 * readiness it binds; this form only fetches it (read-only) so the user can see it and acknowledge
 * exactly the non-passing set. A request changes no status by itself: it needs distinct platform
 * approvers.
 */
export function RequestDialog({ tenantId, brandId, action, canReadRegistries, onClose, onDone }: RequestDialogProps) {
  const reasons = REQUEST_REASON_CODES[action]
  const recordsLicensing = action === 'activate' || action === 'reactivate' || action === 'ratify'

  const [reason, setReason] = useState('')
  const [model, setModel] = useState<LicensingModel | ''>('')
  const [licStatus, setLicStatus] = useState<LicensingStatus | ''>('')
  const [licenceId, setLicenceId] = useState('')
  const [detRef, setDetRef] = useState('')
  const [detRole, setDetRole] = useState('')
  const [opName, setOpName] = useState('')
  const [opReg, setOpReg] = useState('')
  const [jurisText, setJurisText] = useState('')
  const [note, setNote] = useState('')
  const [acked, setAcked] = useState<Set<string>>(new Set())
  const submitting = useRef(false)

  const jurisdictionIds = parseIds(jurisText)
  const jurisValid = jurisdictionIds.length >= 1 && jurisdictionIds.length <= 32 && jurisdictionIds.every((j) => UUID_RE.test(j))
  const licenceValid = licenceId === '' || UUID_RE.test(licenceId)

  const registries = useQuery({
    queryKey: ['launch-registries'],
    queryFn: async () => ({ jurisdictions: await listJurisdictions(), licences: await listLicences() }),
    enabled: canReadRegistries && recordsLicensing,
    retry: false,
  })

  const readinessParams: ReadinessParams = {
    action,
    brand_id: brandId,
    jurisdiction_ids: recordsLicensing && jurisValid ? jurisdictionIds.join(',') : undefined,
    licensing_model: recordsLicensing && model && licStatus ? model : undefined,
    licensing_status: recordsLicensing && model && licStatus ? licStatus : undefined,
    licence_id: recordsLicensing && model && licStatus && licenceId && licenceValid && allowsLicenceId(model) ? licenceId : undefined,
  }
  const debouncedParams = useDebouncedValue(JSON.stringify(readinessParams), 300)
  const readinessEnabled = !recordsLicensing || jurisValid
  const readinessQuery = useQuery({
    queryKey: ['launch-readiness', tenantId, debouncedParams],
    queryFn: () => getLaunchReadiness(tenantId, JSON.parse(debouncedParams) as ReadinessParams),
    enabled: readinessEnabled,
    retry: false,
  })
  const readiness = readinessEnabled ? readinessQuery.data?.readiness : undefined
  const nonPassing = readiness?.non_passing_check_codes ?? []
  const allAcked = nonPassing.every((c) => acked.has(c))

  const mutation = useMutation({
    mutationFn: (body: LaunchCreateBody) => createLaunchRequest(tenantId, body),
    onSuccess: (res) => onDone(res),
    onSettled: () => {
      submitting.current = false
    },
  })

  const licenceBacked = model === 'platform_licence' || model === 'tenant_licence'
  const missing: string[] = []
  if (!reason) missing.push('a reason code')
  if (recordsLicensing) {
    if (!model || !licStatus) missing.push('the licensing model and status')
    if (!opName.trim()) missing.push('the responsible operator name')
    if (!jurisValid) missing.push('1 to 32 valid jurisdiction ids')
    if (licenceBacked && !UUID_RE.test(licenceId)) missing.push('a licence id')
    if (!licenceValid) missing.push('a valid licence id')
    if (model && needsDetermination(model) && (!detRef.trim() || !detRole.trim())) missing.push('the determination reference and responsible function')
  }
  if (!readiness) missing.push('the readiness evaluation')
  if (readiness && !allAcked) missing.push('acknowledgement of every non-passing check')

  function submit() {
    if (submitting.current || missing.length > 0 || !readiness) return
    submitting.current = true
    const body: LaunchCreateBody = {
      action,
      reason_code: reason,
      acknowledged_check_codes: nonPassing,
      ...(brandId ? { brand_id: brandId } : {}),
      ...(note.trim() ? { conditions_note: note.trim() } : {}),
    }
    if (recordsLicensing && model && licStatus) {
      body.licensing = {
        model,
        status: licStatus,
        ...(licenceId && allowsLicenceId(model) ? { licence_id: licenceId } : {}),
        ...(needsDetermination(model) ? { determination_reference: detRef.trim(), determination_by_role: detRole.trim() } : {}),
      }
      body.responsible_operator_name = opName.trim()
      if (opReg.trim()) body.responsible_operator_registration = opReg.trim()
      body.jurisdiction_ids = jurisdictionIds
    }
    mutation.mutate(body)
  }

  function toggleJurisdiction(id: string, on: boolean) {
    const cur = new Set(jurisdictionIds)
    if (on) cur.add(id)
    else cur.delete(id)
    setJurisText([...cur].join(', '))
  }

  const title = `${actionLabel(action)} request`
  const registryData = registries.data

  return (
    <Modal title={title} onClose={onClose} size="lg">
      <div className="flex flex-col gap-4">
        <LaunchDisclaimer />
        <p className="text-sm text-slate-600">
          Filing this request changes no status by itself. It needs the distinct platform approvers named after it is filed.
          Four-eyes approval cannot be skipped.
        </p>

        <SelectField
          label="Reason code"
          value={reason}
          onChange={(e) => setReason(e.target.value)}
          options={[{ value: '', label: 'Select a reason' }, ...reasons]}
        />

        {recordsLicensing && (
          <fieldset className="flex flex-col gap-3 rounded-md border border-border p-3">
            <legend className="px-1 text-sm font-semibold text-slate-800">Licensing basis (recorded, never judged)</legend>
            <p className="text-xs text-slate-500">
              The model records the basis of this decision. Every model is always available; no licence is required by the system.
            </p>
            <SelectField
              label="Licensing model (recorded basis)"
              value={model}
              onChange={(e) => setModel(e.target.value as LicensingModel | '')}
              options={[{ value: '', label: 'Select a model' }, ...LICENSING_MODEL_OPTIONS]}
            />
            <SelectField
              label="Licensing status (recorded basis)"
              value={licStatus}
              onChange={(e) => setLicStatus(e.target.value as LicensingStatus | '')}
              options={[{ value: '', label: 'Select a status' }, ...LICENSING_STATUS_OPTIONS]}
              help={
                model === 'not_applicable_recorded_determination'
                  ? 'This model requires the status "Not required per determination" and no licence id.'
                  : undefined
              }
            />
            {model && allowsLicenceId(model) && (
              <>
                {registryData ? (
                  <SelectField
                    label="Licence"
                    value={licenceId}
                    onChange={(e) => setLicenceId(e.target.value)}
                    options={[
                      { value: '', label: licenceBacked ? 'Select a licence' : 'None' },
                      ...registryData.licences.map((l) => ({ value: l.id, label: `${l.licence_number} (${l.licensee}, ${l.status})` })),
                    ]}
                  />
                ) : (
                  <TextInput
                    label="Licence id"
                    value={licenceId}
                    onChange={(e) => setLicenceId(e.target.value.trim())}
                    placeholder="UUID of a licence registry row"
                    help={licenceBacked ? 'Required for a licence-backed model.' : 'Optional for this model.'}
                  />
                )}
              </>
            )}
            {model && needsDetermination(model) && (
              <>
                <TextInput
                  label="Determination reference"
                  maxLength={200}
                  value={detRef}
                  onChange={(e) => setDetRef(e.target.value)}
                  help="An external document reference, never the document."
                />
                <TextInput
                  label="Determination by (responsible function)"
                  maxLength={100}
                  value={detRole}
                  onChange={(e) => setDetRole(e.target.value)}
                  help='The function, for example "external counsel", never a person&apos;s name.'
                />
              </>
            )}
            <TextInput label="Responsible operator (legal entity)" maxLength={200} value={opName} onChange={(e) => setOpName(e.target.value)} />
            <TextInput
              label="Operator registration number"
              maxLength={100}
              value={opReg}
              onChange={(e) => setOpReg(e.target.value)}
              help="Company or registry number of the operating entity."
            />
            <TextInput
              label="Jurisdiction ids"
              value={jurisText}
              onChange={(e) => setJurisText(e.target.value)}
              placeholder="Comma-separated UUIDs, 1 to 32"
              help={jurisText && !jurisValid ? 'Enter 1 to 32 valid UUIDs.' : 'Each jurisdiction needs a configuration in effect for this tenant.'}
            />
            {registryData && registryData.jurisdictions.length > 0 && (
              <div className="flex flex-col gap-1" role="group" aria-label="Jurisdiction registry">
                {registryData.jurisdictions.map((j) => (
                  <label key={j.id} className="flex items-center gap-2 text-xs text-slate-700">
                    <input
                      type="checkbox"
                      className="h-4 w-4"
                      checked={jurisdictionIds.includes(j.id)}
                      onChange={(e) => toggleJurisdiction(j.id, e.target.checked)}
                    />
                    {j.code} - {j.name}
                  </label>
                ))}
              </div>
            )}
            <WarningBox>
              {CONDITIONS_NOTE_WARNING} The same applies to the determination reference and the operator fields above.
            </WarningBox>
          </fieldset>
        )}

        <label className="flex flex-col gap-1 text-sm">
          <span className="font-medium text-slate-700">Conditions / notes (optional) - record only, not enforced</span>
          <WarningBox>{CONDITIONS_NOTE_WARNING}</WarningBox>
          <textarea
            aria-label="Conditions note"
            className="rounded-md border border-border px-3 py-2 text-sm shadow-sm focus:border-brand-500 focus:outline-none focus:ring-1 focus:ring-brand-500"
            rows={3}
            maxLength={2000}
            value={note}
            onChange={(e) => setNote(e.target.value)}
          />
          <span className="text-xs text-slate-500">{note.length} / 2000</span>
        </label>

        {action === 'close' && <p className="text-sm font-medium text-red-700">{FUNDS_STRANDED_NOTICE}</p>}

        <section className="flex flex-col gap-2" aria-label="Readiness">
          <h3 className="text-sm font-semibold text-slate-800">Readiness</h3>
          {!readinessEnabled && <p className="text-xs text-slate-500">Enter at least one valid jurisdiction id to evaluate readiness.</p>}
          {readinessEnabled && readinessQuery.isLoading && <LoadingSpinner label="Evaluating readiness..." />}
          {readinessEnabled && readinessQuery.error && <LaunchErrorAlert error={readinessQuery.error} />}
          {readiness && (
            <ReadinessChecklist
              readiness={readiness}
              mode={action === 'close' ? 'close' : 'other'}
              acknowledged={acked}
              onToggleAck={(code, on) =>
                setAcked((prev) => {
                  const next = new Set(prev)
                  if (on) next.add(code)
                  else next.delete(code)
                  return next
                })
              }
            />
          )}
        </section>

        <LaunchErrorAlert error={mutation.error} />
        {missing.length > 0 && <p className="text-xs text-slate-500">Still needed: {missing.join(', ')}.</p>}
        <div className="flex justify-end gap-2">
          <Button variant="secondary" onClick={onClose} disabled={mutation.isPending}>
            Cancel
          </Button>
          <Button variant={action === 'close' ? 'danger' : 'primary'} onClick={submit} disabled={missing.length > 0} isLoading={mutation.isPending}>
            {`File ${actionLabel(action).toLowerCase()} request`}
          </Button>
        </div>
      </div>
    </Modal>
  )
}
