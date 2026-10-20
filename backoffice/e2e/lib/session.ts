import { expect, type Page } from 'playwright/test'
import { account, loadWorld, totpFor } from './world'

/** Sessions live in memory only (a reload ends them), so navigation after login is always client-side. */
export async function spaGoto(page: Page, path: string) {
  await page.evaluate((p) => {
    window.history.pushState({}, '', p)
    window.dispatchEvent(new PopStateEvent('popstate'))
  }, path)
}

export interface Session {
  page: Page
  key: string
  accessToken: () => string
}

/** Real UI login: password step, then the TOTP step. Returns the page and the access token seen on the wire (for API probes). */
export async function loginAs(page: Page, key: string): Promise<Session> {
  const a = account(key)
  let token = ''
  page.on('response', async (r) => {
    if (r.url().endsWith('/v1/staff/auth/mfa/verify') && r.ok()) {
      try {
        token = (await r.json()).access_token ?? token
      } catch {
        /* not json */
      }
    }
  })
  await page.goto(loadWorld().baseUrl + '/')
  await page.getByLabel('Tenant slug').fill(a.tenantSlug)
  await page.getByLabel('Email').fill(a.email)
  await page.getByLabel('Password').fill(a.password)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await page.getByLabel('Authentication code').fill(await totpFor(key))
  await page.getByRole('button', { name: 'Verify' }).click()
  await expect(page.getByRole('heading', { name: 'Operator Back Office' })).toHaveCount(0, { timeout: 15000 })
  await expect.poll(() => token, { timeout: 15000 }).not.toBe('')
  return { page, key, accessToken: () => token }
}

/** Clicks, and answers the step-up prompt ("Confirm it is you") if the server asks for one before `done` shows. */
export async function clickWithStepUp(s: Session, click: () => Promise<void>, done: () => Promise<void>): Promise<boolean> {
  const dialog = s.page.getByRole('dialog', { name: /Confirm it is you/ })
  await click()
  const outcome = await Promise.race([
    dialog.waitFor({ state: 'visible', timeout: 15000 }).then(() => 'stepup' as const),
    done().then(() => 'done' as const),
  ])
  if (outcome === 'stepup') {
    await dialog.getByLabel('6-digit code').fill(await totpFor(s.key))
    await dialog.getByRole('button', { name: 'Verify' }).click()
    await done()
  }
  return outcome === 'stepup'
}
