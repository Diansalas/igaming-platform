// lic21: database access for the KYC-policy and residence specs.
//  * ownerSql   : the table OWNER of the private e2e database (ground truth reads, sanctioned fixture inserts), with the tenant or
//                 platform-admin session setting the RLS policies key on.
//  * runtimeTx  : the RUNTIME role (igaming_runtime, the role platform-api itself uses) for the BELOW-THE-APPLICATION-LAYER bypass
//                 attempts. Every attempt runs inside a transaction that is ROLLED BACK, with SQLSTATE verbosity, and returns the
//                 SQLSTATE (or the output). Credentials are split into PG* variables, never placed in an argument list.
import { execFileSync, spawnSync } from 'node:child_process'
import { randomUUID } from 'node:crypto'

function pgEnv(raw: string | undefined, what: string): NodeJS.ProcessEnv {
  if (!raw) throw new Error(`${what} is not set`)
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
const ownerEnv = () => pgEnv(process.env.E2E_OWNER_DATABASE_URL, 'E2E_OWNER_DATABASE_URL')
const runtimeEnv = () => pgEnv(process.env.E2E_RUNTIME_DATABASE_URL, 'E2E_RUNTIME_DATABASE_URL')

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/
export const isUuid = (s: string) => UUID.test(s)
export const lit = (s: string) => `'${s.replace(/'/g, "''")}'`

export type Session = { tenant?: string; platformAdmin?: string }
function sessionSql(s: Session): string {
  const out: string[] = []
  if (s.tenant) {
    if (!isUuid(s.tenant)) throw new Error('bad tenant id')
    out.push(`SELECT set_config('app.tenant_id', '${s.tenant}', true) AS x1 \\gset`)
  }
  if (s.platformAdmin) {
    if (!isUuid(s.platformAdmin)) throw new Error('bad platform admin id')
    out.push(`SELECT set_config('app.platform_admin_principal_id', '${s.platformAdmin}', true) AS x2 \\gset`)
  }
  return out.join('\n')
}

/** Owner role, one transaction (committed). Returns trimmed stdout (-At). Throws on error. */
export function ownerSql(session: Session, q: string): string {
  const script = `BEGIN;\n${sessionSql(session)}\n${q};\nCOMMIT;\n`
  return execFileSync('psql', ['-X', '-v', 'ON_ERROR_STOP=1', '-Atq'], { env: ownerEnv(), input: script })
    .toString()
    .trim()
    .split('\n')
    .filter((l) => l !== 'COMMIT')
    .join('\n')
}

/** Owner role, read only. Rolled back. */
export function ownerRead(session: Session, q: string): string {
  const script = `BEGIN;\n${sessionSql(session)}\n${q};\nROLLBACK;\n`
  return execFileSync('psql', ['-X', '-v', 'ON_ERROR_STOP=1', '-Atq'], { env: ownerEnv(), input: script })
    .toString()
    .trim()
    .split('\n')
    .filter((l) => l !== 'ROLLBACK')
    .join('\n')
}

export interface AttemptResult {
  /** SQLSTATE of the failing statement ('' when everything succeeded). */
  sqlstate: string
  /** Server message (only filled by runtimeTxMessage; runtimeTx reports the SQLSTATE alone). */
  message: string
  /** stdout of the statements that succeeded (-At). */
  out: string
}

/**
 * Runs `body` as the RUNTIME role in ONE transaction that is always rolled back. `settings` become transaction-local GUCs
 * (exactly what a holder of the runtime credential can do: claim a session shape).
 */
export function runtimeTx(settings: Record<string, string>, body: string): AttemptResult {
  const sets = Object.entries(settings)
    .map(([k, v], i) => `SELECT set_config(${lit(k)}, ${lit(v)}, true) AS s${i} \\gset`)
    .join('\n')
  const script = `\\set VERBOSITY sqlstate\nBEGIN;\n${sets}\n${body};\nROLLBACK;\n`
  const r = spawnSync('psql', ['-X', '-v', 'ON_ERROR_STOP=1', '-Atq'], { env: runtimeEnv(), input: script })
  const err = r.stderr.toString()
  const out = r.stdout.toString().trim()
  const m = /ERROR:\s+([0-9A-Z]{5})(?::\s*(.*))?/.exec(err)
  if (r.status !== 0 && !m) throw new Error(`runtime attempt failed without a SQLSTATE: ${err.slice(0, 300)}`)
  return { sqlstate: m?.[1] ?? '', message: '', out }
}

/** Plain statement (no extra transaction wrapper beyond runtimeTx) with the server message; for refusals without a custom SQLSTATE. */
export function runtimeTxMessage(settings: Record<string, string>, body: string): AttemptResult {
  const sets = Object.entries(settings)
    .map(([k, v], i) => `SELECT set_config(${lit(k)}, ${lit(v)}, true) AS s${i} \\gset`)
    .join('\n')
  const script = `\\set VERBOSITY verbose\nBEGIN;\n${sets}\n${body};\nROLLBACK;\n`
  const r = spawnSync('psql', ['-X', '-v', 'ON_ERROR_STOP=1', '-Atq'], { env: runtimeEnv(), input: script })
  const err = r.stderr.toString()
  const m = /ERROR:\s+([0-9A-Z]{5}):\s*(.*)/.exec(err)
  return { sqlstate: m?.[1] ?? '', message: m?.[2] ?? '', out: r.stdout.toString().trim() }
}

export const newUuid = () => randomUUID()
