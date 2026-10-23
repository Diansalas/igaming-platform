// Market configuration: the gaps spec 11 left open (slice C3, "Not covered" of docs/plans/lic32-e2e-record.md). Real stack as in 11:
// the built Back Office in Chromium, the real platform-api, a real migrated private database, platform administrators linked to
// distinct Persons (PA_A/PA_A2 share P1, PA_B/PA_B2 share P2, PA_C, PA_D). Synthetic data only; nothing here is a legal approval.
//
//   (a) licence country ceiling WITHDRAWAL: narrowing, one actor, applied at once; DB, REAL resolver, audit
//   (b) BRAND rung disable (narrowing) then enable (widening, two Persons) with the resolver following each step
//   (c) PRODUCT-scoped operation rung: disable (narrowing), withdraw (widening, reference required, two Persons)
//   (d) 24 h approval FRESHNESS: the UI shows the server's state, the server refuses, nothing is written, cancel recovers
//   (e) 72 h request EXPIRY: stale UI approve after expiry, and an untouched expired request
//   (f) STALE UI: Approve clicked after the request was cancelled / executed meanwhile: the server's refusal is rendered, DB unchanged
//   (g) tampered bodies / wrong ids / tenant staff on the real stack: refused, nothing written
//   (h) CONCURRENT approvers on the real platform-api, several rounds
//   (i) PLACEHOLDERS (test.fixme): exact closed codes for precheck refusals, awaiting the C1 merge
//
// Clock control for (d)/(e) is lib/mc_gaps.ts shiftChange (private test database, owner path, see its comment). Each test uses its own
// country / licence so the tests are independent and repeatable on a fresh database (README).
import { expect, test, type Browser } from 'playwright/test'
import { apiApprove, apiFile, apiLogin, syntheticLicenceId, tenantId } from '../lib/api'
import { createLicence, sql } from '../lib/db'
import { decide } from '../lib/licensing'
import {
  ceilingBody, mcBrandId, mcCall, mcCeiling, mcCeilingVersions, mcChange, mcFileAndApprove, mcPolicy, mcResolve, policyBody,
} from '../lib/mc_world'
import { CEILING_TITLE, classHint, fillPolicy, openChangeLink, openLicencePage, openTenantPage, submitMarket } from '../lib/mc_ui'
import {
  auditCounts, ceilingAudit, changeFingerprint, fillScopedOperationPolicy, globalCounts, mcResolveScoped, productPolicy, shiftChange, staleApprove,
} from '../lib/mc_gaps'
import { clickWithStepUp, loginAs, spaGoto, type Session } from '../lib/session'

const ctx = async (browser: Browser, key: string): Promise<Session> => loginAs(await (await browser.newContext()).newPage(), key)
const OPS = ['registration', 'deposit', 'wagering', 'withdrawal']
const staffId = (key: string) => sql(`SELECT id FROM staff_users WHERE email='${key.toLowerCase()}@e2e.invalid'`)

/** Files a widening ceiling enable through the real API as PA_A; returns the change id. */
async function fileCeiling(lic: string, country: string, token?: string): Promise<string> {
  const f = await apiFile(token ?? (await apiLogin('PA_A')), ceilingBody(lic, country, 'enable', `E2E-C3-CEILING-${country}`))
  expect(f.status).toBe(201)
  expect(f.json.executed).toBe(false)
  return f.json.change_request.id as string
}
const approveApi = async (key: string, id: string) => apiApprove(await apiLogin(key), id, 'e2e_c3_ok')

test('(a) ceiling WITHDRAWAL through the UI: narrowing, one actor, applied at once; DB, real resolver and audit', async ({ browser }) => {
  const LIC = syntheticLicenceId(), TEN = tenantId(), CC = 'SE'
  await mcFileAndApprove(ceilingBody(LIC, CC, 'enable', 'E2E-C3-CEILING-SE'))
  await mcFileAndApprove(policyBody({ tenant: TEN, scope: 'tenant', country: CC, kind: 'enable', ref: 'E2E-C3-POLICY-SE' }))
  for (const op of OPS) expect(mcResolve(TEN, CC, op)).toBe('permitted')
  expect(mcCeiling(LIC, CC)).toMatchObject({ state: 'enabled', status: 'active', versions: 1 })

  const D = await ctx(browser, 'PA_D')
  await openLicencePage(D, LIC)
  await D.page.getByRole('button', { name: `File a change for ${CC}` }).click()
  const d = D.page.getByRole('dialog', { name: CEILING_TITLE })
  await expect(d.getByLabel('Country code', { exact: true })).toHaveValue(CC)
  const sel = d.getByLabel('Action', { exact: true })
  await sel.selectOption({ label: (await sel.locator('option', { hasText: /^Withdraw/ }).first().textContent()) as string })
  await d.getByLabel('Reason code', { exact: true }).fill('e2e_ceiling_withdraw')
  await expect(classHint(d)).toContainText('narrowing')
  await submitMarket(D, d, 'File ceiling change', /Applied at once/)
  const id = await openChangeLink(D, d)

  // DB: executed in the filing call by one actor, no approval, append-only second version, the open one is the withdrawn one
  expect(mcChange(id)).toMatchObject({ status: 'executed', widening: false, approvals: 0, kind: 'licence_ceiling_version', requestedBy: staffId('PA_D') })
  expect(mcCeiling(LIC, CC)).toMatchObject({ state: 'disabled', status: 'withdrawn', versions: 2 })
  // the tenant's own policy row is untouched, yet the REAL resolver closes the market for every operation
  expect(mcPolicy({ tenant: TEN, scope: 'tenant', country: CC })).toMatchObject({ state: 'enabled', status: 'active', versions: 1 })
  for (const op of OPS) expect(mcResolve(TEN, CC, op)).toBe('not_permitted_by_licence')
  // audit: requested + executed by the single actor, no approved event; the domain row records before -> after with the reason code
  expect(auditCounts(id)).toEqual({ requested: 1, executed: 1 })
  const vid = sql(`SELECT id FROM licence_country_ceilings WHERE licence_id='${LIC}' AND country_code='${CC}' AND effective_to IS NULL`)
  expect(ceilingAudit(vid)).toBe(`active>withdrawn|e2e_ceiling_withdraw|${staffId('PA_D')}`)
})

test('(b) brand rung through the UI: disable is narrowing and immediate, enable is widening with two distinct Persons, the real resolver follows', async ({ browser }) => {
  const LIC = syntheticLicenceId(), TEN = tenantId(), CC = 'FI', brand = mcBrandId(TEN)
  await mcFileAndApprove(ceilingBody(LIC, CC, 'enable', 'E2E-C3-CEILING-FI'))
  await mcFileAndApprove(policyBody({ tenant: TEN, scope: 'tenant', country: CC, kind: 'enable', ref: 'E2E-C3-POLICY-FI' }))
  const key = { tenant: TEN, scope: 'brand' as const, country: CC, brand }
  expect(mcResolveScoped(TEN, CC, 'deposit', { brand })).toBe('permitted')

  // (1) disable at the brand rung: narrowing, applied at once by one actor
  const D = await ctx(browser, 'PA_D')
  await openTenantPage(D, TEN)
  let d = await fillPolicy(D, { rung: 'brand', brandId: brand, country: CC, action: 'disable', reason: 'e2e_brand_disable' })
  await expect(classHint(d)).toContainText('narrowing')
  await submitMarket(D, d, 'File policy change', /Applied at once/)
  const dis = await openChangeLink(D, d)
  expect(mcChange(dis)).toMatchObject({ status: 'executed', widening: false, approvals: 0 })
  expect(mcPolicy(key)).toMatchObject({ state: 'disabled', status: 'active', versions: 1 })
  expect(mcResolveScoped(TEN, CC, 'deposit', { brand })).toBe('disabled_by_brand')
  expect(mcResolveScoped(TEN, CC, 'deposit')).toBe('permitted') // a query without the brand inherits the tenant rung

  // (2) enable at the brand rung: widening, pending, same-Person twins refused, two distinct Persons execute
  const A = await ctx(browser, 'PA_A')
  await openTenantPage(A, TEN)
  d = await fillPolicy(A, { rung: 'brand', brandId: brand, country: CC, action: 'enable', ref: 'E2E-C3-BRAND-FI', reason: 'e2e_brand_enable' })
  await expect(classHint(d)).toContainText('widening')
  await submitMarket(A, d, 'File policy change', /Pending approval\. Nothing has changed yet/)
  const en = await openChangeLink(A, d)
  expect(mcChange(en)).toMatchObject({ status: 'requested', widening: true, required: 2, approvals: 0, kind: 'operating_policy_version' })
  expect(mcPolicy(key)).toMatchObject({ state: 'disabled', versions: 1 })
  expect(mcResolveScoped(TEN, CC, 'deposit', { brand })).toBe('disabled_by_brand')

  const A2 = await ctx(browser, 'PA_A2')
  await decide(A2, en, 'approve', 'e2e_c3_sameperson', () => expect(A2.page.getByRole('alert').filter({ hasText: 'another account of the same person cannot either' })).toBeVisible())
  const B = await ctx(browser, 'PA_B')
  await decide(B, en, 'approve', 'e2e_c3_one', () => expect(B.page.getByText(/Approval recorded: 1 of 2\. Still pending approval/)).toBeVisible())
  expect(mcResolveScoped(TEN, CC, 'deposit', { brand })).toBe('disabled_by_brand') // one approval changes nothing
  const B2 = await ctx(browser, 'PA_B2')
  await decide(B2, en, 'approve', 'e2e_c3_dup', () => expect(B2.page.getByRole('alert').filter({ hasText: 'You (this person) already approved' })).toBeVisible())
  expect(mcChange(en).approvals).toBe(1)
  const C = await ctx(browser, 'PA_C')
  await decide(C, en, 'approve', 'e2e_c3_two', () => expect(C.page.getByText('Approval recorded and the change was executed.')).toBeVisible())
  expect(mcChange(en)).toMatchObject({ status: 'executed', approvals: 2 })
  expect(mcPolicy(key)).toMatchObject({ state: 'enabled', status: 'active', ref: 'E2E-C3-BRAND-FI', versions: 2 })
  expect(mcResolveScoped(TEN, CC, 'deposit', { brand })).toBe('permitted')
  expect(auditCounts(en)).toEqual({ requested: 1, approved: 2, executed: 1, approve_denied: 1 }) // the A2 same-Person attempt is the one denied event
})

test('(c) product-scoped operation rung through the UI: disable is narrowing and immediate, withdraw is widening (reference required, two Persons)', async ({ browser }) => {
  const LIC = syntheticLicenceId(), TEN = tenantId(), CC = 'NO'
  await mcFileAndApprove(ceilingBody(LIC, CC, 'enable', 'E2E-C3-CEILING-NO'))
  await mcFileAndApprove(policyBody({ tenant: TEN, scope: 'tenant', country: CC, kind: 'enable', ref: 'E2E-C3-POLICY-NO' }))
  const D = await ctx(browser, 'PA_D')
  await openTenantPage(D, TEN)

  let d = await fillScopedOperationPolicy(D, { country: CC, operation: 'deposit', product: 'casino', action: 'disable', reason: 'e2e_product_disable' })
  await expect(classHint(d)).toContainText('narrowing')
  await submitMarket(D, d, 'File policy change', /Applied at once/)
  const dis = await openChangeLink(D, d)
  expect(mcChange(dis)).toMatchObject({ status: 'executed', widening: false, approvals: 0 })
  expect(productPolicy(TEN, CC, 'deposit', 'casino')).toEqual({ state: 'disabled', status: 'active', versions: 1 })
  expect(mcResolveScoped(TEN, CC, 'deposit', { product: 'casino' })).toBe('disabled_by_operation')
  expect(mcResolveScoped(TEN, CC, 'deposit', { product: 'sportsbook' })).toBe('permitted')
  expect(mcResolveScoped(TEN, CC, 'deposit')).toBe('permitted') // a product-less query is not blocked by a product-scoped row
  expect(mcResolveScoped(TEN, CC, 'withdrawal', { product: 'casino' })).toBe('permitted')

  // withdraw the product disable: the UI requires the reference, the class is widening, nothing changes until two distinct Persons approve
  await openTenantPage(D, TEN)
  d = await fillScopedOperationPolicy(D, { country: CC, operation: 'deposit', product: 'casino', action: 'withdraw', reason: 'e2e_product_withdraw' })
  await expect(d.getByRole('note', { name: 'Withdrawal note' })).toContainText('authorization reference is required')
  await expect(d.getByRole('button', { name: 'File policy change', exact: true })).toBeDisabled()
  await d.getByRole('textbox', { name: /^Authorization reference/ }).fill('E2E-C3-PRODUCT-WITHDRAW')
  await expect(classHint(d)).toContainText('widening')
  await submitMarket(D, d, 'File policy change', /Pending approval/)
  const wd = await openChangeLink(D, d)
  expect(mcChange(wd)).toMatchObject({ status: 'requested', widening: true, required: 2, approvals: 0 })
  expect(productPolicy(TEN, CC, 'deposit', 'casino')).toEqual({ state: 'disabled', status: 'active', versions: 1 })
  expect(mcResolveScoped(TEN, CC, 'deposit', { product: 'casino' })).toBe('disabled_by_operation')
  expect((await approveApi('PA_B', wd)).status).toBe(200)
  expect(mcResolveScoped(TEN, CC, 'deposit', { product: 'casino' })).toBe('disabled_by_operation') // one Person is not enough
  expect((await approveApi('PA_B2', wd)).json.code).toBe('LICENSING_CHANGE_ALREADY_DECIDED') // the same Person's second account
  expect((await approveApi('PA_C', wd)).json.executed).toBe(true)
  expect(productPolicy(TEN, CC, 'deposit', 'casino')).toEqual({ state: 'disabled', status: 'withdrawn', versions: 2 })
  expect(mcResolveScoped(TEN, CC, 'deposit', { product: 'casino' })).toBe('permitted')
})

test('(d) 24 h freshness: the UI shows an old approval as such, the server refuses the final approval, nothing is written, cancel recovers', async ({ browser }) => {
  const LIC = syntheticLicenceId(), TEN = tenantId(), CC = 'IS'
  const id = await fileCeiling(LIC, CC)
  expect((await approveApi('PA_B', id)).status).toBe(200)
  shiftChange(id, null, '25 hours') // the first approval is now 25 h old
  const fp = changeFingerprint(id)
  expect(JSON.parse(fp)).toMatchObject({ status: 'requested', approvals: 1 })

  const C = await ctx(browser, 'PA_C')
  await spaGoto(C.page, `/licensing/changes/${id}`)
  // the page renders the SERVER's state: the approval is marked old and the warning is shown
  await expect(C.page.getByText('older than 24 hours', { exact: true })).toBeVisible()
  await expect(C.page.getByText(/An approval older than 24 hours does not count at execution/)).toBeVisible()
  const approve = C.page.locator('[aria-label="Decisions"]').getByRole('button', { name: 'Approve', exact: true })
  await expect(approve).toBeEnabled()
  await approve.click()
  const dlg = C.page.getByRole('dialog', { name: 'Approve licensing change' })
  await dlg.getByLabel('Reason code').fill('e2e_c3_stale')
  const posted = C.page.waitForResponse((r) => r.url().endsWith(`/${id}/approve`) && r.request().method() === 'POST')
  await clickWithStepUp(C, () => dlg.getByRole('button', { name: 'Approve', exact: true }).click(),
    () => expect(C.page.getByRole('alert').filter({ hasText: /An earlier approval is older than 24 hours and no longer counts/ })).toBeVisible({ timeout: 15000 }))
  const resp = await posted
  console.log(`[13 d] stale final approval on the wire: status=${resp.status()} code=${(await resp.json()).code}`)
  expect(resp.status()).toBe(409)
  expect((await resp.json()).code).toBe('LICENSING_CHANGE_APPROVALS_STALE')
  // nothing was written: same status, still one approval, same audit, no ceiling, resolver unchanged
  expect(changeFingerprint(id)).toBe(fp)
  expect(mcCeiling(LIC, CC)).toBeNull()
  expect(mcResolve(TEN, CC, 'deposit')).toBe('not_permitted_by_licence')
  // the request can no longer reach execution: a further distinct Person is refused the same way
  expect((await approveApi('PA_D', id)).json.code).toBe('LICENSING_CHANGE_APPROVALS_STALE')
  expect(changeFingerprint(id)).toBe(fp)
  // recovery path named by the UI: the requester cancels (UI) and may file a new request
  const A = await ctx(browser, 'PA_A')
  await decide(A, id, 'cancel', '', () => expect(A.page.getByText('Cancelled. The request is closed and nothing was changed.')).toBeVisible())
  expect(mcChange(id)).toMatchObject({ status: 'cancelled', approvals: 1 })
  expect(mcCeiling(LIC, CC)).toBeNull()
})

test('(e) 72 h expiry: a stale UI approve after expiry is refused and the expiry is recorded; an untouched expired request shows as expired and cannot be approved', async ({ browser }) => {
  const LIC = syntheticLicenceId(), TEN = tenantId()
  // (1) the page is open while the request is still valid; it expires meanwhile; the stale click is refused by the server
  const id = await fileCeiling(LIC, 'LU')
  expect((await approveApi('PA_B', id)).status).toBe(200)
  const C = await ctx(browser, 'PA_C')
  const alert = await staleApprove(C, id, async () => shiftChange(id, '73 hours', null), /This request is no longer open/)
  await expect(alert).toBeVisible()
  expect(JSON.parse(changeFingerprint(id))).toEqual({ status: 'expired', approvals: 1, audit: { requested: 1, approved: 1, expired: 1 } })
  expect(mcCeiling(LIC, 'LU')).toBeNull()
  expect(mcResolve(TEN, 'LU', 'deposit')).toBe('not_permitted_by_licence')
  // a fresh load (leave the page and come back: the same URL would keep the cached query) shows the server's recorded state
  await spaGoto(C.page, '/licensing/changes')
  await spaGoto(C.page, `/licensing/changes/${id}`)
  await expect(C.page.getByText('Expired', { exact: true }).first()).toBeVisible()
  await expect(C.page.locator('[aria-label="Decisions"]')).toHaveCount(0)
  // no decision of any kind brings it back
  for (const key of ['PA_D']) expect((await approveApi(key, id)).json.code).toBe('LICENSING_CHANGE_NOT_PENDING')
  const rej = await mcCall('POST', `/v1/admin/licensing-changes/${id}/reject`, await apiLogin('PA_D'), { reason_code: 'e2e_late' })
  expect([rej.status, rej.json.code]).toEqual([409, 'LICENSING_CHANGE_NOT_PENDING'])
  expect(JSON.parse(changeFingerprint(id)).audit).toEqual({ requested: 1, approved: 1, expired: 1 })

  // (2) CHARACTERISATION: an expired request nobody has touched still reads status "requested" in the database (expiry is recorded lazily)
  // but the UI marks it expired and disables Approve; the server refuses and then records the expiry
  const id2 = await fileCeiling(LIC, 'LV')
  shiftChange(id2, '73 hours', null)
  expect(mcChange(id2).status).toBe('requested')
  const D = await ctx(browser, 'PA_D')
  await spaGoto(D.page, `/licensing/changes/${id2}`)
  await expect(D.page.getByText('(expired)')).toBeVisible()
  await expect(D.page.locator('[aria-label="Decisions"]').getByRole('button', { name: 'Approve', exact: true })).toBeDisabled()
  const r = await approveApi('PA_D', id2)
  expect([r.status, r.json.code]).toEqual([409, 'LICENSING_CHANGE_NOT_PENDING'])
  expect(JSON.parse(changeFingerprint(id2))).toEqual({ status: 'expired', approvals: 0, audit: { requested: 1, expired: 1 } })
  expect(mcCeiling(LIC, 'LV')).toBeNull()
})

test('(f) STALE UI: Approve clicked after the request was cancelled / executed meanwhile; the server refusal is shown and the database is unchanged', async ({ browser }) => {
  const LIC = syntheticLicenceId()
  // (1) cancelled meanwhile (by the requester, through the API, while PA_B's dialog page stays stale)
  const id = await fileCeiling(LIC, 'BE')
  const B = await ctx(browser, 'PA_B')
  const posted = B.page.waitForResponse((r) => r.url().endsWith(`/${id}/approve`) && r.request().method() === 'POST')
  const alert = await staleApprove(B, id, async () => {
    const c = await mcCall('POST', `/v1/admin/licensing-changes/${id}/cancel`, await apiLogin('PA_A'), {})
    expect(c.status).toBe(200)
  }, /This request is no longer open \(it was executed, rejected, cancelled or expired\)/)
  await expect(alert).toContainText('Nothing was changed.')
  const resp = await posted
  expect([resp.status(), (await resp.json()).code]).toEqual([409, 'LICENSING_CHANGE_NOT_PENDING'])
  expect(JSON.parse(changeFingerprint(id))).toEqual({ status: 'cancelled', approvals: 0, audit: { requested: 1, cancelled: 1 } })
  expect(mcCeiling(LIC, 'BE')).toBeNull()

  // (2) executed meanwhile (two OTHER Persons approve through the API while PA_D's page is stale): no third approval, one ceiling row
  const id2 = await fileCeiling(LIC, 'HR')
  const D = await ctx(browser, 'PA_D')
  const alert2 = await staleApprove(D, id2, async () => {
    expect((await approveApi('PA_B', id2)).status).toBe(200)
    expect((await approveApi('PA_C', id2)).json.executed).toBe(true)
  }, /This request is no longer open/)
  await expect(alert2).toBeVisible()
  expect(JSON.parse(changeFingerprint(id2))).toEqual({ status: 'executed', approvals: 2, audit: { requested: 1, approved: 2, executed: 1 } })
  expect(mcCeiling(LIC, 'HR')).toMatchObject({ state: 'enabled', status: 'active', versions: 1 })
  expect(mcCeilingVersions(LIC, 'HR')).toBe(1)
})

test('(g) tampered bodies, wrong ids and tenant staff on the real stack are refused and write nothing', async () => {
  const LIC = syntheticLicenceId(), CC = 'CZ'
  // tenant staff first, then the platform accounts last, so every token is inside the 60 s step-up window when it is used
  const tenantToks = { T_COMPLIANCE: await apiLogin('T_COMPLIANCE'), T_TENANT_ADMIN: await apiLogin('T_TENANT_ADMIN') }
  const B = await apiLogin('PA_B'), A = await apiLogin('PA_A')
  const id = await fileCeiling(LIC, CC, A)
  const before = { c: globalCounts(), f: changeFingerprint(id) }
  const post = (tok: string, path: string, body: unknown) => mcCall('POST', path, tok, body)
  // a stale/hand-crafted client that sends its own class or approvals count
  for (const extra of [{ widening: false }, { required_approvals: 0 }, { class: 'narrowing' }, { status: 'executed' }]) {
    const r = await post(A, '/v1/admin/licensing-changes', { ...ceilingBody(LIC, 'PL', 'enable', 'E2E-C3-TAMPER'), ...extra })
    expect([r.status, r.json.code]).toEqual([400, 'validation_error'])
  }
  const bad = ceilingBody(LIC, 'PL', 'enable', 'E2E-C3-TAMPER') as any
  bad.payload.widening = false
  expect((await post(A, '/v1/admin/licensing-changes', bad)).status).toBe(400)
  for (const body of [{ reason_code: 'ok', required_approvals: 1 }, { reason_code: 'ok', payload_hash: '0'.repeat(64) }, { reason_code: 'ok', approver_id: staffId('PA_C') }, {}, { reason_code: 'has space' }]) {
    expect((await post(B, `/v1/admin/licensing-changes/${id}/approve`, body)).status).toBe(400)
  }
  // wrong ids
  expect((await post(B, `/v1/admin/licensing-changes/00000000-0000-4000-8000-00000000dead/approve`, { reason_code: 'ok' })).status).toBe(404)
  expect((await post(B, `/v1/admin/licensing-changes/not-a-uuid/approve`, { reason_code: 'ok' })).status).toBe(400)
  // the requester, tenant staff and anonymous
  const self = await post(A, `/v1/admin/licensing-changes/${id}/approve`, { reason_code: 'ok' })
  expect([self.status, self.json.code]).toEqual([409, 'LICENSING_CHANGE_SELF_APPROVAL'])
  for (const tk of Object.values(tenantToks)) {
    const r = await post(tk, `/v1/admin/licensing-changes/${id}/approve`, { reason_code: 'ok' })
    expect(r.status).toBe(403)
    expect(r.json.code).not.toBe('step_up_required') // refused for the ROLE, not for a stale second factor
  }
  expect((await post('', `/v1/admin/licensing-changes/${id}/approve`, { reason_code: 'ok' })).status).toBe(401)
  // the class is the SERVER's: a genuine widening is never executed at once whatever the client believed
  const w = await post(A, '/v1/admin/licensing-changes', ceilingBody(LIC, 'PL', 'enable', 'E2E-C3-WIDENING'))
  expect([w.status, w.json.executed, w.json.change_request.widening, w.json.change_request.required_approvals]).toEqual([201, false, true, 2])
  // exactly what we deliberately filed and the one self-approval attempt's denied audit row changed; everything else is untouched
  const after = globalCounts()
  expect(after.requests).toBe(before.c.requests + 1)
  expect(after.approvals).toBe(before.c.approvals)
  expect(after.ceilings).toBe(before.c.ceilings)
  expect(JSON.parse(changeFingerprint(id))).toEqual({ ...JSON.parse(before.f), audit: { ...JSON.parse(before.f).audit, approve_denied: 1 } })
  expect((await post(A, `/v1/admin/licensing-changes/${w.json.change_request.id}/cancel`, {})).status).toBe(200)
  expect((await post(A, `/v1/admin/licensing-changes/${id}/cancel`, {})).status).toBe(200)
})

test('(h) CONCURRENT approvers on the real platform-api: exactly one execution per round, two Persons, consistent audit', async () => {
  // PA_A (the filer) is logged in LAST so every token is inside the 60 s step-up window while the rounds run
  const keys = ['PA_A2', 'PA_B2', 'PA_D', 'PA_C', 'PA_B', 'PA_A']
  const tok: Record<string, string> = {}
  const started = Date.now()
  for (const k of keys) tok[k] = await apiLogin(k) // sequential: each account has its own TOTP replay rule
  const post = (k: string, id: string) => mcCall('POST', `/v1/admin/licensing-changes/${id}/approve`, tok[k], { reason_code: 'e2e_race' })
  let rounds = 0
  for (; rounds < 6 && Date.now() - started < 40_000; rounds++) { // inside the 60 s step-up window of the tokens above
    const lic = createLicence('E2E-C3-RACE', 'active')
    const f = await mcCall('POST', '/v1/admin/licensing-changes', tok.PA_A, ceilingBody(lic.id, 'MT', 'enable', 'E2E-C3-RACE'))
    expect(f.status).toBe(201)
    const id = f.json.change_request.id as string
    const calls = ['PA_B', 'PA_B2', 'PA_C', 'PA_D', 'PA_B', 'PA_A', 'PA_A2']
    const res = await Promise.all(calls.map((k) => post(k, id)))
    const ok = res.filter((r) => r.status === 200)
    const executed = ok.filter((r) => r.json.executed === true)
    for (const r of res.filter((x) => x.status !== 200)) {
      expect(r.status).toBe(409)
      expect(['LICENSING_CHANGE_NOT_PENDING', 'LICENSING_CHANGE_ALREADY_DECIDED', 'LICENSING_CHANGE_SELF_APPROVAL']).toContain(r.json.code)
    }
    expect([ok.length, executed.length], `round ${rounds}: ${JSON.stringify(res.map((r) => [r.status, r.json?.code]))}`).toEqual([2, 1])
    expect(mcChange(id)).toMatchObject({ status: 'executed', approvals: 2 })
    expect(Number(sql(`SELECT count(DISTINCT approver_person_id) FROM licensing_change_approvals WHERE request_id='${id}'`))).toBe(2)
    expect(mcCeilingVersions(lic.id, 'MT')).toBe(1)
    const a = auditCounts(id)
    expect([a.requested, a.approved, a.executed]).toEqual([1, 2, 1])
    expect(a.approve_denied ?? 0).toBeLessThanOrEqual(2) // PA_A / PA_A2 reaching the request before it executed are denied-audited; after, NOT_PENDING
  }
  console.log(`[13 h] concurrent approver rounds completed: ${rounds}`)
  expect(rounds).toBeGreaterThanOrEqual(3)
})

// ---- (i) PLACEHOLDERS: the exact closed codes of the precheck refusals (slice C1 adds them) ----------------------------------------
// Today the precheck refusal is 409 LICENSING_CHANGE_REFUSED with no check_codes (lic32 finding 8). C1 is adding closed refusal codes;
// these tests are written AFTER that merge. They are fixme (reported as skipped), never silently dropped.
test.describe('[awaiting C1 merge] exact closed refusal codes for precheck failures', () => {
  test.fixme('[awaiting C1 merge] tenant policy enable with no ceiling and no operating basis: wire check_codes equals the closed code and the UI shows its exact wording', async () => {
    // intended: POST operating_policy_version enable for a country with no ceiling/basis -> 409, check_codes == [<C1 code>], UI alert == mapped sentence, nothing stored
  })
  test.fixme('[awaiting C1 merge] unassigned (unknown) country code on a ceiling or policy filing: exact closed code, UI wording, nothing stored', async () => {})
  test.fixme('[awaiting C1 merge] tenant jurisdiction configuration with a currency that is not an active asset: exact closed code, UI wording, nothing stored', async () => {})
  test.fixme('[awaiting C1 merge] brand-rung policy naming a brand of another tenant: exact closed code (and still a not-found shape for tenant sessions), nothing stored', async () => {})
})
