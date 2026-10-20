// lic21: ground truth and UI helpers for the residence maker-checker spec (08).
import { expect, type Locator } from 'playwright/test'
import { lit, ownerRead } from './kyc_db'
import { tenantIdOf } from './kyc_world'
import { clickWithStepUp, type Session } from './session'

const T = () => ({ tenant: tenantIdOf() })
const rd = (q: string) => ownerRead(T(), q)

export interface Verification { country: string; setBy: string; setAt: string; setAtMicro: string; status: string; person: string }
export function verification(id: string): Verification {
  const [country, setBy, setAt, setAtMicro, status, person] = rd(
    `SELECT coalesce(verified_residence_country,'')||'|'||coalesce(verified_residence_set_by::text,'')||'|'||coalesce(verified_residence_set_at::text,'')||'|'||
            coalesce(to_char(verified_residence_set_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),'')||'|'||status||'|'||coalesce(person_id::text,'')
       FROM kyc_verifications WHERE id = ${lit(id)}`,
  ).split('|')
  return { country, setBy, setAt, setAtMicro, status, person }
}

export interface Check { approvalRequired: boolean; mode: string; approvedBy: string; approvedByPerson: string; approvedAt: string; determinedBy: string; determinedByPerson: string; reason: string; country: string }
export function checks(verificationId: string): Check[] {
  const out = rd(
    `SELECT approval_required::text||'|'||policy_mode||'|'||coalesce(approved_by::text,'')||'|'||coalesce(approved_by_person_id::text,'')||'|'||coalesce(approved_at::text,'')||'|'||
            determined_by||'|'||coalesce(determined_by_person_id::text,'')||'|'||coalesce(approval_reason_code,'')||'|'||country
       FROM residence_determination_checks WHERE verification_id = ${lit(verificationId)} ORDER BY determined_at`,
  )
  return out
    ? out.split('\n').map((l) => {
        const [ar, mode, approvedBy, approvedByPerson, approvedAt, determinedBy, determinedByPerson, reason, country] = l.split('|')
        return { approvalRequired: ar === 'true', mode, approvedBy, approvedByPerson, approvedAt, determinedBy, determinedByPerson, reason, country }
      })
    : []
}

export interface PolicyVersion { id: string; mode: string; previous: string; status: string; required: number; approvals: number; requestedBy: string; activatedAt: string }
export function policyVersions(): PolicyVersion[] {
  const out = rd(
    `SELECT p.id||'|'||p.mode||'|'||p.previous_mode||'|'||p.status||'|'||p.required_approvals||'|'||
            (SELECT count(*) FROM residence_approval_policy_approvals a WHERE a.policy_id = p.id)||'|'||p.requested_by||'|'||coalesce(p.activated_at::text,'')
       FROM residence_approval_policies p ORDER BY p.requested_at, p.id`,
  )
  return out
    ? out.split('\n').map((l) => {
        const [id, mode, previous, status, required, approvals, requestedBy, activatedAt] = l.split('|')
        return { id, mode, previous, status, required: Number(required), approvals: Number(approvals), requestedBy, activatedAt }
      })
    : []
}
export const policyApprovers = (policyId: string): string[] =>
  rd(`SELECT approver_person_id FROM residence_approval_policy_approvals WHERE policy_id = ${lit(policyId)} ORDER BY approved_at`).split('\n').filter(Boolean)
/** The mode in force, computed by the database's own function. */
export const modeInForce = (): string => rd(`SELECT residence_policy_mode(${lit(tenantIdOf())}::uuid)`)

/** A fingerprint of everything the residence controls guard for one verification plus the three residence tables and their audit rows. */
export function fingerprint(verificationId: string): string {
  return rd(
    `SELECT md5(
        coalesce((SELECT to_jsonb(k)::text FROM kyc_verifications k WHERE k.id = ${lit(verificationId)}), '') ||
        coalesce((SELECT string_agg(to_jsonb(c)::text, ',' ORDER BY c.id) FROM residence_determination_checks c), '') ||
        coalesce((SELECT string_agg(to_jsonb(p)::text, ',' ORDER BY p.id) FROM residence_approval_policies p), '') ||
        coalesce((SELECT string_agg(to_jsonb(a)::text, ',' ORDER BY a.id) FROM residence_approval_policy_approvals a), '')
     ) || '|' || (SELECT count(*) FROM audit_log WHERE action LIKE 'kyc.residence%' OR action LIKE 'residence_policy.%')`,
  )
}
export const residenceAudit = (action: string, target?: string): number =>
  Number(rd(`SELECT count(*) FROM audit_log WHERE action = ${lit(action)}${target ? ` AND target_id = ${lit(target)}` : ''}`))

// ---- UI -------------------------------------------------------------------------------------------------------------------------

export async function openResidence(s: Session) {
  await s.page.getByRole('navigation').getByRole('link', { name: 'Residence Approval' }).click()
  await expect(s.page.getByRole('heading', { name: 'Residence determination approval' })).toBeVisible()
  await expect(s.page.getByText('Mode in force', { exact: true })).toBeVisible()
}

export const modeCard = (s: Session): Locator => s.page.locator('form').filter({ has: s.page.getByLabel('New mode', { exact: true }) })

/** Chooses a mode explicitly and files it; resolves when `done` holds. */
export async function fileMode(s: Session, label: 'None' | 'Into open market' | 'All', reason: string, hint: RegExp, done: () => Promise<void>) {
  const f = modeCard(s)
  await f.getByLabel('New mode', { exact: true }).selectOption({ label })
  await expect(f.getByRole('note', { name: 'Class hint' })).toContainText(hint)
  await f.getByLabel('Reason code', { exact: true }).fill(reason)
  await clickWithStepUp(s, () => f.getByRole('button', { name: 'File the change' }).click(), done)
}

export const versionRow = (s: Session, mode: string): Locator => s.page.getByRole('row').filter({ hasText: 'Pending approval' }).filter({ hasText: mode })
export const queueRow = (s: Session, verificationId: string): Locator => s.page.getByRole('row').filter({ hasText: verificationId.slice(0, 8) })

export async function approvePolicyDialog(s: Session, row: Locator, reason: string, done: (d: Locator) => Promise<void>) {
  await row.getByRole('button', { name: 'Approve', exact: true }).click()
  const d = s.page.getByRole('dialog', { name: 'Approve residence mode change' })
  await d.getByLabel('Reason code', { exact: true }).fill(reason)
  await clickWithStepUp(s, () => d.getByRole('button', { name: 'Approve', exact: true }).click(), () => done(d))
  return d
}

export async function approveDeterminationDialog(s: Session, verificationId: string, reason: string, done: (d: Locator) => Promise<void>) {
  await queueRow(s, verificationId).getByRole('button', { name: 'Approve as second Person' }).click()
  const d = s.page.getByRole('dialog', { name: 'Approve residence determination' })
  await d.getByLabel('Reason code', { exact: true }).fill(reason)
  await clickWithStepUp(s, () => d.getByRole('button', { name: 'Approve determination', exact: true }).click(), () => done(d))
  return d
}

export async function expectRefusal(d: Locator, status: number, code = 'forbidden') {
  const a = d.getByRole('alert').filter({ hasText: `HTTP ${status}` })
  await expect(a).toBeVisible({ timeout: 20000 })
  await expect(a).toContainText(code)
  await expect(a).toContainText('Nothing was changed')
}
