import { expect, test } from 'playwright/test'
import { loadWorld } from '../lib/world'
import { loginAs, spaGoto } from '../lib/session'

test('platform admin signs in with password + TOTP and sees the Licensing section', async ({ page }) => {
  await loginAs(page, 'PA_D')
  await expect(page.getByRole('navigation').getByRole('link', { name: 'Licensing' })).toBeVisible()
  await page.getByRole('navigation').getByRole('link', { name: 'Licensing' }).click()
  await expect(page.getByRole('heading', { name: 'Licensing and jurisdictions' })).toBeVisible()
})

test('an unknown account is refused at the login form', async ({ page }) => {
  await page.goto(loadWorld().baseUrl + '/')
  await page.getByLabel('Email').fill('nobody@e2e.invalid')
  await page.getByLabel('Password').fill('not-a-real-password-1')
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page.getByRole('alert')).toBeVisible()
  await expect(page.getByLabel('Authentication code')).toHaveCount(0)
})

test('tenant staff: no Licensing nav, section denied in the UI AND by the server', async ({ page, request }) => {
  const w = loadWorld()
  const s = await loginAs(page, 'T_TENANT_ADMIN')
  await expect(page.getByRole('navigation').getByRole('link', { name: 'Licensing' })).toHaveCount(0)
  await spaGoto(page, '/licensing/licences')
  await expect(page.getByText('Your role does not have access to this section.')).toBeVisible()
  // The UI gate is a convenience; prove the server itself refuses the same tenant token.
  const tok = s.accessToken()
  expect(tok).not.toBe('')
  for (const p of ['/v1/admin/licences', '/v1/admin/jurisdictions', '/v1/admin/tenants']) {
    const r = await request.get(w.apiUrl + p, { headers: { Authorization: `Bearer ${tok}` } })
    expect(r.status(), p).toBe(403)
  }
  // By design (docs/runbooks/licensing-change-approvals.md) tenant staff may read licensing changes of THEIR OWN tenant only:
  // the list is tenant-scoped and, with nothing filed for this tenant, empty. No platform change is visible.
  const own = await request.get(w.apiUrl + '/v1/admin/licensing-changes', { headers: { Authorization: `Bearer ${tok}` } })
  expect(own.status()).toBe(200)
  expect((await own.json()).items).toEqual([])
  const anon = await request.get(w.apiUrl + '/v1/admin/licences')
  expect(anon.status()).toBe(401)
})

test('platform admin token is accepted by the same endpoints (control for the denial above)', async ({ page, request }) => {
  const s = await loginAs(page, 'PA_D')
  for (const p of ['/v1/admin/licensing-changes', '/v1/admin/licences', '/v1/admin/jurisdictions']) {
    const r = await request.get(loadWorld().apiUrl + p, { headers: { Authorization: `Bearer ${s.accessToken()}` } })
    expect(r.status(), p).toBe(200)
  }
})
