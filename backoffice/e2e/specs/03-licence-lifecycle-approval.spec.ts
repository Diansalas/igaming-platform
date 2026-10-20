import { expect, test } from 'playwright/test'
import { changeStatus, createLicence, licenceStatus } from '../lib/db'
import { changeIdFromOpenLink, decide, fileStatusChange, submitFiling } from '../lib/licensing'
import { loginAs, spaGoto } from '../lib/session'
import { loadWorld } from '../lib/world'


// Reinstating a suspended licence is WIDENING: filed (pending approval), needs two approvals from two other Persons.
// Accounts: A files; A2 shares A's Person; B approves; B2 shares B's Person; C gives the second distinct approval.
test('widening licence change: pending approval, same-Person refusals rendered, two distinct approvers execute it', async ({ browser, request }) => {
  const lic = createLicence('E2E-REINSTATE', 'suspended')
  const mk = async (key: string) => loginAs(await (await browser.newContext()).newPage(), key)
  const A = await mk('PA_A')

  await fileStatusChange(A, lic.id, 'active', 'e2e_reinstate')
  await submitFiling(A, /Pending approval\. Nothing has changed yet/)
  const id = await changeIdFromOpenLink(A)
  expect(changeStatus(id)).toBe('requested')
  expect(licenceStatus(lic.id)).toBe('suspended')

  // Requester: button disabled by the UI hint ...
  await expect(A.page.locator('[aria-label="Decisions"]').getByRole('button', { name: 'Approve', exact: true })).toBeDisabled()
  await expect(A.page.getByText('You filed this request.')).toBeVisible()
  // ... and the SERVER refuses the same account (React ignores clicks on a disabled button, so call the API with A's own token).
  const self = await request.post(`${loadWorld().apiUrl}/v1/admin/licensing-changes/${id}/approve`, {
    headers: { Authorization: `Bearer ${A.accessToken()}` },
    data: { reason_code: 'e2e_self' },
  })
  expect(self.status()).toBe(409)
  expect((await self.json()).code).toBe('LICENSING_CHANGE_SELF_APPROVAL')
  expect(changeStatus(id)).toBe('requested')

  // A2 (second account of the SAME Person as the requester): UI offers Approve, server refuses.
  const A2 = await mk('PA_A2')
  await decide(A2, id, 'approve', 'e2e_sameperson', () =>
    expect(A2.page.getByRole('alert').filter({ hasText: 'another account of the same person cannot either' })).toBeVisible())
  expect(licenceStatus(lic.id)).toBe('suspended')

  // B: first approval recorded, still pending, nothing changed.
  const B = await mk('PA_B')
  await decide(B, id, 'approve', 'e2e_review_one', () => expect(B.page.getByText(/Approval recorded: 1 of 2\. Still pending approval/)).toBeVisible())
  expect(changeStatus(id)).toBe('requested')
  expect(licenceStatus(lic.id)).toBe('suspended')

  // B2 (same Person as B): cannot supply the second approval.
  const B2 = await mk('PA_B2')
  await decide(B2, id, 'approve', 'e2e_dup_person', () =>
    expect(B2.page.getByRole('alert').filter({ hasText: 'You (this person) already approved' })).toBeVisible())
  expect(licenceStatus(lic.id)).toBe('suspended')

  // C: second distinct approver executes the change.
  const C = await mk('PA_C')
  await decide(C, id, 'approve', 'e2e_review_two', () => expect(C.page.getByText('Approval recorded and the change was executed.')).toBeVisible())
  expect(changeStatus(id)).toBe('executed')
  expect(licenceStatus(lic.id)).toBe('active')

  // The licence record changed and the history shows it.
  await spaGoto(C.page, `/licensing/licences/${lic.id}`)
  await expect(C.page.getByText('Active (recorded)', { exact: true }).first()).toBeVisible()
  await expect(C.page.getByRole('cell', { name: 'update' }).first()).toBeVisible()
})
