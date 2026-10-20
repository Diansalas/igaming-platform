// Operating bases through the REAL stack (lic15). Each test uses its own (country, operation) scope in the synthetic tenant, so
// tests are independent; the database is fresh per run (README). Ground truth is read from the database as the table owner.
import { expect, test } from 'playwright/test'
import { apiApprove, apiFile, apiLogin, currentBasis, daysFromToday, seedActiveBasis, setPayload, syntheticLicenceId, tenantId } from '../lib/api'
import { fillFileDialog, openRequestFromDialog, openTenantBases, submitDialog } from '../lib/basis'
import { changeStatus } from '../lib/db'
import { decide } from '../lib/licensing'
import { loginAs, spaGoto } from '../lib/session'
import { loadWorld } from '../lib/world'

const ctx = async (browser: import('playwright/test').Browser, key: string) => loginAs(await (await browser.newContext()).newPage(), key)

test('(a) no-licence basis: explicit fields, pending approval, requester refused, two distinct approvers activate it', async ({ browser, request }) => {
  const scope = { country: 'FI', operation: 'wagering' }
  expect(currentBasis(scope)).toBeNull()
  const A = await ctx(browser, 'PA_A')
  // reach the screen through the nav and the explicit tenant picker (no default tenant)
  await A.page.getByRole('navigation').getByRole('link', { name: 'Licensing' }).click()
  await A.page.getByRole('link', { name: 'Operating bases', exact: true }).click()
  await A.page.getByLabel('Find tenant').fill(loadWorld().tenantSlug)
  await expect(A.page.getByLabel('Tenant', { exact: true }).locator('option', { hasText: loadWorld().tenantSlug })).toHaveCount(1)
  await A.page.getByLabel('Tenant', { exact: true }).selectOption(tenantId())
  await expect(A.page.getByText('No operating basis is recorded for this tenant')).toBeVisible()

  const d = await fillFileDialog(A, { scope, kind: 'no_licence_documented', reviewDue: daysFromToday(90) })
  await submitDialog(A, d, 'File basis', /Pending approval\. Nothing has changed yet/)
  const id = await openRequestFromDialog(A, d)
  // BEFORE approval: no basis row exists, so the resolver (which reads only approved, executed rows) cannot permit this scope.
  expect(currentBasis(scope)).toBeNull()
  expect(changeStatus(id)).toBe('requested')
  const api = await request.get(`${loadWorld().apiUrl}/v1/admin/operating-bases?tenant_id=${tenantId()}&country_code=FI&current=true`, { headers: { Authorization: `Bearer ${A.accessToken()}` } })
  expect((await api.json()).total).toBe(0)

  // requester cannot approve: UI hint + server refusal
  await expect(A.page.locator('[aria-label="Decisions"]').getByRole('button', { name: 'Approve', exact: true })).toBeDisabled()
  const self = await request.post(`${loadWorld().apiUrl}/v1/admin/licensing-changes/${id}/approve`, { headers: { Authorization: `Bearer ${A.accessToken()}` }, data: { reason_code: 'e2e_self' } })
  expect(self.status()).toBe(409)
  expect(currentBasis(scope)).toBeNull()

  const B = await ctx(browser, 'PA_B')
  await decide(B, id, 'approve', 'e2e_basis_one', () => expect(B.page.getByText(/Approval recorded: 1 of 2\. Still pending approval/)).toBeVisible())
  expect(currentBasis(scope)).toBeNull()
  const C = await ctx(browser, 'PA_C')
  await decide(C, id, 'approve', 'e2e_basis_two', () => expect(C.page.getByText('Approval recorded and the change was executed.')).toBeVisible())

  const b = currentBasis(scope)!
  expect(b.status).toBe('active')
  expect(b.kind).toBe('no_licence_documented')
  expect(b.changeRequestId).toBe(id)
  expect(b.reviewDue).toBe(daysFromToday(90))

  // the detail screen shows the decision record with both approvers
  await spaGoto(C.page, `/licensing/operating-bases/${b.id}`)
  await expect(C.page.getByLabel('Approvers')).toBeVisible()
  await expect(C.page.getByLabel('Approvers').getByRole('listitem')).toHaveCount(2)
  await expect(C.page.getByText('Documented decision without a licence').first()).toBeVisible()
})

test('(b) licence-kind basis (platform licence): pending approval then two approvers activate it', async ({ browser }) => {
  const scope = { country: 'FI', operation: 'deposit' }
  const A = await ctx(browser, 'PA_A')
  await openTenantBases(A)
  const d = await fillFileDialog(A, { scope, kind: 'platform_licence' })
  await submitDialog(A, d, 'File basis', /Pending approval/)
  const id = await openRequestFromDialog(A, d)
  expect(currentBasis(scope)).toBeNull()
  const B = await ctx(browser, 'PA_B')
  await decide(B, id, 'approve', 'e2e_licbasis_one', () => expect(B.page.getByText(/Approval recorded: 1 of 2/)).toBeVisible())
  const C = await ctx(browser, 'PA_C')
  await decide(C, id, 'approve', 'e2e_licbasis_two', () => expect(C.page.getByText('Approval recorded and the change was executed.')).toBeVisible())
  const b = currentBasis(scope)!
  expect(b.status).toBe('active')
  expect(b.kind).toBe('platform_licence')
  expect(b.changeRequestId).toBe(id)
  await spaGoto(C.page, `/licensing/operating-bases/${b.id}`)
  await expect(C.page.getByRole('link', { name: 'STG-SYNTHETIC-1' })).toBeVisible()
  void syntheticLicenceId
})

test('(c) withdraw a basis: single actor, applied at once, scope then relies on no basis', async ({ browser }) => {
  const scope = { country: 'SE', operation: 'wagering' }
  const seeded = await seedActiveBasis(scope)
  const D = await ctx(browser, 'PA_D')
  await spaGoto(D.page, `/licensing/operating-bases/${seeded.id}`)
  await D.page.getByRole('button', { name: 'Withdraw this basis' }).click()
  const d = D.page.getByRole('dialog', { name: 'Withdraw operating basis' })
  await d.getByLabel('Reason code', { exact: true }).fill('e2e_withdraw')
  await submitDialog(D, d, 'Withdraw basis', /Applied at once/)
  const b = currentBasis(scope)!
  expect(b.status).toBe('withdrawn')
  expect(b.versions).toBe(2)
})

test('(d) review: flag for re-review applies at once; extending the date and clearing the flag is widening (pending, then approved)', async ({ browser }) => {
  const scope = { country: 'SE', operation: 'deposit' }
  const seeded = await seedActiveBasis(scope)
  const D = await ctx(browser, 'PA_D')
  await spaGoto(D.page, `/licensing/operating-bases/${seeded.id}`)
  await D.page.getByRole('button', { name: 'Record a review' }).click()
  let d = D.page.getByRole('dialog', { name: 'Record an operating basis review' })
  await d.getByLabel('Review outcome').selectOption('flag')
  await d.getByLabel('New review due on').fill(daysFromToday(30)) // earlier than 90: narrowing
  await d.getByLabel('Review reference').fill('E2E-REVIEW-1')
  await d.getByLabel('Reason code', { exact: true }).fill('e2e_flag')
  await expect(d.getByRole('note', { name: 'Review class hint' })).toContainText('narrowing')
  await submitDialog(D, d, 'File review', /Applied at once/)
  let b = currentBasis(scope)!
  expect(b.rereview).toBe(true)
  expect(b.reviewDue).toBe(daysFromToday(30))
  expect(b.versions).toBe(2)
  await d.getByRole('button', { name: 'Close', exact: true }).last().click()

  // the new current version is shown as flagged; reopen it and extend (widening)
  await spaGoto(D.page, `/licensing/operating-bases/${b.id}`)
  await expect(D.page.getByText('Flagged for re-review: no longer permitting').first()).toBeVisible()
  await D.page.getByRole('button', { name: 'Record a review' }).click()
  d = D.page.getByRole('dialog', { name: 'Record an operating basis review' })
  await d.getByLabel('Review outcome').selectOption('clear')
  await d.getByLabel('New review due on').fill(daysFromToday(120))
  await d.getByLabel('Review reference').fill('E2E-REVIEW-2')
  await d.getByLabel('Reason code', { exact: true }).fill('e2e_extend')
  await expect(d.getByRole('note', { name: 'Review class hint' })).toContainText('widening')
  await submitDialog(D, d, 'File review', /Pending approval/)
  const id = await openRequestFromDialog(D, d)
  b = currentBasis(scope)!
  expect(b.rereview).toBe(true) // unchanged until two approvals
  for (const k of ['PA_B', 'PA_C']) expect((await apiApprove(await apiLogin(k), id)).status).toBe(200)
  b = currentBasis(scope)!
  expect(b.rereview).toBe(false)
  expect(b.reviewDue).toBe(daysFromToday(120))
  expect(b.versions).toBe(3)
})

test('(e) tenant staff: operating-basis screens hidden in the UI and refused by the server', async ({ page, request }) => {
  const w = loadWorld()
  const s = await loginAs(page, 'T_TENANT_ADMIN')
  await expect(page.getByRole('navigation').getByRole('link', { name: 'Licensing' })).toHaveCount(0)
  await spaGoto(page, `/licensing/operating-bases/tenants/${tenantId()}`)
  await expect(page.getByText('Your role does not have access to this section.')).toBeVisible()
  const h = { Authorization: `Bearer ${s.accessToken()}` }
  expect((await request.get(`${w.apiUrl}/v1/admin/operating-bases?tenant_id=${tenantId()}`, { headers: h })).status()).toBe(403)
  // tenant staff cannot file a basis either (not 2xx; no row and no request written)
  const before = currentBasis({ country: 'FI', operation: 'registration' })
  const f = await request.post(`${w.apiUrl}/v1/admin/licensing-changes`, { headers: h, data: { kind: 'operating_basis_set', reason_code: 'e2e_tenant', payload: setPayload({ country: 'FI', operation: 'registration' }) } })
  expect(f.status()).toBeGreaterThanOrEqual(400)
  expect(f.status()).toBeLessThan(500)
  expect(currentBasis({ country: 'FI', operation: 'registration' })).toEqual(before)
})

test('(f) validation: client refuses an empty review date for a no-licence basis; server refusal of a bad payload is rendered', async ({ browser }) => {
  const scope = { country: 'FI', operation: 'registration' }
  const A = await ctx(browser, 'PA_A')
  await openTenantBases(A)
  // no review date: the submit button stays disabled
  let d = await fillFileDialog(A, { scope, kind: 'no_licence_documented' })
  await expect(d.getByRole('button', { name: 'File basis', exact: true })).toBeDisabled()
  await expect(d.getByLabel('Review due on (required)')).toHaveValue('')
  await d.getByLabel('Review due on (required)').fill(daysFromToday(60))
  await expect(d.getByRole('button', { name: 'File basis', exact: true })).toBeEnabled()
  // an operation that passes the client shape check but is not in the vocabulary: the SERVER refuses and the UI shows it
  await d.getByLabel('Operation code', { exact: true }).fill('e2e_unknown_operation')
  await clickRefusal(A, d)
  await expect(d.getByRole('alert').filter({ hasText: /Nothing was changed|could not be completed|rejected the request/ })).toBeVisible()
  expect(currentBasis(scope)).toBeNull()
  expect(currentBasis({ country: 'FI', operation: 'e2e_unknown_operation' })).toBeNull()
})

async function clickRefusal(s: Awaited<ReturnType<typeof ctx>>, d: import('playwright/test').Locator, button = 'File basis') {
  await d.getByRole('button', { name: button, exact: true }).click()
  const step = s.page.getByRole('dialog', { name: /Confirm it is you/ })
  if (await step.isVisible({ timeout: 3000 }).catch(() => false)) {
    const { totpFor } = await import('../lib/world')
    await step.getByLabel('6-digit code').fill(await totpFor(s.key))
    await step.getByRole('button', { name: 'Verify' }).click()
  }
}

test('(g) a review date in the past: records an overdue basis that is shown as no longer permitting (if the system allows it)', async ({ browser }) => {
  const scope = { country: 'SE', operation: 'withdrawal' }
  const seeded = await seedActiveBasis(scope)
  const D = await ctx(browser, 'PA_D')
  await spaGoto(D.page, `/licensing/operating-bases/${seeded.id}`)
  await D.page.getByRole('button', { name: 'Record a review' }).click()
  const d = D.page.getByRole('dialog', { name: 'Record an operating basis review' })
  await d.getByLabel('Review outcome').selectOption('clear')
  await d.getByLabel('New review due on').fill(daysFromToday(-2))
  await d.getByLabel('Review reference').fill('E2E-REVIEW-PAST')
  await d.getByLabel('Reason code', { exact: true }).fill('e2e_past')
  await clickRefusal(D, d, 'File review')
  const outcome = d.getByRole('status').or(d.getByRole('alert')).first()
  await expect(outcome).toBeVisible({ timeout: 15000 })
  const text = (await outcome.textContent()) ?? ''
  const b = currentBasis(scope)!
  if (/Applied at once/.test(text)) {
    expect(b.reviewDue).toBe(daysFromToday(-2))
    await d.getByRole('button', { name: 'Close', exact: true }).last().click()
    await spaGoto(D.page, `/licensing/operating-bases/${b.id}`)
    await expect(D.page.getByText('Review overdue: no longer permitting').first()).toBeVisible()
  } else {
    // The system refused a past date: record that fact; the basis must be unchanged.
    console.log(`[06 g] past review date not accepted by the server: ${text.slice(0, 160)}`)
    expect(b.reviewDue).toBe(daysFromToday(90))
  }
})

test('(h) step-up prompt: a session older than the critical step-up age is asked for a fresh TOTP before filing', async ({ browser }) => {
  test.setTimeout(300_000)
  const scope = { country: 'SE', operation: 'registration' }
  const D = await ctx(browser, 'PA_D')
  await openTenantBases(D)
  const d = await fillFileDialog(D, { scope, kind: 'no_licence_documented', reviewDue: daysFromToday(60) })
  // run.sh sets STAFF_STEP_UP_CRITICAL_MAX_AGE_SECONDS=60 (the lowest the server allows); age the session past it
  await D.page.waitForTimeout(Number(process.env.E2E_STEPUP_WAIT_MS ?? 70_000))
  const prompted = await submitDialog(D, d, 'File basis', /Pending approval/)
  expect(prompted).toBe(true)
})
