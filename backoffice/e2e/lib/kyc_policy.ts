// lic21: UI + ground-truth helpers for the KYC enforcement policy spec (07).
import { expect, type Locator } from 'playwright/test'
import { jurisdictionId } from './api'
import { lit, ownerRead } from './kyc_db'
import { platformAnchor, tenantIdOf } from './kyc_world'
import { clickWithStepUp, spaGoto, type Session } from './session'

export interface PolicyRow {
  id: string
  status: string
  trigger: string
  threshold: string
  asset: string
  playOp: string
  createdBy: string
  successor: string
  legal: string
}

const cols = `id||'|'||status||'|'||trigger_type||'|'||coalesce(threshold_minor_units::text,'')||'|'||coalesce(asset_code,'')||'|'||coalesce(play_operation,'')||'|'||created_by_actor_id||'|'||coalesce(superseded_by_policy_id::text,'')||'|'||coalesce(legal_review_reference,'')`
const parse = (l: string): PolicyRow => {
  const [id, status, trigger, threshold, asset, playOp, createdBy, successor, legal] = l.split('|')
  return { id, status, trigger, threshold, asset, playOp, createdBy, successor, legal }
}
const sess = () => ({ platformAdmin: platformAnchor() })

/** Ground truth: every policy row, oldest first (the table is platform scoped; the owner reads it). */
export function allPolicies(): PolicyRow[] {
  const out = ownerRead(sess(), `SELECT ${cols} FROM kyc_enforcement_policies ORDER BY created_at, id`)
  return out ? out.split('\n').map(parse) : []
}
export function policy(id: string): PolicyRow {
  const out = ownerRead(sess(), `SELECT ${cols} FROM kyc_enforcement_policies WHERE id = ${lit(id)}`)
  if (!out) throw new Error(`no policy ${id}`)
  return parse(out)
}
/** A fingerprint of the whole table + its audit rows: equal before and after means NOTHING changed. */
export function policyFingerprint(): string {
  return ownerRead(
    sess(),
    `SELECT md5(coalesce((SELECT string_agg(to_jsonb(p)::text, ',' ORDER BY p.id) FROM kyc_enforcement_policies p), '')) || '|' ||
            (SELECT count(*) FROM audit_log WHERE action LIKE 'kyc_enforcement_policy.%')`,
  )
}
export const policyAudit = (id: string, action: string): number =>
  Number(ownerRead(sess(), `SELECT count(*) FROM audit_log WHERE action = ${lit('kyc_enforcement_policy.' + action)} AND target_id = ${lit(id)}`))
export const policyAuditActor = (id: string, action: string): string =>
  ownerRead(sess(), `SELECT coalesce(actor_id::text,'') FROM audit_log WHERE action = ${lit('kyc_enforcement_policy.' + action)} AND target_id = ${lit(id)} ORDER BY created_at DESC LIMIT 1`)

export async function openPolicies(s: Session) {
  await s.page.getByRole('navigation').getByRole('link', { name: 'KYC Policies' }).click()
  await expect(s.page.getByRole('heading', { name: 'KYC enforcement policies' })).toBeVisible()
}

export interface AuthorOpts {
  trigger: 'cumulative_deposit' | 'play'
  threshold?: string
  asset?: string
  playOp?: 'casino_play' | 'sportsbook_play'
  legal: string
  reason: string
}
export const AUTHOR_TITLE = 'Author a KYC enforcement policy (draft)'

/** Fills every explicit field of the author dialog (nothing is defaulted). Does not submit. */
export async function fillAuthor(s: Session, o: AuthorOpts): Promise<Locator> {
  await s.page.getByRole('button', { name: 'Author a policy' }).click()
  const d = s.page.getByRole('dialog', { name: AUTHOR_TITLE })
  await d.getByLabel('Licence jurisdiction', { exact: true }).selectOption(jurisdictionId())
  await d.getByLabel('Trigger type', { exact: true }).selectOption(o.trigger)
  if (o.trigger === 'cumulative_deposit') {
    await d.getByLabel('Threshold in minor units', { exact: true }).fill(o.threshold as string)
    await d.getByLabel('Asset code', { exact: true }).fill(o.asset as string)
  } else {
    await d.getByLabel('Play operation', { exact: true }).selectOption(o.playOp as string)
  }
  await d.getByLabel('Legal review reference', { exact: true }).fill(o.legal)
  await d.getByLabel('Reason code', { exact: true }).fill(o.reason)
  return d
}

export async function submitAuthor(s: Session, d: Locator): Promise<boolean> {
  return clickWithStepUp(
    s,
    () => d.getByRole('button', { name: 'Create draft', exact: true }).click(),
    () => expect(s.page.getByRole('status').filter({ hasText: /Draft created/ })).toBeVisible({ timeout: 20000 }),
  )
}

/** The table does not print policy ids; each policy of these specs has a UNIQUE reason code, which the Reason code column shows. */
export const rowFor = (s: Session, reason: string): Locator => s.page.getByRole('row').filter({ hasText: reason })

/** Opens the action dialog from the row, enters the reason, submits; resolves when `done` holds (success text or the rendered refusal). */
export async function rowAction(s: Session, rowReason: string, action: 'Activate' | 'Withdraw draft' | 'Supersede', reason: string, extra: { successor?: string } = {}) {
  await rowFor(s, rowReason).getByRole('button', { name: action, exact: true }).click()
  const title = { Activate: 'Activate KYC enforcement policy', 'Withdraw draft': 'Withdraw draft KYC enforcement policy', Supersede: 'Supersede active KYC enforcement policy' }[action]
  const d = s.page.getByRole('dialog', { name: title })
  if (extra.successor) await d.getByLabel('Successor draft', { exact: true }).selectOption(extra.successor)
  await d.getByLabel('Reason code', { exact: true }).fill(reason)
  return d
}

export async function submitAction(s: Session, d: Locator, action: string, done: () => Promise<void>) {
  return clickWithStepUp(s, () => d.getByRole('button', { name: action, exact: true }).click(), done)
}

/** The refusal the UI renders for a server 403 on an action dialog: the fixed sentence, HTTP status and the closed code. */
export async function expectRefusalRendered(d: Locator, status: number, code: string) {
  const alert = d.getByRole('alert').filter({ hasText: `HTTP ${status}` })
  await expect(alert).toBeVisible({ timeout: 20000 })
  await expect(alert).toContainText(code)
  await expect(alert).toContainText('Nothing was changed.')
}

export const gotoPolicies = (s: Session) => spaGoto(s.page, '/kyc-policies')
export { tenantIdOf }
