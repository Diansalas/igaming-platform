// lic22: AML screening harness helpers (spec 10). Sanctioned paths only:
//   * tenant jurisdiction configuration and officer capability grants go through the real governed HTTP routes (two distinct approvers);
//   * the jurisdiction SCREENING policy has no HTTP route, so tools/aml-fixture calls the real AML officer service (see its header);
//   * the MOCK screening worker is the one platform-api starts itself outside production (nothing is started or faked here);
//   * the only owner-role write is the synthetic MOCK marker on a KYC verification's vendor subject reference (see markVerification).
import { execFileSync, spawn, type ChildProcessWithoutNullStreams } from 'node:child_process'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { expect, type Locator } from 'playwright/test'
import { apiLogin } from './api'
import { sql } from './db'
import { clickWithStepUp, spaGoto, type Session } from './session'
import { sqlT } from './wd_db'
import { call, type WdPlayer, type WdTenant } from './wd_world'

const repoRoot = join(fileURLToPath(new URL('.', import.meta.url)), '..', '..', '..')
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))
export const staffId = (key: string) => sql(`SELECT id FROM staff_users WHERE email = '${key.toLowerCase()}@e2e.invalid'`)
export const jurisdictionId = () => sql(`SELECT id FROM jurisdictions WHERE code = 'STG-SYNTHETIC'`)

/** The tenant is configured to operate in the synthetic jurisdiction (a governed change: filed by PA_A, approved by PA_B and PA_C). */
export async function configureTenantJurisdiction(t: WdTenant) {
  const f = await call('POST', '/v1/admin/licensing-changes', await apiLogin('PA_A'), {
    kind: 'tenant_jurisdiction_config_version', reason_code: 'e2e_setup',
    payload: { tenant_id: t.id, jurisdiction_id: jurisdictionId(), action: 'set', kyc_ruleset_id: null, aml_ruleset_id: null, rg_ruleset_id: null, reporting_ruleset_id: null, allowed_currencies: ['EUR'], allowed_payment_methods: ['card', 'bank_transfer'], geo_block_list: [] },
  })
  if (f.status !== 201) throw new Error(`tenant jurisdiction config: ${f.status} ${f.text.slice(0, 200)}`)
  if (!f.json.executed) {
    for (const k of ['PA_B', 'PA_C']) {
      const r = await call('POST', `/v1/admin/licensing-changes/${f.json.change_request.id}/approve`, await apiLogin(k), { reason_code: 'e2e_ok' })
      if (r.status !== 200) throw new Error(`approve tenant jurisdiction config (${k}): ${r.status} ${r.text.slice(0, 200)}`)
    }
  }
  expect(sqlT(t.id, `SELECT count(*) FROM tenant_jurisdiction_configs WHERE tenant_id='${t.id}'`)).toBe('1')
}

/** aml_officer grant (platform scope) for `grantee`, requested by `requester`, approved by `approver` (three distinct Persons). Returns the grant id. */
export async function grantOfficer(t: WdTenant, grantee: string, requester = 'PA_A', approver = 'PA_B'): Promise<string> {
  const rq = await call('POST', `/v1/admin/tenants/${t.id}/capability-grants/requests`, await apiLogin(requester), {
    grantee_staff_id: staffId(grantee), capability: 'aml_officer', reason_code: 'e2e_officer', valid_until: new Date(Date.now() + 3 * 3600_000).toISOString(),
  })
  if (rq.status !== 201) throw new Error(`officer request: ${rq.status} ${rq.text.slice(0, 200)}`)
  const ap = await call('POST', `/v1/admin/tenants/${t.id}/capability-grants/requests/${rq.json.id}/approve`, await apiLogin(approver), { reason_code: 'e2e_officer_ok' })
  if (ap.status !== 200) throw new Error(`officer approve: ${ap.status} ${ap.text.slice(0, 200)}`)
  return ap.json.id
}

export async function revokeGrant(t: WdTenant, grantId: string, by = 'PA_A') {
  const r = await call('POST', `/v1/admin/tenants/${t.id}/capability-grants/${grantId}/revoke`, await apiLogin(by), { reason_code: 'e2e_revoke' })
  if (r.status !== 204) throw new Error(`revoke: ${r.status} ${r.text.slice(0, 200)}`)
}

/** Builds (once per run) and runs tools/aml-fixture: the jurisdiction screening policy, authored by the given platform officer. */
export function authorScreeningPolicy(t: WdTenant, officerKey: string): string {
  const tool = join(process.env.E2E_RUN_DIR!, 'bin', 'aml-fixture')
  execFileSync('go', ['build', '-tags', 'e2efixture', '-o', tool, './backoffice/e2e/tools/aml-fixture'], { cwd: repoRoot, stdio: ['ignore', 'ignore', 'inherit'] })
  return execFileSync(tool, ['screening-policy', '-tenant', t.id, '-staff', staffId(officerKey), '-jurisdiction', jurisdictionId()], {
    env: { ...process.env, APP_ENV: 'development', DATABASE_URL: process.env.E2E_RUNTIME_DATABASE_URL! },
  }).toString().trim()
}

// ---------------------------------------------------------------------------------------------------------------------------
// MOCK screening worker heartbeat

export const heartbeatFresh = () => Number(sql(`SELECT count(*) FROM screening_worker_heartbeats WHERE last_seen_at > clock_timestamp() - interval '10 minutes'`)) > 0

/**
 * Makes "no worker heartbeat" true and KEEPS it true until release(): the table owner deletes the heartbeat rows (committed) and then holds
 * SHARE ROW EXCLUSIVE on screening_worker_heartbeats in an open transaction, so the real MOCK worker's next heartbeat UPSERT waits on the lock
 * (readers, including the trigger-policy authoring check, are not blocked). Nothing is stubbed: the authoring function really finds no recent
 * heartbeat. release() commits, the worker's pending heartbeat lands and the worker is unchanged. Owner role, private database only.
 */
export async function withoutHeartbeat(): Promise<{ release: () => Promise<void> }> {
  const u = new URL(process.env.E2E_OWNER_DATABASE_URL!)
  const env = { PATH: process.env.PATH, PGHOST: u.hostname, PGPORT: u.port || '5432', PGUSER: decodeURIComponent(u.username), PGPASSWORD: decodeURIComponent(u.password), PGDATABASE: u.pathname.slice(1), PGSSLMODE: u.searchParams.get('sslmode') ?? 'prefer' }
  for (let attempt = 0; attempt < 12; attempt++) {
    sql(`DELETE FROM screening_worker_heartbeats`)
    const p: ChildProcessWithoutNullStreams = spawn('psql', ['-X', '-v', 'ON_ERROR_STOP=1', '-Atq'], { env })
    let out = ''
    p.stdout.on('data', (d) => (out += d.toString()))
    p.stderr.on('data', (d) => (out += d.toString()))
    p.stdin.write(`BEGIN;\nLOCK TABLE screening_worker_heartbeats IN SHARE ROW EXCLUSIVE MODE;\nSELECT 'LOCKED:' || count(*) FROM screening_worker_heartbeats;\n`)
    for (let i = 0; i < 100 && !/LOCKED:\d+/.test(out); i++) await sleep(100)
    const m = /LOCKED:(\d+)/.exec(out)
    if (m && m[1] === '0') {
      return {
        release: async () => {
          p.stdin.write('COMMIT;\n\\q\n')
          await new Promise<void>((r) => p.on('exit', () => r()))
        },
      }
    }
    // a heartbeat slipped in between the delete and the lock: roll back and try again
    p.stdin.write('ROLLBACK;\n\\q\n')
    await new Promise<void>((r) => p.on('exit', () => r()))
  }
  throw new Error('could not establish a no-heartbeat window')
}

export async function waitHeartbeat(timeoutMs = 60000) {
  const t0 = Date.now()
  while (Date.now() - t0 < timeoutMs) {
    if (Number(sql(`SELECT count(*) FROM screening_worker_heartbeats WHERE last_seen_at > clock_timestamp() - interval '30 seconds'`)) > 0) return
    await sleep(1000)
  }
  throw new Error('the MOCK screening worker did not report a heartbeat')
}

// ---------------------------------------------------------------------------------------------------------------------------
// players and the MOCK marker

/**
 * FIXTURE (owner role, synthetic): sets the vendor subject reference of a verification to a MOCK screening marker (SYNTH-SANCTION-... /
 * SYNTH-PEP-...), which is the only way the MOCK provider (internal/aml/screening_mock.go) is told to answer a match. The KYC MOCK provider
 * never produces such a reference itself. The real worker then reads it exactly as it would read a vendor reference.
 */
export function markVerification(t: WdTenant, verificationId: string, marker: 'SYNTH-SANCTION-' | 'SYNTH-PEP-') {
  const ref = `${marker}e2e${Date.now()}`
  const out = sqlT(t.id, `UPDATE kyc_verifications SET provider_reference='${ref}' WHERE id='${verificationId}' RETURNING provider_reference`)
  expect(out).toBe(ref)
}

export interface Req { id: string; trigger: string; status: string; outcome?: string; match_lists?: string[]; resolved: boolean; idempotency_key: string }
export async function screeningRequests(t: WdTenant, officer: string, personId: string): Promise<Req[]> {
  const r = await call('GET', `/v1/admin/tenants/${t.id}/screening/persons/${personId}/requests`, await apiLogin(officer))
  if (r.status !== 200) throw new Error(`list screening requests: ${r.status} ${r.text.slice(0, 200)}`)
  return r.json.items
}
export async function waitRequest(t: WdTenant, officer: string, personId: string, pred: (r: Req) => boolean, what: string, timeoutMs = 90000): Promise<Req> {
  const t0 = Date.now()
  for (;;) {
    const hit = (await screeningRequests(t, officer, personId)).find(pred)
    if (hit) return hit
    if (Date.now() - t0 > timeoutMs) throw new Error(`timeout waiting for ${what}`)
    await sleep(2000)
  }
}

/** A second KYC verification for an already-approved player, approved by compliance (a new kyc_approval event). Marker optional. */
export async function secondVerification(t: WdTenant, p: WdPlayer, marker?: 'SYNTH-SANCTION-' | 'SYNTH-PEP-'): Promise<string> {
  const v = await call('POST', '/v1/me/kyc/verifications', p.token)
  if (v.status !== 201 && v.status !== 200) throw new Error(`second kyc start: ${v.status} ${v.text.slice(0, 200)}`)
  for (let i = 0; i < 60; i++) {
    const g = await call('GET', '/v1/me/kyc/verifications', p.token)
    if (g.json?.find((x: any) => x.id === v.json.id)?.status !== 'unverified') break
    await sleep(1000)
  }
  if (marker) markVerification(t, v.json.id, marker)
  const rv = await call('POST', `/v1/admin/kyc/verifications/${v.json.id}/review`, await apiLogin(`${t.tag}_COMPLIANCE`), { status: 'approved', reason: 'e2e synthetic second verification' })
  if (rv.status !== 200) throw new Error(`second kyc review: ${rv.status} ${rv.text.slice(0, 200)}`)
  return v.json.id
}

// ---------------------------------------------------------------------------------------------------------------------------
// UI drivers (features/aml/ScreeningPage.tsx)

export async function openScreening(s: Session, t: WdTenant) {
  await spaGoto(s.page, '/aml')
  await s.page.getByRole('link', { name: 'Automatic screening: trigger policy and screening requests' }).click()
  await expect(s.page.getByRole('heading', { name: 'AML Screening' })).toBeVisible()
  await s.page.getByLabel('Find tenant').fill(t.slug)
  const sel = s.page.getByLabel('Tenant', { exact: true })
  await expect(sel.locator('option', { hasText: t.slug })).toHaveCount(1)
  await sel.selectOption(t.id)
}

export async function showPolicy(s: Session) {
  // leave and re-enter the tab so the policy is read afresh (the screen caches the answer per jurisdiction)
  await s.page.getByRole('tab', { name: 'Screening requests' }).click()
  await s.page.getByRole('tab', { name: 'Trigger policy' }).click()
  await s.page.getByLabel('Licence jurisdiction').selectOption(jurisdictionId())
  await s.page.getByRole('button', { name: 'Show policy in force' }).click()
}

const localInput = (d: Date) => {
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`
}

export interface PolicyForm { events: ('Registration' | 'KYC approval' | 'Scheduled re-screen')[]; gate: boolean; seconds?: string; legal?: string; reason: string }
/** Opens the authoring dialog and fills every explicit field (nothing is preselected). Does not submit. */
export async function fillPolicy(s: Session, f: PolicyForm): Promise<Locator> {
  await s.page.getByRole('button', { name: 'Author next version' }).click()
  const d = s.page.getByRole('dialog', { name: 'Author the next trigger policy version' })
  for (const e of f.events) await d.getByLabel(e, { exact: true }).check()
  await d.getByLabel('Withdrawal gate', { exact: true }).selectOption(f.gate ? 'on' : 'off')
  if (f.gate) {
    await d.getByLabel('Trapped alert after (seconds)').fill(f.seconds ?? '600')
    await d.getByLabel(/I understand that a withdrawal gate/).check()
  }
  // the datetime-local input has minute resolution: use the next full minute (at least 5 s ahead), which the server accepts (it allows up to
  // 30 days ahead); the version takes effect at that time (waitPolicyEffective)
  const next = new Date(Math.ceil((Date.now() + 5000) / 60000) * 60000)
  await d.getByLabel('Effective from').fill(localInput(next))
  await d.getByLabel('Legal review reference').fill(f.legal ?? 'E2E-SYNTHETIC-NOT-A-LEGAL-REVIEW')
  await d.getByLabel('Reason code', { exact: true }).fill(f.reason)
  return d
}

export async function submitPolicy(s: Session, d: Locator, done: () => Promise<void>) {
  await clickWithStepUp(s, () => d.getByRole('button', { name: 'Author version' }).click(), done)
}

export async function showRequests(s: Session, personId: string) {
  await s.page.getByRole('tab', { name: 'Screening requests' }).click()
  await s.page.getByLabel('Person id').fill(personId)
  await s.page.getByRole('button', { name: 'Show requests' }).click()
}

export interface ResolveForm { outcome: 'Clear (no match)' | 'Potential match (places a hold)' | 'Confirmed match (places a hold)'; lists: ('Sanctions list' | 'PEP list')[]; matches?: ('Sanctions list' | 'PEP list')[]; reason: string }
export async function fillResolve(s: Session, requestRow: Locator, f: ResolveForm): Promise<Locator> {
  await requestRow.getByRole('button', { name: 'Resolve' }).click()
  const d = s.page.getByRole('dialog', { name: 'Resolve a screening request' })
  await d.getByLabel('Outcome').selectOption({ label: f.outcome })
  const lists = d.getByRole('group', { name: 'Lists screened' })
  for (const l of f.lists) await lists.getByLabel(l, { exact: true }).check()
  if (f.matches) {
    const m = d.getByRole('group', { name: 'Lists with a match' })
    for (const l of f.matches) await m.getByLabel(l, { exact: true }).check()
  }
  await d.getByLabel('Reason code', { exact: true }).fill(f.reason)
  return d
}
export async function submitResolve(s: Session, d: Locator, done: () => Promise<void>) {
  await clickWithStepUp(s, () => d.getByRole('button', { name: 'Record resolution' }).click(), done)
}

export const requestRow = (s: Session, requestId: string) => s.page.getByRole('row').filter({ hasText: requestId.slice(0, 8) })

/** Waits until the trigger policy in force for the synthetic jurisdiction has the expected gate flag (a version applies from its effective time). */
export async function waitPolicyEffective(t: WdTenant, officer: string, gate: boolean, timeoutMs = 120000) {
  const t0 = Date.now()
  for (;;) {
    const r = await call('GET', `/v1/admin/tenants/${t.id}/screening/trigger-policy?jurisdiction_id=${jurisdictionId()}`, await apiLogin(officer))
    if (r.status === 200 && r.json.configured && r.json.gate_withdrawal === gate) return
    if (Date.now() - t0 > timeoutMs) throw new Error('trigger policy did not become effective')
    await sleep(3000)
  }
}
