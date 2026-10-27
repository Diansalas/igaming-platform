import { expect, test } from 'playwright/test'
import { createLicence } from '../lib/db'
import { loadWorld } from '../lib/world'
import { loginAs } from '../lib/session'

test('jurisdictions, licences, licence detail and version history render real API data', async ({ page }) => {
  const w = loadWorld()
  const lic = createLicence('E2E-READ', 'active')
  await loginAs(page, 'PA_D')
  await page.getByRole('navigation').getByRole('link', { name: 'Licensing' }).click()

  await page.getByRole('link', { name: 'Jurisdictions' }).click()
  await expect(page.getByRole('link', { name: w.jurisdictionCode })).toBeVisible()
  await page.getByLabel('Filter jurisdictions').fill('no-such-jurisdiction')
  await expect(page.getByText('No jurisdictions are recorded.')).toBeVisible()
  await page.getByLabel('Filter jurisdictions').fill(w.jurisdictionCode)
  await page.getByRole('link', { name: w.jurisdictionCode }).click()
  await expect(page.getByText('Read-only reference data.')).toBeVisible()
  await expect(page.getByRole('link', { name: w.syntheticLicenceNumber })).toBeVisible()

  await page.getByRole('link', { name: 'Licences', exact: true }).click()
  await expect(page.getByRole('link', { name: w.syntheticLicenceNumber })).toBeVisible()
  await page.getByLabel('Filter licences').fill(lic.number)
  await page.getByRole('link', { name: lic.number }).click()
  await expect(page.getByText(`Licence ${lic.number}`)).toBeVisible()
  await expect(page.getByText('Version history')).toBeVisible()
  await expect(page.getByRole('cell', { name: 'insert' }).first()).toBeVisible()

  await page.getByRole('link', { name: 'Licensing changes' }).click()
  await expect(page.getByRole('combobox', { name: 'Status' })).toBeVisible()
  await expect(page.getByRole('note', { name: 'Disclaimer' })).toBeVisible()
})
