import { expect, test } from 'playwright/test'
import { createLicence, licenceStatus } from '../lib/db'
import { fileStatusChange, submitFiling } from '../lib/licensing'
import { loginAs } from '../lib/session'

test('a narrowing change (suspend) filed by a platform admin is applied at once, no second person', async ({ page }) => {
  const lic = createLicence('E2E-NARROW', 'active')
  const B = await loginAs(page, 'PA_B')
  await fileStatusChange(B, lic.id, 'suspended', 'e2e_suspend')
  await submitFiling(B, /Applied at once/)
  expect(licenceStatus(lic.id)).toBe('suspended')
})
