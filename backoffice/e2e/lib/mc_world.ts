// lic32: ground truth and setup helpers for the market-configuration spec (11). Reads are owner-role (private database only);
// governed data is created only through the real filing + approval routes; the market RESOLVER is the real Go code, called by
// tools/mcprobe over the runtime role (no HTTP endpoint exposes it).
import { execFileSync } from 'node:child_process'
import { join } from 'node:path'
import { apiApprove, apiFile, apiLogin } from './api'
import { sql } from './db'
import { sqlT } from './wd_db'
import { loadWorld } from './world'

export async function mcCall(method: string, path: string, token: string, body?: unknown): Promise<{ status: number; json: any; text: string }> {
  const r = await fetch(loadWorld().apiUrl + path, {
    method,
    headers: { 'content-type': 'application/json', ...(token ? { authorization: `Bearer ${token}` } : {}) },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  const text = await r.text()
  let json: any = null
  try { json = JSON.parse(text) } catch { /* not json */ }
  return { status: r.status, json, text }
}

/** The REAL market resolver outcome for (tenant, country, operation) at the database clock, e.g. "permitted", "not_permitted_by_licence". */
export function mcResolve(tenant: string, country: string, operation: string): string {
  const bin = join(process.env.E2E_RUN_DIR!, 'bin', 'e2e-mcprobe')
  const out = execFileSync(bin, ['resolve', tenant, country, operation], { env: { PATH: process.env.PATH, E2E_RUNTIME_DATABASE_URL: process.env.E2E_RUNTIME_DATABASE_URL! } }).toString().trim()
  const m = /^RESOLVE\|(\S+)$/.exec(out)
  if (!m) throw new Error(`mcprobe: unexpected output ${out}`)
  return m[1]
}

export interface CeilingRow { id: string; state: string; status: string; ref: string; versions: number }
/** The open (current) ceiling version of (licence, country) and the number of versions ever stored; null when none. */
export function mcCeiling(licenceId: string, country: string): CeilingRow | null {
  const total = Number(sql(`SELECT count(*) FROM licence_country_ceilings WHERE licence_id='${licenceId}' AND country_code='${country}'`))
  if (total === 0) return null
  const row = sql(`SELECT id||'|'||state||'|'||status||'|'||coalesce(authorization_reference,'') FROM licence_country_ceilings WHERE licence_id='${licenceId}' AND country_code='${country}' AND effective_to IS NULL`)
  if (!row) return { id: '', state: 'none-open', status: 'none-open', ref: '', versions: total }
  const [id, state, status, ref] = row.split('|')
  return { id, state, status, ref, versions: total }
}
export const mcCeilingVersions = (licenceId: string, country: string) => Number(sql(`SELECT count(*) FROM licence_country_ceilings WHERE licence_id='${licenceId}' AND country_code='${country}'`))

export interface PolicyScopeKey { tenant: string; scope: 'tenant' | 'brand' | 'operation'; country: string; brand?: string | null; operation?: string | null }
const pWhere = (k: PolicyScopeKey) =>
  `tenant_id='${k.tenant}' AND scope_kind='${k.scope}' AND country_code='${k.country}' AND ${k.brand ? `brand_id='${k.brand}'` : 'brand_id IS NULL'} AND ${k.operation ? `operation_code='${k.operation}'` : 'operation_code IS NULL'}`
export interface PolicyRow { id: string; state: string; status: string; ref: string; versions: number }
export function mcPolicy(k: PolicyScopeKey): PolicyRow | null {
  const total = Number(sqlT(k.tenant, `SELECT count(*) FROM operating_country_policies WHERE ${pWhere(k)}`))
  if (total === 0) return null
  const row = sqlT(k.tenant, `SELECT id||'|'||state||'|'||status||'|'||coalesce(authorization_reference,'') FROM operating_country_policies WHERE ${pWhere(k)} AND effective_to IS NULL`)
  if (!row) return { id: '', state: 'none-open', status: 'none-open', ref: '', versions: total }
  const [id, state, status, ref] = row.split('|')
  return { id, state, status, ref, versions: total }
}
export const mcPolicyVersions = (k: PolicyScopeKey) => Number(sqlT(k.tenant, `SELECT count(*) FROM operating_country_policies WHERE ${pWhere(k)}`))
/** All policy rows of the tenant for a country (any scope): proof that a refused filing stored nothing. */
export const mcPolicyAnyCount = (tenant: string, country: string) => Number(sqlT(tenant, `SELECT count(*) FROM operating_country_policies WHERE tenant_id='${tenant}' AND country_code='${country}'`))

export interface ChangeRow { status: string; widening: boolean; required: number; approvals: number; kind: string; requestedBy: string }
export function mcChange(id: string): ChangeRow {
  const [status, widening, required, approvals, kind, by] = sql(`SELECT status||'|'||widening::text||'|'||required_approvals||'|'||(SELECT count(*) FROM licensing_change_approvals a WHERE a.request_id = r.id)||'|'||kind||'|'||requested_by FROM licensing_change_requests r WHERE id='${id}'`).split('|')
  return { status, widening: widening === 'true', required: Number(required), approvals: Number(approvals), kind, requestedBy: by }
}
export const mcChangeCount = (kind: string) => Number(sql(`SELECT count(*) FROM licensing_change_requests WHERE kind='${kind}'`))

const must = (what: string, r: { status: number; text: string }, ok: number[]) => {
  if (!ok.includes(r.status)) throw new Error(`${what}: ${r.status} ${r.text.slice(0, 240)}`)
}

/** File a governed change through the real route and (when it is pending) approve it with two distinct Persons. Returns the change id. */
export async function mcFileAndApprove(body: unknown, approvers: string[] = ['PA_B', 'PA_C'], filer = 'PA_A'): Promise<string> {
  const f = await apiFile(await apiLogin(filer), body)
  must('file', { status: f.status, text: JSON.stringify(f.json) ?? '' }, [201])
  const id = f.json.change_request.id as string
  if (!f.json.executed) {
    for (const k of approvers) {
      const r = await apiApprove(await apiLogin(k), id, 'e2e_setup_ok')
      must(`approve ${k}`, { status: r.status, text: JSON.stringify(r.json) ?? '' }, [200])
    }
  }
  return id
}

export const ceilingBody = (licenceId: string, country: string, kind: 'enable' | 'disable' | 'withdraw', ref: string | null = null) => ({
  kind: 'licence_ceiling_version', reason_code: 'e2e_setup',
  payload: { licence_id: licenceId, country_code: country, state: kind === 'enable' ? 'enabled' : 'disabled', status: kind === 'withdraw' ? 'withdrawn' : 'active', authorization_reference: ref },
})
export const policyBody = (p: { tenant: string; scope: 'tenant' | 'brand' | 'operation'; country: string; kind: 'enable' | 'disable' | 'withdraw'; ref?: string | null; brand?: string | null; operation?: string | null; product?: string | null }) => ({
  kind: 'operating_policy_version', reason_code: 'e2e_setup',
  payload: {
    tenant_id: p.tenant, scope: p.scope, brand_id: p.brand ?? null, operation_code: p.operation ?? null, product_code: p.product ?? null, country_code: p.country,
    state: p.kind === 'enable' ? 'enabled' : 'disabled', status: p.kind === 'withdraw' ? 'withdrawn' : 'active', authorization_reference: p.ref ?? null,
  },
})

export async function mcCreateJurisdictionApi(code: string, name: string, by = 'PA_A') {
  return mcCall('POST', '/v1/admin/jurisdictions', await apiLogin(by), { code, name, reason_code: 'e2e_setup' })
}

export const mcBrandId = (tenant: string) => sqlT(tenant, `SELECT id FROM brands WHERE tenant_id='${tenant}' ORDER BY created_at LIMIT 1`)
export const mcJurisdictionByCode = (code: string) => sql(`SELECT id FROM jurisdictions WHERE code='${code}'`)
