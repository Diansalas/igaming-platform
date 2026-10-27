import { expect } from 'playwright/test'
import { clickWithStepUp, spaGoto, type Session } from './session'

/** Files a licence status change from the licence detail page; returns the new change request id. */
export async function fileStatusChange(s: Session, licenceId: string, status: 'active' | 'suspended' | 'expired', reason: string) {
  const p = s.page
  await spaGoto(p, `/licensing/licences/${licenceId}`)
  await p.getByRole('button', { name: 'Change licence status' }).click()
  const dlg = p.getByRole('dialog', { name: 'File a licensing change' })
  await dlg.getByLabel('New status').selectOption(status)
  await dlg.getByLabel('Reason code').fill(reason)
  return dlg
}

export async function submitFiling(s: Session, expectText: RegExp) {
  const dlg = s.page.getByRole('dialog', { name: 'File a licensing change' })
  await clickWithStepUp(
    s,
    () => dlg.getByRole('button', { name: 'File change' }).click(),
    () => expect(dlg.getByRole('status').filter({ hasText: expectText })).toBeVisible({ timeout: 15000 }),
  )
  return dlg
}

export async function changeIdFromOpenLink(s: Session): Promise<string> {
  const dlg = s.page.getByRole('dialog', { name: 'File a licensing change' })
  await dlg.getByRole('link', { name: /Open request/ }).click()
  await expect(s.page).toHaveURL(/\/licensing\/changes\/[0-9a-f-]{36}/)
  return s.page.url().split('/').pop()!
}

/** Opens the decision dialog (approve|reject|cancel), enters the reason when needed and confirms. */
export async function decide(s: Session, changeId: string, op: 'approve' | 'reject' | 'cancel', reason: string, done: () => Promise<void>) {
  const p = s.page
  await spaGoto(p, `/licensing/changes/${changeId}`)
  const open = op === 'cancel' ? 'Cancel request' : op === 'approve' ? 'Approve' : 'Reject'
  const btn = p.locator('[aria-label="Decisions"]').getByRole('button', { name: open, exact: true })
  await btn.click()
  const title = { approve: 'Approve licensing change', reject: 'Reject licensing change', cancel: 'Cancel licensing change' }[op]
  const dlg = p.getByRole('dialog', { name: title })
  if (op !== 'cancel') await dlg.getByLabel('Reason code').fill(reason)
  await clickWithStepUp(s, () => dlg.getByRole('button', { name: open, exact: true }).click(), done)
  return dlg
}
