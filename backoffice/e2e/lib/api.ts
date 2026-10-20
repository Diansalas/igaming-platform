// Direct API helpers for SETUP of independent specs (the behaviour under test is driven through the UI). They use the real
// login + TOTP flow and the real governed filing/approval routes: no database shortcut for governed data.
import { loadWorld, totpFor } from './world'
import { sql } from './db'

async function post(path: string, body: unknown, token?: string) {
  const r = await fetch(loadWorld().apiUrl + path, {
    method: 'POST',
    headers: { 'content-type': 'application/json', ...(token ? { authorization: `Bearer ${token}` } : {}) },
    body: JSON.stringify(body),
  })
  const text = await r.text()
  let json: any = null
  try { json = JSON.parse(text) } catch { /* empty */ }
  return { status: r.status, json }
}

export async function apiLogin(key: string): Promise<string> {
  const a = loadWorld().accounts[key]
  const l = await post('/v1/staff/auth/login', { email: a.email, password: a.password, ...(a.tenantSlug ? { tenant_slug: a.tenantSlug } : {}) })
  if (l.status !== 200) throw new Error(`api login ${key}: ${l.status}`)
  const v = await post('/v1/staff/auth/mfa/verify', { challenge_token: l.json.challenge.token, code: await totpFor(key) })
  if (v.status !== 200) throw new Error(`api mfa ${key}: ${v.status}`)
  return v.json.access_token
}
export const apiFile = (token: string, body: unknown) => post('/v1/admin/licensing-changes', body, token)
export const apiApprove = (token: string, id: string, reason = 'e2e_setup_ok') => post(`/v1/admin/licensing-changes/${id}/approve`, { reason_code: reason }, token)

export const tenantId = () => sql(`SELECT id FROM tenants WHERE slug = '${loadWorld().tenantSlug}'`)
export const jurisdictionId = () => sql(`SELECT id FROM jurisdictions WHERE code = 'STG-SYNTHETIC'`)
export const syntheticLicenceId = () => sql(`SELECT id FROM licences WHERE licence_number = 'STG-SYNTHETIC-1'`)

export const daysFromToday = (n: number) => new Date(Date.now() + n * 86400000).toISOString().slice(0, 10)

export interface Scope { country: string; operation: string }
const where = (s: Scope) => `tenant_id = '${tenantId()}' AND brand_id IS NULL AND product_code IS NULL AND country_code = '${s.country}' AND operation_code = '${s.operation}'`
/** The current (open) version of a scope, as ground truth from the database; null when there is none. */
export function currentBasis(s: Scope): null | { id: string; status: string; kind: string; reviewDue: string; rereview: boolean; changeRequestId: string; versions: number } {
  const row = sql(`SELECT id||'|'||status||'|'||basis_kind||'|'||coalesce(review_due_at::text,'')||'|'||needs_rereview::text||'|'||coalesce(change_request_id::text,'') FROM operating_bases WHERE ${where(s)} AND effective_to IS NULL`)
  if (!row) return null
  const [id, status, kind, reviewDue, rereview, changeRequestId] = row.split('|')
  return { id, status, kind, reviewDue, rereview: rereview === 'true', changeRequestId, versions: Number(sql(`SELECT count(*) FROM operating_bases WHERE ${where(s)}`)) }
}

export function setPayload(s: Scope, over: Record<string, unknown> = {}) {
  return {
    tenant_id: tenantId(), brand_id: null, country_code: s.country, operation_code: s.operation, product_code: null,
    basis_kind: 'no_licence_documented', licence_id: null, jurisdiction_id: jurisdictionId(),
    determination_reference: `E2E-DET-${s.country}-${s.operation}`, determined_by_role: 'head_of_compliance',
    basis_evidence_reference: null, restrictions_note: null, restricted_product_codes: [], review_due_at: daysFromToday(90), ...over,
  }
}

/** Creates an ACTIVE documented no-licence basis for a scope through the real filing + two-approver route. */
export async function seedActiveBasis(s: Scope, over: Record<string, unknown> = {}) {
  const a = await apiLogin('PA_A')
  const f = await apiFile(a, { kind: 'operating_basis_set', reason_code: 'e2e_setup', payload: setPayload(s, over) })
  if (f.status !== 201 || f.json?.executed) throw new Error(`seed file: ${f.status} ${JSON.stringify(f.json).slice(0, 200)}`)
  const id = f.json.change_request.id
  for (const k of ['PA_B', 'PA_C']) {
    const r = await apiApprove(await apiLogin(k), id)
    if (r.status !== 200) throw new Error(`seed approve ${k}: ${r.status} ${JSON.stringify(r.json).slice(0, 200)}`)
  }
  const b = currentBasis(s)
  if (!b || b.status !== 'active') throw new Error('seed: basis not active')
  return b
}
