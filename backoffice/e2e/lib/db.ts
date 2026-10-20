// Owner-role access to the PRIVATE e2e database through psql, used ONLY to create synthetic licence fixtures (the
// governed-write guard admits the table owner, the documented fixture path) and to read back ground truth. The URL is split
// into PG* variables so no credential appears in a process argument list.
import { execFileSync } from 'node:child_process'
import { randomBytes } from 'node:crypto'

function env(): NodeJS.ProcessEnv {
  const raw = process.env.E2E_OWNER_DATABASE_URL
  if (!raw) throw new Error('E2E_OWNER_DATABASE_URL is not set')
  const u = new URL(raw)
  return {
    PATH: process.env.PATH,
    PGHOST: u.hostname,
    PGPORT: u.port || '5432',
    PGUSER: decodeURIComponent(u.username),
    PGPASSWORD: decodeURIComponent(u.password),
    PGDATABASE: u.pathname.slice(1),
    PGSSLMODE: u.searchParams.get('sslmode') ?? 'prefer',
  }
}
/** Runs q as the table owner inside a transaction that carries the platform-admin session setting (what the RLS policies key on). */
export function sql(q: string): string {
  const script = `BEGIN;
SELECT set_config('app.platform_admin_principal_id', (SELECT id::text FROM staff_users WHERE email = 'pa_a@e2e.invalid'), true) AS x \\gset
${q};
COMMIT;
`
  return execFileSync('psql', ['-X', '-v', 'ON_ERROR_STOP=1', '-Atq'], { env: env(), input: script }).toString().trim().split('\n').filter((l) => l !== 'COMMIT').join('\n')
}
const ident = /^[A-Za-z0-9_.-]+$/
export const uniq = (p: string) => `${p}-${randomBytes(4).toString('hex')}`

/** A fresh synthetic licence in the synthetic jurisdiction. Unique number per call: specs stay independent and repeatable. */
export function createLicence(prefix: string, status: 'active' | 'suspended' | 'expired'): { id: string; number: string } {
  const number = uniq(prefix)
  if (!ident.test(number) || !['active', 'suspended', 'expired'].includes(status)) throw new Error('bad fixture input')
  const id = sql(
    `INSERT INTO licences (jurisdiction_id, licensee, licence_number, status)
     SELECT id, 'platform', '${number}', '${status}' FROM jurisdictions WHERE code = 'STG-SYNTHETIC' RETURNING id`,
  ).split('\n')[0]
  return { id, number }
}
export const licenceStatus = (id: string) => sql(`SELECT status FROM licences WHERE id = '${id}'`)
export const changeStatus = (id: string) => sql(`SELECT status FROM licensing_change_requests WHERE id = '${id}'`)
