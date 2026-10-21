import { useQuery } from '@tanstack/react-query'
import { listJurisdictions } from '../../api/licensing'
import { SelectField, TextInput } from '../../components/FormField'
import { describeScreeningError, type ScreeningOp } from './screeningErrors'
import { safeToken } from './ScreeningLabels'

export const LEGAL_REF_MAX = 200
// eslint-disable-next-line no-control-regex
const CONTROL = /[\u0000-\u001f\u007f-\u009f]/
export const isLegalReference = (s: string) => s.length >= 1 && s.length <= LEGAL_REF_MAX && s === s.trim() && !CONTROL.test(s)

export function ScreeningError({ error, op }: { error: unknown; op: ScreeningOp }) {
  if (!error) return null
  const v = describeScreeningError(error, op)
  return (
    <div role="alert" className="rounded-md border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-800">
      <p className="font-medium">{v.message}</p>
      {v.requestId && <p className="mt-1 text-xs text-red-600">Request ID: {safeToken(v.requestId) === 'unprintable' ? 'unprintable' : v.requestId}</p>}
    </div>
  )
}

export function JurisdictionInput({ value, onChange, selectable }: { value: string; onChange: (v: string) => void; selectable: boolean }) {
  const list = useQuery({ queryKey: ['aml', 'screening', 'jurisdictions'], queryFn: listJurisdictions, enabled: selectable, retry: false })
  if (selectable && list.data) {
    return (
      <SelectField
        label="Licence jurisdiction"
        value={value}
        onChange={(e) => onChange(e.target.value)}
        options={[{ value: '', label: 'Choose a jurisdiction' }, ...list.data.map((j) => ({ value: j.id, label: `${safeToken(j.code)} - ${j.name}` }))]}
      />
    )
  }
  return <TextInput label="Jurisdiction id" value={value} onChange={(e) => onChange(e.target.value.trim())} autoComplete="off" help="The licensing jurisdiction (a UUID) whose policy you want." />
}

