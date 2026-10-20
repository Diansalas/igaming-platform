// lic21: fixtures for the KYC-policy and residence specs. Everything here uses SANCTIONED mechanisms only:
//   * platform admins: cmd/seed-admin -break-glass -force-additional (the harness's existing mechanism), linked to a Person;
//   * an UNLINKED platform admin and extra tenant staff: the documented owner fixture path of the private test database (the
//     same one the Go tests use, internal/testsupport/staffsv.InsertLegacyUnlinkedStaff: an owner INSERT with the SF014 person
//     trigger disabled for that one statement, in a transaction), copying the password hash of an existing seeded account.
//     No role, password or credential of the database is touched; the accounts live in this throwaway database only;
//   * second factors: the real HTTP enrolment flow with a bootstrap token issued by cmd/seed-admin -issue-bootstrap;
//   * players, KYC verifications and deposits: the real player and staff HTTP API (mock KYC vendor, mock PSP).
import { execFileSync } from 'node:child_process'
import { createHmac, randomBytes, randomUUID } from 'node:crypto'
import { readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { apiLogin } from './api'
import { lit, ownerRead, ownerSql } from './kyc_db'
import { loadWorld, type Account } from './world'

const worldFile = () => process.env.E2E_WORLD_FILE as string
type WorldRaw = ReturnType<typeof loadWorld> & { accounts: Record<string, Account & { person?: string; bootstrapToken?: string }> }
const readWorld = (): WorldRaw => JSON.parse(readFileSync(worldFile(), 'utf8'))
const writeWorld = (w: WorldRaw) => writeFileSync(worldFile(), JSON.stringify(w, null, 2), { mode: 0o600 })

const OP = ['-operator', 'e2e-harness', '-reason', 'e2e_synthetic_fixture']
const toolEnv = () => ({ ...process.env, APP_ENV: 'development', DATABASE_URL: process.env.E2E_OWNER_DATABASE_URL })
const bin = (n: string) => join(process.env.E2E_RUN_DIR as string, 'bin', n)

export const hasAccount = (key: string) => !!readWorld().accounts[key]
export const staffId = (key: string): string => {
  const a = loadWorld().accounts[key]
  const t = tenantIdOf()
  const q = `SELECT id FROM staff_users WHERE lower(email) = lower(${lit(a.email)})`
  return a.tenantSlug ? ownerRead({ tenant: t }, q) : ownerRead({ platformAdmin: platformAnchor() }, q)
}
export const staffPerson = (key: string): string => {
  const a = loadWorld().accounts[key]
  const q = `SELECT coalesce(person_id::text, '') FROM staff_users WHERE lower(email) = lower(${lit(a.email)})`
  return a.tenantSlug ? ownerRead({ tenant: tenantIdOf() }, q) : ownerRead({ platformAdmin: platformAnchor() }, q)
}
export const tenantIdOf = (): string => ownerRead({}, `SELECT id FROM tenants WHERE slug = ${lit(loadWorld().tenantSlug)}`)
/** PA_A is the first-admin bootstrap and the session setting the platform-side RLS policies key on in fixtures. */
export const platformAnchor = (): string => ownerRead({}, `SELECT id FROM staff_users WHERE email = 'pa_a@e2e.invalid'`)

// ---- HTTP ------------------------------------------------------------------------------------------------------------------

export interface Res { status: number; json: any }
export async function kapi(method: string, path: string, body?: unknown, token?: string): Promise<Res> {
  const r = await fetch(loadWorld().apiUrl + path, {
    method,
    headers: { 'content-type': 'application/json', ...(token ? { authorization: `Bearer ${token}` } : {}) },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  const text = await r.text()
  let json: any = null
  try { json = JSON.parse(text) } catch { /* empty body */ }
  return { status: r.status, json }
}

// ---- second-factor enrolment (real HTTP flow) --------------------------------------------------------------------------------

const ALPHA = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567'
function b32(s: string): Buffer {
  let bits = 0, v = 0
  const o: number[] = []
  for (const c of s.replace(/=+$/, '')) { v = (v << 5) | ALPHA.indexOf(c); bits += 5; if (bits >= 8) { o.push((v >>> (bits - 8)) & 255); bits -= 8 } }
  return Buffer.from(o)
}
function totp(secret: string, step: number): string {
  const m = Buffer.alloc(8); m.writeBigUInt64BE(BigInt(step))
  const h = createHmac('sha1', b32(secret)).update(m).digest(); const o = h[h.length - 1] & 15
  return String((((h[o] & 127) << 24) | (h[o + 1] << 16) | (h[o + 2] << 8) | h[o + 3]) % 1e6).padStart(6, '0')
}
async function enrol(key: string, token: string) {
  const w = readWorld(); const a = w.accounts[key]
  const redeem: Record<string, string> = { token, email: a.email, password: a.password }
  if (a.tenantSlug) redeem.tenant_slug = a.tenantSlug
  const red = await kapi('POST', '/v1/staff/auth/enrol/redeem', redeem)
  if (red.status !== 200) throw new Error(`enrol redeem ${key}: ${red.status}`)
  const ch = red.json.challenge.token
  const start = await kapi('POST', '/v1/staff/auth/enrol/start', { challenge_token: ch })
  if (start.status !== 200) throw new Error(`enrol start ${key}: ${start.status}`)
  const step = Math.floor(Date.now() / 30000)
  const conf = await kapi('POST', '/v1/staff/auth/enrol/confirm', { challenge_token: ch, factor_id: start.json.factor_id, code: totp(start.json.secret_base32, step) })
  if (conf.status !== 200) throw new Error(`enrol confirm ${key}: ${conf.status}`)
  a.totpSecret = start.json.secret_base32
  a.lastStep = step
  delete a.bootstrapToken
  writeWorld(w)
}
function issueBootstrap(email: string, tenantSlug: string): string {
  const out = execFileSync(bin('seed-admin'), ['-issue-bootstrap', '-email', email, ...(tenantSlug ? ['-tenant-slug', tenantSlug] : []), ...OP], { env: toolEnv() }).toString().trim().split('\n')
  const token = out[out.length - 1].trim()
  if (!/^\S{20,}$/.test(token)) throw new Error('seed-admin -issue-bootstrap: unexpected output')
  return token
}
const newPassword = () => 'E2e-' + randomBytes(12).toString('hex')

function register(key: string, email: string, password: string, tenantSlug: string, role: string, person?: string) {
  const w = readWorld()
  w.accounts[key] = { key, email, password, tenantSlug, role, lastStep: 0, ...(person ? { person } : {}) } as any
  writeWorld(w)
}

// ---- accounts ------------------------------------------------------------------------------------------------------------------

/** A new platform admin via the harness's sanctioned break-glass seeding, linked to its own Person or to the Person of `samePersonAs`. */
export async function ensurePlatformAdmin(key: string, o: { samePersonAs?: string } = {}) {
  if (hasAccount(key)) return
  const email = `${key.toLowerCase()}@e2e.invalid`
  const password = newPassword()
  const person = o.samePersonAs ? staffPerson(o.samePersonAs) : ''
  const out = execFileSync(bin('seed-admin'), ['-email', email, '-break-glass', '-force-additional', ...(person ? ['-person-id', person] : ['-create-person']), ...OP], { env: { ...toolEnv(), SEED_ADMIN_PASSWORD: password } }).toString()
  const token = out.trim().split('\n').pop()!.trim()
  register(key, email, password, '', 'platform_admin', /linked to person ([0-9a-f-]{36})/.exec(out)?.[1])
  await enrol(key, token)
}

/**
 * LEGACY unlinked platform admin (no Person): the sanctioned test fixture is the owner INSERT with the SF014 trigger disabled for
 * that statement (migration 0139 refuses to create such an account any other way). Same password as `like`.
 */
export async function ensureUnlinkedPlatformAdmin(key: string, like = 'PA_A') {
  if (hasAccount(key)) return
  const email = `${key.toLowerCase()}@e2e.invalid`
  const src = loadWorld().accounts[like]
  ownerSql({ platformAdmin: platformAnchor() }, `
    ALTER TABLE staff_users DISABLE TRIGGER sf_14_staff_person_required;
    INSERT INTO staff_users (tenant_id, email, password_hash, role, status)
      SELECT NULL, ${lit(email)}, password_hash, 'platform_admin', 'active' FROM staff_users WHERE email = ${lit(src.email)};
    ALTER TABLE staff_users ENABLE TRIGGER sf_14_staff_person_required`)
  register(key, email, src.password, '', 'platform_admin')
  await enrol(key, issueBootstrap(email, ''))
}

/** Extra tenant staff of the synthetic tenant (owner fixture insert; password and hash copied from `like`). person: new | same as another account | a given uuid. */
export async function ensureTenantStaff(key: string, o: { role: string; person: 'new' | { sameAs: string } | { uuid: string }; like?: string }) {
  if (hasAccount(key)) return
  const w = loadWorld()
  const like = w.accounts[o.like ?? 'T_COMPLIANCE']
  const t = tenantIdOf()
  const person = o.person === 'new' ? randomUUID() : 'uuid' in o.person ? o.person.uuid : staffPerson(o.person.sameAs)
  const email = `${key.toLowerCase()}@${w.tenantSlug}.acceptance.invalid`
  ownerSql({ tenant: t }, `
    INSERT INTO persons (id) SELECT ${lit(person)}::uuid WHERE NOT EXISTS (SELECT 1 FROM persons WHERE id = ${lit(person)}::uuid);
    INSERT INTO staff_users (tenant_id, email, password_hash, role, status, person_id)
      SELECT tenant_id, ${lit(email)}, password_hash, ${lit(o.role)}, 'active', ${lit(person)}::uuid FROM staff_users WHERE email = ${lit(like.email)}`)
  register(key, email, like.password, w.tenantSlug, o.role, person)
  await enrol(key, issueBootstrap(email, w.tenantSlug))
}

// ---- players, KYC, deposits (real API) ----------------------------------------------------------------------------------

export interface Player { email: string; password: string; token: string; id: string }
const BRAND = 'e2e-brand'

/** Registers, e-mail-verifies (test-support seam) and logs in a synthetic player of the synthetic brand. */
export async function newPlayer(tag: string): Promise<Player> {
  const email = `${tag}-${randomBytes(4).toString('hex')}@e2e.invalid`
  const password = 'E2e-Player-' + randomBytes(10).toString('hex')
  const reg = await kapi('POST', '/v1/auth/register', { brand_slug: BRAND, email, password })
  if (reg.status !== 201) throw new Error(`register: ${reg.status}`)
  const l1 = await kapi('POST', '/v1/auth/login', { brand_slug: BRAND, email, password })
  if (l1.status !== 200) throw new Error(`player login: ${l1.status}`)
  const ev = await kapi('POST', '/v1/me/email-verification/request', undefined, l1.json.access_token)
  if (ev.status !== 200 || !ev.json?.token) throw new Error(`email verification seam: ${ev.status} (TEST_SUPPORT_ENDPOINTS_ENABLED?)`)
  const cf = await kapi('POST', '/v1/auth/email-verification/confirm', { token: ev.json.token })
  if (cf.status !== 204) throw new Error(`email confirm: ${cf.status}`)
  const l2 = await kapi('POST', '/v1/auth/login', { brand_slug: BRAND, email, password })
  const me = await kapi('GET', '/v1/me', undefined, l2.json.access_token)
  if (me.status !== 200 || me.json.status !== 'active') throw new Error(`player not active: ${me.status} ${me.json?.status}`)
  return { email, password, token: l2.json.access_token, id: me.json.id }
}

/** Tenant admin configures the mock PSP capability for EUR/card (the same call the staging acceptance suite makes). Idempotent. */
export async function ensureMockPayments() {
  const t = await apiLogin('T_TENANT_ADMIN')
  const r = await kapi('PUT', '/v1/admin/providers/mock-payments/capability', { supported_fiat_currencies: ['EUR'], supported_payment_methods: ['card'], supports_deposit: true, supports_withdrawal: true, priority: 0, status: 'active' }, t)
  if (r.status !== 200) throw new Error(`mock-payments capability: ${r.status}`)
}

/** Compliance switches verified-residence evidence collection on for the tenant (precondition of a residence determination). */
export async function ensureResidenceCollection(complianceToken: string) {
  const r = await kapi('PUT', '/v1/admin/jurisdiction-evidence-collection/verified_residence', { active: true, reason_code: 'e2e_setup' }, complianceToken)
  if (r.status !== 200 || r.json?.active !== true) throw new Error(`evidence collection: ${r.status}`)
}

export const deposit = (p: Player, amount: number, key: string) =>
  kapi('POST', '/v1/me/deposits', { asset_code: 'EUR', amount, payment_method: 'card', idempotency_key: key }, p.token)

/** Starts a KYC verification and waits until the outbox worker has submitted it to the MOCK vendor (status pending). */
export async function startVerification(p: Player): Promise<string> {
  const v = await kapi('POST', '/v1/me/kyc/verifications', undefined, p.token)
  if (v.status !== 201 && v.status !== 200) throw new Error(`start verification: ${v.status}`)
  const id = v.json.id as string
  for (let i = 0; i < 40; i++) {
    const g = await kapi('GET', '/v1/me/kyc/verifications', undefined, p.token)
    const list = Array.isArray(g.json) ? g.json : g.json?.items ?? []
    const row = list.find((x: any) => x.id === id)
    if (row && row.status !== 'unverified') return id
    await new Promise((r) => setTimeout(r, 2000))
  }
  throw new Error('verification never reached the mock vendor (KYC outbox worker?)')
}

export const reviewVerification = (token: string, id: string, status: 'approved' | 'rejected', country?: string) =>
  kapi('POST', `/v1/admin/kyc/verifications/${id}/review`, { status, reason: 'e2e synthetic', ...(country ? { verified_residence_country: country } : {}) }, token)

/** The real Go market-evidence read (kyc.ReadMarketResidenceEvidence) through the test-only probe, with the runtime role. */
export function marketEvidence(tenant: string, player: string): { ok: boolean; country: string } {
  const out = execFileSync(bin('e2e-dbprobe'), ['market-evidence', tenant, player], { env: { PATH: process.env.PATH, E2E_RUNTIME_DATABASE_URL: process.env.E2E_RUNTIME_DATABASE_URL } }).toString().trim()
  const m = /^EVIDENCE\|(true|false)(?:\|(.*))?$/.exec(out)
  if (!m) throw new Error(`unexpected probe output: ${out}`)
  return { ok: m[1] === 'true', country: m[2] ?? '' }
}
