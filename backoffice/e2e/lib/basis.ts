import { expect, type Locator } from 'playwright/test'
import { clickWithStepUp, spaGoto, type Session } from './session'
import { jurisdictionId, syntheticLicenceId, tenantId, type Scope } from './api'

export const FILE_TITLE = 'File an operating basis'

export async function openTenantBases(s: Session) {
  await spaGoto(s.page, `/licensing/operating-bases/tenants/${tenantId()}`)
  await expect(s.page.getByRole('heading', { name: /Operating bases of tenant/ })).toBeVisible()
}

export interface FileOpts {
  scope: Scope
  kind: 'no_licence_documented' | 'platform_licence'
  reviewDue?: string // '' = leave blank
  reason?: string
  operationOverride?: string
}

/** Opens the dialog from the tenant list and fills every explicit field. Does not submit. */
export async function fillFileDialog(s: Session, o: FileOpts): Promise<Locator> {
  await s.page.getByRole('button', { name: FILE_TITLE }).click()
  const d = s.page.getByRole('dialog', { name: FILE_TITLE })
  await d.getByLabel('Brand scope', { exact: true }).selectOption('tenant')
  await d.getByLabel('Country code', { exact: true }).fill(o.scope.country)
  await d.getByLabel('Operation code', { exact: true }).fill(o.operationOverride ?? o.scope.operation)
  await d.getByLabel('Product scope', { exact: true }).selectOption('all')
  await d.getByLabel('Basis', { exact: true }).selectOption(o.kind)
  if (o.kind === 'no_licence_documented') {
    await expect(d.getByRole('note', { name: /./ }).filter({ hasText: 'documented decision, not proof of legality' })).toBeVisible()
    await d.getByLabel('Jurisdiction relied on', { exact: true }).selectOption(jurisdictionId())
    if (o.reviewDue) await d.getByLabel('Review due on (required)', { exact: true }).fill(o.reviewDue)
  } else {
    await d.getByLabel('Licence', { exact: true }).selectOption(syntheticLicenceId())
    if (o.reviewDue) await d.getByLabel('Review due on (optional)', { exact: true }).fill(o.reviewDue)
  }
  await d.getByLabel('Determination reference', { exact: true }).fill(`E2E-DET-${o.scope.country}-${o.scope.operation}`)
  await d.getByLabel('Determined by (function)', { exact: true }).fill('head_of_compliance')
  await d.getByLabel('Reason code', { exact: true }).fill(o.reason ?? 'e2e_basis')
  return d
}

export async function submitDialog(s: Session, d: Locator, button: string, expectText: RegExp): Promise<boolean> {
  return clickWithStepUp(
    s,
    () => d.getByRole('button', { name: button, exact: true }).click(),
    () => expect(d.getByRole('status').filter({ hasText: expectText })).toBeVisible({ timeout: 15000 }),
  )
}

export async function openRequestFromDialog(s: Session, d: Locator): Promise<string> {
  await d.getByRole('link', { name: /Open (request|record)/ }).click()
  await expect(s.page).toHaveURL(/\/licensing\/changes\/[0-9a-f-]{36}/)
  return s.page.url().split('/').pop()!
}
