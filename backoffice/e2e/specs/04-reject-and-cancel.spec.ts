import { expect, test } from 'playwright/test'
import { changeStatus, createLicence, licenceStatus } from '../lib/db'
import { changeIdFromOpenLink, decide, fileStatusChange, submitFiling } from '../lib/licensing'
import { loginAs } from '../lib/session'

test('a second person rejects a pending widening change: closed, licence unchanged', async ({ browser }) => {
  const lic = createLicence('E2E-REJECT', 'suspended')
  const C = await loginAs(await (await browser.newContext()).newPage(), 'PA_C')
  await fileStatusChange(C, lic.id, 'active', 'e2e_reinstate_reject')
  await submitFiling(C, /Pending approval/)
  const id = await changeIdFromOpenLink(C)

  const D = await loginAs(await (await browser.newContext()).newPage(), 'PA_D')
  await decide(D, id, 'reject', 'e2e_not_justified', () => expect(D.page.getByText('Rejected. The request is closed and nothing was changed.')).toBeVisible())
  expect(changeStatus(id)).toBe('rejected')
  expect(licenceStatus(lic.id)).toBe('suspended')
})

test('the requester cancels their own pending change: closed, licence unchanged', async ({ browser }) => {
  const lic = createLicence('E2E-CANCEL', 'suspended')
  const C = await loginAs(await (await browser.newContext()).newPage(), 'PA_C')
  await fileStatusChange(C, lic.id, 'active', 'e2e_reinstate_cancel')
  await submitFiling(C, /Pending approval/)
  const id = await changeIdFromOpenLink(C)
  await decide(C, id, 'cancel', '', () => expect(C.page.getByText('Cancelled. The request is closed and nothing was changed.')).toBeVisible())
  expect(changeStatus(id)).toBe('cancelled')
  expect(licenceStatus(lic.id)).toBe('suspended')
})
