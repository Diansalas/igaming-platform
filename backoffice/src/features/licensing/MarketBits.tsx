import { Link } from 'react-router-dom'
import { UUID_RE } from './basisRules'
import { AUTH_REF_NOTE, CLASS_WORDS, MARKET_DISCLAIMER } from './marketLabels'

export const MARKET_CHANGE_KINDS = ['licence_ceiling_version', 'operating_policy_version', 'tenant_jurisdiction_config_version']

const str = (v: unknown): string | null => (typeof v === 'string' ? v : null)
const list = (v: unknown): string => (Array.isArray(v) && v.every((x) => typeof x === 'string') ? ((v as string[]).length ? (v as string[]).join(', ') : 'none') : 'not printable')
const idText = (v: unknown): string => (typeof v === 'string' && UUID_RE.test(v) ? v : 'not printable')

function stateWords(state: unknown, status: unknown): string {
  if (state === 'enabled' && status === 'active') return 'ENABLE'
  if (state === 'disabled' && status === 'active') return 'DISABLE'
  if (state === 'disabled' && status === 'withdrawn') return 'WITHDRAW the version for'
  return 'record an unrecognised state for'
}

/** The sentence an approver reads. Text only (React escapes it); an unexpected value is printed as "not printable". */
function sentence(kind: string, p: Record<string, unknown>): string {
  const country = str(p.country_code) ?? 'not printable'
  if (kind === 'licence_ceiling_version') {
    return `On licence ${idText(p.licence_id)}: ${stateWords(p.state, p.status)} country ${country} in the licence country ceiling.`
  }
  if (kind === 'operating_policy_version') {
    const scope = p.scope
    const where =
      scope === 'tenant'
        ? 'the whole tenant'
        : scope === 'brand'
          ? `brand ${idText(p.brand_id)}`
          : `operation ${str(p.operation_code) ?? 'not printable'}, ${p.brand_id ? `brand ${idText(p.brand_id)}` : 'all brands'}, ${p.product_code ? `product ${str(p.product_code) ?? 'not printable'}` : 'all products'}`
    const rung = scope === 'tenant' || scope === 'brand' || scope === 'operation' ? String(scope) : 'unrecognised'
    return `For tenant ${idText(p.tenant_id)}, ${rung} rung: ${stateWords(p.state, p.status)} country ${country} for ${where}.`
  }
  const action = p.action === 'close' ? 'CLOSE the version in force' : p.action === 'set' ? 'SET a new version (replacing the version in force)' : 'record an unrecognised action on'
  return `For tenant ${idText(p.tenant_id)}: ${action} of the configuration for jurisdiction ${idText(p.jurisdiction_id)}.`
}

const ROWS: Record<string, [string, string][]> = {
  licence_ceiling_version: [
    ['licence_id', 'Licence'],
    ['country_code', 'Country'],
    ['state', 'State'],
    ['status', 'Version status'],
    ['authorization_reference', 'Authorization reference (supplied by the operator, not verified)'],
  ],
  operating_policy_version: [
    ['tenant_id', 'Tenant'],
    ['scope', 'Rung'],
    ['brand_id', 'Brand'],
    ['operation_code', 'Operation'],
    ['product_code', 'Product'],
    ['country_code', 'Country'],
    ['state', 'State'],
    ['status', 'Version status'],
    ['authorization_reference', 'Authorization reference (supplied by the operator, not verified)'],
  ],
  tenant_jurisdiction_config_version: [
    ['tenant_id', 'Tenant'],
    ['jurisdiction_id', 'Jurisdiction'],
    ['action', 'Action'],
    ['kyc_ruleset_id', 'KYC ruleset id'],
    ['aml_ruleset_id', 'AML ruleset id'],
    ['rg_ruleset_id', 'Responsible gaming ruleset id'],
    ['reporting_ruleset_id', 'Reporting ruleset id'],
    ['allowed_currencies', 'Allowed currencies'],
    ['allowed_payment_methods', 'Allowed payment methods'],
    ['geo_block_list', 'Geo block list'],
  ],
}
const NULL_TEXT: Record<string, string> = { brand_id: 'Not set', operation_code: 'Not set', product_code: 'Not set', authorization_reference: 'None' }

function valueText(key: string, v: unknown): string {
  if (v === null || v === undefined) return NULL_TEXT[key] ?? 'None'
  if (Array.isArray(v)) return list(v)
  if (typeof v === 'string') return v
  return 'not printable'
}

/** A readable view of a market-configuration change payload, so approvers decide on words and not on raw JSON. */
export function MarketPayloadSummary({ kind, payload, widening }: { kind: string; payload: Record<string, unknown>; widening: boolean }) {
  if (!MARKET_CHANGE_KINDS.includes(kind)) return null
  const tenant = typeof payload.tenant_id === 'string' && UUID_RE.test(payload.tenant_id) ? payload.tenant_id : null
  const licence = typeof payload.licence_id === 'string' && UUID_RE.test(payload.licence_id) ? payload.licence_id : null
  const rows = ROWS[kind].filter(([k]) => Object.prototype.hasOwnProperty.call(payload, k))
  return (
    <section aria-label="Market configuration summary" className="flex flex-col gap-2">
      <p role="note" aria-label="Market configuration disclaimer" className="rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-sm text-amber-900">{MARKET_DISCLAIMER}</p>
      <p aria-label="Change in words" className="text-sm font-medium text-slate-800">{sentence(kind, payload)}</p>
      <p aria-label="Approval path" className="text-sm text-slate-700">{widening ? CLASS_WORDS.widening : CLASS_WORDS.narrowing}.</p>
      <dl className="grid grid-cols-2 gap-3 text-sm sm:grid-cols-3">
        {rows.map(([k, label]) => (
          <div key={k}>
            <dt className="text-slate-500">{label}</dt>
            <dd className="mt-1 break-words">{valueText(k, payload[k])}</dd>
          </div>
        ))}
      </dl>
      {Object.prototype.hasOwnProperty.call(payload, 'authorization_reference') && <p role="note" aria-label="Authorization reference note" className="text-xs text-slate-600">{AUTH_REF_NOTE}</p>}
      {tenant && <Link className="text-sm text-brand-700 hover:underline" to={`/licensing/tenants/${encodeURIComponent(tenant)}`}>See this tenant&apos;s licensing</Link>}
      {licence && <Link className="text-sm text-brand-700 hover:underline" to={`/licensing/licences/${encodeURIComponent(licence)}`}>See this licence</Link>}
    </section>
  )
}
