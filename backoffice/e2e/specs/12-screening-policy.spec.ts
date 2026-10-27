// AML screening POLICY tab through the REAL stack (lic32; lic31 flows 8, 9, 10): a platform administrator acting as an AML officer authors the
// jurisdiction screening policy in the Back Office; the closed refusal codes arrive on the wire (`check_codes`) and render their exact wording.
// The MOCK screening worker is the one platform-api starts outside production; nothing is stubbed. Synthetic only; the legal review reference is a
// synthetic marker, not a legal review.
import { expect, test, type Browser } from 'playwright/test'
import { apiLogin } from '../lib/api'
import { loginAs } from '../lib/session'
import { codesOf, fillScreeningPolicy, openScreeningPolicyTab, spCount, spRows, submitScreeningPolicy, thisMinute, SUBMIT, WORDING } from '../lib/sp_ui'
import { configureTenantJurisdiction, fillPolicy, grantOfficer, jurisdictionId, openScreening, submitPolicy, waitHeartbeat, waitPolicyEffective, withoutHeartbeat } from '../lib/wd_aml'
import { call, createTenant, type WdTenant } from '../lib/wd_world'
import { loadWorld } from '../lib/world'

const ctx = async (browser: Browser, key: string) => loginAs(await (await browser.newContext()).newPage(), key)
const DAY = 86400_000
/** The server's ANSWER to the authoring POST: a 403 here is only the step-up challenge of an aged session (the UI re-sends once after the prompt), so it is skipped. */
const answered = (page: import('playwright/test').Page) => page.waitForResponse((r) => r.url().includes('/screening/policy') && r.request().method() === 'POST' && r.status() !== 403)

test.describe.serial('12 AML screening policy tab: authoring, closed refusal codes on the wire, exact wording, authorization', () => {
  test.setTimeout(900_000)
  let T: WdTenant
  const policyPath = () => `/v1/admin/tenants/${T.id}/screening/policy`
  const body = (over: Record<string, unknown> = {}) => ({
    jurisdiction_id: jurisdictionId(), lists: ['sanctions', 'pep'], rescreen_interval_seconds: 86400, backfill_grace_seconds: 0,
    effective_from: thisMinute().toISOString(), legal_review_reference: 'E2E-SYNTHETIC-NOT-A-LEGAL-REVIEW', reason_code: 'e2e_direct', ...over,
  })

  test.beforeAll(async () => {
    T = await createTenant('sp1')
    await configureTenantJurisdiction(T)
    await grantOfficer(T, 'PA_C')
    await grantOfficer(T, 'PA_D')
    expect(spCount(T)).toBe(0)
  })

  test('(a) unconfigured: nothing is assumed; the first version is authored with explicit fields and stored as a platform-acting officer version', async ({ browser }) => {
    const C = await ctx(browser, 'PA_C')
    await openScreening(C, T)
    await openScreeningPolicyTab(C)
    await expect(C.page.getByText('No screening policy is in force for this jurisdiction. The platform ships none: no interval and no grace are assumed.')).toBeVisible()
    const get0 = await call('GET', `${policyPath()}?jurisdiction_id=${jurisdictionId()}`, C.accessToken())
    expect(get0.status).toBe(200)
    expect(get0.json.configured).toBe(false)
    // the dialog starts empty: no list, interval, grace or effective time is preselected, and submit is disabled
    await C.page.getByRole('button', { name: 'Author next screening policy version' }).click()
    const e = C.page.getByRole('dialog', { name: 'Author the next screening policy version' })
    await expect(e.getByLabel('Sanctions list', { exact: true })).not.toBeChecked()
    await expect(e.getByLabel('PEP list', { exact: true })).not.toBeChecked()
    for (const l of ['Re-screen interval (seconds)', 'Backfill grace (seconds)', 'Effective from', 'Legal review reference', 'Reason code']) await expect(e.getByLabel(l, { exact: true })).toHaveValue('')
    await expect(e.getByRole('button', { name: SUBMIT, exact: true })).toBeDisabled()
    await e.getByRole('button', { name: 'Cancel' }).click()

    const d = await fillScreeningPolicy(C, { lists: ['Sanctions list', 'PEP list'], interval: '86400', grace: '0', from: thisMinute(), reason: 'e2e_sp_first' })
    await submitScreeningPolicy(C, d, () => expect(C.page.getByRole('status').filter({ hasText: /Screening policy version .* recorded/ })).toBeVisible())
    expect(spCount(T)).toBe(1)
    expect(spRows(T)).toEqual(['pep,sanctions|86400|0|platform_acting'])
    await openScreeningPolicyTab(C)
    const dl = C.page.getByLabel('Screening policy in force')
    await expect(dl).toContainText(/PEP list, Sanctions list|Sanctions list, PEP list/)
    await expect(dl).toContainText('86400 seconds')
    await expect(dl).toContainText('0 seconds') // an explicit 0 grace is a stated value
    const get1 = await call('GET', `${policyPath()}?jurisdiction_id=${jurisdictionId()}`, C.accessToken())
    expect(get1.json).toMatchObject({ configured: true, rescreen_interval_seconds: 86400, backfill_grace_seconds: 0 })
  })

  test('(b) the UI blocks what it can certainly predict (floor, loosening against the policy in force) and sends nothing', async ({ browser }) => {
    const C = await ctx(browser, 'PA_C')
    await openScreening(C, T)
    await openScreeningPolicyTab(C)
    const before = spCount(T)
    let reqs = 0
    C.page.on('request', (r) => { if (r.method() === 'POST' && r.url().includes('/screening/policy')) reqs++ })
    // floor: 3599 is refused client side with the floor sentence, submit stays disabled
    let d = await fillScreeningPolicy(C, { lists: ['Sanctions list', 'PEP list'], interval: '3599', grace: '0', from: thisMinute(), reason: 'e2e_sp_floor' })
    await expect(d.getByRole('alert').filter({ hasText: WORDING.floorClient })).toBeVisible()
    await expect(d.getByRole('button', { name: SUBMIT, exact: true })).toBeDisabled()
    await d.getByRole('button', { name: 'Cancel' }).click()
    // loosening against the version in force (a list removed, a longer interval): blocked with the loosening alert
    d = await fillScreeningPolicy(C, { lists: ['Sanctions list'], interval: '172800', grace: '0', from: thisMinute(), reason: 'e2e_sp_loosen' })
    await expect(d.getByRole('alert', { name: 'Loosening refused' })).toContainText('a list screened by the policy in force would be removed')
    await expect(d.getByRole('alert', { name: 'Loosening refused' })).toContainText('the re-screen interval would be longer than in the policy in force')
    await expect(d.getByRole('button', { name: SUBMIT, exact: true })).toBeDisabled()
    expect(reqs).toBe(0)
    expect(spCount(T)).toBe(before)
  })

  test('(c) direct API calls with the officer session: the closed codes arrive on the wire and nothing is stored', async ({ browser }) => {
    const C = await ctx(browser, 'PA_C')
    const tok = C.accessToken()
    const n0 = spCount(T)
    const wire: string[] = []
    const post = async (name: string, over: Record<string, unknown>, status: number, code: string) => {
      const r = await call('POST', policyPath(), tok, body(over))
      wire.push(`${name}: status=${r.status} code=${r.json?.code} check_codes=${JSON.stringify(codesOf(r.json))}`)
      expect(r.status, name).toBe(status)
      expect(codesOf(r.json), name).toContain(code) // a 409 also carries the generic `illegal_transition` check code (observed, see the record)
      expect(spCount(T), name).toBe(n0)
      return r
    }
    // interval below the engineering floor (the handler answers before any officer check)
    await post('rescreen_interval_below_floor', { rescreen_interval_seconds: 3599, reason_code: 'e2e_direct_floor' }, 400, 'rescreen_interval_below_floor')
    // loosening: a list removed and a longer interval than the latest version
    await post('policy_loosening_refused', { lists: ['sanctions'], rescreen_interval_seconds: 172800, reason_code: 'e2e_direct_loosen' }, 409, 'policy_loosening_refused')
    // an effective time more than 30 days ahead, and one more than 2 minutes in the past
    await post('policy_effective_from_out_of_range (+40d)', { effective_from: new Date(Date.now() + 40 * DAY).toISOString(), reason_code: 'e2e_direct_far' }, 400, 'policy_effective_from_out_of_range')
    await post('policy_effective_from_out_of_range (-1h)', { effective_from: new Date(Date.now() - 3600_000).toISOString(), reason_code: 'e2e_direct_past' }, 400, 'policy_effective_from_out_of_range')
    console.log(`[12 c] closed codes on the wire:\n  ${wire.join('\n  ')}`)
  })

  test('(d) refusals reached through the UI render their exact wording: effective time out of range; loosening against a FUTURE-dated latest version', async ({ browser }) => {
    const C = await ctx(browser, 'PA_C')
    await openScreening(C, T)
    await openScreeningPolicyTab(C)
    const n0 = spCount(T)
    // (1) +40 days: the UI only WARNS about the window, the server refuses with its closed code and the UI renders it
    let d = await fillScreeningPolicy(C, { lists: ['Sanctions list', 'PEP list'], interval: '86400', grace: '0', from: new Date(Date.now() + 40 * DAY), reason: 'e2e_sp_far' })
    await expect(d.getByRole('status').filter({ hasText: 'This time looks outside the window the server accepts' })).toBeVisible()
    const far = answered(C.page)
    await submitScreeningPolicy(C, d, () => expect(d.getByRole('alert').filter({ hasText: WORDING.policy_effective_from_out_of_range })).toBeVisible())
    const fr = await far
    expect(fr.status()).toBe(400)
    expect(codesOf(await fr.json())).toEqual(['policy_effective_from_out_of_range'])
    expect(spCount(T)).toBe(n0)
    await d.getByRole('button', { name: 'Cancel' }).click()

    // (2) a tighter version dated tomorrow is accepted (it tightens the policy in force, so the UI allows it) ...
    d = await fillScreeningPolicy(C, { lists: ['Sanctions list', 'PEP list'], interval: '43200', grace: '0', from: new Date(Date.now() + DAY), reason: 'e2e_sp_tomorrow' })
    await submitScreeningPolicy(C, d, () => expect(C.page.getByRole('status').filter({ hasText: /Screening policy version .* recorded/ })).toBeVisible())
    expect(spCount(T)).toBe(n0 + 1)
    // ... and a version dated the day after, looser than THAT latest version (60000 s > 43200 s) but tighter than the one in force (86400 s):
    // the UI compares with the version in force only, so it sends it, the server compares with the latest and refuses
    await openScreeningPolicyTab(C)
    d = await fillScreeningPolicy(C, { lists: ['Sanctions list', 'PEP list'], interval: '60000', grace: '0', from: new Date(Date.now() + 2 * DAY), reason: 'e2e_sp_loosen_latest' })
    await expect(d.getByRole('alert', { name: 'Loosening refused' })).toHaveCount(0)
    const loose = answered(C.page)
    await submitScreeningPolicy(C, d, () => expect(d.getByRole('alert').filter({ hasText: WORDING.policy_loosening_refused })).toBeVisible())
    const lr = await loose
    expect(lr.status()).toBe(409)
    expect(codesOf(await lr.json())).toContain('policy_loosening_refused')
    expect(spCount(T)).toBe(n0 + 1)
    expect(spRows(T)[0]).toBe('pep,sanctions|43200|0|platform_acting')
  })

  test('(e) the existing trigger-policy screen: enabling the withdrawal gate with no worker heartbeat shows the exact heartbeat wording (no longer the generic text)', async ({ browser }) => {
    const C = await ctx(browser, 'PA_C')
    await openScreening(C, T)
    await C.page.getByRole('tab', { name: 'Trigger policy' }).click()
    await C.page.getByLabel('Licence jurisdiction').selectOption(jurisdictionId())
    await C.page.getByRole('button', { name: 'Show policy in force' }).click()
    const hb = await withoutHeartbeat()
    try {
      const d = await fillPolicy(C, { events: ['Registration', 'KYC approval', 'Scheduled re-screen'], gate: true, seconds: '600', reason: 'e2e_gate_nohb' })
      const posted = C.page.waitForResponse((r) => r.url().includes('/screening/trigger-policy') && r.request().method() === 'POST')
      await submitPolicy(C, d, () => expect(d.getByRole('alert').filter({ hasText: WORDING.screening_worker_heartbeat_missing })).toBeVisible())
      const resp = await posted
      const j = await resp.json()
      console.log(`[12 e] gate without heartbeat on the wire: status=${resp.status()} code=${j.code} check_codes=${JSON.stringify(codesOf(j))}`)
      expect(codesOf(j)).toEqual(['screening_worker_heartbeat_missing'])
      expect([400, 409]).toContain(resp.status())
      await expect(d.getByRole('alert')).not.toContainText('The server rejected the policy as invalid')
      await expect(d.getByRole('alert')).not.toContainText('does not say which applied')
    } finally {
      await hb.release()
    }
    await waitHeartbeat()
  })

  test('(f) a withdrawal gate in force makes a short interval reachable through the UI: rescreen_interval_below_alert_threshold renders its exact wording', async ({ browser }) => {
    const C = await ctx(browser, 'PA_C')
    await openScreening(C, T)
    await C.page.getByRole('tab', { name: 'Trigger policy' }).click()
    await C.page.getByLabel('Licence jurisdiction').selectOption(jurisdictionId())
    await C.page.getByRole('button', { name: 'Show policy in force' }).click()
    const g = await fillPolicy(C, { events: ['Registration', 'KYC approval', 'Scheduled re-screen'], gate: true, seconds: '3600', reason: 'e2e_gate_3600' })
    await submitPolicy(C, g, () => expect(C.page.getByRole('status').filter({ hasText: /Policy version .* recorded/ })).toBeVisible())
    await waitPolicyEffective(T, 'PA_C', true)

    await C.page.getByRole('tab', { name: 'Screening policy' }).click()
    await C.page.getByLabel('Licence jurisdiction').selectOption(jurisdictionId())
    await C.page.getByRole('button', { name: 'Show screening policy in force' }).click()
    await expect(C.page.getByLabel('Gate threshold of the jurisdiction')).toContainText('alert threshold of 3600 seconds, so the interval must be at least 7200 seconds')
    const n0 = spCount(T)
    // 3600 s is valid on its own (the floor) and tighter than every stored version, but below twice the gate threshold
    const d = await fillScreeningPolicy(C, { lists: ['Sanctions list', 'PEP list'], interval: '3600', grace: '0', from: new Date(Date.now() + 3 * DAY), reason: 'e2e_sp_below_gate' })
    await expect(d.getByRole('status').filter({ hasText: 'the server will refuse an interval below 7200 seconds' })).toBeVisible()
    const posted = answered(C.page)
    await submitScreeningPolicy(C, d, () => expect(d.getByRole('alert').filter({ hasText: WORDING.rescreen_interval_below_alert_threshold })).toBeVisible())
    const resp = await posted
    const j = await resp.json()
    console.log(`[12 f] below alert threshold on the wire: status=${resp.status()} code=${j.code} check_codes=${JSON.stringify(codesOf(j))}`)
    expect(codesOf(j)).toEqual(['rescreen_interval_below_alert_threshold'])
    expect(spCount(T)).toBe(n0)
    // an interval at exactly twice the threshold is accepted (boundary)
    await d.getByRole('button', { name: 'Cancel' }).click()
    const ok = await fillScreeningPolicy(C, { lists: ['Sanctions list', 'PEP list'], interval: '7200', grace: '0', from: new Date(Date.now() + 3 * DAY), reason: 'e2e_sp_at_gate' })
    await submitScreeningPolicy(C, ok, () => expect(C.page.getByRole('status').filter({ hasText: /Screening policy version .* recorded/ })).toBeVisible())
    expect(spCount(T)).toBe(n0 + 1)
    expect(spRows(T)[0]).toBe('pep,sanctions|7200|0|platform_acting')
  })

  test('(g) who may read and author: non-officer platform admin, tenant staff and another tenant are refused (403, 404) and nothing is written', async ({ browser }) => {
    const n0 = spCount(T)
    const rows0 = spRows(T)
    // a platform admin WITHOUT an officer grant (PA_A): the read is refused and rendered; the direct POST is refused
    const A = await ctx(browser, 'PA_A')
    await openScreening(A, T)
    await A.page.getByRole('tab', { name: 'Screening policy' }).click()
    await A.page.getByLabel('Licence jurisdiction').selectOption(jurisdictionId())
    await A.page.getByRole('button', { name: 'Show screening policy in force' }).click()
    await expect(A.page.getByRole('alert').filter({ hasText: 'does not hold an in-force AML officer grant for this tenant' })).toBeVisible()
    const a = await call('POST', policyPath(), A.accessToken(), body({ rescreen_interval_seconds: 3600, reason_code: 'e2e_nope' }))
    console.log(`[12 g] non-officer platform admin POST: status=${a.status} check_codes=${JSON.stringify(codesOf(a.json))}`)
    expect(a.status).toBe(403)

    // tenant staff of this tenant: may READ (compliance), cannot author (server 403 platform_officer_required); the tab offers no authoring
    const comp = await apiLogin(`${T.tag}_COMPLIANCE`)
    const rd = await call('GET', `${policyPath()}?jurisdiction_id=${jurisdictionId()}`, comp)
    console.log(`[12 g] tenant compliance GET: status=${rd.status}`)
    const cw = await call('POST', policyPath(), comp, body({ reason_code: 'e2e_tenant_try' }))
    console.log(`[12 g] tenant compliance POST: status=${cw.status} check_codes=${JSON.stringify(codesOf(cw.json))}`)
    expect(cw.status).toBe(403)
    // the route's permission gate answers a tenant role first, with no closed check code (the database is never reached)
    expect(codesOf(cw.json)).toEqual([])
    const ta = await call('POST', policyPath(), await apiLogin(`${T.tag}_TENANT_ADMIN`), body({ reason_code: 'e2e_tenant_try' }))
    console.log(`[12 g] tenant admin POST: status=${ta.status} check_codes=${JSON.stringify(codesOf(ta.json))}`)
    expect([403, 404]).toContain(ta.status)
    const S = await ctx(browser, `${T.tag}_COMPLIANCE`)
    await S.page.getByRole('navigation').getByRole('link', { name: 'AML' }).click()
    await S.page.getByRole('link', { name: 'Automatic screening: trigger policy and screening requests' }).click()
    await S.page.getByRole('tab', { name: 'Screening policy' }).click()
    await expect(S.page.getByText('Authoring is not offered to your role. The screening policy is read-only here.')).toBeVisible()
    await expect(S.page.getByRole('button', { name: 'Author next screening policy version' })).toHaveCount(0)

    // another tenant: the sp1 compliance token against the seeded tenant's id is 404; the officer's POST for a tenant id that is not theirs
    // scope cannot reach it either
    const w = loadWorld()
    const other = (await import('../lib/db')).sql(`SELECT id FROM tenants WHERE slug = '${w.tenantSlug}'`)
    const r404 = await call('GET', `/v1/admin/tenants/${other}/screening/policy?jurisdiction_id=${jurisdictionId()}`, comp)
    const w404 = await call('POST', `/v1/admin/tenants/${other}/screening/policy`, comp, body({ reason_code: 'e2e_other' }))
    console.log(`[12 g] other tenant with tenant staff token: GET=${r404.status} POST=${w404.status} check_codes=${JSON.stringify(codesOf(w404.json))}`)
    expect(r404.status).toBe(404)
    expect(w404.status).toBe(404)
    // the officer (PA_C) on a tenant where she holds no grant (the seeded e2e tenant): refused, nothing written
    const Cd = await apiLogin('PA_C')
    const o = await call('POST', `/v1/admin/tenants/${other}/screening/policy`, Cd, body({ reason_code: 'e2e_other_officer' }))
    console.log(`[12 g] officer of ${T.slug} on the seeded tenant: POST=${o.status} check_codes=${JSON.stringify(codesOf(o.json))}`)
    expect(o.status).toBeGreaterThanOrEqual(400)
    expect(o.status).toBeLessThan(500)
    expect(spCount(T)).toBe(n0)
    expect(spRows(T)).toEqual(rows0)
  })
})
