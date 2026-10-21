// lic32: Back Office UI drivers for the market-configuration dialogs (features/licensing/MarketDialogs.tsx).
import { expect, type Locator } from 'playwright/test'
import { clickWithStepUp, spaGoto, type Session } from './session'

export const CEILING_TITLE = 'File a licence country ceiling change'
export const POLICY_TITLE = 'File an operating-country policy change'
export const CONFIG_TITLE = 'File a tenant jurisdiction configuration change'
export const JURISDICTION_TITLE = 'Create a jurisdiction'

export const ACTION_TEXT = { enable: /^Enable/, disable: /^Disable/, withdraw: /^Withdraw/ }

export async function openLicencePage(s: Session, licenceId: string) {
  await spaGoto(s.page, `/licensing/licences/${licenceId}`)
  await expect(s.page.getByRole('button', { name: 'File a country ceiling change' })).toBeVisible()
}
export async function openTenantPage(s: Session, tenantId: string) {
  await spaGoto(s.page, `/licensing/tenants/${tenantId}`)
  await expect(s.page.getByRole('button', { name: 'File an operating-country policy change' })).toBeVisible()
}

async function pickAction(d: Locator, action: 'enable' | 'disable' | 'withdraw') {
  const sel = d.getByLabel('Action', { exact: true })
  const label = await sel.locator('option', { hasText: ACTION_TEXT[action] }).first().textContent()
  await sel.selectOption({ label: label! })
}

export interface CeilingForm { country: string; action: 'enable' | 'disable' | 'withdraw'; ref?: string; reason: string }
/** Opens the generic entry on the licence page and fills every explicit field. Does not submit. */
export async function fillCeiling(s: Session, f: CeilingForm): Promise<Locator> {
  await s.page.getByRole('button', { name: 'File a country ceiling change' }).click()
  const d = s.page.getByRole('dialog', { name: CEILING_TITLE })
  await d.getByLabel('Country code', { exact: true }).fill(f.country)
  await pickAction(d, f.action)
  if (f.ref !== undefined) await d.getByRole('textbox', { name: /^Authorization reference/ }).fill(f.ref)
  await d.getByLabel('Reason code', { exact: true }).fill(f.reason)
  return d
}

export interface PolicyForm {
  rung: 'tenant' | 'brand' | 'operation'
  country: string
  action: 'enable' | 'disable' | 'withdraw'
  ref?: string
  reason: string
  brandId?: string
  operation?: string
}
export async function fillPolicy(s: Session, f: PolicyForm): Promise<Locator> {
  await s.page.getByRole('button', { name: 'File an operating-country policy change' }).click()
  const d = s.page.getByRole('dialog', { name: POLICY_TITLE })
  await d.getByLabel('Rung', { exact: true }).selectOption(f.rung)
  if (f.rung === 'brand') await d.getByLabel('Brand', { exact: true }).selectOption(f.brandId!)
  if (f.rung === 'operation') {
    await d.getByLabel('Brand scope', { exact: true }).selectOption('tenant')
    await d.getByLabel('Operation code', { exact: true }).fill(f.operation!)
    await d.getByLabel('Product scope', { exact: true }).selectOption('all')
  }
  await d.getByLabel('Country code', { exact: true }).fill(f.country)
  await pickAction(d, f.action)
  if (f.ref !== undefined) await d.getByRole('textbox', { name: /^Authorization reference/ }).fill(f.ref)
  await d.getByLabel('Reason code', { exact: true }).fill(f.reason)
  return d
}

export async function submitMarket(s: Session, d: Locator, button: string, expectText: RegExp): Promise<boolean> {
  return clickWithStepUp(
    s,
    () => d.getByRole('button', { name: button, exact: true }).click(),
    () => expect(d.getByRole('status').filter({ hasText: expectText })).toBeVisible({ timeout: 15000 }),
  )
}
/** Clicks submit and waits for a rendered refusal (role=alert) in the dialog; answers a step-up prompt if one appears. */
export async function submitRefused(s: Session, d: Locator, button: string, alertText: RegExp): Promise<boolean> {
  return clickWithStepUp(
    s,
    () => d.getByRole('button', { name: button, exact: true }).click(),
    () => expect(d.getByRole('alert').filter({ hasText: alertText })).toBeVisible({ timeout: 15000 }),
  )
}

export async function openChangeLink(s: Session, d: Locator): Promise<string> {
  await d.getByRole('link', { name: /Open (request|record)/ }).click()
  await expect(s.page).toHaveURL(/\/licensing\/changes\/[0-9a-f-]{36}/)
  return s.page.url().split('/').pop()!
}
export const classHint = (d: Locator) => d.getByRole('note', { name: 'Change class hint' })
