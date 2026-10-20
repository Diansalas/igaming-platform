// lic22: database ground truth for the withdrawal-restriction (09) and AML-screening (10) specs.
// Two access paths, both against the PRIVATE e2e database only:
//   * sqlT: the TABLE OWNER with a tenant scope (app.tenant_id) and the platform-admin principal, for READING ground truth (and the
//     documented owner fixture writes of lib/db.ts). Tenant-owned tables are FORCE RLS, so a tenant scope is needed to see rows.
//   * runtimeSql: the igaming_runtime role (E2E_RUNTIME_DATABASE_URL), exactly the role the API runs as, for the
//     "below the application layer" attempts. It never sets a session principal unless the caller does so in the SQL.
// The URLs are split into PG* variables so no credential appears in a process argument list.
import { spawnSync, execFileSync } from 'node:child_process'

function pgEnv(raw: string | undefined, name: string): NodeJS.ProcessEnv {
  if (!raw) throw new Error(`${name} is not set`)
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

/** Owner-role query inside one transaction scoped to ONE tenant (app.tenant_id only: tenant-owned tables are FORCE RLS and a tenant
 * plus a platform principal together is a mis-scoped session that sees nothing). Platform-scoped tables: use sql() of lib/db.ts. */
export function sqlT(tenantId: string, q: string): string {
  const script = `BEGIN;
SELECT set_config('app.tenant_id', '${tenantId}', true) AS y \\gset
${q};
COMMIT;
`
  return execFileSync('psql', ['-X', '-v', 'ON_ERROR_STOP=1', '-Atq'], { env: pgEnv(process.env.E2E_OWNER_DATABASE_URL, 'E2E_OWNER_DATABASE_URL'), input: script })
    .toString().trim().split('\n').filter((l) => l !== 'COMMIT').join('\n')
}

export interface RuntimeResult { ok: boolean; out: string; err: string; sqlstate: string }

/** Runs SQL as the RUNTIME role. Never throws on a database error: returns the SQLSTATE and message so a spec can assert the refusal. */
export function runtimeSql(script: string): RuntimeResult {
  const r = spawnSync('psql', ['-X', '-v', 'ON_ERROR_STOP=1', '-v', 'VERBOSITY=verbose', '-Atq'], {
    env: pgEnv(process.env.E2E_RUNTIME_DATABASE_URL, 'E2E_RUNTIME_DATABASE_URL'),
    input: script,
  })
  const err = r.stderr.toString()
  const m = /ERROR:\s+([0-9A-Z]{5}):/.exec(err)
  return { ok: r.status === 0, out: r.stdout.toString().trim(), err: err.trim(), sqlstate: m ? m[1] : '' }
}

export const n = (s: string): number => Number(s.split('\n')[0] || '0')

/** Sums of debits and credits over ALL ledger entries of the tenant, per asset (strings: NUMERIC(38,0)). */
export function ledgerTotals(tenantId: string): { debits: string; credits: string; entries: number } {
  const [d, c, e] = sqlT(tenantId, `SELECT COALESCE(SUM(amount) FILTER (WHERE direction='debit'),0)||'|'||COALESCE(SUM(amount) FILTER (WHERE direction='credit'),0)||'|'||count(*) FROM ledger_entries WHERE tenant_id='${tenantId}'`).split('|')
  return { debits: d, credits: c, entries: Number(e) }
}

/** Number of accounts whose balance projection differs from the sum of its ledger entries (must be 0: balances are projections). */
export function projectionDrift(tenantId: string): number {
  return n(sqlT(tenantId, `SELECT count(*) FROM wallet_balance_projection p, LATERAL (
    SELECT COALESCE(SUM(amount) FILTER (WHERE direction='debit'),0) AS d, COALESCE(SUM(amount) FILTER (WHERE direction='credit'),0) AS c
      FROM ledger_entries e WHERE e.ledger_account_id = p.ledger_account_id) s
    WHERE p.tenant_id='${tenantId}' AND (p.debit_total <> s.d OR p.credit_total <> s.c)`))
}

/** Row counts that a REFUSED withdrawal must leave untouched. */
export function moneyCounts(tenantId: string): { requests: number; ledgerTx: number; entries: number } {
  const [r, t, e] = sqlT(tenantId, `SELECT (SELECT count(*) FROM withdrawal_requests)||'|'||(SELECT count(*) FROM ledger_transactions)||'|'||(SELECT count(*) FROM ledger_entries)`).split('|')
  return { requests: Number(r), ledgerTx: Number(t), entries: Number(e) }
}
