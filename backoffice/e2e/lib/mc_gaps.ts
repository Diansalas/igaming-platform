// c3 (QA gaps): helpers of spec 13. Additive only: nothing here changes lib/mc_world.ts, lib/mc_ui.ts or any earlier spec.
// Ground truth is read from the PRIVATE database as the table owner (lib/db.ts sql(), the same path the specs 11/12 use) and the
// market RESOLVER is the real Go code (tools/mcprobe, runtime role). The only owner-role WRITE in this file is `shiftChange`, which
// moves a change request / its approvals into the past in the private test database (see its comment); it never touches the runtime.
import { execFileSync } from 'node:child_process'
import { join } from 'node:path'
import { expect, type Locator } from 'playwright/test'
import { sql } from './db'
import { clickWithStepUp, spaGoto, type Session } from './session'
import { sqlT } from './wd_db'

/** The REAL resolver with an optional brand and product (tools/mcprobe, additive args). */
export function mcResolveScoped(tenant: string, country: string, operation: string, o: { brand?: string | null; product?: string | null } = {}): string {
  const bin = join(process.env.E2E_RUN_DIR!, 'bin', 'e2e-mcprobe')
  const out = execFileSync(bin, ['resolve', tenant, country, operation, o.brand ?? '-', o.product ?? '-'], { env: { PATH: process.env.PATH, E2E_RUNTIME_DATABASE_URL: process.env.E2E_RUNTIME_DATABASE_URL! } }).toString().trim()
  const m = /^RESOLVE\|(\S+)$/.exec(out)
  if (!m) throw new Error(`mcprobe: unexpected output ${out}`)
  return m[1]
}

/**
 * Clock control for the 24 h freshness and 72 h expiry paths, PRIVATE TEST DATABASE ONLY, as the table owner. The change tables are
 * append-only (guard triggers) with FORCEd row level security and no UPDATE policy on approvals, so for ONE transaction the table's own
 * guard trigger and (for approvals) row level security are switched off, the timestamps are moved back, and both are switched on again
 * before COMMIT (transactional DDL: a failure restores them). No production code is touched; the runtime role cannot do any of this.
 * `reqBy` moves created_at and expires_at of the request, `apprBy` moves decided_at of its approvals (Postgres intervals).
 */
export function shiftChange(id: string, reqBy: string | null, apprBy: string | null): void {
  const iv = /^[0-9]+ (hours?|minutes?)$/
  if (!/^[0-9a-f-]{36}$/.test(id) || !iv.test(reqBy ?? '1 hour') || !iv.test(apprBy ?? '1 hour')) throw new Error('bad shift input')
  const parts: string[] = []
  if (reqBy) {
    parts.push(
      `ALTER TABLE licensing_change_requests DISABLE TRIGGER licensing_change_requests_guard`,
      `UPDATE licensing_change_requests SET created_at = created_at - interval '${reqBy}', expires_at = expires_at - interval '${reqBy}' WHERE id = '${id}'`,
      `ALTER TABLE licensing_change_requests ENABLE TRIGGER licensing_change_requests_guard`,
    )
  }
  if (apprBy) {
    parts.push(
      `ALTER TABLE licensing_change_approvals DISABLE TRIGGER licensing_change_approvals_guard`,
      `ALTER TABLE licensing_change_approvals DISABLE ROW LEVEL SECURITY`,
      `UPDATE licensing_change_approvals SET decided_at = decided_at - interval '${apprBy}' WHERE request_id = '${id}'`,
      `ALTER TABLE licensing_change_approvals ENABLE ROW LEVEL SECURITY`,
      `ALTER TABLE licensing_change_approvals ENABLE TRIGGER licensing_change_approvals_guard`,
    )
  }
  sql(parts.join(';\n'))
  // the guards are back on, and RLS is enabled AND forced, on both tables
  expect(sql(`SELECT count(*) FROM pg_trigger WHERE tgname IN ('licensing_change_requests_guard','licensing_change_approvals_guard') AND tgenabled = 'O'`)).toBe('2')
  expect(sql(`SELECT count(*) FROM pg_class WHERE relname IN ('licensing_change_requests','licensing_change_approvals') AND relrowsecurity AND relforcerowsecurity`)).toBe('2')
}

/** Per-action count of licensing_change.* audit rows of one request (success and denied). */
export function auditCounts(id: string): Record<string, number> {
  const rows = sql(`SELECT action||'|'||count(*) FROM audit_log WHERE target_id = '${id}' AND action LIKE 'licensing_change.%' GROUP BY action ORDER BY action`)
  const out: Record<string, number> = {}
  for (const l of rows.split('\n').filter(Boolean)) {
    const [a, n] = l.split('|')
    out[a.replace('licensing_change.', '')] = Number(n)
  }
  return out
}

/** Everything an unchanged server looks like for one change: status, approval count, audit counts. */
export function changeFingerprint(id: string): string {
  const [status, approvals] = sql(`SELECT status||'|'||(SELECT count(*) FROM licensing_change_approvals a WHERE a.request_id = r.id) FROM licensing_change_requests r WHERE id = '${id}'`).split('|')
  return JSON.stringify({ status, approvals: Number(approvals), audit: auditCounts(id) })
}

export const globalCounts = () => ({
  requests: Number(sql(`SELECT count(*) FROM licensing_change_requests`)),
  approvals: Number(sql(`SELECT count(*) FROM licensing_change_approvals`)),
  ceilings: Number(sql(`SELECT count(*) FROM licence_country_ceilings`)),
  auditChange: Number(sql(`SELECT count(*) FROM audit_log WHERE action LIKE 'licensing_change.%'`)),
})

/** Rows of the operation rung with a product: the open version (if any) and the number of versions ever stored. */
export function productPolicy(tenant: string, country: string, operation: string, product: string): { state: string; status: string; versions: number } | null {
  const total = Number(sqlT(tenant, `SELECT count(*) FROM operating_country_policies WHERE tenant_id='${tenant}' AND scope_kind='operation' AND country_code='${country}' AND operation_code='${operation}' AND product_code='${product}'`))
  if (total === 0) return null
  const row = sqlT(tenant, `SELECT state||'|'||status FROM operating_country_policies WHERE tenant_id='${tenant}' AND scope_kind='operation' AND country_code='${country}' AND operation_code='${operation}' AND product_code='${product}' AND effective_to IS NULL`)
  const [state, status] = (row || 'none-open|none-open').split('|')
  return { state, status, versions: total }
}

export const ceilingAudit = (versionId: string) =>
  sql(`SELECT (metadata->'before'->>'status')||'>'||(metadata->'after'->>'status')||'|'||(metadata->>'reason_code')||'|'||actor_id FROM audit_log WHERE action='operating_market.licence_ceiling_version_created' AND target_id='${versionId}'`)

/**
 * The stale-UI probe. Opens the change detail as `s`, waits until the page offers Approve, THEN changes the server state with `mutate`
 * (the page is not refreshed), shows that the stale page still offers Approve, clicks it, fills the reason and submits. The caller
 * asserts the refusal text, which this function waits for, and the database.
 */
export async function staleApprove(s: Session, changeId: string, mutate: () => Promise<void>, refusal: RegExp): Promise<Locator> {
  await spaGoto(s.page, `/licensing/changes/${changeId}`)
  const approve = s.page.locator('[aria-label="Decisions"]').getByRole('button', { name: 'Approve', exact: true })
  await expect(approve).toBeEnabled()
  await mutate()
  await expect(approve).toBeEnabled() // the stale page still offers the action: the button is a hint, the server decides
  await approve.click()
  const dlg = s.page.getByRole('dialog', { name: 'Approve licensing change' })
  await dlg.getByLabel('Reason code').fill('e2e_stale_ui')
  const alert = s.page.getByRole('alert').filter({ hasText: refusal })
  await clickWithStepUp(s, () => dlg.getByRole('button', { name: 'Approve', exact: true }).click(), () => expect(alert).toBeVisible({ timeout: 15000 }))
  return alert
}

export interface ScopedPolicyForm { country: string; operation: string; brandId?: string; product?: string; action: 'enable' | 'disable' | 'withdraw'; ref?: string; reason: string }
/** Operation-rung policy dialog with an optional brand and an optional single product (mc_ui.fillPolicy only does tenant-wide, all-products). */
export async function fillScopedOperationPolicy(s: Session, f: ScopedPolicyForm): Promise<Locator> {
  await s.page.getByRole('button', { name: 'File an operating-country policy change' }).click()
  const d = s.page.getByRole('dialog', { name: 'File an operating-country policy change' })
  await d.getByLabel('Rung', { exact: true }).selectOption('operation')
  if (f.brandId) {
    await d.getByLabel('Brand scope', { exact: true }).selectOption('brand')
    await d.getByLabel('Brand', { exact: true }).selectOption(f.brandId)
  } else await d.getByLabel('Brand scope', { exact: true }).selectOption('tenant')
  await d.getByLabel('Operation code', { exact: true }).fill(f.operation)
  if (f.product) {
    await d.getByLabel('Product scope', { exact: true }).selectOption('one')
    await d.getByLabel('Product code', { exact: true }).fill(f.product)
  } else await d.getByLabel('Product scope', { exact: true }).selectOption('all')
  await d.getByLabel('Country code', { exact: true }).fill(f.country)
  const sel = d.getByLabel('Action', { exact: true })
  const re = { enable: /^Enable/, disable: /^Disable/, withdraw: /^Withdraw/ }[f.action]
  await sel.selectOption({ label: (await sel.locator('option', { hasText: re }).first().textContent()) as string })
  if (f.ref !== undefined) await d.getByRole('textbox', { name: /^Authorization reference/ }).fill(f.ref)
  await d.getByLabel('Reason code', { exact: true }).fill(f.reason)
  return d
}
