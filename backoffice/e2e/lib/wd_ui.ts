// lic22: Back Office UI drivers for the withdrawal restriction policy screens (features/policies/WithdrawalRestrictionPages.tsx).
import { expect, type Locator } from 'playwright/test'
import { clickWithStepUp, spaGoto, type Session } from './session'
import type { WdTenant } from './wd_world'

/** Nav -> Withdrawal Restrictions -> explicit tenant picker (no tenant is preselected) -> the tenant's policy page. */
export async function openPolicyViaNav(s: Session, t: WdTenant) {
  await s.page.getByRole('navigation').getByRole('link', { name: 'Withdrawal Restrictions' }).click()
  await expect(s.page.getByRole('heading', { name: 'Withdrawal restriction policy' })).toBeVisible()
  await s.page.getByLabel('Find tenant').fill(t.slug)
  await expect(s.page.getByLabel('Tenant', { exact: true }).locator('option', { hasText: t.slug })).toHaveCount(1)
  await s.page.getByLabel('Tenant', { exact: true }).selectOption(t.id)
  await expect(s.page).toHaveURL(new RegExp(`/withdrawal-restrictions/tenants/${t.id}`))
}

export const openPolicy = async (s: Session, t: WdTenant) => {
  await spaGoto(s.page, `/withdrawal-restrictions/tenants/${t.id}`)
  await expect(s.page.getByText(/configured set version \d+/)).toBeVisible()
}

export type CauseLabel = 'Licence suspended' | 'Licence expired' | 'Licence not yet issued' | 'No licence bound'
export type ActionLabel = 'Allow' | 'Hold for review' | 'Block'

/** Opens the filing dialog and fills every explicit field. Does not submit. */
export async function fillFiling(s: Session, cause: CauseLabel, action: ActionLabel, reason: string, note?: string): Promise<Locator> {
  await s.page.getByRole('button', { name: 'File a change' }).click()
  const d = s.page.getByRole('dialog', { name: 'File a withdrawal restriction change' })
  await d.getByLabel('Cause').selectOption({ label: cause })
  await d.getByLabel('Action').selectOption({ label: action })
  await d.getByLabel('Reason code').fill(reason)
  if (note) await d.getByLabel('Note (optional)').fill(note)
  return d
}

export async function submitFilingDialog(s: Session, d: Locator, expectText: RegExp) {
  await clickWithStepUp(
    s,
    () => d.getByRole('button', { name: 'File change' }).click(),
    () => expect(s.page.getByRole('status').filter({ hasText: expectText }).first()).toBeVisible({ timeout: 15000 }),
  )
}

/** Opens a change request from the "Open request" / "Open record" link shown after filing, returns its id. */
export async function openFiledChange(s: Session): Promise<string> {
  await s.page.getByRole('link', { name: /Open (request|record)/ }).first().click()
  await expect(s.page).toHaveURL(/\/withdrawal-restrictions\/changes\/[0-9a-f-]{36}/)
  return s.page.url().split('/').pop()!
}

/** Opens the decision dialog (approve|reject|cancel) of a change, enters the reason when needed and confirms. */
export async function decideChange(s: Session, changeId: string, op: 'approve' | 'reject' | 'cancel', reason: string, done: () => Promise<void>) {
  const p = s.page
  await spaGoto(p, `/withdrawal-restrictions/changes/${changeId}`)
  const open = op === 'cancel' ? 'Cancel request' : op === 'approve' ? 'Approve' : 'Reject'
  await p.locator('[aria-label="Decisions"]').getByRole('button', { name: open, exact: true }).click()
  const title = { approve: 'Approve withdrawal restriction change', reject: 'Reject withdrawal restriction change', cancel: 'Cancel withdrawal restriction change' }[op]
  const dlg = p.getByRole('dialog', { name: title })
  if (op !== 'cancel') await dlg.getByLabel('Reason code').fill(reason)
  await clickWithStepUp(s, () => dlg.getByRole('button', { name: open, exact: true }).click(), done)
  return dlg
}
