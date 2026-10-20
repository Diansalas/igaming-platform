// Seeds the PRIVATE e2e database using the platform's own sanctioned tools only (no role or credential changes):
//   cmd/seed-admin           first platform admin; further platform admins via -break-glass -force-additional (synthetic DB only)
//   cmd/seed-staging-world   synthetic ACTIVE tenant + brand + synthetic jurisdiction/licence + tenant staff
// plus the write-once database_environment anchor ('test') and the actor-proof verification key, both as the table owner.
// Usage: node seed.mjs <binDir> <worldFile>   env: E2E_OWNER_DATABASE_URL, secrets env (see run.sh)
import { execFileSync } from 'node:child_process'
import { randomBytes } from 'node:crypto'
import { readFileSync, writeFileSync, mkdtempSync } from 'node:fs'
import { join } from 'node:path'

const [bin, worldFile] = process.argv.slice(2)
const owner = process.env.E2E_OWNER_DATABASE_URL
const pgEnv = (() => {
  const u = new URL(owner)
  return { PATH: process.env.PATH, PGHOST: u.hostname, PGPORT: u.port || '5432', PGUSER: decodeURIComponent(u.username), PGPASSWORD: decodeURIComponent(u.password), PGDATABASE: u.pathname.slice(1), PGSSLMODE: u.searchParams.get('sslmode') ?? 'prefer' }
})()
const psql = (q) => execFileSync('psql', ['-X', '-v', 'ON_ERROR_STOP=1', '-Atq', '-c', q], { env: pgEnv }).toString().trim()
const toolEnv = { ...process.env, APP_ENV: 'development', DATABASE_URL: owner }
const pw = () => 'E2e-' + randomBytes(12).toString('hex')
const OP = ['-operator', 'e2e-harness', '-reason', 'e2e_synthetic_fixture']

if (psql('SELECT count(*) FROM staff_users') !== '0') throw new Error('refused: the database already has staff; the e2e harness needs a freshly migrated private database')
psql(`INSERT INTO database_environment (environment) VALUES ('test') ON CONFLICT DO NOTHING`)
if (!/^[0-9a-f]{64}$/.test(process.env.E2E_ACTOR_PROOF_HEX ?? '')) throw new Error('E2E_ACTOR_PROOF_HEX missing')
psql(`INSERT INTO actorproof_private.actor_proof_keys (kid, secret) VALUES ('${process.env.ACTOR_PROOF_ACTIVE_KID}', decode('${process.env.E2E_ACTOR_PROOF_HEX}', 'hex')) ON CONFLICT (kid) DO NOTHING`)

const accounts = {}
function seedAdmin(key, args) {
  const password = pw()
  const email = `${key.toLowerCase()}@e2e.invalid`
  const out = execFileSync(join(bin, 'seed-admin'), ['-email', email, ...args, ...OP], { env: { ...toolEnv, SEED_ADMIN_PASSWORD: password } }).toString()
  const person = /linked to person ([0-9a-f-]{36})/.exec(out)?.[1]
  const lines = out.trim().split('\n')
  const token = lines[lines.length - 1].trim()
  if (!person || !/^\S{20,}$/.test(token)) throw new Error(`seed-admin ${key}: unexpected output`)
  accounts[key] = { key, email, password, tenantSlug: '', role: 'platform_admin', bootstrapToken: token, lastStep: 0, person }
  return person
}
// A and A2 share Person P1; B and B2 share P2 (server-side same-Person refusals); C, D have their own.
const p1 = seedAdmin('PA_A', ['-create-person'])
const brk = ['-break-glass', '-force-additional']
seedAdmin('PA_A2', [...brk, '-person-id', p1])
const p2 = seedAdmin('PA_B', [...brk, '-create-person'])
seedAdmin('PA_B2', [...brk, '-person-id', p2])
seedAdmin('PA_C', [...brk, '-create-person'])
seedAdmin('PA_D', [...brk, '-create-person'])

const dir = mkdtempSync(join(process.env.E2E_RUN_DIR, 'creds-'))
const planF = join(dir, 'plan.json'), applyF = join(dir, 'apply.json')
const slug = 'e2e-tenant'
const swArgs = (phase) => ['-phase', phase, '-tenant-slug', slug, '-brand-slug', 'e2e-brand', '-country', 'MT', '-authorization-reference', 'E2E-SYNTHETIC-NOT-A-REAL-AUTHORISATION', ...OP, '-actor-id', psql(`SELECT id FROM staff_users WHERE email = 'pa_a@e2e.invalid'`)]
execFileSync(join(bin, 'seed-staging-world'), [...swArgs('plan'), '-credentials-out', planF], { env: toolEnv, stdio: ['ignore', 'ignore', 'inherit'] })
execFileSync(join(bin, 'seed-staging-world'), [...swArgs('apply'), '-credentials-in', planF, '-credentials-out', applyF], { env: toolEnv, stdio: ['ignore', 'ignore', 'inherit'] })
const creds = JSON.parse(readFileSync(applyF, 'utf8'))
for (const [k, c] of Object.entries(creds.actors)) {
  accounts['T_' + k] = { key: 'T_' + k, email: c.email, password: c.password, tenantSlug: slug, role: k.toLowerCase(), bootstrapToken: c.bootstrap_token, lastStep: 0 }
}
writeFileSync(worldFile, JSON.stringify({
  baseUrl: process.env.E2E_BASE_URL, apiUrl: process.env.E2E_API_URL, tenantSlug: slug,
  jurisdictionCode: 'STG-SYNTHETIC', syntheticLicenceNumber: 'STG-SYNTHETIC-1', accounts,
}, null, 2), { mode: 0o600 })
console.log(`seeded ${Object.keys(accounts).length} accounts`)
