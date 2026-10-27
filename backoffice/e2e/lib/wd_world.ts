// lic22: synthetic tenants, players and money flows for the withdrawal-restriction (09) and AML-screening (10) specs.
// Everything goes through sanctioned paths of the real stack:
//   * tenants: cmd/seed-staging-world (the same tool seed.mjs uses), one extra synthetic ACTIVE tenant per scenario;
//     tenant staff are enrolled over the real HTTP flow (setup/enrol.mjs against a temporary world file);
//   * the payments capability: PUT /v1/admin/providers/mock-payments/capability as that tenant's tenant_admin;
//   * players: /v1/auth/register, e-mail confirmation (test-support seam), deposit + MOCK PSP simulate-callback (the real
//     signature-verifying pipeline), MOCK KYC started by the player and approved by the tenant's compliance staff over HTTP,
//     payout instrument registration (MOCK verifier), withdrawals;
//   * the only owner (table-owner) writes are licence fixtures (the documented fixture path of lib/db.ts) and re-pointing
//     tenants.licence_id after the player is funded (the market gate needs the seeded licence while the player is set up).
import { execFileSync } from 'node:child_process'
import { randomBytes } from 'node:crypto'
import { mkdtempSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { apiLogin } from './api'
import { createLicence, sql } from './db'
import { sqlT } from './wd_db'
import { loadWorld, type Account } from './world'

const HERE = fileURLToPath(new URL('.', import.meta.url))
const worldFile = () => process.env.E2E_WORLD_FILE!

export interface WdTenant { tag: string; slug: string; brandSlug: string; id: string; brandId: string; licenceId: string; licenceNumber: string }

async function http(method: string, path: string, token: string | '', body?: unknown): Promise<{ status: number; json: any; text: string }> {
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
export const call = http

const runDir = () => process.env.E2E_RUN_DIR!
const bin = (n: string) => join(runDir(), 'bin', n)

/** Creates a synthetic ACTIVE tenant + brand + staff (cmd/seed-staging-world), enrols the staff, configures the mock payments capability. */
export async function createTenant(tag: string): Promise<WdTenant> {
  const rnd = randomBytes(3).toString('hex')
  const slug = `e2e-${tag}-${rnd}`
  const brandSlug = `${slug}-b`
  const owner = process.env.E2E_OWNER_DATABASE_URL!
  const toolEnv = { ...process.env, APP_ENV: 'development', DATABASE_URL: owner }
  const OP = ['-operator', 'e2e-harness', '-reason', 'e2e_synthetic_fixture']
  const dir = mkdtempSync(join(runDir(), 'wdcreds-'))
  const planF = join(dir, 'plan.json'), applyF = join(dir, 'apply.json')
  const actor = sql(`SELECT id FROM staff_users WHERE email = 'pa_a@e2e.invalid'`)
  const args = (phase: string) => ['-phase', phase, '-tenant-slug', slug, '-brand-slug', brandSlug, '-country', 'MT', '-authorization-reference', 'E2E-SYNTHETIC-NOT-A-REAL-AUTHORISATION', ...OP, '-actor-id', actor]
  execFileSync(bin('seed-staging-world'), [...args('plan'), '-credentials-out', planF], { env: toolEnv, stdio: ['ignore', 'ignore', 'inherit'] })
  execFileSync(bin('seed-staging-world'), [...args('apply'), '-credentials-in', planF, '-credentials-out', applyF], { env: toolEnv, stdio: ['ignore', 'ignore', 'inherit'] })
  const creds = JSON.parse(readFileSync(applyF, 'utf8'))
  // enrol every account of the new tenant through the real HTTP flow (setup/enrol.mjs), in a temporary world file
  const w = loadWorld()
  const fresh: Record<string, Account & { bootstrapToken?: string }> = {}
  for (const [k, c] of Object.entries<any>(creds.actors)) {
    const key = `${tag}_${k}`
    fresh[key] = { key, email: c.email, password: c.password, tenantSlug: slug, role: k.toLowerCase(), bootstrapToken: c.bootstrap_token, lastStep: 0 }
  }
  const tmp = join(dir, 'enrol-world.json')
  writeFileSync(tmp, JSON.stringify({ apiUrl: w.apiUrl, accounts: fresh }), { mode: 0o600 })
  execFileSync('node', [join(HERE, '..', 'setup', 'enrol.mjs'), tmp], { stdio: ['ignore', 'ignore', 'inherit'] })
  const enrolled = JSON.parse(readFileSync(tmp, 'utf8')).accounts
  const w2 = loadWorld()
  Object.assign(w2.accounts, enrolled)
  writeFileSync(worldFile(), JSON.stringify(w2, null, 2), { mode: 0o600 })

  const id = sql(`SELECT id FROM tenants WHERE slug = '${slug}'`)
  const brandId = sqlT(id, `SELECT id FROM brands WHERE slug = '${brandSlug}'`)
  const t: WdTenant = { tag, slug, brandSlug, id, brandId, licenceId: '', licenceNumber: '' }
  // payments capability (no HTTP-free route exists for a new tenant; the tenant_admin PUT is the supported one)
  const ta = await apiLogin(`${tag}_TENANT_ADMIN`)
  const cap = await http('PUT', '/v1/admin/providers/mock-payments/capability', ta, {
    supported_fiat_currencies: ['EUR'], supported_payment_methods: ['card', 'bank_transfer'], supports_deposit: true, supports_withdrawal: true, priority: 0, status: 'active',
  })
  if (cap.status !== 200) throw new Error(`payments capability: ${cap.status} ${cap.text.slice(0, 200)}`)
  return t
}

/** Re-points the tenant at its own fresh licence in the given status (owner fixture; the player must already be funded). */
export function bindOwnLicence(t: WdTenant, status: 'active' | 'suspended' | 'expired', prefix = 'E2E-WD'): { id: string; number: string } {
  const lic = createLicence(`${prefix}-${t.tag.toUpperCase()}`, status)
  sql(`UPDATE tenants SET licence_id = '${lic.id}' WHERE id = '${t.id}'`)
  t.licenceId = lic.id
  t.licenceNumber = lic.number
  return lic
}

/** Detaches the licence from the tenant row (tenants.licence_id NULL): the tenant then relies only on approved operating bases. */
export function unbindTenantLicence(t: WdTenant) {
  sql(`UPDATE tenants SET licence_id = NULL WHERE id = '${t.id}'`)
  t.licenceId = ''
}

// ---------------------------------------------------------------------------------------------------------------------------
// players

function luhnValid(d: string): boolean {
  let s = 0, alt = false
  for (let i = d.length - 1; i >= 0; i--) { let x = +d[i]; if (alt) { x *= 2; if (x > 9) x -= 9 } s += x; alt = !alt }
  return s % 10 === 0
}
/** A checksum-valid synthetic GB IBAN (unique per call; names no real account). Not a Luhn-valid digit run (a possible card number is refused). */
export function syntheticIban(): string {
  const run = Date.now().toString() + randomBytes(6).toString('hex')
  let digits = ''
  for (const c of run + '0123456789abcdef') digits += String(c.charCodeAt(0) % 10)
  const a = digits.slice(0, 14).split('')
  while (luhnValid(a.join(''))) a[13] = String((+a[13] + 1) % 10)
  const bban = 'WEST' + a.join('')
  let num = ''
  for (const c of bban + 'GB00') num += /[A-Z]/.test(c) ? String(c.charCodeAt(0) - 55) : c
  let rem = 0
  for (const c of num) rem = (rem * 10 + +c) % 97
  return 'GB' + String(98 - rem).padStart(2, '0') + bban
}

export interface WdPlayer { email: string; password: string; token: string; accountId: string; personId: string; instrumentId: string; walletId: string; verificationId: string }

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))

/** Registers, activates, funds (MOCK PSP) and KYC-approves (MOCK KYC + compliance review) a player; registers a payout instrument. */
export async function createFundedPlayer(t: WdTenant, depositMinor = 10000, opts: { kyc?: boolean; instrument?: boolean } = {}): Promise<WdPlayer> {
  const email = `p${Date.now()}${randomBytes(2).toString('hex')}@e2e.invalid`
  const password = 'E2e-Player-' + randomBytes(8).toString('hex') + '!'
  let r = await http('POST', '/v1/auth/register', '', { brand_slug: t.brandSlug, email, password })
  if (r.status !== 201) throw new Error(`register: ${r.status} ${r.text.slice(0, 200)}`)
  let token: string = r.json.access_token
  r = await http('POST', '/v1/me/email-verification/request', token)
  if (r.status !== 200 || !r.json?.token) throw new Error(`email-verification request: ${r.status} (test-support seam off?)`)
  r = await http('POST', '/v1/auth/email-verification/confirm', '', { token: r.json.token })
  if (r.status !== 204) throw new Error(`email-verification confirm: ${r.status}`)
  r = await http('POST', '/v1/auth/login', '', { brand_slug: t.brandSlug, email, password })
  if (r.status !== 200) throw new Error(`player login: ${r.status}`)
  token = r.json.access_token
  const p: WdPlayer = { email, password, token, accountId: '', personId: '', instrumentId: '', walletId: '', verificationId: '' }
  if (depositMinor > 0) {
    r = await http('POST', '/v1/me/deposits', token, { asset_code: 'EUR', amount: depositMinor, payment_method: 'card', idempotency_key: 'dep-' + randomBytes(6).toString('hex') })
    if (r.status !== 201 || r.json?.status !== 'pending') throw new Error(`deposit: ${r.status} ${r.text.slice(0, 200)}`)
    const cb = await http('POST', `/v1/me/deposits/${r.json.id}/simulate-callback`, token)
    if (cb.status !== 200 || cb.json?.status !== 'succeeded') throw new Error(`simulate-callback: ${cb.status} ${cb.text.slice(0, 200)}`)
  }
  const wl = await http('GET', '/v1/me/wallets', token)
  p.walletId = wl.json?.[0]?.wallet_id ?? ''
  p.accountId = sqlT(t.id, `SELECT id FROM player_accounts WHERE email = lower('${email}')`)
  p.personId = sqlT(t.id, `SELECT COALESCE(person_id::text,'') FROM player_accounts WHERE id = '${p.accountId}'`)
  if (opts.kyc !== false) {
    const v = await http('POST', '/v1/me/kyc/verifications', token)
    if (v.status !== 201 && v.status !== 200) throw new Error(`kyc start: ${v.status} ${v.text.slice(0, 200)}`)
    p.verificationId = v.json.id
    for (let i = 0; i < 60; i++) {
      const g = await http('GET', '/v1/me/kyc/verifications', token)
      if (g.json?.find((x: any) => x.id === p.verificationId)?.status !== 'unverified') break
      await sleep(1000)
    }
    const c = await apiLogin(`${t.tag}_COMPLIANCE`)
    const rv = await http('POST', `/v1/admin/kyc/verifications/${p.verificationId}/review`, c, { status: 'approved', reason: 'e2e synthetic player' })
    if (rv.status !== 200 || rv.json?.status !== 'approved') throw new Error(`kyc review: ${rv.status} ${rv.text.slice(0, 200)}`)
  }
  if (opts.instrument !== false) {
    const ins = await http('POST', '/v1/me/payout-instruments', token, { kind: 'bank_account', rail: 'bank_transfer', asset_codes: ['EUR'], detail: { iban: syntheticIban() } })
    if (ins.status !== 201) throw new Error(`payout instrument: ${ins.status} ${ins.text.slice(0, 200)}`)
    p.instrumentId = ins.json.id
  }
  return p
}

export async function playerLogin(t: WdTenant, p: WdPlayer): Promise<string> {
  const r = await http('POST', '/v1/auth/login', '', { brand_slug: t.brandSlug, email: p.email, password: p.password })
  if (r.status !== 200) throw new Error(`player login: ${r.status}`)
  p.token = r.json.access_token
  return p.token
}

export async function requestWithdrawal(p: WdPlayer, amount: number, key?: string) {
  const idem = key ?? 'wd-' + randomBytes(6).toString('hex')
  const r = await http('POST', '/v1/me/withdrawals', p.token, { asset_code: 'EUR', amount, idempotency_key: idem, payout_instrument_id: p.instrumentId })
  return { ...r, key: idem }
}

export async function wallet(p: WdPlayer) {
  const r = await http('GET', '/v1/me/wallets', p.token)
  const w = r.json?.[0]
  return { cash: w.cash_balance as number, available: w.available_balance as number, held: w.held_for_withdrawal as number }
}

/** Two distinct finance staff approve and one submits (T1p): returns the submit response. Moves requested -> pending_review via the queue read. */
export async function approveAndSubmit(t: WdTenant, withdrawalId: string) {
  const f1 = await apiLogin(`${t.tag}_FINANCE1`)
  const f2 = await apiLogin(`${t.tag}_FINANCE2`)
  await http('GET', '/v1/admin/withdrawals?limit=100', f1)
  const a1 = await http('POST', `/v1/admin/withdrawals/${withdrawalId}/approve`, f1, {})
  const a2 = await http('POST', `/v1/admin/withdrawals/${withdrawalId}/approve`, f2, {})
  const sub = await http('POST', `/v1/admin/withdrawals/${withdrawalId}/submit`, f1, { payment_method: 'bank_transfer' })
  return { a1, a2, submit: sub }
}

/** Two distinct finance staff approve a request (moves requested -> pending_review via the queue read first). */
export async function approveWithdrawal(t: WdTenant, withdrawalId: string) {
  const f1 = await apiLogin(`${t.tag}_FINANCE1`)
  const f2 = await apiLogin(`${t.tag}_FINANCE2`)
  await http('GET', '/v1/admin/withdrawals?limit=100', f1)
  const a1 = await http('POST', `/v1/admin/withdrawals/${withdrawalId}/approve`, f1, {})
  const a2 = await http('POST', `/v1/admin/withdrawals/${withdrawalId}/approve`, f2, {})
  return { a1, a2 }
}
export async function submitWithdrawal(t: WdTenant, withdrawalId: string) {
  return http('POST', `/v1/admin/withdrawals/${withdrawalId}/submit`, await apiLogin(`${t.tag}_FINANCE1`), { payment_method: 'bank_transfer' })
}
export const withdrawalState = (t: WdTenant, id: string) => sqlT(t.id, `SELECT state FROM withdrawal_requests WHERE id='${id}'`)

// ---------------------------------------------------------------------------------------------------------------------------
// withdrawal restriction policy: API setup helpers (the behaviour under test is driven through the UI)

export const wrpFile = (tok: string, tenantId: string, cause: string, action: string, reason = 'e2e_setup') =>
  http('POST', '/v1/admin/withdrawal-restriction-policy-changes', tok, { tenant_id: tenantId, cause, action, reason_code: reason })
export const wrpApprove = (tok: string, id: string, reason = 'e2e_setup_ok') =>
  http('POST', `/v1/admin/withdrawal-restriction-policy-changes/${id}/approve`, tok, { reason_code: reason })
export const wrpPolicy = (tok: string, tenantId: string) => http('GET', `/v1/admin/tenants/${tenantId}/withdrawal-restriction-policy`, tok)

export interface WrpRow { setVersion: number; cause: string; action: string; changeId: string; createdBy: string }
export function wrpVersions(tenantId: string): WrpRow[] {
  const out = sqlT(tenantId, `SELECT set_version||'|'||cause||'|'||action||'|'||COALESCE(change_id::text,'')||'|'||COALESCE(created_by::text,'') FROM withdrawal_restriction_policies WHERE tenant_id='${tenantId}' ORDER BY set_version`)
  return out ? out.split('\n').map((l) => { const [v, c, a, ch, by] = l.split('|'); return { setVersion: Number(v), cause: c, action: a, changeId: ch, createdBy: by } }) : []
}
export const wrpChangeStatus = (_tenantId: string, id: string) => sql(`SELECT status FROM withdrawal_restriction_policy_changes WHERE id='${id}'`)

/** audit_log rows for the tenant: [action, outcome, metadata-json] newest first. */
export function auditRows(tenantId: string, actionLike: string, limit = 5): { action: string; outcome: string; md: any }[] {
  const out = sqlT(tenantId, `SELECT action||'|'||outcome||'|'||metadata::text FROM audit_log WHERE tenant_id='${tenantId}' AND action LIKE '${actionLike}' ORDER BY created_at DESC, id DESC LIMIT ${limit}`)
  return out ? out.split('\n').map((l) => { const i = l.indexOf('|'), j = l.indexOf('|', i + 1); return { action: l.slice(0, i), outcome: l.slice(i + 1, j), md: JSON.parse(l.slice(j + 1)) } }) : []
}
