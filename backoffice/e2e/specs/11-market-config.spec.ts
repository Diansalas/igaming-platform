// Market configuration filing through the REAL stack (lic32): the lic31 Back Office flows (licence country ceiling, operating-country
// policy, tenant jurisdiction configuration, jurisdiction creation, change detail) driven in the built UI as platform administrators
// linked to distinct Persons, against the real platform-api and a real migrated private database. Synthetic data only; nothing here is
// a legal approval of any country. Ground truth is read from the database (owner role) and the market RESOLVER is the real Go code
// (tools/mcprobe, runtime role). Each test uses its own country so tests are independent (the database is fresh per run, README).
// Accounts: PA_A/PA_A2 share Person P1; PA_B/PA_B2 share P2; PA_C, PA_D have their own.
import { expect, test, type Browser, type Locator } from 'playwright/test'
import { apiApprove, apiLogin, jurisdictionId, syntheticLicenceId, tenantId } from '../lib/api'
import { changeStatus, createLicence, sql } from '../lib/db'
import { decide } from '../lib/licensing'
import {
  ceilingBody, mcBrandId, mcCall, mcCeiling, mcChange, mcChangeCount, mcCreateJurisdictionApi, mcFileAndApprove, mcJurisdictionByCode,
  mcPolicy, mcPolicyAnyCount, mcPolicyVersions, mcResolve, policyBody,
} from '../lib/mc_world'
import { classHint, fillCeiling, fillPolicy, openChangeLink, openLicencePage, openTenantPage, submitMarket, submitRefused } from '../lib/mc_ui'
import { loginAs, spaGoto, type Session } from '../lib/session'
import { sqlT as sqlTq } from '../lib/wd_db'
import { loadWorld } from '../lib/world'

const ctx = async (browser: Browser, key: string): Promise<Session> => loginAs(await (await browser.newContext()).newPage(), key)
const OPS = ['registration', 'deposit', 'wagering', 'withdrawal']
const rnd = () => Math.random().toString(16).slice(2, 8).toUpperCase()

test('(a) enable a country ceiling: widening, pending approval, requester and same-Person refused, two distinct Persons approve, row exists, resolver outcome changes', async ({ browser, request }) => {
  const LIC = syntheticLicenceId(), TEN = tenantId(), CC = 'IE'
  expect(mcCeiling(LIC, CC)).toBeNull()
  const before = mcResolve(TEN, CC, 'deposit')
  expect(before).not.toBe('permitted')
  const A = await ctx(browser, 'PA_A')
  await openLicencePage(A, LIC)
  const d = await fillCeiling(A, { country: CC, action: 'enable', ref: 'E2E-SYNTHETIC-CEILING-IE', reason: 'e2e_ceiling_enable' })
  await expect(classHint(d)).toContainText('widening')
  await submitMarket(A, d, 'File ceiling change', /Pending approval\. Nothing has changed yet/)
  const id = await openChangeLink(A, d)
  const row = mcChange(id)
  expect(row).toMatchObject({ status: 'requested', widening: true, required: 2, approvals: 0, kind: 'licence_ceiling_version' })
  // BEFORE approval nothing exists and the resolver is unchanged
  expect(mcCeiling(LIC, CC)).toBeNull()
  expect(mcResolve(TEN, CC, 'deposit')).toBe(before)

  // requester: UI hint (disabled) AND the server (409 LICENSING_CHANGE_SELF_APPROVAL with the requester's own token)
  await expect(A.page.locator('[aria-label="Decisions"]').getByRole('button', { name: 'Approve', exact: true })).toBeDisabled()
  const self = await request.post(`${loadWorld().apiUrl}/v1/admin/licensing-changes/${id}/approve`, { headers: { Authorization: `Bearer ${A.accessToken()}` }, data: { reason_code: 'e2e_self' } })
  expect(self.status()).toBe(409)
  expect((await self.json()).code).toBe('LICENSING_CHANGE_SELF_APPROVAL')
  // the second account of the requester's Person: the UI offers Approve, the server refuses and the refusal is rendered
  const A2 = await ctx(browser, 'PA_A2')
  await decide(A2, id, 'approve', 'e2e_sameperson', () => expect(A2.page.getByRole('alert').filter({ hasText: 'another account of the same person cannot either' })).toBeVisible())
  expect(mcChange(id).approvals).toBe(0)
  expect(mcCeiling(LIC, CC)).toBeNull()

  const B = await ctx(browser, 'PA_B')
  await decide(B, id, 'approve', 'e2e_ceiling_one', () => expect(B.page.getByText(/Approval recorded: 1 of 2\. Still pending approval/)).toBeVisible())
  expect(mcChange(id)).toMatchObject({ status: 'requested', approvals: 1 })
  expect(mcCeiling(LIC, CC)).toBeNull()
  expect(mcResolve(TEN, CC, 'deposit')).toBe(before)
  const B2 = await ctx(browser, 'PA_B2')
  await decide(B2, id, 'approve', 'e2e_dup_person', () => expect(B2.page.getByRole('alert').filter({ hasText: 'You (this person) already approved' })).toBeVisible())
  expect(mcChange(id).approvals).toBe(1)

  const C = await ctx(browser, 'PA_C')
  await decide(C, id, 'approve', 'e2e_ceiling_two', () => expect(C.page.getByText('Approval recorded and the change was executed.')).toBeVisible())
  expect(mcChange(id)).toMatchObject({ status: 'executed', approvals: 2 })
  // the row exists, as filed
  expect(mcCeiling(LIC, CC)).toMatchObject({ state: 'enabled', status: 'active', ref: 'E2E-SYNTHETIC-CEILING-IE', versions: 1 })
  // the read API agrees
  const api = await mcCall('GET', `/v1/admin/licence-country-ceilings?licence_id=${LIC}&country_code=${CC}&current=true`, C.accessToken())
  expect(api.status).toBe(200)
  expect(api.json.total).toBe(1)
  expect(api.json.items[0]).toMatchObject({ country_code: CC, state: 'enabled', status: 'active' })
  // the real resolver: a ceiling alone does not open the country for the tenant (a tenant rung is still needed): the outcome changed from
  // "licence does not permit" to "tenant has not configured it"
  const after = mcResolve(TEN, CC, 'deposit')
  console.log(`[11 a] resolver ${CC}/deposit: before=${before} after=${after}`)
  expect(after).not.toBe(before)
  expect(after).toBe('not_configured')
  expect(after).not.toBe('permitted')
})

test('(b) disable a ceiling: narrowing, applied at once by a single actor', async ({ browser }) => {
  const lic = createLicence('E2E-CEILDIS', 'active'), CC = 'DK'
  await mcFileAndApprove(ceilingBody(lic.id, CC, 'enable', 'E2E-SYNTHETIC-CEILING-DK'))
  expect(mcCeiling(lic.id, CC)).toMatchObject({ state: 'enabled', status: 'active', versions: 1 })
  const D = await ctx(browser, 'PA_D')
  await openLicencePage(D, lic.id)
  // the per-row entry of the current version carries the country over
  await D.page.getByRole('button', { name: `File a change for ${CC}` }).click()
  const d = D.page.getByRole('dialog', { name: 'File a licence country ceiling change' })
  await expect(d.getByLabel('Country code', { exact: true })).toHaveValue(CC)
  await d.getByLabel('Action', { exact: true }).selectOption({ label: await d.getByLabel('Action', { exact: true }).locator('option', { hasText: /^Disable/ }).first().textContent() as string })
  await d.getByLabel('Reason code', { exact: true }).fill('e2e_ceiling_disable')
  await expect(classHint(d)).toContainText('narrowing')
  await submitMarket(D, d, 'File ceiling change', /Applied at once/)
  const id = await openChangeLink(D, d)
  const row = mcChange(id)
  expect(row.status).toBe('executed')
  expect(row.widening).toBe(false)
  expect(row.required).toBeLessThanOrEqual(1)
  expect(row.approvals).toBe(0)
  expect(changeStatus(id)).toBe('executed')
  expect(mcCeiling(lic.id, CC)).toMatchObject({ state: 'disabled', status: 'active', versions: 2 })
})

test('(c) operating-country policy: refused while no ceiling or basis exists (UI wording + server code), accepted through the governed flow after the ceiling; operation rung narrows; brand-rung withdraw needs a reference', async ({ browser }) => {
  const LIC = syntheticLicenceId(), TEN = tenantId(), CC = 'NL'
  const tenantKey = { tenant: TEN, scope: 'tenant' as const, country: CC }
  const policyChanges = () => mcChangeCount('operating_policy_version')
  expect(mcCeiling(LIC, CC)).toBeNull()
  const A = await ctx(browser, 'PA_A')
  await openTenantPage(A, TEN)

  // (1) no ceiling and no operating basis: refused at filing (the precheck), nothing stored, no change request
  const n0 = policyChanges()
  let d = await fillPolicy(A, { rung: 'tenant', country: CC, action: 'enable', ref: 'E2E-SYNTHETIC-POLICY-NL', reason: 'e2e_policy_enable' })
  await expect(classHint(d)).toContainText('widening')
  const posted = A.page.waitForResponse((r) => r.url().endsWith('/v1/admin/licensing-changes') && r.request().method() === 'POST')
  await submitRefused(A, d, 'File policy change', /The change was refused by the domain rules/)
  const resp = await posted
  const body = await resp.json()
  console.log(`[11 c] precheck refusal on the wire: status=${resp.status()} code=${body.code} check_codes=${JSON.stringify(body.check_codes ?? null)}`)
  expect(resp.status()).toBe(409)
  expect(body.code).toBe('LICENSING_CHANGE_REFUSED')
  expect(JSON.stringify(body)).not.toMatch(/E2E-SYNTHETIC-POLICY-NL/) // the server does not echo the operator-supplied reference
  expect(policyChanges()).toBe(n0)
  expect(mcPolicyAnyCount(TEN, CC)).toBe(0)
  expect(mcResolve(TEN, CC, 'deposit')).toBe('not_permitted_by_licence')
  await d.getByRole('button', { name: 'Cancel', exact: true }).click()

  // (2) the ceiling now exists, through the governed two-approver flow
  await mcFileAndApprove(ceilingBody(LIC, CC, 'enable', 'E2E-SYNTHETIC-CEILING-NL'))
  expect(mcCeiling(LIC, CC)).toMatchObject({ state: 'enabled', status: 'active' })
  expect(mcResolve(TEN, CC, 'deposit')).toBe('not_configured')

  // (3) the same filing is now accepted as a pending (widening) request; nothing is stored until two distinct Persons approve
  d = await fillPolicy(A, { rung: 'tenant', country: CC, action: 'enable', ref: 'E2E-SYNTHETIC-POLICY-NL', reason: 'e2e_policy_enable' })
  await submitMarket(A, d, 'File policy change', /Pending approval\. Nothing has changed yet/)
  const id = await openChangeLink(A, d)
  expect(mcChange(id)).toMatchObject({ status: 'requested', widening: true, required: 2, approvals: 0, kind: 'operating_policy_version' })
  expect(mcPolicy(tenantKey)).toBeNull()
  expect(mcResolve(TEN, CC, 'deposit')).toBe('not_configured')
  const B = await ctx(browser, 'PA_B')
  await decide(B, id, 'approve', 'e2e_policy_one', () => expect(B.page.getByText(/Approval recorded: 1 of 2\. Still pending approval/)).toBeVisible())
  expect(mcPolicy(tenantKey)).toBeNull()
  const C = await ctx(browser, 'PA_C')
  await decide(C, id, 'approve', 'e2e_policy_two', () => expect(C.page.getByText('Approval recorded and the change was executed.')).toBeVisible())
  expect(mcPolicy(tenantKey)).toMatchObject({ state: 'enabled', status: 'active', ref: 'E2E-SYNTHETIC-POLICY-NL', versions: 1 })
  expect(mcResolve(TEN, CC, 'deposit')).toBe('permitted')
  const read = await mcCall('GET', `/v1/admin/operating-country-policies?tenant_id=${TEN}&country_code=${CC}&current=true`, C.accessToken())
  expect(read.status).toBe(200)
  expect(read.json.items.map((p: any) => `${p.scope}:${p.state}:${p.status}`)).toEqual(['tenant:enabled:active'])

  // (4) operation rung: a disable is a narrowing and is applied at once by one actor; only that operation closes
  const D = await ctx(browser, 'PA_D')
  await openTenantPage(D, TEN)
  d = await fillPolicy(D, { rung: 'operation', operation: 'deposit', country: CC, action: 'disable', reason: 'e2e_op_disable' })
  await expect(classHint(d)).toContainText('narrowing')
  await submitMarket(D, d, 'File policy change', /Applied at once/)
  const opId = await openChangeLink(D, d)
  expect(mcChange(opId)).toMatchObject({ status: 'executed', widening: false })
  expect(mcPolicy({ ...tenantKey, scope: 'operation', operation: 'deposit' })).toMatchObject({ state: 'disabled', status: 'active' })
  expect(mcResolve(TEN, CC, 'deposit')).toBe('disabled_by_operation')
  expect(mcResolve(TEN, CC, 'registration')).toBe('permitted')

  // (5) brand rung: seed a brand-level disable (narrowing, immediate), then withdrawing it widens and REQUIRES a reference
  const brand = mcBrandId(TEN)
  await mcFileAndApprove(policyBody({ tenant: TEN, scope: 'brand', country: CC, kind: 'disable', brand }))
  const brandKey = { ...tenantKey, scope: 'brand' as const, brand }
  expect(mcPolicy(brandKey)).toMatchObject({ state: 'disabled', status: 'active', versions: 1 })
  await openTenantPage(D, TEN)
  d = await fillPolicy(D, { rung: 'brand', brandId: brand, country: CC, action: 'withdraw', reason: 'e2e_brand_withdraw' })
  await expect(d.getByRole('textbox', { name: /^Authorization reference \(operator-supplied, required\)/ })).toBeVisible()
  await expect(d.getByRole('note', { name: 'Withdrawal note' })).toContainText('authorization reference is required')
  await expect(d.getByRole('button', { name: 'File policy change', exact: true })).toBeDisabled() // the UI blocks it first
  // the SERVER refuses the same filing without a reference (direct call with the session's token) and stores nothing
  const nB = policyChanges(), vB = mcPolicyVersions(brandKey)
  const refused = await mcCall('POST', '/v1/admin/licensing-changes', D.accessToken(), policyBody({ tenant: TEN, scope: 'brand', country: CC, kind: 'withdraw', brand, ref: null }))
  console.log(`[11 c] brand withdraw without reference on the wire: status=${refused.status} code=${refused.json?.code}`)
  expect(refused.status).toBe(400)
  expect(refused.json.code).toBe('validation_error')
  expect(policyChanges()).toBe(nB)
  expect(mcPolicyVersions(brandKey)).toBe(vB)
  // with a reference it is accepted as a pending (widening) request
  await d.getByRole('textbox', { name: /^Authorization reference/ }).fill('E2E-SYNTHETIC-BRAND-WITHDRAW')
  await expect(classHint(d)).toContainText('widening')
  await submitMarket(D, d, 'File policy change', /Pending approval/)
  const wId = await openChangeLink(D, d)
  expect(mcChange(wId)).toMatchObject({ status: 'requested', widening: true, required: 2 })
  expect(mcPolicy(brandKey)).toMatchObject({ state: 'disabled', status: 'active', versions: 1 }) // unchanged until approved
  const cancel = await mcCall('POST', `/v1/admin/licensing-changes/${wId}/cancel`, D.accessToken(), { reason_code: 'e2e_tidy' })
  expect(cancel.status).toBe(200)
})

// The UI class hint ("This looks like a widening change" / "narrowing") compared with the database's own classification (licensing_change_requests.widening).
const hintClass = async (d: Locator) => {
  const t = (await classHint(d).textContent()) ?? ''
  return /widening change/.test(t) ? 'widening' : /narrowing change/.test(t) ? 'narrowing' : 'other'
}

test('(d) tenant jurisdiction configuration set/close: the UI class hint agrees with the database classification for every step', async ({ browser }) => {
  const TEN = tenantId(), code = `E2E-MCJ-${rnd()}`
  expect((await mcCreateJurisdictionApi(code, 'E2E synthetic jurisdiction for configuration')).status).toBe(201)
  const J = mcJurisdictionByCode(code)
  const cur = () => sqlTcfg(TEN, J)
  const A = await ctx(browser, 'PA_A')
  const agree: string[] = []

  async function file(s: Session, o: { action: 'set' | 'close'; currencies?: string; payments?: string; geo?: string; reason: string; expectOutcome: RegExp; expectInForce: boolean }) {
    await spaGoto(s.page, '/licensing/jurisdictions')
    await openTenantPage(s, TEN)
    await s.page.getByRole('button', { name: 'File a jurisdiction configuration change' }).click()
    const d = s.page.getByRole('dialog', { name: 'File a tenant jurisdiction configuration change' })
    await d.getByLabel('Action', { exact: true }).selectOption(o.action)
    await d.getByLabel('Jurisdiction', { exact: true }).selectOption(J)
    await expect(d.getByRole('note', { name: 'Version in force' })).toContainText(o.expectInForce ? 'A version is in force' : 'No version is in force')
    if (o.action === 'set') {
      await d.getByLabel('Allowed currencies', { exact: true }).fill(o.currencies ?? '')
      await d.getByLabel('Allowed payment methods', { exact: true }).fill(o.payments ?? '')
      await d.getByLabel('Geo block list', { exact: true }).fill(o.geo ?? '')
    }
    await d.getByLabel('Reason code', { exact: true }).fill(o.reason)
    const hint = await hintClass(d)
    await submitMarket(s, d, 'File configuration change', o.expectOutcome)
    const id = await openChangeLink(s, d)
    const db = mcChange(id)
    const dbClass = db.widening ? 'widening' : 'narrowing'
    agree.push(`${o.reason}: hint=${hint} db=${dbClass} (required=${db.required}, status=${db.status})`)
    expect(hint).toBe(dbClass)
    return { id, db }
  }
  const approve = async (id: string) => {
    for (const k of ['PA_B', 'PA_C']) expect((await apiApprove(await apiLogin(k), id)).status).toBe(200)
    expect(mcChange(id).status).toBe('executed')
  }

  // 1. first version: widening (no version in force), pending until two distinct Persons approve
  let r = await file(A, { action: 'set', currencies: 'EUR, USD', payments: 'card, bank_transfer', geo: 'DE, FR', reason: 'e2e_cfg_first', expectOutcome: /Pending approval/, expectInForce: false })
  expect(r.db).toMatchObject({ status: 'requested', widening: true, required: 2, kind: 'tenant_jurisdiction_config_version' })
  expect(cur()).toBeNull()
  await approve(r.id)
  expect(cur()).toEqual({ currencies: ['EUR', 'USD'], methods: ['card', 'bank_transfer'], geo: ['DE', 'FR'] })

  // 2. remove a currency: narrowing, applied at once by one actor
  const D = await ctx(browser, 'PA_D')
  r = await file(D, { action: 'set', currencies: 'EUR', payments: 'card, bank_transfer', geo: 'DE, FR', reason: 'e2e_cfg_rm_cur', expectOutcome: /Applied at once/, expectInForce: true })
  expect(r.db).toMatchObject({ status: 'executed', widening: false })
  expect(cur()).toEqual({ currencies: ['EUR'], methods: ['card', 'bank_transfer'], geo: ['DE', 'FR'] })

  // 3. add a currency back: widening
  r = await file(A, { action: 'set', currencies: 'EUR, USD', payments: 'card, bank_transfer', geo: 'DE, FR', reason: 'e2e_cfg_add_cur', expectOutcome: /Pending approval/, expectInForce: true })
  expect(r.db).toMatchObject({ status: 'requested', widening: true, required: 2 })
  expect(cur()).toEqual({ currencies: ['EUR'], methods: ['card', 'bank_transfer'], geo: ['DE', 'FR'] })
  await approve(r.id)
  expect(cur()!.currencies).toEqual(['EUR', 'USD'])

  // 4. remove a geo block: widening
  r = await file(A, { action: 'set', currencies: 'EUR, USD', payments: 'card, bank_transfer', geo: 'DE', reason: 'e2e_cfg_rm_geo', expectOutcome: /Pending approval/, expectInForce: true })
  expect(r.db).toMatchObject({ status: 'requested', widening: true, required: 2 })
  await approve(r.id)
  expect(cur()!.geo).toEqual(['DE'])

  // 5. add a geo block: narrowing, applied at once
  r = await file(D, { action: 'set', currencies: 'EUR, USD', payments: 'card, bank_transfer', geo: 'DE, FR', reason: 'e2e_cfg_add_geo', expectOutcome: /Applied at once/, expectInForce: true })
  expect(r.db).toMatchObject({ status: 'executed', widening: false })
  expect(cur()!.geo).toEqual(['DE', 'FR'])

  // 6. close: narrowing, applied at once
  r = await file(D, { action: 'close', reason: 'e2e_cfg_close', expectOutcome: /Applied at once/, expectInForce: true })
  expect(r.db).toMatchObject({ status: 'executed', widening: false })
  expect(cur()).toBeNull()
  console.log(`[11 d] class hint vs database classification:\n  ${agree.join('\n  ')}`)
})

/** The open configuration version of (tenant, jurisdiction) as plain arrays; null when none is in force. */
function sqlTcfg(tenant: string, jur: string): { currencies: string[]; methods: string[]; geo: string[] } | null {
  const row = sqlTq(tenant, `SELECT allowed_currencies::text||'|'||allowed_payment_methods::text||'|'||geo_block_list::text FROM tenant_jurisdiction_configs WHERE tenant_id='${tenant}' AND jurisdiction_id='${jur}' AND effective_to IS NULL`)
  if (!row) return null
  const [c, m, g] = row.split('|').map((x) => JSON.parse(x) as string[])
  return { currencies: c, methods: m, geo: g }
}

test('(e) jurisdiction creation: duplicate code refused; creation is single-actor by server design (governance finding: a jurisdiction row grants no permission)', async ({ browser }) => {
  const TEN = tenantId(), LIC = syntheticLicenceId(), code = `E2E-MCJ-${rnd()}`
  const counts = () => ({
    jurisdictions: Number(sql(`SELECT count(*) FROM jurisdictions`)),
    licences: Number(sql(`SELECT count(*) FROM licences`)),
    ceilings: Number(sql(`SELECT count(*) FROM licence_country_ceilings`)),
    changes: Number(sql(`SELECT count(*) FROM licensing_change_requests`)),
  })
  const resolverBefore = OPS.map((o) => mcResolve(TEN, 'FR', o))
  const before = counts()
  const A = await ctx(browser, 'PA_A')
  await spaGoto(A.page, '/licensing/jurisdictions')
  await A.page.getByRole('button', { name: 'Create a jurisdiction' }).click()
  const d = A.page.getByRole('dialog', { name: 'Create a jurisdiction' })
  await expect(d.getByRole('note', { name: 'Jurisdiction note' })).toContainText('NOT four-eyes gated')
  await d.getByLabel('Code', { exact: true }).fill(code)
  await d.getByLabel('Name', { exact: true }).fill('E2E synthetic registry record')
  await d.getByLabel('Reason code', { exact: true }).fill('e2e_jur_create')
  const posted = A.page.waitForResponse((r) => r.url().endsWith('/v1/admin/jurisdictions') && r.request().method() === 'POST')
  const { clickWithStepUp } = await import('../lib/session')
  await clickWithStepUp(A, () => d.getByRole('button', { name: 'Create jurisdiction', exact: true }).click(), () => expect(d.getByRole('status').filter({ hasText: 'Jurisdiction recorded.' })).toBeVisible({ timeout: 15000 }))
  expect((await posted).status()).toBe(201)

  // applied at once by ONE actor: the row exists, no licensing change request was filed, one audit row with a NULL tenant
  const id = mcJurisdictionByCode(code)
  expect(id).toMatch(/^[0-9a-f-]{36}$/)
  expect(counts()).toEqual({ ...before, jurisdictions: before.jurisdictions + 1 })
  expect(sql(`SELECT count(*) FROM audit_log WHERE action='jurisdiction_registry.jurisdiction_created' AND target_id='${id}' AND tenant_id IS NULL`)).toBe('1')
  expect(sql(`SELECT actor_id FROM audit_log WHERE action='jurisdiction_registry.jurisdiction_created' AND target_id='${id}'`)).toBe(sql(`SELECT id FROM staff_users WHERE email='pa_a@e2e.invalid'`))
  // it grants NO market permission and creates no licence: no ceiling, no licence, and the real resolver is unchanged
  expect(sql(`SELECT count(*) FROM licences WHERE jurisdiction_id='${id}'`)).toBe('0')
  expect(OPS.map((o) => mcResolve(TEN, 'FR', o))).toEqual(resolverBefore)
  expect(resolverBefore.every((o) => o !== 'permitted')).toBe(true)
  console.log('[11 e] FINDING (governance gap, DECISION PENDING): POST /v1/admin/jurisdictions is applied at once by a single platform administrator (no maker-checker); it grants no permission by itself (verified: no licence, no ceiling, resolver unchanged), but a market owner can add jurisdiction rows without a second person.')
  await d.getByRole('button', { name: 'Close', exact: true }).last().click()

  // duplicate code: refused by the server, rendered, nothing stored
  await A.page.getByRole('button', { name: 'Create a jurisdiction' }).click()
  const d2 = A.page.getByRole('dialog', { name: 'Create a jurisdiction' })
  await d2.getByLabel('Code', { exact: true }).fill(code)
  await d2.getByLabel('Name', { exact: true }).fill('E2E duplicate attempt')
  await d2.getByLabel('Reason code', { exact: true }).fill('e2e_jur_dup')
  const dup = A.page.waitForResponse((r) => r.url().endsWith('/v1/admin/jurisdictions') && r.request().method() === 'POST')
  await submitRefused(A, d2, 'Create jurisdiction', /rejected the request as invalid/)
  const dr = await dup
  const db = await dr.json()
  console.log(`[11 e] duplicate code on the wire: status=${dr.status()} code=${db.code}`)
  expect(dr.status()).toBe(400)
  expect(db.code).toBe('validation_error')
  expect(counts()).toEqual({ ...before, jurisdictions: before.jurisdictions + 1 })
  expect(sql(`SELECT count(*) FROM jurisdictions WHERE code='${code}'`)).toBe('1')
  expect(sql(`SELECT name FROM jurisdictions WHERE code='${code}'`)).toBe('E2E synthetic registry record')
  void LIC
})

test('(f) tenant staff: Licensing nav hidden, UI denial, server 403 on platform-only filing and reads, nothing stored; own-tenant policy filing by compliance is characterised', async ({ browser }) => {
  const TEN = tenantId(), LIC = syntheticLicenceId(), CC = 'ES'
  const w = loadWorld()
  const before = { changes: Number(sql(`SELECT count(*) FROM licensing_change_requests`)), jur: Number(sql(`SELECT count(*) FROM jurisdictions`)), ceilings: Number(sql(`SELECT count(*) FROM licence_country_ceilings`)) }
  for (const key of ['T_TENANT_ADMIN', 'T_COMPLIANCE']) {
    const s = await ctx(browser, key)
    await expect(s.page.getByRole('navigation').getByRole('link', { name: 'Licensing' })).toHaveCount(0)
    for (const path of [`/licensing/licences/${LIC}`, `/licensing/tenants/${TEN}`, '/licensing/jurisdictions']) {
      await spaGoto(s.page, path)
      await expect(s.page.getByText('Your role does not have access to this section.')).toBeVisible()
      await expect(s.page.getByRole('button', { name: /File a|Create a jurisdiction/ })).toHaveCount(0)
    }
    const tok = s.accessToken()
    const ceiling = await mcCall('POST', '/v1/admin/licensing-changes', tok, ceilingBody(LIC, CC, 'enable', 'E2E-SYNTHETIC-TENANT-TRY'))
    expect(ceiling.status, `${key} ceiling filing`).toBe(403)
    const jur = await mcCall('POST', '/v1/admin/jurisdictions', tok, { code: `E2E-TENANT-${rnd()}`, name: 'tenant attempt', reason_code: 'e2e_tenant' })
    expect(jur.status, `${key} jurisdiction creation`).toBe(403)
    expect((await mcCall('GET', `/v1/admin/licence-country-ceilings?licence_id=${LIC}`, tok)).status).toBe(403)
    expect((await mcCall('GET', '/v1/admin/jurisdictions', tok)).status).toBe(403)
    // a tenant-rung policy for a country with no ceiling: refused in every case and nothing is stored (characterised, not assumed)
    const pol = await mcCall('POST', '/v1/admin/licensing-changes', tok, policyBody({ tenant: TEN, scope: 'tenant', country: CC, kind: 'enable', ref: 'E2E-SYNTHETIC-TENANT-TRY' }))
    console.log(`[11 f] ${key}: ceiling=${ceiling.status} jurisdiction=${jur.status} tenant-rung policy (no ceiling)=${pol.status} ${pol.json?.code ?? ''}`)
    expect(pol.status).toBeGreaterThanOrEqual(400)
    expect(pol.status).toBeLessThan(500)
  }
  // anonymous
  expect((await mcCall('POST', '/v1/admin/licensing-changes', '', ceilingBody(LIC, CC, 'enable', 'x'))).status).toBe(401)
  expect(mcCeiling(LIC, CC)).toBeNull()
  expect(mcPolicyAnyCount(TEN, CC)).toBe(0)
  expect({ changes: Number(sql(`SELECT count(*) FROM licensing_change_requests`)), jur: Number(sql(`SELECT count(*) FROM jurisdictions`)), ceilings: Number(sql(`SELECT count(*) FROM licence_country_ceilings`)) }).toEqual(before)

  // CHARACTERISATION (not a denial): the server lets tenant staff holding the tenant-policy permission file a tenant-rung policy for their OWN
  // tenant (lic5 design; the Back Office section stays platform-only). With a ceiling in place, tenant compliance's filing is accepted as a
  // PENDING widening request: it stores no policy and needs two platform approvals. Cancelled below by the requester.
  const SK = 'SK'
  await mcFileAndApprove(ceilingBody(LIC, SK, 'enable', 'E2E-SYNTHETIC-CEILING-SK'))
  const comp = await apiLogin('T_COMPLIANCE')
  const filed = await mcCall('POST', '/v1/admin/licensing-changes', comp, policyBody({ tenant: TEN, scope: 'tenant', country: SK, kind: 'enable', ref: 'E2E-SYNTHETIC-TENANT-FILED' }))
  console.log(`[11 f] FINDING: tenant compliance filing a tenant-rung policy for its own tenant (ceiling present): status=${filed.status} executed=${filed.json?.executed}`)
  expect(filed.status).toBe(201)
  expect(filed.json.executed).toBe(false)
  expect(mcChange(filed.json.change_request.id)).toMatchObject({ status: 'requested', widening: true, required: 2 })
  expect(mcPolicyAnyCount(TEN, SK)).toBe(0)
  expect(mcResolve(TEN, SK, 'deposit')).toBe('not_configured')
  expect((await mcCall('POST', `/v1/admin/licensing-changes/${filed.json.change_request.id}/cancel`, comp, { reason_code: 'e2e_tidy' })).status).toBe(200)
  void w
})

test('(g) the change detail page renders the three kinds in words, with the approver counts the database holds', async ({ browser }) => {
  const TEN = tenantId(), LIC = syntheticLicenceId(), CC = 'AT', brand = mcBrandId(tenantId())
  const code = `E2E-MCJ-${rnd()}`
  expect((await mcCreateJurisdictionApi(code, 'E2E synthetic jurisdiction for the detail page')).status).toBe(201)
  const J = mcJurisdictionByCode(code)
  const tokA = await apiLogin('PA_A')
  const file = async (body: unknown) => {
    const r = await mcCall('POST', '/v1/admin/licensing-changes', tokA, body)
    expect(r.status).toBe(201)
    return r.json.change_request.id as string
  }
  const ceiling = await file(ceilingBody(LIC, CC, 'enable', 'E2E-SYNTHETIC-CEILING-AT'))
  const policy = await file(policyBody({ tenant: TEN, scope: 'brand', country: CC, kind: 'withdraw', brand, ref: 'E2E-SYNTHETIC-DETAIL' }))
  const config = await file({ kind: 'tenant_jurisdiction_config_version', reason_code: 'e2e_detail', payload: { tenant_id: TEN, jurisdiction_id: J, action: 'set', kyc_ruleset_id: null, aml_ruleset_id: null, rg_ruleset_id: null, reporting_ruleset_id: null, allowed_currencies: ['EUR'], allowed_payment_methods: ['card'], geo_block_list: ['FR'] } })
  // a narrowing one, applied at once
  const lic2 = createLicence('E2E-DETAIL', 'active')
  await mcFileAndApprove(ceilingBody(lic2.id, CC, 'enable', 'E2E-SYNTHETIC-CEILING-AT2'))
  const narrow = await file(ceilingBody(lic2.id, CC, 'disable'))
  expect(mcChange(narrow)).toMatchObject({ status: 'executed', widening: false })

  // one approval on the ceiling request so a non-zero count is rendered
  expect((await apiApprove(await apiLogin('PA_B'), ceiling)).status).toBe(200)
  const C = await ctx(browser, 'PA_C')
  const expectations: { id: string; sentence: RegExp; path: string }[] = [
    { id: ceiling, sentence: new RegExp(`^On licence ${LIC}: ENABLE country ${CC} in the licence country ceiling\\.$`), path: 'Widening: needs 2 approvals from 2 different people.' },
    { id: policy, sentence: new RegExp(`^For tenant ${TEN}, brand rung: WITHDRAW the version for country ${CC} for brand ${brand}\\.$`), path: 'Widening: needs 2 approvals from 2 different people.' },
    { id: config, sentence: new RegExp(`^For tenant ${TEN}: SET a new version \\(replacing the version in force\\) of the configuration for jurisdiction ${J}\\.$`), path: 'Widening: needs 2 approvals from 2 different people.' },
    { id: narrow, sentence: new RegExp(`^On licence ${lic2.id}: DISABLE country ${CC} in the licence country ceiling\\.$`), path: 'Narrowing: applied at once by one platform administrator.' },
  ]
  for (const e of expectations) {
    await spaGoto(C.page, `/licensing/changes/${e.id}`)
    const db = mcChange(e.id)
    await expect(C.page.getByLabel('Change in words')).toHaveText(e.sentence)
    await expect(C.page.getByLabel('Approval path')).toHaveText(e.path)
    await expect(C.page.getByLabel('Market configuration disclaimer')).toBeVisible()
    await expect(C.page.getByText(`${db.approvals} of ${db.required} required`)).toBeVisible()
    await expect(C.page.getByText(db.widening ? 'Widening' : 'Narrowing or neutral', { exact: true })).toBeVisible()
  }
  expect(mcChange(ceiling)).toMatchObject({ widening: true, required: 2, approvals: 1 })
  expect(mcChange(policy)).toMatchObject({ widening: true, required: 2, approvals: 0 })
  expect(mcChange(config)).toMatchObject({ widening: true, required: 2, approvals: 0 })
  // the field list names the operator-supplied reference as unverified and prints the frozen values
  await spaGoto(C.page, `/licensing/changes/${ceiling}`)
  await expect(C.page.getByText('Authorization reference (supplied by the operator, not verified)')).toBeVisible()
  await expect(C.page.getByText('E2E-SYNTHETIC-CEILING-AT', { exact: true }).first()).toBeVisible()
  // tidy: the requester cancels what is still open
  for (const id of [ceiling, policy, config]) expect((await mcCall('POST', `/v1/admin/licensing-changes/${id}/cancel`, tokA, { reason_code: 'e2e_tidy' })).status).toBe(200)
})

test('(h) deny by default: an approved ceiling alone does not open the country for the tenant; only an approved tenant policy does', async ({ browser }) => {
  const TEN = tenantId(), LIC = syntheticLicenceId(), CC = 'PT'
  const A = await ctx(browser, 'PA_A')
  await mcFileAndApprove(ceilingBody(LIC, CC, 'enable', 'E2E-SYNTHETIC-CEILING-PT'))
  expect(mcCeiling(LIC, CC)).toMatchObject({ state: 'enabled', status: 'active' })
  const tok = A.accessToken()
  const ceil = await mcCall('GET', `/v1/admin/licence-country-ceilings?licence_id=${LIC}&country_code=${CC}&current=true`, tok)
  expect(ceil.json.total).toBe(1)
  // the read API: no tenant policy for the country; the real resolver: closed for every operation
  const pol = () => mcCall('GET', `/v1/admin/operating-country-policies?tenant_id=${TEN}&country_code=${CC}`, tok)
  expect((await pol()).json.total).toBe(0)
  expect(OPS.map((o) => mcResolve(TEN, CC, o))).toEqual(OPS.map(() => 'not_configured'))
  // the tenant page lists no policy for the country
  await openTenantPage(A, TEN)
  await expect(A.page.getByRole('cell', { name: CC, exact: true })).toHaveCount(0)

  // a FILED but unapproved tenant policy changes nothing
  const f = await mcCall('POST', '/v1/admin/licensing-changes', tok, policyBody({ tenant: TEN, scope: 'tenant', country: CC, kind: 'enable', ref: 'E2E-SYNTHETIC-POLICY-PT' }))
  expect(f.status).toBe(201)
  expect(f.json.executed).toBe(false)
  const id = f.json.change_request.id as string
  expect((await apiApprove(await apiLogin('PA_B'), id)).status).toBe(200) // one of two
  expect((await pol()).json.total).toBe(0)
  expect(OPS.map((o) => mcResolve(TEN, CC, o))).toEqual(OPS.map(() => 'not_configured'))
  // the second distinct Person approves: now (and only now) the tenant policy exists and the country opens
  expect((await apiApprove(await apiLogin('PA_C'), id)).status).toBe(200)
  expect((await pol()).json.total).toBe(1)
  expect(mcResolve(TEN, CC, 'deposit')).toBe('permitted')
})
