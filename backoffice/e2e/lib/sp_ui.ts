// lic32: Back Office drivers and ground truth for the AML screening POLICY tab (features/aml/ScreeningPolicyTab.tsx), spec 12.
import { expect, type Locator } from 'playwright/test'
import { jurisdictionId } from './wd_aml'
import { clickWithStepUp, type Session } from './session'
import { sqlT } from './wd_db'
import type { WdTenant } from './wd_world'

export const localInput = (d: Date) => {
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}`
}
/** The start of the current minute: inside the 2-minute past tolerance, so the version is in force at once. */
export const thisMinute = () => new Date(Math.floor(Date.now() / 60000) * 60000)

export async function openScreeningPolicyTab(s: Session) {
  await s.page.getByRole('tab', { name: 'Screening policy' }).click()
  await s.page.getByLabel('Licence jurisdiction').selectOption(jurisdictionId())
  await s.page.getByRole('button', { name: 'Show screening policy in force' }).click()
}

export interface SpForm { lists: ('Sanctions list' | 'PEP list')[]; interval: string; grace: string; from: Date; legal?: string; reason: string }
export async function fillScreeningPolicy(s: Session, f: SpForm): Promise<Locator> {
  await s.page.getByRole('button', { name: 'Author next screening policy version' }).click()
  const d = s.page.getByRole('dialog', { name: 'Author the next screening policy version' })
  for (const l of f.lists) await d.getByLabel(l, { exact: true }).check()
  await d.getByLabel('Re-screen interval (seconds)', { exact: true }).fill(f.interval)
  await d.getByLabel('Backfill grace (seconds)', { exact: true }).fill(f.grace)
  await d.getByLabel('Effective from', { exact: true }).fill(localInput(f.from))
  await d.getByLabel('Legal review reference', { exact: true }).fill(f.legal ?? 'E2E-SYNTHETIC-NOT-A-LEGAL-REVIEW')
  await d.getByLabel('Reason code', { exact: true }).fill(f.reason)
  return d
}
export const SUBMIT = 'Author screening policy version'
export const submitScreeningPolicy = (s: Session, d: Locator, done: () => Promise<void>) =>
  clickWithStepUp(s, () => d.getByRole('button', { name: SUBMIT, exact: true }).click(), done)

export const spCount = (t: WdTenant) => Number(sqlT(t.id, `SELECT count(*) FROM aml_screening_policies WHERE tenant_id='${t.id}'`))
/** The stored versions, newest first, as plain strings: lists|interval|grace|created_by_scope. */
export const spRows = (t: WdTenant) =>
  sqlT(t.id, `SELECT array_to_string(lists, ',')||'|'||rescreen_interval_seconds||'|'||backfill_grace_seconds||'|'||created_by_scope FROM aml_screening_policies WHERE tenant_id='${t.id}' ORDER BY created_at DESC`).split('\n').filter(Boolean)

/** Exact UI wording of the closed refusal codes (copied, not imported: a change to the UI text must fail this spec on purpose). */
export const WORDING = {
  policy_loosening_refused: 'The new version is less strict than the latest one: a list was removed, the re-screen interval or the grace is longer, or it takes effect earlier. Loosening has no approval path and is not available here. Nothing was changed.',
  policy_effective_from_out_of_range: 'A screening policy takes effect between now (2 minutes of clock skew are tolerated) and 30 days ahead. The chosen time is outside that window. Nothing was changed.',
  rescreen_interval_below_alert_threshold: 'The re-screen interval is below twice the withdrawal gate alert threshold of this jurisdiction. Lengthen the interval, or change the gate threshold first. Nothing was changed.',
  screening_worker_heartbeat_missing: 'No screening worker has reported in, so a withdrawal gate cannot be enabled. A gate with no working worker would trap player funds. The production platform starts no screening worker, by design. Nothing was changed.',
  platform_officer_required: 'Screening configuration is authored by a platform administrator acting as an AML officer for this tenant. Your account is not acting in that capacity. Nothing was changed.',
  floorClient: 'The interval is below the engineering safety floor of 3600 seconds.',
} as const

export function codesOf(json: any): string[] {
  const c = json?.check_codes ?? json?.error?.check_codes
  return Array.isArray(c) ? c : []
}
export { expect }
